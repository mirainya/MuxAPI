// Package health 管理渠道熔断、模型能力缓存、路由统计与状态告警。
package health

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is the channel-level circuit breaker state.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Open:
		return "OPEN"
	case HalfOpen:
		return "HALF_OPEN"
	default:
		return "CLOSED"
	}
}

type breaker struct {
	state             State
	fails             int
	openUntil         time.Time
	generation        uint64
	recoveryLease     uint64
	earlyTrial        bool
	recoverySuccesses int
	reopenCount       int
	lastProbe         time.Time
	lastSuccess       time.Time

	latencyMs   int64
	latencyEWMA float64
	inFlight    int64

	reqs       int64
	failReqs   int64
	totLatency int64
	latSamples int64
	trend      []TrendPoint
	lastReqs   int64
	lastFails  int64
}

type modelKey struct {
	upstreamID int64
	model      string
}

// ModelExclusion is the storage-neutral representation of a durable negative
// capability observation. A nil ExcludedUntil means permanent exclusion.
type ModelExclusion struct {
	UpstreamID    int64
	Model         string
	ExcludedUntil *time.Time
	FailureCount  int
	LastStatus    int
	LastReason    string
	LastFailedAt  time.Time
	UpdatedAt     time.Time
	reprobeLease  uint64
}

// ModelExclusionStore is implemented by the persistence layer. The health
// package intentionally depends on this small interface to avoid a store cycle.
type ModelExclusionStore interface {
	LoadModelExclusions() ([]ModelExclusion, error)
	UpsertModelExclusion(ModelExclusion) error
	DeleteModelExclusion(upstreamID int64, model string) error
}

func (e *ModelExclusion) publicCopy() ModelExclusion {
	if e == nil {
		return ModelExclusion{}
	}
	copy := *e
	copy.reprobeLease = 0
	return copy
}

func expiry(now time.Time, ttl time.Duration) *time.Time {
	if ttl <= 0 {
		return nil
	}
	v := now.Add(ttl)
	return &v
}

// Result is the breaker-relevant outcome of one leased attempt.
type Result uint8

const (
	ResultNeutral Result = iota
	ResultSuccess
	ResultFailure
	ResultModelUnsupported
)

// Lease identifies one exact channel attempt. The manager validates its token
// against the internal lease registry before accepting a completion.
type Lease struct {
	UpstreamID int64
	Token      uint64
}

func (l Lease) Valid() bool { return l.UpstreamID != 0 && l.Token != 0 }

type leaseRecord struct {
	upstreamID    int64
	groupID       int64
	generation    uint64
	model         string
	probe         bool
	authoritative bool
	recovery      bool
	modelReprobe  bool
}

type groupRecovery struct {
	owner     uint64
	notBefore time.Time
}

// RecoveryCandidate contains only in-memory data used to pick a controlled
// last-route trial when a group has no normally schedulable channel.
type RecoveryCandidate struct {
	State       string
	EarlyTrial  bool
	ReopenCount int
	LastSuccess time.Time
	OpenUntil   time.Time
}

// TrendPoint is one channel health sample.
type TrendPoint struct {
	TS       int64   `json:"ts"`
	Status   int     `json:"status"`
	LatMs    int64   `json:"lat_ms"`
	SuccRate float64 `json:"succ_rate"`
}

const (
	statNoData   = 0
	statOK       = 1
	statDegraded = 2
	statDown     = 3
	trendCap     = 60
	ewmaAlpha    = 0.3

	defaultModelUnsupportedTTL = time.Duration(0)
	defaultRecoverySuccessGoal = 2
	defaultMaxCooldown         = 5 * time.Minute
)

// AlertEvent reports a channel breaker transition. Model is the request model
// that triggered the transition, not a separate model-level breaker key.
type AlertEvent struct {
	UpstreamID int64  `json:"upstream_id"`
	Model      string `json:"model"`
	FromState  string `json:"from_state"`
	ToState    string `json:"to_state"`
	Fails      int    `json:"fails"`
	TS         int64  `json:"ts"`
}

