package server

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/store"
)

func (s *Server) persistRoutingAudit(requestID string, started time.Time, groupID int64, model, endpoint string, result forward.Result) {
	features := result.RouteFeatures.Normalize()
	decision := result.RouteDecision

	// Emit structured audit log for every request (JSON, greppable by "route_audit").
	s.emitRouteAuditLog(requestID, started, model, endpoint, features, decision, result)
	if decision != nil && (decision.SelectedID != 0 || len(decision.Evaluations) > 0) {
		var selectedCost, noCacheCost, cacheCost, estimatedSavings *float64
		if decision.SelectedID != 0 {
			selectedCost = floatPtr(decision.EffectiveCost)
			noCacheCost = floatPtr(decision.Cost.NoCacheTotal)
			cacheCost = floatPtr(decision.Cost.CacheTotal)
			estimatedSavings = floatPtr(decision.EstimatedSavings)
		}
		record := store.RouteDecisionRecord{
			RequestID: requestID, GroupID: groupID, Model: model, Protocol: features.Protocol,
			Endpoint: endpoint, SessionKey: features.SessionID, PrefixHash: features.CacheKey,
			CacheKey: features.CacheKey, Strategy: "cost", Reason: decision.Reason,
			SelectedUpstreamID: decision.SelectedID, ForecastWindow: decision.Forecast.Window,
			ForecastRequests: decision.Forecast.Requests, EstimatedInputTokens: features.InputTokens,
			ReusablePrefixTokens: features.ReusableInputTokens, EstimatedOutputTokens: features.EstimatedOutputTokens,
			SelectedCost: selectedCost, NoCacheCost: noCacheCost,
			CacheCost: cacheCost, EstimatedSavings: estimatedSavings,
			Confidence: decision.Confidence, CacheSelected: decision.Cost.CacheUsed, Exploration: decision.Exploration, CreatedAt: started,
		}
		for _, evaluation := range decision.Evaluations {
			details, _ := json.Marshal(evaluation)
			record.Candidates = append(record.Candidates, store.RouteCandidateRecord{
				UpstreamID: evaluation.CandidateID, UpstreamName: evaluation.CandidateName,
				Protocol: evaluation.Protocol, Priority: evaluation.Priority, Eligible: evaluation.Eligible,
				Selected:        evaluation.CandidateID == decision.SelectedID,
				RejectionReason: evaluation.RejectReason, PricingSource: evaluation.PricingSource,
				PricingConfidence: evaluation.Cost.Confidence, CacheSupported: evaluation.Cost.CacheEligible,
				CacheExisting: evaluation.CacheExisting, CacheSelected: evaluation.Cost.CacheUsed,
				CacheHitRate:      evaluation.CacheHitRate,
				ForecastTotalCost: floatPtr(evaluation.Cost.SelectedTotal), ForecastNoCacheCost: floatPtr(evaluation.Cost.NoCacheTotal),
				ForecastCacheCost: floatPtr(evaluation.Cost.CacheTotal), EstimatedSavings: floatPtr(evaluation.Cost.Savings),
				BreakEvenRequests: floatPtr(evaluation.Cost.BreakEvenRequests), ExpectedHits: evaluation.Cost.ExpectedHits,
				ExpectedMisses: evaluation.Cost.ExpectedMisses, ExpectedCreates: evaluation.Cost.ExpectedCreates,
				EstimatedTTFTMs: evaluation.P95TTFTMs, EstimatedDurationMs: evaluation.P95DurationMs,
				SuccessRate: evaluation.SuccessRate,
				Details:     details,
			})
		}
		if id, err := s.store.SaveRouteDecision(record); err != nil {
			slog.Warn("save route decision failed", "request_id", requestID, "err", err)
		} else {
			_ = id
			actualCost := actualRouteCost(result.Attempts)
			// ActualUpstreamID may differ from SelectedUpstreamID when the
			// initial pick failed and a failover attempt succeeded. Persist
			// it so downstream analysis can distinguish 'was picked' from
			// 'actually served the successful attempt' — otherwise a JOIN by
			// selected_upstream_id misattributes failover requests to the
			// upstream that failed.
			complete := store.RouteDecisionOutcome{
				ActualCost:        actualCost,
				ActualInputTokens: optionalInt64Ptr(result.InputTokens), ActualOutputTokens: optionalInt64Ptr(result.OutputTokens),
				ActualCachedTokens: optionalInt64Ptr(result.CachedTokens), ActualCacheCreationTokens: optionalInt64Ptr(result.CacheCreationTokens),
				ActualUpstreamID: result.FinalUpstreamID,
				Outcome:          result.Outcome, CompletedAt: time.Now(),
			}
			if err := s.store.CompleteRouteDecision(requestID, complete); err != nil {
				slog.Warn("complete route decision failed", "request_id", requestID, "err", err)
			}
		}
	}

	observations := make([]store.RoutingObservationRecord, 0, len(result.Attempts))
	for _, attempt := range result.Attempts {
		cacheEligible := (features.ReusableInputTokens > 0 &&
			(attempt.Outcome == forward.OutcomeSuccess || attempt.Outcome == forward.OutcomePartial)) ||
			attempt.CachedTokens > 0 || attempt.CacheCreationTokens > 0
		observations = append(observations, store.RoutingObservationRecord{
			RequestID: requestID, AttemptNo: attempt.AttemptNo, GroupID: groupID, UpstreamID: attempt.UpstreamID,
			APIKeyHash: attempt.UpstreamKeyHash, Model: model, SessionKey: features.SessionID,
			PrefixHash: features.CacheKey, CacheKey: features.CacheKey, PrefixTokens: features.ReusableInputTokens,
			InputTokens: attempt.InputTokens, OutputTokens: attempt.OutputTokens, CachedTokens: attempt.CachedTokens,
			CacheCreationTokens: attempt.CacheCreationTokens, TTFTMs: attempt.TTFTMs, DurationMs: attempt.DurationMs,
			// Routing reliability measures the channel, not malformed client
			// input or a model-capability miss. Only upstream failures and
			// post-commit stream failures lower this rate.
			Success: routingAttemptSucceeded(attempt.Outcome),
			// A reusable prefix with zero cached tokens is an observed miss. Keep
			// it in the denominator so future choices learn the real hit rate.
			CacheEligible: cacheEligible,
			CacheHit:      attempt.CachedTokens > 0, CacheCreated: attempt.CacheCreationTokens > 0,
			CacheTTL:   cacheTTLForProtocol(attempt.Protocol),
			ObservedAt: attempt.CompletedAt,
		})
	}
	if err := s.store.SaveRoutingObservations(observations); err != nil {
		slog.Warn("save routing observations failed", "request_id", requestID, "err", err)
	}
}

