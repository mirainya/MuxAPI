package health

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChannelFailuresAccumulateAcrossModels(t *testing.T) {
	m := New(3, time.Hour)
	const id = int64(1)
	m.Report(id, "gpt-5.6", false, 0)
	m.Report(id, "gpt-5.5", false, 0)
	if !m.IsAvailable(id, "gpt-5.4") {
		t.Fatal("two failures should remain below threshold")
	}
	m.Report(id, "claude-opus", false, 0)
	if m.IsAvailable(id, "gpt-5.4") {
		t.Fatal("failures from different models must open the channel")
	}
	if got := m.EffectiveState(id); got != "OPEN" {
		t.Fatalf("expected OPEN, got %s", got)
	}
}

func TestTimeoutUsesConfiguredFailureThreshold(t *testing.T) {
	m := New(3, time.Hour)
	m.ReportTimeout(1, "gpt-5.6", 120000)
	if !m.IsAvailable(1, "gpt-5.6") {
		t.Fatal("one timeout must remain below the configured threshold")
	}
	m.ReportTimeout(1, "gpt-5.6", 120000)
	m.ReportTimeout(1, "gpt-5.6", 120000)
	if m.IsAvailable(1, "gpt-5.6") {
		t.Fatal("three consecutive timeouts must open the channel")
	}
	snapshot := m.Snapshot(1)
	if snapshot.Reqs != 3 || snapshot.FailReqs != 3 {
		t.Fatalf("timeouts should count as ordinary failed requests: %+v", snapshot)
	}
}

func TestSetFailurePolicyAppliesWithoutRestart(t *testing.T) {
	m := New(3, time.Hour)
	m.Report(1, "gpt", false, 0)
	if !m.IsAvailable(1, "gpt") {
		t.Fatal("one failure should remain below the initial threshold")
	}

	m.SetFailurePolicy(1, 5*time.Millisecond)
	m.Report(2, "gpt", false, 0)
	if m.IsAvailable(2, "gpt") {
		t.Fatal("updated threshold should open a new channel after one failure")
	}
	time.Sleep(10 * time.Millisecond)
	if !m.IsAvailable(2, "gpt") {
		t.Fatal("updated cooldown should expose the channel after expiry")
	}
}

func TestClosedSuccessResetsConsecutiveFailures(t *testing.T) {
	m := New(3, time.Hour)
	m.Report(1, "a", false, 0)
	m.Report(1, "b", false, 0)
	m.Report(1, "c", true, 40)
	m.Report(1, "d", false, 0)
	if !m.IsAvailable(1, "e") {
		t.Fatal("a successful channel request should reset consecutive failures")
	}
}

func TestRecoveryRequiresTwoSuccesses(t *testing.T) {
	m := New(1, 5*time.Millisecond)
	m.Report(1, "gpt", false, 0)
	time.Sleep(10 * time.Millisecond)
	m.ObserveProbe(1, "gpt", true, 50)
	if got := m.EffectiveState(1); got != "HALF_OPEN" {
		t.Fatalf("first recovery success should enter HALF_OPEN, got %s", got)
	}
	m.ObserveProbe(1, "gpt", true, 45)
	if got := m.EffectiveState(1); got != "CLOSED" {
		t.Fatalf("second recovery success should close the channel, got %s", got)
	}
}

func TestLateInFlightSuccessDoesNotBypassOpenCooldown(t *testing.T) {
	m := New(1, time.Hour)
	m.Report(1, "failed", false, 0)
	m.Report(1, "already-running", true, 40)
	if got := m.EffectiveState(1); got != "OPEN" {
		t.Fatalf("late in-flight success bypassed cooldown: state=%s", got)
	}
	if m.IsAvailable(1, "next") {
		t.Fatal("channel must remain unavailable until controlled recovery")
	}
}

func TestRecoveryFailureReopens(t *testing.T) {
	m := New(1, 10*time.Millisecond)
	m.Report(1, "gpt", false, 0)
	time.Sleep(15 * time.Millisecond)
	m.ObserveProbe(1, "gpt", true, 50)
	m.ObserveProbe(1, "gpt", false, 0)
	if got := m.EffectiveState(1); got != "OPEN" {
		t.Fatalf("HALF_OPEN failure should reopen the channel, got %s", got)
	}
}

