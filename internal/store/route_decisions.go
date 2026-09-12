package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RouteDecisionRecord is the durable explanation for one routing choice.
// Cost pointers distinguish an authoritative zero from an unavailable price.
type RouteDecisionRecord struct {
	RequestID             string
	GroupID               int64
	Model                 string
	Protocol              string
	Endpoint              string
	SessionKey            string
	PrefixHash            string
	CacheKey              string
	Strategy              string
	Reason                string
	SelectedUpstreamID    int64
	ForecastWindow        time.Duration
	ForecastRequests      float64
	EstimatedInputTokens  int64
	ReusablePrefixTokens  int64
	EstimatedOutputTokens int64
	SelectedCost          *float64
	NoCacheCost           *float64
	CacheCost             *float64
	EstimatedSavings      *float64
	Confidence            float64
	CacheSelected         bool
	Exploration           bool
	CreatedAt             time.Time
	Candidates            []RouteCandidateRecord
}

// RouteCandidateRecord stores the complete forecast considered for one
// upstream. APIKeyHash is an irreversible credential identity used to keep
// provider caches isolated across API-key rotations.
type RouteCandidateRecord struct {
	UpstreamID          int64
	APIKeyHash          string
	UpstreamName        string
	Protocol            string
	Priority            int
	Eligible            bool
	Selected            bool
	RejectionReason     string
	PricingSource       string
	PricingConfidence   float64
	CacheSupported      bool
	CacheExisting       bool
	CacheSelected       bool
	CacheHitRate        float64
	ForecastTotalCost   *float64
	ForecastNoCacheCost *float64
	ForecastCacheCost   *float64
	EstimatedSavings    *float64
	BreakEvenRequests   *float64
	ExpectedHits        float64
	ExpectedMisses      float64
	ExpectedCreates     float64
	EstimatedTTFTMs     float64
	EstimatedDurationMs float64
	SuccessRate         float64
	Details             json.RawMessage
}

type RouteDecisionOutcome struct {
	ActualCost                  *float64
	ActualInputTokens           *int64
	ActualInputTokensNormalized *bool
	ActualOutputTokens          *int64
	ActualCachedTokens          *int64
	ActualCacheCreationTokens   *int64
	// ActualUpstreamID is the upstream whose usage is being recorded. It may
	// differ from RouteDecisionRecord.SelectedUpstreamID when the initial pick
	// failed and a failover attempt succeeded. Zero means unknown / unchanged.
	ActualUpstreamID int64
	Outcome          string
	CompletedAt      time.Time
}

type RouteDecisionEntry struct {
	ID                          int64                 `json:"id"`
	RequestID                   string                `json:"request_id"`
	GroupID                     int64                 `json:"group_id"`
	Model                       string                `json:"model"`
	Protocol                    string                `json:"protocol"`
	Endpoint                    string                `json:"endpoint"`
	SessionKey                  string                `json:"session_key,omitempty"`
	PrefixHash                  string                `json:"prefix_hash,omitempty"`
	CacheKey                    string                `json:"cache_key,omitempty"`
	Strategy                    string                `json:"strategy"`
	Reason                      string                `json:"reason"`
	SelectedUpstreamID          int64                 `json:"selected_upstream_id"`
	SelectedUpstreamName        string                `json:"selected_upstream"`
	CandidateCount              int                   `json:"candidate_count"`
	ForecastWindowSeconds       int64                 `json:"forecast_window_seconds"`
	ForecastRequests            float64               `json:"forecast_requests"`
	EstimatedInputTokens        int64                 `json:"estimated_input_tokens"`
	ReusablePrefixTokens        int64                 `json:"reusable_prefix_tokens"`
	EstimatedOutputTokens       int64                 `json:"estimated_output_tokens"`
	SelectedCost                *float64              `json:"selected_cost,omitempty"`
	NoCacheCost                 *float64              `json:"no_cache_cost,omitempty"`
	CacheCost                   *float64              `json:"cache_cost,omitempty"`
	EstimatedSavings            *float64              `json:"estimated_savings,omitempty"`
	Confidence                  float64               `json:"confidence"`
	CacheSelected               bool                  `json:"cache_selected"`
	Exploration                 bool                  `json:"exploration"`
	ActualCost                  *float64              `json:"actual_cost,omitempty"`
	ActualInputTokens           *int64                `json:"actual_input_tokens,omitempty"`
	ActualInputTokensNormalized bool                  `json:"-"`
	ActualPromptTokens          *int64                `json:"actual_prompt_tokens,omitempty"`
	ActualOutputTokens          *int64                `json:"actual_output_tokens,omitempty"`
	ActualCachedTokens          *int64                `json:"actual_cached_tokens,omitempty"`
	ActualCacheCreationTokens   *int64                `json:"actual_cache_creation_tokens,omitempty"`
	ActualOutcome               string                `json:"actual_outcome,omitempty"`
	CreatedAt                   int64                 `json:"created_at"`
	CompletedAt                 int64                 `json:"completed_at,omitempty"`
	Candidates                  []RouteCandidateEntry `json:"candidates,omitempty"`
}

