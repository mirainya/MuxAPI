package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// intelligentRouter adapts the durable store/health observations to the
// protocol-independent routing cost model. The short-lived read-through
// caches reduce database work on the request hot path; PostgreSQL remains the
// source of truth and the existing health manager supplies fast breaker state.
type intelligentRouter struct {
	store     *store.Store
	config    func() routing.Config
	mu        sync.Mutex
	billing   billingCacheEntry
	prices    map[string]priceCacheEntry
	stats     map[statsCacheKey]statsCacheEntry
	prefix    map[prefixCacheKey]prefixCacheEntry
	inflation map[int64]inflationCacheEntry
}

const (
	billingCacheTTL = 30 * time.Second
	routingCacheTTL = 5 * time.Second
	maxPrefixCache  = 10_000
)

type billingCacheEntry struct {
	expires time.Time
	values  map[int64]store.BillingStatus
}

type priceCacheEntry struct {
	expires time.Time
	value   store.ModelPricing
	err     error
}

type statsCacheKey struct {
	upstreamID int64
	model      string
	window     time.Duration
}

type statsCacheEntry struct {
	expires time.Time
	value   store.UpstreamRoutingStats
	err     error
}

type prefixCacheKey struct {
	apiKeyHash string
	upstreamID int64
	model      string
	prefixHash string
	window     time.Duration
}

type prefixCacheEntry struct {
	expires time.Time
	value   store.PrefixCacheStats
	err     error
}

type inflationCacheEntry struct {
	expires time.Time
	value   float64
}

func (r *intelligentRouter) pick(s *Scheduler, groupID int64, model string, features routing.RequestFeatures, exclude map[int64]bool) (*upstream.Upstream, health.Lease, routing.Decision, error) {
	all := s.list(groupID)
	if len(all) == 0 {
		return nil, health.Lease{}, routing.Decision{}, ErrNoUpstream
	}
	blocked := make(map[int64]bool, len(exclude))
	for id, value := range exclude {
		blocked[id] = value
	}
	now := time.Now()
	cfg := routing.DefaultConfig()
	if r.config != nil {
		cfg = r.config()
	}
	var lastDecision routing.Decision
	for attempts := 0; attempts < len(all); attempts++ {
		candidates := r.candidates(s, all, model, features, blocked, now, cfg)
		if len(candidates) == 0 {
			break
		}
		decision, err := routing.Choose(routing.Request{
			Features: features, Forecast: r.forecast(all, model, features, cfg, now),
			Candidates: candidates, Config: cfg, Now: now,
		})
		lastDecision = decision
		blockHardLimitRejections(blocked, decision.Evaluations, cfg)
		if err != nil {
			break
		}
		for _, candidate := range all {
			if candidate.ID != decision.SelectedID || blocked[candidate.ID] {
				continue
			}
			if lease, ok := s.health.Claim(groupID, candidate.ID, model); ok {
				return candidate, lease, decision, nil
			}
			blocked[candidate.ID] = true
			break
		}
	}
	// Unknown price, a cold catalog, or a claim race must never make the
	// gateway unavailable. Preserve the original scheduler behavior.
	candidate, lease, err := s.PickExcluding(groupID, model, blocked)
	if candidate == nil || err != nil {
		if lastDecision.Reason == "" && len(lastDecision.Evaluations) > 0 {
			lastDecision.Reason = "no eligible intelligent-routing candidate"
		}
		return candidate, lease, lastDecision, err
	}
	return candidate, lease, routing.Decision{
		SelectedID: candidate.ID, SelectedName: candidate.Name,
		Reason:   "fallback: cost data incomplete; standard health/P2C scheduler",
		Forecast: routing.TrafficForecast{Window: cfg.Window},
	}, nil
}

func blockHardLimitRejections(blocked map[int64]bool, evaluations []routing.CandidateEvaluation, cfg routing.Config) {
	for _, evaluation := range evaluations {
		if evaluation.Samples <= 0 {
			continue
		}
		if cfg.MaxTTFTMs > 0 && evaluation.P95TTFTMs > cfg.MaxTTFTMs {
			blocked[evaluation.CandidateID] = true
			continue
		}
		if cfg.MaxDurationMs > 0 && evaluation.P95DurationMs > cfg.MaxDurationMs {
			blocked[evaluation.CandidateID] = true
		}
	}
}

func (r *intelligentRouter) candidates(s *Scheduler, all []*upstream.Upstream, model string, features routing.RequestFeatures, blocked map[int64]bool, now time.Time, cfg routing.Config) []routing.Candidate {
	statuses := r.billingStatuses(now)
	out := make([]routing.Candidate, 0, len(all))
	for _, item := range all {
		if item == nil || blocked[item.ID] {
			continue
		}
		state := ""
		if reporter, ok := s.health.(interface{ EffectiveState(int64) string }); ok {
			state = reporter.EffectiveState(item.ID)
		}
		healthy := state != "OPEN"
		supports := s.health.IsAvailable(item.ID, model)
		price := r.price(item, model, statuses)
		performance := r.performance(item.ID, model, cfg.Window, now, s.health)
		cache := r.cache(item, model, features, now, cfg.Window)
		out = append(out, routing.Candidate{
			ID: item.ID, Name: item.Name, Protocol: item.Protocol,
			Priority: item.Priority, Weight: item.Weight, Healthy: healthy,
			SupportsModel: supports, Price: price, Cache: cache,
			Performance: performance,
			LastError:   state,
		})
	}
	return out
}

