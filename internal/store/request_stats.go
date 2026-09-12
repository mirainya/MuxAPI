package store

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type LogFilterOptions struct {
	Models     []string            `json:"models"`
	Groups     []string            `json:"groups"`
	Keys       []string            `json:"keys"`
	Endpoints  []string            `json:"endpoints"`
	ErrorKinds []string            `json:"error_kinds"`
	Upstreams  []LogUpstreamOption `json:"upstreams"`
}

type LogUpstreamOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (s *Store) LogFilterOptions() (*LogFilterOptions, error) {
	opt := &LogFilterOptions{
		Models: []string{}, Groups: []string{}, Keys: []string{}, Endpoints: []string{},
		ErrorKinds: []string{}, Upstreams: []LogUpstreamOption{},
	}
	collect := func(q string) ([]string, error) {
		rows, err := s.query(q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				return nil, err
			}
			if value != "" {
				out = append(out, value)
			}
		}
		return out, rows.Err()
	}
	var err error
	if opt.Models, err = collect(`SELECT DISTINCT model FROM requests WHERE model<>'' ORDER BY model`); err != nil {
		return nil, err
	}
	if opt.Groups, err = collect(`SELECT DISTINCT g.name FROM requests r JOIN groups g ON g.id=r.group_id ORDER BY g.name`); err != nil {
		return nil, err
	}
	if opt.Keys, err = collect(`SELECT DISTINCT key_name FROM requests WHERE key_name<>'' ORDER BY key_name`); err != nil {
		return nil, err
	}
	if opt.Endpoints, err = collect(`SELECT DISTINCT endpoint FROM requests WHERE endpoint<>'' ORDER BY endpoint`); err != nil {
		return nil, err
	}
	if opt.ErrorKinds, err = collect(`SELECT DISTINCT error_kind FROM requests WHERE error_kind<>'' ORDER BY error_kind`); err != nil {
		return nil, err
	}
	rows, err := s.query(`SELECT DISTINCT a.upstream_id,COALESCE(u.name,'')
		FROM request_attempts a LEFT JOIN upstreams u ON u.id=a.upstream_id
		WHERE a.upstream_id>0 ORDER BY a.upstream_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item LogUpstreamOption
		if err := rows.Scan(&item.ID, &item.Name); err != nil {
			return nil, err
		}
		if item.Name == "" {
			item.Name = fmt.Sprintf("#%d", item.ID)
		}
		opt.Upstreams = append(opt.Upstreams, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return opt, nil
}

type RequestStats struct {
	Total               int64   `json:"total"`
	DirectSuccess       int64   `json:"direct_success"`
	FailoverSuccess     int64   `json:"failover_success"`
	Failed              int64   `json:"failed"`
	Partial             int64   `json:"partial"`
	Canceled            int64   `json:"canceled"`
	ClientError         int64   `json:"client_error"`
	Retried             int64   `json:"retried"`
	SuccessRate         float64 `json:"success_rate"`
	P50TTFTMs           int64   `json:"p50_ttft_ms"`
	P95TTFTMs           int64   `json:"p95_ttft_ms"`
	P95DurationMs       int64   `json:"p95_duration_ms"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheInputTokens    int64   `json:"cache_input_tokens"`
	CacheRate           float64 `json:"cache_rate"`
}

