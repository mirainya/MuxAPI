package store

import (
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"
)

const (
	billingAuditAbsoluteTolerance = 0.01
	billingAuditRelativeTolerance = 0.10
	// billingCatalogTolerance 价目核对阈值。本地价目表(LiteLLM)与上游自报原价
	// 天然有差异，容忍度比计费核对宽；超出说明上游挂牌价明显偏离公共行情。
	billingCatalogTolerance = 0.25
)

// BillingAudit compares provider counters across snapshots in one window.
// It avoids treating cumulative provider values as per-window cost.
//
// 比对分两条独立轨道，避免把「价目表不一致」误报成「上游多收」：
//   - 价目核对：本地 ListCost vs 上游自报 ReportedListCost，差异说明两张价目表不一致
//   - 计费核对：ActualCost vs 计费基准×倍率，基准优先用上游自报原价（不受价表漂移影响）
//
// BillingBasis 标明计费核对用的是哪种基准："reported"（可信）或 "local"（上游未提供
// 原价时降级，结论受价目表差异污染）。
type BillingAudit struct {
	Status             string   `json:"status"`
	Reason             string   `json:"reason,omitempty"`
	FromAt             int64    `json:"from_at,omitempty"`
	ToAt               int64    `json:"to_at,omitempty"`
	WindowSeconds      int64    `json:"window_seconds,omitempty"`
	SnapshotCount      int      `json:"snapshot_count,omitempty"`
	MultiplierChanged  bool     `json:"multiplier_changed,omitempty"`
	ListCost           *float64 `json:"list_cost,omitempty"`
	ReportedListCost   *float64 `json:"reported_list_cost,omitempty"`
	BillingBasis       string   `json:"billing_basis,omitempty"`
	LocalPricingReason string   `json:"local_pricing_reason,omitempty"`
	CatalogDeviation   *float64 `json:"catalog_deviation,omitempty"`
	CatalogRate        *float64 `json:"catalog_deviation_rate,omitempty"`
	TheoreticalCost    *float64 `json:"theoretical_cost,omitempty"`
	ActualCost         *float64 `json:"actual_cost,omitempty"`
	ActualSource       string   `json:"actual_source,omitempty"`
	BalanceSpent       *float64 `json:"balance_spent,omitempty"`
	ExpectedMultiplier *float64 `json:"expected_multiplier,omitempty"`
	ObservedMultiplier *float64 `json:"observed_multiplier,omitempty"`
	Deviation          *float64 `json:"deviation,omitempty"`
	DeviationRate      *float64 `json:"deviation_rate,omitempty"`
	PricingSource      string   `json:"pricing_source,omitempty"`
	PricingVersion     string   `json:"pricing_version,omitempty"`
	PriceCoverage      *float64 `json:"price_coverage,omitempty"`
	RequestCount       int64    `json:"request_count,omitempty"`
	PricedRequestCount int64    `json:"priced_request_count,omitempty"`
	MissingUsageCount  int64    `json:"missing_usage_count,omitempty"`
	MissingModels      []string `json:"missing_models,omitempty"`

	// theoreticalPerPair 标记 TheoreticalCost 是否由逐对倍率累加而来。
	// 仅在此情况下，倍率变动过的窗口其偏差才可用于判定超收。
	theoreticalPerPair bool
}

// BillingStatus is the latest provider-reported billing state for one upstream.
// Nullable numeric fields distinguish a real zero from data the provider did not expose.
type BillingStatus struct {
	UpstreamID          int64         `json:"upstream_id"`
	Currency            string        `json:"currency"`
	Remaining           *float64      `json:"remaining,omitempty"`
	Unlimited           bool          `json:"unlimited"`
	BillingGroup        string        `json:"billing_group,omitempty"`
	GroupMultiplier     *float64      `json:"group_multiplier,omitempty"`
	EffectiveMultiplier *float64      `json:"effective_multiplier,omitempty"`
	ReportedListCost    *float64      `json:"reported_list_cost,omitempty"`
	ReportedActualCost  *float64      `json:"reported_actual_cost,omitempty"`
	Status              string        `json:"status"`
	Error               string        `json:"error,omitempty"`
	ObservedAt          int64         `json:"observed_at,omitempty"`
	LastSuccessAt       int64         `json:"last_success_at,omitempty"`
	RefreshedAt         int64         `json:"refreshed_at"`
	Audit               *BillingAudit `json:"audit,omitempty"`
}

