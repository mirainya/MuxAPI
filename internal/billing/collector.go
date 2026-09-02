// Package billing reads provider-side balances, multipliers, and cumulative costs.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mirainya/muxapi/internal/upstream"
)

const maxBillingResponse = 2 << 20

var (
	ErrBillingDisabled = errors.New("billing collection is disabled for this upstream")
	// ErrRateLimited 上游 429，短期内不可能成功。调用方应指数退避而非按固定节奏死撞。
	ErrRateLimited = errors.New("billing endpoint rate limited")
)

// Result is a normalized provider billing snapshot. Nil numbers are unavailable,
// while zero values from the provider remain distinguishable and meaningful.
type Result struct {
	Currency            string
	Remaining           *float64
	Unlimited           bool
	BillingGroup        string
	GroupMultiplier     *float64
	EffectiveMultiplier *float64
	ReportedListCost    *float64
	ReportedActualCost  *float64
	ObservedAt          time.Time
	Warning             string
}

// Fetch selects the configured provider adapter. The caller owns timeout and scheduling.
func Fetch(ctx context.Context, item *upstream.Upstream) (Result, error) {
	switch item.BillingType {
	case upstream.BillingAuto:
		// Probe the two known relay billing contracts without requiring the
		// operator to identify the platform first. A failed probe is harmless;
		// only return disabled when neither contract is available.
		if result, err := fetchSub2API(ctx, item); err == nil {
			return result, nil
		}
		result, err := fetchNewAPI(ctx, item)
		if err == nil {
			return result, nil
		}
		return Result{}, fmt.Errorf("automatic billing detection failed: %w", err)
	case upstream.BillingSub2API:
		return fetchSub2API(ctx, item)
	case upstream.BillingNewAPI:
		return fetchNewAPI(ctx, item)
	default:
		return Result{}, ErrBillingDisabled
	}
}

func getJSON(ctx context.Context, item *upstream.Upstream, path string, target any) error {
	req, err := item.BuildRequest(http.MethodGet, path, nil, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)
	resp, err := (&http.Client{Transport: item.NewTransport()}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBillingResponse+1))
	if err != nil {
		return err
	}
	if len(body) > maxBillingResponse {
		return errors.New("billing response is too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests {
			return fmt.Errorf("%w: %s returned 429: %s", ErrRateLimited, path, clipMessage(string(body)))
		}
		return fmt.Errorf("billing endpoint %s returned %d: %s", path, resp.StatusCode, clipMessage(string(body)))
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode billing endpoint %s: %w", path, err)
	}
	return nil
}