func (s *Store) RequestStats(filter RequestFilter) (*RequestStats, error) {
	where, args := s.requestWhere(filter, false)
	if s.postgres {
		stats := &RequestStats{}
		err := s.queryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN r.outcome='success' AND r.attempt_count<=1 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.outcome='success' AND r.attempt_count>1 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.outcome NOT IN ('success','partial','canceled','client_error') THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.outcome='partial' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.outcome='canceled' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.outcome='client_error' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.attempt_count>1 THEN 1 ELSE 0 END),0),
			COALESCE(CAST(percentile_cont(0.50) WITHIN GROUP (ORDER BY r.ttft_ms)
				FILTER (WHERE r.ttft_ms>0) AS BIGINT),0),
			COALESCE(CAST(percentile_cont(0.95) WITHIN GROUP (ORDER BY r.ttft_ms)
				FILTER (WHERE r.ttft_ms>0) AS BIGINT),0),
			COALESCE(CAST(percentile_cont(0.95) WITHIN GROUP (ORDER BY r.duration_ms)
				FILTER (WHERE r.duration_ms>0) AS BIGINT),0),
			COALESCE(SUM(r.input_tokens),0),COALESCE(SUM(r.output_tokens),0),COALESCE(SUM(r.cached_tokens),0),
			COALESCE(SUM(r.cache_creation_tokens),0),
			COALESCE(SUM(CASE WHEN r.input_tokens_normalized OR LOWER(COALESCE(u.protocol,'')) IN ('claude','anthropic','messages','anthropic-messages')
				THEN r.input_tokens+r.cached_tokens+r.cache_creation_tokens
				WHEN LOWER(COALESCE(u.protocol,'')) IN ('openai','chat','chat_completions','chat-completions','openai-response','openai_responses','responses','codex','gemini','google','generativelanguage','generatecontent')
				THEN r.input_tokens+r.cache_creation_tokens ELSE r.input_tokens+r.cached_tokens+r.cache_creation_tokens END),0)
			FROM requests r LEFT JOIN groups g ON g.id=r.group_id
			LEFT JOIN upstreams u ON u.id=r.final_upstream_id`+where, args...).Scan(
			&stats.Total, &stats.DirectSuccess, &stats.FailoverSuccess, &stats.Failed,
			&stats.Partial, &stats.Canceled, &stats.ClientError, &stats.Retried,
			&stats.P50TTFTMs, &stats.P95TTFTMs, &stats.P95DurationMs,
			&stats.InputTokens, &stats.OutputTokens, &stats.CachedTokens,
			&stats.CacheCreationTokens, &stats.CacheInputTokens,
		)
		if err != nil {
			return nil, err
		}
		if stats.Total > 0 {
			stats.SuccessRate = float64(stats.DirectSuccess+stats.FailoverSuccess) / float64(stats.Total)
		}
		stats.CacheRate = tokenCacheRate(stats.CachedTokens, stats.CacheInputTokens)
		return stats, nil
	}
	rows, err := s.query(`SELECT r.outcome,r.attempt_count,r.ttft_ms,r.duration_ms,
		r.input_tokens,r.input_tokens_normalized,r.output_tokens,r.cached_tokens,r.cache_creation_tokens,COALESCE(u.protocol,'')
		FROM requests r LEFT JOIN groups g ON g.id=r.group_id
		LEFT JOIN upstreams u ON u.id=r.final_upstream_id`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := &RequestStats{}
	var ttfts, durations []int64
	for rows.Next() {
		var outcome string
		var attempts int
		var ttft, duration, input, output, cached, cacheCreation int64
		var inputNormalized bool
		var protocol string
		if err := rows.Scan(&outcome, &attempts, &ttft, &duration, &input, &inputNormalized, &output, &cached, &cacheCreation, &protocol); err != nil {
			return nil, err
		}
		stats.Total++
		if attempts > 1 {
			stats.Retried++
		}
		switch outcome {
		case "success":
			if attempts > 1 {
				stats.FailoverSuccess++
			} else {
				stats.DirectSuccess++
			}
		case "partial":
			stats.Partial++
		case "canceled":
			stats.Canceled++
		case "client_error":
			stats.ClientError++
		default:
			stats.Failed++
		}
		if ttft > 0 {
			ttfts = append(ttfts, ttft)
		}
		if duration > 0 {
			durations = append(durations, duration)
		}
		stats.InputTokens += input
		stats.OutputTokens += output
		stats.CachedTokens += cached
		stats.CacheCreationTokens += cacheCreation
		stats.CacheInputTokens += cacheInputTokens(input, cached, cacheCreation, protocol, inputNormalized)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if stats.Total > 0 {
		stats.SuccessRate = float64(stats.DirectSuccess+stats.FailoverSuccess) / float64(stats.Total)
	}
	stats.P50TTFTMs = percentile(ttfts, 0.50)
	stats.P95TTFTMs = percentile(ttfts, 0.95)
	stats.P95DurationMs = percentile(durations, 0.95)
	stats.CacheRate = tokenCacheRate(stats.CachedTokens, stats.CacheInputTokens)
	return stats, nil
}

type ChannelCacheStats struct {
	UpstreamID          int64   `json:"upstream_id"`
	UpstreamName        string  `json:"upstream_name"`
	UsageRequests       int64   `json:"usage_requests"`
	InputTokens         int64   `json:"input_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheRate           float64 `json:"cache_rate"`
}

