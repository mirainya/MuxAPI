package forward

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestForwardAuditsCompressedCodexSSE(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Errorf("transport should negotiate gzip itself, got %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		_, _ = io.WriteString(compressed, "event: response.completed\n")
		_, _ = io.WriteString(compressed, `data: {"type":"response.completed","response":{"usage":{"input_tokens":4391,"output_tokens":5,"input_tokens_details":{"cached_tokens":3385}}}}`+"\n\n")
		_ = compressed.Close()
	}))
	defer upstreamServer.Close()

	ups := []*upstream.Upstream{{ID: 1, BaseURL: upstreamServer.URL, APIKey: "key", Protocol: "codex", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 1)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	request.Header.Set("Accept-Encoding", "gzip, deflate, br")

	result := fwd.Forward(recorder, request, []byte(`{"model":"gpt-5.6-sol","stream":true}`), 1, "")
	if result.InputTokens != 4391 || result.OutputTokens != 5 || result.CachedTokens != 3385 {
		t.Fatalf("compressed SSE usage was not audited: %+v", result)
	}
	if !result.StreamCompleted || result.LastEvent != "response.completed" {
		t.Fatalf("compressed SSE completion was not audited: %+v", result)
	}
	if encoding := recorder.Header().Get("Content-Encoding"); encoding != "" {
		t.Fatalf("decoded response must not retain Content-Encoding: %q", encoding)
	}
}

func TestForwardNativeGeminiRequest(t *testing.T) {
	var gotPath, gotKey string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotKey = r.Header.Get("x-goog-api-key")
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"contents"`) || strings.Contains(string(body), `"model"`) {
			t.Errorf("native Gemini body changed: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1}}\n\n")
	}))
	defer upstreamServer.Close()

	ups := []*upstream.Upstream{{ID: 1, BaseURL: upstreamServer.URL, APIKey: "gemini-key", Protocol: "gemini", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 1)
	body := []byte(`{"contents":[{"parts":[{"text":"hello"}]}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-test:streamGenerateContent?key=client-key", bytes.NewReader(body))
	request.Header.Set("x-goog-api-key", "client-key")
	recorder := httptest.NewRecorder()
	result := fwd.Forward(recorder, request, body, 1, "")
	if result.Outcome != OutcomeSuccess || recorder.Code != http.StatusOK {
		t.Fatalf("native Gemini result status=%d result=%+v body=%s", recorder.Code, result, recorder.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-test:streamGenerateContent?alt=sse" {
		t.Fatalf("native Gemini path/query = %q", gotPath)
	}
	if gotKey != "gemini-key" {
		t.Fatalf("native Gemini key = %q", gotKey)
	}
	if !strings.Contains(recorder.Body.String(), "\"finishReason\":\"STOP\"") || result.InputTokens != 3 || result.OutputTokens != 1 {
		t.Fatalf("native Gemini response/audit = %s %+v", recorder.Body.String(), result)
	}
}

func TestForwardUsesDynamicMaxAttemptsPerRequest(t *testing.T) {
	servers := make([]*httptest.Server, 0, 4)
	upstreams := make([]*upstream.Upstream, 0, 4)
	for i := 1; i <= 4; i++ {
		status := http.StatusServiceUnavailable
		if i == 4 {
			status = http.StatusOK
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			if status == http.StatusOK {
				_, _ = io.WriteString(w, `{"ok":true}`)
			}
		}))
		servers = append(servers, server)
		upstreams = append(upstreams, &upstream.Upstream{
			ID: int64(i), BaseURL: server.URL, APIKey: "k", Priority: i, Weight: 1,
		})
	}
	defer func() {
		for _, server := range servers {
			server.Close()
		}
	}()

	hm := health.New(100, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 2)
	var limit atomic.Int64
	limit.Store(2)
	fwd.SetMaxAttemptsProvider(func() int { return int(limit.Load()) })
	body := []byte(`{"model":"gpt"}`)

	first := fwd.Forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)), body, 1, "")
	if len(first.Attempts) != 2 || first.Outcome == OutcomeSuccess {
		t.Fatalf("first request should stop after two upstreams: %+v", first)
	}

	limit.Store(4)
	second := fwd.Forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)), body, 1, "")
	if len(second.Attempts) != 4 || second.Outcome != OutcomeSuccess || second.FinalUpstreamID != 4 {
		t.Fatalf("updated limit should take effect on the next request: %+v", second)
	}
}

// 验证 SSE 流式逐行透传不丢事件、不破坏格式。
func TestForwardSSEStreaming(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for _, ev := range []string{
			"event: message_start",
			`data: {"type":"message_start"}`,
			"",
			"event: content_block_delta",
			`data: {"text":"hi"}`,
			"",
			"event: message_stop",
			`data: {"type":"message_stop"}`,
			"",
		} {
			io.WriteString(w, ev+"\n")
			fl.Flush()
		}
	}))
	defer upSrv.Close()

	ups := []*upstream.Upstream{{ID: 1, Name: "A", BaseURL: upSrv.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 3)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	fwd.Forward(rec, req, []byte(`{"stream":true}`), 0, "")

	res := rec.Result()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("应透传 SSE Content-Type，实际 %q", ct)
	}
	body, _ := io.ReadAll(res.Body)
	for _, want := range []string{"message_start", `"text":"hi"`, "content_block_delta"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("SSE 应含 %q，实际:\n%s", want, body)
		}
	}
}

