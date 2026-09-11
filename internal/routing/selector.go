package routing

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ErrNoCandidate means every candidate was unavailable, incompatible, outside
// a hard latency limit, or lacked sufficient pricing information.
var ErrNoCandidate = errors.New("no eligible intelligent-routing candidate")

// Selector is a reusable stateless wrapper around Choose. The wrapper exists
// for request handlers that keep routing configuration on a long-lived server;
// Choose itself remains available for one-off previews and tests.
type Selector struct {
	Config Config
}

// NewSelector returns a selector with the supplied cost/latency policy.
func NewSelector(config Config) *Selector {
	return &Selector{Config: config.normalized()}
}

// Choose applies the selector's default config when the request does not
// provide one. A non-zero Request.Config always wins, allowing per-group
// overrides without allocating another selector.
func (s *Selector) Choose(request Request) (Decision, error) {
	if s != nil && request.Config == (Config{}) {
		request.Config = s.Config
	}
	return Choose(request)
}

// Pick returns the selected candidate itself in addition to the full decision
// audit. It is convenient for forwarding code that already has a candidate
// slice and needs the route object after selection.
func (s *Selector) Pick(request Request) (Candidate, Decision, error) {
	decision, err := s.Choose(request)
	if err != nil {
		return Candidate{}, decision, err
	}
	for _, candidate := range request.Candidates {
		if candidate.ID == decision.SelectedID {
			return candidate, decision, nil
		}
	}
	return Candidate{}, decision, ErrNoCandidate
}

// Request is one selector invocation. Forecast may come from Tracker.Forecast
// or a durable aggregate. When it is empty, the current request is treated as
// a one-request window.
type Request struct {
	Features   RequestFeatures `json:"features"`
	Forecast   TrafficForecast `json:"forecast"`
	Candidates []Candidate     `json:"candidates"`
	Config     Config          `json:"config"`
	Now        time.Time       `json:"now,omitempty"`
}

// CandidateEvaluation explains why one candidate was selected or rejected.
type CandidateEvaluation struct {
	CandidateID          int64        `json:"candidate_id"`
	CandidateName        string       `json:"candidate_name"`
	Protocol             string       `json:"protocol,omitempty"`
	Eligible             bool         `json:"eligible"`
	RejectReason         string       `json:"reject_reason,omitempty"`
	Cost                 CostEstimate `json:"cost"`
	EffectiveCost        float64      `json:"effective_cost"`
	LatencyScore         float64      `json:"latency_score"`
	ReliabilityScore     float64      `json:"reliability_score"`
	P95TTFTMs            float64      `json:"p95_ttft_ms"`
	P95DurationMs        float64      `json:"p95_duration_ms"`
	SuccessRate          float64      `json:"success_rate"`
	EffectiveSuccessRate float64      `json:"effective_success_rate"`
	Price                Pricing      `json:"price"`
	PricingSource        string       `json:"pricing_source,omitempty"`
	CacheExisting        bool         `json:"cache_existing"`
	CacheHitRate         float64      `json:"cache_hit_rate"`
	TieScore             float64      `json:"tie_score"`
	Priority             int          `json:"priority"`
	Samples              int64        `json:"samples"`
	Explanation          string       `json:"explanation,omitempty"`
}

// Decision contains the winner and every candidate's auditable evaluation.
// Evaluations retain caller order to make them easy to correlate with a
// scheduler's candidate list.
type Decision struct {
	SelectedID       int64                 `json:"selected_id"`
	SelectedName     string                `json:"selected_name"`
	Reason           string                `json:"reason"`
	Forecast         TrafficForecast       `json:"forecast"`
	Cost             CostEstimate          `json:"cost"`
	EffectiveCost    float64               `json:"effective_cost"`
	RunnerUpID       int64                 `json:"runner_up_id,omitempty"`
	EstimatedSavings float64               `json:"estimated_savings,omitempty"`
	Confidence       float64               `json:"confidence"`
	Exploration      bool                  `json:"exploration,omitempty"`
	CacheProfile     CacheProfile          `json:"cache_profile,omitempty"`
	Evaluations      []CandidateEvaluation `json:"evaluations"`
}

