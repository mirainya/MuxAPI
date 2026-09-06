// Command muxapi 组装网关各层，并管理 HTTP 服务与后台任务的生命周期。
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mirainya/muxapi/internal/backup"
	"github.com/mirainya/muxapi/internal/billing"
	"github.com/mirainya/muxapi/internal/config"
	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/modelmapping"
	"github.com/mirainya/muxapi/internal/monitor"
	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/server"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// Version is set at build time via -ldflags "-X main.Version=v1.2.3".
var Version = "dev"

func main() {
	cfg := config.Load()
	if cfg.AdminToken == "" {
		slog.Warn("MUXAPI_TOKEN 未设置：管理后台无鉴权，切勿对外暴露")
	}

	st, err := store.OpenWithOptions(cfg.DatabaseURL, store.OpenOptions{ReadOnly: cfg.ReadOnly})
	if err != nil {
		slog.Error("open store failed", "err", err)
		return
	}
	defer st.Close()

	// Runtime policies live in the settings table. Environment values only seed
	// an installation that has not stored the corresponding setting yet.
	initialMaxAttempts := cfg.MaxRetries
	if initialMaxAttempts < 6 {
		initialMaxAttempts = 6
	}
	runtimeDefaults := map[string]string{
		"fail_threshold":        strconv.Itoa(cfg.FailThreshold),
		"cooldown":              cfg.Cooldown.String(),
		"max_upstream_attempts": strconv.Itoa(initialMaxAttempts),
		"max_body_bytes":        strconv.FormatInt(cfg.MaxBody, 10),
		"model_unsupported_ttl": "0s",
	}
	storedSettings, err := st.Settings()
	if err != nil {
		slog.Error("load runtime settings failed", "err", err)
		return
	}
	if !cfg.ReadOnly {
		missing := make(map[string]string)
		for key, value := range runtimeDefaults {
			if storedSettings[key] == "" {
				missing[key] = value
			}
		}
		if err := st.SetSettings(missing); err != nil {
			slog.Error("seed runtime settings failed", "err", err)
			return
		}
		for key, value := range missing {
			storedSettings[key] = value
		}
	}
	runtimeSettings := newRuntimeSettingSnapshot(st, storedSettings)
	settingDuration := func(key string, def time.Duration) func() time.Duration {
		return func() time.Duration {
			if d, err := time.ParseDuration(runtimeSettings.Get(key)); err == nil && d > 0 {
				return d
			}
			return def
		}
	}
	settingDurationAllowZero := func(key string, def time.Duration) func() time.Duration {
		return func() time.Duration {
			if d, err := time.ParseDuration(runtimeSettings.Get(key)); err == nil && d >= 0 {
				return d
			}
			return def
		}
	}
	settingString := func(key, def string) func() string {
		return func() string {
			if v := runtimeSettings.Get(key); v != "" {
				return v
			}
			return def
		}
	}
	settingInt := func(key string, def int) func() int {
		return func() int {
			if n, err := strconv.Atoi(runtimeSettings.Get(key)); err == nil && n > 0 {
				return n
			}
			return def
		}
	}
	settingInt64 := func(key string, def int64) func() int64 {
		return func() int64 {
			if n, err := strconv.ParseInt(runtimeSettings.Get(key), 10, 64); err == nil && n > 0 {
				return n
			}
			return def
		}
	}
	settingIntAllowZero := func(key string, def int) func() int {
		return func() int {
			if n, err := strconv.Atoi(runtimeSettings.Get(key)); err == nil && n >= 0 {
				return n
			}
			return def
		}
	}
	settingFloat := func(key string, def float64) func() float64 {
		return func() float64 {
			if n, err := strconv.ParseFloat(runtimeSettings.Get(key), 64); err == nil && n >= 0 {
				return n
			}
			return def
		}
	}
	settingBool := func(key string, def bool) func() bool {
		return func() bool {
			if enabled, err := strconv.ParseBool(runtimeSettings.Get(key)); err == nil {
				return enabled
			}
			return def
		}
	}

	// 调度用：某分组下启用的上游（实时查库，后台增删即时生效）
	listByGroup := func(groupID int64) []*upstream.Upstream {
		ups, err := st.ListEnabledByGroup(groupID)
		if err != nil {
			slog.Error("list upstreams failed", "err", err)
			return nil
		}
		return ups
	}

	// 依赖顺序：健康状态 -> 调度 -> 转发 -> 主动监控 -> HTTP 接入。
	failThreshold := settingInt("fail_threshold", cfg.FailThreshold)
	cooldown := settingDuration("cooldown", cfg.Cooldown)
	hm := health.New(failThreshold(), cooldown())
	breakerRecoverySuccesses := settingInt("breaker_recovery_successes", 2)
	breakerMaxCooldown := settingDuration("breaker_max_cooldown", 5*time.Minute)
	modelUnsupportedTTL := settingDurationAllowZero("model_unsupported_ttl", 0)
	hm.SetAdvancedPolicy(breakerRecoverySuccesses(), breakerMaxCooldown(), modelUnsupportedTTL())
	if err := hm.SetModelExclusionStore(st); err != nil {
		slog.Error("restore model exclusions failed", "err", err)
		return
	}
	// 重启恢复：用最近的转发样本重建选路用的渠道延迟 EWMA，不重建熔断状态
	if samples, err := st.RecentSamples(2000); err != nil {
		slog.Warn("seed route stats from logs failed", "err", err)
	} else if len(samples) > 0 {
		hs := make([]health.RouteSample, len(samples))
		for i, s := range samples {
			hs[i] = health.RouteSample{UpstreamID: s.UpstreamID, OK: s.OK, LatencyMs: s.LatencyMs}
		}
		hm.Seed(hs)
		slog.Info("seeded route stats from logs", "samples", len(hs))
	}
	sched := scheduler.New(listByGroup, hm)
	// Cost/cache-aware selection is enabled globally. It falls back to the
	// existing health/P2C scheduler when pricing is incomplete or cold.
	routeConfig := func() routing.Config {
		return routing.Config{
			Window:            settingDuration("routing_window", 15*time.Minute)(),
			CostTieTolerance:  settingFloat("routing_cost_tie_tolerance", 0.01)(),
			LatencyWeight:     settingFloat("routing_latency_weight", 0.25)(),
			ReliabilityWeight: settingFloat("routing_reliability_weight", 0.15)(),
			MinSamples:        int64(settingIntAllowZero("routing_min_samples", 20)()),
			MaxTTFTMs:         settingFloat("routing_max_ttft_ms", 0)(),
			MaxDurationMs:     settingFloat("routing_max_duration_ms", 0)(),
			AllowUnknownPrice: settingBool("routing_allow_unknown_price", false)(),
			ExplorationRate:   settingFloat("routing_exploration_rate", 0.02)(),
		}
	}
	sched.SetIntelligentRouting(st, routeConfig)
	sched.SetIntelligentRoutingEnabled(settingBool("intelligent_routing_enabled", true))
	maxUpstreamAttempts := settingInt("max_upstream_attempts", initialMaxAttempts)
	var maxAttemptsValue atomic.Int64
	maxAttemptsValue.Store(int64(maxUpstreamAttempts()))
	fwd := forward.New(sched, hm, int(maxAttemptsValue.Load()))
	fwd.SetMaxAttemptsProvider(func() int { return int(maxAttemptsValue.Load()) })
	// 模型名归一化：别名表来自环境变量，上下文后缀剥离为内置行为。
	if aliases := forward.ParseModelAliases(cfg.ModelAliases); len(aliases) > 0 {
		fwd.SetModelAliases(aliases)
		slog.Info("model aliases loaded", "count", len(aliases))
	}
	mon := monitor.New(st)
	// 首字节和流中空闲使用不同的超时策略，避免把 TTFT 分位数误用于长流。
	firstResponseTimeoutMs := settingInt("first_response_timeout_ms", 120000)
	fwd.SetFirstResponseTimeout(func() time.Duration {
		return time.Duration(firstResponseTimeoutMs()) * time.Millisecond
	})

	// Model mapping: per-upstream model name translation + auto-learning.
	modelMapper := modelmapping.New(st)
	fwd.SetModelMapper(modelMapper)
	// Adaptive first-byte timeout: use per-upstream P95 TTFT from routing stats.
	fwd.SetTTFTEstimator(sched)
	fwd.SetAdaptiveTimeoutPolicyProvider(func() forward.AdaptiveTimeoutPolicy {
		return forward.AdaptiveTimeoutPolicy{
			Enabled:    settingBool("adaptive_timeout_enabled", true)(),
			Floor:      time.Duration(settingInt("adaptive_timeout_floor_ms", 10000)()) * time.Millisecond,
			Multiplier: settingFloat("adaptive_timeout_multiplier", 2)(),
			MinSamples: int64(settingInt("adaptive_timeout_min_samples", 5)()),
			TokenStep:  int64(settingInt("adaptive_timeout_token_step", 50000)()),
			TokenBonus: time.Duration(settingIntAllowZero("adaptive_timeout_token_bonus_ms", 5000)()) * time.Millisecond,
		}
	})
	fwd.SetStreamIdleTimeoutProvider(func() time.Duration {
		return time.Duration(settingInt("stream_idle_timeout_ms", 300000)()) * time.Millisecond
	})

	// 健康事件主动告警：熔断翻转时推送 Webhook（URL 空则关闭）。
	// id→name 解析用现成 List()，解析不到回退 id 字符串。
	upstreamName := func(id int64) string {
		ups, _ := st.List()
		for _, u := range ups {
			if u.ID == id {
				return u.Name
			}
		}
		return ""
	}
	hm.SetAlerter(health.NewWebhookAlerter(
		settingString("alert_webhook", ""),
		settingDuration("alert_debounce", 60*time.Second),
		upstreamName,
	))
	// 探测系统统一：monitor 探测器是唯一主动探测源，注入 hm 后一次探测双写
	//（看板统计 + 路由熔断器）。探测间隔/路径已全下放到各监控项，
	// 传 nil 让 prober 用内置默认（5m / /v1/chat/completions），监控项可逐项覆盖。
	monProber := monitor.NewProber(mon, st, hm, nil, nil)
	billingMgr := billing.NewManager(st)
	backupSvc := backup.NewService(st, cfg.DatabaseURL)
	maxBodyBytes := settingInt64("max_body_bytes", cfg.MaxBody)
	var maxBodyValue atomic.Int64
	maxBodyValue.Store(maxBodyBytes())
	srv := server.New(fwd, cfg.AdminToken, st, hm, mon, monProber, maxBodyValue.Load())
	srv.SetReadOnly(cfg.ReadOnly)
	srv.SetVersion(Version)
	srv.SetMaxBodyProvider(maxBodyValue.Load)
	srv.SetSettingsChanged(func() {
		if err := runtimeSettings.Reload(); err != nil {
			slog.Error("refresh runtime settings failed", "err", err)
			return
		}
		hm.SetFailurePolicy(failThreshold(), cooldown())
		hm.SetAdvancedPolicy(breakerRecoverySuccesses(), breakerMaxCooldown(), modelUnsupportedTTL())
		maxAttemptsValue.Store(int64(maxUpstreamAttempts()))
		maxBodyValue.Store(maxBodyBytes())
	})
	srv.SetBillingManager(billingMgr)
	srv.SetBackupService(backupSvc)
	srv.SetModelMappingService(modelMapper)
	modelMapper.SetModelLister(srv)

	// 收到 SIGINT/SIGTERM 时取消：停探测并触发优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 后台 goroutine 用 WaitGroup 跟踪：Shutdown 后等它们退出再 st.Close()，
	// 消除退出期探测/清理仍在写库而 DB 已关的竞态。
	var wg sync.WaitGroup
	if !cfg.ReadOnly {
		// 监控、计费、备份和清理任务都会写库；只读调试模式全部停用。
		wg.Add(1)
		go func() {
			defer wg.Done()
			monProber.Run(ctx)
		}()
		// Provider billing collection is low-frequency and isolated from forwarding.
		wg.Add(1)
		go func() {
			defer wg.Done()
			billingMgr.Run(ctx)
		}()
		// Backup scheduler: runs cron jobs and waits for ctx cancellation.
		wg.Add(1)
		go func() {
			defer wg.Done()
			backupSvc.Run(ctx)
		}()
		// Zero means permanent retention. This is the default for routing and
		// billing history; a positive value remains available as an explicit
		// operator-configured cleanup policy.
		requestRetentionDays := settingInt("request_retention_days", 0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			runLogJanitor(ctx, st, requestRetentionDays, settingIntAllowZero("probe_retention_hours", 0), settingIntAllowZero("billing_snapshot_retention_days", 0))
		}()
	} else {
		slog.Info("read-only mode enabled: background writers disabled")
	}

	// 防 slowloris：仅限制读 header 的时长，不设全局 ReadTimeout——
	// 否则会误杀慢上传/流式上传。MaxHeaderBytes 限制 header 体积。
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server exited", "err", err)
			stop()
		}
	}()
	slog.Info("muxapi starting", "addr", cfg.Addr)

	<-ctx.Done() // 等信号
	slog.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		slog.Error("shutdown failed", "err", err)
	}
	wg.Wait() // 等后台 goroutine 退出，再让 defer st.Close() 安全关库
}

