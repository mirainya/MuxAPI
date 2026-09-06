// Package forward 负责选取上游、协议转换、故障切换及响应转发。
// 它以"响应尚未写给客户端"为换源边界，避免流式响应重复输出。
package forward

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	healthpkg "github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/translate"
	"github.com/mirainya/muxapi/internal/upstream"
	"github.com/tidwall/sjson"
)

const defaultFirstResponseTimeout = 120 * time.Second

// Health 汇总转发层需要的熔断反馈、并发占用和模型能力接口。
type Health interface {
	Complete(lease healthpkg.Lease, result healthpkg.Result, latencyMs int64)
}

type modelUnsupportedHealth interface {
	CompleteModelUnsupported(lease healthpkg.Lease, latencyMs int64, status int, reason string)
}

// Picker 从指定分组选择一个未尝试且可用的上游。
type Picker interface {
	PickExcluding(groupID int64, model string, exclude map[int64]bool) (*upstream.Upstream, healthpkg.Lease, error)
}

// FeaturePicker is an optional extension implemented by the intelligent
// scheduler. Keeping it separate from Picker preserves compatibility with
// embedders and tests that provide a minimal picker.
type FeaturePicker interface {
	PickWithFeatures(groupID int64, model string, features routing.RequestFeatures, exclude map[int64]bool) (*upstream.Upstream, healthpkg.Lease, routing.Decision, error)
}

// ModelMapper resolves model names per-upstream and records outcomes for
// auto-learning. Implementations must be safe for concurrent use.
type ModelMapper interface {
	Resolve(upstreamID int64, model string) string
	RecordFailure(upstreamID int64, model string)
	RecordSuccess(upstreamID int64, model string)
}

// TTFTEstimator provides per-upstream latency estimates for adaptive timeout.
type TTFTEstimator interface {
	EstimateTTFT(upstreamID int64, model string) (p95Ms float64, samples int64)
}

// AdaptiveTimeoutPolicy controls how historical TTFT is converted into a
// per-request first-response deadline. The configured first-response timeout
// remains the hard ceiling.
type AdaptiveTimeoutPolicy struct {
	Enabled    bool
	Floor      time.Duration
	Multiplier float64
	MinSamples int64
	TokenStep  int64
	TokenBonus time.Duration
}

func DefaultAdaptiveTimeoutPolicy() AdaptiveTimeoutPolicy {
	return AdaptiveTimeoutPolicy{Enabled: true, Floor: 10 * time.Second, Multiplier: 2,
		MinSamples: 5, TokenStep: 50_000, TokenBonus: 5 * time.Second}
}

// Forwarder 协调一次客户端请求的选路、重试、协议转换和响应审计。
type Forwarder struct {
	picker               Picker
	health               Health
	modelAliases         map[string]string
	modelMapper          ModelMapper
	ttftEstimator        TTFTEstimator
	adaptivePolicy       func() AdaptiveTimeoutPolicy
	streamIdleTimeout    func() time.Duration
	maxAttempts          int
	maxAttemptsProvider  func() int
	firstResponseTimeout func() time.Duration
}

// New 创建转发器；maxRetries 实际表示单个请求最多尝试的上游数。
func New(p Picker, h Health, maxRetries int) *Forwarder {
	maxAttempts := maxRetries
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &Forwarder{picker: p, health: h, maxAttempts: maxAttempts}
}

// SetFirstResponseTimeout 设置动态首响应超时读取器，便于运行时修改配置。
func (f *Forwarder) SetFirstResponseTimeout(timeout func() time.Duration) {
	f.firstResponseTimeout = timeout
}

// SetModelAliases 配置模型别名映射，启动时设置一次。
func (f *Forwarder) SetModelAliases(aliases map[string]string) {
	f.modelAliases = aliases
}

// SetModelMapper installs the per-upstream model name resolver. When set,
// the forwarder translates model names before building upstream requests.
func (f *Forwarder) SetModelMapper(mapper ModelMapper) {
	f.modelMapper = mapper
}

// SetTTFTEstimator installs the per-upstream latency estimator for adaptive
// first-byte timeout. When set, each attempt uses a timeout derived from the
// upstream's historical P95 TTFT instead of the global fixed value.
func (f *Forwarder) SetTTFTEstimator(estimator TTFTEstimator) {
	f.ttftEstimator = estimator
}

func (f *Forwarder) SetAdaptiveTimeoutPolicyProvider(provider func() AdaptiveTimeoutPolicy) {
	f.adaptivePolicy = provider
}

func (f *Forwarder) SetStreamIdleTimeoutProvider(provider func() time.Duration) {
	f.streamIdleTimeout = provider
}

// SetMaxAttemptsProvider supplies the current per-request upstream attempt limit.
// The provider is evaluated once when a request starts, so a settings change
// cannot alter the retry budget halfway through an in-flight request.
func (f *Forwarder) SetMaxAttemptsProvider(provider func() int) {
	f.maxAttemptsProvider = provider
}

func (f *Forwarder) maxAttemptLimit() int {
	limit := f.maxAttempts
	if f.maxAttemptsProvider != nil {
		if value := f.maxAttemptsProvider(); value > 0 {
			limit = value
		}
	}
	if limit < 1 {
		return 1
	}
	return limit
}

func (f *Forwarder) firstByteTimeout() time.Duration {
	if f.firstResponseTimeout != nil {
		if timeout := f.firstResponseTimeout(); timeout > 0 {
			return timeout
		}
	}
	return defaultFirstResponseTimeout
}

