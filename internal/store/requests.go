package store

import (
	"fmt"
	"strings"
	"time"
)

type RequestFilter struct {
	BeforeID   int64
	Limit      int
	Offset     int
	Model      string
	Group      string
	Status     string
	KeyName    string
	Endpoint   string
	ErrorKind  string
	Query      string
	Stream     string
	UpstreamID int64
	Since      time.Time
	Until      time.Time
	Retried    bool
	SlowMs     int64
}

func (s *Store) requestWhere(filter RequestFilter, includeCursor bool) (string, []any) {
	var where strings.Builder
	where.WriteString(" WHERE 1=1")
	var args []any
	if includeCursor && filter.BeforeID > 0 {
		where.WriteString(" AND r.id < ?")
		args = append(args, filter.BeforeID)
	}
	if filter.Model != "" {
		where.WriteString(" AND r.model = ?")
		args = append(args, filter.Model)
	}
	if filter.Group != "" {
		where.WriteString(" AND g.name = ?")
		args = append(args, filter.Group)
	}
	if filter.KeyName != "" {
		where.WriteString(" AND r.key_name = ?")
		args = append(args, filter.KeyName)
	}
	if filter.Endpoint != "" {
		where.WriteString(" AND r.endpoint = ?")
		args = append(args, filter.Endpoint)
	}
	if filter.ErrorKind != "" {
		where.WriteString(" AND r.error_kind = ?")
		args = append(args, filter.ErrorKind)
	}
	if filter.Query != "" {
		query := strings.ToLower(filter.Query)
		where.WriteString(" AND (LOWER(CAST(r.request_id AS TEXT)) LIKE ? OR LOWER(r.client_ip) LIKE ? OR LOWER(r.user_agent) LIKE ?)")
		args = append(args, query+"%", "%"+query+"%", "%"+query+"%")
	}
	if filter.UpstreamID > 0 {
		where.WriteString(" AND EXISTS (SELECT 1 FROM request_attempts af WHERE af.request_id=r.request_id AND af.upstream_id=?)")
		args = append(args, filter.UpstreamID)
	}
	if !filter.Since.IsZero() {
		where.WriteString(" AND r.created_at >= ?")
		args = append(args, s.timeValue(filter.Since))
	}
	if !filter.Until.IsZero() {
		where.WriteString(" AND r.created_at < ?")
		args = append(args, s.timeValue(filter.Until))
	}
	if filter.Retried {
		where.WriteString(" AND r.attempt_count > 1")
	}
	if filter.SlowMs > 0 {
		where.WriteString(" AND r.duration_ms >= ?")
		args = append(args, filter.SlowMs)
	}
	switch filter.Stream {
	case "stream":
		where.WriteString(" AND r.stream=TRUE")
	case "nonstream":
		where.WriteString(" AND r.stream=FALSE")
	}
	switch filter.Status {
	case "direct_success":
		where.WriteString(" AND r.outcome='success' AND r.attempt_count<=1")
	case "failover_success":
		where.WriteString(" AND r.outcome='success' AND r.attempt_count>1")
	case "failed":
		where.WriteString(" AND r.outcome IN ('failed','unavailable')")
	case "partial":
		where.WriteString(" AND r.outcome='partial'")
	case "canceled":
		where.WriteString(" AND r.outcome='canceled'")
	case "client_error":
		where.WriteString(" AND r.outcome='client_error'")
	case "ok":
		where.WriteString(" AND r.outcome='success'")
	case "fail":
		where.WriteString(" AND r.outcome<>'success'")
	}
	return where.String(), args
}

