package store

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// OverviewTrendWindow defines the display density for one dashboard range.
// 24h uses hourly data; longer ranges use fewer, wider buckets to keep the chart readable.
type OverviewTrendWindow struct {
	Key      string
	Duration time.Duration
	Bucket   time.Duration
}

var overviewTrendWindows = []OverviewTrendWindow{
	{Key: "24h", Duration: 24 * time.Hour, Bucket: time.Hour},
	{Key: "7d", Duration: 7 * 24 * time.Hour, Bucket: 6 * time.Hour},
	{Key: "30d", Duration: 30 * 24 * time.Hour, Bucket: 24 * time.Hour},
}

// LookupOverviewTrendWindow limits the admin view to the snapshot retention range.
func LookupOverviewTrendWindow(key string) OverviewTrendWindow {
	for _, window := range overviewTrendWindows {
		if window.Key == key {
			return window
		}
	}
	return overviewTrendWindows[1]
}

// OverviewBalancePoint is the known paid-value balance at one point in time.
// Provider credits are divided by the upstream credit ratio before aggregation.
// Remaining stays nil until there is a successful billing snapshot for that currency.
type OverviewBalancePoint struct {
	Ts            int64    `json:"ts"`
	Remaining     *float64 `json:"remaining"`
	UpstreamCount int      `json:"upstream_count"`
}

// OverviewBalanceSeries keeps currencies separate: summing USD and credits would be misleading.
type OverviewBalanceSeries struct {
	Currency string                 `json:"currency"`
	Points   []OverviewBalancePoint `json:"points"`
}

// OverviewUpstreamBalanceSeries is one channel's credit-ratio-adjusted balance history.
type OverviewUpstreamBalanceSeries struct {
	UpstreamID int64                  `json:"upstream_id"`
	Name       string                 `json:"name"`
	Currency   string                 `json:"currency"`
	Unlimited  bool                   `json:"unlimited"`
	Points     []OverviewBalancePoint `json:"points"`
}

type OverviewSuccessPoint struct {
	Ts      int64    `json:"ts"`
	Total   int64    `json:"total"`
	Success int64    `json:"success"`
	Rate    *float64 `json:"rate"`
}

// OverviewTrends is the compact data source for the admin home page.
// Group scope applies to enabled group members; shared upstreams are selected only once.
type OverviewTrends struct {
	Window           string                          `json:"window"`
	GroupID          int64                           `json:"group_id"`
	TagID            int64                           `json:"tag_id"`
	FromAt           int64                           `json:"from_at"`
	ToAt             int64                           `json:"to_at"`
	BucketSeconds    int64                           `json:"bucket_seconds"`
	UpstreamCount    int                             `json:"upstream_count"`
	UnlimitedCount   int                             `json:"unlimited_count"`
	Balances         []OverviewBalanceSeries         `json:"balances"`
	UpstreamBalances []OverviewUpstreamBalanceSeries `json:"upstream_balances"`
	Success          []OverviewSuccessPoint          `json:"success"`
}

type overviewBillingUpstream struct {
	ID          int64
	Name        string
	CreditRatio float64
}

// OverviewTrends builds two charts with one scope: available balance and request success rate.
func (s *Store) OverviewTrends(groupID int64, window OverviewTrendWindow, now time.Time) (*OverviewTrends, error) {
	return s.overviewTrends(groupID, 0, window, now)
}

// OverviewTrendsByTag builds the same charts for one primary upstream tag.
func (s *Store) OverviewTrendsByTag(tagID int64, window OverviewTrendWindow, now time.Time) (*OverviewTrends, error) {
	return s.overviewTrends(0, tagID, window, now)
}