// adaptiveTimeout returns a per-attempt timeout based on the upstream's
// historical P95 TTFT and the request's input token count. Falls back to
// the global firstByteTimeout when no estimator is configured or data is cold.
func (f *Forwarder) adaptiveTimeout(upstreamID int64, model string, inputTokens int64) time.Duration {
	policy := DefaultAdaptiveTimeoutPolicy()
	if f.adaptivePolicy != nil {
		policy = f.adaptivePolicy()
	}
	if !policy.Enabled || f.ttftEstimator == nil {
		return f.firstByteTimeout()
	}
	p95, samples := f.ttftEstimator.EstimateTTFT(upstreamID, model)
	return ComputeAdaptiveTimeout(AdaptiveTimeoutParams{
		P95Ms:                p95,
		Samples:              samples,
		InputTokens:          inputTokens,
		Multiplier:           policy.Multiplier,
		Floor:                policy.Floor,
		Ceiling:              f.firstByteTimeout(),
		MinSamples:           policy.MinSamples,
		TokenStep:            policy.TokenStep,
		TokenBonus:           policy.TokenBonus,
		TokenBonusConfigured: true,
	})
}

func (f *Forwarder) streamIdleDeadline() time.Duration {
	if f.streamIdleTimeout != nil {
		if value := f.streamIdleTimeout(); value > 0 {
			return value
		}
	}
	return 5 * time.Minute
}

// StatusClientClosedRequest 用于审计客户端提前断开；Go 标准库没有该常量。
const StatusClientClosedRequest = 499

// Outcome* 是请求审计使用的稳定结果分类。
const (
	OutcomeSuccess     = "success"
	OutcomeFailed      = "failed"
	OutcomeCanceled    = "canceled"
	OutcomePartial     = "partial"
	OutcomeClientError = "client_error"
	OutcomeUnsupported = "unsupported"
	OutcomeUnavailable = "unavailable"
)

// AttemptResult 记录一次上游尝试，完整请求可能包含多次尝试。
type AttemptResult struct {
	AttemptNo int
	// Protocol 快照本次尝试实际使用的渠道协议。费用比对靠它决定 cached_tokens
	// 的口径，事后现查会用改动后的协议解释历史用量。
	Protocol            string
	MappedModel         string
	UpstreamID          int64
	Priority            int
	SelectionReason     string
	HealthBefore        string
	HealthAfter         string
	Status              int
	Outcome             string
	TTFTMs              int64
	DurationMs          int64
	ResponseBytes       int64
	Stream              bool
	StreamCompleted     bool
	LastEvent           string
	InputTokens         int64
	OutputTokens        int64
	CachedTokens        int64
	CacheCreationTokens int64
	UpstreamRequestID   string
	ErrorKind           string
	ErrorSource         string
	CreatedAt           time.Time
	CompletedAt         time.Time
	Error               string
	UpstreamKeyHash     string
	RouteDecision       *routing.Decision
}

// Result 汇总最终响应及按时间排列的全部上游尝试。
type Result struct {
	Status              int
	Outcome             string
	FinalUpstreamID     int64
	TTFTMs              int64
	ResponseBytes       int64
	StreamCompleted     bool
	LastEvent           string
	InputTokens         int64
	OutputTokens        int64
	CachedTokens        int64
	CacheCreationTokens int64
	UpstreamRequestID   string
	ErrorKind           string
	ErrorSource         string
	Error               string
	Attempts            []AttemptResult
	RouteFeatures       routing.RequestFeatures
	RouteDecision       *routing.Decision
}

type attemptContext struct {
	number          int
	protocol        string
	mappedModel     string
	upstreamID      int64
	priority        int
	selectionReason string
	healthBefore    string
	keyHash         string
	routeDecision   *routing.Decision
	started         time.Time
}

type healthStateReporter interface {
	EffectiveState(id int64) string
}

func healthState(h Health, id int64) string {
	if reporter, ok := h.(healthStateReporter); ok {
		return reporter.EffectiveState(id)
	}
	return ""
}

func (a attemptContext) finish(h Health, status int, outcome string, relay relayResult, errorKind, errorSource, errText string) AttemptResult {
	completed := time.Now()
	return AttemptResult{
		AttemptNo: a.number, Protocol: a.protocol, MappedModel: a.mappedModel,
		UpstreamID: a.upstreamID, Priority: a.priority,
		SelectionReason: a.selectionReason, HealthBefore: a.healthBefore, HealthAfter: healthState(h, a.upstreamID),
		Status: status, Outcome: outcome, TTFTMs: relay.ttftMs, DurationMs: completed.Sub(a.started).Milliseconds(),
		ResponseBytes: relay.bytesSent, Stream: relay.stream, StreamCompleted: relay.streamCompleted,
		LastEvent: relay.lastEvent, InputTokens: relay.usage.input, OutputTokens: relay.usage.output,
		CachedTokens: relay.usage.cached, CacheCreationTokens: relay.usage.cacheCreation,
		UpstreamRequestID: relay.upstreamRequestID,
		ErrorKind:         errorKind, ErrorSource: errorSource,
		CreatedAt: a.started, CompletedAt: completed, Error: clipErr(errText),
		UpstreamKeyHash: a.keyHash, RouteDecision: a.routeDecision,
	}
}

func resultFromAttempt(attempt AttemptResult, attempts []AttemptResult) Result {
	return Result{
		Status: attempt.Status, Outcome: attempt.Outcome, FinalUpstreamID: attempt.UpstreamID,
		TTFTMs: attempt.TTFTMs, ResponseBytes: attempt.ResponseBytes,
		StreamCompleted: attempt.StreamCompleted, LastEvent: attempt.LastEvent,
		InputTokens: attempt.InputTokens, OutputTokens: attempt.OutputTokens, CachedTokens: attempt.CachedTokens,
		CacheCreationTokens: attempt.CacheCreationTokens,
		UpstreamRequestID:   attempt.UpstreamRequestID, ErrorKind: attempt.ErrorKind,
		ErrorSource: attempt.ErrorSource, Error: attempt.Error, Attempts: attempts,
		RouteDecision: attempt.RouteDecision,
	}
}

