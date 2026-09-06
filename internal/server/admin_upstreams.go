package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/billing"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// upstreamDTO 对外视图：api_key 脱敏，不回显完整凭证。
type upstreamDTO struct {
	ID           int64                `json:"id"`
	Name         string               `json:"name"`
	Source       string               `json:"source"`
	PrimaryTagID int64                `json:"primary_tag_id"`
	TagIDs       []int64              `json:"tag_ids"`
	Tags         []upstream.Tag       `json:"tags"`
	BaseURL      string               `json:"base_url"`
	Proxy        string               `json:"proxy"`
	Protocol     string               `json:"protocol"`
	BillingType  string               `json:"billing_type"`
	CacheMode    string               `json:"cache_mode"`
	CreditRatio  float64              `json:"credit_ratio"`
	APIKey       string               `json:"api_key,omitempty"` // 输入用；输出时脱敏到 masked
	Masked       string               `json:"masked,omitempty"`
	Enabled      bool                 `json:"enabled"`
	ChannelProbe bool                 `json:"channel_probe"`          // 兼容旧数据；熔断固定为渠道级
	Health       healthView           `json:"health"`                 // 运行时健康（仅 GET 列表填充）
	ModelHealth  []modelHealthView    `json:"model_health,omitempty"` // 模型级健康（仅 GET 列表填充，无则省略）
	Billing      *store.BillingStatus `json:"billing,omitempty"`
}