// 验证首个上游流式失败(5xx)时回切到下一个上游并成功流式透传。
func TestForwardSSEFailover(t *testing.T) {
	var aHits, bHits int32
	// 上游 A：优先级高，但 503 失败
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.WriteHeader(503)
	}))
	defer srvA.Close()
	// 上游 B：备用，正常吐 SSE
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		io.WriteString(w, `data: {"text":"from-B"}`+"\n")
		io.WriteString(w, `data: {"type":"message_stop"}`+"\n")
		fl.Flush()
	}))
	defer srvB.Close()

	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", BaseURL: srvA.URL, APIKey: "k", Priority: 10, Enabled: true},
		{ID: 2, Name: "B", BaseURL: srvB.URL, APIKey: "k", Priority: 20, Enabled: true},
	}
	hm := health.New(1, time.Hour) // 阈值1：A一失败立即熔断，下次Pick避开
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 3)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	fwd.Forward(rec, req, []byte(`{"stream":true}`), 0, "")

	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("回切后应 200，实际 %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "from-B") {
		t.Fatalf("应回切到 B 并透传其 SSE，实际:\n%s", body)
	}
	if atomic.LoadInt32(&aHits) != 1 || atomic.LoadInt32(&bHits) != 1 {
		t.Fatalf("A 应被试 1 次、B 1 次，实际 A=%d B=%d", aHits, bHits)
	}
	if !hm.IsAvailable(2, "") || hm.IsAvailable(1, "") {
		t.Fatal("失败后 A 应熔断、B 应可用")
	}
}

func TestForwardSSECleanEOFAfterFirstByteIsTransparent(t *testing.T) {
	var backupHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: response.output_text.delta\n")
		io.WriteString(w, "data: {\"delta\":\"ok\"}\n\n")
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		io.WriteString(w, `{"backup":true}`)
	}))
	defer backup.Close()

	upstreams := []*upstream.Upstream{
		{ID: 1, BaseURL: primary.URL, APIKey: "k", Priority: 1, Weight: 1},
		{ID: 2, BaseURL: backup.URL, APIKey: "k", Priority: 2, Weight: 1},
	}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 2)
	body := []byte(`{"model":"gpt","stream":true}`)
	recorder := httptest.NewRecorder()
	result := fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)), body, 1, "")

	if result.Outcome != OutcomeSuccess || len(result.Attempts) != 1 {
		t.Fatalf("clean EOF after streamed bytes must be transparent, got %+v", result)
	}
	if atomic.LoadInt32(&backupHits) != 0 {
		t.Fatal("backup must not run after response bytes were committed")
	}
	if got := recorder.Body.String(); !strings.Contains(got, `"delta":"ok"`) {
		t.Fatalf("stream bytes were not relayed: %s", got)
	}
}

func TestForwardSSEEmptyBodyFailsOverBeforeCommit(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"text\":\"backup\"}\n\n")
	}))
	defer backup.Close()

	upstreams := []*upstream.Upstream{
		{ID: 1, BaseURL: primary.URL, APIKey: "k", Priority: 1, Weight: 1},
		{ID: 2, BaseURL: backup.URL, APIKey: "k", Priority: 2, Weight: 1},
	}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 2)
	body := []byte(`{"model":"gpt","stream":true}`)
	recorder := httptest.NewRecorder()
	result := fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)), body, 1, "")

	if result.Outcome != OutcomeSuccess || len(result.Attempts) != 2 || result.Attempts[0].Outcome != OutcomeFailed {
		t.Fatalf("empty stream must fail over before commit, got %+v", result)
	}
	if got := recorder.Body.String(); !strings.Contains(got, `"text":"backup"`) {
		t.Fatalf("backup stream was not relayed: %s", got)
	}
}

// 验证按失败原因区分熔断范围：5xx 仅熔断 (上游,模型)，401 熔断整上游。
func TestForwardFailScope(t *testing.T) {
	srv503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv503.Close()
	ups := []*upstream.Upstream{{ID: 1, Name: "A", BaseURL: srv503.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm := health.New(1, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 0)

	body := []byte(`{"model":"gpt-x","stream":false}`)
	fwd.Forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body))), body, 0, "")
	if hm.IsAvailable(1, "gpt-x") || hm.IsAvailable(1, "gpt-y") || hm.IsAvailable(1, "") {
		t.Fatal("503 should open the whole upstream breaker")
	}

	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv401.Close()
	ups2 := []*upstream.Upstream{{ID: 2, Name: "C", BaseURL: srv401.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm2 := health.New(1, time.Hour)
	fwd2 := New(scheduler.New(func(int64) []*upstream.Upstream { return ups2 }, hm2), hm2, 0)
	body2 := []byte(`{"model":"gpt-x"}`)
	fwd2.Forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body2))), body2, 0, "")
	if hm2.IsAvailable(2, "anything") {
		t.Fatal("401 should open the whole upstream breaker")
	}
}