// Forward 执行一次请求。只有在响应尚未提交给客户端时，失败才会切换上游。
func (f *Forwarder) Forward(w http.ResponseWriter, r *http.Request, body []byte, groupID int64, keyName string) Result {
	model := translate.RequestModel(r.URL.Path, body)
	if canonical := f.canonicalizeModel(model); canonical != model {
		// 同步改写请求体：透传路径原样发送 body，翻译路径的 translator 也从
		// 归一化后的名字取值。请求审计记录的是客户端原始模型名。
		if rewritten, err := sjson.SetBytes(body, "model", canonical); err == nil {
			body = rewritten
		}
		model = canonical
	}
	streamRequested := translate.RequestStream(r.URL.Path, body)
	sourceFormat, sourceKnown := translate.SourceFromRequest(r.URL.Path, r.Header)
	if !sourceKnown {
		sourceFormat = translate.Passthrough
	}
	features, featureErr := routing.ExtractRequestFeatures(body, routing.FeatureOptions{
		Protocol: string(sourceFormat), Headers: r.Header,
	})
	if featureErr != nil {
		features = routing.RequestFeatures{Model: model, Protocol: string(sourceFormat), Stream: streamRequested}.Normalize()
	}
	if features.Model == "" {
		features.Model = model
	}
	if features.Protocol == "" {
		features.Protocol = string(sourceFormat)
	}
	// Native Gemini carries stream mode in the endpoint suffix, not the JSON
	// body; keep the routing feature in sync with the request boundary helper.
	features.Stream = streamRequested
	tried := map[int64]bool{}
	var lastErr error
	maxAttempts := f.maxAttemptLimit()
	attempts := make([]AttemptResult, 0, maxAttempts)
	var routeDecision *routing.Decision
	withRouting := func(result Result) Result {
		result.RouteFeatures = features
		result.RouteDecision = routeDecision
		return result
	}

	for upstreamAttempts := 0; upstreamAttempts < maxAttempts; {
		// tried 同时防止重复选择；本地协议不兼容不消耗实际网络尝试次数。
		var candidate *upstream.Upstream
		var lease healthpkg.Lease
		var decision routing.Decision
		var err error
		if picker, ok := f.picker.(FeaturePicker); ok {
			candidate, lease, decision, err = picker.PickWithFeatures(groupID, model, features, tried)
			if err == nil && routeDecision == nil && decision.SelectedID != 0 {
				copyDecision := decision
				routeDecision = &copyDecision
			}
		} else {
			candidate, lease, err = f.picker.PickExcluding(groupID, model, tried)
		}
		if err != nil {
			break
		}
		tried[candidate.ID] = true
		completed := false
		complete := func(result healthpkg.Result, latencyMs int64) {
			if completed {
				return
			}
			completed = true
			f.health.Complete(lease, result, latencyMs)
		}
		release := func() { complete(healthpkg.ResultNeutral, 0) }
		attemptStarted := time.Now()
		attemptNo := len(attempts) + 1
		selectionReason := "initial"
		if attemptNo > 1 {
			selectionReason = "failover"
		}
		beforeState := healthState(f.health, candidate.ID)
		if beforeState == "HALF_OPEN" {
			selectionReason = "recovery_trial"
		}
		if routeDecision != nil && attemptNo == 1 {
			selectionReason = routeDecision.Reason
		}
		attemptCtx := attemptContext{
			number: attemptNo, protocol: candidate.Protocol, upstreamID: candidate.ID,
			priority:        candidate.Priority,
			selectionReason: selectionReason, healthBefore: beforeState, started: attemptStarted,
			keyHash: hashUpstreamKey(candidate.APIKey), routeDecision: routeDecision,
		}

		// 每次换源都从原始客户端请求重新转换，不能复用上一上游的请求体。
		targetFormat, validProtocol := translate.NormalizeFormat(candidate.Protocol)
		if !validProtocol {
			release()
			err = errors.New("unsupported upstream protocol: " + candidate.Protocol)
			attempts = append(attempts, attemptCtx.finish(f.health, 0, OutcomeUnsupported, relayResult{}, "protocol_unsupported", "gateway", err.Error()))
			lastErr = err
			continue
		}
		// Resolve per-upstream model name translation. The original `model`
		// is used for health/scheduler tracking; `upstreamModel` goes to the
		// actual upstream request.
		upstreamModel := model
		if f.modelMapper != nil {
			upstreamModel = f.modelMapper.Resolve(candidate.ID, model)
		}
		if upstreamModel != model {
			attemptCtx.mappedModel = upstreamModel
		}
		exchange, err := translate.NewExchange(sourceFormat, targetFormat, upstreamModel, streamRequested, body)
		if err != nil {
			release()
			attempts = append(attempts, attemptCtx.finish(f.health, 0, OutcomeUnsupported, relayResult{}, "protocol_unsupported", "gateway", err.Error()))
			lastErr = err
			continue
		}
		// Inject cache_control hint for extended TTL on Claude protocol.
		// NOTE: Only inject when adaptive TTL selects 1h. For 5min TTL, the
		// upstream auto-detects cacheable prefixes without client hints.
		if routeDecision != nil && routeDecision.Cost.CacheUsed &&
			routeDecision.CacheProfile.PreferredTTL > 5*time.Minute &&
			targetFormat == translate.Claude {
			exchange.UpstreamRequest = injectClaudeCacheControl(exchange.UpstreamRequest, routeDecision.CacheProfile.PreferredTTL)
		}
		targetPath, err := translate.TargetPathForRequest(targetFormat, r.URL.Path, upstreamModel, exchange.UpstreamStream)
		if err != nil {
			release()
			attempts = append(attempts, attemptCtx.finish(f.health, 0, OutcomeUnsupported, relayResult{}, "protocol_unsupported", "gateway", err.Error()))
			lastErr = err
			continue
		}
		if query := translate.TargetQuery(sourceFormat, targetFormat, r.URL.Query(), exchange.UpstreamStream); query != "" {
			targetPath += "?" + query
		}
		upstreamAttempts++
		req, err := candidate.BuildRequest(r.Method, targetPath, bytes.NewReader(exchange.UpstreamRequest), r.Header)
		if err != nil {
			complete(healthpkg.ResultFailure, 0)
			attempts = append(attempts, attemptCtx.finish(f.health, 0, OutcomeFailed, relayResult{}, "request_build", "gateway", err.Error()))
			lastErr = err
			continue
		}
		translate.ConfigureRequestHeaders(req.Header, targetFormat, exchange.Translated())

		// 首个有效输出使用绝对超时；之后仅正文活动刷新流空闲超时。
		// SSE 心跳和空事件不能无限延长首个有效输出等待。
		ctx, cancel := context.WithCancelCause(r.Context())
		watchdog := newResponseWatchdog(f.adaptiveTimeout(candidate.ID, model, features.InputTokens), cancel)
		req = req.WithContext(ctx)
		client := &http.Client{Timeout: 0, Transport: candidate.NewTransport()}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			watchdog.stop()
			cause := context.Cause(ctx)
			cancel(nil)
			if r.Context().Err() != nil {
				release()
				finished := attemptCtx.finish(f.health, StatusClientClosedRequest, OutcomeCanceled, relayResult{}, "client_canceled", "client", r.Context().Err().Error())
				attempts = append(attempts, finished)
				return withRouting(resultFromAttempt(finished, attempts))
			}
			errorKind := "upstream_network"
			if errors.Is(cause, errFirstResponseTimeout) {
				errorKind = "first_response_timeout"
			}
			complete(healthpkg.ResultFailure, 0)
			attempts = append(attempts, attemptCtx.finish(f.health, 0, OutcomeFailed, relayResult{}, errorKind, "upstream", err.Error()))
			lastErr = err
			continue
		}
		markOutput := func() { watchdog.markOutput(f.streamIdleDeadline()) }

		// 400/404/503 需要先读取正文，以区分"不支持模型"和普通客户端/上游错误。
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusServiceUnavailable {
			payload, readErr := readLimitedBody(resp.Body, 2<<20, markOutput)
			watchdog.stop()
			cause := context.Cause(ctx)
			cancel(nil)
			resp.Body.Close()
			if readErr != nil {
				if r.Context().Err() != nil {
					release()
					finished := attemptCtx.finish(f.health, StatusClientClosedRequest, OutcomeCanceled, relayResult{}, "client_canceled", "client", r.Context().Err().Error())
					attempts = append(attempts, finished)
					return withRouting(resultFromAttempt(finished, attempts))
				}
				errorKind := "upstream_read"
				if errors.Is(cause, errFirstResponseTimeout) {
					errorKind = "first_response_timeout"
				}
				complete(healthpkg.ResultFailure, 0)
				attempts = append(attempts, attemptCtx.finish(f.health, 0, OutcomeFailed, relayResult{}, errorKind, "upstream", readErr.Error()))
				lastErr = readErr
				continue
			}
			responseMeta := relayResult{ttftMs: time.Since(start).Milliseconds(), upstreamRequestID: upstreamRequestID(resp.Header)}
			if upstream.IsModelUnsupported(resp.StatusCode, model, string(payload)) {
				if f.modelMapper != nil {
					f.modelMapper.RecordFailure(candidate.ID, model)
				}
				if !completed {
					completed = true
					if detailed, ok := f.health.(modelUnsupportedHealth); ok {
						detailed.CompleteModelUnsupported(lease, responseMeta.ttftMs, resp.StatusCode, string(payload))
					} else {
						f.health.Complete(lease, healthpkg.ResultModelUnsupported, responseMeta.ttftMs)
					}
				}
				attempts = append(attempts, attemptCtx.finish(f.health, resp.StatusCode, OutcomeUnsupported,
					responseMeta, "model_unsupported", "upstream", string(payload)))
				continue
			}

			// 503 that is NOT model-unsupported should fall through to normal
			// failure handling (breaker + failover), not be treated as client error.
			if resp.StatusCode == http.StatusServiceUnavailable {
				complete(healthpkg.ResultFailure, responseMeta.ttftMs)
				attempts = append(attempts, attemptCtx.finish(f.health, resp.StatusCode, OutcomeFailed,
					responseMeta, "upstream_error", "upstream", string(payload)))
				lastErr = errors.New(string(payload))
				continue
			}

			// 其他 4xx 描述客户端请求，不改变渠道健康状态，也不换源。
			clientPayload := payload
			if exchange.Translated() {
				clientPayload = translate.ErrorResponse(sourceFormat, resp.StatusCode, payload)
				resp.Header.Del("Content-Length")
				resp.Header.Del("Content-Encoding")
				resp.Header.Set("Content-Type", "application/json")
			}
			resp.Body = io.NopCloser(bytes.NewReader(clientPayload))
			result := relayResponse(w, resp, start, nil)
			release()
			if result.err != nil {
				finished := attemptCtx.finish(f.health, StatusClientClosedRequest, OutcomeCanceled,
					result, "downstream_write", "client", result.err.Error())
				attempts = append(attempts, finished)
				return withRouting(resultFromAttempt(finished, attempts))
			}
			finished := attemptCtx.finish(f.health, resp.StatusCode, OutcomeClientError,
				result, "client_request", "client", string(payload))
			attempts = append(attempts, finished)
			return withRouting(resultFromAttempt(finished, attempts))
		}

		// 认证、限流及 5xx 属于渠道失败：记录熔断反馈后尝试下一个上游。
		if upstream.IsFailureStatus(resp.StatusCode) {
			payload, _ := readLimitedBody(resp.Body, 64<<10, markOutput)
			watchdog.stop()
			resp.Body.Close()
			cancel(nil)
			latency := time.Since(start).Milliseconds()
			complete(healthpkg.ResultFailure, latency)
			responseMeta := relayResult{ttftMs: latency, upstreamRequestID: upstreamRequestID(resp.Header)}
			attempts = append(attempts, attemptCtx.finish(f.health, resp.StatusCode, OutcomeFailed,
				responseMeta, "upstream_http", "upstream", string(payload)))
			lastErr = errors.New(http.StatusText(resp.StatusCode))
			continue
		}

		// relayResult.committed 表示响应是否已写出；只有未写出时才能安全换源。
		result := relayTranslatedResponse(r.Context(), w, resp, start, markOutput, exchange)
		watchdog.stop()
		cause := context.Cause(ctx)
		cancel(nil)

		if result.err != nil {
			if result.source == relayDownstream || r.Context().Err() != nil {
				release()
				errText := result.err.Error()
				errorKind := "downstream_write"
				if r.Context().Err() != nil {
					errText = r.Context().Err().Error()
					errorKind = "client_canceled"
				}
				finished := attemptCtx.finish(f.health, StatusClientClosedRequest, OutcomeCanceled,
					result, errorKind, "client", errText)
				attempts = append(attempts, finished)
				return withRouting(resultFromAttempt(finished, attempts))
			}
			complete(healthpkg.ResultFailure, result.ttftMs)
			outcome := OutcomeFailed
			status := 0
			if result.committed {
				outcome = OutcomePartial
				status = resp.StatusCode
			}
			errorKind := relayErrorKind(result.err, cause)
			finished := attemptCtx.finish(f.health, status, outcome, result, errorKind, "upstream", result.err.Error())
			attempts = append(attempts, finished)
			lastErr = result.err
			if !result.committed {
				continue
			}
			return withRouting(resultFromAttempt(finished, attempts))
		}

		// 上游在流中投递了 error 事件：HTTP 状态码仍是 200，但这次调用其实失败了。
		// 响应已提交无法换源，但必须给熔断器报失败——否则总在半路报错的渠道
		// 会被记成健康渠道，熔断永不触发。
		// 注意：仅「缺少终止事件」不算失败，部分上游合法地不发终止标记。
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if result.streamErrored {
				complete(healthpkg.ResultFailure, result.ttftMs)
			} else {
				if f.modelMapper != nil {
					f.modelMapper.RecordSuccess(candidate.ID, model)
				}
				complete(healthpkg.ResultSuccess, result.ttftMs)
			}
		} else {
			release()
		}
		outcome := OutcomeSuccess
		if result.streamErrored {
			outcome = OutcomePartial
		}
		if resp.StatusCode >= 400 {
			outcome = OutcomeClientError
		}
		errorKind, errorSource := "", ""
		switch {
		case outcome == OutcomeClientError:
			errorKind, errorSource = "client_request", "client"
		case result.streamErrored:
			errorKind, errorSource = "stream_error", "upstream"
		}
		finished := attemptCtx.finish(f.health, resp.StatusCode, outcome, result, errorKind, errorSource, "")
		attempts = append(attempts, finished)
		return withRouting(resultFromAttempt(finished, attempts))
	}

	// 循环结束后统一生成网关错误，并保留最后一次尝试的审计信息。
	if len(tried) == 0 {
		http.Error(w, "no upstream available", http.StatusServiceUnavailable)
		return Result{Status: http.StatusServiceUnavailable, Outcome: OutcomeUnavailable,
			ErrorKind: "no_upstream", ErrorSource: "gateway", Error: "no upstream available", Attempts: attempts,
			RouteFeatures: features, RouteDecision: routeDecision}
	}
	if lastErr != nil {
		// 只回笼统信息：上游原文可能含渠道地址等内部细节，完整错误留在审计里。
		http.Error(w, "all upstreams failed", http.StatusBadGateway)
		if len(attempts) > 0 {
			final := attempts[len(attempts)-1]
			result := resultFromAttempt(final, attempts)
			result.Status = http.StatusBadGateway
			result.Outcome = OutcomeFailed
			result.Error = clipErr(lastErr.Error())
			result.RouteFeatures = features
			result.RouteDecision = routeDecision
			return result
		}
		return Result{Status: http.StatusBadGateway, Outcome: OutcomeFailed,
			ErrorKind: "upstream_error", ErrorSource: "upstream", Error: clipErr(lastErr.Error()), Attempts: attempts,
			RouteFeatures: features, RouteDecision: routeDecision}
	}
	http.Error(w, "all upstreams failed", http.StatusBadGateway)
	return Result{Status: http.StatusBadGateway, Outcome: OutcomeFailed,
		ErrorKind: "upstream_error", ErrorSource: "upstream", Error: "all upstreams failed", Attempts: attempts,
		RouteFeatures: features, RouteDecision: routeDecision}
}

