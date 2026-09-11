package routing

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Provider cache prices are not universally exposed. These conservative
// ratios are only used after cache support has been explicitly enabled or
// observed from a real response, and are recorded as warnings in the decision
// audit so an operator can replace them with provider data later.
const (
	defaultCacheReadRatio  = 0.5
	defaultCacheWriteRatio = 1.25
)

// CostEstimate is a full auditable cost forecast for one candidate. All costs
// use the candidate's currency units (normally USD). The no-cache baseline is
// always populated, even when cache pricing is incomplete.
type CostEstimate struct {
	Window             time.Duration `json:"window"`
	Requests           float64       `json:"requests"`
	PrefixTokens       float64       `json:"prefix_tokens"`
	SuffixTokens       float64       `json:"suffix_tokens"`
	OutputTokensPerReq float64       `json:"output_tokens_per_request"`
	NoCacheTotal       float64       `json:"no_cache_total"`
	CacheTotal         float64       `json:"cache_total"`
	SelectedTotal      float64       `json:"selected_total"`
	NoCacheInputCost   float64       `json:"no_cache_input_cost"`
	CacheInputCost     float64       `json:"cache_input_cost"`
	CacheReadCost      float64       `json:"cache_read_cost"`
	CacheWriteCost     float64       `json:"cache_write_cost"`
	OutputCost         float64       `json:"output_cost"`
	ExpectedHits       float64       `json:"expected_hits"`
	ExpectedMisses     float64       `json:"expected_misses"`
	ExpectedCreates    float64       `json:"expected_creates"`
	CacheLifetimes     float64       `json:"cache_lifetimes"`
	BreakEvenRequests  float64       `json:"break_even_requests"`
	Savings            float64       `json:"savings"`
	CacheEligible      bool          `json:"cache_eligible"`
	CacheUsed          bool          `json:"cache_used"`
	PricingComplete    bool          `json:"pricing_complete"`
	Confidence         float64       `json:"confidence"`
	Warnings           []string      `json:"warnings,omitempty"`
}

// EstimateActualCost prices observed usage. inputTokens is assumed to be the
// uncached prompt total under Anthropic semantics — audit.parseUsageObject
// normalizes OpenAI/Gemini prompt_tokens (which are inclusive of cached) at
// ingest time, so callers should pass the stored routing_observations.input
// value directly. The protocol argument is accepted for backward compatibility
// and future protocol-specific pricing quirks but no longer used for token
// arithmetic.
func EstimateActualCost(price Pricing, protocol string, inputTokens, outputTokens,
	cachedTokens, cacheCreationTokens int64) (float64, bool) {
	_ = protocol
	if inputTokens < 0 || outputTokens < 0 || cachedTokens < 0 || cacheCreationTokens < 0 {
		return 0, false
	}
	if inputTokens == 0 && outputTokens == 0 && cachedTokens == 0 && cacheCreationTokens == 0 {
		return 0, false
	}
	price = price.Normalized()
	if (inputTokens > 0 && !price.InputKnown) || (outputTokens > 0 && !price.OutputKnown) ||
		(cachedTokens > 0 && !price.CacheReadKnown) ||
		(cacheCreationTokens > 0 && !price.CacheWriteKnown) {
		return 0, false
	}
	cost := float64(inputTokens)*price.InputPerToken +
		float64(outputTokens)*price.OutputPerToken +
		float64(cachedTokens)*price.CacheReadPerToken +
		float64(cacheCreationTokens)*price.CacheWritePerToken
	return cost * price.Multiplier, true
}