// Choose selects the candidate with the lowest risk-adjusted expected cost.
// Successful-history latency and reliability only reorder candidates whose
// costs are within Config.CostTieTolerance of the minimum. Priority is the
// final tie-breaker, so the selector is global rather than strict-tiered.
func Choose(request Request) (Decision, error) {
	cfg := request.Config.normalized()
	features := request.Features.Normalize()
	now := request.Now
	if now.IsZero() {
		now = time.Now()
	}
	forecast := request.Forecast.normalized(features, cfg.Window)
	decision := Decision{Forecast: forecast, Evaluations: make([]CandidateEvaluation, len(request.Candidates))}
	eligible := make([]int, 0, len(request.Candidates))
	for i, candidate := range request.Candidates {
		performance := candidate.Performance.normalized()
		evaluation := CandidateEvaluation{
			CandidateID: candidate.ID, CandidateName: candidate.Name, Protocol: candidate.Protocol,
			Priority: candidate.Priority, Samples: performance.Samples,
			P95TTFTMs: performance.P95TTFTMs, P95DurationMs: performance.P95DurationMs,
			SuccessRate: performance.SuccessRate, Price: candidate.Price.Normalized(), PricingSource: candidate.Price.Source,
			CacheExisting: candidate.Cache.Existing.Valid, CacheHitRate: candidate.Cache.HitRate,
		}
		switch {
		case !candidate.Healthy:
			evaluation.RejectReason = nonEmpty(candidate.LastError, "upstream is unhealthy")
		case !candidate.SupportsModel:
			evaluation.RejectReason = "model is not supported"
		case cfg.MaxTTFTMs > 0 && performance.Samples > 0 && performance.P95TTFTMs > cfg.MaxTTFTMs:
			evaluation.RejectReason = fmt.Sprintf("p95 TTFT %.0fms exceeds limit %.0fms", performance.P95TTFTMs, cfg.MaxTTFTMs)
		case cfg.MaxDurationMs > 0 && performance.Samples > 0 && performance.P95DurationMs > cfg.MaxDurationMs:
			evaluation.RejectReason = fmt.Sprintf("p95 duration %.0fms exceeds limit %.0fms", performance.P95DurationMs, cfg.MaxDurationMs)
		default:
			evaluation.Cost = EstimateWindowCost(features, forecast, candidate.Price, candidate.Cache, now, cfg.Window)
			if !evaluation.Cost.PricingComplete && !cfg.AllowUnknownPrice {
				evaluation.RejectReason = strings.Join(evaluation.Cost.Warnings, "; ")
				if evaluation.RejectReason == "" {
					evaluation.RejectReason = "pricing is incomplete"
				}
			} else {
				evaluation.Eligible = true
				evaluation.EffectiveCost = evaluation.Cost.SelectedTotal
				// Expected spend per successful completion captures retry costs.
				// Before MinSamples, use one virtual success: a recovering channel
				// remains selectable, but each observed failure immediately raises
				// its expected cost instead of looking identical to an unseen one.
				if performance.Samples > 0 {
					effectiveSuccessRate := performance.SuccessRate
					if performance.Samples < cfg.MinSamples {
						effectiveSuccessRate = (performance.SuccessRate*float64(performance.Samples) + 1) /
							(float64(performance.Samples) + 1)
					}
					evaluation.EffectiveSuccessRate = effectiveSuccessRate
					if effectiveSuccessRate <= 0 {
						evaluation.Eligible = false
						evaluation.RejectReason = "observed success rate is zero"
					} else {
						evaluation.EffectiveCost /= effectiveSuccessRate
					}
				}
			}
		}
		if evaluation.Eligible {
			eligible = append(eligible, i)
		}
		decision.Evaluations[i] = evaluation
	}
	if len(eligible) == 0 {
		return decision, ErrNoCandidate
	}

	minCost := math.Inf(1)
	for _, index := range eligible {
		if value := decision.Evaluations[index].EffectiveCost; value < minCost {
			minCost = value
		}
	}
	tieLimit := minCost * (1 + cfg.CostTieTolerance)
	if minCost == 0 {
		tieLimit = 1e-12
	}
	shortlist := make([]int, 0, len(eligible))
	for _, index := range eligible {
		if decision.Evaluations[index].EffectiveCost <= tieLimit+1e-15 {
			shortlist = append(shortlist, index)
		}
	}
	assignTieScores(decision.Evaluations, shortlist, request.Candidates, cfg)
	sort.SliceStable(shortlist, func(i, j int) bool {
		a := decision.Evaluations[shortlist[i]]
		b := decision.Evaluations[shortlist[j]]
		if !almostEqual(a.TieScore, b.TieScore) {
			return a.TieScore < b.TieScore
		}
		if !almostEqual(a.EffectiveCost, b.EffectiveCost) {
			return a.EffectiveCost < b.EffectiveCost
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.CandidateID < b.CandidateID
	})
	winnerIndex := shortlist[0]
	if explored, ok := chooseExploration(request, cfg, decision.Evaluations, eligible, winnerIndex, now); ok {
		winnerIndex = explored
		decision.Exploration = true
	}
	winner := request.Candidates[winnerIndex]
	winnerEval := decision.Evaluations[winnerIndex]
	decision.SelectedID = winner.ID
	decision.SelectedName = winner.Name
	decision.Cost = winnerEval.Cost
	decision.EffectiveCost = winnerEval.EffectiveCost
	decision.Confidence = winnerEval.Cost.Confidence
	decision.CacheProfile = winner.Cache

	allByCost := append([]int(nil), eligible...)
	sort.SliceStable(allByCost, func(i, j int) bool {
		return decision.Evaluations[allByCost[i]].EffectiveCost < decision.Evaluations[allByCost[j]].EffectiveCost
	})
	for _, index := range allByCost {
		if index == winnerIndex {
			continue
		}
		decision.RunnerUpID = request.Candidates[index].ID
		decision.EstimatedSavings = decision.Evaluations[index].EffectiveCost - winnerEval.EffectiveCost
		if decision.EstimatedSavings < 0 {
			decision.EstimatedSavings = 0
		}
		break
	}
	decision.Reason = selectionReason(winnerEval, decision, len(shortlist) > 1)
	if decision.Exploration {
		decision.Reason = "exploration sample: " + decision.Reason
	}
	for i := range decision.Evaluations {
		evaluation := &decision.Evaluations[i]
		if evaluation.Eligible {
			evaluation.Explanation = FormatCostReason(evaluation.Cost)
			if i == winnerIndex {
				evaluation.Explanation = "selected: " + decision.Reason
			}
		}
	}
	return decision, nil
}

// chooseExploration uses a stable time-bucketed hash instead of process-global
// randomness. This keeps request tests and audit replay deterministic while
// still sending a small, bounded sample to less-observed eligible channels.
// Only explores candidates within CostTieTolerance of the runner-up to avoid
// wasting money on channels that are obviously more expensive.
func chooseExploration(request Request, cfg Config, evaluations []CandidateEvaluation, eligible []int, winner int, now time.Time) (int, bool) {
	if request.Now.IsZero() || cfg.ExplorationRate <= 0 || len(eligible) < 2 {
		return 0, false
	}
	bucketWindow := cfg.Window
	if bucketWindow <= 0 {
		bucketWindow = 15 * time.Minute
	}
	bucket := now.UnixNano() / bucketWindow.Nanoseconds()
	hash := sha256.Sum256([]byte(request.Features.CacheKey + "\x00" + request.Features.Model + "\x00" + fmt.Sprint(bucket)))
	// Rate >= 1 must always fire. float64 rounds ^uint64(0) up to 2^64, and
	// the back-cast to uint64 is implementation-defined (on x86 it lands on
	// 2^63), silently degrading rate=1.0 to ~50% fire rate.
	var threshold uint64
	if cfg.ExplorationRate >= 1 {
		threshold = ^uint64(0)
	} else {
		threshold = uint64(cfg.ExplorationRate * float64(^uint64(0)))
	}
	if binary.BigEndian.Uint64(hash[:8]) > threshold {
		return 0, false
	}
	// Only explore among candidates that are at most 2x the winner's forecast
	// cost. The evaluation's EffectiveCost is the real per-decision price
	// (SelectedTotal, possibly divided by success rate); Price.Multiplier is
	// only a billing scalar and is typically ~1 across all channels, so it
	// filters nothing in practice.
	winnerCost := evaluations[winner].EffectiveCost
	if math.IsNaN(winnerCost) || math.IsInf(winnerCost, 0) || winnerCost < 0 {
		winnerCost = 0
	}
	// When winnerCost == 0 (e.g. the forecast reports zero requests or zero
	// tokens), the 2x ceiling collapses to 0 and would filter every candidate.
	// Fall back to a permissive ceiling in that case so exploration can still
	// probe cold candidates during early warm-up.
	costCeiling := winnerCost * 2
	unbounded := winnerCost == 0
	best := -1
	for _, index := range eligible {
		if index == winner {
			continue
		}
		cost := evaluations[index].EffectiveCost
		if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
			continue
		}
		if !unbounded && cost > costCeiling {
			continue
		}
		candidate := request.Candidates[index]
		if best < 0 || candidate.Performance.Samples < request.Candidates[best].Performance.Samples ||
			(candidate.Performance.Samples == request.Candidates[best].Performance.Samples && candidate.ID < request.Candidates[best].ID) {
			best = index
		}
	}
	return best, best >= 0
}

