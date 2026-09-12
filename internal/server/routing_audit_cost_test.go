package server

import (
	"math"
	"testing"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/routing"
)

func TestRouteActualCostUsesSelectedPricingSnapshot(t *testing.T) {
	decision := &routing.Decision{
		SelectedID: 64,
		Evaluations: []routing.CandidateEvaluation{{
			CandidateID: 64,
			Pricing: routing.Pricing{
				InputPerToken: 5e-6, OutputPerToken: 25e-6,
				CacheReadPerToken: 5e-7, CacheWritePerToken: 6.25e-6,
				InputKnown: true, OutputKnown: true, CacheReadKnown: true,
				CacheWriteKnown: true, Multiplier: 0.2,
			},
		}},
	}
	result := forward.Result{
		FinalUpstreamID: 64, InputTokens: 49_801, OutputTokens: 10,
		CachedTokens: 284_384, CacheCreationTokens: 7_039,
	}
	cost, ok := routeActualCost(decision, result)
	if !ok || math.Abs(cost-0.08708815) > 1e-9 {
		t.Fatalf("actual cost=%.9f ok=%v want 0.08708815", cost, ok)
	}
}