func TestForwardSSEIncrementalFlush(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		io.WriteString(w, `data: {"chunk":1}`+"\n")
		fl.Flush()
		<-release // 阻塞，直到测试确认已收到首块才吐第二块
		io.WriteString(w, `data: {"chunk":2}`+"\n")
		fl.Flush()
	}))
	defer srv.Close()

	ups := []*upstream.Upstream{{ID: 1, Name: "A", BaseURL: srv.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 3)

	pr, pw := io.Pipe()
	rec := &flushRecorder{header: http.Header{}, w: pw, flushed: make(chan struct{}, 8)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	go fwd.Forward(rec, req, []byte(`{"stream":true}`), 0, "")

	br := bufio.NewReader(pr)
	line1, _ := br.ReadString('\n')
	if !strings.Contains(line1, `"chunk":1`) {
		t.Fatalf("首块应先到达，实际 %q", line1)
	}
	select {
	case <-rec.flushed: // 确认首块是被 flush 出来的，不是缓冲
	case <-time.After(time.Second):
		t.Fatal("首块应触发 flush")
	}
	close(release) // 放行第二块
	line2, _ := br.ReadString('\n')
	if !strings.Contains(line2, `"chunk":2`) {
		t.Fatalf("第二块应随后到达，实际 %q", line2)
	}
	pr.Close()
}

// 回归：默认熔断阈值(>1)下，单次请求内首个上游失败也应立即切到下一个。
// 旧实现依赖「失败即熔断」才能换上游，阈值3时一次失败不熔断 → Pick 又选回 A → 误判 502。
func TestForwardFailoverWithHighThreshold(t *testing.T) {
	var aHits, bHits int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.WriteHeader(503)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		w.WriteHeader(200)
		io.WriteString(w, `{"text":"from-B"}`)
	}))
	defer srvB.Close()

	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", BaseURL: srvA.URL, APIKey: "k", Priority: 10, Enabled: true},
		{ID: 2, Name: "B", BaseURL: srvB.URL, APIKey: "k", Priority: 20, Enabled: true},
	}
	hm := health.New(3, time.Hour) // 默认阈值3：A 一次失败不熔断，仍须切到 B
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 3)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	fwd.Forward(rec, req, []byte(`{}`), 0, "")

	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("阈值3下首个上游失败也应切到 B 返回 200，实际 %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "from-B") {
		t.Fatalf("应切到 B，实际:\n%s", body)
	}
	if atomic.LoadInt32(&aHits) != 1 || atomic.LoadInt32(&bHits) != 1 {
		t.Fatalf("A 应被试 1 次、B 1 次，实际 A=%d B=%d", aHits, bHits)
	}
}

// 回归(生产事故还原)：上游返回 403(余额不足/凭证失效)应触发故障切换，
// 而非把 403 当「成功」透传给客户端。旧实现只把 5xx/429 当失败，导致
// 余额耗尽的首选上游持续 403、永不切换到有余额的备用上游。
func TestForward403Failover(t *testing.T) {
	var aHits, bHits int32
	// 上游 A：优先级高，但余额不足返回 403
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.WriteHeader(403)
		io.WriteString(w, `{"code":"INSUFFICIENT_BALANCE"}`)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		w.WriteHeader(200)
		io.WriteString(w, `{"text":"from-B"}`)
	}))
	defer srvB.Close()

	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", BaseURL: srvA.URL, APIKey: "k", Priority: 10, Enabled: true},
		{ID: 2, Name: "B", BaseURL: srvB.URL, APIKey: "k", Priority: 20, Enabled: true},
	}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 3)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	fwd.Forward(rec, req, []byte(`{}`), 0, "")

	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("A 返回 403 应切到 B 得 200，实际 %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "from-B") {
		t.Fatalf("应切到 B，实际:\n%s", body)
	}
	if atomic.LoadInt32(&aHits) != 1 || atomic.LoadInt32(&bHits) != 1 {
		t.Fatalf("A 应被试 1 次、B 1 次，实际 A=%d B=%d", aHits, bHits)
	}
}

// countingHealth 记录 Report 调用次数，并兼当 Picker 返回固定上游(避开 exclude)，
// 用于断言「客户端断连不污染健康」。
type countingHealth struct {
	reports int32
	up      *upstream.Upstream
}

func (c *countingHealth) Report(id int64, model string, ok bool, latencyMs int64) {
	atomic.AddInt32(&c.reports, 1)
}
func (c *countingHealth) ReleaseClaim(id int64)                       {}
func (c *countingHealth) MarkModelUnsupported(id int64, model string) {}
func (c *countingHealth) MarkModelSupported(id int64, model string)   {}
func (c *countingHealth) PickExcluding(groupID int64, model string, exclude map[int64]bool) (*upstream.Upstream, error) {
	if exclude[c.up.ID] {
		return nil, io.EOF // 没有更多可用上游
	}
	return c.up, nil
}