func TestResetCircuitPreservesStatisticsAndModelCache(t *testing.T) {
	m := New(1, time.Hour)
	m.Report(1, "gpt", false, 0)
	m.MarkModelUnsupported(1, "unsupported")
	before := m.Snapshot(1)

	m.ResetCircuit(1)

	after := m.Snapshot(1)
	if after.State != "CLOSED" || after.Fails != 0 || !after.OpenUntil.IsZero() {
		t.Fatalf("circuit was not reset: %+v", after)
	}
	if after.Reqs != before.Reqs || after.FailReqs != before.FailReqs {
		t.Fatalf("traffic statistics changed: before=%+v after=%+v", before, after)
	}
	if !m.IsModelUnsupported(1, "unsupported") {
		t.Fatal("manual circuit reset must not clear model capability cache")
	}
	if !m.IsAvailable(1, "gpt") {
		t.Fatal("reset channel should be immediately available")
	}
}

func TestHalfOpenAllowsOneClaim(t *testing.T) {
	m := New(1, 10*time.Millisecond)
	m.Report(1, "gpt", false, 0)
	time.Sleep(15 * time.Millisecond)
	if !m.IsAvailable(1, "gpt") {
		t.Fatal("cooldown expiry should expose one HALF_OPEN slot")
	}
	first, ok := m.Claim(1, 1, "gpt")
	if !ok {
		t.Fatal("first HALF_OPEN claim should pass")
	}
	if _, ok := m.Claim(1, 1, "gpt"); ok {
		t.Fatal("second concurrent HALF_OPEN claim should be blocked")
	}
	m.Release(first)
	second, ok := m.Claim(1, 1, "gpt")
	if !ok {
		t.Fatal("released HALF_OPEN slot should be reusable")
	}
	m.Release(second)
}

func TestHalfOpenConcurrentBurst(t *testing.T) {
	m := New(1, 5*time.Millisecond)
	m.Report(1, "gpt", false, 0)
	time.Sleep(10 * time.Millisecond)
	var accepted int32
	var acceptedLease Lease
	var leaseMu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lease, ok := m.Claim(1, 1, "gpt"); ok {
				atomic.AddInt32(&accepted, 1)
				leaseMu.Lock()
				acceptedLease = lease
				leaseMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("expected one HALF_OPEN claim, got %d", accepted)
	}
	m.Release(acceptedLease)
}

func TestModelUnsupportedDoesNotAffectChannel(t *testing.T) {
	m := New(1, time.Hour)
	m.MarkModelUnsupported(1, "gpt-5.6")
	if m.IsAvailable(1, "gpt-5.6") {
		t.Fatal("unsupported model should be excluded")
	}
	if !m.IsAvailable(1, "gpt-5.5") {
		t.Fatal("model capability must not affect channel health")
	}
	if got := m.EffectiveState(1); got != "CLOSED" {
		t.Fatalf("capability exclusion must not open channel, got %s", got)
	}
	states := m.ModelStates(1)
	if len(states) != 1 || states[0].Model != "gpt-5.6" || states[0].State != "UNSUPPORTED" {
		t.Fatalf("unexpected capability states: %+v", states)
	}
	m.MarkModelSupported(1, "gpt-5.6")
	if !m.IsAvailable(1, "gpt-5.6") {
		t.Fatal("supported model should be eligible again")
	}
}

func TestModelUnsupportedExpires(t *testing.T) {
	m := New(1, time.Hour)
	key := modelKey{1, "gpt"}
	expires := time.Now().Add(-time.Second)
	m.unsupported[key] = &ModelExclusion{UpstreamID: 1, Model: "gpt", ExcludedUntil: &expires}
	if !m.IsAvailable(1, "gpt") {
		t.Fatal("expired capability exclusion should be removed")
	}
}

type memoryExclusionStore struct {
	mu      sync.Mutex
	records map[modelKey]ModelExclusion
}

func newMemoryExclusionStore() *memoryExclusionStore {
	return &memoryExclusionStore{records: make(map[modelKey]ModelExclusion)}
}

func (s *memoryExclusionStore) LoadModelExclusions() ([]ModelExclusion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ModelExclusion, 0, len(s.records))
	for _, record := range s.records {
		out = append(out, record)
	}
	return out, nil
}

func (s *memoryExclusionStore) UpsertModelExclusion(record ModelExclusion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := modelKey{record.UpstreamID, record.Model}
	if previous, ok := s.records[key]; ok {
		record.FailureCount = previous.FailureCount + 1
	}
	s.records[key] = record
	return nil
}

func (s *memoryExclusionStore) DeleteModelExclusion(upstreamID int64, model string) error {
	s.mu.Lock()
	delete(s.records, modelKey{upstreamID, model})
	s.mu.Unlock()
	return nil
}