// RequestCacheStats 按实际尝试的渠道汇总缓存 Token，重试会归属到对应渠道。
func (s *Store) RequestCacheStats(filter RequestFilter) ([]ChannelCacheStats, error) {
	where, args := s.requestWhere(filter, false)
	if filter.UpstreamID > 0 {
		where += " AND a.upstream_id=?"
		args = append(args, filter.UpstreamID)
	}
	rows, err := s.query(`SELECT a.upstream_id,COALESCE(u.name,''),
		COALESCE(SUM(CASE WHEN a.input_tokens>0 OR a.cached_tokens>0 OR a.cache_creation_tokens>0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN a.input_tokens_normalized OR LOWER(COALESCE(u.protocol,'')) IN ('claude','anthropic','messages','anthropic-messages')
			THEN a.input_tokens+a.cached_tokens+a.cache_creation_tokens
			WHEN LOWER(COALESCE(u.protocol,'')) IN ('openai','chat','chat_completions','chat-completions','openai-response','openai_responses','responses','codex','gemini','google','generativelanguage','generatecontent')
			THEN a.input_tokens+a.cache_creation_tokens ELSE a.input_tokens+a.cached_tokens+a.cache_creation_tokens END),0),
		COALESCE(SUM(a.cached_tokens),0),COALESCE(SUM(a.cache_creation_tokens),0)
		FROM request_attempts a
		JOIN requests r ON r.request_id=a.request_id
		LEFT JOIN upstreams u ON u.id=a.upstream_id
		LEFT JOIN groups g ON g.id=r.group_id`+where+`
		GROUP BY a.upstream_id,u.name
		ORDER BY 4 DESC,2 ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChannelCacheStats{}
	for rows.Next() {
		var item ChannelCacheStats
		if err := rows.Scan(&item.UpstreamID, &item.UpstreamName, &item.UsageRequests,
			&item.InputTokens, &item.CachedTokens, &item.CacheCreationTokens); err != nil {
			return nil, err
		}
		item.CacheRate = tokenCacheRate(item.CachedTokens, item.InputTokens)
		out = append(out, item)
	}
	return out, rows.Err()
}

func tokenCacheRate(cached, input int64) float64 {
	if input <= 0 {
		return 0
	}
	rate := float64(cached) / float64(input)
	if rate > 1 {
		return 1
	}
	return rate
}

func cacheInputTokens(input, cached, cacheCreation int64, protocol string, normalized bool) int64 {
	if normalized || isExclusiveInputProtocol(protocol) {
		return input + cached + cacheCreation
	}
	if isKnownInclusiveInputProtocol(protocol) {
		// Legacy OpenAI/Responses rows stored input_tokens inclusively.
		return input + cacheCreation
	}
	return input + cached + cacheCreation
}

func isExclusiveInputProtocol(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "claude", "anthropic", "messages", "anthropic-messages":
		return true
	default:
		return false
	}
}

func isKnownInclusiveInputProtocol(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "openai", "chat", "chat_completions", "chat-completions", "openai-response", "openai_responses", "responses", "codex", "gemini", "google", "generativelanguage", "generatecontent":
		return true
	default:
		return false
	}
}

func percentile(values []int64, quantile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := int(float64(len(values)-1)*quantile + 0.5)
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

// RouteSample 一条历史路由样本，供启动时重建渠道的延迟 EWMA。
type RouteSample struct {
	UpstreamID int64
	Model      string
	OK         bool
	LatencyMs  int64
}

// RecentSamples 只回放会影响路由健康的成功或失败尝试。
func (s *Store) RecentSamples(limit int) ([]RouteSample, error) {
	if limit <= 0 {
		limit = 2000
	}
	rows, err := s.query(`SELECT a.upstream_id,r.model,a.outcome,a.ttft_ms FROM
		(SELECT id,request_id,upstream_id,outcome,ttft_ms FROM request_attempts
		 WHERE upstream_id>0 AND outcome IN ('success','failed') ORDER BY id DESC LIMIT ?) a
		JOIN requests r ON r.request_id=a.request_id ORDER BY a.id ASC`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RouteSample
	for rows.Next() {
		var s RouteSample
		var outcome string
		if err := rows.Scan(&s.UpstreamID, &s.Model, &outcome, &s.LatencyMs); err != nil {
			return nil, err
		}
		s.OK = outcome == "success"
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) ListRequests(limit int) ([]*RequestEntry, error) {
	page, err := s.ListRequestsPage(RequestFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	for _, entry := range page.Entries {
		entry.Attempts, err = s.listRequestAttempts(entry.RequestID)
		if err != nil {
			return nil, err
		}
	}
	return page.Entries, nil
}

// PruneRequests 每轮分批删除超过 keepDays 天的请求及其尝试记录。
func (s *Store) PruneRequests(keepDays, batch int) (int64, error) {
	if keepDays <= 0 || batch <= 0 {
		return 0, nil
	}
	cutoff := s.timeValue(time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour))
	// 先删关联的 attempts，再删 requests（不依赖外键级联）
	s.exec(`DELETE FROM request_attempts WHERE request_id IN (
		SELECT request_id FROM requests WHERE created_at < ? ORDER BY created_at LIMIT ?
	)`, cutoff, batch)
	res, err := s.exec(`DELETE FROM requests WHERE id IN (
		SELECT id FROM requests WHERE created_at < ? ORDER BY created_at LIMIT ?
	)`, cutoff, batch)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