func hashUpstreamKey(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func relayErrorKind(err error, cause error) string {
	switch {
	case errors.Is(cause, errFirstResponseTimeout):
		return "first_response_timeout"
	case errors.Is(cause, errStreamIdleTimeout):
		return "stream_idle_timeout"
	case errors.Is(err, errEmptyResponse):
		return "empty_response"
	case errors.Is(err, errErrorPayload):
		return "error_payload"
	default:
		return "upstream_disconnect"
	}
}

// clipErr 截断上游错误文本。必须按 rune 边界切并清洗非法字节：审计文本会写入
// Postgres 的 text 列，非法 UTF-8 会让整条 INSERT 失败、连带丢掉这次请求的审计。
func clipErr(value string) string {
	return clipUTF8(strings.TrimSpace(value), 500)
}

// clipUTF8 把字符串限制到 maxBytes 字节以内，且保证结果是合法 UTF-8。
// 先替换非法字节（上游可能返回二进制或截断的响应体），再按 rune 边界回退。
func clipUTF8(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func readLimitedBody(reader io.Reader, limit int64, onOutput func()) ([]byte, error) {
	return io.ReadAll(io.LimitReader(activityReader{Reader: reader, onOutput: onOutput}, limit))
}

var errEmptyResponse = errors.New("upstream returned an empty response")
var errErrorPayload = errors.New("upstream returned an error payload with a successful status")
var errFirstResponseTimeout = errors.New("upstream first response timeout")
var errStreamIdleTimeout = errors.New("upstream stream idle timeout")

// responseWatchdog applies an absolute deadline until the first meaningful
// output, then an idle deadline refreshed by later output.
type responseWatchdog struct {
	timeout  time.Duration
	cancel   context.CancelCauseFunc
	activity chan struct{}
	policy   chan watchdogReset
	done     chan struct{}
	stopped  chan struct{}
	once     sync.Once
	output   atomic.Bool
}

type watchdogReset struct {
	timeout time.Duration
	cause   error
}

func newResponseWatchdog(timeout time.Duration, cancel context.CancelCauseFunc) *responseWatchdog {
	w := &responseWatchdog{
		timeout: timeout, cancel: cancel,
		activity: make(chan struct{}, 1), policy: make(chan watchdogReset, 1),
		done: make(chan struct{}), stopped: make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *responseWatchdog) run() {
	timer := time.NewTimer(w.timeout)
	cause := error(errFirstResponseTimeout)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		close(w.stopped)
	}()
	for {
		select {
		case <-timer.C:
			w.cancel(cause)
			return
		case <-w.activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(w.timeout)
		case reset := <-w.policy:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			w.timeout = reset.timeout
			cause = reset.cause
			timer.Reset(w.timeout)
		case <-w.done:
			return
		}
	}
}

func (w *responseWatchdog) touch() {
	select {
	case w.activity <- struct{}{}:
	default:
	}
}

func (w *responseWatchdog) markOutput(idleTimeout time.Duration) {
	if w.output.CompareAndSwap(false, true) {
		w.setPolicy(idleTimeout, errStreamIdleTimeout)
		return
	}
	w.touch()
}

func (w *responseWatchdog) setPolicy(timeout time.Duration, cause error) {
	if timeout <= 0 || cause == nil {
		return
	}
	select {
	case w.policy <- watchdogReset{timeout: timeout, cause: cause}:
	case <-w.stopped:
	}
}

func (w *responseWatchdog) stop() {
	w.once.Do(func() { close(w.done) })
	<-w.stopped
}

type activityReader struct {
	io.Reader
	onOutput func()
}

func (r activityReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 && r.onOutput != nil {
		r.onOutput()
	}
	return n, err
}

type relaySource int

const (
	relayUpstream relaySource = iota
	relayDownstream
)

// relayResult 区分上游读取错误和客户端写入错误，并标记响应提交边界。
type relayResult struct {
	err               error
	source            relaySource
	ttftMs            int64
	committed         bool
	bytesSent         int64
	stream            bool
	streamCompleted   bool
	lastEvent         string
	streamErrored     bool
	usage             tokenUsage
	upstreamRequestID string
}

func relayResponse(w http.ResponseWriter, resp *http.Response, start time.Time, onOutput func()) relayResult {
	return relayTranslatedResponse(context.Background(), w, resp, start, onOutput, nil)
}

// relayTranslatedResponse 按"上游是否流式"和"客户端是否要求流式"选择转发方式。
// auditReadCloser 在同一次读取中旁路提取完成事件、Token 用量和请求 ID。
func relayTranslatedResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, start time.Time, onOutput func(), exchange *translate.Exchange) relayResult {
	contentType := resp.Header.Get("Content-Type")
	stream := strings.HasPrefix(contentType, "text/event-stream")
	audited := newAuditReadCloser(resp.Body, stream)
	resp.Body = audited
	defer resp.Body.Close()
	var result relayResult
	if stream && exchange != nil && exchange.Translated() {
		if exchange.Stream {
			result = relayTranslatedSSE(ctx, w, resp, start, onOutput, exchange)
		} else {
			result = relayTranslatedSSEToBody(ctx, w, resp, start, onOutput, exchange)
		}
	} else if stream {
		result = relaySSE(w, resp, start, onOutput)
	} else if exchange != nil && exchange.Translated() {
		result = relayTranslatedBody(ctx, w, resp, start, onOutput, exchange)
	} else {
		result = relayBody(w, resp, start, onOutput)
	}
	audited.audit.finish()
	result.stream = stream
	result.streamCompleted = audited.audit.streamCompleted
	result.streamErrored = audited.audit.streamErrored
	result.lastEvent = audited.audit.lastEvent
	result.usage = audited.audit.usage
	result.upstreamRequestID = upstreamRequestID(resp.Header)
	return result
}

// relayTranslatedSSE 逐行翻译 SSE；翻译器可能将一个上游事件展开为多个客户端事件。
func relayTranslatedSSE(ctx context.Context, w http.ResponseWriter, resp *http.Response, start time.Time, onOutput func(), exchange *translate.Exchange) relayResult {
	flusher, canFlush := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 52_428_800)
	committed := false
	ttft := int64(0)
	bytesSent := int64(0)
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		chunks, err := exchange.TranslateStream(ctx, line)
		if err != nil {
			return relayResult{err: err, source: relayUpstream, ttftMs: ttft, committed: committed, bytesSent: bytesSent}
		}
		for _, chunk := range chunks {
			chunk = frameSSEChunk(chunk)
			if len(chunk) == 0 {
				continue
			}
			if onOutput != nil {
				onOutput()
			}
			// 首个有效翻译结果才提交响应；此前的读取或翻译错误仍可换源。
			if !committed {
				ttft = time.Since(start).Milliseconds()
				copyTranslatedResponseHeaders(w.Header(), resp.Header, true)
				w.WriteHeader(resp.StatusCode)
				committed = true
			}
			written, writeErr := w.Write(chunk)
			bytesSent += int64(written)
			if writeErr != nil {
				return relayResult{err: writeErr, source: relayDownstream, ttftMs: ttft, committed: true, bytesSent: bytesSent}
			}
			if canFlush {
				flusher.Flush()
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return relayResult{err: err, source: relayUpstream, ttftMs: ttft, committed: committed, bytesSent: bytesSent}
	}
	if !committed {
		return relayResult{err: errEmptyResponse, source: relayUpstream}
	}
	return relayResult{ttftMs: ttft, committed: true, bytesSent: bytesSent}
}

// relayTranslatedSSEToBody 汇集流式上游的终态，再向非流式客户端输出单个 JSON。
func relayTranslatedSSEToBody(ctx context.Context, w http.ResponseWriter, resp *http.Response, start time.Time, onOutput func(), exchange *translate.Exchange) relayResult {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 52_428_800)
	ttft := int64(0)
	firstLine := true
	var rawStream bytes.Buffer
	var terminalPayload []byte
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		rawStream.Write(line)
		rawStream.WriteByte('\n')
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !json.Valid(payload) || !meaningfulSSEPayload(payload) {
			continue
		}
		if onOutput != nil {
			onOutput()
		}
		if firstLine {
			firstLine = false
			ttft = time.Since(start).Milliseconds()
		}
		terminalPayload = append(terminalPayload[:0], payload...)
	}
	if err := scanner.Err(); err != nil {
		return relayResult{err: err, source: relayUpstream, ttftMs: ttft}
	}
	// Claude 转换器需要完整事件序列；其他转换器只需最后一个终态载荷。
	translationInput := terminalPayload
	if exchange.Target == translate.Claude {
		translationInput = rawStream.Bytes()
	}
	if len(translationInput) == 0 {
		return relayResult{err: errEmptyResponse, source: relayUpstream, ttftMs: ttft}
	}
	translated, err := exchange.TranslateNonStream(ctx, translationInput)
	if err != nil || len(translated) == 0 {
		if err == nil {
			err = errEmptyResponse
		}
		return relayResult{err: err, source: relayUpstream, ttftMs: ttft}
	}
	// 流中途出错时聚合结果是错误信封。响应尚未提交，仍可换源，
	// 不能把上游的错误当成正常响应回给客户端。
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && upstream.IsErrorPayload(translated) {
		return relayResult{err: errErrorPayload, source: relayUpstream, ttftMs: ttft}
	}
	copyTranslatedResponseHeaders(w.Header(), resp.Header, false)
	w.WriteHeader(resp.StatusCode)
	written, err := w.Write(translated)
	if err != nil {
		return relayResult{err: err, source: relayDownstream, ttftMs: ttft, committed: true, bytesSent: int64(written)}
	}
	return relayResult{ttftMs: ttft, committed: true, bytesSent: int64(written)}
}

