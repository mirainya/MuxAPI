package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// TestManagerRateLimitPoolBackoff：同 base_url 的所有上游共享冷却。
// 生产实测：aws0/aws0ex/aws0sushua 都在 us.oojj.top，一把 key 429 后剩下的 10 分钟也白打。
func TestManagerRateLimitPoolBackoff(t *testing.T) {
	var hits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/usage/token/" {
			hits.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.NotFound(w, r)
	}))
	defer provider.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "backoff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 同 base_url 造三把 key（模拟 aws0/aws0ex/aws0sushua）
	for _, name := range []string{"k1", "k2", "k3"} {
		u := &upstream.Upstream{
			Name: name, BaseURL: provider.URL, APIKey: "sk-" + name,
			BillingType: upstream.BillingNewAPI, Enabled: true,
		}
		if err := st.Create(u); err != nil {
			t.Fatal(err)
		}
	}

	m := NewManager(st)
	// 先撞一把 429 建冷却
	first, _ := st.Get(1)
	_, err = m.Refresh(context.Background(), first.ID)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("first refresh: want ErrRateLimited, got %v", err)
	}
	if !m.inCoolDown(first) {
		t.Fatal("manual Refresh should put the upstream in cool-down")
	}

	// 另外两把 key 应共享冷却，直接跳过
	k2, _ := st.Get(2)
	k3, _ := st.Get(3)
	if !m.inCoolDown(k2) || !m.inCoolDown(k3) {
		t.Fatalf("pool-mates should share cool-down: k2=%v k3=%v", m.inCoolDown(k2), m.inCoolDown(k3))
	}

	// RefreshAll 应完全跳过整池
	hitsBefore := hits.Load()
	m.RefreshAll(context.Background())
	if got := hits.Load() - hitsBefore; got != 0 {
		t.Fatalf("RefreshAll should skip cool-down pool, upstream hit %d times", got)
	}
}

// TestPoolKeyNormalization：base_url 的大小写/末尾斜杠差异不应拆池。
func TestPoolKeyNormalization(t *testing.T) {
	cases := []struct{ a, b string }{
		{"https://us.oojj.top", "https://us.oojj.top/"},
		{"https://US.OOJJ.TOP", "https://us.oojj.top"},
		{"  https://us.oojj.top  ", "https://us.oojj.top"},
	}
	for _, c := range cases {
		x := poolKey(&upstream.Upstream{BaseURL: c.a})
		y := poolKey(&upstream.Upstream{BaseURL: c.b})
		if x != y {
			t.Errorf("poolKey(%q)=%q ≠ poolKey(%q)=%q", c.a, x, c.b, y)
		}
	}
}

// TestPoolKeyIsolatesDifferentHosts：不同域名不共享冷却。
func TestPoolKeyIsolatesDifferentHosts(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "isolate.db"))
	defer st.Close()
	m := NewManager(st)
	u1 := &upstream.Upstream{ID: 1, BaseURL: "https://us.oojj.top"}
	u2 := &upstream.Upstream{ID: 2, BaseURL: "https://oojj.top"}
	m.markRateLimited(u1)
	if m.inCoolDown(u2) {
		t.Fatal("different base_url should not share cool-down")
	}
	if !m.inCoolDown(u1) {
		t.Fatal("same base_url should retain cool-down")
	}
}

// TestCoolDownExpires：冷却时间过后，允许再次尝试。
func TestCoolDownExpires(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "expire.db"))
	defer st.Close()
	m := NewManager(st)
	m.coolDownMu.Lock()
	m.coolDown["https://foo"] = time.Now().Add(-time.Minute)
	m.coolDownMu.Unlock()
	if m.inCoolDown(&upstream.Upstream{BaseURL: "https://foo"}) {
		t.Fatal("expired cool-down should not block")
	}
	m.coolDownMu.Lock()
	_, still := m.coolDown["https://foo"]
	m.coolDownMu.Unlock()
	if still {
		t.Fatal("inCoolDown should clean up expired entries")
	}
}

// TestStalenessOrder：staleness 排序键取 last_success_at；从未成功过=0=最高优。
func TestStalenessOrder(t *testing.T) {
	statuses := map[int64]store.BillingStatus{
		10: {UpstreamID: 10, LastSuccessAt: 1000}, // 老
		20: {UpstreamID: 20, LastSuccessAt: 3000}, // 新
		// upstream 30 没记录 → 视作 0，最需要刷
	}
	if !(staleness(statuses, 30) < staleness(statuses, 10) &&
		staleness(statuses, 10) < staleness(statuses, 20)) {
		t.Fatalf("staleness order wrong: 30=%d 10=%d 20=%d",
			staleness(statuses, 30), staleness(statuses, 10), staleness(statuses, 20))
	}
}
