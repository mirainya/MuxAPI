package store

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

const DefaultRoutingStatsWindow = 15 * time.Minute

// RoutingObservationRecord is the predictor-facing form of one completed
// upstream attempt. APIKeyHash must identify the upstream credential without
// storing it; callers should use a stable cryptographic hash.
type RoutingObservationRecord struct {
	RequestID           string
	AttemptNo           int
	GroupID             int64
	UpstreamID          int64
	APIKeyHash          string
	Model               string
	SessionKey          string
	PrefixHash          string
	CacheKey            string
	PrefixTokens        int64
	InputTokens         int64
	OutputTokens        int64
	CachedTokens        int64
	CacheCreationTokens int64
	TTFTMs              int64
	DurationMs          int64
	Success             bool
	CacheEligible       bool
	CacheHit            bool
	CacheCreated        bool
	CacheTTL            time.Duration
	CacheExpiresAt      time.Time
	ObservedAt          time.Time
}

type RoutingObservationEntry struct {
	ID                  int64  `json:"id"`
	RequestID           string `json:"request_id"`
	AttemptNo           int    `json:"attempt_no"`
	GroupID             int64  `json:"group_id"`
	UpstreamID          int64  `json:"upstream_id"`
	APIKeyHash          string `json:"api_key_hash,omitempty"`
	Model               string `json:"model"`
	SessionKey          string `json:"session_key,omitempty"`
	PrefixHash          string `json:"prefix_hash,omitempty"`
	CacheKey            string `json:"cache_key,omitempty"`
	PrefixTokens        int64  `json:"prefix_tokens"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CachedTokens        int64  `json:"cached_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	TTFTMs              int64  `json:"ttft_ms"`
	DurationMs          int64  `json:"duration_ms"`
	Success             bool   `json:"success"`
	CacheEligible       bool   `json:"cache_eligible"`
	CacheHit            bool   `json:"cache_hit"`
	CacheCreated        bool   `json:"cache_created"`
	CacheExpiresAt      int64  `json:"cache_expires_at,omitempty"`
	ObservedAt          int64  `json:"observed_at"`
}

type RoutingObservationFilter struct {
	Limit      int
	GroupID    int64
	UpstreamID int64
	APIKeyHash string
	Model      string
	SessionKey string
	PrefixHash string
	Since      time.Time
	Until      time.Time
}