// H2 回归：客户端中途断连(ctx canceled)时，转发层不得 Report 失败——
// 否则一次 abort 会让本次试过的多个健康上游各记一次失败、累积误熔断。
func TestForwardClientCancelNoReport(t *testing.T) {
	// 上游收到请求后阻塞不返回，制造「客户端先取消」的时序窗口。
	gotReq := make(chan struct{})
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(gotReq)
		<-block // 一直阻塞到测试结束
	}))
	defer srv.Close()
	defer close(block)

	ch := &countingHealth{up: &upstream.Upstream{ID: 1, Name: "A", BaseURL: srv.URL, APIKey: "k", Priority: 1, Enabled: true}}
	fwd := New(ch, ch, 3)

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`)).WithContext(ctx)

	done := make(chan Result, 1)
	go func() {
		done <- fwd.Forward(rec, req, []byte(`{"model":"m"}`), 0, "")
	}()
	<-gotReq // 确保请求已发到上游、正阻塞在等响应头
	cancel() // 客户端断连
	select {
	case result := <-done:
		if result.Outcome != OutcomeCanceled || result.Status != StatusClientClosedRequest || len(result.Attempts) != 1 {
			t.Fatalf("取消请求应返回 499 和一条取消尝试，实际 %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("客户端取消后 Forward 应尽快返回")
	}
	if n := atomic.LoadInt32(&ch.reports); n != 0 {
		t.Fatalf("客户端断连不应 Report 任何健康反馈，实际 Report %d 次", n)
	}
}

func TestForwardRetriesSuccessfulStatusErrorPayload(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"error":{"message":"upstream failed"}}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer good.Close()

	ups := []*upstream.Upstream{
		{ID: 1, BaseURL: bad.URL, APIKey: "a", Priority: 1, Enabled: true},
		{ID: 2, BaseURL: good.URL, APIKey: "b", Priority: 2, Enabled: true},
	}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 2)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	result := fwd.Forward(rec, req, []byte(`{"model":"m"}`), 1, "k")

	if result.Outcome != OutcomeSuccess || len(result.Attempts) != 2 || result.Attempts[0].Outcome != OutcomeFailed {
		t.Fatalf("200 错误对象应触发换源，实际 %+v", result)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("应返回第二个渠道响应，实际 %s", body)
	}
}

// H3 回归：单条 SSE data 行超 1MB(旧 Scanner 上限) 也应完整透传、不静默截断。
func TestForwardSSELongLineNoTruncate(t *testing.T) {
	huge := strings.Repeat("x", 3*1024*1024) // 3MB，远超旧 Scanner 1MB 上限
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		io.WriteString(w, "data: "+huge+"\n")
		fl.Flush()
		io.WriteString(w, "data: [DONE]\n")
		fl.Flush()
	}))
	defer srv.Close()

	ups := []*upstream.Upstream{{ID: 1, Name: "A", BaseURL: srv.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 3)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	fwd.Forward(rec, req, []byte(`{"stream":true}`), 0, "")

	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), huge) {
		t.Fatalf("超长单行应完整透传，实际长度 %d 不含完整 payload", len(body))
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatal("超长行后续事件([DONE])也应到达，证明未静默截断")
	}
}

type flushRecorder struct {
	header  http.Header
	w       *io.PipeWriter
	flushed chan struct{}
}

func (f *flushRecorder) Header() http.Header { return f.header }
func (f *flushRecorder) WriteHeader(int)     {}
func (f *flushRecorder) Write(b []byte) (int, error) {
	return f.w.Write(b)
}
func (f *flushRecorder) Flush() {
	select {
	case f.flushed <- struct{}{}:
	default:
	}
}

func TestForwardHalfOpenConcurrentSingleProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Millisecond)
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	ups := []*upstream.Upstream{{ID: 1, Name: "primary", BaseURL: srv.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm := health.New(1, 20*time.Millisecond)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 0)

	hm.Report(1, "gpt-5.5", false, 0)
	time.Sleep(30 * time.Millisecond)

	body := []byte(`{"model":"gpt-5.5","stream":false}`)
	var ok200, got503 int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			fwd.Forward(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body))), body, 0, "")
			switch rec.Code {
			case 200:
				atomic.AddInt64(&ok200, 1)
			case 503:
				atomic.AddInt64(&got503, 1)
			}
		}()
	}
	wg.Wait()
	if ok200 != 1 || got503 != 7 {
		t.Fatalf("half-open single upstream should allow 1 probe and reject 7, got 200=%d 503=%d", ok200, got503)
	}
}

func TestForwardModelUnsupportedFailover(t *testing.T) {
	var aHits, bHits int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"model_not_found"}}`)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer b.Close()

	upstreams := []*upstream.Upstream{
		{ID: 1, BaseURL: a.URL, APIKey: "k", Priority: 1, Weight: 1},
		{ID: 2, BaseURL: b.URL, APIKey: "k", Priority: 2, Weight: 1},
	}
	hm := health.New(1, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 3)
	body := []byte(`{"model":"gpt-5.6"}`)
	recorder := httptest.NewRecorder()
	fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)), body, 1, "")

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("expected failover success, status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if aHits != 1 || bHits != 1 {
		t.Fatalf("expected one attempt per upstream, A=%d B=%d", aHits, bHits)
	}
	if !hm.IsModelUnsupported(1, "gpt-5.6") {
		t.Fatal("model capability should be cached for A")
	}
	if hm.EffectiveState(1) != "CLOSED" {
		t.Fatal("model capability mismatch must not open A")
	}
}