func (s *Store) overviewTrends(groupID, tagID int64, window OverviewTrendWindow, now time.Time) (*OverviewTrends, error) {
	if groupID < 0 || tagID < 0 {
		return nil, fmt.Errorf("scope id must not be negative")
	}
	if window.Bucket <= 0 || window.Duration < window.Bucket {
		return nil, fmt.Errorf("invalid overview trend window")
	}

	end := now.Truncate(window.Bucket)
	start := end.Add(-window.Duration)
	pointCount := int(window.Duration/window.Bucket) + 1
	billingUpstreams, err := s.overviewBillingUpstreams(groupID, tagID)
	if err != nil {
		return nil, err
	}
	upstreamIDs := make([]int64, 0, len(billingUpstreams))
	for _, item := range billingUpstreams {
		upstreamIDs = append(upstreamIDs, item.ID)
	}
	// 两个趋势查询互不依赖：并行执行可减少远程数据库往返的串行等待。
	var (
		snapshots               []BillingSnapshot
		success                 []OverviewSuccessPoint
		snapshotErr, successErr error
		wg                      sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		snapshots, snapshotErr = s.overviewBillingSnapshots(upstreamIDs, start, end)
	}()
	go func() {
		defer wg.Done()
		success, successErr = s.overviewSuccessTrend(groupID, tagID, start, pointCount, window.Bucket)
	}()
	wg.Wait()
	if snapshotErr != nil {
		return nil, snapshotErr
	}
	if successErr != nil {
		return nil, successErr
	}
	balances, unlimited := aggregateOverviewBalances(snapshots, billingUpstreams, start, pointCount, window.Bucket)
	upstreamBalances := aggregateOverviewUpstreamBalances(snapshots, billingUpstreams, start, pointCount, window.Bucket)
	return &OverviewTrends{
		Window: window.Key, GroupID: groupID, TagID: tagID, FromAt: start.Unix(), ToAt: end.Unix(),
		BucketSeconds: int64(window.Bucket / time.Second), UpstreamCount: len(upstreamIDs),
		UnlimitedCount: unlimited, Balances: balances, UpstreamBalances: upstreamBalances, Success: success,
	}, nil
}

func (s *Store) overviewBillingUpstreams(groupID, tagID int64) ([]overviewBillingUpstream, error) {
	query := `SELECT DISTINCT u.id,u.name,u.credit_ratio FROM upstreams u`
	args := []any{}
	if groupID > 0 {
		query += ` JOIN group_upstreams gu ON gu.upstream_id=u.id`
	}
	if tagID > 0 {
		query += ` JOIN upstream_tags ut ON ut.upstream_id=u.id AND ut.tag_id=? AND ut.is_primary=TRUE`
		args = append(args, tagID)
	}
	query += ` WHERE u.enabled=TRUE AND u.billing_type<>'' AND u.billing_type<>'none'`
	if groupID > 0 {
		query += ` AND gu.group_id=? AND gu.enabled=TRUE`
		args = append(args, groupID)
	}
	query += ` ORDER BY u.id`
	rows, err := s.query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	upstreams := []overviewBillingUpstream{}
	for rows.Next() {
		var item overviewBillingUpstream
		if err := rows.Scan(&item.ID, &item.Name, &item.CreditRatio); err != nil {
			return nil, err
		}
		upstreams = append(upstreams, item)
	}
	return upstreams, rows.Err()
}