func TestPermanentModelExclusionPersistsAcrossRestartAndSuccess(t *testing.T) {
	storage := newMemoryExclusionStore()
	first := New(3, time.Hour)
	first.SetAdvancedPolicy(2, time.Hour, 0)
	if err := first.SetModelExclusionStore(storage); err != nil {
		t.Fatal(err)
	}

	lateSuccess, _ := first.Claim(1, 7, "glm-5")
	failure, _ := first.Claim(1, 7, "glm-5")
	first.CompleteModelUnsupported(failure, 12, 404, `{"error":{"code":"model_not_found"}}`)
	first.Complete(lateSuccess, ResultSuccess, 10)
	if !first.IsModelUnsupported(7, "glm-5") {
		t.Fatal("ordinary success must not clear a newer permanent exclusion")
	}

	restarted := New(3, time.Hour)
	if err := restarted.SetModelExclusionStore(storage); err != nil {
		t.Fatal(err)
	}
	if !restarted.IsModelUnsupported(7, "glm-5") || restarted.IsAvailable(7, "glm-5") {
		t.Fatal("restart did not restore permanent model exclusion")
	}
	states := restarted.ModelStates(7)
	if len(states) != 1 || states[0].Model != "glm-5" {
		t.Fatalf("unexpected restored states: %+v", states)
	}
	if err := restarted.RecoverModel(7, "glm-5"); err != nil {
		t.Fatal(err)
	}
	if restarted.IsModelUnsupported(7, "glm-5") {
		t.Fatal("manual recovery did not clear memory state")
	}
	if records, _ := storage.LoadModelExclusions(); len(records) != 0 {
		t.Fatalf("manual recovery did not clear durable state: %+v", records)
	}
}

func TestExpiredModelExclusionAllowsSingleConcurrentReprobe(t *testing.T) {
	storage := newMemoryExclusionStore()
	expires := time.Now().Add(-time.Second)
	storage.records[modelKey{9, "glm-5"}] = ModelExclusion{
		UpstreamID: 9, Model: "glm-5", ExcludedUntil: &expires,
		FailureCount: 1, LastFailedAt: time.Now().Add(-time.Minute),
	}
	m := New(3, time.Hour)
	m.SetAdvancedPolicy(2, time.Hour, time.Minute)
	if err := m.SetModelExclusionStore(storage); err != nil {
		t.Fatal(err)
	}

	var accepted int32
	var acceptedLease Lease
	var leaseMu sync.Mutex
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lease, ok := m.Claim(1, 9, "glm-5"); ok {
				atomic.AddInt32(&accepted, 1)
				leaseMu.Lock()
				acceptedLease = lease
				leaseMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("expired exclusion admitted %d concurrent re-probes, want 1", accepted)
	}
	m.Complete(acceptedLease, ResultSuccess, 10)
	if m.IsModelUnsupported(9, "glm-5") {
		t.Fatal("successful TTL re-probe did not clear exclusion")
	}
	if records, _ := storage.LoadModelExclusions(); len(records) != 0 {
		t.Fatalf("successful TTL re-probe did not clear durable row: %+v", records)
	}
}

func TestModelsDiscoveredDoesNotClearExclusion(t *testing.T) {
	m := New(3, time.Hour)
	m.MarkModelUnsupported(1, "glm-5")
	m.MarkModelsDiscovered(1, []string{"glm-5", "deepseek-v4"})
	if !m.IsModelUnsupported(1, "glm-5") {
		t.Fatal("positive model discovery cleared a negative capability record")
	}
}

func TestInFlightAccounting(t *testing.T) {
	m := New(3, time.Hour)
	first, ok1 := m.Claim(1, 1, "gpt")
	second, ok2 := m.Claim(1, 1, "gpt")
	if !ok1 || !ok2 {
		t.Fatal("closed channel claims should pass")
	}
	if got := m.InFlight(1); got != 2 {
		t.Fatalf("expected in-flight=2, got %d", got)
	}
	m.Release(first)
	m.Release(first)
	m.Release(second)
	if got := m.InFlight(1); got != 0 {
		t.Fatalf("release must be idempotent at zero, got %d", got)
	}
}

func TestLatencyEWMAUsesChannelTTFT(t *testing.T) {
	m := New(3, time.Hour)
	m.Report(1, "gpt-5.6", true, 100)
	m.Report(1, "gpt-5.5", true, 200)
	// 每个模型的成功延迟都汇入同一把渠道 EWMA，两次上报后落在 100~200 之间。
	latency := m.LatencyEWMA(1)
	if latency <= 100 || latency >= 200 {
		t.Fatalf("unexpected channel EWMA: latency=%v", latency)
	}
}

func TestTrafficStats(t *testing.T) {
	m := New(3, time.Hour)
	m.Report(1, "a", true, 100)
	m.Report(1, "b", false, 0)
	m.Report(1, "c", true, 300)
	snapshot := m.Snapshot(1)
	if snapshot.Reqs != 3 || snapshot.FailReqs != 1 {
		t.Fatalf("unexpected counters: %+v", snapshot)
	}
	if snapshot.SuccRate < 0.66 || snapshot.SuccRate > 0.67 {
		t.Fatalf("unexpected success rate: %v", snapshot.SuccRate)
	}
	if snapshot.AvgLatMs != 200 {
		t.Fatalf("unexpected average TTFT: %d", snapshot.AvgLatMs)
	}
}