func TestForwardClientErrorsDoNotFailOver(t *testing.T) {
	for _, status := range []int{http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var backupHits atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"invalid client request"}}`)
			}))
			defer primary.Close()
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupHits.Add(1)
				io.WriteString(w, `{"ok":true}`)
			}))
			defer backup.Close()

			upstreams := []*upstream.Upstream{
				{ID: 1, BaseURL: primary.URL, APIKey: "k", Priority: 1, Weight: 1},
				{ID: 2, BaseURL: backup.URL, APIKey: "k", Priority: 2, Weight: 1},
			}
			hm := health.New(1, time.Hour)
			fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 2)
			body := []byte(`{"model":"gpt-5.6"}`)
			recorder := httptest.NewRecorder()
			result := fwd.Forward(recorder,
				httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)), body, 1, "")

			if recorder.Code != status || result.Status != status || result.Outcome != OutcomeClientError {
				t.Fatalf("status=%d result=%+v", recorder.Code, result)
			}
			if backupHits.Load() != 0 {
				t.Fatalf("client error %d must not reach backup", status)
			}
			if hm.EffectiveState(1) != "CLOSED" {
				t.Fatalf("client error %d must not open primary breaker", status)
			}
		})
	}
}

func TestForwardPreservesQuery(t *testing.T) {
	queries := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	upstreams := []*upstream.Upstream{{ID: 1, BaseURL: server.URL, APIKey: "k", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 1)
	body := []byte(`{"model":"gpt"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses?beta=true&trace=1", bytes.NewReader(body))
	fwd.Forward(httptest.NewRecorder(), request, body, 1, "")
	if got := <-queries; got != "beta=true&trace=1" {
		t.Fatalf("query was not preserved: %q", got)
	}
}

type failingWriter struct{ header http.Header }

func (w *failingWriter) Header() http.Header { return w.header }
func (w *failingWriter) WriteHeader(int)     {}
func (w *failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("client connection closed")
}
func (w *failingWriter) Flush() {}

func TestDownstreamWriteFailureDoesNotReportChannelFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {}\ndata: [DONE]\n")
	}))
	defer server.Close()
	counting := &countingHealth{up: &upstream.Upstream{ID: 1, BaseURL: server.URL, APIKey: "k", Priority: 1}}
	fwd := New(counting, counting, 1)
	body := []byte(`{"model":"gpt","stream":true}`)
	fwd.Forward(&failingWriter{header: http.Header{}}, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)), body, 1, "")
	if got := atomic.LoadInt32(&counting.reports); got != 0 {
		t.Fatalf("downstream write error must be neutral, got %d health reports", got)
	}
}

func TestFirstByteTimeoutFailsOver(t *testing.T) {
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer stalled.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer backup.Close()
	upstreams := []*upstream.Upstream{
		{ID: 1, BaseURL: stalled.URL, APIKey: "k", Priority: 1, Weight: 1},
		{ID: 2, BaseURL: backup.URL, APIKey: "k", Priority: 2, Weight: 1},
	}
	hm := health.New(1, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 2)
	fwd.SetFirstResponseTimeout(func() time.Duration { return 20 * time.Millisecond })
	body := []byte(`{"model":"gpt","stream":true}`)
	recorder := httptest.NewRecorder()
	fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)), body, 1, "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("expected backup response after first-event timeout, status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestResponseWatchdogReleasesStalledStreamAfterFirstByte(t *testing.T) {
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"chunk\":1}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer stalled.Close()

	upstreams := []*upstream.Upstream{{ID: 1, BaseURL: stalled.URL, APIKey: "k", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 1)
	fwd.SetFirstResponseTimeout(func() time.Duration { return 20 * time.Millisecond })
	body := []byte(`{"model":"gpt","stream":true}`)
	recorder := httptest.NewRecorder()
	started := time.Now()
	result := fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)), body, 1, "")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled stream was not cancelled promptly: %v", elapsed)
	}
	if result.Outcome != OutcomePartial || result.ErrorKind != "first_response_timeout" {
		t.Fatalf("result = %+v, want a timed-out partial stream", result)
	}
	if hm.EffectiveState(1) != "OPEN" {
		t.Fatalf("stalled upstream should be opened by the breaker, state=%s", hm.EffectiveState(1))
	}
}

func TestStreamingLatencyUsesTTFT(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"chunk\":1}\n")
		flusher.Flush()
		time.Sleep(150 * time.Millisecond)
		io.WriteString(w, "data: [DONE]\n")
		flusher.Flush()
	}))
	defer server.Close()
	upstreams := []*upstream.Upstream{{ID: 1, BaseURL: server.URL, APIKey: "k", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 1)
	body := []byte(`{"model":"gpt","stream":true}`)
	started := time.Now()
	fwd.Forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)), body, 1, "")
	elapsed := time.Since(started)
	if elapsed < 140*time.Millisecond {
		t.Fatalf("test did not exercise a long stream: %v", elapsed)
	}
	if got := hm.Snapshot(1).LatencyMs; got >= 100 {
		t.Fatalf("routing latency should be TTFT, not total stream duration: %dms", got)
	}
}