// relayTranslatedBody 必须先读完整正文再转换，因此转换成功前不会提交响应。
func relayTranslatedBody(ctx context.Context, w http.ResponseWriter, resp *http.Response, start time.Time, onOutput func(), exchange *translate.Exchange) relayResult {
	buffer := make([]byte, 32*1024)
	var body bytes.Buffer
	ttft := int64(0)
	firstByte := true
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if onOutput != nil {
				onOutput()
			}
			if firstByte {
				firstByte = false
				ttft = time.Since(start).Milliseconds()
			}
			body.Write(buffer[:n])
		}
		if readErr != nil {
			if readErr != io.EOF {
				return relayResult{err: readErr, source: relayUpstream, ttftMs: ttft}
			}
			break
		}
	}
	if body.Len() == 0 {
		return relayResult{err: errEmptyResponse, source: relayUpstream, ttftMs: ttft}
	}
	translated, err := exchange.TranslateNonStream(ctx, body.Bytes())
	if err != nil {
		return relayResult{err: err, source: relayUpstream, ttftMs: ttft}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && upstream.IsErrorPayload(translated) {
		return relayResult{err: errErrorPayload, source: relayUpstream, ttftMs: ttft}
	}
	copyTranslatedResponseHeaders(w.Header(), resp.Header, false)
	w.WriteHeader(resp.StatusCode)
	written, writeErr := w.Write(translated)
	if writeErr != nil {
		return relayResult{err: writeErr, source: relayDownstream, ttftMs: ttft, committed: true, bytesSent: int64(written)}
	}
	return relayResult{ttftMs: ttft, committed: true, bytesSent: int64(written)}
}

