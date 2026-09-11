package server

import (
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/store"
)

func TestActualRouteCostUsesAttemptDecisionAndSkipsFailedAttempts(t *testing.T) {
	price := routing.Pricing{
		InputPerToken: 1, OutputPerToken: 2, CacheReadPerToken: 0.1, CacheWritePerToken: 1.25,
		InputKnown: true, OutputKnown: true, CacheReadKnown: true, CacheWriteKnown: true,
		Multiplier: 0.5,
	}
	decision := &routing.Decision{Evaluations: []routing.CandidateEvaluation{{CandidateID: 2, Price: price}}}
	cost := actualRouteCost([]forward.AttemptResult{
		{UpstreamID: 1, Outcome: forward.OutcomeFailed, OutputTokens: 999, RouteDecision: decision},
		{UpstreamID: 2, Protocol: "claude", Outcome: forward.OutcomeSuccess,
			InputTokens: 100, OutputTokens: 10, CachedTokens: 80, CacheCreationTokens: 20,
			RouteDecision: decision},
	})
	if cost == nil || *cost != (100+20+8+25)*0.5 {
		t.Fatalf("actual route cost = %v", cost)
	}
}

func TestPersistRoutingAuditStoresNoCandidateDecision(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := &Server{store: st}
	requestID := "00000000-0000-0000-0000-000000000001"
	srv.persistRoutingAudit(requestID, time.Now(), 3, "claude-test", "/v1/messages", forward.Result{
		Outcome: forward.OutcomeUnavailable,
		RouteDecision: &routing.Decision{
			Reason: "no eligible intelligent-routing candidate",
			Evaluations: []routing.CandidateEvaluation{{
				CandidateID: 19, CandidateName: "primary", RejectReason: "upstream is unhealthy",
			}},
		},
	})
	entry, err := st.GetRouteDecisionByRequestID(requestID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.SelectedUpstreamID != 0 || entry.Reason == "" || len(entry.Candidates) != 1 ||
		entry.Candidates[0].RejectionReason != "upstream is unhealthy" {
		t.Fatalf("no-candidate audit was not preserved: %+v", entry)
	}
}

func TestActualRouteCostStaysUnknownWithoutUsageOrPricing(t *testing.T) {
	decision := &routing.Decision{Evaluations: []routing.CandidateEvaluation{{CandidateID: 1}}}
	if cost := actualRouteCost([]forward.AttemptResult{{
		UpstreamID: 1, Outcome: forward.OutcomeSuccess, RouteDecision: decision,
	}}); cost != nil {
		t.Fatalf("unknown cost must remain nil, got %v", *cost)
	}
}