type Alerter interface {
	Notify(ev AlertEvent)
}

// Manager owns one breaker per upstream and a write-through negative model
// capability cache. Model exclusions never participate in channel recovery.
type Manager struct {
	mu            sync.Mutex
	breakers      map[int64]*breaker
	unsupported   map[modelKey]*ModelExclusion
	capLatest     map[modelKey]uint64
	leases        map[uint64]leaseRecord
	groupRecovery map[int64]*groupRecovery
	nextToken     uint64
	failThreshold int
	cooldown      time.Duration
	recoveryGoal  int
	maxCooldown   time.Duration
	modelTTL      time.Duration
	persistModel  func(ModelExclusion) error
	deleteModel   func(upstreamID int64, model string) error
	alerter       Alerter
}

// New 创建渠道级健康管理器，并规范化失败阈值与冷却时间。
func New(failThreshold int, cooldown time.Duration) *Manager {
	if failThreshold < 1 {
		failThreshold = 1
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &Manager{
		breakers:      make(map[int64]*breaker),
		unsupported:   make(map[modelKey]*ModelExclusion),
		capLatest:     make(map[modelKey]uint64),
		leases:        make(map[uint64]leaseRecord),
		groupRecovery: make(map[int64]*groupRecovery),
		failThreshold: failThreshold,
		cooldown:      cooldown,
		recoveryGoal:  defaultRecoverySuccessGoal,
		maxCooldown:   defaultMaxCooldown,
		modelTTL:      defaultModelUnsupportedTTL,
	}
}

// SetAdvancedPolicy updates recovery and capability-cache behavior without a
// restart. Invalid values fall back to conservative defaults.
func (m *Manager) SetAdvancedPolicy(recoveryGoal int, maxCooldown, modelTTL time.Duration) {
	if recoveryGoal < 1 {
		recoveryGoal = defaultRecoverySuccessGoal
	}
	if maxCooldown <= 0 {
		maxCooldown = defaultMaxCooldown
	}
	if modelTTL < 0 {
		modelTTL = defaultModelUnsupportedTTL
	}
	m.mu.Lock()
	m.recoveryGoal, m.maxCooldown, m.modelTTL = recoveryGoal, maxCooldown, modelTTL
	m.mu.Unlock()
}

// SetModelExclusionStore attaches durable capability state and restores it on
// startup. Expired TTL records remain in memory so Claim can admit only one
// controlled re-probe at a time.
func (m *Manager) SetModelExclusionStore(store ModelExclusionStore) error {
	m.mu.Lock()
	m.persistModel = nil
	m.deleteModel = nil
	m.mu.Unlock()
	if store == nil {
		return nil
	}
	records, err := store.LoadModelExclusions()
	if err != nil {
		return err
	}
	m.mu.Lock()
	for _, record := range records {
		if record.UpstreamID <= 0 || strings.TrimSpace(record.Model) == "" {
			continue
		}
		if record.FailureCount < 1 {
			record.FailureCount = 1
		}
		key := modelKey{record.UpstreamID, record.Model}
		entry := record
		m.unsupported[key] = &entry
		m.nextToken++
		m.capLatest[key] = m.nextToken
	}
	m.persistModel = store.UpsertModelExclusion
	m.deleteModel = store.DeleteModelExclusion
	m.mu.Unlock()
	return nil
}

// RecoverModel removes one exclusion from memory and storage. It is the
// explicit operator path for re-enabling a model on a channel.
func (m *Manager) RecoverModel(id int64, model string) error {
	model = strings.TrimSpace(model)
	if id <= 0 || model == "" {
		return nil
	}
	m.mu.Lock()
	deleteModel := m.deleteModel
	m.mu.Unlock()
	if deleteModel != nil {
		if err := deleteModel(id, model); err != nil {
			return err
		}
	}
	m.mu.Lock()
	key := modelKey{id, model}
	m.nextToken++
	m.capLatest[key] = m.nextToken
	delete(m.unsupported, key)
	m.mu.Unlock()
	return nil
}

// MarkModelsDiscovered records positive discovery without clearing a durable
// exclusion. A model list is evidence that the endpoint advertises a name, not
// proof that this exact request path can serve it.
func (m *Manager) MarkModelsDiscovered(id int64, models []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, model := range models {
		key := modelKey{id, strings.TrimSpace(model)}
		if key.model == "" {
			continue
		}
		if exclusion := m.unsupported[key]; exclusion != nil && (exclusion.ExcludedUntil == nil || time.Now().Before(*exclusion.ExcludedUntil)) {
			continue
		}
		m.nextToken++
		m.capLatest[key] = m.nextToken
	}
}

// SetAlerter 设置状态翻转通知器。
func (m *Manager) SetAlerter(a Alerter) { m.alerter = a }

// SetFailurePolicy updates the breaker policy for future state transitions.
// Existing OPEN channels keep their current deadline; the new cooldown is used
// when they are opened or reopened next.
func (m *Manager) SetFailurePolicy(failThreshold int, cooldown time.Duration) {
	if failThreshold < 1 {
		failThreshold = 1
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	m.mu.Lock()
	m.failThreshold = failThreshold
	m.cooldown = cooldown
	m.mu.Unlock()
}

func (m *Manager) get(id int64) *breaker {
	b := m.breakers[id]
	if b == nil {
		b = &breaker{state: Closed}
		m.breakers[id] = b
	}
	return b
}

func (m *Manager) canServe(b *breaker) bool {
	now := time.Now()
	// 冷却到期只进入半开态，真正恢复仍需后续成功结果确认。
	if b.state == Open {
		if now.Before(b.openUntil) {
			return false
		}
		b.state = HalfOpen
		b.recoverySuccesses = 0
		b.recoveryLease = 0
		b.earlyTrial = false
	}
	return b.state != HalfOpen || b.recoveryLease == 0
}

func (m *Manager) modelUnsupportedLocked(id int64, model string) bool {
	if model == "" {
		return false
	}
	k := modelKey{id, model}
	exclusion, ok := m.unsupported[k]
	if !ok {
		return false
	}
	if exclusion.ExcludedUntil == nil || time.Now().Before(*exclusion.ExcludedUntil) {
		return true
	}
	return exclusion.reprobeLease != 0
}

func (m *Manager) modelNeedsReprobeLocked(id int64, model string) bool {
	exclusion := m.unsupported[modelKey{id, model}]
	return exclusion != nil && exclusion.ExcludedUntil != nil &&
		!time.Now().Before(*exclusion.ExcludedUntil) && exclusion.reprobeLease == 0
}

// IsAvailable checks channel health and the model capability cache.
func (m *Manager) IsAvailable(id int64, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelUnsupportedLocked(id, model) {
		return false
	}
	return m.canServe(m.get(id))
}

// Claim reserves a business request. Recovery requests are exclusive both for
// the channel and for the group, while CLOSED channels remain concurrent.
func (m *Manager) Claim(groupID, id int64, model string) (Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelUnsupportedLocked(id, model) {
		return Lease{}, false
	}
	b := m.get(id)
	if !m.canServe(b) {
		return Lease{}, false
	}
	recovery := b.state == HalfOpen
	if recovery && !m.groupCanRecoverLocked(groupID) {
		return Lease{}, false
	}
	lease := m.newLeaseLocked(groupID, id, model, false, true, recovery)
	if m.modelNeedsReprobeLocked(id, model) {
		key := modelKey{id, model}
		m.unsupported[key].reprobeLease = lease.Token
		record := m.leases[lease.Token]
		record.modelReprobe = true
		m.leases[lease.Token] = record
	}
	return lease, true
}

// ClaimLastResort permits one early, group-scoped recovery trial after all
// normal candidates have become unavailable. A failed trial must wait for the
// regular exponential cooldown before another recovery attempt.
func (m *Manager) ClaimLastResort(groupID, id int64, model string) (Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelUnsupportedLocked(id, model) || !m.groupCanRecoverLocked(groupID) {
		return Lease{}, false
	}
	b := m.get(id)
	if b.state != Open || !b.earlyTrial || b.recoveryLease != 0 {
		return Lease{}, false
	}
	b.state = HalfOpen
	b.recoverySuccesses = 0
	b.earlyTrial = false
	lease := m.newLeaseLocked(groupID, id, model, false, true, true)
	if m.modelNeedsReprobeLocked(id, model) {
		key := modelKey{id, model}
		m.unsupported[key].reprobeLease = lease.Token
		record := m.leases[lease.Token]
		record.modelReprobe = true
		m.leases[lease.Token] = record
	}
	return lease, true
}

// BeginProbe always returns an observation lease so monitoring remains useful.
// An OPEN channel only grants state-changing recovery ownership after cooldown;
// other concurrent or early probes may update monitoring data but not state.
func (m *Manager) BeginProbe(id int64, model string) Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(id)
	m.canServe(b)
	authoritative := b.state == Closed
	recovery := false
	if b.state == HalfOpen && b.recoveryLease == 0 {
		authoritative = true
		recovery = true
	}
	return m.newLeaseLocked(0, id, model, true, authoritative, recovery)
}

func (m *Manager) newLeaseLocked(groupID, id int64, model string, probe, authoritative, recovery bool) Lease {
	m.nextToken++
	token := m.nextToken
	b := m.get(id)
	m.leases[token] = leaseRecord{
		upstreamID: id, groupID: groupID, generation: b.generation, model: model,
		probe: probe, authoritative: authoritative, recovery: recovery,
	}
	if !probe {
		b.inFlight++
	}
	if recovery {
		b.recoveryLease = token
		if groupID != 0 {
			g := m.groupRecovery[groupID]
			if g == nil {
				g = &groupRecovery{}
				m.groupRecovery[groupID] = g
			}
			g.owner = token
		}
	}
	return Lease{UpstreamID: id, Token: token}
}

func (m *Manager) groupCanRecoverLocked(groupID int64) bool {
	if groupID == 0 {
		return true
	}
	g := m.groupRecovery[groupID]
	return g == nil || (g.owner == 0 && !time.Now().Before(g.notBefore))
}

// Complete records and releases one lease exactly once. Results from an older
// breaker generation remain visible in statistics but cannot change state.
func (m *Manager) Complete(lease Lease, result Result, latencyMs int64) {
	m.complete(lease, result, latencyMs, 0, "")
}

// CompleteModelUnsupported records the structured upstream failure details and
// durably excludes this exact upstream/model pair.
func (m *Manager) CompleteModelUnsupported(lease Lease, latencyMs int64, status int, reason string) {
	m.complete(lease, ResultModelUnsupported, latencyMs, status, reason)
}

func (m *Manager) complete(lease Lease, result Result, latencyMs int64, status int, reason string) {
	m.mu.Lock()
	record, ok := m.leases[lease.Token]
	if !ok || record.upstreamID != lease.UpstreamID {
		m.mu.Unlock()
		return
	}
	delete(m.leases, lease.Token)
	b := m.get(record.upstreamID)
	if !record.probe && b.inFlight > 0 {
		b.inFlight--
	}
	if b.recoveryLease == lease.Token {
		b.recoveryLease = 0
	}
	if record.probe {
		b.lastProbe = time.Now()
	} else if result == ResultSuccess || result == ResultFailure {
		b.reqs++
		if result == ResultFailure {
			b.failReqs++
		} else if latencyMs > 0 {
			b.totLatency += latencyMs
			b.latSamples++
		}
	}
	if result == ResultSuccess {
		m.observeSuccessLocked(b, latencyMs)
	}

	sameGeneration := record.generation == b.generation
	key := modelKey{record.upstreamID, record.model}
	var persist *ModelExclusion
	var remove *modelKey
	if exclusion := m.unsupported[key]; exclusion != nil && exclusion.reprobeLease == lease.Token {
		exclusion.reprobeLease = 0
	}
	if sameGeneration && record.model != "" && result == ResultSuccess && lease.Token >= m.capLatest[key] {
		m.capLatest[key] = lease.Token
		if record.modelReprobe {
			delete(m.unsupported, key)
			copy := key
			remove = &copy
		}
	} else if sameGeneration && record.model != "" && result == ResultModelUnsupported && lease.Token >= m.capLatest[key] {
		m.capLatest[key] = lease.Token
		now := time.Now()
		exclusion := m.unsupported[key]
		if exclusion == nil {
			exclusion = &ModelExclusion{UpstreamID: record.upstreamID, Model: record.model}
			m.unsupported[key] = exclusion
		}
		exclusion.FailureCount++
		exclusion.LastStatus = status
		exclusion.LastReason = truncateModelReason(reason)
		exclusion.LastFailedAt = now
		exclusion.UpdatedAt = now
		exclusion.ExcludedUntil = expiry(now, m.modelTTL)
		copy := exclusion.publicCopy()
		persist = &copy
	}

	from, to := b.state, b.state
	if sameGeneration && record.authoritative {
		switch result {
		case ResultSuccess:
			from, to = m.drive(b, true, 0)
		case ResultFailure:
			from, to = m.drive(b, false, 0)
		}
	}
	if record.recovery && record.groupID != 0 {
		g := m.groupRecovery[record.groupID]
		if g != nil && g.owner == lease.Token {
			g.owner = 0
			switch result {
			case ResultSuccess:
				delete(m.groupRecovery, record.groupID)
			case ResultFailure:
				g.notBefore = b.openUntil
			default:
				if !time.Now().Before(g.notBefore) {
					delete(m.groupRecovery, record.groupID)
				}
			}
		}
	}
	ev, flipped := transitionEvent(record.upstreamID, record.model, from, to, b.fails)
	m.mu.Unlock()
	if persist != nil && m.persistModel != nil {
		if err := m.persistModel(*persist); err != nil {
			slog.Error("persist model exclusion failed", "upstream_id", persist.UpstreamID, "model", persist.Model, "err", err)
		}
	}
	if remove != nil && m.deleteModel != nil {
		if err := m.deleteModel(remove.upstreamID, remove.model); err != nil {
			slog.Error("delete expired model exclusion after successful re-probe failed", "upstream_id", remove.upstreamID, "model", remove.model, "err", err)
		}
	}
	if flipped {
		m.dispatch(ev)
	}
}

// Release abandons an attempt without changing health or capability state.
func (m *Manager) Release(lease Lease) { m.Complete(lease, ResultNeutral, 0) }

func (m *Manager) observeSuccessLocked(b *breaker, latencyMs int64) {
	b.lastSuccess = time.Now()
	if latencyMs <= 0 {
		return
	}
	b.latencyMs = latencyMs
	if b.latencyEWMA == 0 {
		b.latencyEWMA = float64(latencyMs)
	} else {
		b.latencyEWMA = ewmaAlpha*float64(latencyMs) + (1-ewmaAlpha)*b.latencyEWMA
	}
}

func (m *Manager) InFlight(id int64) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.get(id).inFlight
}

