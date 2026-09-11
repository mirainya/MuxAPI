package routing

import "time"

// SessionCacheState represents the lifecycle of a session's cache on one upstream.
type SessionCacheState int

const (
	CacheCold    SessionCacheState = iota // never cached or fully expired with no recent activity
	CacheWarming                          // cache created but no hit observed yet
	CacheHot                              // recent cache hit, entry is valid
	CacheExpired                          // entry past TTL
)

func (s SessionCacheState) String() string {
	switch s {
	case CacheCold:
		return "COLD"
	case CacheWarming:
		return "WARMING"
	case CacheHot:
		return "HOT"
	case CacheExpired:
		return "EXPIRED"
	default:
		return "UNKNOWN"
	}
}

const defaultPriorHitRate = 0.85

// Bayesian prior pseudo-observations. The prior hit rate is modeled as a
// Beta(priorAlpha, priorBeta) distribution. With strength=5 and rate=0.85,
// alpha=4.25 and beta=0.75. A single observed miss moves the posterior mean
// from 0.85 to ~0.71 instead of the naïve 0.0.
const (
	priorStrength = 5.0
	priorAlpha    = defaultPriorHitRate * priorStrength       // 4.25
	priorBeta     = (1 - defaultPriorHitRate) * priorStrength // 0.75
)

// CacheStateParams holds the raw observations needed to determine cache state.
// All timestamps are Unix seconds (0 = unknown). The caller fetches these from
// the store layer; this function is a pure deterministic transform.
type CacheStateParams struct {
	Supported          bool
	CacheMode          string // "enabled" / "auto" / "disabled"
	Observations       int64
	HitCount           int64
	MissCount          int64
	CreateCount        int64
	WindowCreateCount  int64
	WindowCreateTokens int64
	PrefixTokens       int64
	ExpiresAt          int64 // unix seconds, 0 = unknown
	FirstSeenAt        int64 // unix seconds
	LastHitAt          int64 // unix seconds
	CoverageRatio      float64
	Protocol           string // "gemini" forces 1h TTL
}

// SessionCache is the resolved cache state for one session on one upstream.
type SessionCache struct {
	State                  SessionCacheState
	ExpiresAt              time.Time
	HitRate                float64
	HitRateSource          HitRateSource
	CoverageRatio          float64
	CreateCount            int64
	CreateRate             float64
	CreateTokensPerRequest float64
	CacheWriteObserved     bool
	PrefixTokens           int64
	PreferredTTL           time.Duration
}

// ResolveSessionCache determines the cache state from raw observations.
func ResolveSessionCache(params CacheStateParams, now time.Time) SessionCache {
	sc := SessionCache{
		CoverageRatio:      params.CoverageRatio,
		CreateCount:        params.CreateCount,
		CacheWriteObserved: params.Observations > 0 && (params.HitCount+params.MissCount > 0),
		PrefixTokens:       params.PrefixTokens,
	}
	eligible := params.HitCount + params.MissCount
	if eligible > 0 {
		sc.CreateRate = float64(params.WindowCreateCount) / float64(eligible)
		sc.CreateTokensPerRequest = float64(params.WindowCreateTokens) / float64(eligible)
		if sc.CreateRate < 0 {
			sc.CreateRate = 0
		}
		if sc.CreateRate > 1 {
			sc.CreateRate = 1
		}
	}

	if !params.Supported {
		sc.State = CacheCold
		sc.HitRateSource = HitRateUnknown
		return sc
	}

	sc.PreferredTTL = selectAdaptiveTTL(params, now)

	// Determine hit rate using Bayesian posterior (Beta-Binomial model).
	// Prior Beta(4.25, 0.75) encodes the belief that caching generally works.
	// Observed hits/misses update the posterior; the mean converges to the
	// true rate as observations accumulate, while a single miss no longer
	// collapses the estimate to zero.
	if params.Observations > 0 && (params.HitCount > 0 || params.MissCount > 0) {
		hits := float64(params.HitCount)
		misses := float64(params.MissCount)
		sc.HitRate = (priorAlpha + hits) / (priorAlpha + priorBeta + hits + misses)
		sc.HitRateSource = HitRateObserved
	} else if params.CacheMode == "enabled" || (params.Supported && params.Observations == 0) {
		sc.HitRate = defaultPriorHitRate
		sc.HitRateSource = HitRatePrior
	} else {
		sc.HitRateSource = HitRateUnknown
	}

	// Determine expiry
	if params.ExpiresAt > 0 {
		sc.ExpiresAt = time.Unix(params.ExpiresAt, 0)
	}

	// Determine state
	switch {
	case params.Observations == 0:
		sc.State = CacheCold
	case params.ExpiresAt > 0 && params.ExpiresAt <= now.Unix():
		sc.State = CacheExpired
	case params.HitCount == 0 && params.CreateCount > 0 && params.ExpiresAt > now.Unix():
		sc.State = CacheWarming
	case params.ExpiresAt > now.Unix():
		sc.State = CacheHot
	case params.HitCount > 0 || params.CreateCount > 0:
		sc.State = CacheExpired
	default:
		sc.State = CacheCold
	}

	return sc
}

