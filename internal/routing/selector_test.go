package routing

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func basePrice(input float64) Pricing {
	return Pricing{InputPerToken: input, OutputPerToken: 1e-6, InputKnown: true, OutputKnown: true, Multiplier: 1, Confidence: 0.8}
}

func healthyCandidate(id int64, name string, priority int, price Pricing) Candidate {
	return Candidate{ID: id, Name: name, Priority: priority, Healthy: true, SupportsModel: true, Price: price}
}

func TestChooseCrossesPriorityForLowerCost(t *testing.T) {
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 10_000, EstimatedOutputTokens: 100},
		Forecast: TrafficForecast{Requests: 1},
		Candidates: []Candidate{
			healthyCandidate(1, "priority expensive", 0, basePrice(5e-6)),
			healthyCandidate(2, "lower priority cheap", 10, basePrice(1e-6)),
		},
		Config: DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != 2 {
		t.Fatalf("selected %d, want lower-priority cheaper candidate 2; %+v", decision.SelectedID, decision)
	}
	if decision.EstimatedSavings <= 0 || !strings.Contains(decision.Reason, "lowest forecast cost") {
		t.Fatalf("missing cost explanation: %+v", decision)
	}
}

func TestChooseCacheCandidateAtRepeatedVolume(t *testing.T) {
	cachePrice := basePrice(2e-6)
	cachePrice.CacheWritePerToken = 2.5e-6
	cachePrice.CacheReadPerToken = 0.2e-6
	cachePrice.CacheWriteKnown = true
	cachePrice.CacheReadKnown = true
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 10_000, ReusableInputTokens: 9_000, EstimatedOutputTokens: 100},
		Forecast: TrafficForecast{Requests: 20, Window: 5 * time.Minute},
		Candidates: []Candidate{
			healthyCandidate(1, "no cache", 0, basePrice(1e-6)),
			func() Candidate {
				c := healthyCandidate(2, "cache", 5, cachePrice)
				c.Cache = CacheProfile{Supported: true, TTL: 5 * time.Minute, HitRate: 1, HitRateSource: HitRateObserved}
				return c
			}(),
		},
		Config: DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != 2 || !decision.Cost.CacheUsed {
		t.Fatalf("cache candidate should win repeated workload: %+v", decision)
	}
}

func TestChooseNearEqualCostUsesLatency(t *testing.T) {
	fast := healthyCandidate(1, "fast", 10, basePrice(1.005e-6))
	fast.Performance = Performance{Samples: 100, SuccessRate: 1, P95TTFTMs: 100}
	slow := healthyCandidate(2, "slow", 0, basePrice(1e-6))
	slow.Performance = Performance{Samples: 100, SuccessRate: 1, P95TTFTMs: 1_000}
	cfg := DefaultConfig()
	cfg.CostTieTolerance = 0.01
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 10_000}, Forecast: TrafficForecast{Requests: 1},
		Candidates: []Candidate{slow, fast}, Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != fast.ID || !strings.Contains(decision.Reason, "latency") {
		t.Fatalf("near-equal cost should prefer faster candidate: %+v", decision)
	}
}

func TestChooseRejectsUnknownPriceAndHardLatency(t *testing.T) {
	unknown := healthyCandidate(1, "unknown", 0, Pricing{})
	slow := healthyCandidate(2, "slow", 0, basePrice(1e-6))
	slow.Performance = Performance{Samples: 10, SuccessRate: 1, P95TTFTMs: 2_000}
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 100}, Candidates: []Candidate{unknown, slow},
		Config: Config{MaxTTFTMs: 1_000},
	})
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("error = %v, want ErrNoCandidate", err)
	}
	if decision.Evaluations[0].RejectReason == "" || decision.Evaluations[1].RejectReason == "" {
		t.Fatalf("missing rejection explanations: %+v", decision.Evaluations)
	}
}

func TestChooseAccountsForFailureRetryCost(t *testing.T) {
	unreliable := healthyCandidate(1, "cheap unreliable", 0, basePrice(0.6e-6))
	unreliable.Performance = Performance{Samples: 100, SuccessRate: 0.5, P95TTFTMs: 100}
	reliable := healthyCandidate(2, "reliable", 0, basePrice(1e-6))
	reliable.Performance = Performance{Samples: 100, SuccessRate: 1, P95TTFTMs: 200}
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 10_000}, Candidates: []Candidate{unreliable, reliable},
		Config: DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != reliable.ID {
		t.Fatalf("expected retry-adjusted reliable candidate, got %+v", decision)
	}
}