type UpstreamRoutingStats struct {
	UpstreamID          int64   `json:"upstream_id"`
	Model               string  `json:"model"`
	FromAt              int64   `json:"from_at"`
	ToAt                int64   `json:"to_at"`
	Requests            int64   `json:"requests"`
	Successes           int64   `json:"successes"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheHits           int64   `json:"cache_hits"`
	CacheMisses         int64   `json:"cache_misses"`
	RequestsPerMinute   float64 `json:"requests_per_minute"`
	OutputPerRequest    float64 `json:"output_per_request"`
	CacheHitRate        float64 `json:"cache_hit_rate"`
	SuccessRate         float64 `json:"success_rate"`
	P50TTFTMs           int64   `json:"p50_ttft_ms"`
	P95TTFTMs           int64   `json:"p95_ttft_ms"`
	P95DurationMs       int64   `json:"p95_duration_ms"`
}

// CacheCoverageRatio returns the average fraction of tokens that were actually
// cached (cached_tokens / total_tokens) for an upstream. This accounts for
// proxied upstreams that only cache a portion of the prefix.
func (s *Store) CacheCoverageRatio(upstreamID int64) (float64, error) {
	var ratio float64
	err := s.queryRow(`SELECT AVG(CAST(cached_tokens AS REAL) / NULLIF(cached_tokens + input_tokens + cache_creation_tokens, 0))
		FROM (SELECT cached_tokens, input_tokens, cache_creation_tokens
			FROM request_attempts WHERE upstream_id=? AND status=200 AND cached_tokens > 0
			ORDER BY id DESC LIMIT 50) sub`, upstreamID).Scan(&ratio)
	if err != nil {
		return 0, err
	}
	return ratio, nil
}

// TokenInflationFactor returns the average ratio of actual billed tokens to the
// routing estimate (prefix_tokens) for an upstream. Upstreams that inject extra
// system prompts will have inflation > 1.0, meaning the cost model should use a
// larger InputTokens when estimating the no-cache baseline and suffix cost.
// Only considers recent cache-eligible observations where prefix_tokens is
// meaningful (> 1024).
func (s *Store) TokenInflationFactor(upstreamID int64) (float64, error) {
	var factor float64
	err := s.queryRow(`SELECT AVG(CAST(input_tokens + cached_tokens + cache_creation_tokens AS REAL) / prefix_tokens)
		FROM (SELECT input_tokens, cached_tokens, cache_creation_tokens, prefix_tokens
			FROM routing_observations
			WHERE upstream_id=? AND prefix_tokens > 1024 AND (cached_tokens > 0 OR cache_creation_tokens > 0)
			ORDER BY observed_at DESC LIMIT 50) sub`, upstreamID).Scan(&factor)
	if err != nil {
		return 1, err
	}
	if factor < 1 {
		return 1, nil
	}
	return factor, nil
}

type PrefixCacheStats struct {
	APIKeyHash         string  `json:"api_key_hash,omitempty"`
	UpstreamID         int64   `json:"upstream_id"`
	Model              string  `json:"model"`
	PrefixHash         string  `json:"prefix_hash"`
	SessionKey         string  `json:"session_key,omitempty"`
	CacheKey           string  `json:"cache_key,omitempty"`
	PrefixTokens       int64   `json:"prefix_tokens"`
	Observations       int64   `json:"observations"`
	HitCount           int64   `json:"hit_count"`
	MissCount          int64   `json:"miss_count"`
	CreateCount        int64   `json:"create_count"`
	HitRate            float64 `json:"hit_rate"`
	WindowObservations int64   `json:"window_observations"`
	WindowHitCount     int64   `json:"window_hit_count"`
	WindowMissCount    int64   `json:"window_miss_count"`
	WindowHitRate      float64 `json:"window_hit_rate"`
	LastHitAt          int64   `json:"last_hit_at,omitempty"`
	LastMissAt         int64   `json:"last_miss_at,omitempty"`
	LastCreatedAt      int64   `json:"last_created_at,omitempty"`
	ExpiresAt          int64   `json:"expires_at,omitempty"`
	FirstSeenAt        int64   `json:"first_seen_at"`
	LastSeenAt         int64   `json:"last_seen_at"`
	Valid              bool    `json:"valid"`
}

func normalizeRoutingObservation(observation *RoutingObservationRecord) error {
	observation.RequestID = strings.TrimSpace(observation.RequestID)
	if observation.RequestID == "" {
		return errors.New("routing observation request id is required")
	}
	if observation.AttemptNo <= 0 {
		observation.AttemptNo = 1
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = time.Now()
	}
	counts := []*int64{
		&observation.PrefixTokens, &observation.InputTokens, &observation.OutputTokens,
		&observation.CachedTokens, &observation.CacheCreationTokens, &observation.TTFTMs,
		&observation.DurationMs,
	}
	for _, count := range counts {
		if *count < 0 {
			*count = 0
		}
	}
	if observation.CachedTokens > 0 {
		observation.CacheHit = true
	}
	if observation.CacheCreationTokens > 0 {
		observation.CacheCreated = true
	}
	if observation.CacheHit || observation.CacheCreated {
		observation.CacheEligible = true
	}
	if observation.PrefixTokens == 0 {
		observation.PrefixTokens = observation.CachedTokens
		if observation.CacheCreationTokens > observation.PrefixTokens {
			observation.PrefixTokens = observation.CacheCreationTokens
		}
	}
	if observation.CacheExpiresAt.IsZero() && observation.CacheTTL > 0 &&
		(observation.CacheHit || observation.CacheCreated) {
		observation.CacheExpiresAt = observation.ObservedAt.Add(observation.CacheTTL)
	}
	return nil
}

func nullableRoutingTime(s *Store, value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return s.timeValue(value)
}

// SaveRoutingObservation persists one observation and updates the latest
// cache entry in the same transaction.
func (s *Store) SaveRoutingObservation(observation RoutingObservationRecord) error {
	return s.SaveRoutingObservations([]RoutingObservationRecord{observation})
}

// SaveRoutingObservations is idempotent on request ID plus attempt number.
// Duplicate writes are ignored and therefore cannot inflate hit-rate counters.
func (s *Store) SaveRoutingObservations(observations []RoutingObservationRecord) error {
	if len(observations) == 0 {
		return nil
	}
	for i := range observations {
		if err := normalizeRoutingObservation(&observations[i]); err != nil {
			return err
		}
	}
	tx, err := s.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, observation := range observations {
		result, err := tx.Exec(`INSERT INTO routing_observations(
			request_id,attempt_no,group_id,upstream_id,api_key_hash,model,session_key,prefix_hash,cache_key,
			prefix_tokens,input_tokens,output_tokens,cached_tokens,cache_creation_tokens,ttft_ms,duration_ms,
			success,cache_eligible,cache_hit,cache_created,cache_expires_at,observed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(request_id,attempt_no) DO NOTHING`,
			observation.RequestID, observation.AttemptNo, observation.GroupID, observation.UpstreamID,
			observation.APIKeyHash, observation.Model, observation.SessionKey, observation.PrefixHash,
			observation.CacheKey, observation.PrefixTokens, observation.InputTokens, observation.OutputTokens,
			observation.CachedTokens, observation.CacheCreationTokens, observation.TTFTMs,
			observation.DurationMs, observation.Success, observation.CacheEligible, observation.CacheHit,
			observation.CacheCreated, nullableRoutingTime(s, observation.CacheExpiresAt),
			s.timeValue(observation.ObservedAt))
		if err != nil {
			return err
		}
		if inserted, err := result.RowsAffected(); err == nil && inserted == 0 {
			continue
		}
		if observation.UpstreamID <= 0 || observation.PrefixHash == "" || !observation.CacheEligible {
			continue
		}
		if err := s.upsertPrefixCacheStats(tx, observation); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolCount(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func (s *Store) upsertPrefixCacheStats(tx *rawTx, observation RoutingObservationRecord) error {
	hitCount := boolCount(observation.CacheHit)
	missCount := boolCount(observation.CacheEligible && !observation.CacheHit)
	createCount := boolCount(observation.CacheCreated)
	var lastHit, lastMiss, lastCreated any
	if observation.CacheHit {
		lastHit = s.timeValue(observation.ObservedAt)
	}
	if observation.CacheEligible && !observation.CacheHit {
		lastMiss = s.timeValue(observation.ObservedAt)
	}
	if observation.CacheCreated {
		lastCreated = s.timeValue(observation.ObservedAt)
	}
	_, err := tx.Exec(`INSERT INTO upstream_prefix_cache_stats(
		api_key_hash,upstream_id,model,prefix_hash,session_key,cache_key,prefix_tokens,observations,
		hit_count,miss_count,create_count,last_hit_at,last_miss_at,last_created_at,expires_at,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(api_key_hash,upstream_id,model,prefix_hash) DO UPDATE SET
		session_key=CASE WHEN excluded.session_key<>'' THEN excluded.session_key ELSE upstream_prefix_cache_stats.session_key END,
		cache_key=CASE WHEN excluded.cache_key<>'' THEN excluded.cache_key ELSE upstream_prefix_cache_stats.cache_key END,
		prefix_tokens=CASE WHEN excluded.prefix_tokens>0 THEN excluded.prefix_tokens ELSE upstream_prefix_cache_stats.prefix_tokens END,
		observations=upstream_prefix_cache_stats.observations+1,
		hit_count=upstream_prefix_cache_stats.hit_count+excluded.hit_count,
		miss_count=upstream_prefix_cache_stats.miss_count+excluded.miss_count,
		create_count=upstream_prefix_cache_stats.create_count+excluded.create_count,
		last_hit_at=COALESCE(excluded.last_hit_at,upstream_prefix_cache_stats.last_hit_at),
		last_miss_at=COALESCE(excluded.last_miss_at,upstream_prefix_cache_stats.last_miss_at),
		last_created_at=COALESCE(excluded.last_created_at,upstream_prefix_cache_stats.last_created_at),
		expires_at=COALESCE(excluded.expires_at,upstream_prefix_cache_stats.expires_at),
		last_seen_at=excluded.last_seen_at`,
		observation.APIKeyHash, observation.UpstreamID, observation.Model, observation.PrefixHash,
		observation.SessionKey, observation.CacheKey, observation.PrefixTokens, hitCount, missCount,
		createCount, lastHit, lastMiss, lastCreated, nullableRoutingTime(s, observation.CacheExpiresAt),
		s.timeValue(observation.ObservedAt), s.timeValue(observation.ObservedAt))
	return err
}

func (s *Store) routingObservationSelect(where string) string {
	return `SELECT id,request_id,attempt_no,group_id,upstream_id,api_key_hash,model,session_key,prefix_hash,
		cache_key,prefix_tokens,input_tokens,output_tokens,cached_tokens,cache_creation_tokens,ttft_ms,duration_ms,
		success,cache_eligible,cache_hit,cache_created,COALESCE(` + s.unixExpr("cache_expires_at") + `,0),` +
		s.unixExpr("observed_at") + ` FROM routing_observations` + where
}

func scanRoutingObservation(scanner rowScanner) (RoutingObservationEntry, error) {
	var entry RoutingObservationEntry
	err := scanner.Scan(&entry.ID, &entry.RequestID, &entry.AttemptNo, &entry.GroupID, &entry.UpstreamID,
		&entry.APIKeyHash, &entry.Model, &entry.SessionKey, &entry.PrefixHash, &entry.CacheKey,
		&entry.PrefixTokens, &entry.InputTokens, &entry.OutputTokens, &entry.CachedTokens,
		&entry.CacheCreationTokens, &entry.TTFTMs, &entry.DurationMs, &entry.Success,
		&entry.CacheEligible, &entry.CacheHit, &entry.CacheCreated, &entry.CacheExpiresAt, &entry.ObservedAt)
	return entry, err
}

// ListRoutingObservations returns the newest matching observations in
// chronological order, ready to replay into an in-memory predictor.
func (s *Store) ListRoutingObservations(filter RoutingObservationFilter) ([]RoutingObservationEntry, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 10_000
	} else if limit > 100_000 {
		limit = 100_000
	}
	var where strings.Builder
	where.WriteString(" WHERE 1=1")
	var args []any
	if filter.GroupID > 0 {
		where.WriteString(" AND group_id=?")
		args = append(args, filter.GroupID)
	}
	if filter.UpstreamID > 0 {
		where.WriteString(" AND upstream_id=?")
		args = append(args, filter.UpstreamID)
	}
	if filter.APIKeyHash != "" {
		where.WriteString(" AND api_key_hash=?")
		args = append(args, filter.APIKeyHash)
	}
	if filter.Model != "" {
		where.WriteString(" AND model=?")
		args = append(args, filter.Model)
	}
	if filter.SessionKey != "" {
		where.WriteString(" AND session_key=?")
		args = append(args, filter.SessionKey)
	}
	if filter.PrefixHash != "" {
		where.WriteString(" AND prefix_hash=?")
		args = append(args, filter.PrefixHash)
	}
	if !filter.Since.IsZero() {
		where.WriteString(" AND observed_at>=?")
		args = append(args, s.timeValue(filter.Since))
	}
	if !filter.Until.IsZero() {
		where.WriteString(" AND observed_at<?")
		args = append(args, s.timeValue(filter.Until))
	}
	query := s.routingObservationSelect(where.String()) + " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []RoutingObservationEntry{}
	for rows.Next() {
		entry, err := scanRoutingObservation(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	return entries, nil
}

func routingRatio(numerator, denominator int64) float64 {
	if denominator <= 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func routingPercentile(values []int64, fraction float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	if fraction <= 0 {
		return values[0]
	}
	if fraction >= 1 {
		return values[len(values)-1]
	}
	return values[int(float64(len(values)-1)*fraction)]
}

// GetUpstreamRoutingStats returns the recent performance and traffic features
// used by the hot path. A non-positive window uses 15 minutes.
func (s *Store) GetUpstreamRoutingStats(upstreamID int64, model string, window time.Duration, now time.Time) (UpstreamRoutingStats, error) {
	if window <= 0 {
		window = DefaultRoutingStatsWindow
	}
	if now.IsZero() {
		now = time.Now()
	}
	from := now.Add(-window)
	stats := UpstreamRoutingStats{UpstreamID: upstreamID, Model: model, FromAt: from.Unix(), ToAt: now.Unix()}
	rows, err := s.query(`SELECT success,input_tokens,output_tokens,cached_tokens,cache_creation_tokens,
		cache_eligible,cache_hit,ttft_ms,duration_ms FROM routing_observations
		WHERE upstream_id=? AND model=? AND observed_at>=? AND observed_at<?`,
		upstreamID, model, s.timeValue(from), s.timeValue(now))
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	var ttfts, durations []int64
	for rows.Next() {
		var success, cacheEligible, cacheHit bool
		var input, output, cached, created, ttft, duration int64
		if err := rows.Scan(&success, &input, &output, &cached, &created, &cacheEligible,
			&cacheHit, &ttft, &duration); err != nil {
			return stats, err
		}
		stats.Requests++
		stats.InputTokens += input
		stats.OutputTokens += output
		stats.CachedTokens += cached
		stats.CacheCreationTokens += created
		if success {
			stats.Successes++
		}
		if cacheEligible {
			if cacheHit {
				stats.CacheHits++
			} else {
				stats.CacheMisses++
			}
		}
		if ttft > 0 {
			ttfts = append(ttfts, ttft)
		}
		if duration > 0 {
			durations = append(durations, duration)
		}
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}
	stats.SuccessRate = routingRatio(stats.Successes, stats.Requests)
	stats.CacheHitRate = routingRatio(stats.CacheHits, stats.CacheHits+stats.CacheMisses)
	if stats.Requests > 0 {
		stats.OutputPerRequest = float64(stats.OutputTokens) / float64(stats.Requests)
	}
	if minutes := window.Minutes(); minutes > 0 {
		stats.RequestsPerMinute = float64(stats.Requests) / minutes
	}
	stats.P50TTFTMs = routingPercentile(ttfts, 0.50)
	stats.P95TTFTMs = routingPercentile(ttfts, 0.95)
	stats.P95DurationMs = routingPercentile(durations, 0.95)
	return stats, nil
}

// GetPrefixCacheStats reads the cache state isolated by upstream credential,
// upstream, model, and prefix. Lifetime counters are returned alongside a
// recent-window hit rate; zero/unknown expiry is treated conservatively as
// not currently valid.
func (s *Store) GetPrefixCacheStats(apiKeyHash string, upstreamID int64, model, prefixHash string, window time.Duration, now time.Time) (PrefixCacheStats, error) {
	if window <= 0 {
		window = DefaultRoutingStatsWindow
	}
	if now.IsZero() {
		now = time.Now()
	}
	stats := PrefixCacheStats{APIKeyHash: apiKeyHash, UpstreamID: upstreamID, Model: model, PrefixHash: prefixHash}
	query := `SELECT session_key,cache_key,prefix_tokens,observations,hit_count,miss_count,create_count,
		COALESCE(` + s.unixExpr("last_hit_at") + `,0),COALESCE(` + s.unixExpr("last_miss_at") + `,0),
		COALESCE(` + s.unixExpr("last_created_at") + `,0),COALESCE(` + s.unixExpr("expires_at") + `,0),
		` + s.unixExpr("first_seen_at") + `,` + s.unixExpr("last_seen_at") + `
		FROM upstream_prefix_cache_stats WHERE api_key_hash=? AND upstream_id=? AND model=? AND prefix_hash=?`
	err := s.queryRow(query, apiKeyHash, upstreamID, model, prefixHash).Scan(
		&stats.SessionKey, &stats.CacheKey, &stats.PrefixTokens, &stats.Observations,
		&stats.HitCount, &stats.MissCount, &stats.CreateCount, &stats.LastHitAt, &stats.LastMissAt,
		&stats.LastCreatedAt, &stats.ExpiresAt, &stats.FirstSeenAt, &stats.LastSeenAt)
	if err != nil {
		// Fallback: if no exact prefix_hash match, aggregate by session_key from
		// routing_observations. This handles multi-turn sessions where the prefix
		// hash changes every turn but the session (and its cache behavior) is stable.
		stats, fallbackErr := s.getSessionCacheStats(apiKeyHash, upstreamID, model, prefixHash, window, now)
		if fallbackErr != nil {
			return stats, err
		}
		return stats, nil
	}
	stats.HitRate = routingRatio(stats.HitCount, stats.HitCount+stats.MissCount)
	stats.Valid = stats.ExpiresAt > now.Unix()
	from := now.Add(-window)
	if err := s.queryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN cache_eligible=TRUE AND cache_hit=TRUE THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN cache_eligible=TRUE AND cache_hit=FALSE THEN 1 ELSE 0 END),0)
		FROM routing_observations WHERE api_key_hash=? AND upstream_id=? AND model=? AND prefix_hash=?
		AND observed_at>=? AND observed_at<?`, apiKeyHash, upstreamID, model, prefixHash,
		s.timeValue(from), s.timeValue(now)).Scan(&stats.WindowObservations, &stats.WindowHitCount,
		&stats.WindowMissCount); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stats, err
	}
	stats.WindowHitRate = routingRatio(stats.WindowHitCount, stats.WindowHitCount+stats.WindowMissCount)
	return stats, nil
}