// MarkModelUnsupported excludes a deterministic model/channel mismatch without
// changing channel health. It is permanent unless modelTTL is configured.
func (m *Manager) MarkModelUnsupported(id int64, model string) {
	m.MarkModelUnsupportedWithDetails(id, model, 0, "")
}

func (m *Manager) MarkModelUnsupportedWithDetails(id int64, model string, status int, reason string) {
	model = strings.TrimSpace(model)
	if id <= 0 || model == "" {
		return
	}
	m.mu.Lock()
	m.nextToken++
	key := modelKey{id, model}
	m.capLatest[key] = m.nextToken
	now := time.Now()
	exclusion := m.unsupported[key]
	if exclusion == nil {
		exclusion = &ModelExclusion{UpstreamID: id, Model: model}
		m.unsupported[key] = exclusion
	}
	exclusion.FailureCount++
	exclusion.LastStatus = status
	exclusion.LastReason = truncateModelReason(reason)
	exclusion.LastFailedAt = now
	exclusion.UpdatedAt = now
	exclusion.ExcludedUntil = expiry(now, m.modelTTL)
	copy := exclusion.publicCopy()
	m.mu.Unlock()
	if m.persistModel != nil {
		if err := m.persistModel(copy); err != nil {
			slog.Error("persist model exclusion failed", "upstream_id", id, "model", model, "err", err)
		}
	}
}