func TestChoosePenalizesFailuresBeforeMinimumSamples(t *testing.T) {
	unreliable := healthyCandidate(1, "cold failing", 0, basePrice(0.6e-6))
	unreliable.Performance = Performance{Samples: 2, SuccessRate: 0}
	reliable := healthyCandidate(2, "reliable", 0, basePrice(1e-6))
	reliable.Performance = Performance{Samples: 100, SuccessRate: 1}
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 10_000}, Candidates: []Candidate{unreliable, reliable},
		Config: DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != reliable.ID {
		t.Fatalf("two cold-start failures should raise retry-adjusted cost: %+v", decision)
	}
	if got := decision.Evaluations[0].EffectiveSuccessRate; got != 1.0/3.0 {
		t.Fatalf("effective success rate = %v, want 1/3", got)
	}
}

func TestChooseKeepsRecoveringCandidateEligibleBeforeMinimumSamples(t *testing.T) {
	candidate := healthyCandidate(1, "recovering", 0, basePrice(1e-6))
	candidate.Performance = Performance{Samples: 3, SuccessRate: 0}
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 10_000}, Candidates: []Candidate{candidate},
		Config: DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != candidate.ID || decision.Evaluations[0].EffectiveSuccessRate != 0.25 {
		t.Fatalf("recovering candidate should retain a bounded trial path: %+v", decision)
	}
}

func TestSelectorWrapperPick(t *testing.T) {
	selector := NewSelector(DefaultConfig())
	selected, decision, err := selector.Pick(Request{
		Features:   RequestFeatures{InputTokens: 100},
		Candidates: []Candidate{healthyCandidate(7, "only", 0, basePrice(1e-6))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != 7 || decision.SelectedID != 7 {
		t.Fatalf("wrapper selected %+v with decision %+v", selected, decision)
	}
}

func TestChooseExploresLeastObservedEligibleCandidate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cold := healthyCandidate(2, "cold", 10, basePrice(2e-6))
	cold.Performance.Samples = 0
	warm := healthyCandidate(1, "warm", 0, basePrice(1e-6))
	warm.Performance = Performance{Samples: 100, SuccessRate: 1}
	cfg := DefaultConfig()
	cfg.ExplorationRate = 1
	decision, err := Choose(Request{
		Features:   RequestFeatures{Model: "gpt-5", CacheKey: "session"},
		Candidates: []Candidate{warm, cold}, Config: cfg, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != cold.ID || !decision.Exploration {
		t.Fatalf("expected exploration sample of cold candidate: %+v", decision)
	}
}

// Exploration must NOT pick a candidate whose forecast cost is materially
// higher than the winner's. Previously the ceiling compared Price.Multiplier
// (typically ~1 everywhere), so a 100x-more-expensive fallback slipped through.
func TestChooseExplorationRejectsExpensiveCandidates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Both eligible under identical multipliers, but "expensive" is 100x pricier
	// per token; a real forecast must reject it as an exploration target.
	cheap := healthyCandidate(1, "cheap", 0, basePrice(1e-7))
	cheap.Performance = Performance{Samples: 100, SuccessRate: 1}
	expensive := healthyCandidate(2, "expensive", 10, basePrice(1e-5))
	expensive.Performance = Performance{Samples: 0}
	cfg := DefaultConfig()
	cfg.ExplorationRate = 1
	// Give the request non-zero tokens so EffectiveCost > 0 and the ceiling
	// is enforced. Otherwise every candidate has cost=0 and the safety
	// fallback for winnerCost==0 kicks in.
	decision, err := Choose(Request{
		Features: RequestFeatures{
			Model: "gpt-5", CacheKey: "session-expensive",
			InputTokens: 10_000, EstimatedOutputTokens: 100,
		},
		Candidates: []Candidate{cheap, expensive}, Config: cfg, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID == expensive.ID {
		t.Fatalf("100x-more-expensive candidate must not be explored: %+v", decision)
	}
}

// Rate=1.0 must always fire — regression for the float64→uint64 rounding bug
// that silently degraded rate=1.0 to ~50% fire rate.
func TestChooseExplorationRateOneAlwaysFires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cold := healthyCandidate(2, "cold", 10, basePrice(1e-7))
	cold.Performance.Samples = 0
	warm := healthyCandidate(1, "warm", 0, basePrice(1e-7))
	warm.Performance = Performance{Samples: 100, SuccessRate: 1}
	cfg := DefaultConfig()
	cfg.ExplorationRate = 1
	// Try 20 distinct cache_keys — ALL should explore at rate=1.0 regardless
	// of how the hash lands.
	for i := 0; i < 20; i++ {
		decision, err := Choose(Request{
			Features:   RequestFeatures{Model: "gpt-5", CacheKey: fmt.Sprintf("s-%d", i)},
			Candidates: []Candidate{warm, cold}, Config: cfg, Now: now,
		})
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if !decision.Exploration {
			t.Fatalf("iter %d: exploration must fire at rate=1.0, got winner=%s", i, decision.SelectedName)
		}
	}
}
