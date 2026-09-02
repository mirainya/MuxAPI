package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

const (
	defaultRefreshInterval = 10 * time.Minute
	defaultRefreshTimeout  = 10 * time.Second
	defaultConcurrency     = 4
	defaultPricingInterval = 24 * time.Hour
	defaultPricingTimeout  = 30 * time.Second
	// rateLimitBackoff 上游 429 后跳过多少个刷新周期。取略大于 interval 的值就够，
	// 3 倍能覆盖 15-30 分钟窗口的上游限流(us.oojj.top 实测约每 15-20 分钟一次)。
	rateLimitBackoff = 3
)

// Manager schedules provider billing collection independently from request forwarding.
type Manager struct {
	store           *store.Store
	interval        time.Duration
	timeout         time.Duration
	concurrency     int
	slots           chan struct{}
	pricingInterval time.Duration
	pricingTimeout  time.Duration
	pricingURL      string
	pricingClient   *http.Client
	pricingFallback []byte

	// coolDown 记录每个上游服务(按 base_url)下次允许再打计费端点的时刻。
	// 用 base_url 而非 upstream_id 是因为限流通常施加在服务端 IP/账号级别：
	// aws0/aws0ex/aws0sushua 都在 us.oojj.top，其中一把 key 拿到 429，剩下的
	// 10 分钟内基本也白打。共享冷却避免撞墙 + 打光对方好意的排队额度。
	// 只有 429 触发冷却；普通错误(网络断/500) 走原节奏重试。
	coolDownMu sync.Mutex
	coolDown   map[string]time.Time
}

func NewManager(st *store.Store) *Manager {
	return &Manager{
		store: st, interval: defaultRefreshInterval, timeout: defaultRefreshTimeout,
		concurrency: defaultConcurrency, slots: make(chan struct{}, defaultConcurrency),
		pricingInterval: defaultPricingInterval, pricingTimeout: defaultPricingTimeout,
		pricingURL: defaultPricingURL, pricingClient: http.DefaultClient,
		pricingFallback: embeddedPricingCatalog,
		coolDown:        make(map[string]time.Time),
	}
}

// poolKey normalizes base_url so小写/末尾斜杠差异不产生独立池。
func poolKey(u *upstream.Upstream) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(u.BaseURL), "/"))
}

func (m *Manager) inCoolDown(u *upstream.Upstream) bool {
	key := poolKey(u)
	if key == "" {
		return false
	}
	m.coolDownMu.Lock()
	defer m.coolDownMu.Unlock()
	until, ok := m.coolDown[key]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(m.coolDown, key)
	return false
}

func (m *Manager) markRateLimited(u *upstream.Upstream) {
	key := poolKey(u)
	if key == "" {
		return
	}
	m.coolDownMu.Lock()
	m.coolDown[key] = time.Now().Add(rateLimitBackoff * m.interval)
	m.coolDownMu.Unlock()
}

