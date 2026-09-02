// Package upstream 定义上游配置、请求构建、代理连接和错误分类。
package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Upstream 上游抽象。MVP 只做 relay 中转透传一种实现。
// 上游是全局凭证池成员；Priority/Weight 是「组内视图」字段——
// 由 store 从 group_upstreams 中间表 JOIN 填充，表示该上游在某分组内的调度策略。
type Upstream struct {
	ID           int64
	Name         string
	Source       string // 管理视图中的来源/供应商；不参与路由判定
	PrimaryTagID int64
	TagIDs       []int64
	Tags         []Tag
	TagsSet      bool
	GroupIDs     []int64 // 创建请求附带：要同时加入的分组 id（仅 admin 创建路径使用）
	BaseURL      string  // 中转站地址
	APIKey       string  // 透传凭证
	Proxy        string  // 转发/探测走的代理出口(空=按环境变量或直连)
	Protocol     string  // 上游协议；passthrough 保持客户端请求格式
	BillingType  string  // 计费接口类型：none/auto/sub2api/newapi
	CreditRatio  float64 // 储值积分倍率（默认 1）：充1元得 N 元积分时填 N；路由倍率 = 上报倍率 / CreditRatio
	CacheMode    string  // 缓存策略：auto(观察后启用) / enabled / disabled
	Priority     int     // 组内视图：越小越优先
	Weight       int     // 组内视图：同优先级层分流权重
	Enabled      bool
	ChannelProbe bool // 兼容旧数据；熔断固定为渠道级
}

const (
	BillingNone    = "none"
	BillingAuto    = "auto"
	BillingSub2API = "sub2api"
	BillingNewAPI  = "newapi"
	CacheAuto      = "auto"
	CacheEnabled   = "enabled"
	CacheDisabled  = "disabled"
)

// NormalizeBillingType validates the management-facing billing adapter name.
func NormalizeBillingType(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", BillingNone:
		return BillingNone, true
	case BillingAuto:
		return BillingAuto, true
	case BillingSub2API:
		return BillingSub2API, true
	case BillingNewAPI:
		return BillingNewAPI, true
	default:
		return "", false
	}
}

// NormalizeCacheMode validates the per-upstream cache capability policy.
func NormalizeCacheMode(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", CacheAuto:
		return CacheAuto, true
	case CacheEnabled:
		return CacheEnabled, true
	case CacheDisabled:
		return CacheDisabled, true
	default:
		return "", false
	}
}

// Tag is user-managed upstream metadata. IsPrimary controls visual grouping;
// other tags are filters and never affect routing or health.
type Tag struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	IsPrimary bool   `json:"is_primary,omitempty"`
}

// ProxyTransport 按代理 URL 构建 Transport：空则回退到环境变量(HTTPS_PROXY)，解析失败也回退。
// 每次新建一个独立 Transport，供探测/拉模型这类一次性短连接使用。
func ProxyTransport(proxy string) *http.Transport {
	t := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if proxy != "" {
		if p, err := url.Parse(proxy); err == nil {
			t.Proxy = http.ProxyURL(p)
		}
	}
	return t
}

// sharedTransports 按代理出口缓存复用的 Transport（key=proxy 字符串）。
// 转发热路径每请求新建 Transport 会泄漏连接/fd（旧实现），改为按出口共享一份、
// 带空闲连接回收，long-run 不再耗尽资源。
var sharedTransports sync.Map // proxy string -> *http.Transport

// SharedTransport 取（或惰性建）该代理出口共享的 Transport，供转发热路径复用。
// 设 IdleConnTimeout=90s、MaxIdleConnsPerHost=100 让空闲连接及时回收又能复用；
// 注意：不在此设 ResponseHeaderTimeout（它随容忍线动态变化），由调用方在 context/client 层控制。
func SharedTransport(proxy string) *http.Transport {
	if v, ok := sharedTransports.Load(proxy); ok {
		return v.(*http.Transport)
	}
	t := ProxyTransport(proxy)
	t.IdleConnTimeout = 90 * time.Second
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 100
	// LoadOrStore 防并发重复建：多个 goroutine 同时 miss 时只留一份。
	actual, _ := sharedTransports.LoadOrStore(proxy, t)
	return actual.(*http.Transport)
}

// NewTransport 该上游专属共享 Transport（含其代理出口），转发热路径复用以免连接泄漏。
func (u *Upstream) NewTransport() *http.Transport { return SharedTransport(u.Proxy) }