func TestForwardTranslatesResponsesToClaude(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Fatal("missing anthropic-version header")
		}
		requestBody, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(requestBody), `"messages"`) || strings.Contains(string(requestBody), `"input"`) {
			t.Fatalf("request was not translated to Claude: %s", requestBody)
		}
		if !strings.Contains(string(requestBody), `"stream":true`) {
			t.Fatalf("translated request should use upstream streaming: %s", requestBody)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":2}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"translated hello\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstreamServer.Close()

	upstreams := []*upstream.Upstream{{ID: 1, BaseURL: upstreamServer.URL, APIKey: "k", Protocol: "claude", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 1)
	body := []byte(`{"model":"claude-test","input":"hello","stream":false}`)
	recorder := httptest.NewRecorder()
	result := fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)), body, 1, "")

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "translated hello") {
		t.Fatalf("translated response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if result.Outcome != OutcomeSuccess || len(result.Attempts) != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestForwardPreservesNativeCodexRequest(t *testing.T) {
	body := []byte(`{"model":"gpt-test","input":[{"type":"additional_tools","tools":[]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"parallel_tool_calls":false,"client_metadata":{"source":"desktop"},"stream":true,"store":false}`)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, _ := io.ReadAll(r.Body)
		if !bytes.Equal(requestBody, body) {
			t.Fatalf("native Codex request was changed:\n got: %s\nwant: %s", requestBody, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1},\"output\":[]}}\n\n")
	}))
	defer upstreamServer.Close()

	upstreams := []*upstream.Upstream{{ID: 1, BaseURL: upstreamServer.URL, APIKey: "k", Protocol: "codex", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 1)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	request.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"turn_1"}`)
	result := fwd.Forward(recorder, request, body, 1, "")

	if recorder.Code != http.StatusOK || result.Outcome != OutcomeSuccess {
		t.Fatalf("status=%d result=%+v body=%s", recorder.Code, result, recorder.Body.String())
	}
}

func TestForwardTranslatesClaudeToOpenAI(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if r.Header.Get("anthropic-version") != "" {
			t.Fatal("anthropic-version header should not reach OpenAI upstream")
		}
		requestBody, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(requestBody), `"messages"`) || !strings.Contains(string(requestBody), `"max_tokens":16`) {
			t.Fatalf("request was not translated to OpenAI: %s", requestBody)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"translated hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	}))
	defer upstreamServer.Close()

	upstreams := []*upstream.Upstream{{ID: 1, BaseURL: upstreamServer.URL, APIKey: "k", Protocol: "openai", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 1)
	body := []byte(`{"model":"gpt-test","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("anthropic-version", "2023-06-01")
	result := fwd.Forward(recorder, request, body, 1, "")

	var response struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || response.Type != "message" || response.Role != "assistant" ||
		response.StopReason != "end_turn" || len(response.Content) != 1 || response.Content[0].Text != "translated hello" {
		t.Fatalf("translated response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if result.Outcome != OutcomeSuccess || len(result.Attempts) != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestForwardTranslatesClaudeStreamToCodex(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if r.Header.Get("anthropic-version") != "" {
			t.Fatal("anthropic-version header should not reach Codex upstream")
		}
		requestBody, _ := io.ReadAll(r.Body)
		var request map[string]any
		if err := json.Unmarshal(requestBody, &request); err != nil {
			t.Fatal(err)
		}
		if _, ok := request["input"]; !ok || request["stream"] != true || request["store"] != false {
			t.Fatalf("request was not translated to Codex: %s", requestBody)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\"},\"content_index\":0,\"output_index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"translated hello\"}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\",\"stop_reason\":\"stop\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3},\"output\":[]}}\n\n")
	}))
	defer upstreamServer.Close()

	upstreams := []*upstream.Upstream{{ID: 1, BaseURL: upstreamServer.URL, APIKey: "k", Protocol: "codex", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 1)
	body := []byte(`{"model":"gpt-test","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("anthropic-version", "2023-06-01")
	result := fwd.Forward(recorder, request, body, 1, "")

	response := recorder.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "translated hello", "event: message_stop"} {
		if !strings.Contains(response, want) {
			t.Fatalf("translated stream missing %q: %s", want, response)
		}
	}
	if recorder.Code != http.StatusOK || result.Outcome != OutcomeSuccess || len(result.Attempts) != 1 {
		t.Fatalf("status=%d result=%+v body=%s", recorder.Code, result, response)
	}
}

func TestForwardTranslatesClaudeClientErrorEnvelope(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"invalid prompt"}}`)
	}))
	defer upstreamServer.Close()

	upstreams := []*upstream.Upstream{{ID: 1, BaseURL: upstreamServer.URL, APIKey: "k", Protocol: "claude", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 1)
	body := []byte(`{"model":"claude-test","input":"hello","stream":false}`)
	recorder := httptest.NewRecorder()
	result := fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)), body, 1, "")

	var response struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadRequest || response.Error.Message != "invalid prompt" || response.Error.Type != "invalid_request_error" {
		t.Fatalf("status=%d response=%s", recorder.Code, recorder.Body.String())
	}
	if result.Outcome != OutcomeClientError {
		t.Fatalf("result = %+v", result)
	}
}