func (m *Manager) acquire(ctx context.Context) error {
	select {
	case m.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) release() { <-m.slots }

func (m *Manager) refreshItem(ctx context.Context, item *upstream.Upstream) (store.BillingStatus, error) {
	if item.BillingType == upstream.BillingNone || item.BillingType == "" {
		return store.BillingStatus{}, ErrBillingDisabled
	}
	if err := m.acquire(ctx); err != nil {
		return store.BillingStatus{}, err
	}
	defer m.release()

	requestCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	result, collectErr := Fetch(requestCtx, item)
	now := time.Now().Unix()
	if collectErr != nil {
		if err := m.store.SaveBillingFailure(item.ID, collectErr.Error(), now); err != nil {
			return store.BillingStatus{}, errors.Join(collectErr, err)
		}
		state, err := m.store.GetBillingStatus(item.ID)
		if err != nil {
			return store.BillingStatus{}, errors.Join(collectErr, err)
		}
		return state, collectErr
	}
	flushCtx, flushCancel := context.WithTimeout(ctx, 2*time.Second)
	flushErr := m.store.FlushRequests(flushCtx)
	flushCancel()
	if flushErr != nil {
		return store.BillingStatus{}, fmt.Errorf("flush request audit before billing snapshot: %w", flushErr)
	}

	status := "ok"
	if result.Warning != "" {
		status = "partial"
	}
	observedAt := result.ObservedAt.Unix()
	if result.ObservedAt.IsZero() {
		observedAt = now
	}
	state := store.BillingStatus{
		UpstreamID: item.ID, Currency: result.Currency, Remaining: result.Remaining,
		Unlimited: result.Unlimited, BillingGroup: result.BillingGroup,
		GroupMultiplier: result.GroupMultiplier, EffectiveMultiplier: result.EffectiveMultiplier,
		ReportedListCost: result.ReportedListCost, ReportedActualCost: result.ReportedActualCost,
		Status: status, Error: result.Warning, ObservedAt: observedAt, RefreshedAt: now,
	}
	if err := m.store.SaveBillingSuccess(state); err != nil {
		return store.BillingStatus{}, err
	}
	return m.store.GetBillingStatus(item.ID)
}

// Refresh updates one upstream immediately and returns the persisted state.
func (m *Manager) Refresh(ctx context.Context, upstreamID int64) (store.BillingStatus, error) {
	item, err := m.store.Get(upstreamID)
	if err != nil {
		return store.BillingStatus{}, err
	}
	state, err := m.refreshItem(ctx, item)
	if errors.Is(err, ErrRateLimited) {
		m.markRateLimited(item)
	}
	return state, err
}

// RefreshAll updates every configured billing upstream with a bounded worker pool.
// 上游按数据陈旧度排序（last_success_at 越老越优先）：一轮刷不完时(冷却/超时)，
// 最需要更新的余额/倍率先落，路由决策拿到的数字保持最新。
// base_url 命中冷却池的 upstream 整个跳过，避免撞墙 + 消耗对方限流额度。
func (m *Manager) RefreshAll(ctx context.Context) {
	items, err := m.store.List()
	if err != nil {
		slog.Warn("list billing upstreams failed", "err", err)
		return
	}
	statuses, err := m.store.ListBillingStatusesLite()
	if err != nil {
		// 无历史状态并非致命；退化为无序刷新
		slog.Warn("load billing statuses for prioritization failed", "err", err)
		statuses = nil
	}

	// 过滤 + 排序：先按 base_url 冷却池排除，再按 last_success_at 升序（0=从未成功，最高优）
	queue := make([]*upstream.Upstream, 0, len(items))
	skippedCoolDown := 0
	for _, item := range items {
		if item.BillingType == upstream.BillingNone || item.BillingType == "" {
			continue
		}
		if m.inCoolDown(item) {
			skippedCoolDown++
			continue
		}
		queue = append(queue, item)
	}
	sort.SliceStable(queue, func(i, j int) bool {
		return staleness(statuses, queue[i].ID) < staleness(statuses, queue[j].ID)
	})
	if skippedCoolDown > 0 {
		slog.Debug("billing refresh skipped upstreams in cool-down",
			"count", skippedCoolDown, "queued", len(queue))
	}

	jobs := make(chan *upstream.Upstream)
	var workers sync.WaitGroup
	workers.Add(m.concurrency)
	for range m.concurrency {
		go func() {
			defer workers.Done()
			for item := range jobs {
				_, err := m.refreshItem(ctx, item)
				if err == nil || errors.Is(err, context.Canceled) {
					continue
				}
				if errors.Is(err, ErrRateLimited) {
					m.markRateLimited(item)
					// 上游限流是常态而非故障，只记 INFO 一次(冷却期内不会再打)
					slog.Info("billing refresh rate limited, backing off",
						"upstream_id", item.ID, "name", item.Name, "pool", poolKey(item),
						"backoff", rateLimitBackoff*m.interval)
					continue
				}
				slog.Warn("billing refresh failed", "upstream_id", item.ID, "name", item.Name, "err", err)
			}
		}()
	}
	for _, item := range queue {
		// 竞态窗口：从进队到 worker 取走之间，如果同池另一个 upstream 撞了 429 并
		// mark 了冷却，这里再判一次。避免用剩下的 worker 继续消耗同池额度。
		if m.inCoolDown(item) {
			continue
		}
		select {
		case jobs <- item:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		}
	}
	close(jobs)
	workers.Wait()
}

// staleness 返回排序键：越小越优先刷。取 last_success_at；从未成功过(=0)返回最小值。
func staleness(statuses map[int64]store.BillingStatus, id int64) int64 {
	if statuses == nil {
		return 0
	}
	if s, ok := statuses[id]; ok {
		return s.LastSuccessAt
	}
	return 0
}

// Run refreshes billing and pricing independently on fixed low-frequency intervals.
func (m *Manager) Run(ctx context.Context) {
	if err := m.refreshPricing(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("pricing catalog refresh failed", "err", err)
	}
	m.RefreshAll(ctx)
	billingTicker := time.NewTicker(m.interval)
	pricingTicker := time.NewTicker(m.pricingInterval)
	defer billingTicker.Stop()
	defer pricingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-billingTicker.C:
			m.RefreshAll(ctx)
		case <-pricingTicker.C:
			if err := m.refreshPricing(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("pricing catalog refresh failed", "err", err)
			}
		}
	}
}
