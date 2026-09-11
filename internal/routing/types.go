// Package routing contains the protocol-independent cost model used by the
// intelligent upstream selector.  It deliberately has no dependency on the
// HTTP forwarding or persistence layers so it can be used in shadow tools,
// tests, and the request hot path alike.
package routing

import (
	"math"
	"strings"
	"time"
)

// RequestFeatures describes the part of a request that affects upstream cost.
// Token fields are estimates until the provider returns an authoritative usage
// object.  ReusableInputTokens is the stable prefix that a provider can cache;
// it must not include the request's newly appended user turn.
type RequestFeatures struct {
	Model                 string  `json:"model"`
	Protocol              string  `json:"protocol"`
	SessionID             string  `json:"session_id,omitempty"`
	CacheKey              string  `json:"cache_key,omitempty"`
	CacheScope            string  `json:"cache_scope,omitempty"`
	InputTokens           int64   `json:"input_tokens"`
	ReusableInputTokens   int64   `json:"reusable_input_tokens"`
	EstimatedOutputTokens int64   `json:"estimated_output_tokens"`
	MaxOutputTokens       int64   `json:"max_output_tokens,omitempty"`
	MessageCount          int     `json:"message_count,omitempty"`
	ToolCallCount         int     `json:"tool_call_count,omitempty"`
	CodeRatio             float64 `json:"code_ratio,omitempty"`
	ComplexityScore       float64 `json:"complexity_score,omitempty"`
	ReasoningEffort       string  `json:"reasoning_effort,omitempty"`
	Stream                bool    `json:"stream,omitempty"`
}

// Normalize clamps token counts and ratios while retaining the model and
// protocol labels.  Callers can safely pass user supplied estimates through
// this function before invoking the cost model.
func (f RequestFeatures) Normalize() RequestFeatures {
	if f.InputTokens < 0 {
		f.InputTokens = 0
	}
	if f.ReusableInputTokens < 0 {
		f.ReusableInputTokens = 0
	}
	if f.ReusableInputTokens > f.InputTokens {
		f.ReusableInputTokens = f.InputTokens
	}
	if f.EstimatedOutputTokens < 0 {
		f.EstimatedOutputTokens = 0
	}
	if f.MaxOutputTokens < 0 {
		f.MaxOutputTokens = 0
	}
	if f.EstimatedOutputTokens == 0 && f.MaxOutputTokens > 0 {
		f.EstimatedOutputTokens = f.MaxOutputTokens
	}
	if f.CodeRatio < 0 {
		f.CodeRatio = 0
	}
	if f.CodeRatio > 1 {
		f.CodeRatio = 1
	}
	if f.ComplexityScore < 0 || math.IsNaN(f.ComplexityScore) || math.IsInf(f.ComplexityScore, 0) {
		f.ComplexityScore = 0
	}
	if f.ComplexityScore > 1 {
		f.ComplexityScore = 1
	}
	f.Model = strings.TrimSpace(f.Model)
	f.Protocol = NormalizeProtocol(f.Protocol)
	return f
}

// NormalizeProtocol maps common aliases to the protocol names understood by
// the feature extractor. Unknown protocols are retained as lower-case values
// and are treated as generic OpenAI-shaped JSON where possible.
func NormalizeProtocol(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "openai", "chat", "chat_completions", "chat-completions":
		if strings.TrimSpace(value) == "" {
			return "openai"
		}
		return "openai"
	case "responses", "openai-responses", "openai_responses", "codex":
		return "responses"
	case "claude", "anthropic", "messages", "anthropic-messages":
		return "claude"
	case "gemini", "google", "generativelanguage", "generatecontent":
		return "gemini"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

// Pricing stores prices in currency units per token.  A nil/zero price is
// valid (for example a free cache read), while Known flags distinguish an
// actual zero from a price that has not been discovered yet.
type Pricing struct {
	InputPerToken      float64 `json:"input_per_token"`
	OutputPerToken     float64 `json:"output_per_token"`
	CacheReadPerToken  float64 `json:"cache_read_per_token"`
	CacheWritePerToken float64 `json:"cache_write_per_token"`
	InputKnown         bool    `json:"input_known"`
	OutputKnown        bool    `json:"output_known"`
	CacheReadKnown     bool    `json:"cache_read_known"`
	CacheWriteKnown    bool    `json:"cache_write_known"`
	Multiplier         float64 `json:"multiplier"`
	Source             string  `json:"source,omitempty"`
	Confidence         float64 `json:"confidence,omitempty"`
}

// Normalized returns a copy with finite non-negative rates and a default
// multiplier of one.  Negative/NaN values are never allowed to influence a
// routing decision.
func (p Pricing) Normalized() Pricing {
	clean := func(value float64) float64 {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0
		}
		return value
	}
	p.InputPerToken = clean(p.InputPerToken)
	p.OutputPerToken = clean(p.OutputPerToken)
	p.CacheReadPerToken = clean(p.CacheReadPerToken)
	p.CacheWritePerToken = clean(p.CacheWritePerToken)
	if p.Multiplier <= 0 || math.IsNaN(p.Multiplier) || math.IsInf(p.Multiplier, 0) {
		p.Multiplier = 1
	}
	if p.Confidence < 0 || math.IsNaN(p.Confidence) || math.IsInf(p.Confidence, 0) {
		p.Confidence = 0
	}
	if p.Confidence > 1 {
		p.Confidence = 1
	}
	return p
}