func TestForwardTranslatesClaudeStreamBeforeCommit(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, event := range []string{
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":2}}}\n\n",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"stream hello\"}}\n\n",
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n",
			"data: {\"type\":\"message_stop\"}\n\n",
		} {
			io.WriteString(w, event)
			flusher.Flush()
		}
	}))
	defer upstreamServer.Close()

	upstreams := []*upstream.Upstream{{ID: 1, BaseURL: upstreamServer.URL, APIKey: "k", Protocol: "claude", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 1)
	body := []byte(`{"model":"claude-test","input":"hello","stream":true}`)
	recorder := httptest.NewRecorder()
	result := fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)), body, 1, "")

	response := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(response, "response.created") || !strings.Contains(response, "stream hello") {
		t.Fatalf("translated stream status=%d body=%s", recorder.Code, response)
	}
	if !result.StreamCompleted {
		t.Fatalf("translated stream should complete: %+v", result)
	}
}

func TestProtocolMismatchDoesNotConsumeRetryBudget(t *testing.T) {
	var backupHits int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer backup.Close()

	upstreams := []*upstream.Upstream{
		{ID: 1, BaseURL: "http://unused.invalid", APIKey: "k", Protocol: "openai-response", Priority: 1, Weight: 1},
		{ID: 2, BaseURL: backup.URL, APIKey: "k", Protocol: "passthrough", Priority: 2, Weight: 1},
	}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 1)
	body := []byte(`{"model":"claude-test","messages":[],"stream":false}`)
	recorder := httptest.NewRecorder()
	result := fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)), body, 1, "")

	if recorder.Code != http.StatusOK || backupHits != 1 {
		t.Fatalf("backup was not used, status=%d hits=%d body=%s", recorder.Code, backupHits, recorder.Body.String())
	}
	if len(result.Attempts) != 2 || result.Attempts[0].ErrorKind != "protocol_unsupported" {
		t.Fatalf("attempts = %+v", result.Attempts)
	}
	if hm.EffectiveState(1) != "CLOSED" {
		t.Fatal("protocol mismatch must not affect channel health")
	}
}

// 连续验证完整请求链：首选渠道正常时命中首选，故障时切到备用，
// 主动探测确认恢复后，下一个请求立即回到高优先级渠道。
func TestForwardStrictPriorityFailoverAndAutomaticFailback(t *testing.T) {
	var primaryFails atomic.Bool
	var primaryHits, backupHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		if primaryFails.Load() {
			http.Error(w, "primary unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"source":"primary"}`)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"source":"backup"}`)
	}))
	defer backup.Close()

	upstreams := []*upstream.Upstream{
		{ID: 1, Name: "primary", BaseURL: primary.URL, APIKey: "k", Protocol: "passthrough", Priority: 1, Weight: 1},
		{ID: 2, Name: "backup", BaseURL: backup.URL, APIKey: "k", Protocol: "passthrough", Priority: 2, Weight: 1},
	}
	hm := health.New(1, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 2)
	body := []byte(`{"model":"gpt-test","stream":false}`)
	forwardOnce := func() (Result, string) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		result := fwd.Forward(recorder, request, body, 1, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		return result, recorder.Body.String()
	}

	first, firstBody := forwardOnce()
	if first.FinalUpstreamID != 1 || len(first.Attempts) != 1 || !strings.Contains(firstBody, "primary") {
		t.Fatalf("initial request should use primary: result=%+v body=%s", first, firstBody)
	}

	primaryFails.Store(true)
	second, secondBody := forwardOnce()
	if second.FinalUpstreamID != 2 || len(second.Attempts) != 2 || !strings.Contains(secondBody, "backup") {
		t.Fatalf("failed primary should switch to backup: result=%+v body=%s", second, secondBody)
	}
	if hm.EffectiveState(1) != "OPEN" {
		t.Fatalf("primary should be open after failure, state=%s", hm.EffectiveState(1))
	}

	primaryFails.Store(false)
	hm.ObserveProbe(1, "gpt-test", true, 20)
	hm.ObserveProbe(1, "gpt-test", true, 18)
	third, thirdBody := forwardOnce()
	if third.FinalUpstreamID != 1 || len(third.Attempts) != 1 || !strings.Contains(thirdBody, "primary") {
		t.Fatalf("recovered primary should receive next request: result=%+v body=%s", third, thirdBody)
	}
	if primaryHits.Load() != 3 || backupHits.Load() != 1 {
		t.Fatalf("unexpected hit counts: primary=%d backup=%d", primaryHits.Load(), backupHits.Load())
	}
}