func frameSSEChunk(chunk []byte) []byte {
	chunk = bytes.TrimSpace(chunk)
	if len(chunk) == 0 {
		return nil
	}
	out := append([]byte(nil), chunk...)
	return append(out, '\n', '\n')
}

func copyTranslatedResponseHeaders(dst, src http.Header, stream bool) {
	copyResponseHeaders(dst, src)
	dst.Del("Content-Length")
	dst.Del("Content-Encoding")
	if stream {
		dst.Set("Content-Type", "text/event-stream")
	} else {
		dst.Set("Content-Type", "application/json")
	}
}

// relaySSE 透明转发 SSE，并在每次写入后立即刷新可用的 Flusher。
func relaySSE(w http.ResponseWriter, resp *http.Response, start time.Time, onOutput func()) relayResult {
	flusher, canFlush := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	committed := false
	ttft := int64(0)
	bytesSent := int64(0)
	var pending bytes.Buffer
	detector := sseOutputDetector{}

	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			meaningful := detector.observe(buffer[:n])
			if !committed {
				pending.Write(buffer[:n])
				if !meaningful {
					if pending.Len() > 64<<10 && len(detector.line) == 0 {
						pending.Reset()
					}
					if readErr == nil {
						continue
					}
				} else {
					if onOutput != nil {
						onOutput()
					}
					ttft = time.Since(start).Milliseconds()
					copyResponseHeaders(w.Header(), resp.Header)
					w.WriteHeader(resp.StatusCode)
					committed = true
					written, err := w.Write(pending.Bytes())
					bytesSent += int64(written)
					pending.Reset()
					if err != nil {
						return relayResult{err: err, source: relayDownstream, ttftMs: ttft, committed: true, bytesSent: bytesSent}
					}
				}
			} else {
				if meaningful && onOutput != nil {
					onOutput()
				}
				written, err := w.Write(buffer[:n])
				bytesSent += int64(written)
				if err != nil {
					return relayResult{err: err, source: relayDownstream, ttftMs: ttft, committed: true, bytesSent: bytesSent}
				}
			}
			if committed && canFlush {
				flusher.Flush()
			}
		}

		if readErr != nil {
			if readErr == io.EOF && committed {
				return relayResult{ttftMs: ttft, committed: true, bytesSent: bytesSent}
			}
			if readErr == io.EOF {
				readErr = errEmptyResponse
			}
			return relayResult{err: readErr, source: relayUpstream, ttftMs: ttft, committed: committed, bytesSent: bytesSent}
		}
	}
}