// selectAdaptiveTTL picks 5min or 1h based on session behavior.
func selectAdaptiveTTL(params CacheStateParams, now time.Time) time.Duration {
	const defaultTTL = 5 * time.Minute
	const extendedTTL = time.Hour

	if NormalizeProtocol(params.Protocol) == "gemini" {
		return extendedTTL
	}

	if params.FirstSeenAt <= 0 || params.Observations <= 0 {
		return defaultTTL
	}

	sessionDuration := time.Duration(now.Unix()-params.FirstSeenAt) * time.Second

	if sessionDuration > 10*time.Minute && params.CreateCount >= 2 {
		return extendedTTL
	}

	avgInterval := sessionDuration / time.Duration(params.Observations)
	if avgInterval > 4*time.Minute && params.CreateCount >= 1 {
		return extendedTTL
	}

	return defaultTTL
}

// ToCacheProfile converts the resolved session cache to the CacheProfile
// consumed by the cost model. This is the single point where state-machine
// semantics translate to cost-model inputs.
func (sc SessionCache) ToCacheProfile() CacheProfile {
	if !sc.isSupported() {
		return CacheProfile{}
	}

	profile := CacheProfile{
		Supported:              true,
		TTL:                    sc.PreferredTTL,
		MinTokens:              1024,
		HitRate:                sc.HitRate,
		HitRateSource:          sc.HitRateSource,
		CoverageRatio:          sc.CoverageRatio,
		PreferredTTL:           sc.PreferredTTL,
		CreateRate:             sc.CreateRate,
		CreateTokensPerRequest: sc.CreateTokensPerRequest,
		CacheWriteObserved:     sc.CacheWriteObserved,
	}

	switch sc.State {
	case CacheHot, CacheWarming:
		profile.Existing = CacheEntry{
			Valid:        true,
			PrefixTokens: sc.PrefixTokens,
			ExpiresAt:    sc.ExpiresAt,
		}
	case CacheExpired, CacheCold:
		// When using a prior hit rate (never routed), assume the cache entry
		// already exists. This prevents cold channels from being permanently
		// penalized by a guaranteed first-miss write cost that hot channels
		// have already amortized — making the comparison fair at steady state.
		if sc.HitRateSource == HitRatePrior {
			profile.Existing = CacheEntry{Valid: true, PrefixTokens: sc.PrefixTokens}
		} else {
			profile.Existing = CacheEntry{Valid: false}
		}
	}

	return profile
}

func (sc SessionCache) isSupported() bool {
	return sc.PreferredTTL > 0 || sc.HitRateSource != HitRateUnknown
}