// Complete reports whether the ordinary input/output prices are known. Cache
// prices are required only when a candidate advertises cache support.
func (p Pricing) Complete(cacheRequired bool) bool {
	if !p.InputKnown || !p.OutputKnown {
		return false
	}
	return !cacheRequired || (p.CacheReadKnown && p.CacheWriteKnown)
}

// HitRateSource indicates the provenance of the CacheProfile.HitRate value.
type HitRateSource string

const (
	HitRateObserved HitRateSource = "observed"
	HitRatePrior    HitRateSource = "prior"
	HitRateUnknown  HitRateSource = "unknown"
)

// CacheEntry describes the cache state for this exact API-key/model/prefix
// tuple.  ExpiresAt may be zero when the caller only knows that the entry is
// valid for the current request.
type CacheEntry struct {
	Valid        bool      `json:"valid"`
	PrefixTokens int64     `json:"prefix_tokens"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// CacheProfile describes provider cache behavior and observed hit rate.
type CacheProfile struct {
	Supported     bool          `json:"supported"`
	TTL           time.Duration `json:"ttl"`
	MinTokens     int64         `json:"min_tokens,omitempty"`
	HitRate       float64       `json:"hit_rate"`
	HitRateSource HitRateSource `json:"hit_rate_source"`
	// Cache reads and writes can happen in the same rolling conversation.
	CreateRate             float64 `json:"create_rate,omitempty"`
	CreateTokensPerRequest float64 `json:"create_tokens_per_request,omitempty"`
	CacheWriteObserved     bool    `json:"cache_write_observed,omitempty"`
	// CoverageRatio is the fraction of the reusable prefix that the upstream
	// actually caches. Some proxied upstreams only cache ~77% of the prefix.
	// 0 or 1 means assume full prefix is cached (the standard behavior).
	CoverageRatio float64    `json:"coverage_ratio,omitempty"`
	Existing      CacheEntry `json:"existing"`
	// PreferredTTL is the adaptive TTL selected by the scheduler based on
	// session behavior. When longer than the default 5min, the forwarder may
	// inject cache_control hints into the upstream request.
	PreferredTTL time.Duration `json:"preferred_ttl,omitempty"`
	// Required means this candidate's billing/cache policy cannot be disabled
	// for the request. If false, the estimate is free to use ordinary input
	// when a cache write would cost more than it can save in the window.
	Required bool `json:"required,omitempty"`
	// CacheWriteIncludesInput and CacheReadIncludesInput model providers that
	// charge the ordinary input rate in addition to their cache rate. Most
	// Anthropic/OpenAI style endpoints replace the ordinary rate, so both are
	// false by default.
	CacheWriteIncludesInput bool `json:"cache_write_includes_input,omitempty"`
	CacheReadIncludesInput  bool `json:"cache_read_includes_input,omitempty"`
	// InputInflation is the observed ratio of actual billed input tokens to the
	// router's estimated InputTokens. Upstreams that inject system prompts will
	// have inflation > 1.0. The cost model uses this to correct the suffix size
	// (InputTokens * inflation - ReusableInputTokens). 0 or 1 means no correction.
	InputInflation float64 `json:"input_inflation,omitempty"`
}

// TrafficForecast is the expected load over a decision window. Requests may be
// fractional because it is a forecast; zero means derive it from the rate, and
// if both are absent one current request is assumed.
type TrafficForecast struct {
	Window             time.Duration `json:"window"`
	Requests           float64       `json:"requests"`
	RequestsPerMinute  float64       `json:"requests_per_minute,omitempty"`
	OutputTokens       float64       `json:"output_tokens,omitempty"`
	OutputTokensPerReq float64       `json:"output_tokens_per_request,omitempty"`
}

func (f TrafficForecast) normalized(features RequestFeatures, defaultWindow time.Duration) TrafficForecast {
	if f.Window <= 0 {
		f.Window = defaultWindow
	}
	if f.Window <= 0 {
		f.Window = 15 * time.Minute
	}
	if f.Requests < 0 || math.IsNaN(f.Requests) || math.IsInf(f.Requests, 0) {
		f.Requests = 0
	}
	if f.RequestsPerMinute < 0 || math.IsNaN(f.RequestsPerMinute) || math.IsInf(f.RequestsPerMinute, 0) {
		f.RequestsPerMinute = 0
	}
	if f.Requests <= 0 && f.RequestsPerMinute > 0 {
		f.Requests = f.RequestsPerMinute * f.Window.Minutes()
	}
	// The forecast is evaluated while routing a real current request, so the
	// window can never contain fewer than one attempt.
	if f.Requests < 1 {
		f.Requests = 1
	}
	if f.OutputTokensPerReq <= 0 {
		if f.OutputTokens > 0 {
			f.OutputTokensPerReq = f.OutputTokens / f.Requests
		} else {
			f.OutputTokensPerReq = float64(features.EstimatedOutputTokens)
		}
	}
	if f.OutputTokensPerReq < 0 || math.IsNaN(f.OutputTokensPerReq) || math.IsInf(f.OutputTokensPerReq, 0) {
		f.OutputTokensPerReq = 0
	}
	return f
}

// Performance contains recent latency/reliability observations. Durations are
// in milliseconds to make JSON/API integration straightforward.
type Performance struct {
	Samples       int64   `json:"samples"`
	P50TTFTMs     float64 `json:"p50_ttft_ms"`
	P95TTFTMs     float64 `json:"p95_ttft_ms"`
	P95DurationMs float64 `json:"p95_duration_ms"`
	SuccessRate   float64 `json:"success_rate"`
	InFlight      int64   `json:"in_flight"`
}

func (p Performance) normalized() Performance {
	if p.SuccessRate < 0 || math.IsNaN(p.SuccessRate) || math.IsInf(p.SuccessRate, 0) {
		p.SuccessRate = 0
	}
	if p.SuccessRate > 1 {
		p.SuccessRate = 1
	}
	if p.Samples < 0 {
		p.Samples = 0
	}
	if p.P50TTFTMs < 0 || math.IsNaN(p.P50TTFTMs) || math.IsInf(p.P50TTFTMs, 0) {
		p.P50TTFTMs = 0
	}
	if p.P95TTFTMs < 0 || math.IsNaN(p.P95TTFTMs) || math.IsInf(p.P95TTFTMs, 0) {
		p.P95TTFTMs = 0
	}
	if p.P95DurationMs < 0 || math.IsNaN(p.P95DurationMs) || math.IsInf(p.P95DurationMs, 0) {
		p.P95DurationMs = 0
	}
	if p.InFlight < 0 {
		p.InFlight = 0
	}
	return p
}

// Candidate is one possible upstream route for a request.
type Candidate struct {
	ID            int64        `json:"id"`
	Name          string       `json:"name"`
	Protocol      string       `json:"protocol,omitempty"`
	Priority      int          `json:"priority,omitempty"`
	Weight        int          `json:"weight,omitempty"`
	Healthy       bool         `json:"healthy"`
	SupportsModel bool         `json:"supports_model"`
	Price         Pricing      `json:"price"`
	Cache         CacheProfile `json:"cache"`
	Performance   Performance  `json:"performance"`
	LastError     string       `json:"last_error,omitempty"`
}

// Config controls selection. Cost is the hard objective; latency and
// reliability are used to break near-equal cost ties. Unknown prices are
// rejected by default so a guessed zero price can never win accidentally.
type Config struct {
	Window            time.Duration `json:"window"`
	CostTieTolerance  float64       `json:"cost_tie_tolerance"`
	LatencyWeight     float64       `json:"latency_weight"`
	ReliabilityWeight float64       `json:"reliability_weight"`
	MaxTTFTMs         float64       `json:"max_ttft_ms,omitempty"`
	MaxDurationMs     float64       `json:"max_duration_ms,omitempty"`
	MinSamples        int64         `json:"min_samples,omitempty"`
	AllowUnknownPrice bool          `json:"allow_unknown_price,omitempty"`
	ExplorationRate   float64       `json:"exploration_rate,omitempty"`
}

// DefaultConfig uses a 15-minute prediction horizon and only lets latency or
// reliability override a cost difference within one percent.
func DefaultConfig() Config {
	return Config{
		Window:            15 * time.Minute,
		CostTieTolerance:  0.01,
		LatencyWeight:     0.25,
		ReliabilityWeight: 0.15,
		MinSamples:        20,
		ExplorationRate:   0.02,
	}
}

func (c Config) normalized() Config {
	if c.Window <= 0 {
		c.Window = DefaultConfig().Window
	}
	if c.CostTieTolerance < 0 || math.IsNaN(c.CostTieTolerance) || math.IsInf(c.CostTieTolerance, 0) {
		c.CostTieTolerance = 0
	}
	if c.LatencyWeight < 0 || math.IsNaN(c.LatencyWeight) || math.IsInf(c.LatencyWeight, 0) {
		c.LatencyWeight = 0
	}
	if c.ReliabilityWeight < 0 || math.IsNaN(c.ReliabilityWeight) || math.IsInf(c.ReliabilityWeight, 0) {
		c.ReliabilityWeight = 0
	}
	if c.MaxTTFTMs < 0 || math.IsNaN(c.MaxTTFTMs) || math.IsInf(c.MaxTTFTMs, 0) {
		c.MaxTTFTMs = 0
	}
	if c.MaxDurationMs < 0 || math.IsNaN(c.MaxDurationMs) || math.IsInf(c.MaxDurationMs, 0) {
		c.MaxDurationMs = 0
	}
	if c.MinSamples < 0 {
		c.MinSamples = 0
	}
	if c.ExplorationRate < 0 || math.IsNaN(c.ExplorationRate) || math.IsInf(c.ExplorationRate, 0) {
		c.ExplorationRate = 0
	}
	if c.ExplorationRate > 1 {
		c.ExplorationRate = 1
	}
	return c
}
