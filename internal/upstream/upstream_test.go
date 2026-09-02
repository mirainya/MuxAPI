package upstream

import (
	"net/http"
	"strings"
	"testing"
)

func TestBuildRequestLetsTransportManageCompression(t *testing.T) {
	u := &Upstream{BaseURL: "https://example.com", APIKey: "upstream-key"}
	headers := http.Header{
		"Accept-Encoding": []string{"gzip, deflate, br"},
		"User-Agent":      []string{"Codex Desktop"},
	}
	req, err := u.BuildRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`), headers)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Accept-Encoding"); got != "" {
		t.Fatalf("client compression header must not reach upstream request: %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != "Codex Desktop" {
		t.Fatalf("ordinary client headers must remain forwarded: %q", got)
	}
}

func TestIsFailureStatus(t *testing.T) {
	// 应触发故障切换/熔断的状态码
	fail := []int{
		http.StatusUnauthorized,        // 401 凭证失效
		http.StatusPaymentRequired,     // 402 需付费
		http.StatusForbidden,           // 403 余额不足/无权限
		http.StatusRequestTimeout,      // 408 上游超时
		http.StatusTooManyRequests,     // 429 限流
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
	}
	for _, c := range fail {
		if !IsFailureStatus(c) {
			t.Errorf("状态码 %d 应判为上游失败", c)
		}
	}
	// 不应触发切换的状态码（成功 / 客户端请求本身的问题，透传给客户端）
	pass := []int{
		http.StatusOK,                    // 200
		http.StatusCreated,               // 201
		http.StatusBadRequest,            // 400 请求畸形/上下文超限
		http.StatusRequestEntityTooLarge, // 413 请求体过大
		http.StatusUnprocessableEntity,   // 422 请求参数无法处理
		http.StatusNotFound,              // 404 模型/路径不存在
	}
	for _, c := range pass {
		if IsFailureStatus(c) {
			t.Errorf("状态码 %d 不应判为上游失败", c)
		}
	}
}

func TestIsModelUnsupported(t *testing.T) {
	if IsModelUnsupported(http.StatusNotFound, "gpt-5.6", `{"error":"not found"}`) {
		t.Fatal("generic 404 must not create a model capability exclusion")
	}
	if !IsModelUnsupported(http.StatusNotFound, "gpt-5.6", `{"error":"model not found"}`) {
		t.Fatal("explicit model 404 should be treated as unsupported capability")
	}
	if !IsModelUnsupported(http.StatusBadRequest, "gpt-5.6", `{"code":"model_not_found"}`) {
		t.Fatal("explicit model_not_found should be treated as unsupported capability")
	}
	if IsModelUnsupported(http.StatusBadRequest, "gpt-5.6", `{"error":"invalid temperature"}`) {
		t.Fatal("ordinary request validation errors must not change model capability")
	}
	if IsModelUnsupported(http.StatusNotFound, "", `{"error":"not found"}`) {
		t.Fatal("missing model must not create a capability exclusion")
	}
}

func TestIsErrorPayload(t *testing.T) {
	for _, body := range []string{`{"error":{"message":"failed"}}`, `{"type":"error"}`} {
		if !IsErrorPayload([]byte(body)) {
			t.Fatalf("应识别错误响应: %s", body)
		}
	}
	for _, body := range []string{`{"ok":true}`, `{"error":null}`, `{"error":false}`} {
		if IsErrorPayload([]byte(body)) {
			t.Fatalf("不应误判正常响应: %s", body)
		}
	}
}