// BillingSnapshot retains successful provider counters for later cost audits.
type BillingSnapshot struct {
	ID                  int64    `json:"id"`
	UpstreamID          int64    `json:"upstream_id"`
	Currency            string   `json:"currency"`
	Remaining           *float64 `json:"remaining,omitempty"`
	Unlimited           bool     `json:"unlimited"`
	BillingGroup        string   `json:"billing_group,omitempty"`
	GroupMultiplier     *float64 `json:"group_multiplier,omitempty"`
	EffectiveMultiplier *float64 `json:"effective_multiplier,omitempty"`
	ReportedListCost    *float64 `json:"reported_list_cost,omitempty"`
	ReportedActualCost  *float64 `json:"reported_actual_cost,omitempty"`
	ObservedAt          int64    `json:"observed_at"`
}

func billingTimestamp(value int64, fallback time.Time) time.Time {
	if value > 0 {
		return time.Unix(value, 0)
	}
	return fallback
}

// SaveBillingSuccess atomically replaces the latest state and appends a history snapshot.
func (s *Store) SaveBillingSuccess(state BillingStatus) error {
	now := time.Now()
	observed := billingTimestamp(state.ObservedAt, now)
	refreshed := billingTimestamp(state.RefreshedAt, now)
	if state.Currency == "" {
		state.Currency = "USD"
	}
	if state.Status == "" {
		state.Status = "ok"
	}
	tx, err := s.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO upstream_billing_status(
		upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,effective_multiplier,
		reported_list_cost,reported_actual_cost,status,error_text,observed_at,last_success_at,refreshed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(upstream_id) DO UPDATE SET
		currency=excluded.currency,remaining=excluded.remaining,unlimited=excluded.unlimited,
		billing_group=COALESCE(excluded.billing_group,upstream_billing_status.billing_group),
		group_multiplier=COALESCE(excluded.group_multiplier,upstream_billing_status.group_multiplier),
		effective_multiplier=COALESCE(excluded.effective_multiplier,upstream_billing_status.effective_multiplier),
		reported_list_cost=excluded.reported_list_cost,
		reported_actual_cost=excluded.reported_actual_cost,status=excluded.status,error_text=excluded.error_text,
		observed_at=excluded.observed_at,last_success_at=excluded.last_success_at,refreshed_at=excluded.refreshed_at`,
		state.UpstreamID, state.Currency, state.Remaining, state.Unlimited, state.BillingGroup,
		state.GroupMultiplier, state.EffectiveMultiplier, state.ReportedListCost, state.ReportedActualCost,
		state.Status, state.Error, s.timeValue(observed), s.timeValue(refreshed), s.timeValue(refreshed))
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO upstream_billing_snapshots(
		upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,effective_multiplier,
		reported_list_cost,reported_actual_cost,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		state.UpstreamID, state.Currency, state.Remaining, state.Unlimited, state.BillingGroup,
		state.GroupMultiplier, state.EffectiveMultiplier, state.ReportedListCost, state.ReportedActualCost,
		s.timeValue(observed))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SaveBillingFailure records a failed refresh without discarding the last successful values.
func (s *Store) SaveBillingFailure(upstreamID int64, message string, refreshedAt int64) error {
	refreshed := billingTimestamp(refreshedAt, time.Now())
	_, err := s.exec(`INSERT INTO upstream_billing_status(
		upstream_id,currency,status,error_text,refreshed_at) VALUES(?,'USD','error',?,?)
		ON CONFLICT(upstream_id) DO UPDATE SET status='error',error_text=excluded.error_text,
		refreshed_at=excluded.refreshed_at`, upstreamID, message, s.timeValue(refreshed))
	return err
}

// SetBillingMultiplier 手动录入一次倍率(等价于"人工探测")。同时写 effective/group，
// status 保持 ok；下次 auto-refresh 从上游扣费日志拿到 group_ratio 会自然覆盖，
// 因此这里不引入新的持久化字段(与 auto-refresh 出的普通值同轨)。
func (s *Store) SetBillingMultiplier(upstreamID int64, multiplier float64) error {
	now := time.Now()
	nowUnix := now.Unix()
	refreshed := billingTimestamp(nowUnix, now)
	observed := refreshed
	_, err := s.exec(`INSERT INTO upstream_billing_status(
		upstream_id,currency,effective_multiplier,group_multiplier,status,
		observed_at,last_success_at,refreshed_at)
		VALUES(?,'USD',?,?,'ok',?,?,?)
		ON CONFLICT(upstream_id) DO UPDATE SET
			effective_multiplier=excluded.effective_multiplier,
			group_multiplier=excluded.group_multiplier,
			status='ok',error_text='',
			observed_at=excluded.observed_at,
			last_success_at=excluded.last_success_at,
			refreshed_at=excluded.refreshed_at`,
		upstreamID, multiplier, multiplier,
		s.timeValue(observed), s.timeValue(observed), s.timeValue(refreshed))
	return err
}

// LastKnownMultiplier returns the most recent non-null effective_multiplier
// from billing snapshots. Used as fallback when the current status has lost
// its multiplier due to partial/error refreshes.
func (s *Store) LastKnownMultiplier(upstreamID int64) (float64, error) {
	var m float64
	err := s.queryRow(`SELECT effective_multiplier FROM upstream_billing_snapshots
		WHERE upstream_id=? AND effective_multiplier IS NOT NULL AND effective_multiplier > 0
		ORDER BY observed_at DESC LIMIT 1`, upstreamID).Scan(&m)
	if err != nil {
		return 0, err
	}
	return m, nil
}

// ResetBillingData removes counters tied to an old provider type, URL, or API key.
func (s *Store) ResetBillingData(upstreamID int64) error {
	tx, err := s.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM upstream_billing_snapshots WHERE upstream_id=?`, upstreamID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM upstream_billing_status WHERE upstream_id=?`, upstreamID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) billingTimeExpr(column string) string {
	return "COALESCE(" + s.unixExpr(column) + ",0)"
}

func scanBillingStatus(scanner interface{ Scan(...any) error }) (BillingStatus, error) {
	var state BillingStatus
	err := scanner.Scan(&state.UpstreamID, &state.Currency, &state.Remaining, &state.Unlimited,
		&state.BillingGroup, &state.GroupMultiplier, &state.EffectiveMultiplier,
		&state.ReportedListCost, &state.ReportedActualCost, &state.Status, &state.Error,
		&state.ObservedAt, &state.LastSuccessAt, &state.RefreshedAt)
	return state, err
}

func billingValue(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func billingCounterDelta(current, previous *float64) (*float64, bool) {
	if current == nil || previous == nil {
		return nil, true
	}
	delta := *current - *previous
	if delta < -1e-9 {
		return nil, false
	}
	if math.Abs(delta) < 1e-9 {
		delta = 0
	}
	return &delta, true
}

func snapshotMultiplier(snapshot BillingSnapshot) *float64 {
	if snapshot.EffectiveMultiplier != nil {
		return snapshot.EffectiveMultiplier
	}
	return snapshot.GroupMultiplier
}

func billingAuditBaseFromSnapshots(snapshots []BillingSnapshot) BillingAudit {
	audit := BillingAudit{Status: "pending", Reason: "insufficient_snapshots"}
	if len(snapshots) < 2 {
		return audit
	}
	latest, previous := snapshots[0], snapshots[1]
	audit.FromAt = previous.ObservedAt
	audit.ToAt = latest.ObservedAt
	audit.ExpectedMultiplier = billingValue(snapshotMultiplier(latest))

	reportedListCost, _ := billingCounterDelta(latest.ReportedListCost, previous.ReportedListCost)
	actualCost, actualValid := billingCounterDelta(latest.ReportedActualCost, previous.ReportedActualCost)
	audit.ReportedListCost = reportedListCost

	if !latest.Unlimited && !previous.Unlimited && latest.Remaining != nil && previous.Remaining != nil {
		spent := *previous.Remaining - *latest.Remaining
		if spent >= -1e-9 {
			if math.Abs(spent) < 1e-9 {
				spent = 0
			}
			audit.BalanceSpent = &spent
		}
	}

	if actualValid && actualCost != nil {
		audit.ActualCost = actualCost
		audit.ActualSource = "reported"
	} else if audit.BalanceSpent != nil {
		audit.ActualCost = billingValue(audit.BalanceSpent)
		audit.ActualSource = "balance"
	}
	return audit
}

func appendMissingModel(models []string, model string) []string {
	if model == "" {
		model = "(empty model)"
	}
	for _, existing := range models {
		if existing == model {
			return models
		}
	}
	return append(models, model)
}

func priceTokenPart(tokens int64, unitCost *float64) (float64, bool) {
	if tokens <= 0 {
		return 0, true
	}
	if unitCost == nil {
		return 0, false
	}
	return float64(tokens) * *unitCost, true
}

func usageListCost(usage BillingWindowUsage, price ModelPricing) (float64, bool) {
	inputTokens := usage.InputTokens
	if usage.Protocol != "claude" {
		inputTokens -= usage.CachedTokens
		if inputTokens < 0 {
			return 0, false
		}
	}
	inputCost, inputOK := priceTokenPart(inputTokens, price.InputCostPerToken)
	outputCost, outputOK := priceTokenPart(usage.OutputTokens, price.OutputCostPerToken)
	cacheReadCost, cacheReadOK := priceTokenPart(usage.CachedTokens, price.CacheReadInputTokenCost)
	cacheWriteCost, cacheWriteOK := priceTokenPart(usage.CacheCreationTokens, price.CacheWriteInputTokenCost)
	return inputCost + outputCost + cacheReadCost + cacheWriteCost,
		inputOK && outputOK && cacheReadOK && cacheWriteOK
}

// applyLocalPricing 用本地价目表估算窗口用量成本。
//
// 它只负责「算出能算的」并记录降级原因，不设置最终 Status —— 本地价目不完整
// 只影响价目核对轨道，计费核对（实际扣费 vs 上游自报原价×倍率）完全不依赖它。
// 早期版本在这里直接判 unavailable，导致上游账目明明精确吻合却什么结论都不给。
func (s *Store) applyLocalPricing(audit *BillingAudit, upstreamID int64) error {
	status, err := s.GetPricingCatalogStatus()
	if err == nil {
		audit.PricingSource = status.Source
		audit.PricingVersion = status.Version
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	usageGroups, err := s.ListBillingWindowUsage(upstreamID, audit.FromAt, audit.ToAt)
	if err != nil {
		return err
	}
	var listCost float64
	for _, usage := range usageGroups {
		audit.RequestCount += usage.RequestCount
		audit.MissingUsageCount += usage.MissingUsageCount
		eligibleRequests := usage.RequestCount - usage.MissingUsageCount
		price, lookupErr := s.LookupModelPricing(usage.Model)
		if lookupErr != nil {
			if errors.Is(lookupErr, sql.ErrNoRows) {
				audit.MissingModels = appendMissingModel(audit.MissingModels, usage.Model)
				continue
			}
			return lookupErr
		}
		cost, complete := usageListCost(usage, price)
		if !complete {
			audit.MissingModels = appendMissingModel(audit.MissingModels, usage.Model)
			continue
		}
		listCost += cost
		audit.PricedRequestCount += eligibleRequests
	}
	if audit.RequestCount > 0 {
		coverage := float64(audit.PricedRequestCount) / float64(audit.RequestCount)
		audit.PriceCoverage = &coverage
	}
	// 记录本地估算为何不可信；ListCost 保持 nil 让价目核对自动跳过。
	switch {
	case audit.RequestCount > 0 && (status.ModelCount == 0 || audit.PricingSource == ""):
		audit.LocalPricingReason = "pricing_catalog_unavailable"
	case audit.MissingUsageCount > 0:
		audit.LocalPricingReason = "request_usage_incomplete"
	case len(audit.MissingModels) > 0:
		audit.LocalPricingReason = "model_price_unavailable"
	default:
		audit.ListCost = &listCost
	}
	return nil
}

func lookupModelPricing(prices map[string]ModelPricing, model string) (ModelPricing, bool) {
	for _, candidate := range pricingModelCandidates(model) {
		if price, ok := prices[candidate]; ok {
			return price, true
		}
	}
	return ModelPricing{}, false
}

func (s *Store) listModelPricing(models []string) (map[string]ModelPricing, error) {
	seen := make(map[string]struct{})
	var candidates []string
	for _, model := range models {
		for _, candidate := range pricingModelCandidates(model) {
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}
	prices := make(map[string]ModelPricing, len(candidates))
	if len(candidates) == 0 {
		return prices, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(candidates)), ",")
	rows, err := s.query(`SELECT model,input_cost_per_token,output_cost_per_token,
		cache_read_input_token_cost,cache_creation_input_token_cost
		FROM model_pricing WHERE model IN (`+placeholders+`)`, stringSliceToAny(candidates)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var price ModelPricing
		if err := rows.Scan(&price.Model, &price.InputCostPerToken, &price.OutputCostPerToken,
			&price.CacheReadInputTokenCost, &price.CacheWriteInputTokenCost); err != nil {
			return nil, err
		}
		prices[price.Model] = price
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return prices, nil
}

func stringSliceToAny(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

func (s *Store) billingAuditFromSnapshots(snapshots []BillingSnapshot) BillingAudit {
	audit := billingAuditBaseFromSnapshots(snapshots)
	if status, err := s.GetPricingCatalogStatus(); err == nil {
		audit.PricingSource = status.Source
		audit.PricingVersion = status.Version
	}
	if len(snapshots) < 2 {
		return audit
	}
	if err := s.applyLocalPricing(&audit, snapshots[0].UpstreamID); err != nil {
		audit.Status = "unavailable"
		audit.Reason = "pricing_query_failed"
		return audit
	}
	latestMultiplier := snapshotMultiplier(snapshots[0])
	previousMultiplier := snapshotMultiplier(snapshots[1])
	if latestMultiplier != nil && previousMultiplier != nil &&
		math.Abs(*previousMultiplier-*latestMultiplier) > 1e-9 {
		audit.MultiplierChanged = true
	}
	audit.SnapshotCount = len(snapshots)
	evaluateBillingAudit(&audit, latestMultiplier)
	return audit
}

func scanBillingSnapshot(scanner interface{ Scan(...any) error }) (BillingSnapshot, error) {
	var snapshot BillingSnapshot
	err := scanner.Scan(&snapshot.ID, &snapshot.UpstreamID, &snapshot.Currency, &snapshot.Remaining,
		&snapshot.Unlimited, &snapshot.BillingGroup, &snapshot.GroupMultiplier,
		&snapshot.EffectiveMultiplier, &snapshot.ReportedListCost, &snapshot.ReportedActualCost,
		&snapshot.ObservedAt)
	return snapshot, err
}

func (s *Store) latestBillingAudits() (map[int64]BillingAudit, error) {
	query := `SELECT id,upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,
		effective_multiplier,reported_list_cost,reported_actual_cost,observed_unix FROM (
			SELECT id,upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,
				effective_multiplier,reported_list_cost,reported_actual_cost,` +
		s.billingTimeExpr("observed_at") + ` AS observed_unix,
				ROW_NUMBER() OVER (PARTITION BY upstream_id ORDER BY observed_at DESC,id DESC) AS snapshot_rank
			FROM upstream_billing_snapshots
		) ranked WHERE snapshot_rank<=2 ORDER BY upstream_id,snapshot_rank`
	rows, err := s.query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byUpstream := make(map[int64][]BillingSnapshot)
	for rows.Next() {
		snapshot, err := scanBillingSnapshot(rows)
		if err != nil {
			return nil, err
		}
		byUpstream[snapshot.UpstreamID] = append(byUpstream[snapshot.UpstreamID], snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	audits := make(map[int64]BillingAudit, len(byUpstream))
	for upstreamID, snapshots := range byUpstream {
		audits[upstreamID] = s.billingAuditFromSnapshots(snapshots)
	}
	return audits, nil
}

// GetBillingStatus returns the latest state. A missing row is reported as sql.ErrNoRows.
func (s *Store) GetBillingStatus(upstreamID int64) (BillingStatus, error) {
	query := `SELECT upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,effective_multiplier,
		reported_list_cost,reported_actual_cost,status,error_text,` +
		s.billingTimeExpr("observed_at") + `,` + s.billingTimeExpr("last_success_at") + `,` +
		s.billingTimeExpr("refreshed_at") + ` FROM upstream_billing_status WHERE upstream_id=?`
	state, err := scanBillingStatus(s.queryRow(query, upstreamID))
	if err != nil {
		return BillingStatus{}, err
	}
	snapshots, err := s.ListBillingSnapshots(upstreamID, 2)
	if err != nil {
		return BillingStatus{}, err
	}
	audit := s.billingAuditFromSnapshots(snapshots)
	state.Audit = &audit
	return state, nil
}

// ListBillingStatuses returns the latest state indexed by upstream ID.
func (s *Store) ListBillingStatuses() (map[int64]BillingStatus, error) {
	return s.listBillingStatuses(false)
}

// ListBillingStatusesLite returns statuses without computing audits (faster for list views).
func (s *Store) ListBillingStatusesLite() (map[int64]BillingStatus, error) {
	return s.listBillingStatuses(true)
}

func (s *Store) listBillingStatuses(skipAudits bool) (map[int64]BillingStatus, error) {
	query := `SELECT upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,effective_multiplier,
		reported_list_cost,reported_actual_cost,status,error_text,` +
		s.billingTimeExpr("observed_at") + `,` + s.billingTimeExpr("last_success_at") + `,` +
		s.billingTimeExpr("refreshed_at") + ` FROM upstream_billing_status`
	rows, err := s.query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]BillingStatus)
	for rows.Next() {
		state, err := scanBillingStatus(rows)
		if err != nil {
			return nil, err
		}
		out[state.UpstreamID] = state
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if skipAudits {
		return out, nil
	}
	audits, err := s.latestBillingAudits()
	if err != nil {
		return nil, err
	}
	for upstreamID, state := range out {
		audit, ok := audits[upstreamID]
		if !ok {
			audit = s.billingAuditFromSnapshots(nil)
		}
		state.Audit = &audit
		out[upstreamID] = state
	}
	return out, nil
}

// ListBillingSnapshots returns newest snapshots first with a bounded page size.
func (s *Store) ListBillingSnapshots(upstreamID int64, limit int) ([]BillingSnapshot, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `SELECT id,upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,
		effective_multiplier,reported_list_cost,reported_actual_cost,` + s.billingTimeExpr("observed_at") +
		` FROM upstream_billing_snapshots WHERE upstream_id=? ORDER BY observed_at DESC,id DESC LIMIT ?`
	rows, err := s.query(query, upstreamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BillingSnapshot
	for rows.Next() {
		snapshot, err := scanBillingSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snapshot)
	}
	return out, rows.Err()
}