func (s *Store) requestSelect(where string) string {
	return fmt.Sprintf(`SELECT r.id,r.request_id,r.group_id,COALESCE(g.name,''),
		r.final_upstream_id,COALESCE(u.name,''),r.model,r.endpoint,r.key_name,r.client_ip,r.user_agent,r.status,r.outcome,
		r.ttft_ms,r.duration_ms,r.attempt_count,%s,%s,r.error_text,r.stream,r.request_bytes,
		r.response_bytes,r.input_tokens,r.input_tokens_normalized,r.output_tokens,r.cached_tokens,r.cache_creation_tokens,
		CASE WHEN r.input_tokens_normalized OR LOWER(COALESCE(u.protocol,'')) IN ('claude','anthropic','messages','anthropic-messages')
			THEN r.input_tokens+r.cached_tokens+r.cache_creation_tokens
			WHEN LOWER(COALESCE(u.protocol,'')) IN ('openai','chat','chat_completions','chat-completions','openai-response','openai_responses','responses','codex','gemini','google','generativelanguage','generatecontent')
			THEN r.input_tokens+r.cache_creation_tokens ELSE r.input_tokens+r.cached_tokens+r.cache_creation_tokens END,
		r.stream_completed,r.last_event,
		r.upstream_request_id,r.error_kind,r.error_source,
		COALESCE(rd.estimated_input_tokens,0),
		CASE WHEN rd.estimated_input_tokens>0 AND
			(r.input_tokens+r.cached_tokens+r.cache_creation_tokens)>0
			THEN CAST(r.input_tokens+r.cached_tokens+r.cache_creation_tokens AS REAL) /
				rd.estimated_input_tokens ELSE 0 END
		FROM requests r
		LEFT JOIN upstreams u ON u.id=r.final_upstream_id
		LEFT JOIN groups g ON g.id=r.group_id
		LEFT JOIN route_decisions rd ON rd.request_id=r.request_id%s`,
		s.unixExpr("r.created_at"), s.unixExpr("r.completed_at"), where)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRequestEntry(row rowScanner) (*RequestEntry, error) {
	e := &RequestEntry{}
	err := row.Scan(
		&e.ID, &e.RequestID, &e.GroupID, &e.GroupName, &e.FinalUpstreamID,
		&e.FinalUpstreamName, &e.Model, &e.Endpoint, &e.KeyName, &e.ClientIP, &e.UserAgent, &e.Status, &e.Outcome,
		&e.TTFTMs, &e.DurationMs, &e.AttemptCount, &e.CreatedAt, &e.CompletedAt, &e.Error,
		&e.Stream, &e.RequestBytes, &e.ResponseBytes, &e.InputTokens, &e.InputTokensNormalized, &e.OutputTokens,
		&e.CachedTokens, &e.CacheCreationTokens, &e.CacheInputTokens,
		&e.StreamCompleted, &e.LastEvent, &e.UpstreamRequestID,
		&e.ErrorKind, &e.ErrorSource, &e.EstimatedInputTokens, &e.TokenInflation,
	)
	e.CacheRate = tokenCacheRate(e.CachedTokens, e.CacheInputTokens)
	return e, err
}

func (s *Store) ListRequestsPage(filter RequestFilter) (*RequestPage, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	where, args := s.requestWhere(filter, true)
	q := s.requestSelect(where) + " ORDER BY r.id DESC LIMIT ?"
	args = append(args, limit+1)
	if filter.Offset > 0 {
		q += " OFFSET ?"
		args = append(args, filter.Offset)
	}
	rows, err := s.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &RequestPage{Entries: []*RequestEntry{}}
	for rows.Next() {
		e, err := scanRequestEntry(rows)
		if err != nil {
			return nil, err
		}
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		page.HasMore = true
	}
	if err := s.loadRouteSummaries(page.Entries); err != nil {
		return nil, err
	}
	if n := len(page.Entries); n > 0 {
		page.NextCursor = page.Entries[n-1].ID
	}
	return page, nil
}

func (s *Store) loadRouteSummaries(entries []*RequestEntry) error {
	if len(entries) == 0 {
		return nil
	}
	placeholders := make([]string, len(entries))
	args := make([]any, len(entries))
	byRequest := make(map[string]*RequestEntry, len(entries))
	for i, entry := range entries {
		placeholders[i] = "?"
		args[i] = entry.RequestID
		byRequest[entry.RequestID] = entry
	}
	q := `SELECT CAST(a.request_id AS TEXT),a.attempt_no,a.upstream_id,COALESCE(u.name,''),
		a.status,a.outcome,a.error_kind FROM request_attempts a
		LEFT JOIN upstreams u ON u.id=a.upstream_id
		WHERE CAST(a.request_id AS TEXT) IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY a.request_id,a.attempt_no`
	rows, err := s.query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var requestID string
		var step RequestRouteStep
		if err := rows.Scan(&requestID, &step.AttemptNo, &step.UpstreamID, &step.UpstreamName,
			&step.Status, &step.Outcome, &step.ErrorKind); err != nil {
			return err
		}
		if entry := byRequest[requestID]; entry != nil {
			entry.Route = append(entry.Route, step)
		}
	}
	return rows.Err()
}

