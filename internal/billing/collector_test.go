package billing

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mirainya/muxapi/internal/upstream"
)

func TestFetchSub2API(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("missing provider authorization")
			return
		}
		switch r.URL.Path {
		case "/v1/usage":
			w.Write([]byte(`{"balance":24.93248518,"remaining":24.93248518,"unit":"USD","isValid":true,"usage":{"total":{"cost":101.4511718,"actual_cost":15.036224541}}}`))
		case "/v1/sub2api/billing":
			w.Write([]byte(`{"object":"sub2api.key_billing","group_rate_multiplier":0.155,"resolved_rate_multiplier":0.155,"effective_rate_multiplier":0.155,"observed_at":"2026-07-24T08:18:28Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, APIKey: "sk-test", BillingType: upstream.BillingSub2API,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Remaining == nil || *result.Remaining != 24.93248518 ||
		result.EffectiveMultiplier == nil || *result.EffectiveMultiplier != 0.155 ||
		result.ReportedListCost == nil || *result.ReportedListCost != 101.4511718 ||
		result.ReportedActualCost == nil || *result.ReportedActualCost != 15.036224541 {
		t.Fatalf("unexpected Sub2API result: %+v", result)
	}
}

func TestFetchSub2APIPreservesNegativeBalance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/usage":
			w.Write([]byte(`{"balance":-1.25,"remaining":-1.25,"unit":"USD","isValid":false}`))
		case "/v1/sub2api/billing":
			w.Write([]byte(`{"object":"sub2api.key_billing","group_rate_multiplier":0.2,"effective_rate_multiplier":0.2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, APIKey: "sk-test", BillingType: upstream.BillingSub2API,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Unlimited || result.Remaining == nil || *result.Remaining != -1.25 {
		t.Fatalf("negative Sub2API balance must remain visible: %+v", result)
	}
}

// groups 表是权威公示价，log 里的 group_ratio 只是历史扣费快照。
// 站点调价后旧日志的 group_ratio 不再反映当前价，必须以 groups 为准。
func TestFetchNewAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"code":true,"data":{"object":"token_usage","total_used":2864211,"total_available":-2864211,"unlimited_quota":true}}`))
		case "/api/status":
			w.Write([]byte(`{"success":true,"data":{"quota_per_unit":500000}}`))
		case "/api/log/token":
			w.Write([]byte(`{"success":true,"data":[{"group":"OAI-PRO20X","other":"{\"group_ratio\":0.066,\"user_group_ratio\":-1}"}]}`))
		case "/api/user/groups":
			// 站点已调价到 0.22，log 里的 0.066 是历史快照
			w.Write([]byte(`{"success":true,"data":{"OAI-PRO20X":{"ratio":0.22}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, APIKey: "sk-test", BillingType: upstream.BillingNewAPI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Unlimited || result.Remaining != nil || result.BillingGroup != "OAI-PRO20X" ||
		result.EffectiveMultiplier == nil || *result.EffectiveMultiplier != 0.22 ||
		result.GroupMultiplier == nil || *result.GroupMultiplier != 0.22 ||
		result.ReportedActualCost == nil || math.Abs(*result.ReportedActualCost-5.728422) > 0.000001 {
		t.Fatalf("unexpected New API result: %+v", result)
	}
	if result.ReportedListCost != nil {
		t.Fatalf("New API does not report list cost; derived values would hide overcharge: %v", *result.ReportedListCost)
	}
}

// 关键回归：/api/log/token 混错误日志和消费日志。BillingGroup 名字仍取最新一条
// (错误也带 group)，但倍率一律走 /api/user/groups 拿当前公示价。
func TestFetchNewAPISkipsErrorLogsForGroupName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"data":{"object":"token_usage","total_used":500000,"unlimited_quota":true}}`))
		case "/api/status":
			w.Write([]byte(`{"data":{"quota_per_unit":500000}}`))
		case "/api/log/token":
			w.Write([]byte(`{"success":true,"data":[
				{"group":"OAI-PRO20X","other":"{\"error_code\":\"upstream_error\",\"status_code\":503}"},
				{"group":"OAI-PRO20X","other":"{\"group_ratio\":0.18,\"user_group_ratio\":-1}"}
			]}`))
		case "/api/user/groups":
			w.Write([]byte(`{"success":true,"data":{"OAI-PRO20X":{"ratio":0.15}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, BillingType: upstream.BillingNewAPI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BillingGroup != "OAI-PRO20X" {
		t.Fatalf("billing group: %+v", result)
	}
	// 从 groups 表拿当前价 0.15，不是 log 里的历史 0.18
	if result.EffectiveMultiplier == nil || *result.EffectiveMultiplier != 0.15 {
		t.Fatalf("effective must be current group price 0.15, got %v", result.EffectiveMultiplier)
	}
	if result.GroupMultiplier == nil || *result.GroupMultiplier != 0.15 {
		t.Fatalf("group must be current group price 0.15, got %v", result.GroupMultiplier)
	}
}

// user_group_ratio(个人议价) 存在时覆盖 effective，group_multiplier 仍是公示价。
func TestFetchNewAPIUserGroupRatioOverridesEffective(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"data":{"object":"token_usage","total_used":100,"unlimited_quota":true}}`))
		case "/api/status":
			w.Write([]byte(`{"data":{"quota_per_unit":500000}}`))
		case "/api/log/token":
			// 有效个人议价 0.08
			w.Write([]byte(`{"success":true,"data":[
				{"group":"vip","other":"{\"group_ratio\":0.22,\"user_group_ratio\":0.08}"}
			]}`))
		case "/api/user/groups":
			w.Write([]byte(`{"success":true,"data":{"vip":{"ratio":0.22}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, BillingType: upstream.BillingNewAPI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.GroupMultiplier == nil || *result.GroupMultiplier != 0.22 {
		t.Fatalf("group must reflect the public rate 0.22, got %v", result.GroupMultiplier)
	}
	if result.EffectiveMultiplier == nil || *result.EffectiveMultiplier != 0.08 {
		t.Fatalf("effective must reflect the personal discount 0.08, got %v", result.EffectiveMultiplier)
	}
}

func TestFetchNewAPISkipsErrorLogBeforePersonalRatio(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"data":{"object":"token_usage","total_used":100,"unlimited_quota":true}}`))
		case "/api/status":
			w.Write([]byte(`{"data":{"quota_per_unit":500000}}`))
		case "/api/log/token":
			w.Write([]byte(`{"success":true,"data":[
				{"group":"vip","other":"{\"error_code\":\"upstream_error\",\"status_code\":503}"},
				{"group":"vip","other":"{\"group_ratio\":0.22,\"user_group_ratio\":0.08}"}
			]}`))
		case "/api/user/groups":
			w.Write([]byte(`{"success":true,"data":{"vip":{"ratio":0.22}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, BillingType: upstream.BillingNewAPI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveMultiplier == nil || *result.EffectiveMultiplier != 0.08 {
		t.Fatalf("effective must come from the latest charged log, got %v", result.EffectiveMultiplier)
	}
}

// groups 表没有当前 group 时(罕见)，回退到扣费日志的历史 group_ratio。
func TestFetchNewAPIFallsBackWhenGroupsTableMisses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"data":{"object":"token_usage","total_used":0,"unlimited_quota":true}}`))
		case "/api/status":
			w.Write([]byte(`{"data":{"quota_per_unit":500000}}`))
		case "/api/log/token":
			w.Write([]byte(`{"success":true,"data":[
				{"group":"claude-kiro","other":"{\"group_ratio\":0.15,\"user_group_ratio\":-1}"}
			]}`))
		case "/api/user/groups":
			w.Write([]byte(`{"success":true,"data":{"other-group":{"ratio":1.0}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, BillingType: upstream.BillingNewAPI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveMultiplier == nil || *result.EffectiveMultiplier != 0.15 {
		t.Fatalf("fallback to log group_ratio, got %v", result.EffectiveMultiplier)
	}
	if result.Warning == "" {
		t.Fatal("fallback must be reported as unverified")
	}
}

func TestFetchNewAPIPartialWithoutLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"data":{"object":"token_usage","total_used":100,"total_available":250000,"unlimited_quota":false}}`))
		case "/api/status":
			w.Write([]byte(`{"data":{"quota_per_unit":500000}}`))
		case "/api/log/token":
			http.Error(w, "logs disabled", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, BillingType: upstream.BillingNewAPI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Remaining == nil || *result.Remaining != 0.5 || result.Warning == "" {
		t.Fatalf("partial New API data should retain balance: %+v", result)
	}
}