// getSessionCacheStats aggregates cache observations by session_key when the
// exact prefix_hash doesn't exist in the summary table. This is the common case
// for multi-turn conversations where each request has a slightly different prefix.
func (s *Store) getSessionCacheStats(apiKeyHash string, upstreamID int64, model, sessionKey string, window time.Duration, now time.Time) (PrefixCacheStats, error) {
	stats := PrefixCacheStats{APIKeyHash: apiKeyHash, UpstreamID: upstreamID, Model: model, PrefixHash: sessionKey, SessionKey: sessionKey}
	from := now.Add(-window)
	err := s.queryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN cache_hit THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN cache_eligible AND NOT cache_hit THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN cache_created THEN 1 ELSE 0 END),0),
		COALESCE(MAX(prefix_tokens),0),
		COALESCE(MAX(CASE WHEN cache_hit THEN `+s.unixExpr("observed_at")+` ELSE 0 END),0),
		COALESCE(MAX(CASE WHEN cache_created THEN `+s.unixExpr("observed_at")+` ELSE 0 END),0),
		COALESCE(MIN(`+s.unixExpr("observed_at")+`),0)
		FROM routing_observations
		WHERE api_key_hash=? AND upstream_id=? AND model=? AND session_key=?
		AND observed_at>=? AND observed_at<?`,
		apiKeyHash, upstreamID, model, sessionKey,
		s.timeValue(from), s.timeValue(now)).Scan(
		&stats.WindowObservations, &stats.WindowHitCount, &stats.WindowMissCount,
		&stats.CreateCount, &stats.PrefixTokens, &stats.LastHitAt, &stats.LastCreatedAt,
		&stats.FirstSeenAt)
	if err != nil {
		return stats, err
	}
	// If no observations in window, widen to entire session lifetime.
	// This handles the case where a channel was healthy 20min ago (with cache hits)
	// but got circuit-broken and is now recovering — we want its historical
	// cache behavior to inform the routing decision.
	if stats.WindowObservations == 0 {
		err = s.queryRow(`SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN cache_hit THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN cache_eligible AND NOT cache_hit THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN cache_created THEN 1 ELSE 0 END),0),
			COALESCE(MAX(prefix_tokens),0),
			COALESCE(MAX(CASE WHEN cache_hit THEN `+s.unixExpr("observed_at")+` ELSE 0 END),0),
			COALESCE(MAX(CASE WHEN cache_created THEN `+s.unixExpr("observed_at")+` ELSE 0 END),0),
			COALESCE(MIN(`+s.unixExpr("observed_at")+`),0)
			FROM routing_observations
			WHERE api_key_hash=? AND upstream_id=? AND model=? AND session_key=?
			AND success=TRUE`,
			apiKeyHash, upstreamID, model, sessionKey).Scan(
			&stats.WindowObservations, &stats.WindowHitCount, &stats.WindowMissCount,
			&stats.CreateCount, &stats.PrefixTokens, &stats.LastHitAt, &stats.LastCreatedAt,
			&stats.FirstSeenAt)
		if err != nil {
			return stats, err
		}
	}
	if stats.WindowObservations == 0 {
		return stats, sql.ErrNoRows
	}
	stats.Observations = stats.WindowObservations
	stats.HitCount = stats.WindowHitCount
	stats.MissCount = stats.WindowMissCount
	stats.HitRate = routingRatio(stats.HitCount, stats.HitCount+stats.MissCount)
	stats.WindowHitRate = stats.HitRate
	if stats.LastHitAt > 0 || stats.LastCreatedAt > 0 {
		latestCache := stats.LastHitAt
		if stats.LastCreatedAt > latestCache {
			latestCache = stats.LastCreatedAt
		}
		// Use adaptive TTL for ExpiresAt: if the session has been running
		// long enough and cache was rebuilt multiple times, assume 1h TTL
		// was used for the latest creation (see selectCacheTTL in scheduler).
		assumedTTL := 5 * time.Minute
		sessionDuration := now.Unix() - stats.FirstSeenAt
		if sessionDuration > int64((10*time.Minute)/time.Second) && stats.CreateCount >= 2 {
			assumedTTL = time.Hour
		}
		stats.ExpiresAt = latestCache + int64(assumedTTL/time.Second)
		stats.Valid = stats.ExpiresAt > now.Unix()
	}
	return stats, nil
}
