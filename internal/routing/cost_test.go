package routing

import (
	"math"
	"testing"
	"time"
)

func TestEstimateWindowCostNoCache(t *testing.T) {
	features := RequestFeatures{InputTokens: 1_000, ReusableInputTokens: 800, EstimatedOutputTokens: 100}
	price := Pricing{InputPerToken: 1e-6, OutputPerToken: 2e-6, InputKnown: true, OutputKnown: true, Multiplier: 1}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 10, Window: 15 * time.Minute}, price, CacheProfile{}, time.Time{}, 15*time.Minute)

	closeTo(t, cost.SelectedTotal, 0.012)
	closeTo(t, cost.NoCacheInputCost, 0.010)
	closeTo(t, cost.OutputCost, 0.002)
	if cost.CacheUsed || cost.CacheEligible {
		t.Fatalf("cache unexpectedly used: %+v", cost)
	}
}

func TestEstimateWindowCostCacheWinsAtVolume(t *testing.T) {
	features := RequestFeatures{InputTokens: 1_000, ReusableInputTokens: 800, EstimatedOutputTokens: 100}
	price := Pricing{
		InputPerToken: 1e-6, OutputPerToken: 2e-6,
		CacheWritePerToken: 1.25e-6, CacheReadPerToken: 0.1e-6,
		InputKnown: true, OutputKnown: true, CacheWriteKnown: true, CacheReadKnown: true,
		Multiplier: 1, Confidence: 0.9,
	}
	cache := CacheProfile{Supported: true, TTL: 15 * time.Minute, HitRate: 1, HitRateSource: HitRateObserved}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 10, Window: 15 * time.Minute}, price, cache, time.Time{}, 15*time.Minute)

	if !cost.CacheUsed || !cost.CacheEligible {
		t.Fatalf("cache should win: %+v", cost)
	}
	closeTo(t, cost.ExpectedCreates, 1)
	closeTo(t, cost.ExpectedHits, 9)
	closeTo(t, cost.CacheInputCost, 0.002)
	closeTo(t, cost.CacheWriteCost, 0.001)
	closeTo(t, cost.CacheReadCost, 0.00072)
	closeTo(t, cost.SelectedTotal, 0.00572)
	closeTo(t, cost.Savings, 0.00628)
	if cost.BreakEvenRequests <= 1 || cost.BreakEvenRequests >= 2 {
		t.Fatalf("break-even = %v, want between 1 and 2", cost.BreakEvenRequests)
	}
}

func TestEstimateWindowCostChoosesOrdinaryWhenWriteIsTooExpensive(t *testing.T) {
	features := RequestFeatures{InputTokens: 1_000, ReusableInputTokens: 900, EstimatedOutputTokens: 10}
	price := Pricing{
		InputPerToken: 1e-6, OutputPerToken: 2e-6,
		CacheWritePerToken: 3e-6, CacheReadPerToken: 0.1e-6,
		InputKnown: true, OutputKnown: true, CacheWriteKnown: true, CacheReadKnown: true,
	}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 1}, price,
		CacheProfile{Supported: true, HitRate: 1, HitRateSource: HitRateObserved}, time.Time{}, 5*time.Minute)

	if cost.CacheUsed {
		t.Fatalf("one expensive cache write should not win: %+v", cost)
	}
	if cost.CacheTotal <= cost.NoCacheTotal {
		t.Fatalf("cache total %v should exceed no-cache %v", cost.CacheTotal, cost.NoCacheTotal)
	}
	closeTo(t, cost.SelectedTotal, cost.NoCacheTotal)
}

func TestEstimateWindowCostDefaultsMissingCacheRates(t *testing.T) {
	features := RequestFeatures{InputTokens: 2_000, ReusableInputTokens: 1_800, EstimatedOutputTokens: 100}
	price := Pricing{InputPerToken: 1e-6, OutputPerToken: 2e-6, InputKnown: true, OutputKnown: true, Confidence: 0.8}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 20, Window: 15 * time.Minute}, price,
		CacheProfile{Supported: true, HitRate: 0.9, HitRateSource: HitRateObserved}, time.Time{}, 15*time.Minute)
	if !cost.PricingComplete || len(cost.Warnings) == 0 {
		t.Fatalf("missing cache rates should use an auditable default: %+v", cost)
	}
	if !cost.CacheUsed || cost.CacheTotal >= cost.NoCacheTotal {
		t.Fatalf("default cache rates should win this repeated workload: %+v", cost)
	}
}