// 验证换源后仍从原始客户端正文重新翻译，备用 Claude 渠道能够接管
// Responses 请求并将流式上游结果聚合为非流式客户端响应。
func TestForwardFailoverToTranslatedClaudeUpstream(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "primary unavailable", http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("translated backup path = %q", r.URL.Path)
		}
		requestBody, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(requestBody), `"messages"`) || !strings.Contains(string(requestBody), `"stream":true`) {
			t.Fatalf("backup request was not translated to Claude: %s", requestBody)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_backup\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":2}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"translated backup\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer backup.Close()

	upstreams := []*upstream.Upstream{
		{ID: 1, BaseURL: primary.URL, APIKey: "k", Protocol: "passthrough", Priority: 1, Weight: 1},
		{ID: 2, BaseURL: backup.URL, APIKey: "k", Protocol: "claude", Priority: 2, Weight: 1},
	}
	hm := health.New(1, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return upstreams }, hm), hm, 2)
	body := []byte(`{"model":"claude-test","input":"hello","stream":false}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	result := fwd.Forward(recorder, request, body, 1, "")

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "translated backup") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if result.FinalUpstreamID != 2 || len(result.Attempts) != 2 || result.Attempts[0].Outcome != OutcomeFailed {
		t.Fatalf("result = %+v", result)
	}
	if backupHits.Load() != 1 {
		t.Fatalf("backup hits = %d", backupHits.Load())
	}
}

// recordingHealth 记录每次 Report 的 ok 值，用于断言「流未跑完不得报成功」。
type recordingHealth struct {
	mu    sync.Mutex
	oks   []bool
	inner *health.Manager
}

func (r *recordingHealth) Report(id int64, model string, ok bool, latencyMs int64) {
	r.mu.Lock()
	r.oks = append(r.oks, ok)
	r.mu.Unlock()
	r.inner.Report(id, model, ok, latencyMs)
}
func (r *recordingHealth) ReleaseClaim(id int64) { r.inner.ReleaseClaim(id) }
func (r *recordingHealth) MarkModelUnsupported(id int64, model string) {
	r.inner.MarkModelUnsupported(id, model)
}
func (r *recordingHealth) MarkModelSupported(id int64, model string) {
	r.inner.MarkModelSupported(id, model)
}
func (r *recordingHealth) EffectiveState(id int64) string { return r.inner.EffectiveState(id) }
func (r *recordingHealth) reported() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.oks...)
}

// 上游以 200 开流后在流中投递 error 事件时，不得向熔断器报成功——
// 否则一个总在半路报错的渠道会被当成健康渠道，熔断永不触发。
// 注意：单纯「没有终止事件」不算失败（见 TestForwardSSECleanEOFAfterFirstByteIsTransparent），
// 这里依据的是上游明确的 error 事件。
func TestForwardErroredStreamIsNotReportedHealthy(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		flusher.Flush()
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n")
		flusher.Flush()
		io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
		flusher.Flush()
	}))
	defer upSrv.Close()

	ups := []*upstream.Upstream{{ID: 1, BaseURL: upSrv.URL, APIKey: "k", Protocol: "passthrough", Priority: 1, Weight: 1, Enabled: true}}
	hm := &recordingHealth{inner: health.New(3, time.Hour)}
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm.inner), hm, 1)

	recorder := httptest.NewRecorder()
	body := []byte(`{"model":"claude-test","stream":true}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	result := fwd.Forward(recorder, request, body, 1, "")

	if result.Outcome != OutcomePartial {
		t.Fatalf("outcome = %q, want %q for a stream that errored mid-flight", result.Outcome, OutcomePartial)
	}
	if result.ErrorKind != "stream_error" {
		t.Fatalf("error kind = %q, want stream_error. Result: %+v", result.ErrorKind, result)
	}
	for _, ok := range hm.reported() {
		if ok {
			t.Fatalf("errored stream must not report success to the breaker, got reports %v", hm.reported())
		}
	}
}

// 完整流（有 message_stop）仍必须报成功，避免上一条修复把健康渠道全判失败。
func TestForwardCompletedStreamStillReportsHealthy(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upSrv.Close()

	ups := []*upstream.Upstream{{ID: 1, BaseURL: upSrv.URL, APIKey: "k", Protocol: "passthrough", Priority: 1, Weight: 1, Enabled: true}}
	hm := &recordingHealth{inner: health.New(3, time.Hour)}
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm.inner), hm, 1)

	recorder := httptest.NewRecorder()
	body := []byte(`{"model":"claude-test","stream":true}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	result := fwd.Forward(recorder, request, body, 1, "")

	if !result.StreamCompleted || result.Outcome != OutcomeSuccess {
		t.Fatalf("completed stream should stay success: %+v", result)
	}
	reports := hm.reported()
	if len(reports) != 1 || !reports[0] {
		t.Fatalf("completed stream must report success once, got %v", reports)
	}
}

// 非流式客户端 + 流式上游：翻译结果是错误信封时必须换源，
// 不能把 {"error":...} 当成正常响应回给客户端。
func TestForwardSSEToBodyErrorPayloadFailsOver(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_bad\",\"usage\":{\"input_tokens\":1}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	}))
	defer failing.Close()
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ok\",\"usage\":{\"input_tokens\":1}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recovered\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer backup.Close()

	ups := []*upstream.Upstream{
		{ID: 1, BaseURL: failing.URL, APIKey: "k", Protocol: "claude", Priority: 1, Weight: 1, Enabled: true},
		{ID: 2, BaseURL: backup.URL, APIKey: "k", Protocol: "claude", Priority: 2, Weight: 1, Enabled: true},
	}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 2)

	recorder := httptest.NewRecorder()
	body := []byte(`{"model":"claude-test","input":"hi","stream":false}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	result := fwd.Forward(recorder, request, body, 1, "")

	if backupHits.Load() != 1 {
		t.Fatalf("error envelope should have triggered failover, backup hits = %d", backupHits.Load())
	}
	if result.FinalUpstreamID != 2 {
		t.Fatalf("final upstream = %d, want 2. Result: %+v", result.FinalUpstreamID, result)
	}
	if !strings.Contains(recorder.Body.String(), "recovered") {
		t.Fatalf("client should receive the backup response, got: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "Overloaded") {
		t.Fatalf("error envelope must not reach the client: %s", recorder.Body.String())
	}
}