// runLogJanitor 定时执行可选的数据清理：启动先执行一次，之后每 10 分钟一轮。
// keepDays() 每轮读取最新保留天数；每批最多 5000 个请求，避免长事务影响业务查询。
// 默认不删除探测结果、请求记录或计费快照；正数保留策略仍可由运维显式启用。
func runLogJanitor(ctx context.Context, st *store.Store, keepDays, probeKeepHours, billingKeepDays func() int) {
	const (
		requestBatch = 5000
		maxBatches   = 10
	)
	prune := func() {
		days := keepDays()
		if days > 0 {
			for batch := 0; batch < maxBatches; batch++ {
				deleted, err := st.PruneRequests(days, requestBatch)
				if err != nil {
					slog.Error("request janitor prune failed", "err", err)
					break
				}
				if deleted > 0 {
					slog.Info("request janitor pruned", "deleted", deleted, "keepDays", days)
				}
				if deleted < requestBatch {
					break
				}
			}
		}
		probeHours := probeKeepHours()
		if deleted, err := st.PruneProbes(probeHours); err != nil {
			slog.Error("probe janitor prune failed", "err", err)
		} else if deleted > 0 {
			slog.Info("probe janitor pruned", "deleted", deleted, "keepHours", probeHours)
		}
		// 计费快照同样默认永久保留；仅显式正数策略才清理。
		billingDays := billingKeepDays()
		if deleted, err := st.PruneBillingSnapshots(billingDays); err != nil {
			slog.Error("billing snapshot janitor prune failed", "err", err)
		} else if deleted > 0 {
			slog.Info("billing snapshot janitor pruned", "deleted", deleted,
				"keepDays", billingDays)
		}
	}
	prune() // 启动即清一次，立刻收敛历史堆积
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