// EstimateWindowCost computes both strategies for the expected request
// window. The cache strategy assumes a cache write replaces ordinary input
// billing for the reusable prefix unless the profile explicitly says
// otherwise. Cache reads and writes are independent: rolling conversations may
// report both cached and cache_creation tokens in one request.
func EstimateWindowCost(features RequestFeatures, forecast TrafficForecast, pricing Pricing, cache CacheProfile, now time.Time, defaultWindow time.Duration) CostEstimate {
	features = features.Normalize()
	forecast = forecast.normalized(features, defaultWindow)
	pricing = pricing.Normalized()

	// InputInflation corrects for upstreams that inject additional tokens
	// (system prompts, tool definitions) not visible to the router at decision
	// time. These injected tokens are stable across requests in a session, so on
	// cache-supporting upstreams they participate in the cache lifecycle just
	// like the client-supplied prefix — they are billed as cache_read on hits
	// and cache_creation on misses, not at the full input rate.
	originalInputTokens := features.InputTokens
	if cache.InputInflation > 1 {
		inflated := int64(float64(features.InputTokens) * cache.InputInflation)
		if inflated > features.InputTokens {
			features.InputTokens = inflated
		}
	}
	inflationTokens := float64(features.InputTokens - originalInputTokens)
	if inflationTokens < 0 {
		inflationTokens = 0
	}

	prefix := float64(features.ReusableInputTokens)
	if !cache.Supported || (cache.MinTokens > 0 && int64(prefix) < cache.MinTokens) {
		prefix = 0
	}
	if prefix < 0 || math.IsNaN(prefix) || math.IsInf(prefix, 0) {
		prefix = 0
	}
	// CoverageRatio models upstreams that only cache a fraction of the prefix.
	// The uncached portion is billed at full input price even on hits. It also
	// applies to the inflation tokens the upstream injects around the prefix.
	coverage := cache.CoverageRatio
	if coverage <= 0 || coverage > 1 || math.IsNaN(coverage) || math.IsInf(coverage, 0) {
		coverage = 1
	}
	// cacheable tokens = reusable prefix + upstream-injected inflation. Both are
	// stable across the session and share the provider cache lifecycle.
	cacheable := prefix + inflationTokens
	cachedPortion := cacheable * coverage
	uncachedPortion := cacheable - cachedPortion
	// true suffix = the newly appended user turn, never cacheable.
	suffix := float64(originalInputTokens) - prefix
	if suffix < 0 {
		suffix = 0
	}
	out := forecast.OutputTokensPerReq
	n := forecast.Requests
	multiplier := pricing.Multiplier
	input := pricing.InputPerToken
	output := pricing.OutputPerToken

	result := CostEstimate{
		Window:             forecast.Window,
		Requests:           n,
		PrefixTokens:       prefix,
		SuffixTokens:       suffix,
		OutputTokensPerReq: out,
		PricingComplete:    pricing.Complete(prefix > 0 && cache.Supported),
		Confidence:         pricing.Confidence,
		Warnings:           []string{},
	}
	if !pricing.InputKnown {
		result.Warnings = append(result.Warnings, "input price is unknown")
	}
	if !pricing.OutputKnown {
		result.Warnings = append(result.Warnings, "output price is unknown")
	}
	if prefix > 0 && cache.Supported && !pricing.CacheReadKnown && !pricing.InputKnown {
		result.Warnings = append(result.Warnings, "cache read price is unknown")
	}
	if prefix > 0 && cache.Supported && !pricing.CacheWriteKnown && !pricing.InputKnown {
		result.Warnings = append(result.Warnings, "cache write price is unknown")
	}
	if result.Confidence == 0 && result.PricingComplete {
		result.Confidence = 0.5
	}

	result.NoCacheInputCost = n * float64(features.InputTokens) * input * multiplier
	result.OutputCost = n * out * output * multiplier
	result.NoCacheTotal = result.NoCacheInputCost + result.OutputCost

	if prefix == 0 || !cache.Supported {
		result.CacheTotal = result.NoCacheTotal
		result.SelectedTotal = result.NoCacheTotal
		result.CacheInputCost = result.NoCacheInputCost
		result.Savings = 0
		result.BreakEvenRequests = -1
		if cache.Supported && prefix == 0 {
			result.Warnings = append(result.Warnings, "reusable prefix is below cache minimum")
		}
		return result
	}
	result.CacheEligible = true
	if !pricing.CacheReadKnown && pricing.InputKnown {
		pricing.CacheReadPerToken = pricing.InputPerToken * defaultCacheReadRatio
		pricing.CacheReadKnown = true
		result.Warnings = append(result.Warnings, "cache read price unavailable; using conservative default")
		if result.Confidence == 0 || result.Confidence > 0.45 {
			result.Confidence = 0.45
		}
	}
	if !pricing.CacheWriteKnown && pricing.InputKnown {
		pricing.CacheWritePerToken = pricing.InputPerToken * defaultCacheWriteRatio
		pricing.CacheWriteKnown = true
		result.Warnings = append(result.Warnings, "cache write price unavailable; using conservative default")
		if result.Confidence == 0 || result.Confidence > 0.45 {
			result.Confidence = 0.45
		}
	}
	result.PricingComplete = pricing.Complete(true)

	lifetimes, initialExisting := cacheLifetimes(cache, forecast.Window, n, now)
	hitRate := cache.HitRate
	switch cache.HitRateSource {
	case HitRateObserved:
		// use HitRate as-is
	case HitRatePrior:
		result.Warnings = append(result.Warnings, "using optimistic prior hit rate; will converge to observed")
	case HitRateUnknown, "":
		hitRate = 0
		result.Warnings = append(result.Warnings, "cache hit rate is unknown; assuming misses")
	}
	if hitRate < 0 || math.IsNaN(hitRate) || math.IsInf(hitRate, 0) {
		hitRate = 0
	}
	if hitRate > 1 {
		hitRate = 1
	}

	// One guaranteed miss starts each new TTL lifetime. An existing valid
	// entry converts the first request into a hit; the measured hit rate is
	// applied to requests left after those deterministic events.
	guaranteedMisses := lifetimes
	guaranteedHits := float64(0)
	if initialExisting && n > 0 {
		guaranteedMisses--
		guaranteedHits = 1
	}
	if guaranteedMisses < 0 {
		guaranteedMisses = 0
	}
	if guaranteedMisses > n {
		guaranteedMisses = n
	}
	remaining := n - guaranteedMisses - guaranteedHits
	if remaining < 0 {
		remaining = 0
	}
	hits := guaranteedHits + remaining*hitRate
	misses := n - hits
	if misses < 0 {
		misses = 0
	}
	result.ExpectedHits = hits
	result.ExpectedMisses = misses
	result.ExpectedCreates = misses
	// cachedPortion covers reusable prefix AND upstream-injected inflation.
	// Both are stable across the session and share the provider cache lifecycle.
	writeTokens := misses * cachedPortion
	if cache.CacheWriteObserved {
		observedCreates := n * cache.CreateRate
		if observedCreates > result.ExpectedCreates {
			result.ExpectedCreates = observedCreates
		}
		observedWriteTokens := n * cache.CreateTokensPerRequest
		if observedWriteTokens > writeTokens {
			writeTokens = observedWriteTokens
			result.Warnings = append(result.Warnings, "cache writes overlap hits; using observed rolling write-token rate")
		}
	}
	result.CacheLifetimes = lifetimes

	result.CacheReadCost = hits * cachedPortion * pricing.CacheReadPerToken * multiplier
	result.CacheWriteCost = writeTokens * pricing.CacheWritePerToken * multiplier
	// true suffix (new user turn) + uncached portion of prefix+inflation always
	// pay full input price. Inflation tokens ONLY appear at full input price via
	// the uncached share of coverage; the cached share flows through read/write.
	result.CacheInputCost = n * (suffix + uncachedPortion) * input * multiplier
	if cache.CacheReadIncludesInput {
		result.CacheReadCost += hits * cachedPortion * input * multiplier
	}
	if cache.CacheWriteIncludesInput {
		result.CacheWriteCost += writeTokens * input * multiplier
	}
	result.CacheTotal = result.CacheInputCost + result.CacheReadCost + result.CacheWriteCost + result.OutputCost
	result.SelectedTotal = result.NoCacheTotal
	if cache.Required || result.CacheTotal < result.NoCacheTotal {
		result.CacheUsed = true
		result.SelectedTotal = result.CacheTotal
	}
	result.Savings = result.NoCacheTotal - result.SelectedTotal
	result.BreakEvenRequests = breakEvenRequests(features, forecast, pricing, cache, multiplier)
	return result
}

