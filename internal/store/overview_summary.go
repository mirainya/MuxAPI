package store

import (
	"database/sql"
	"errors"
	"time"
)

// OverviewCostEstimate is a USD list-price estimate for billed attempts in one window.
// Coverage shows how much of the request set had both complete Usage and known pricing.
type OverviewCostEstimate struct {
	Amount             *float64 `json:"amount,omitempty"`
	Currency           string   `json:"currency"`
	RequestCount       int64    `json:"request_count"`
	PricedRequestCount int64    `json:"priced_request_count"`
	Coverage           float64  `json:"coverage"`
	PricingSource      string   `json:"pricing_source,omitempty"`
}

// OverviewUsageCost estimates list-price spend across all upstream attempts.
// It intentionally follows the same success/partial and cache accounting rules as billing audits.
func (s *Store) OverviewUsageCost(fromAt, toAt int64) (OverviewCostEstimate, error) {
	estimate := OverviewCostEstimate{Currency: "USD"}
	status, err := s.GetPricingCatalogStatus()
	if err == nil {
		estimate.PricingSource = status.Source
	} else if !errors.Is(err, sql.ErrNoRows) {
		return estimate, err
	}

	protocolExpr := `COALESCE(NULLIF(a.protocol,''),u.protocol,'')`
	incompleteExpr := `CASE WHEN (a.input_tokens=0 AND a.output_tokens=0 AND a.cached_tokens=0
			AND a.cache_creation_tokens=0) OR (a.outcome<>'success' AND a.output_tokens=0)
		THEN 1 ELSE 0 END`
	query := `SELECT r.model,` + protocolExpr + `,COUNT(*),
		COALESCE(SUM(` + incompleteExpr + `),0),COALESCE(SUM(a.input_tokens),0),
		COALESCE(SUM(a.output_tokens),0),COALESCE(SUM(a.cached_tokens),0),
		COALESCE(SUM(a.cache_creation_tokens),0)
		FROM request_attempts a
		JOIN requests r ON r.request_id=a.request_id
		LEFT JOIN upstreams u ON u.id=a.upstream_id
		WHERE a.outcome IN ('success','partial') AND a.completed_at>? AND a.completed_at<=?
		GROUP BY r.model,` + protocolExpr + ` ORDER BY r.model`
	rows, err := s.query(query, s.timeValue(time.Unix(fromAt, 0)), s.timeValue(time.Unix(toAt, 0)))
	if err != nil {
		return estimate, err
	}

	var usages []BillingWindowUsage
	for rows.Next() {
		var usage BillingWindowUsage
		if err := rows.Scan(&usage.Model, &usage.Protocol, &usage.RequestCount, &usage.MissingUsageCount,
			&usage.InputTokens, &usage.OutputTokens, &usage.CachedTokens,
			&usage.CacheCreationTokens); err != nil {
			rows.Close()
			return estimate, err
		}
		usages = append(usages, usage)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return estimate, err
	}
	if err := rows.Close(); err != nil {
		return estimate, err
	}

	// SQLite tests use a single connection, so release the aggregate cursor
	// before loading the complete price set.
	models := make([]string, 0, len(usages))
	for _, usage := range usages {
		models = append(models, usage.Model)
	}
	prices, err := s.listModelPricing(models)
	if err != nil {
		return estimate, err
	}
	var amount float64
	for _, usage := range usages {
		estimate.RequestCount += usage.RequestCount
		eligible := usage.RequestCount - usage.MissingUsageCount
		if eligible <= 0 {
			continue
		}
		price, found := lookupModelPricing(prices, usage.Model)
		if !found {
			continue
		}
		cost, complete := usageListCost(usage, price)
		if !complete {
			continue
		}
		amount += cost
		estimate.PricedRequestCount += eligible
	}
	if estimate.RequestCount > 0 {
		estimate.Coverage = float64(estimate.PricedRequestCount) / float64(estimate.RequestCount)
	}
	if estimate.PricedRequestCount > 0 {
		estimate.Amount = &amount
	}
	return estimate, nil
}