func mask(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

// adminUpstreams 处理上游全局池的列表与创建。
func (s *Server) adminUpstreams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.List()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var billingStates map[int64]store.BillingStatus
		if r.URL.Query().Get("view") == "overview" {
			billingStates, err = s.store.ListBillingStatusesLite()
		} else {
			billingStates, err = s.store.ListBillingStatuses()
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out := make([]upstreamDTO, 0, len(list))
		for _, u := range list {
			item := upstreamDTO{
				ID: u.ID, Name: u.Name, Source: u.Source, BaseURL: u.BaseURL, Proxy: u.Proxy, Protocol: u.Protocol, BillingType: u.BillingType, CacheMode: u.CacheMode,
				PrimaryTagID: u.PrimaryTagID, TagIDs: u.TagIDs, Tags: u.Tags,
				Masked: mask(u.APIKey), Enabled: u.Enabled, ChannelProbe: u.ChannelProbe, CreditRatio: u.CreditRatio,
				Health:      toHealthView(s.health.Snapshot(u.ID), s.health.EffectiveState(u.ID)),
				ModelHealth: toModelHealthViews(s.health.ModelStates(u.ID)),
			}
			if u.BillingType != upstream.BillingNone {
				state, ok := billingStates[u.ID]
				if !ok {
					state = store.BillingStatus{UpstreamID: u.ID, Currency: "USD", Status: "pending"}
				}
				item.Billing = &state
			}
			out = append(out, item)
		}
		writeJSON(w, out)
	case http.MethodPost:
		u, err := decodeUpstream(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.Create(u); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(201)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) adminUpstreamItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/upstreams/")
	parts := strings.Split(rest, "/")
	if parts[0] == "batch" && r.Method == http.MethodPost {
		s.batchUpdateUpstreams(w, r)
		return
	}
	if parts[0] == "reorder" && r.Method == http.MethodPost {
		s.reorderUpstreams(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if len(parts) == 2 && parts[1] == "models" { // 连通测试 + 拉模型
		s.testUpstream(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "models" && parts[2] == "recover" {
		s.recoverUpstreamModel(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "test" { // 真实对话测试(SSE流式回显)
		s.testUpstreamChat(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "recover" {
		s.recoverUpstream(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "billing" && parts[2] == "refresh" {
		s.refreshUpstreamBilling(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "billing" && parts[2] == "multiplier" && r.Method == http.MethodPut {
		s.setUpstreamBillingMultiplier(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "billing" && parts[2] == "audit" {
		s.upstreamBillingAudit(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "monitors" && r.Method == http.MethodPost { // 批量建监控
		s.batchCreateMonitors(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodPut:
		u, err := decodeUpstream(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		u.ID = id
		if err := s.store.Update(u); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	case http.MethodDelete:
		if err := s.store.Delete(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// 删库后同步丢弃内存态，否则熔断器与模型缓存会随删除累积。
		s.health.Forget(id)
		s.forgetUpstreamModels(id)
		w.WriteHeader(204)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) refreshUpstreamBilling(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.billingMgr == nil {
		http.Error(w, "billing manager is unavailable", http.StatusServiceUnavailable)
		return
	}
	state, err := s.billingMgr.Refresh(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "upstream not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, billing.ErrBillingDisabled) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil && state.UpstreamID == 0 {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, state)
}

// setUpstreamBillingMultiplier 手动录入一次倍率，等价于"人工探测结果"。
// 直接改 upstream_billing_status 的 effective_multiplier + group_multiplier，
// 下次 auto-refresh 若从上游扣费日志拿到 group_ratio 会自然覆盖(那是权威值)。
// 用途：上游面板显示的公示价 ≠ muxapi 从扣费日志推出的实际倍率时，先手动填正确的
// 分组价把路由决策拉回正轨，等有真实请求跑过后 auto-refresh 会拿到更准的实际扣费价。
func (s *Server) setUpstreamBillingMultiplier(w http.ResponseWriter, r *http.Request, id int64) {
	var body struct {
		Multiplier float64 `json:"multiplier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Multiplier <= 0 {
		http.Error(w, "multiplier must be greater than zero", http.StatusBadRequest)
		return
	}
	item, err := s.store.Get(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "upstream not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if item.BillingType == upstream.BillingNone || item.BillingType == "" {
		http.Error(w, "billing is disabled for this upstream", http.StatusBadRequest)
		return
	}
	if err := s.store.SetBillingMultiplier(id, body.Multiplier); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state, err := s.store.GetBillingStatus(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, state)
}

// upstreamBillingAudit 返回指定时间窗口的费用比对。单个刷新间隔的样本量太小、
// 且结论会被下一轮采集覆盖，故聚合到小时以上再判定，窗口可由调用方选择。
func (s *Server) upstreamBillingAudit(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.store.Get(id); errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "upstream not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	window := store.LookupBillingWindow(r.URL.Query().Get("window"))
	audit, err := s.store.BillingAuditRange(id, window, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	options := make([]map[string]any, 0, len(store.BillingWindows()))
	for _, item := range store.BillingWindows() {
		options = append(options, map[string]any{
			"key": item.Key, "label": item.Label,
			"seconds": int64(item.Duration / time.Second),
		})
	}
	writeJSON(w, map[string]any{
		"window":  window.Key,
		"label":   window.Label,
		"windows": options,
		"audit":   audit,
	})
}

// recoverUpstream clears only the in-memory channel breaker. Historical
// routing statistics and model capability exclusions remain intact.
func (s *Server) recoverUpstream(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.store.Get(id); errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "upstream not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.health.ResetCircuit(id)
	writeJSON(w, toHealthView(s.health.Snapshot(id), s.health.EffectiveState(id)))
}

// recoverUpstreamModel clears one temporary model capability exclusion.
func (s *Server) recoverUpstreamModel(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.store.Get(id); errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "upstream not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var input struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	input.Model = strings.TrimSpace(input.Model)
	if input.Model == "" || len([]rune(input.Model)) > 256 {
		http.Error(w, "model must be 1-256 characters", http.StatusBadRequest)
		return
	}
	if err := s.health.RecoverModel(id, input.Model); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) batchUpdateUpstreams(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs          []int64 `json:"ids"`
		Enabled      *bool   `json:"enabled"`
		PrimaryTagID *int64  `json:"primary_tag_id"`
		AddTagIDs    []int64 `json:"add_tag_ids"`
		RemoveTagIDs []int64 `json:"remove_tag_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	update := store.UpstreamBatchUpdate{
		Enabled: in.Enabled, PrimaryTagID: in.PrimaryTagID,
		AddTagIDs: in.AddTagIDs, RemoveTagIDs: in.RemoveTagIDs,
	}
	if err := s.store.BatchUpdateUpstreams(in.IDs, update); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var tagColors = map[string]bool{
	"gray": true, "green": true, "amber": true, "red": true,
	"blue": true, "purple": true, "pink": true, "teal": true,
	"cyan": true, "indigo": true, "orange": true, "lime": true,
	// extended palette
	"rose": true, "emerald": true, "sky": true,
	"violet": true, "fuchsia": true, "yellow": true,
}

func decodeTag(r *http.Request) (string, string, error) {
	var in struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return "", "", err
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Color = strings.TrimSpace(in.Color)
	if in.Name == "" || len([]rune(in.Name)) > 40 {
		return "", "", errors.New("tag name must be 1-40 characters")
	}
	if !tagColors[in.Color] {
		return "", "", errors.New("unsupported tag color")
	}
	return in.Name, in.Color, nil
}

func (s *Server) adminTags(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tags, err := s.store.ListTags()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, tags)
	case http.MethodPost:
		name, color, err := decodeTag(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		id, err := s.store.CreateTag(name, color)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]int64{"id": id})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) adminTagItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/admin/tags/"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", 400)
		return
	}
	switch r.Method {
	case http.MethodPut:
		name, color, err := decodeTag(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.UpdateTag(id, name, color); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := s.store.DeleteTag(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// testUpstream 实时拉该上游 /v1/models：既是连通测试，也返回模型列表。
func (s *Server) testUpstream(w http.ResponseWriter, r *http.Request, id int64) {
	u, err := s.store.Get(id)
	if err != nil {
		http.Error(w, "upstream not found", 404)
		return
	}
	type result struct {
		OK        bool     `json:"ok"`
		Status    int      `json:"status,omitempty"`
		LatencyMs int64    `json:"latency_ms"`
		Models    []string `json:"models,omitempty"`
		Error     string   `json:"error,omitempty"`
	}
	start := time.Now()
	models, status, err := u.FetchModels(r.Context(), 10*time.Second)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, result{Status: status, LatencyMs: lat, Error: err.Error()})
		return
	}
	writeJSON(w, result{OK: true, Status: status, LatencyMs: lat, Models: models})
}

// batchCreateMonitors 为某上游的一批模型批量建监控，已存在的跳过。
// body: {models:[], stream, probe_text, max_tokens, interval_sec, path, enabled}
func (s *Server) batchCreateMonitors(w http.ResponseWriter, r *http.Request, id int64) {
	var in struct {
		Models      []string `json:"models"`
		Enabled     bool     `json:"enabled"`
		Stream      bool     `json:"stream"`
		ProbeText   string   `json:"probe_text"`
		MaxTokens   int      `json:"max_tokens"`
		IntervalSec int      `json:"interval_sec"`
		Path        string   `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if len(in.Models) == 0 {
		http.Error(w, "no models selected", 400)
		return
	}
	// 校验 upstream 存在：与单建监控同口径，杜绝孤儿监控行
	if _, err := s.store.Get(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "upstream not found", 404)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	tmpl := store.Monitor{
		Enabled: in.Enabled, Stream: in.Stream, ProbeText: strings.TrimSpace(in.ProbeText),
		MaxTokens: in.MaxTokens, IntervalSec: in.IntervalSec, Path: strings.TrimSpace(in.Path),
	}
	created, skipped, err := s.store.BatchCreateMonitors(id, in.Models, tmpl)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]int{"created": created, "skipped": skipped})
}