// cacheLifetimes estimates how many writes the window needs. TTL-less caches
// are treated as one lifetime. If a known entry expires partway through the
// window, its remaining lifetime is counted separately from subsequent TTLs.
func cacheLifetimes(cache CacheProfile, window time.Duration, requests float64, now time.Time) (lifetimes float64, existing bool) {
	if requests <= 0 {
		return 0, false
	}
	existing = cache.Existing.Valid && (cache.Existing.ExpiresAt.IsZero() || cache.Existing.ExpiresAt.After(now))
	if cache.TTL <= 0 {
		return 1, existing
	}
	if window <= 0 {
		window = cache.TTL
	}
	if existing && !cache.Existing.ExpiresAt.IsZero() {
		remaining := cache.Existing.ExpiresAt.Sub(now)
		if remaining <= 0 {
			existing = false
		} else if remaining >= window {
			return 1, existing
		} else if remaining < window {
			lifetimes = 1 + math.Ceil(float64(window-remaining)/float64(cache.TTL))
			if lifetimes > requests {
				lifetimes = requests
			}
			return lifetimes, existing
		}
	}
	lifetimes = math.Ceil(float64(window) / float64(cache.TTL))
	if lifetimes < 1 {
		lifetimes = 1
	}
	if lifetimes > requests {
		lifetimes = requests
	}
	return lifetimes, existing
}