func (r *intelligentRouter) price(item *upstream.Upstream, model string, statuses map[int64]store.BillingStatus) routing.Pricing {
	catalogPrice, err := r.modelPricing(model, time.Now())
	if err != nil {
		return routing.Pricing{}
	}

	params := routing.PricingParams{
		InputCostPerToken:  catalogPrice.InputCostPerToken,
		OutputCostPerToken: catalogPrice.OutputCostPerToken,
		CacheReadPerToken:  catalogPrice.CacheReadInputTokenCost,
		CacheWritePerToken: catalogPrice.CacheWriteInputTokenCost,
		CreditRatio:        item.CreditRatio,
	}

	if status, ok := statuses[item.ID]; ok {
		if status.EffectiveMultiplier != nil && *status.EffectiveMultiplier > 0 {
			params.EffectiveMultiplier = *status.EffectiveMultiplier
		} else if status.GroupMultiplier != nil && *status.GroupMultiplier > 0 {
			params.GroupMultiplier = *status.GroupMultiplier
		} else {
			params.LastKnownMultiplier = r.lastKnownMultiplier(item.ID)
		}
	}

	return routing.ResolvePricing(params)
}

func (r *intelligentRouter) performance(id int64, model string, window time.Duration, now time.Time, h Health) routing.Performance {
	stats, err := r.upstreamStats(id, model, window, now)
	if err != nil {
		return routing.Performance{InFlight: h.InFlight(id), Samples: 0}
	}
	samples := stats.Requests
	if samples == 0 {
		// The in-memory EWMA survives the transition before durable routing
		// observations accumulate, but it is not a reliability sample.
		latency := float64(h.LatencyEWMA(id))
		return routing.Performance{P50TTFTMs: latency, P95TTFTMs: latency, InFlight: h.InFlight(id)}
	}
	return routing.Performance{
		Samples: samples, P50TTFTMs: float64(stats.P50TTFTMs),
		P95TTFTMs: float64(stats.P95TTFTMs), P95DurationMs: float64(stats.P95DurationMs),
		SuccessRate: stats.SuccessRate, InFlight: h.InFlight(id),
	}
}

func (r *intelligentRouter) cache(item *upstream.Upstream, model string, features routing.RequestFeatures, now time.Time, window time.Duration) routing.CacheProfile {
	if features.ReusableInputTokens <= 0 {
		return routing.CacheProfile{}
	}
	cacheMode, _ := upstream.NormalizeCacheMode(item.CacheMode)
	if cacheMode == upstream.CacheDisabled {
		return routing.CacheProfile{}
	}

	keyHash := hashCredential(item.APIKey)
	prefixHash := features.CacheKey
	if features.SessionID != "" && features.SessionID != features.CacheKey {
		prefixHash = features.SessionID
	}

	stats, err := r.prefixStats(keyHash, item.ID, model, prefixHash, item.Protocol, window, now)
	observed := err == nil
	cacheObserved := observed && (stats.HitCount > 0 || stats.CreateCount > 0)
	supported := cacheMode == upstream.CacheEnabled || cacheMode == upstream.CacheAuto || cacheObserved
	if !supported {
		return routing.CacheProfile{}
	}

	params := routing.CacheStateParams{
		Supported:     true,
		CacheMode:     string(cacheMode),
		CoverageRatio: r.cacheCoverage(item.ID),
		Protocol:      item.Protocol,
	}
	if observed {
		params.Observations = stats.WindowObservations
		params.HitCount = stats.WindowHitCount
		params.MissCount = stats.WindowMissCount
		params.CreateCount = stats.CreateCount
		params.WindowCreateCount = stats.WindowCreateCount
		params.WindowCreateTokens = stats.WindowCreateTokens
		params.PrefixTokens = stats.PrefixTokens
		params.ExpiresAt = stats.ExpiresAt
		params.FirstSeenAt = stats.FirstSeenAt
		params.LastHitAt = stats.LastHitAt
	}

	sc := routing.ResolveSessionCache(params, now)
	profile := sc.ToCacheProfile()
	profile.InputInflation = r.tokenInflation(item.ID, now)
	return profile
}