func TestSeedRestoresChannelStatsOnly(t *testing.T) {
	m := New(3, time.Hour)
	m.Seed([]RouteSample{
		{UpstreamID: 1, OK: true, LatencyMs: 100},
		{UpstreamID: 1, OK: false},
		{UpstreamID: 1, OK: true, LatencyMs: 200},
	})
	if latency := m.LatencyEWMA(1); latency <= 0 {
		t.Fatalf("seed must restore channel latency EWMA, got %v", latency)
	}
	if got := m.EffectiveState(1); got != "CLOSED" {
		t.Fatalf("seed must not restore OPEN state, got %s", got)
	}
}

func TestSampleTrend(t *testing.T) {
	m := New(1, time.Hour)
	m.Report(1, "gpt", false, 0)
	m.Sample()
	snapshot := m.Snapshot(1)
	if len(snapshot.Trend) != 1 || snapshot.Trend[0].Status != statDown {
		t.Fatalf("unexpected trend: %+v", snapshot.Trend)
	}
}

func TestLeaseCompletionIsIdempotent(t *testing.T) {
	m := New(3, time.Hour)
	first, ok := m.Claim(1, 1, "gpt")
	if !ok {
		t.Fatal("first claim failed")
	}
	second, ok := m.Claim(1, 1, "gpt")
	if !ok {
		t.Fatal("second claim failed")
	}
	m.Release(first)
	m.Release(first)
	if got := m.InFlight(1); got != 1 {
		t.Fatalf("duplicate completion released another request: in-flight=%d", got)
	}
	m.Release(second)
}

func TestOldGenerationResultsCannotChangeCircuit(t *testing.T) {
	t.Run("late success cannot recover", func(t *testing.T) {
		m := New(1, time.Hour)
		failed, _ := m.Claim(1, 1, "gpt")
		lateSuccess, _ := m.Claim(1, 1, "gpt")
		m.Complete(failed, ResultFailure, 0)
		m.Complete(lateSuccess, ResultSuccess, 20)
		if got := m.EffectiveState(1); got != "OPEN" {
			t.Fatalf("late success changed a newer generation: %s", got)
		}
	})

	t.Run("late failure cannot reopen reset circuit", func(t *testing.T) {
		m := New(1, time.Hour)
		lateFailure, _ := m.Claim(1, 1, "gpt")
		m.ResetCircuit(1)
		m.Complete(lateFailure, ResultFailure, 0)
		if got := m.EffectiveState(1); got != "CLOSED" {
			t.Fatalf("late failure changed a newer generation: %s", got)
		}
	})
}

func TestRecoveryLeaseExcludesProbeAndGroupPeers(t *testing.T) {
	m := New(1, 5*time.Millisecond)
	m.Report(1, "gpt", false, 0)
	m.Report(2, "gpt", false, 0)
	time.Sleep(10 * time.Millisecond)

	first, ok := m.Claim(7, 1, "gpt")
	if !ok {
		t.Fatal("first group recovery claim failed")
	}
	if _, ok := m.Claim(7, 2, "gpt"); ok {
		t.Fatal("same group must not recover two channels concurrently")
	}
	if _, ok := m.Claim(8, 1, "gpt"); ok {
		t.Fatal("same channel must not recover concurrently across groups")
	}
	probe := m.BeginProbe(1, "gpt")
	m.Complete(probe, ResultSuccess, 10)
	if got := m.EffectiveState(1); got != "HALF_OPEN" {
		t.Fatalf("concurrent probe changed business-owned recovery: %s", got)
	}
	m.Complete(first, ResultFailure, 0)
}

func TestLateCapabilityResultCannotOverrideNewerResult(t *testing.T) {
	m := New(3, time.Hour)
	old := m.BeginProbe(1, "gpt")
	newer := m.BeginProbe(1, "gpt")
	m.Complete(newer, ResultModelUnsupported, 0)
	m.Complete(old, ResultSuccess, 10)
	if !m.IsModelUnsupported(1, "gpt") {
		t.Fatal("late success cleared a newer unsupported result")
	}

	old = m.BeginProbe(2, "gpt")
	newer = m.BeginProbe(2, "gpt")
	m.Complete(newer, ResultSuccess, 10)
	m.Complete(old, ResultModelUnsupported, 0)
	if m.IsModelUnsupported(2, "gpt") {
		t.Fatal("late unsupported result replaced a newer success")
	}
}