func truncateModelReason(reason string) string {
	const maxRunes = 2048
	runes := []rune(strings.TrimSpace(reason))
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}

func (m *Manager) MarkModelSupported(id int64, model string) {
	_ = m.RecoverModel(id, model)
}

func (m *Manager) MarkModelsSupported(id int64, models []string) {
	for _, model := range models {
		_ = m.RecoverModel(id, model)
	}
}

func (m *Manager) IsModelUnsupported(id int64, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.modelUnsupportedLocked(id, model)
}

// LatencyEWMA 返回渠道的成功请求 TTFT EWMA(ms)，供 P2C 比较；0 表示冷启动。
func (m *Manager) LatencyEWMA(id int64) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(m.get(id).latencyEWMA)
}

// Report records one business attempt against the channel breaker.
func (m *Manager) Report(id int64, model string, ok bool, latencyMs int64) {
	m.mu.Lock()
	b := m.get(id)
	b.reqs++
	if !ok {
		b.failReqs++
	} else {
		if latencyMs > 0 {
			b.totLatency += latencyMs
			b.latSamples++
		}
		m.observeSuccessLocked(b, latencyMs)
	}
	from, to := b.state, b.state
	if b.state != Open {
		from, to = m.drive(b, ok, 0)
	}
	ev, flipped := transitionEvent(id, model, from, to, b.fails)
	m.mu.Unlock()
	if flipped {
		m.dispatch(ev)
	}
}