func assignTieScores(evaluations []CandidateEvaluation, indexes []int, candidates []Candidate, cfg Config) {
	maxLatency := float64(0)
	for _, index := range indexes {
		p := candidates[index].Performance.normalized()
		latency := p.P95TTFTMs
		if latency <= 0 {
			latency = p.P95DurationMs
		}
		if latency > maxLatency {
			maxLatency = latency
		}
	}
	for _, index := range indexes {
		p := candidates[index].Performance.normalized()
		latency := p.P95TTFTMs
		if latency <= 0 {
			latency = p.P95DurationMs
		}
		latencyScore := float64(0.5)
		if maxLatency > 0 && latency > 0 {
			latencyScore = latency / maxLatency
		}
		reliabilityScore := float64(0.5)
		if p.Samples > 0 {
			reliabilityScore = 1 - p.SuccessRate
		}
		evaluations[index].LatencyScore = latencyScore
		evaluations[index].ReliabilityScore = reliabilityScore
		evaluations[index].TieScore = latencyScore*cfg.LatencyWeight + reliabilityScore*cfg.ReliabilityWeight + float64(p.InFlight)*0.01
	}
}

func selectionReason(winner CandidateEvaluation, decision Decision, tieBroken bool) string {
	strategy := "ordinary input"
	if winner.Cost.CacheUsed {
		strategy = "provider cache"
	}
	base := fmt.Sprintf("lowest forecast cost via %s: %.8g over %s", strategy, winner.EffectiveCost, winner.Cost.Window)
	if tieBroken {
		base += "; near-equal costs resolved by latency and reliability"
	}
	if decision.RunnerUpID != 0 && decision.EstimatedSavings > 0 {
		base += fmt.Sprintf("; saves %.8g versus runner-up", decision.EstimatedSavings)
	}
	return base
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-12*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}
