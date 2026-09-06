package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/monitor"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// 回归：缓存过期瞬间的并发 /v1/models 请求必须共享同一次上游拉取，
// 否则单个客户端就能把 N×M 条上游连接打出来。
func TestListModelsDeduplicatesConcurrentFetches(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release // 卡住首个拉取，确保后续请求都撞在「在途」状态上
		w.Write([]byte(`{"data":[{"id":"claude-4"},{"id":"gpt-5.6"}]}`))
	}))
	defer provider.Close()

	st, _ := store.Open(":memory:")
	defer st.Close()
	if err := st.Create(&upstream.Upstream{Name: "A", BaseURL: provider.URL, APIKey: "k", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ups, _ := st.List()
	gid, _ := st.CreateGroup("g", "")
	for _, u := range ups {
		st.AddMember(gid, u.ID, 1, 1)
	}
	key, _ := st.CreateKey("k", gid)

	hm := health.New(1, time.Hour)
	sched := scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm)
	srv := New(forward.New(sched, hm, 3), "", st, hm, monitor.New(st), nil, 32<<20)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const callers = 8
	var wg sync.WaitGroup
	firstModel := make([]string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var payload struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&payload)
			if len(payload.Data) > 0 {
				firstModel[index] = payload.Data[0].ID
			}
		}(i)
	}
	time.Sleep(200 * time.Millisecond) // 让所有 goroutine 都进入等待
	close(release)
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Fatalf("concurrent callers must share one upstream fetch, got %d", got)
	}
	for i, id := range firstModel {
		if id != "claude-4" { // 排序后 claude-4 在 gpt-5.6 前
			t.Fatalf("caller %d got %q, want the shared result", i, id)
		}
	}
}

func TestListModelsKeepsAdvertisedModelUntilEveryProviderIsExcluded(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"glm-5"},{"id":"deepseek-v4"}]}`))
	}))
	defer provider.Close()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, name := range []string{"A", "B"} {
		if err := st.Create(&upstream.Upstream{Name: name, BaseURL: provider.URL, APIKey: "k", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	ups, _ := st.List()
	groupID, _ := st.CreateGroup("domestic", "")
	for _, item := range ups {
		st.AddMember(groupID, item.ID, 1, 1)
	}
	key, _ := st.CreateKey("client", groupID)

	hm := health.New(3, time.Hour)
	if err := hm.SetModelExclusionStore(st); err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm)
	srv := New(forward.New(sched, hm, 3), "", st, hm, monitor.New(st), nil, 32<<20)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	fetch := func() []string {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(payload.Data))
		for _, item := range payload.Data {
			ids = append(ids, item.ID)
		}
		return ids
	}

	hm.MarkModelUnsupported(ups[0].ID, "glm-5")
	if got := fetch(); len(got) != 2 {
		t.Fatalf("one healthy provider should keep glm-5 advertised: %v", got)
	}
	if !hm.IsModelUnsupported(ups[0].ID, "glm-5") {
		t.Fatal("positive /v1/models refresh cleared the durable exclusion")
	}
	hm.MarkModelUnsupported(ups[1].ID, "glm-5")
	if got := fetch(); len(got) != 1 || got[0] != "deepseek-v4" {
		t.Fatalf("fully excluded model should be hidden: %v", got)
	}
}