// IsFailureStatus reports channel-level failures that should trigger failover
// and contribute to the single upstream breaker.
func IsFailureStatus(code int) bool {
	if code >= 500 {
		return true
	}
	switch code {
	case http.StatusUnauthorized, // 401
		http.StatusPaymentRequired, // 402
		http.StatusForbidden,       // 403
		http.StatusRequestTimeout,  // 408
		http.StatusTooManyRequests: // 429
		return true
	}
	return false
}

// FailIsUpstreamLevel remains for compatibility. All breaker failures are now
// channel-level; model capability mismatches are handled separately.
func FailIsUpstreamLevel(code int) bool {
	return IsFailureStatus(code)
}

// IsModelUnsupported identifies deterministic model/channel mismatches. They
// exclude only this model for a short TTL and never affect channel health.
func IsModelUnsupported(code int, model, body string) bool {
	if model == "" {
		return false
	}
	if code != http.StatusBadRequest && code != http.StatusNotFound && code != http.StatusServiceUnavailable {
		return false
	}
	lower := strings.ToLower(body)
	for _, marker := range []string{
		"model_not_found",
		"model not found",
		"model does not exist",
		"unknown model",
		"unsupported model",
		"no available channel",
		"模型不存在",
		"不支持该模型",
		"不支持模型",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// IsErrorPayload detects successful-status JSON envelopes that still contain
// an upstream error. Null or empty error fields are not treated as failures.
func IsErrorPayload(body []byte) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return false
	}
	if raw, ok := object["error"]; ok {
		value := strings.TrimSpace(string(raw))
		switch value {
		case "", "null", "false", `""`, "{}", "[]":
		default:
			return true
		}
	}
	var responseType string
	if raw, ok := object["type"]; ok && json.Unmarshal(raw, &responseType) == nil {
		return strings.EqualFold(responseType, "error")
	}
	return false
}

// BuildRequest 构建发往上游的请求：黑名单透传客户端请求头（保留 User-Agent/x-stainless-* 等），
// 由 Go Transport 管理响应压缩，确保转发层能审计解压后的 SSE。
// 仅覆盖凭证为上游 key、重算 Content-Length。URL = TrimSuffix(base_url,"/") + 客户端原始路径(path)。
func (u *Upstream) BuildRequest(method, path string, body io.Reader, clientHeader http.Header) (*http.Request, error) {
	url := strings.TrimSuffix(u.BaseURL, "/") + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range clientHeader { // 全量透传客户端请求头
		if isHopByHopHeader(k) || strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		req.Header[k] = vs
	}
	req.Header.Del("Content-Length") // 由 body 重新计算，避免与原始长度冲突
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// 覆盖凭证为上游自己的 key（客户端那个是 sub2api 发的，上游不认）
	req.Header.Set("Authorization", "Bearer "+u.APIKey)
	req.Header.Set("x-api-key", u.APIKey)
	req.Header.Set("x-goog-api-key", u.APIKey)
	return req, nil
}

func isHopByHopHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

// FetchModels 实时拉该上游的 /v1/models，解析 OpenAI 风格 {"data":[{"id":...}]}
// 返回模型 ID 列表。既用于上游连通测试，也用于下游 /v1/models 汇总。
// 返回 (models, status, error)：status 是上游 HTTP 状态码（网络错误为 0）。
// ctx 让调用方在客户端断开时立即放弃；Transport 走共享池，避免每次拉取都新建连接池。
func (u *Upstream) FetchModels(ctx context.Context, timeout time.Duration) ([]string, int, error) {
	modelsPath := "/v1/models"
	if strings.EqualFold(strings.TrimSpace(u.Protocol), "gemini") {
		modelsPath = "/v1beta/models"
	}
	req, err := u.BuildRequest(http.MethodGet, modelsPath, nil, http.Header{})
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(ctx)
	client := &http.Client{Transport: SharedTransport(u.Proxy)}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	json.Unmarshal(body, &parsed)
	models := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	for _, m := range parsed.Models {
		if id := strings.TrimPrefix(strings.TrimSpace(m.Name), "models/"); id != "" {
			models = append(models, id)
		}
	}
	return models, resp.StatusCode, nil
}

// HTTPError 上游返回非 2xx 时携带状态码与响应体片段。
type HTTPError struct {
	Status int
	Body   string
}

// Error 返回上游响应体片段，便于管理接口展示真实失败原因。
func (e *HTTPError) Error() string { return e.Body }
