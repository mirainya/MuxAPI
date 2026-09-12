package server

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/store"
)

var errBadMonitor = errors.New("monitor requires upstream_id and model")

// registerAdmin 集中注册管理接口，并统一套用管理员鉴权 + gzip 压缩。
// admin 响应体动辄几百 KB(如 /admin/routing/decisions?include_candidates=true
// 一次返回 ~750KB 裸 JSON)，公网走隧道时传输段是主要延迟；gzip 后能压到 ~1/8。
// 所有 admin 端点都是非流式 JSON，可以安全 gzip。
func (s *Server) registerAdmin(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.HandlerFunc { return gzipMiddleware(s.auth(h)) }
	mux.HandleFunc("/admin/upstreams", admin(s.adminUpstreams))            // GET 全局池 / POST 新增
	mux.HandleFunc("/admin/upstreams/", admin(s.adminUpstreamItem))        // PUT 改 / DELETE 删
	mux.HandleFunc("/admin/tags", admin(s.adminTags))                      // GET 列表 / POST 新增
	mux.HandleFunc("/admin/tags/", admin(s.adminTagItem))                  // PUT 改 / DELETE 删
	mux.HandleFunc("/admin/monitors", admin(s.adminMonitors))              // GET 监控列表 / POST 新增
	mux.HandleFunc("/admin/monitors/", admin(s.adminMonitorItem))          // PUT 改 / DELETE 删 / {id}/probe 立即探测
	mux.HandleFunc("/admin/groups", admin(s.adminGroups))                  // GET 列表 / POST 新增
	mux.HandleFunc("/admin/groups/", admin(s.adminGroupSub))               // /{id} 改/删 ; /{id}/upstreams 成员 ; /{id}/keys 密钥
	mux.HandleFunc("/admin/keys/", admin(s.adminKeyItem))                  // PUT 启停 / DELETE 删
	mux.HandleFunc("/admin/logs", admin(s.adminLogs))                      // GET 调用日志(游标分页+筛选)
	mux.HandleFunc("/admin/logs/stats", admin(s.adminLogStats))            // GET 当前筛选范围统计
	mux.HandleFunc("/admin/logs/cache-stats", admin(s.adminLogCacheStats)) // GET 按渠道汇总缓存命中率
	mux.HandleFunc("/admin/logs/options", admin(s.adminLogOptions))        // GET 筛选下拉选项(全量去重)
	mux.HandleFunc("/admin/logs/", admin(s.adminLogItem))                  // GET 单条请求完整尝试链
	mux.HandleFunc("/admin/routing/decisions", admin(s.adminRouteDecisions))
	mux.HandleFunc("/admin/routing/decisions/", admin(s.adminRouteDecisionItem))
	mux.HandleFunc("/admin/overview/summary", admin(s.adminOverviewSummary)) // GET 今日请求/本周费用汇总
	mux.HandleFunc("/admin/overview/trends", admin(s.adminOverviewTrends))   // GET 总览余额/成功率趋势
	mux.HandleFunc("/admin/settings", admin(s.adminSettings))                // GET/PUT 运行时设置
	mux.HandleFunc("/admin/backup", admin(s.adminBackup))                    // GET 列表 / POST 触发
	mux.HandleFunc("/admin/backup/", admin(s.adminBackup))                   // config / schedule / records/{id}
	mux.HandleFunc("/admin/updates", admin(s.adminUpdates))                  // GET 发行版 / POST 更新
	mux.HandleFunc("/admin/model-mappings", admin(s.adminModelMappings))     // GET 列表 / POST 新增
	mux.HandleFunc("/admin/model-mappings/", admin(s.adminModelMappingItem)) // DELETE 删
}

// adminLogs 返回调用日志，兼容游标和偏移量分页。
// 查询参: before(游标), offset(页偏移), limit(默认50,上限500)及业务筛选项。
func (s *Server) adminLogs(w http.ResponseWriter, r *http.Request) {
	page, err := s.store.ListRequestsPage(parseLogFilter(r.URL.Query()))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, page)
}