// ReportTimeout remains for compatibility; a timeout is one ordinary failure.
func (m *Manager) ReportTimeout(id int64, model string, latencyMs int64) {
	m.Report(id, model, false, latencyMs)
}

// ObserveProbe is the synchronous compatibility form of BeginProbe+Complete.
func (m *Manager) ObserveProbe(id int64, model string, ok bool, latencyMs int64) {
	lease := m.BeginProbe(id, model)
	result := ResultFailure
	if ok {
		result = ResultSuccess
	}
	m.Complete(lease, result, latencyMs)
}

// Forget 丢弃某渠道的全部内存状态，供上游被删除时调用。
// 不调用则 breakers/unsupported 会随删除累积，Sample() 还会继续为其追加趋势点。
func (m *Manager) Forget(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for token, record := range m.leases {
		if record.upstreamID != id {
			continue
		}
		if record.groupID != 0 {
			if g := m.groupRecovery[record.groupID]; g != nil && g.owner == token {
				delete(m.groupRecovery, record.groupID)
			}
		}
		delete(m.leases, token)
	}
	delete(m.breakers, id)
	for key := range m.unsupported {
		if key.upstreamID == id {
			delete(m.unsupported, key)
		}
	}
	for key := range m.capLatest {
		if key.upstreamID == id {
			delete(m.capLatest, key)
		}
	}
}