type RouteCandidateEntry struct {
	ID                  int64           `json:"id"`
	UpstreamID          int64           `json:"upstream_id"`
	APIKeyHash          string          `json:"api_key_hash,omitempty"`
	UpstreamName        string          `json:"upstream_name"`
	Protocol            string          `json:"protocol"`
	Priority            int             `json:"priority"`
	Eligible            bool            `json:"eligible"`
	Selected            bool            `json:"selected"`
	RejectionReason     string          `json:"rejection_reason,omitempty"`
	PricingSource       string          `json:"pricing_source,omitempty"`
	PricingConfidence   float64         `json:"pricing_confidence"`
	CacheSupported      bool            `json:"cache_supported"`
	CacheExisting       bool            `json:"cache_existing"`
	CacheSelected       bool            `json:"cache_selected"`
	CacheHitRate        float64         `json:"cache_hit_rate"`
	ForecastTotalCost   *float64        `json:"forecast_total_cost,omitempty"`
	ForecastNoCacheCost *float64        `json:"forecast_no_cache_cost,omitempty"`
	ForecastCacheCost   *float64        `json:"forecast_cache_cost,omitempty"`
	EstimatedSavings    *float64        `json:"estimated_savings,omitempty"`
	BreakEvenRequests   *float64        `json:"break_even_requests,omitempty"`
	ExpectedHits        float64         `json:"expected_hits"`
	ExpectedMisses      float64         `json:"expected_misses"`
	ExpectedCreates     float64         `json:"expected_creates"`
	EstimatedTTFTMs     float64         `json:"estimated_ttft_ms"`
	EstimatedDurationMs float64         `json:"estimated_duration_ms"`
	SuccessRate         float64         `json:"success_rate"`
	Details             json.RawMessage `json:"details,omitempty"`
}

type RouteDecisionFilter struct {
	BeforeID           int64
	Limit              int
	GroupID            int64
	SelectedUpstreamID int64
	Model              string
	SessionKey         string
	PrefixHash         string
	Since              time.Time
	Until              time.Time
	IncludeCandidates  bool
}