// parseLogFilter 将查询参数转换为存储层筛选条件，并限制分页大小。
func parseLogFilter(q url.Values) store.RequestFilter {
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	upstreamID, _ := strconv.ParseInt(q.Get("upstream_id"), 10, 64)
	slowMs, _ := strconv.ParseInt(q.Get("slow_ms"), 10, 64)
	sinceUnix, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	untilUnix, _ := strconv.ParseInt(q.Get("until"), 10, 64)
	filter := store.RequestFilter{
		BeforeID: before, Limit: limit, Offset: offset, Model: q.Get("model"), Group: q.Get("group"),
		Status: q.Get("status"), KeyName: q.Get("key"), Endpoint: q.Get("endpoint"),
		ErrorKind: q.Get("error_kind"), Query: strings.TrimSpace(q.Get("q")),
		Stream: q.Get("stream"), UpstreamID: upstreamID, Retried: q.Get("retried") == "true",
		SlowMs: slowMs,
	}
	if sinceUnix > 0 {
		filter.Since = time.Unix(sinceUnix, 0)
	}
	if untilUnix > 0 {
		filter.Until = time.Unix(untilUnix, 0)
	}
	return filter
}

func (s *Server) adminLogStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.RequestStats(parseLogFilter(r.URL.Query()))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, stats)
}

func (s *Server) adminLogCacheStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.RequestCacheStats(parseLogFilter(r.URL.Query()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, stats)
}

func (s *Server) adminLogItem(w http.ResponseWriter, r *http.Request) {
	idText := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/logs/"), "/")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid request record id", http.StatusBadRequest)
		return
	}
	entry, err := s.store.GetRequest(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "request record not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, entry)
}

// adminLogOptions 返回日志筛选下拉的全量去重选项(模型/分组)。
func (s *Server) adminLogOptions(w http.ResponseWriter, r *http.Request) {
	opt, err := s.store.LogFilterOptions()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, opt)
}

func (s *Server) adminRouteDecisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	items, err := s.store.ListRouteDecisions(parseRouteDecisionFilter(r.URL.Query()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"items": items})
}

func (s *Server) adminRouteDecisionItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	value := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/routing/decisions/"), "/")
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid route decision id", http.StatusBadRequest)
		return
	}
	entry, err := s.store.GetRouteDecision(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "route decision not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, entry)
}

func parseRouteDecisionFilter(q url.Values) store.RouteDecisionFilter {
	limit, _ := strconv.Atoi(q.Get("limit"))
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	groupID, _ := strconv.ParseInt(q.Get("group_id"), 10, 64)
	selected, _ := strconv.ParseInt(q.Get("selected_upstream_id"), 10, 64)
	filter := store.RouteDecisionFilter{Limit: limit, BeforeID: before, GroupID: groupID,
		SelectedUpstreamID: selected, Model: q.Get("model"), SessionKey: q.Get("session_key"),
		PrefixHash: q.Get("prefix_hash"), IncludeCandidates: q.Get("include_candidates") == "true"}
	if value, _ := strconv.ParseInt(q.Get("since"), 10, 64); value > 0 {
		filter.Since = time.Unix(value, 0)
	}
	if value, _ := strconv.ParseInt(q.Get("until"), 10, 64); value > 0 {
		filter.Until = time.Unix(value, 0)
	}
	return filter
}

func settingString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}

func settingValue(dbValue, def string) (string, string) {
	if d, err := time.ParseDuration(dbValue); err == nil && d > 0 {
		return dbValue, "settings"
	}
	return def, "default"
}

func stringSettingValue(dbValue, def string) (string, string) {
	if dbValue != "" {
		return dbValue, "settings"
	}
	return def, "default"
}

func intSettingValue(dbValue string, def int) (string, string) {
	if n, err := strconv.Atoi(dbValue); err == nil && n > 0 {
		return dbValue, "settings"
	}
	return strconv.Itoa(def), "default"
}

func intSettingValueAllowZero(dbValue string, def int) (string, string) {
	if n, err := strconv.Atoi(dbValue); err == nil && n >= 0 {
		return dbValue, "settings"
	}
	return strconv.Itoa(def), "default"
}