// clipMessage 截断上游计费错误体。错误文本会写入 upstream_billing_status.error，
// 按字节硬切会产生非法 UTF-8 并让 Postgres 拒收整条状态更新，故按 rune 边界回退。
func clipMessage(value string) string {
	const maxBytes = 512
	value = strings.ToValidUTF8(strings.TrimSpace(value), "")
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func floatPtr(value float64) *float64 { return &value }

type sub2UsageResponse struct {
	Balance   *float64 `json:"balance"`
	Remaining *float64 `json:"remaining"`
	Unit      string   `json:"unit"`
	IsValid   bool     `json:"isValid"`
	Usage     struct {
		Total struct {
			Cost       *float64 `json:"cost"`
			ActualCost *float64 `json:"actual_cost"`
		} `json:"total"`
	} `json:"usage"`
}

type sub2BillingResponse struct {
	Object                  string  `json:"object"`
	GroupRateMultiplier     float64 `json:"group_rate_multiplier"`
	ResolvedRateMultiplier  float64 `json:"resolved_rate_multiplier"`
	EffectiveRateMultiplier float64 `json:"effective_rate_multiplier"`
}

func fetchSub2API(ctx context.Context, item *upstream.Upstream) (Result, error) {
	var usage sub2UsageResponse
	if err := getJSON(ctx, item, "/v1/usage", &usage); err != nil {
		return Result{}, err
	}
	var provider sub2BillingResponse
	if err := getJSON(ctx, item, "/v1/sub2api/billing", &provider); err != nil {
		return Result{}, err
	}
	if provider.Object != "sub2api.key_billing" {
		return Result{}, errors.New("unexpected Sub2API billing response")
	}
	remaining := usage.Remaining
	if remaining == nil {
		remaining = usage.Balance
	}
	// ObservedAt 必须用本地时钟：它同时是比对窗口的边界，而窗口内的请求是按
	// 本地写入的 completed_at 过滤的。混用上游时钟会让两侧时钟差直接变成窗口
	// 漏算/重算。上游自报的 observed_at 只作参考，不参与窗口计算。
	observed := time.Now()
	currency := strings.TrimSpace(usage.Unit)
	if currency == "" {
		currency = "USD"
	}
	return Result{
		Currency: currency, Remaining: remaining,
		GroupMultiplier:     floatPtr(provider.GroupRateMultiplier),
		EffectiveMultiplier: floatPtr(provider.EffectiveRateMultiplier),
		ReportedListCost:    usage.Usage.Total.Cost, ReportedActualCost: usage.Usage.Total.ActualCost,
		ObservedAt: observed,
	}, nil
}

type newAPIUsageResponse struct {
	Data struct {
		Object         string  `json:"object"`
		TotalUsed      float64 `json:"total_used"`
		TotalAvailable float64 `json:"total_available"`
		UnlimitedQuota bool    `json:"unlimited_quota"`
	} `json:"data"`
}

type newAPIStatusResponse struct {
	Data struct {
		QuotaPerUnit float64 `json:"quota_per_unit"`
	} `json:"data"`
}

// newAPILogResponse 是 /api/log/token 的响应，按时间倒序。它把消费日志与错误日志
// 混在一起返回：两者都带 group，但只有消费日志的 other 里有 group_ratio。
type newAPILogResponse struct {
	Success bool `json:"success"`
	Data    []struct {
		Group string          `json:"group"`
		Other json.RawMessage `json:"other"`
	} `json:"data"`
}

type newAPILogBilling struct {
	GroupRatio     *float64 `json:"group_ratio"`
	UserGroupRatio *float64 `json:"user_group_ratio"`
}

type newAPIGroupResponse struct {
	Data map[string]struct {
		Ratio json.RawMessage `json:"ratio"`
	} `json:"data"`
}

func decodeNewAPILogBilling(raw json.RawMessage, target *newAPILogBilling) bool {
	if len(raw) == 0 {
		return false
	}
	if json.Unmarshal(raw, target) == nil {
		return true
	}
	var encoded string
	return json.Unmarshal(raw, &encoded) == nil && json.Unmarshal([]byte(encoded), target) == nil
}

func fetchNewAPI(ctx context.Context, item *upstream.Upstream) (Result, error) {
	var usage newAPIUsageResponse
	if err := getJSON(ctx, item, "/api/usage/token/", &usage); err != nil {
		return Result{}, err
	}
	if usage.Data.Object != "token_usage" {
		return Result{}, errors.New("unexpected New API usage response")
	}
	var status newAPIStatusResponse
	if err := getJSON(ctx, item, "/api/status", &status); err != nil {
		return Result{}, err
	}
	if status.Data.QuotaPerUnit <= 0 {
		return Result{}, errors.New("New API returned invalid quota_per_unit")
	}

	result := Result{Currency: "USD", Unlimited: usage.Data.UnlimitedQuota, ObservedAt: time.Now()}
	if !result.Unlimited {
		remaining := math.Max(0, usage.Data.TotalAvailable/status.Data.QuotaPerUnit)
		result.Remaining = &remaining
	}
	actual := usage.Data.TotalUsed / status.Data.QuotaPerUnit
	result.ReportedActualCost = &actual

	var logs newAPILogResponse
	if err := getJSON(ctx, item, "/api/log/token", &logs); err != nil {
		result.Warning = err.Error()
		return result, nil
	}
	// 分组名取最新一条日志（错误日志也带 group，且反映当前分组归属）；
	// user_group_ratio 是**个人议价倍率**(>=0 时才是真实扣费)，取最新扣费日志里的。
	// GroupRatio(旧字段) 是历史扣费快照，站点调价后会与当前公示价不符，仅在 groups 表
	// 拿不到时兜底使用——见下方 groups 分支。
	var latestUserGroupRatio *float64
	for _, entry := range logs.Data {
		group := strings.TrimSpace(entry.Group)
		if group == "" {
			continue
		}
		if result.BillingGroup == "" {
			result.BillingGroup = group
		}
		var detail newAPILogBilling
		if !decodeNewAPILogBilling(entry.Other, &detail) || detail.GroupRatio == nil {
			continue
		}
		// 分组变更后，旧分组的扣费不能套到当前分组上。
		if group != result.BillingGroup {
			break
		}
		if detail.UserGroupRatio != nil && *detail.UserGroupRatio >= 0 {
			v := *detail.UserGroupRatio
			latestUserGroupRatio = &v
		}
		break
	}
	if result.BillingGroup == "" {
		result.Warning = "New API has no recent token log for billing group detection"
		return result, nil
	}

	// 始终查 /api/user/groups 拿当前公示价——这才是站点管理员**现在**设置的价格，
	// 与用户面板显示一致；扣费日志里的 group_ratio 是历史快照，站点调价后会失真。
	var groups newAPIGroupResponse
	if err := getJSON(ctx, item, "/api/user/groups", &groups); err != nil {
		result.Warning = err.Error()
		return result, nil
	}
	if group, ok := groups.Data[result.BillingGroup]; ok {
		var ratio float64
		if json.Unmarshal(group.Ratio, &ratio) == nil {
			result.GroupMultiplier = &ratio
			result.EffectiveMultiplier = &ratio
		}
	}
	// 有个人议价则覆盖 effective，group 仍是公示价(供审计对比)
	if latestUserGroupRatio != nil {
		result.EffectiveMultiplier = latestUserGroupRatio
	}
	if result.GroupMultiplier == nil {
		// groups 表没有该 group（罕见），回退到扣费日志的历史 group_ratio 兜底
		for _, entry := range logs.Data {
			if strings.TrimSpace(entry.Group) != result.BillingGroup {
				continue
			}
			var detail newAPILogBilling
			if !decodeNewAPILogBilling(entry.Other, &detail) || detail.GroupRatio == nil {
				continue
			}
			result.GroupMultiplier = detail.GroupRatio
			if result.EffectiveMultiplier == nil {
				result.EffectiveMultiplier = detail.GroupRatio
			}
			result.Warning = "New API billing multiplier fell back to historical log (groups endpoint missing this group)"
			break
		}
	}
	if result.EffectiveMultiplier == nil {
		result.Warning = "New API billing multiplier is unavailable"
	}
	return result, nil
}