func (r *intelligentRouter) forecast(all []*upstream.Upstream, model string, features routing.RequestFeatures, cfg routing.Config, now time.Time) routing.TrafficForecast {
	samples := make([]routing.UpstreamTrafficSample, 0, len(all))
	for _, item := range all {
		if item == nil {
			continue
		}
		stats, err := r.upstreamStats(item.ID, model, cfg.Window, now)
		if err != nil || stats.Requests == 0 {
			continue
		}
		samples = append(samples, routing.UpstreamTrafficSample{
			RequestsPerMinute: stats.RequestsPerMinute,
			OutputPerRequest:  stats.OutputPerRequest,
		})
	}
	return routing.BuildForecast(samples, cfg.Window)
}

func hashCredential(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (r *intelligentRouter) billingStatuses(now time.Time) map[int64]store.BillingStatus {
	r.mu.Lock()
	if r.billing.values != nil && now.Before(r.billing.expires) {
		values := r.billing.values
		r.mu.Unlock()
		return values
	}
	r.mu.Unlock()
	values, err := r.store.ListBillingStatuses()
	if err != nil {
		return nil
	}
	r.mu.Lock()
	r.billing = billingCacheEntry{expires: now.Add(billingCacheTTL), values: values}
	r.mu.Unlock()
	return values
}

func (r *intelligentRouter) lastKnownMultiplier(upstreamID int64) float64 {
	m, _ := r.store.LastKnownMultiplier(upstreamID)
	return m
}

func (r *intelligentRouter) cacheCoverage(upstreamID int64) float64 {
	ratio, _ := r.store.CacheCoverageRatio(upstreamID)
	if ratio <= 0 || ratio > 1 {
		return 1
	}
	return ratio
}

func (r *intelligentRouter) tokenInflation(upstreamID int64, now time.Time) float64 {
	r.mu.Lock()
	if r.inflation != nil {
		if entry, ok := r.inflation[upstreamID]; ok && now.Before(entry.expires) {
			r.mu.Unlock()
			return entry.value
		}
	}
	r.mu.Unlock()
	factor, _ := r.store.TokenInflationFactor(upstreamID)
	if factor < 1 {
		factor = 1
	}
	r.mu.Lock()
	if r.inflation == nil {
		r.inflation = make(map[int64]inflationCacheEntry)
	}
	r.inflation[upstreamID] = inflationCacheEntry{expires: now.Add(billingCacheTTL), value: factor}
	r.mu.Unlock()
	return factor
}

func (r *intelligentRouter) modelPricing(model string, now time.Time) (store.ModelPricing, error) {
	key := strings.TrimSpace(model)
	r.mu.Lock()
	if r.prices != nil {
		if entry, ok := r.prices[key]; ok && now.Before(entry.expires) {
			r.mu.Unlock()
			return entry.value, entry.err
		}
	}
	r.mu.Unlock()
	value, err := r.store.LookupModelPricing(key)
	ttl := time.Hour
	if err != nil {
		// The pricing manager may populate the catalog shortly after startup;
		// do not pin a cold-start miss for the whole successful-price TTL.
		ttl = routingCacheTTL
	}
	r.mu.Lock()
	if r.prices == nil {
		r.prices = make(map[string]priceCacheEntry)
	}
	r.prices[key] = priceCacheEntry{expires: now.Add(ttl), value: value, err: err}
	r.mu.Unlock()
	return value, err
}

func (r *intelligentRouter) upstreamStats(id int64, model string, window time.Duration, now time.Time) (store.UpstreamRoutingStats, error) {
	key := statsCacheKey{upstreamID: id, model: model, window: window}
	r.mu.Lock()
	if entry, ok := r.stats[key]; ok && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.value, entry.err
	}
	r.mu.Unlock()
	value, err := r.store.GetUpstreamRoutingStats(id, model, window, now)
	r.mu.Lock()
	if r.stats == nil {
		r.stats = make(map[statsCacheKey]statsCacheEntry)
	}
	r.stats[key] = statsCacheEntry{expires: now.Add(routingCacheTTL), value: value, err: err}
	r.mu.Unlock()
	return value, err
}

func (r *intelligentRouter) prefixStats(apiKeyHash string, upstreamID int64, model, prefixHash, protocol string, window time.Duration, now time.Time) (store.PrefixCacheStats, error) {
	key := prefixCacheKey{apiKeyHash: apiKeyHash, upstreamID: upstreamID, model: model, prefixHash: prefixHash, window: window}
	r.mu.Lock()
	if entry, ok := r.prefix[key]; ok && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.value, entry.err
	}
	r.mu.Unlock()
	value, err := r.store.GetPrefixCacheStats(apiKeyHash, upstreamID, model, prefixHash, protocol, window, now)
	r.mu.Lock()
	if r.prefix == nil {
		r.prefix = make(map[prefixCacheKey]prefixCacheEntry)
	}
	if len(r.prefix) >= maxPrefixCache {
		r.prefix = make(map[prefixCacheKey]prefixCacheEntry)
	}
	r.prefix[key] = prefixCacheEntry{expires: now.Add(routingCacheTTL), value: value, err: err}
	r.mu.Unlock()
	return value, err
}