// ResetCircuit manually returns one channel to CLOSED without changing its
// traffic statistics or model capability cache.
func (m *Manager) ResetCircuit(id int64) {
	m.mu.Lock()
	b := m.get(id)
	from := b.state
	oldRecoveryLease := b.recoveryLease
	b.generation++
	b.state = Closed
	b.fails = 0
	b.openUntil = time.Time{}
	b.recoveryLease = 0
	b.earlyTrial = false
	b.recoverySuccesses = 0
	b.reopenCount = 0
	if oldRecoveryLease != 0 {
		for _, g := range m.groupRecovery {
			if g.owner == oldRecoveryLease {
				g.owner = 0
				g.notBefore = time.Time{}
			}
		}
	}
	ev, flipped := transitionEvent(id, "", from, Closed, 0)
	m.mu.Unlock()
	if flipped {
		m.dispatch(ev)
	}
}

// drive 是业务请求与主动探测共用的熔断状态机。
func (m *Manager) drive(b *breaker, ok bool, latencyMs int64) (from, to State) {
	from = b.state
	if ok {
		b.fails = 0
		switch b.state {
		case Closed:
			b.recoverySuccesses = 0
		case Open:
			b.state = HalfOpen
			b.recoverySuccesses = 1
		case HalfOpen:
			b.recoverySuccesses++
			if b.recoverySuccesses >= m.recoveryGoal {
				b.state = Closed
				b.recoverySuccesses = 0
				b.reopenCount = 0
				b.openUntil = time.Time{}
				b.earlyTrial = false
			}
		}
		return from, b.state
	}

	b.fails++
	b.recoverySuccesses = 0
	switch {
	case b.state == HalfOpen:
		m.openLocked(b, false)
	case b.state == Closed && b.fails >= m.failThreshold:
		m.openLocked(b, true)
	}
	return from, b.state
}