func TestEstimateWindowCostAccountsForTTLLifetimes(t *testing.T) {
	features := RequestFeatures{InputTokens: 1_000, ReusableInputTokens: 800}
	price := Pricing{
		InputPerToken: 1, OutputPerToken: 0, CacheWritePerToken: 1.25, CacheReadPerToken: 0.1,
		InputKnown: true, OutputKnown: true, CacheWriteKnown: true, CacheReadKnown: true,
	}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 10, Window: 12 * time.Minute}, price,
		CacheProfile{Supported: true, TTL: 5 * time.Minute, HitRate: 1, HitRateSource: HitRateObserved}, time.Time{}, 15*time.Minute)

	closeTo(t, cost.CacheLifetimes, 3)
	closeTo(t, cost.ExpectedCreates, 3)
	closeTo(t, cost.ExpectedHits, 7)
}

func TestExistingCacheAvoidsInitialWrite(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	features := RequestFeatures{InputTokens: 1_000, ReusableInputTokens: 800}
	price := Pricing{
		InputPerToken: 1, OutputPerToken: 0, CacheWritePerToken: 2, CacheReadPerToken: 0.1,
		InputKnown: true, OutputKnown: true, CacheWriteKnown: true, CacheReadKnown: true,
	}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 2, Window: time.Minute}, price,
		CacheProfile{Supported: true, TTL: 5 * time.Minute, HitRate: 1, HitRateSource: HitRateObserved,
			Existing: CacheEntry{Valid: true, ExpiresAt: now.Add(2 * time.Minute)}}, now, 15*time.Minute)

	closeTo(t, cost.ExpectedCreates, 0)
	closeTo(t, cost.ExpectedHits, 2)
}

func closeTo(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-10*math.Max(1, math.Abs(want)) {
		t.Fatalf("got %.12g, want %.12g", got, want)
	}
}

// Claude can report a large cache read and a small cache creation in the same
// response. The forecast must not replace that creation with a full-prefix write.
func TestEstimateWindowCostUsesObservedCacheUsage(t *testing.T) {
	features := RequestFeatures{InputTokens: 153_813, ReusableInputTokens: 149_475, EstimatedOutputTokens: 10}
	price := Pricing{
		InputPerToken: 5e-6, OutputPerToken: 25e-6,
		CacheReadPerToken: 5e-7, CacheWritePerToken: 6.25e-6,
		InputKnown: true, OutputKnown: true, CacheReadKnown: true, CacheWriteKnown: true,
		Multiplier: 0.2,
	}
	cache := CacheProfile{
		Supported: true, TTL: 5 * time.Minute,
		ObservedRequests: 1, ObservedInputTokens: 49_801,
		ObservedCacheReadTokens: 284_384, ObservedCacheWriteTokens: 7_039,
		ObservedCacheCreateRate: 1,
	}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 2, Window: 15 * time.Minute}, price, cache, time.Time{}, 15*time.Minute)

	if !cost.CacheEligible {
		t.Fatalf("cache should be eligible: %+v", cost)
	}
	closeTo(t, cost.CacheInputCost, 2*49_801*5e-6*0.2)
	closeTo(t, cost.CacheReadCost, 2*284_384*5e-7*0.2)
	closeTo(t, cost.CacheWriteCost, 2*7_039*6.25e-6*0.2)
	closeTo(t, cost.CacheTotal, 2*0.43544075*0.2)
}

func TestCalculateUsageCost(t *testing.T) {
	price := Pricing{
		InputPerToken: 5e-6, OutputPerToken: 25e-6,
		CacheReadPerToken: 5e-7, CacheWritePerToken: 6.25e-6,
		InputKnown: true, OutputKnown: true, CacheReadKnown: true, CacheWriteKnown: true,
		Multiplier: 0.2,
	}
	cost, ok := CalculateUsageCost(49_801, 10, 284_384, 7_039, price)
	if !ok || math.Abs(cost-0.08708815) > 1e-9 {
		t.Fatalf("usage cost=%.9f ok=%v want 0.08708815", cost, ok)
	}
}