func actualRouteCost(attempts []forward.AttemptResult) *float64 {
	var total float64
	found := false
	for _, attempt := range attempts {
		if attempt.Outcome != forward.OutcomeSuccess && attempt.Outcome != forward.OutcomePartial {
			continue
		}
		if attempt.RouteDecision == nil {
			continue
		}
		for _, evaluation := range attempt.RouteDecision.Evaluations {
			if evaluation.CandidateID != attempt.UpstreamID {
				continue
			}
			cost, ok := routing.EstimateActualCost(evaluation.Price, attempt.Protocol,
				attempt.InputTokens, attempt.OutputTokens, attempt.CachedTokens, attempt.CacheCreationTokens)
			if ok {
				total += cost
				found = true
			}
			break
		}
	}
	if !found {
		return nil
	}
	return &total
}

func routingAttemptSucceeded(outcome string) bool {
	switch outcome {
	case forward.OutcomeFailed, forward.OutcomePartial:
		return false
	default:
		return true
	}
}

func floatPtr(value float64) *float64 { return &value }

func optionalInt64Ptr(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func cacheTTLForProtocol(protocol string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "gemini":
		// Gemini context caches commonly use a one-hour default lifetime.
		return time.Hour
	case "claude", "anthropic", "openai", "openai-response", "responses", "codex":
		return 5 * time.Minute
	default:
		return 5 * time.Minute
	}
}