func (m *Manager) openLocked(b *breaker, allowEarlyTrial bool) {
	b.state = Open
	b.generation++
	b.recoveryLease = 0
	b.earlyTrial = allowEarlyTrial
	b.reopenCount++
	b.openUntil = time.Now().Add(m.backoff(b.reopenCount))
}

// backoff 对反复熔断使用指数冷却，最长为五分钟或基础冷却时间。
func (m *Manager) backoff(reopenCount int) time.Duration {
	if reopenCount < 1 {
		return m.cooldown
	}
	shift := reopenCount - 1
	if shift > 6 {
		shift = 6
	}
	d := m.cooldown * time.Duration(1<<shift)
	capDuration := m.maxCooldown
	if m.cooldown > capDuration {
		capDuration = m.cooldown
	}
	if d > capDuration {
		return capDuration
	}
	return d
}

func (m *Manager) dispatch(ev AlertEvent) {
	if m.alerter != nil {
		go m.alerter.Notify(ev)
	}
}

func transitionEvent(id int64, model string, from, to State, fails int) (AlertEvent, bool) {
	flip := (from == Closed && to == Open) || (from != Closed && to == Closed)
	if !flip {
		return AlertEvent{}, false
	}
	return AlertEvent{
		UpstreamID: id,
		Model:      model,
		FromState:  from.String(),
		ToState:    to.String(),
		Fails:      fails,
		TS:         time.Now().Unix(),
	}, true
}

// RouteSample 是从历史审计恢复渠道延迟 EWMA 所需的最小数据。
type RouteSample struct {
	UpstreamID int64
	OK         bool
	LatencyMs  int64
}

// Seed restores channel-level routing statistics without restoring OPEN state.
func (m *Manager) Seed(samples []RouteSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sample := range samples {
		b := m.get(sample.UpstreamID)
		if sample.OK && sample.LatencyMs > 0 {
			if b.latencyEWMA == 0 {
				b.latencyEWMA = float64(sample.LatencyMs)
			} else {
				b.latencyEWMA = ewmaAlpha*float64(sample.LatencyMs) + (1-ewmaAlpha)*b.latencyEWMA
			}
		}
	}
}