// breakEvenRequests solves the first-lifetime, perfect-repeat case. It is an
// intentionally transparent diagnostic rather than a replacement for the
// full TTL-aware estimate. -1 means the cache never becomes cheaper under the
// supplied prices, or the equation is not defined.
func breakEvenRequests(features RequestFeatures, forecast TrafficForecast, pricing Pricing, cache CacheProfile, multiplier float64) float64 {
	features = features.Normalize()
	originalInputTokens := features.InputTokens
	if cache.InputInflation > 1 {
		inflated := int64(float64(features.InputTokens) * cache.InputInflation)
		if inflated > features.InputTokens {
			features.InputTokens = inflated
		}
	}
	inflationTokens := float64(features.InputTokens - originalInputTokens)
	if inflationTokens < 0 {
		inflationTokens = 0
	}
	prefix := float64(features.ReusableInputTokens)
	if !cache.Supported || prefix == 0 || (cache.MinTokens > 0 && int64(prefix) < cache.MinTokens) {
		return -1
	}
	coverage := cache.CoverageRatio
	if coverage <= 0 || coverage > 1 || math.IsNaN(coverage) || math.IsInf(coverage, 0) {
		coverage = 1
	}
	cacheable := prefix + inflationTokens
	cachedPortion := cacheable * coverage
	uncachedPortion := cacheable - cachedPortion
	input := pricing.InputPerToken * multiplier
	out := forecast.OutputTokensPerReq * pricing.OutputPerToken * multiplier
	suffix := float64(originalInputTokens) - prefix
	if suffix < 0 {
		suffix = 0
	}
	noCache := float64(features.InputTokens)*input + out
	h := cache.HitRate
	if cache.HitRateSource == HitRateUnknown || cache.HitRateSource == "" {
		h = 0
	}
	if h < 0 || h > 1 || math.IsNaN(h) || math.IsInf(h, 0) {
		h = 0
	}
	miss := (suffix+uncachedPortion)*input + cachedPortion*pricing.CacheWritePerToken*multiplier + out
	hit := (suffix+uncachedPortion)*input + cachedPortion*pricing.CacheReadPerToken*multiplier + out
	if cache.CacheWriteIncludesInput {
		miss += cachedPortion * input
	}
	if cache.CacheReadIncludesInput {
		hit += cachedPortion * input
	}
	// Expected subsequent request cost at the observed hit rate.
	steady := h*hit + (1-h)*miss
	if cache.CacheWriteObserved {
		observedWriteTokens := cache.CreateTokensPerRequest
		modeledWriteTokens := (1 - h) * cachedPortion
		if observedWriteTokens > modeledWriteTokens {
			steady += (observedWriteTokens - modeledWriteTokens) * pricing.CacheWritePerToken * multiplier
			if cache.CacheWriteIncludesInput {
				steady += (observedWriteTokens - modeledWriteTokens) * pricing.InputPerToken * multiplier
			}
		}
	}
	denominator := noCache - steady
	if denominator <= 0 {
		return -1
	}
	first := miss
	be := 1 + (first-noCache)/denominator
	if be < 1 {
		be = 1
	}
	return be
}

// EstimateWindowCostWithConfig is the convenience form used by selectors.
func EstimateWindowCostWithConfig(features RequestFeatures, forecast TrafficForecast, pricing Pricing, cache CacheProfile, now time.Time, cfg Config) CostEstimate {
	cfg = cfg.normalized()
	return EstimateWindowCost(features, forecast, pricing, cache, now, cfg.Window)
}

// FormatCostReason returns a compact human-readable explanation suitable for
// route audit records and admin previews.
func FormatCostReason(cost CostEstimate) string {
	if !cost.CacheEligible {
		return "cache unavailable; using ordinary input pricing"
	}
	if cost.CacheUsed && cost.Savings > 0 {
		if cost.BreakEvenRequests > 0 {
			return "cache forecast saves " + formatMoney(cost.Savings) + " (break-even at " + formatNumber(cost.BreakEvenRequests) + " requests)"
		}
		return "cache forecast saves " + formatMoney(cost.Savings)
	}
	if cost.CacheUsed && cost.Savings < 0 {
		return "required cache costs " + formatMoney(-cost.Savings) + " more than ordinary input"
	}
	if cost.CacheTotal > cost.NoCacheTotal {
		return "ordinary input is " + formatMoney(cost.CacheTotal-cost.NoCacheTotal) + " cheaper than cache"
	}
	return "cache and ordinary input forecasts cost the same"
}

func formatMoney(value float64) string {
	return formatNumber(value) + " cost units"
}

func formatNumber(value float64) string {
	if math.Abs(value-math.Round(value)) < 1e-9 {
		return strconv.FormatFloat(value, 'f', 0, 64)
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 6, 64), "0"), ".")
}