func (s *Server) emitRouteAuditLog(requestID string, started time.Time, model, endpoint string, features routing.RequestFeatures, decision *routing.Decision, result forward.Result) {
	entry := map[string]any{
		"type":                  "route_audit",
		"request_id":            requestID,
		"ts":                    started.UnixMilli(),
		"model":                 model,
		"endpoint":              endpoint,
		"session_id":            features.SessionID,
		"outcome":               result.Outcome,
		"status":                result.Status,
		"ttft_ms":               result.TTFTMs,
		"duration_ms":           time.Since(started).Milliseconds(),
		"input_tokens":          result.InputTokens,
		"output_tokens":         result.OutputTokens,
		"cached_tokens":         result.CachedTokens,
		"cache_creation_tokens": result.CacheCreationTokens,
		"features": map[string]any{
			"input_tokens":    features.InputTokens,
			"reusable_prefix": features.ReusableInputTokens,
			"session_id":      features.SessionID,
			"cache_key":       features.CacheKey,
			"stream":          features.Stream,
		},
	}
	if decision != nil {
		// Determine cache_state and ttl_type from the selected candidate's evaluation.
		cacheState := "cold"
		ttlType := ""
		for _, eval := range decision.Evaluations {
			if eval.CandidateID == decision.SelectedID {
				if eval.CacheExisting && decision.Cost.CacheUsed {
					cacheState = "hot"
				} else if eval.CacheExisting && !decision.Cost.CacheUsed {
					// Cache existed but decision chose not to use it (e.g. expired/stale).
					cacheState = "expired"
				}
				// Derive ttl_type from the cost estimate's cache lifetime hint.
				if eval.Cost.CacheEligible {
					ttl := cacheTTLForProtocol(eval.Protocol)
					if ttl >= time.Hour {
						ttlType = "1h"
					} else {
						ttlType = "5m"
					}
				}
				break
			}
		}

		// Build candidates_summary for quick scanning.
		candidates := make([]map[string]any, 0, len(decision.Evaluations))
		for _, eval := range decision.Evaluations {
			candidates = append(candidates, map[string]any{
				"name":     eval.CandidateName,
				"cost":     eval.EffectiveCost,
				"eligible": eval.Eligible,
				"selected": eval.CandidateID == decision.SelectedID,
			})
		}

		entry["cache_state"] = cacheState
		if ttlType != "" {
			entry["ttl_type"] = ttlType
		}
		entry["candidates_summary"] = candidates
		entry["decision"] = map[string]any{
			"selected_id":       decision.SelectedID,
			"selected_name":     decision.SelectedName,
			"reason":            decision.Reason,
			"effective_cost":    decision.EffectiveCost,
			"confidence":        decision.Confidence,
			"exploration":       decision.Exploration,
			"runner_up_id":      decision.RunnerUpID,
			"estimated_savings": decision.EstimatedSavings,
			"forecast_requests": decision.Forecast.Requests,
			"forecast_rpm":      decision.Forecast.RequestsPerMinute,
			"cache_used":        decision.Cost.CacheUsed,
			"break_even":        decision.Cost.BreakEvenRequests,
		}
	}
	attempts := make([]map[string]any, 0, len(result.Attempts))
	for _, a := range result.Attempts {
		attempts = append(attempts, map[string]any{
			"upstream_id":   a.UpstreamID,
			"outcome":       a.Outcome,
			"status":        a.Status,
			"ttft_ms":       a.TTFTMs,
			"duration_ms":   a.DurationMs,
			"cached":        a.CachedTokens,
			"cache_created": a.CacheCreationTokens,
		})
	}
	entry["attempts"] = attempts
	raw, _ := json.Marshal(entry)
	slog.Info(string(raw), "audit", "route")
}