// Snapshot 是管理接口读取的渠道级健康快照。
type Snapshot struct {
	State     string
	Fails     int
	LatencyMs int64
	OpenUntil time.Time
	LastProbe time.Time
	Reqs      int64
	FailReqs  int64
	SuccRate  float64
	AvgLatMs  int64
	InFlight  int64
	Trend     []TrendPoint
}

// Snapshot 返回指定渠道的累计统计与当前熔断状态副本。
func (m *Manager) Snapshot(id int64) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(id)
	sn := Snapshot{
		State: b.state.String(), Fails: b.fails, LatencyMs: b.latencyMs,
		OpenUntil: b.openUntil, LastProbe: b.lastProbe,
		Reqs: b.reqs, FailReqs: b.failReqs, InFlight: b.inFlight,
		Trend: append([]TrendPoint(nil), b.trend...),
	}
	if b.reqs > 0 {
		sn.SuccRate = float64(b.reqs-b.failReqs) / float64(b.reqs)
	}
	if b.latSamples > 0 {
		sn.AvgLatMs = b.totLatency / b.latSamples
	}
	return sn
}

// ModelHealth 表示一条模型能力排除记录，不是独立熔断器。
type ModelHealth struct {
	Model         string     `json:"model"`
	State         string     `json:"state"`
	ExcludedUntil *time.Time `json:"excluded_until,omitempty"`
	FailureCount  int        `json:"failure_count"`
	LastStatus    int        `json:"last_status"`
	LastReason    string     `json:"last_reason,omitempty"`
	LastFailedAt  time.Time  `json:"last_failed_at"`
}

func (m *Manager) ModelStates(id int64) []ModelHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]ModelHealth, 0)
	for key, exclusion := range m.unsupported {
		if key.upstreamID == id {
			state := "UNSUPPORTED"
			if exclusion.ExcludedUntil != nil && !now.Before(*exclusion.ExcludedUntil) {
				state = "REPROBE_PENDING"
			}
			out = append(out, ModelHealth{
				Model: key.model, State: state, ExcludedUntil: exclusion.ExcludedUntil,
				FailureCount: exclusion.FailureCount, LastStatus: exclusion.LastStatus,
				LastReason: exclusion.LastReason, LastFailedAt: exclusion.LastFailedAt,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func (m *Manager) EffectiveState(id int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.get(id).state.String()
}

// RecoveryInfo returns a side-effect-free snapshot used for last-route choice.
func (m *Manager) RecoveryInfo(id int64) RecoveryCandidate {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(id)
	return RecoveryCandidate{
		State: b.state.String(), EarlyTrial: b.earlyTrial && b.recoveryLease == 0,
		ReopenCount: b.reopenCount, LastSuccess: b.lastSuccess, OpenUntil: b.openUntil,
	}
}

// Sample 将累计请求计数转换成一个趋势采样点，并限制内存窗口长度。
func (m *Manager) Sample() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	for _, b := range m.breakers {
		dReqs := b.reqs - b.lastReqs
		dFails := b.failReqs - b.lastFails
		rate := 1.0
		if dReqs > 0 {
			rate = float64(dReqs-dFails) / float64(dReqs)
		}
		b.lastReqs, b.lastFails = b.reqs, b.failReqs
		status := statOK
		switch {
		case b.state == Open:
			status = statDown
		case b.state == HalfOpen || (dReqs > 0 && rate < 1):
			status = statDegraded
		case b.reqs == 0 && b.lastProbe.IsZero():
			status = statNoData
		}
		b.trend = append(b.trend, TrendPoint{TS: now, Status: status, LatMs: b.latencyMs, SuccRate: rate})
		if len(b.trend) > trendCap {
			b.trend = b.trend[len(b.trend)-trendCap:]
		}
	}
}