type sseOutputDetector struct {
	line []byte
}

func (d *sseOutputDetector) observe(chunk []byte) bool {
	meaningful := false
	for _, value := range chunk {
		if value != '\n' {
			d.line = append(d.line, value)
			continue
		}
		line := bytes.TrimSpace(d.line)
		d.line = d.line[:0]
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if meaningfulSSEPayload(payload) {
			meaningful = true
		}
	}
	return meaningful
}

func meaningfulSSEPayload(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return false
	}
	if !json.Valid(payload) {
		return true
	}
	var value any
	if json.Unmarshal(payload, &value) != nil {
		return true
	}
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return true
	}
}

// relayBody 先暂存最多 64 KiB，用于识别"HTTP 2xx 包含错误 JSON"的异常上游。
// 超过检查窗口后立即提交并继续流式复制，避免大响应全部驻留内存。
func relayBody(w http.ResponseWriter, resp *http.Response, start time.Time, onOutput func()) relayResult {
	const inspectLimit = 64 << 10
	buffer := make([]byte, 32*1024)
	ttft := int64(0)
	firstByteSeen := false
	var pending bytes.Buffer
	bytesSent := int64(0)
	for pending.Len() <= inspectLimit {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if onOutput != nil {
				onOutput()
			}
			if !firstByteSeen {
				firstByteSeen = true
				ttft = time.Since(start).Milliseconds()
			}
			pending.Write(buffer[:n])
		}
		if readErr != nil {
			if readErr != io.EOF {
				return relayResult{err: readErr, source: relayUpstream, ttftMs: ttft}
			}
			if pending.Len() == 0 {
				if resp.StatusCode == http.StatusNoContent {
					copyResponseHeaders(w.Header(), resp.Header)
					w.WriteHeader(resp.StatusCode)
					return relayResult{ttftMs: time.Since(start).Milliseconds(), committed: true}
				}
				return relayResult{err: errEmptyResponse, source: relayUpstream}
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && upstream.IsErrorPayload(pending.Bytes()) {
				return relayResult{err: errErrorPayload, source: relayUpstream, ttftMs: ttft}
			}
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			written, err := w.Write(pending.Bytes())
			bytesSent += int64(written)
			if err != nil {
				return relayResult{err: err, source: relayDownstream, ttftMs: ttft, committed: true, bytesSent: bytesSent}
			}
			return relayResult{ttftMs: ttft, committed: true, bytesSent: bytesSent}
		}
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	written, err := w.Write(pending.Bytes())
	bytesSent += int64(written)
	if err != nil {
		return relayResult{err: err, source: relayDownstream, ttftMs: ttft, committed: true, bytesSent: bytesSent}
	}
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if onOutput != nil {
				onOutput()
			}
			written, err := w.Write(buffer[:n])
			bytesSent += int64(written)
			if err != nil {
				return relayResult{err: err, source: relayDownstream, ttftMs: ttft, committed: true, bytesSent: bytesSent}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return relayResult{ttftMs: ttft, committed: true, bytesSent: bytesSent}
			}
			return relayResult{err: readErr, source: relayUpstream, ttftMs: ttft, committed: true, bytesSent: bytesSent}
		}
	}
}

