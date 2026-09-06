package server

import (
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/store"
)

// healthView 运行时健康精简视图（成员/上游列表用，不含趋势数组，省带宽）。
type healthView struct {
	State     string  `json:"state"`      // CLOSED 正常 / OPEN 熔断 / HALF_OPEN 半开
	Fails     int     `json:"fails"`      // 当前连续失败数
	Reqs      int64   `json:"reqs"`       // 业务请求总数
	SuccRate  float64 `json:"succ_rate"`  // 成功率 0..1（无请求为 0）
	AvgLatMs  int64   `json:"avg_lat_ms"` // 成功请求平均延迟
	LastProbe int64   `json:"last_probe"` // 最后探测 unix 秒，0=从未探测
}

// toHealthView 把上游级快照转精简视图。state 由调用方传入「聚合后的对外状态」
// (health.EffectiveState)，而非上游级原始 State——这样「单模型上游唯一模型熔断」
// 等情形能如实显示熔断，避免运行时列与真实选路口径(IsAvailable 看模型级)错位。
func toHealthView(sn health.Snapshot, state string) healthView {
	var lastProbe int64
	if !sn.LastProbe.IsZero() {
		lastProbe = sn.LastProbe.Unix()
	}
	return healthView{
		State: state, Fails: sn.Fails, Reqs: sn.Reqs,
		SuccRate: sn.SuccRate, AvgLatMs: sn.AvgLatMs, LastProbe: lastProbe,
	}
}

// modelHealthView 模型能力排除的精简视图（上游/成员列表展开模型徽章用）。
type modelHealthView struct {
	Model         string     `json:"model"`
	State         string     `json:"state"`
	ExcludedUntil *time.Time `json:"excluded_until,omitempty"`
	FailureCount  int        `json:"failure_count"`
	LastStatus    int        `json:"last_status"`
	LastReason    string     `json:"last_reason,omitempty"`
	LastFailedAt  time.Time  `json:"last_failed_at"`
}

// toModelHealthViews exposes temporary model capability exclusions.
func toModelHealthViews(ms []health.ModelHealth) []modelHealthView {
	if len(ms) == 0 {
		return nil
	}
	out := make([]modelHealthView, 0, len(ms))
	for _, mh := range ms {
		out = append(out, modelHealthView{
			Model: mh.Model, State: mh.State, ExcludedUntil: mh.ExcludedUntil,
			FailureCount: mh.FailureCount, LastStatus: mh.LastStatus,
			LastReason: mh.LastReason, LastFailedAt: mh.LastFailedAt,
		})
	}
	return out
}

// effectivePriority 返回分组「生效层」的优先级值。
// 生效层 = enabled、未被倍率限制且未熔断渠道中优先级最小的那层。
func effectivePriority(ms []*store.Member, state func(int64) string) (int, bool) {
	best, ok := 0, false
	for _, m := range ms {
		if !m.Enabled || !m.GroupEnabled || m.MultiplierBlocked || state(m.UpstreamID) == "OPEN" {
			continue
		}
		if !ok || m.Priority < best {
			best, ok = m.Priority, true
		}
	}
	return best, ok
}

// memberOut 组成员 + 运行时健康 + 是否生效层。
type memberOut struct {
	*store.Member
	Health      healthView        `json:"health"`
	ModelHealth []modelHealthView `json:"model_health,omitempty"` // 该上游模型级健康（仅 GET 填充，无则省略）
	Effective   bool              `json:"effective"`
}

// groupRuntime 分组运行时概览：生效渠道名 + 各健康档计数（只统计 enabled 成员）。
type groupRuntime struct {
	Effective         []string `json:"effective"`          // 生效层渠道名（同层多个全列）
	Normal            int      `json:"normal"`             // CLOSED
	HalfOpen          int      `json:"half_open"`          // HALF_OPEN
	Open              int      `json:"open"`               // OPEN 熔断
	Total             int      `json:"total"`              // enabled 成员总数
	MultiplierBlocked int      `json:"multiplier_blocked"` // 被倍率上限排除
}

// groupOut 分组 + 运行时概览。
type groupOut struct {
	*store.Group
	Runtime groupRuntime `json:"runtime"`
}

// computeGroupRuntime 汇总分组当前生效层及各渠道健康数量。
func (s *Server) computeGroupRuntime(gid int64) groupRuntime {
	ms, _ := s.store.ListGroupMembers(gid)
	// 缓存各成员「聚合后对外状态」（含模型级聚合），避免重复加锁查询
	eff := make(map[int64]string, len(ms))
	for _, m := range ms {
		eff[m.UpstreamID] = s.health.EffectiveState(m.UpstreamID)
	}
	rt := groupRuntime{Effective: []string{}}
	for _, m := range ms {
		if !m.Enabled || !m.GroupEnabled {
			continue
		}
		rt.Total++
		if m.MultiplierBlocked {
			rt.MultiplierBlocked++
		}
		switch eff[m.UpstreamID] {
		case "OPEN":
			rt.Open++
		case "HALF_OPEN":
			rt.HalfOpen++
		default:
			rt.Normal++
		}
	}
	best, hasEff := effectivePriority(ms, func(id int64) string { return eff[id] })
	if hasEff {
		for _, m := range ms {
			if m.Enabled && m.GroupEnabled && !m.MultiplierBlocked && m.Priority == best && eff[m.UpstreamID] != "OPEN" {
				rt.Effective = append(rt.Effective, m.Name)
			}
		}
	}
	return rt
}
