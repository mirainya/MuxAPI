package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRouteDecisionPersistenceIsIdempotent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "routing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Second)
	selected := 0.012
	record := RouteDecisionRecord{
		RequestID: "route-1", GroupID: 2, Model: "claude-sonnet", Protocol: "claude",
		Endpoint: "/v1/messages", SessionKey: "session-a", PrefixHash: "prefix-a", CacheKey: "key-a",
		Reason: "cache forecast saves 0.1", SelectedUpstreamID: 11, ForecastWindow: 15 * time.Minute,
		ForecastRequests: 4, EstimatedInputTokens: 1000, ReusablePrefixTokens: 800,
		EstimatedOutputTokens: 200, SelectedCost: &selected, Confidence: 0.8, CacheSelected: true,
		CreatedAt: now,
		Candidates: []RouteCandidateRecord{{
			UpstreamID: 11, APIKeyHash: "hash-a", UpstreamName: "cheap", Protocol: "claude", Selected: true,
			Eligible: true, CacheSupported: true, CacheExisting: true, CacheSelected: true,
			CacheHitRate: 0.75, ForecastTotalCost: &selected, Details: json.RawMessage(`{"source":"test"}`),
		}},
	}
	id, err := st.SaveRouteDecision(record)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("route decision id should be assigned")
	}
	updatedReason := record
	updatedReason.Reason = "updated"
	updatedReason.Candidates[0].Details = json.RawMessage(`{"source":"updated"}`)
	updated, err := st.SaveRouteDecision(updatedReason)
	if err != nil {
		t.Fatal(err)
	}
	if updated != id {
		t.Fatalf("idempotent update changed id: %d -> %d", id, updated)
	}
	if got := countRows(t, st, "route_decisions"); got != 1 {
		t.Fatalf("expected one decision, got %d", got)
	}
	if got := countRows(t, st, "route_decision_candidates"); got != 1 {
		t.Fatalf("expected one candidate after replacement, got %d", got)
	}
	entry, err := st.GetRouteDecisionByRequestID("route-1")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Reason != "updated" || len(entry.Candidates) != 1 || string(entry.Candidates[0].Details) != `{"source":"updated"}` {
		t.Fatalf("unexpected decision: %+v", entry)
	}
	actualCost := 0.02
	actualInput := int64(1000)
	if err := st.CompleteRouteDecision("route-1", RouteDecisionOutcome{
		ActualCost: &actualCost, ActualInputTokens: &actualInput, Outcome: "success", CompletedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	entry, err = st.GetRouteDecision(id)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ActualCost == nil || *entry.ActualCost != actualCost || entry.ActualOutcome != "success" {
		t.Fatalf("actual outcome not persisted: %+v", entry)
	}
	if err := st.CompleteRouteDecision("missing", RouteDecisionOutcome{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing decision should return sql.ErrNoRows, got %v", err)
	}
}

func TestRoutingObservationStatsAndCacheIsolation(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "observations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Second)
	observations := []RoutingObservationRecord{
		{RequestID: "obs-1", AttemptNo: 1, UpstreamID: 7, APIKeyHash: "key-a", Model: "m",
			SessionKey: "s", PrefixHash: "p", PrefixTokens: 100, InputTokens: 120, OutputTokens: 20,
			CachedTokens: 100, CacheEligible: true, CacheHit: true, Success: true, TTFTMs: 100,
			DurationMs: 1000, ObservedAt: now.Add(-5 * time.Minute), CacheTTL: time.Hour},
		{RequestID: "obs-2", AttemptNo: 1, UpstreamID: 7, APIKeyHash: "key-a", Model: "m",
			SessionKey: "s", PrefixHash: "p", PrefixTokens: 100, InputTokens: 120, OutputTokens: 40,
			CacheCreationTokens: 100, CacheEligible: true, CacheCreated: true, Success: true, TTFTMs: 200,
			DurationMs: 2000, ObservedAt: now.Add(-4 * time.Minute), CacheTTL: time.Hour},
		{RequestID: "obs-3", AttemptNo: 1, UpstreamID: 7, APIKeyHash: "key-b", Model: "m",
			PrefixHash: "p", PrefixTokens: 100, InputTokens: 120, OutputTokens: 30,
			CacheCreationTokens: 100, CacheEligible: true, CacheCreated: true, Success: false, TTFTMs: 400,
			DurationMs: 4000, ObservedAt: now.Add(-3 * time.Minute), CacheTTL: time.Hour},
	}
	if err := st.SaveRoutingObservations(observations); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRoutingObservation(observations[0]); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, st, "routing_observations"); got != 3 {
		t.Fatalf("duplicate observation should be ignored, got %d", got)
	}
	stats, err := st.GetUpstreamRoutingStats(7, "m", 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 3 || stats.Successes != 2 || stats.CacheHits != 1 || stats.CacheMisses != 2 || stats.OutputPerRequest != 30 {
		t.Fatalf("unexpected upstream stats: %+v", stats)
	}
	if stats.P95TTFTMs != 200 || stats.P95DurationMs != 2000 {
		t.Fatalf("unexpected percentiles: %+v", stats)
	}
	cache, err := st.GetPrefixCacheStats("key-a", 7, "m", "p", "claude", 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if cache.HitCount != 1 || cache.MissCount != 1 || cache.CreateCount != 1 || cache.HitRate != 0.5 || !cache.Valid {
		t.Fatalf("unexpected key-a cache stats: %+v", cache)
	}
	if _, err := st.GetPrefixCacheStats("key-b", 7, "m", "p", "claude", 15*time.Minute, now); err != nil {
		// key-b has its own entry; reading it must not return key-a's cache.
		t.Fatal(err)
	}
	entries, err := st.ListRoutingObservations(RoutingObservationFilter{APIKeyHash: "key-a", Model: "m", Limit: 10})
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected filtered observations: %d, %v", len(entries), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := st.FlushRequests(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCacheStatsExposeRollingWrites(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "session-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Second)
	observations := []RoutingObservationRecord{
		{RequestID: "session-1", UpstreamID: 7, APIKeyHash: "key-a", Model: "m", SessionKey: "session-a", PrefixHash: "prefix-1", PrefixTokens: 1000,
			CachedTokens: 700, CacheCreationTokens: 300, CacheEligible: true, CacheHit: true, CacheCreated: true, Success: true, ObservedAt: now.Add(-2 * time.Minute)},
		{RequestID: "session-2", UpstreamID: 7, APIKeyHash: "key-a", Model: "m", SessionKey: "session-a", PrefixHash: "prefix-2", PrefixTokens: 1200,
			CacheCreationTokens: 1200, CacheEligible: true, CacheCreated: true, Success: true, ObservedAt: now.Add(-time.Minute)},
	}
	if err := st.SaveRoutingObservations(observations); err != nil {
		t.Fatal(err)
	}
	stats, err := st.GetPrefixCacheStats("key-a", 7, "m", "session-a", "claude", 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.WindowCreateCount != 2 || stats.WindowCreateTokens != 1500 || stats.HitCount != 1 || stats.MissCount != 1 {
		t.Fatalf("rolling session cache writes were not aggregated: %+v", stats)
	}
}

// AssumedCacheTTL must mirror routing.selectAdaptiveTTL exactly, otherwise the
// store fallback would stamp an ExpiresAt derived from a wrong TTL and the
// state machine would flip CacheHot → CacheExpired prematurely.
func TestAssumedCacheTTL(t *testing.T) {
	now := int64(1_700_000_000)
	// Gemini always → 1h.
	for _, p := range []string{"gemini", "google", "generativelanguage", "generatecontent"} {
		if got := AssumedCacheTTL(p, 1, 1, now-60, now); got != time.Hour {
			t.Fatalf("%s → %v want 1h", p, got)
		}
	}
	// Long session + rebuilds → 1h.
	if got := AssumedCacheTTL("claude", 20, 3, now-15*60, now); got != time.Hour {
		t.Fatalf("long/rebuilds → %v want 1h", got)
	}
	// Sparse conversation + any rebuild → 1h.
	if got := AssumedCacheTTL("claude", 2, 1, now-10*60, now); got != time.Hour {
		t.Fatalf("sparse → %v want 1h", got)
	}
	// Short session, few rebuilds → 5min.
	if got := AssumedCacheTTL("claude", 3, 1, now-3*60, now); got != 5*time.Minute {
		t.Fatalf("short → %v want 5min", got)
	}
	// No observations → 5min default.
	if got := AssumedCacheTTL("claude", 0, 0, 0, now); got != 5*time.Minute {
		t.Fatalf("empty → %v want 5min", got)
	}
}