// copyResponseHeaders 仅复制端到端响应头，逐跳头由当前 HTTP 连接重新生成。
func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		// X-Request-ID 由网关生成；上游链路 ID 已单独写入审计记录。
		if isHopByHopHeader(key) || strings.EqualFold(key, "X-Request-ID") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

// injectClaudeCacheControl modifies the Anthropic Messages API request body to
// add cache_control with an extended TTL to the last content block of the last
// system message. This signals to the provider that the prefix should be cached
// for longer than the default 5 minutes.
func injectClaudeCacheControl(body []byte, ttl time.Duration) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	// Anthropic cache_control only accepts {"type": "ephemeral"}.
	// The TTL is controlled by the provider (5min default, 1h with beta header),
	// not by a field in cache_control.
	cacheControl := map[string]any{"type": "ephemeral"}

	// Try system field first (can be string or array of content blocks).
	if system, ok := payload["system"]; ok {
		switch s := system.(type) {
		case []any:
			if len(s) > 0 {
				if block, ok := s[len(s)-1].(map[string]any); ok {
					block["cache_control"] = cacheControl
					payload["system"] = s
				}
			}
		case string:
			payload["system"] = []any{
				map[string]any{
					"type":          "text",
					"text":          s,
					"cache_control": cacheControl,
				},
			}
		}
	} else {
		if messages, ok := payload["messages"].([]any); ok && len(messages) > 0 {
			if firstMsg, ok := messages[0].(map[string]any); ok {
				if content, ok := firstMsg["content"].([]any); ok && len(content) > 0 {
					if block, ok := content[len(content)-1].(map[string]any); ok {
						block["cache_control"] = cacheControl
					}
				}
			}
		}
	}

	result, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return result
}
