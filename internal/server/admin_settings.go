package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultFailThreshold = 3
	defaultCooldown      = "30s"
	defaultMaxAttempts   = 6
	defaultMaxBodyBytes  = 32 << 20
)

// adminSettings reads and updates database-backed runtime settings.
func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getRuntimeSettings(w)
	case http.MethodPut:
		s.putRuntimeSettings(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getRuntimeSettings(w http.ResponseWriter) {
	settings, err := s.store.Settings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	value := func(key string) string { return settings[key] }
	logRetention, logRetentionSource := intSettingValueAllowZero(value("request_retention_days"), 0)
	alertWebhook, alertWebhookSource := stringSettingValue(value("alert_webhook"), "")
	alertDebounce, alertDebounceSource := settingValue(value("alert_debounce"), "60s")
	firstResponseTimeout, firstResponseTimeoutSource := intSettingValue(value("first_response_timeout_ms"), 120000)
	failThreshold, failThresholdSource := intSettingValue(value("fail_threshold"), defaultFailThreshold)
	cooldown, cooldownSource := settingValue(value("cooldown"), defaultCooldown)
	maxAttempts, maxAttemptsSource := intSettingValue(value("max_upstream_attempts"), defaultMaxAttempts)
	maxBodyBytes, maxBodySource := intSettingValue(value("max_body_bytes"), defaultMaxBodyBytes)
	defaults := map[string]string{
		"adaptive_timeout_enabled": "true", "adaptive_timeout_floor_ms": "10000",
		"adaptive_timeout_multiplier": "2", "adaptive_timeout_min_samples": "5",
		"adaptive_timeout_token_step": "50000", "adaptive_timeout_token_bonus_ms": "5000",
		"stream_idle_timeout_ms": "300000", "intelligent_routing_enabled": "true",
		"routing_window": "15m", "routing_cost_tie_tolerance": "0.01",
		"routing_latency_weight": "0.25", "routing_reliability_weight": "0.15",
		"routing_min_samples": "20", "routing_max_ttft_ms": "0", "routing_max_duration_ms": "0",
		"routing_allow_unknown_price": "false", "routing_exploration_rate": "0.02",
		"breaker_recovery_successes": "2", "breaker_max_cooldown": "5m",
		"model_unsupported_ttl": "0s", "billing_snapshot_retention_days": "0",
		"probe_retention_hours": "0",
	}

	out := map[string]string{
		"log_retention":                       value("request_retention_days"),
		"alert_webhook":                       value("alert_webhook"),
		"alert_debounce":                      value("alert_debounce"),
		"first_response_timeout_ms":           value("first_response_timeout_ms"),
		"fail_threshold":                      value("fail_threshold"),
		"cooldown":                            value("cooldown"),
		"max_upstream_attempts":               value("max_upstream_attempts"),
		"max_body_bytes":                      value("max_body_bytes"),
		"effective_log_retention":             logRetention,
		"effective_alert_webhook":             alertWebhook,
		"effective_alert_debounce":            alertDebounce,
		"effective_first_response_timeout_ms": firstResponseTimeout,
		"effective_fail_threshold":            failThreshold,
		"effective_cooldown":                  cooldown,
		"effective_max_upstream_attempts":     maxAttempts,
		"effective_max_body_bytes":            maxBodyBytes,
		"log_retention_source":                logRetentionSource,
		"alert_webhook_source":                alertWebhookSource,
		"alert_debounce_source":               alertDebounceSource,
		"first_response_timeout_ms_source":    firstResponseTimeoutSource,
		"fail_threshold_source":               failThresholdSource,
		"cooldown_source":                     cooldownSource,
		"max_upstream_attempts_source":        maxAttemptsSource,
		"max_body_bytes_source":               maxBodySource,
	}
	for key, def := range defaults {
		setting := value(key)
		out[key] = setting
		out["effective_"+key] = def
		out[key+"_source"] = "default"
		if setting != "" {
			out["effective_"+key] = setting
			out[key+"_source"] = "settings"
		}
	}
	writeJSON(w, out)
}

type runtimeSettingsInput struct {
	LogRetention                 any `json:"log_retention"`
	AlertWebhook                 any `json:"alert_webhook"`
	AlertDebounce                any `json:"alert_debounce"`
	FirstResponseTimeoutMs       any `json:"first_response_timeout_ms"`
	FailThreshold                any `json:"fail_threshold"`
	Cooldown                     any `json:"cooldown"`
	MaxUpstreamAttempts          any `json:"max_upstream_attempts"`
	MaxBodyBytes                 any `json:"max_body_bytes"`
	AdaptiveTimeoutEnabled       any `json:"adaptive_timeout_enabled"`
	AdaptiveTimeoutFloorMs       any `json:"adaptive_timeout_floor_ms"`
	AdaptiveTimeoutMultiplier    any `json:"adaptive_timeout_multiplier"`
	AdaptiveTimeoutMinSamples    any `json:"adaptive_timeout_min_samples"`
	AdaptiveTimeoutTokenStep     any `json:"adaptive_timeout_token_step"`
	AdaptiveTimeoutTokenBonusMs  any `json:"adaptive_timeout_token_bonus_ms"`
	StreamIdleTimeoutMs          any `json:"stream_idle_timeout_ms"`
	IntelligentRoutingEnabled    any `json:"intelligent_routing_enabled"`
	RoutingWindow                any `json:"routing_window"`
	RoutingCostTieTolerance      any `json:"routing_cost_tie_tolerance"`
	RoutingLatencyWeight         any `json:"routing_latency_weight"`
	RoutingReliabilityWeight     any `json:"routing_reliability_weight"`
	RoutingMinSamples            any `json:"routing_min_samples"`
	RoutingMaxTTFTMs             any `json:"routing_max_ttft_ms"`
	RoutingMaxDurationMs         any `json:"routing_max_duration_ms"`
	RoutingAllowUnknownPrice     any `json:"routing_allow_unknown_price"`
	RoutingExplorationRate       any `json:"routing_exploration_rate"`
	BreakerRecoverySuccesses     any `json:"breaker_recovery_successes"`
	BreakerMaxCooldown           any `json:"breaker_max_cooldown"`
	ModelUnsupportedTTL          any `json:"model_unsupported_ttl"`
	BillingSnapshotRetentionDays any `json:"billing_snapshot_retention_days"`
	ProbeRetentionHours          any `json:"probe_retention_hours"`
}

func (s *Server) putRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	var raw runtimeSettingsInput
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, "设置参数格式无效", http.StatusBadRequest)
		return
	}

	values := map[string]string{
		"request_retention_days":          settingString(raw.LogRetention),
		"alert_webhook":                   settingString(raw.AlertWebhook),
		"alert_debounce":                  settingString(raw.AlertDebounce),
		"first_response_timeout_ms":       settingString(raw.FirstResponseTimeoutMs),
		"fail_threshold":                  settingString(raw.FailThreshold),
		"cooldown":                        settingString(raw.Cooldown),
		"max_upstream_attempts":           settingString(raw.MaxUpstreamAttempts),
		"max_body_bytes":                  settingString(raw.MaxBodyBytes),
		"adaptive_timeout_enabled":        settingString(raw.AdaptiveTimeoutEnabled),
		"adaptive_timeout_floor_ms":       settingString(raw.AdaptiveTimeoutFloorMs),
		"adaptive_timeout_multiplier":     settingString(raw.AdaptiveTimeoutMultiplier),
		"adaptive_timeout_min_samples":    settingString(raw.AdaptiveTimeoutMinSamples),
		"adaptive_timeout_token_step":     settingString(raw.AdaptiveTimeoutTokenStep),
		"adaptive_timeout_token_bonus_ms": settingString(raw.AdaptiveTimeoutTokenBonusMs),
		"stream_idle_timeout_ms":          settingString(raw.StreamIdleTimeoutMs),
		"intelligent_routing_enabled":     settingString(raw.IntelligentRoutingEnabled),
		"routing_window":                  settingString(raw.RoutingWindow),
		"routing_cost_tie_tolerance":      settingString(raw.RoutingCostTieTolerance),
		"routing_latency_weight":          settingString(raw.RoutingLatencyWeight),
		"routing_reliability_weight":      settingString(raw.RoutingReliabilityWeight),
		"routing_min_samples":             settingString(raw.RoutingMinSamples),
		"routing_max_ttft_ms":             settingString(raw.RoutingMaxTTFTMs),
		"routing_max_duration_ms":         settingString(raw.RoutingMaxDurationMs),
		"routing_allow_unknown_price":     settingString(raw.RoutingAllowUnknownPrice),
		"routing_exploration_rate":        settingString(raw.RoutingExplorationRate),
		"breaker_recovery_successes":      settingString(raw.BreakerRecoverySuccesses),
		"breaker_max_cooldown":            settingString(raw.BreakerMaxCooldown),
		"model_unsupported_ttl":           settingString(raw.ModelUnsupportedTTL),
		"billing_snapshot_retention_days": settingString(raw.BillingSnapshotRetentionDays),
		"probe_retention_hours":           settingString(raw.ProbeRetentionHours),
	}
	provided := map[string]bool{
		"request_retention_days":          raw.LogRetention != nil,
		"alert_webhook":                   raw.AlertWebhook != nil,
		"alert_debounce":                  raw.AlertDebounce != nil,
		"first_response_timeout_ms":       raw.FirstResponseTimeoutMs != nil,
		"fail_threshold":                  raw.FailThreshold != nil,
		"cooldown":                        raw.Cooldown != nil,
		"max_upstream_attempts":           raw.MaxUpstreamAttempts != nil,
		"max_body_bytes":                  raw.MaxBodyBytes != nil,
		"adaptive_timeout_enabled":        raw.AdaptiveTimeoutEnabled != nil,
		"adaptive_timeout_floor_ms":       raw.AdaptiveTimeoutFloorMs != nil,
		"adaptive_timeout_multiplier":     raw.AdaptiveTimeoutMultiplier != nil,
		"adaptive_timeout_min_samples":    raw.AdaptiveTimeoutMinSamples != nil,
		"adaptive_timeout_token_step":     raw.AdaptiveTimeoutTokenStep != nil,
		"adaptive_timeout_token_bonus_ms": raw.AdaptiveTimeoutTokenBonusMs != nil,
		"stream_idle_timeout_ms":          raw.StreamIdleTimeoutMs != nil,
		"intelligent_routing_enabled":     raw.IntelligentRoutingEnabled != nil,
		"routing_window":                  raw.RoutingWindow != nil,
		"routing_cost_tie_tolerance":      raw.RoutingCostTieTolerance != nil,
		"routing_latency_weight":          raw.RoutingLatencyWeight != nil,
		"routing_reliability_weight":      raw.RoutingReliabilityWeight != nil,
		"routing_min_samples":             raw.RoutingMinSamples != nil,
		"routing_max_ttft_ms":             raw.RoutingMaxTTFTMs != nil,
		"routing_max_duration_ms":         raw.RoutingMaxDurationMs != nil,
		"routing_allow_unknown_price":     raw.RoutingAllowUnknownPrice != nil,
		"routing_exploration_rate":        raw.RoutingExplorationRate != nil,
		"breaker_recovery_successes":      raw.BreakerRecoverySuccesses != nil,
		"breaker_max_cooldown":            raw.BreakerMaxCooldown != nil,
		"model_unsupported_ttl":           raw.ModelUnsupportedTTL != nil,
		"billing_snapshot_retention_days": raw.BillingSnapshotRetentionDays != nil,
		"probe_retention_hours":           raw.ProbeRetentionHours != nil,
	}
	for _, key := range []string{
		"request_retention_days", "alert_debounce", "first_response_timeout_ms",
		"fail_threshold", "cooldown", "max_upstream_attempts", "max_body_bytes",
		"adaptive_timeout_enabled", "adaptive_timeout_floor_ms", "adaptive_timeout_multiplier",
		"adaptive_timeout_min_samples", "adaptive_timeout_token_step", "adaptive_timeout_token_bonus_ms",
		"stream_idle_timeout_ms", "intelligent_routing_enabled", "routing_window",
		"routing_cost_tie_tolerance", "routing_latency_weight", "routing_reliability_weight",
		"routing_min_samples", "routing_max_ttft_ms", "routing_max_duration_ms",
		"routing_allow_unknown_price", "routing_exploration_rate", "breaker_recovery_successes",
		"breaker_max_cooldown", "model_unsupported_ttl", "billing_snapshot_retention_days", "probe_retention_hours",
	} {
		if provided[key] && strings.TrimSpace(values[key]) == "" {
			http.Error(w, "设置项不能为空", http.StatusBadRequest)
			return
		}
	}

	for _, key := range []string{"adaptive_timeout_enabled", "intelligent_routing_enabled", "routing_allow_unknown_price"} {
		if !provided[key] {
			continue
		}
		enabled, err := strconv.ParseBool(strings.TrimSpace(values[key]))
		if err != nil {
			http.Error(w, "开关设置须为 true 或 false", http.StatusBadRequest)
			return
		}
		values[key] = strconv.FormatBool(enabled)
	}
	current, err := s.store.Settings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	combined := make(map[string]string, len(current)+len(values))
	for key, value := range current {
		combined[key] = value
	}
	for key, value := range values {
		if provided[key] {
			combined[key] = value
		}
	}
	if err := validateRuntimeSettings(combined); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	updates := make(map[string]string, len(values))
	for key, value := range values {
		if provided[key] {
			updates[key] = value
		}
	}
	if err := s.store.SetSettings(updates); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.settingsChanged != nil {
		s.settingsChanged()
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateRuntimeSettings(values map[string]string) error {
	if value := values["request_retention_days"]; value != "" {
		if n, err := strconv.Atoi(value); err != nil || n < 0 || n > 365 {
			return settingsError("请求记录保留天数须为 0~365 的整数，0 表示永久保留")
		}
	}
	webhook := values["alert_webhook"]
	if webhook != "" && !strings.HasPrefix(webhook, "http://") && !strings.HasPrefix(webhook, "https://") {
		return settingsError("告警 Webhook 须以 http:// 或 https:// 开头")
	}
	if value := values["alert_debounce"]; value != "" {
		if _, err := time.ParseDuration(value); err != nil {
			return settingsError("告警间隔格式无效")
		}
	}
	if value := values["first_response_timeout_ms"]; value != "" {
		if n, err := strconv.Atoi(value); err != nil || n < 1000 || n > 600000 {
			return settingsError("首响应超时须为 1000~600000 毫秒")
		}
	}
	if value := values["fail_threshold"]; value != "" {
		if n, err := strconv.Atoi(value); err != nil || n < 1 || n > 100 {
			return settingsError("熔断失败阈值须为 1~100 的整数")
		}
	}
	if value := values["cooldown"]; value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration < time.Second || duration > 24*time.Hour {
			return settingsError("熔断冷却时间须在 1 秒到 24 小时之间")
		}
	}
	if value := values["max_upstream_attempts"]; value != "" {
		if n, err := strconv.Atoi(value); err != nil || n < 1 || n > 100 {
			return settingsError("最大上游尝试数须为 1~100 的整数")
		}
	}
	if value := values["max_body_bytes"]; value != "" {
		if n, err := strconv.ParseInt(value, 10, 64); err != nil || n < 1 || n > 256<<20 {
			return settingsError("请求体上限须为 1~268435456 字节")
		}
	}
	intRange := func(key string, min, max int64, message string) error {
		if value := values[key]; value != "" {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < min || n > max {
				return settingsError(message)
			}
		}
		return nil
	}
	floatRange := func(key string, min, max float64, message string) error {
		if value := values[key]; value != "" {
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < min || n > max {
				return settingsError(message)
			}
		}
		return nil
	}
	durationRange := func(key string, min, max time.Duration, message string) error {
		if value := values[key]; value != "" {
			d, err := time.ParseDuration(value)
			if err != nil || d < min || d > max {
				return settingsError(message)
			}
		}
		return nil
	}
	for _, key := range []string{"adaptive_timeout_enabled", "intelligent_routing_enabled", "routing_allow_unknown_price"} {
		if value := values[key]; value != "" {
			if _, err := strconv.ParseBool(value); err != nil {
				return settingsError("开关设置须为 true 或 false")
			}
		}
	}
	checks := []error{
		intRange("adaptive_timeout_floor_ms", 1000, 600000, "自适应超时下限须为 1000~600000 毫秒"),
		floatRange("adaptive_timeout_multiplier", 0.1, 10, "自适应超时倍数须为 0.1~10"),
		intRange("adaptive_timeout_min_samples", 1, 10000, "自适应超时最少样本须为 1~10000"),
		intRange("adaptive_timeout_token_step", 1, 10000000, "长上下文步长须为 1~10000000 token"),
		intRange("adaptive_timeout_token_bonus_ms", 0, 600000, "长上下文超时增量须为 0~600000 毫秒"),
		intRange("stream_idle_timeout_ms", 1000, 3600000, "流空闲超时须为 1000~3600000 毫秒"),
		durationRange("routing_window", time.Minute, 24*time.Hour, "路由统计窗口须在 1 分钟到 24 小时之间"),
		floatRange("routing_cost_tie_tolerance", 0, 1, "成本接近阈值须为 0~1"),
		floatRange("routing_latency_weight", 0, 10, "延迟权重须为 0~10"),
		floatRange("routing_reliability_weight", 0, 10, "可靠性权重须为 0~10"),
		intRange("routing_min_samples", 0, 1000000, "路由最少样本须为 0~1000000"),
		intRange("routing_max_ttft_ms", 0, 3600000, "TTFT 硬上限须为 0~3600000 毫秒"),
		intRange("routing_max_duration_ms", 0, 86400000, "总耗时硬上限须为 0~86400000 毫秒"),
		floatRange("routing_exploration_rate", 0, 1, "探索率须为 0~1"),
		intRange("breaker_recovery_successes", 1, 100, "熔断恢复成功次数须为 1~100"),
		durationRange("breaker_max_cooldown", time.Second, 24*time.Hour, "最大熔断冷却须在 1 秒到 24 小时之间"),
		durationRange("model_unsupported_ttl", 0, 24*time.Hour, "模型不支持缓存须在 0 秒到 24 小时之间，0 表示永久排除"),
		intRange("billing_snapshot_retention_days", 0, 3650, "计费快照保留天数须为 0~3650"),
		intRange("probe_retention_hours", 0, 87600, "探测记录保留小时须为 0~87600"),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	integer := func(key string, def int) int {
		value, err := strconv.Atoi(values[key])
		if err != nil {
			return def
		}
		return value
	}
	duration := func(key string, def time.Duration) time.Duration {
		value, err := time.ParseDuration(values[key])
		if err != nil {
			return def
		}
		return value
	}
	if integer("adaptive_timeout_floor_ms", 10000) > integer("first_response_timeout_ms", 120000) {
		return settingsError("自适应超时下限不能超过首响应超时")
	}
	if duration("breaker_max_cooldown", 5*time.Minute) < duration("cooldown", 30*time.Second) {
		return settingsError("最大熔断冷却不能小于基础冷却时间")
	}
	return nil
}

type settingsError string

func (e settingsError) Error() string { return string(e) }