func normalizeDecisionRecord(record *RouteDecisionRecord) error {
	record.RequestID = strings.TrimSpace(record.RequestID)
	if record.RequestID == "" {
		return errors.New("route decision request id is required")
	}
	if record.Strategy == "" {
		record.Strategy = "cost"
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	if record.ForecastWindow < 0 {
		record.ForecastWindow = 0
	}
	if record.ForecastRequests < 0 {
		record.ForecastRequests = 0
	}
	if record.Confidence < 0 {
		record.Confidence = 0
	} else if record.Confidence > 1 {
		record.Confidence = 1
	}
	seen := make(map[int64]struct{}, len(record.Candidates))
	for i := range record.Candidates {
		candidate := &record.Candidates[i]
		if _, exists := seen[candidate.UpstreamID]; exists {
			return fmt.Errorf("duplicate route candidate upstream %d", candidate.UpstreamID)
		}
		seen[candidate.UpstreamID] = struct{}{}
		if candidate.PricingConfidence < 0 {
			candidate.PricingConfidence = 0
		} else if candidate.PricingConfidence > 1 {
			candidate.PricingConfidence = 1
		}
		if len(candidate.Details) == 0 {
			candidate.Details = json.RawMessage(`{}`)
		} else if !json.Valid(candidate.Details) {
			return fmt.Errorf("route candidate %d details are not valid JSON", candidate.UpstreamID)
		}
	}
	return nil
}

// SaveRouteDecision creates or replaces the forecast for one request. It is
// idempotent by request ID so retrying an audit write cannot duplicate rows.
// A previously recorded actual outcome is retained when the forecast is
// replaced.
func (s *Store) SaveRouteDecision(record RouteDecisionRecord) (int64, error) {
	if err := normalizeDecisionRecord(&record); err != nil {
		return 0, err
	}
	tx, err := s.beginTx()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var decisionID int64
	err = tx.QueryRow(`INSERT INTO route_decisions(
		request_id,group_id,model,protocol,endpoint,session_key,prefix_hash,cache_key,strategy,reason,
		selected_upstream_id,candidate_count,forecast_window_seconds,forecast_requests,
		estimated_input_tokens,reusable_prefix_tokens,estimated_output_tokens,selected_cost,no_cache_cost,
		cache_cost,estimated_savings,confidence,cache_selected,exploration,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(request_id) DO UPDATE SET
		group_id=excluded.group_id,model=excluded.model,protocol=excluded.protocol,endpoint=excluded.endpoint,
		session_key=excluded.session_key,prefix_hash=excluded.prefix_hash,cache_key=excluded.cache_key,
		strategy=excluded.strategy,reason=excluded.reason,selected_upstream_id=excluded.selected_upstream_id,
		candidate_count=excluded.candidate_count,forecast_window_seconds=excluded.forecast_window_seconds,
		forecast_requests=excluded.forecast_requests,estimated_input_tokens=excluded.estimated_input_tokens,
		reusable_prefix_tokens=excluded.reusable_prefix_tokens,estimated_output_tokens=excluded.estimated_output_tokens,
		selected_cost=excluded.selected_cost,no_cache_cost=excluded.no_cache_cost,cache_cost=excluded.cache_cost,
		estimated_savings=excluded.estimated_savings,confidence=excluded.confidence,
		cache_selected=excluded.cache_selected,exploration=excluded.exploration,created_at=excluded.created_at
		RETURNING id`,
		record.RequestID, record.GroupID, record.Model, record.Protocol, record.Endpoint, record.SessionKey,
		record.PrefixHash, record.CacheKey, record.Strategy, record.Reason, record.SelectedUpstreamID,
		len(record.Candidates), int64(record.ForecastWindow/time.Second), record.ForecastRequests,
		record.EstimatedInputTokens, record.ReusablePrefixTokens, record.EstimatedOutputTokens,
		record.SelectedCost, record.NoCacheCost, record.CacheCost, record.EstimatedSavings, record.Confidence,
		record.CacheSelected, record.Exploration, s.timeValue(record.CreatedAt)).Scan(&decisionID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM route_decision_candidates WHERE decision_id=?`, decisionID); err != nil {
		return 0, err
	}
	for _, candidate := range record.Candidates {
		details := string(candidate.Details)
		if _, err := tx.Exec(`INSERT INTO route_decision_candidates(
			decision_id,upstream_id,api_key_hash,upstream_name,protocol,priority,eligible,selected,rejection_reason,
			pricing_source,pricing_confidence,cache_supported,cache_existing,cache_selected,cache_hit_rate,
			forecast_total_cost,forecast_no_cache_cost,forecast_cache_cost,estimated_savings,break_even_requests,
			expected_hits,expected_misses,expected_creates,estimated_ttft_ms,estimated_duration_ms,success_rate,details_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			decisionID, candidate.UpstreamID, candidate.APIKeyHash, candidate.UpstreamName, candidate.Protocol,
			candidate.Priority, candidate.Eligible, candidate.Selected, candidate.RejectionReason,
			candidate.PricingSource, candidate.PricingConfidence, candidate.CacheSupported,
			candidate.CacheExisting, candidate.CacheSelected, candidate.CacheHitRate,
			candidate.ForecastTotalCost, candidate.ForecastNoCacheCost, candidate.ForecastCacheCost,
			candidate.EstimatedSavings, candidate.BreakEvenRequests, candidate.ExpectedHits,
			candidate.ExpectedMisses, candidate.ExpectedCreates, candidate.EstimatedTTFTMs,
			candidate.EstimatedDurationMs, candidate.SuccessRate, details); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return decisionID, nil
}

// CompleteRouteDecision attaches authoritative usage/cost after forwarding.
// Nil values leave an earlier value unchanged, making partial reconciliation
// safe when provider cost arrives later than token usage.
func (s *Store) CompleteRouteDecision(requestID string, outcome RouteDecisionOutcome) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return errors.New("route decision request id is required")
	}
	if outcome.CompletedAt.IsZero() {
		outcome.CompletedAt = time.Now()
	}
	var actualUpstream *int64
	if outcome.ActualUpstreamID > 0 {
		actualUpstream = &outcome.ActualUpstreamID
	}
	result, err := s.exec(`UPDATE route_decisions SET
		actual_cost=COALESCE(?,actual_cost),actual_input_tokens=COALESCE(?,actual_input_tokens),
		actual_input_tokens_normalized=COALESCE(?,actual_input_tokens_normalized),
		actual_output_tokens=COALESCE(?,actual_output_tokens),actual_cached_tokens=COALESCE(?,actual_cached_tokens),
		actual_cache_creation_tokens=COALESCE(?,actual_cache_creation_tokens),
		actual_upstream_id=COALESCE(?,actual_upstream_id),
		actual_outcome=CASE WHEN ?='' THEN actual_outcome ELSE ? END,completed_at=?
		WHERE request_id=?`, outcome.ActualCost, outcome.ActualInputTokens, outcome.ActualInputTokensNormalized,
		outcome.ActualOutputTokens, outcome.ActualCachedTokens, outcome.ActualCacheCreationTokens, actualUpstream,
		outcome.Outcome, outcome.Outcome,
		s.timeValue(outcome.CompletedAt), requestID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) routeDecisionSelect(where string) string {
	return `SELECT rd.id,rd.request_id,rd.group_id,rd.model,rd.protocol,rd.endpoint,rd.session_key,rd.prefix_hash,rd.cache_key,rd.strategy,
		rd.reason,rd.selected_upstream_id,COALESCE(u.name,''),rd.candidate_count,rd.forecast_window_seconds,rd.forecast_requests,
		rd.estimated_input_tokens,rd.reusable_prefix_tokens,rd.estimated_output_tokens,rd.selected_cost,rd.no_cache_cost,
		rd.cache_cost,rd.estimated_savings,rd.confidence,rd.cache_selected,rd.exploration,rd.actual_cost,rd.actual_input_tokens,
		rd.actual_input_tokens_normalized,CASE WHEN rd.actual_input_tokens IS NULL THEN NULL
		WHEN rd.actual_input_tokens_normalized THEN rd.actual_input_tokens+COALESCE(rd.actual_cached_tokens,0)+COALESCE(rd.actual_cache_creation_tokens,0)
		WHEN LOWER(COALESCE(NULLIF(au.protocol,''),NULLIF(u.protocol,''),rd.protocol,'')) IN ('claude','anthropic','messages','anthropic-messages') THEN rd.actual_input_tokens+COALESCE(rd.actual_cached_tokens,0)+COALESCE(rd.actual_cache_creation_tokens,0)
		ELSE rd.actual_input_tokens END,
		rd.actual_output_tokens,rd.actual_cached_tokens,rd.actual_cache_creation_tokens,rd.actual_outcome,` +
		s.unixExpr("rd.created_at") + `,COALESCE(` + s.unixExpr("rd.completed_at") + `,0)
		FROM route_decisions rd LEFT JOIN upstreams u ON u.id=rd.selected_upstream_id
		LEFT JOIN upstreams au ON au.id=rd.actual_upstream_id` + where
}

func scanRouteDecision(scanner rowScanner) (*RouteDecisionEntry, error) {
	entry := &RouteDecisionEntry{}
	err := scanner.Scan(&entry.ID, &entry.RequestID, &entry.GroupID, &entry.Model, &entry.Protocol,
		&entry.Endpoint, &entry.SessionKey, &entry.PrefixHash, &entry.CacheKey, &entry.Strategy,
		&entry.Reason, &entry.SelectedUpstreamID, &entry.SelectedUpstreamName, &entry.CandidateCount, &entry.ForecastWindowSeconds,
		&entry.ForecastRequests, &entry.EstimatedInputTokens, &entry.ReusablePrefixTokens,
		&entry.EstimatedOutputTokens, &entry.SelectedCost, &entry.NoCacheCost, &entry.CacheCost,
		&entry.EstimatedSavings, &entry.Confidence, &entry.CacheSelected, &entry.Exploration,
		&entry.ActualCost, &entry.ActualInputTokens, &entry.ActualInputTokensNormalized, &entry.ActualPromptTokens, &entry.ActualOutputTokens, &entry.ActualCachedTokens,
		&entry.ActualCacheCreationTokens, &entry.ActualOutcome, &entry.CreatedAt, &entry.CompletedAt)
	return entry, err
}

func (s *Store) GetRouteDecisionByRequestID(requestID string) (*RouteDecisionEntry, error) {
	entry, err := scanRouteDecision(s.queryRow(s.routeDecisionSelect(" WHERE rd.request_id=?"), requestID))
	if err != nil {
		return nil, err
	}
	entry.Candidates, err = s.listRouteCandidates(entry.ID)
	return entry, err
}

func (s *Store) GetRouteDecision(id int64) (*RouteDecisionEntry, error) {
	entry, err := scanRouteDecision(s.queryRow(s.routeDecisionSelect(" WHERE rd.id=?"), id))
	if err != nil {
		return nil, err
	}
	entry.Candidates, err = s.listRouteCandidates(entry.ID)
	return entry, err
}

func (s *Store) listRouteCandidates(decisionID int64) ([]RouteCandidateEntry, error) {
	rows, err := s.query(`SELECT id,upstream_id,api_key_hash,upstream_name,protocol,priority,eligible,
		selected,rejection_reason,pricing_source,pricing_confidence,cache_supported,cache_existing,
		cache_selected,cache_hit_rate,forecast_total_cost,forecast_no_cache_cost,forecast_cache_cost,
		estimated_savings,break_even_requests,expected_hits,expected_misses,expected_creates,
		estimated_ttft_ms,estimated_duration_ms,success_rate,details_json
		FROM route_decision_candidates WHERE decision_id=? ORDER BY selected DESC,id`, decisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []RouteCandidateEntry{}
	for rows.Next() {
		var entry RouteCandidateEntry
		var details string
		if err := rows.Scan(&entry.ID, &entry.UpstreamID, &entry.APIKeyHash, &entry.UpstreamName,
			&entry.Protocol, &entry.Priority, &entry.Eligible, &entry.Selected, &entry.RejectionReason,
			&entry.PricingSource, &entry.PricingConfidence, &entry.CacheSupported, &entry.CacheExisting,
			&entry.CacheSelected, &entry.CacheHitRate, &entry.ForecastTotalCost, &entry.ForecastNoCacheCost,
			&entry.ForecastCacheCost, &entry.EstimatedSavings, &entry.BreakEvenRequests, &entry.ExpectedHits,
			&entry.ExpectedMisses, &entry.ExpectedCreates, &entry.EstimatedTTFTMs,
			&entry.EstimatedDurationMs, &entry.SuccessRate, &details); err != nil {
			return nil, err
		}
		if json.Valid([]byte(details)) {
			entry.Details = json.RawMessage(details)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// ListRouteDecisions returns newest decisions first. Candidate details are
// omitted unless explicitly requested to keep history queries inexpensive.
func (s *Store) ListRouteDecisions(filter RouteDecisionFilter) ([]*RouteDecisionEntry, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var where strings.Builder
	where.WriteString(" WHERE 1=1")
	var args []any
	if filter.BeforeID > 0 {
		where.WriteString(" AND rd.id<?")
		args = append(args, filter.BeforeID)
	}
	if filter.GroupID > 0 {
		where.WriteString(" AND rd.group_id=?")
		args = append(args, filter.GroupID)
	}
	if filter.SelectedUpstreamID > 0 {
		where.WriteString(" AND rd.selected_upstream_id=?")
		args = append(args, filter.SelectedUpstreamID)
	}
	if filter.Model != "" {
		where.WriteString(" AND rd.model=?")
		args = append(args, filter.Model)
	}
	if filter.SessionKey != "" {
		where.WriteString(" AND rd.session_key=?")
		args = append(args, filter.SessionKey)
	}
	if filter.PrefixHash != "" {
		where.WriteString(" AND rd.prefix_hash=?")
		args = append(args, filter.PrefixHash)
	}
	if !filter.Since.IsZero() {
		where.WriteString(" AND rd.created_at>=?")
		args = append(args, s.timeValue(filter.Since))
	}
	if !filter.Until.IsZero() {
		where.WriteString(" AND rd.created_at<?")
		args = append(args, s.timeValue(filter.Until))
	}
	query := s.routeDecisionSelect(where.String()) + " ORDER BY rd.id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []*RouteDecisionEntry{}
	for rows.Next() {
		entry, err := scanRouteDecision(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if filter.IncludeCandidates {
		for _, entry := range entries {
			entry.Candidates, err = s.listRouteCandidates(entry.ID)
			if err != nil {
				return nil, err
			}
		}
	}
	return entries, nil
}