func (s *Store) GetRequest(id int64) (*RequestEntry, error) {
	entry, err := scanRequestEntry(s.queryRow(s.requestSelect(" WHERE r.id=?"), id))
	if err != nil {
		return nil, err
	}
	entry.Attempts, err = s.listRequestAttempts(entry.RequestID)
	return entry, err
}

func (s *Store) listRequestAttempts(requestID string) ([]*RequestAttemptEntry, error) {
	q := fmt.Sprintf(`SELECT a.id,a.attempt_no,a.upstream_id,COALESCE(u.name,''),a.mapped_model,a.status,a.outcome,
		a.ttft_ms,a.duration_ms,%s,%s,a.error_text,a.priority,a.selection_reason,a.health_before,
		a.health_after,a.response_bytes,a.stream,a.stream_completed,a.last_event,a.input_tokens,
		a.output_tokens,a.cached_tokens,a.cache_creation_tokens,
		CASE WHEN a.input_tokens_normalized OR LOWER(COALESCE(u.protocol,'')) IN ('claude','anthropic','messages','anthropic-messages')
			THEN a.input_tokens+a.cached_tokens+a.cache_creation_tokens
			WHEN LOWER(COALESCE(u.protocol,'')) IN ('openai','chat','chat_completions','chat-completions','openai-response','openai_responses','responses','codex','gemini','google','generativelanguage','generatecontent')
			THEN a.input_tokens+a.cache_creation_tokens ELSE a.input_tokens+a.cached_tokens+a.cache_creation_tokens END,
		a.upstream_request_id,a.error_kind,a.error_source
		FROM request_attempts a LEFT JOIN upstreams u ON u.id=a.upstream_id
		WHERE a.request_id=? ORDER BY a.attempt_no`,
		s.unixExpr("a.created_at"), s.unixExpr("a.completed_at"))
	rows, err := s.query(q, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RequestAttemptEntry{}
	for rows.Next() {
		e := &RequestAttemptEntry{}
		if err := rows.Scan(&e.ID, &e.AttemptNo, &e.UpstreamID, &e.UpstreamName, &e.MappedModel, &e.Status,
			&e.Outcome, &e.TTFTMs, &e.DurationMs, &e.CreatedAt, &e.CompletedAt, &e.Error,
			&e.Priority, &e.SelectionReason, &e.HealthBefore, &e.HealthAfter, &e.ResponseBytes,
			&e.Stream, &e.StreamCompleted, &e.LastEvent, &e.InputTokens, &e.OutputTokens,
			&e.CachedTokens, &e.CacheCreationTokens, &e.CacheInputTokens,
			&e.UpstreamRequestID, &e.ErrorKind, &e.ErrorSource); err != nil {
			return nil, err
		}
		e.CacheRate = tokenCacheRate(e.CachedTokens, e.CacheInputTokens)
		out = append(out, e)
	}
	return out, rows.Err()
}