func (s *Store) overviewBillingSnapshots(upstreamIDs []int64, start, end time.Time) ([]BillingSnapshot, error) {
	if len(upstreamIDs) == 0 {
		return []BillingSnapshot{}, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(upstreamIDs)), ",")
	// 只取窗口内的快照，以及窗口起点之前每个渠道最后一条快照。
	// 后者用于在窗口第一格延续余额，避免读取整张历史表。
	columns := `id,upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,
		effective_multiplier,reported_list_cost,reported_actual_cost,` + s.billingTimeExpr("observed_at") + ` AS observed_at`
	query := `WITH before_window AS (
		SELECT ` + columns + `,
			ROW_NUMBER() OVER (PARTITION BY upstream_id ORDER BY observed_at DESC,id DESC) AS snapshot_rank
		FROM upstream_billing_snapshots
		WHERE upstream_id IN (` + placeholders + `) AND observed_at<=?
	), selected AS (
		SELECT id,upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,
			effective_multiplier,reported_list_cost,reported_actual_cost,observed_at
		FROM before_window WHERE snapshot_rank=1
		UNION ALL
		SELECT ` + columns + `
		FROM upstream_billing_snapshots
		WHERE upstream_id IN (` + placeholders + `) AND observed_at>? AND observed_at<=?
	)
	SELECT id,upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,
		effective_multiplier,reported_list_cost,reported_actual_cost,observed_at
	FROM selected ORDER BY observed_at ASC,id ASC`
	queryArgs := make([]any, 0, len(upstreamIDs)*2+2)
	for _, id := range upstreamIDs {
		queryArgs = append(queryArgs, id)
	}
	queryArgs = append(queryArgs, s.timeValue(start))
	for _, id := range upstreamIDs {
		queryArgs = append(queryArgs, id)
	}
	queryArgs = append(queryArgs, s.timeValue(start), s.timeValue(end))
	rows, err := s.query(query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	snapshots := []BillingSnapshot{}
	for rows.Next() {
		snapshot, err := scanBillingSnapshot(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

func aggregateOverviewBalances(snapshots []BillingSnapshot, upstreams []overviewBillingUpstream, start time.Time, pointCount int, bucket time.Duration) ([]OverviewBalanceSeries, int) {
	creditRatios := overviewCreditRatios(upstreams)
	currencySet := make(map[string]struct{})
	for _, snapshot := range snapshots {
		currency := snapshot.Currency
		if currency == "" {
			currency = "USD"
		}
		currencySet[currency] = struct{}{}
	}
	currencies := make([]string, 0, len(currencySet))
	for currency := range currencySet {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	if len(currencies) == 0 {
		return []OverviewBalanceSeries{}, 0
	}

	series := make(map[string]*OverviewBalanceSeries, len(currencies))
	for _, currency := range currencies {
		series[currency] = &OverviewBalanceSeries{Currency: currency, Points: make([]OverviewBalancePoint, 0, pointCount)}
	}
	latest := make(map[int64]BillingSnapshot)
	nextSnapshot := 0
	unlimited := 0
	for pointIndex := 0; pointIndex < pointCount; pointIndex++ {
		at := start.Add(time.Duration(pointIndex) * bucket).Unix()
		for nextSnapshot < len(snapshots) && snapshots[nextSnapshot].ObservedAt <= at {
			latest[snapshots[nextSnapshot].UpstreamID] = snapshots[nextSnapshot]
			nextSnapshot++
		}
		totals := make(map[string]float64, len(currencies))
		counts := make(map[string]int, len(currencies))
		unlimited = 0
		for _, snapshot := range latest {
			currency := snapshot.Currency
			if currency == "" {
				currency = "USD"
			}
			if snapshot.Unlimited {
				unlimited++
				continue
			}
			if snapshot.Remaining == nil {
				continue
			}
			totals[currency] += overviewBalanceValue(*snapshot.Remaining, creditRatios[snapshot.UpstreamID])
			counts[currency]++
		}
		for _, currency := range currencies {
			point := OverviewBalancePoint{Ts: at, UpstreamCount: counts[currency]}
			if point.UpstreamCount > 0 {
				value := totals[currency]
				point.Remaining = &value
			}
			series[currency].Points = append(series[currency].Points, point)
		}
	}
	out := make([]OverviewBalanceSeries, 0, len(currencies))
	for _, currency := range currencies {
		out = append(out, *series[currency])
	}
	return out, unlimited
}

func aggregateOverviewUpstreamBalances(snapshots []BillingSnapshot, upstreams []overviewBillingUpstream, start time.Time, pointCount int, bucket time.Duration) []OverviewUpstreamBalanceSeries {
	if len(snapshots) == 0 || len(upstreams) == 0 {
		return []OverviewUpstreamBalanceSeries{}
	}
	creditRatios := overviewCreditRatios(upstreams)
	seen := make(map[int64]bool, len(snapshots))
	latestByUpstream := make(map[int64]BillingSnapshot, len(upstreams))
	for _, snapshot := range snapshots {
		seen[snapshot.UpstreamID] = true
		latestByUpstream[snapshot.UpstreamID] = snapshot
	}
	series := make([]OverviewUpstreamBalanceSeries, 0, len(upstreams))
	for _, upstream := range upstreams {
		if !seen[upstream.ID] {
			continue
		}
		latest := latestByUpstream[upstream.ID]
		currency := latest.Currency
		if currency == "" {
			currency = "USD"
		}
		item := OverviewUpstreamBalanceSeries{
			UpstreamID: upstream.ID, Name: upstream.Name, Currency: currency,
			Unlimited: latest.Unlimited, Points: make([]OverviewBalancePoint, 0, pointCount),
		}
		current := make(map[int64]BillingSnapshot, len(upstreams))
		nextSnapshot := 0
		for pointIndex := 0; pointIndex < pointCount; pointIndex++ {
			at := start.Add(time.Duration(pointIndex) * bucket).Unix()
			for nextSnapshot < len(snapshots) && snapshots[nextSnapshot].ObservedAt <= at {
				current[snapshots[nextSnapshot].UpstreamID] = snapshots[nextSnapshot]
				nextSnapshot++
			}
			point := OverviewBalancePoint{Ts: at}
			if snapshot, ok := current[upstream.ID]; ok && !snapshot.Unlimited && snapshot.Remaining != nil {
				value := overviewBalanceValue(*snapshot.Remaining, creditRatios[upstream.ID])
				point.Remaining = &value
			}
			item.Points = append(item.Points, point)
		}
		series = append(series, item)
	}
	return series
}

func overviewCreditRatios(upstreams []overviewBillingUpstream) map[int64]float64 {
	ratios := make(map[int64]float64, len(upstreams))
	for _, item := range upstreams {
		ratios[item.ID] = item.CreditRatio
	}
	return ratios
}

func overviewBalanceValue(remaining, creditRatio float64) float64 {
	if creditRatio <= 0 {
		creditRatio = 1
	}
	return remaining / creditRatio
}

func (s *Store) overviewSuccessTrend(groupID, tagID int64, start time.Time, pointCount int, bucket time.Duration) ([]OverviewSuccessPoint, error) {
	type aggregate struct{ total, success int64 }
	buckets := make(map[int64]aggregate, pointCount)
	seconds := int64(bucket / time.Second)
	query := fmt.Sprintf(`SELECT %s AS bucket,COUNT(*),COALESCE(SUM(CASE WHEN outcome='success' THEN 1 ELSE 0 END),0)
		FROM requests WHERE created_at>=? AND created_at<=?`, s.bucketExpr("created_at", seconds))
	args := []any{s.timeValue(start), s.timeValue(start.Add(time.Duration(pointCount-1) * bucket))}
	if groupID > 0 {
		query += ` AND group_id=?`
		args = append(args, groupID)
	}
	if tagID > 0 {
		query += ` AND final_upstream_id IN (SELECT upstream_id FROM upstream_tags WHERE tag_id=? AND is_primary=TRUE)`
		args = append(args, tagID)
	}
	query += ` GROUP BY bucket`
	rows, err := s.query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var value aggregate
		if err := rows.Scan(&ts, &value.total, &value.success); err != nil {
			return nil, err
		}
		buckets[ts] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]OverviewSuccessPoint, pointCount)
	for pointIndex := range out {
		ts := start.Add(time.Duration(pointIndex) * bucket).Unix()
		point := OverviewSuccessPoint{Ts: ts}
		if value := buckets[ts]; value.total > 0 {
			point.Total, point.Success = value.total, value.success
			rate := float64(value.success) / float64(value.total)
			point.Rate = &rate
		}
		out[pointIndex] = point
	}
	return out, nil
}
