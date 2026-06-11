package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/bandit"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

// newTestBanditSelector builds a *bandit.Selector with the configured
// exploration coefficient and registers the standard sonnet/opus/haiku arms.
// Mirrors the arm registration done by daemon/phase_c_manager.go in production
// so that warm-up tests can exercise the selector's PullCounts() invariants.
func newTestBanditSelector(t *testing.T, cfg model.BanditConfig) *bandit.Selector {
	t.Helper()
	sel, err := bandit.NewSelector(cfg.EffectiveExplorationCoeff())
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	for _, arm := range []string{"sonnet", "opus", "haiku"} {
		sel.AddArm(arm)
	}
	return sel
}

// TestBanditModelSelector_DisabledReturnsEmpty verifies that a disabled
// selector returns "" so AssignWorkers keeps its static
// GetModelForBloomLevel mapping. The empty-string contract is the daemon
// side's substitute for the static fallback that the deleted
// plan.AdaptiveModelSelector used to perform internally.
func TestBanditModelSelector_DisabledReturnsEmpty(t *testing.T) {
	cfg := model.BanditConfig{Enabled: ptr.Bool(false)}
	sel := newBanditModelSelector(nil, cfg, nil)
	if sel != nil {
		t.Fatalf("disabled selector should be nil, got %v", sel)
	}

	// Calling SelectModel on a nil pointer must be safe and return "".
	if got := sel.SelectModel(2, "implement"); got != "" {
		t.Errorf("nil selector SelectModel = %q, want empty", got)
	}
}

// TestBanditModelSelector_NilBanditReturnsEmpty covers the case where the
// constructor is called with a nil *bandit.Selector — a defensive guard for
// callers that forget to wire phaseC.BanditSelector.
func TestBanditModelSelector_NilBanditReturnsEmpty(t *testing.T) {
	cfg := model.BanditConfig{Enabled: ptr.Bool(true)}
	sel := newBanditModelSelector(nil, cfg, nil)
	if sel != nil {
		t.Errorf("nil bandit selector should produce nil wrapper, got %v", sel)
	}
}

// TestBanditModelSelector_InsufficientTraceReturnsEmpty exercises §5-7
// TraceDataRequirement: total pulls across arms must reach the threshold
// before the selector exposes its UCB1 pick.
func TestBanditModelSelector_InsufficientTraceReturnsEmpty(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(1.41),
		MinSamplesBeforeUse:  ptr.Int(2),
		TraceDataRequirement: ptr.Int(10),
	}
	bs := newTestBanditSelector(t, cfg)
	sel := newBanditModelSelector(bs, cfg, nil)

	for i := 0; i < 3; i++ {
		sel.RecordResult("sonnet", 0, 1.0)
		sel.RecordResult("opus", 0, 0.8)
	}
	// total pulls = 6 < TraceDataRequirement(10) → "" (static fallback applies in plan)
	if got := sel.SelectModel(2, "implement"); got != "" {
		t.Errorf("insufficient trace data: SelectModel = %q, want empty", got)
	}
}

// TestBanditModelSelector_InsufficientPerArmReturnsEmpty exercises
// MinSamplesBeforeUse: every registered arm must have at least the per-arm
// sample threshold before the selector exposes its UCB1 pick.
func TestBanditModelSelector_InsufficientPerArmReturnsEmpty(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(1.41),
		MinSamplesBeforeUse:  ptr.Int(5),
		TraceDataRequirement: ptr.Int(3),
	}
	bs := newTestBanditSelector(t, cfg)
	sel := newBanditModelSelector(bs, cfg, nil)

	for i := 0; i < 5; i++ {
		sel.RecordResult("sonnet", 0, 1.0)
		sel.RecordResult("opus", 0, 0.8)
	}
	// haiku has 0 < MinSamplesBeforeUse(5) → "" (static fallback)
	if got := sel.SelectModel(2, "implement"); got != "" {
		t.Errorf("insufficient per-arm samples: SelectModel = %q, want empty", got)
	}
}

// TestBanditModelSelector_SufficientDataReturnsArm verifies that once the
// warm-up gates pass, the selector returns the UCB1 arm (deterministic with
// ExplorationCoeff=0 → pure exploitation of the highest average reward).
func TestBanditModelSelector_SufficientDataReturnsArm(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(0.0),
		MinSamplesBeforeUse:  ptr.Int(2),
		TraceDataRequirement: ptr.Int(6),
	}
	bs := newTestBanditSelector(t, cfg)
	sel := newBanditModelSelector(bs, cfg, nil)

	for i := 0; i < 5; i++ {
		sel.RecordResult("sonnet", 0, 1.0)
		sel.RecordResult("opus", 0, 0.5)
		sel.RecordResult("haiku", 0, 0.1)
	}
	// total=15 >= 6, each arm has 5 >= 2, exploration=0 → exploit best (sonnet)
	if got := sel.SelectModel(2, "implement"); got != "sonnet" {
		t.Errorf("warmed-up selector: SelectModel = %q, want %q", got, "sonnet")
	}
}

// TestBanditModelSelector_RecordResultUpdatesStats verifies that
// RecordResult forwards rewards to the underlying bandit so subsequent
// SelectArm calls can act on them.
func TestBanditModelSelector_RecordResultUpdatesStats(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:          ptr.Bool(true),
		ExplorationCoeff: ptr.Float64(1.41),
	}
	bs := newTestBanditSelector(t, cfg)
	sel := newBanditModelSelector(bs, cfg, nil)

	sel.RecordResult("sonnet", 0, 1.0)
	sel.RecordResult("sonnet", 0, 0.6)

	stats := bs.GetStats()
	arm, ok := stats["sonnet"]
	if !ok {
		t.Fatal("sonnet arm missing from stats")
	}
	if arm.PullCount != 2 {
		t.Errorf("PullCount = %d, want 2", arm.PullCount)
	}
	if arm.AvgReward < 0.79 || arm.AvgReward > 0.81 {
		t.Errorf("AvgReward = %f, want ~0.8", arm.AvgReward)
	}
}

// TestBanditModelSelector_RecordResult_Disabled is the safety contract: a
// disabled selector must accept reward updates as no-ops without panicking
// (the result_handler always reports rewards regardless of selector state).
func TestBanditModelSelector_RecordResult_Disabled(t *testing.T) {
	cfg := model.BanditConfig{Enabled: ptr.Bool(false)}
	sel := newBanditModelSelector(nil, cfg, nil)

	// Must be safe even when sel is nil (disabled path).
	sel.RecordResult("sonnet", 0, 1.0)
}

// TestBanditModelSelector_RecordResult_EmptyModelName exercises the
// defensive guard that suppresses reward updates for an empty model name —
// this prevents stat pollution when the dispatch layer falls back to "" for
// an unassigned bandit decision.
func TestBanditModelSelector_RecordResult_EmptyModelName(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:          ptr.Bool(true),
		ExplorationCoeff: ptr.Float64(1.41),
	}
	bs := newTestBanditSelector(t, cfg)
	sel := newBanditModelSelector(bs, cfg, nil)

	sel.RecordResult("", 0, 1.0)

	for arm, stats := range bs.GetStats() {
		if stats.PullCount != 0 {
			t.Errorf("empty model name leaked into arm %q (PullCount=%d)", arm, stats.PullCount)
		}
	}
}

// TestBanditModelSelector_SelectArmErrorReturnsEmpty exercises the safety
// path where bandit.SelectArm fails (no arms registered). With zero warm-up
// thresholds both the bucket and the global selector pass warm-up vacuously,
// so both SelectArm calls fail; the selector must return "" so AssignWorkers'
// static fallback applies, never propagate the error.
func TestBanditModelSelector_SelectArmErrorReturnsEmpty(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(1.41),
		MinSamplesBeforeUse:  ptr.Int(0),
		TraceDataRequirement: ptr.Int(0),
	}
	// Arm-less global selector → buckets replicate the empty arm set, so
	// SelectArm errors on both the bucket and the global path.
	bs, err := bandit.NewSelector(cfg.EffectiveExplorationCoeff())
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	sel := newBanditModelSelector(bs, cfg, nil)

	if got := sel.SelectModel(5, "implement"); got != "" {
		t.Errorf("SelectArm error path: SelectModel = %q, want empty", got)
	}
}

// TestBloomBucket pins the Bloom level → difficulty bucket mapping,
// including the out-of-range levels that must degrade to global-only
// handling.
func TestBloomBucket(t *testing.T) {
	cases := []struct {
		level int
		want  int
	}{
		{-1, bloomBucketInvalid},
		{0, bloomBucketInvalid},
		{1, bloomBucketLow},
		{2, bloomBucketLow},
		{3, bloomBucketMid},
		{4, bloomBucketMid},
		{5, bloomBucketHigh},
		{6, bloomBucketHigh},
		{7, bloomBucketInvalid},
	}
	for _, c := range cases {
		if got := bloomBucket(c.level); got != c.want {
			t.Errorf("bloomBucket(%d) = %d, want %d", c.level, got, c.want)
		}
	}
}

// TestBanditModelSelector_RewardRoutesToBucketAndGlobal verifies the
// contextual reward fan-out: a reward with a bucketed Bloom level updates
// the global selector and exactly the matching bucket.
func TestBanditModelSelector_RewardRoutesToBucketAndGlobal(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:          ptr.Bool(true),
		ExplorationCoeff: ptr.Float64(1.41),
	}
	bs := newTestBanditSelector(t, cfg)
	sel := newBanditModelSelector(bs, cfg, nil)

	sel.RecordResult("sonnet", 3, 1.0)

	if got := bs.GetStats()["sonnet"].PullCount; got != 1 {
		t.Errorf("global sonnet PullCount = %d, want 1", got)
	}
	if got := sel.buckets[bloomBucketMid].GetStats()["sonnet"].PullCount; got != 1 {
		t.Errorf("mid bucket sonnet PullCount = %d, want 1", got)
	}
	for _, b := range []int{bloomBucketLow, bloomBucketHigh} {
		if got := sel.buckets[b].GetStats()["sonnet"].PullCount; got != 0 {
			t.Errorf("bucket %d sonnet PullCount = %d, want 0 (reward leaked across buckets)", b, got)
		}
	}
}

// TestBanditModelSelector_OutOfRangeBloomUpdatesGlobalOnly verifies the
// degradation path for unknown Bloom levels (0 = lost dispatch record,
// >6 = malformed): the global selector still learns, no bucket does.
func TestBanditModelSelector_OutOfRangeBloomUpdatesGlobalOnly(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:          ptr.Bool(true),
		ExplorationCoeff: ptr.Float64(1.41),
	}
	bs := newTestBanditSelector(t, cfg)
	sel := newBanditModelSelector(bs, cfg, nil)

	sel.RecordResult("sonnet", 0, 1.0)
	sel.RecordResult("sonnet", 7, 1.0)

	if got := bs.GetStats()["sonnet"].PullCount; got != 2 {
		t.Errorf("global sonnet PullCount = %d, want 2", got)
	}
	for b, bucket := range sel.buckets {
		if got := bucket.GetStats()["sonnet"].PullCount; got != 0 {
			t.Errorf("bucket %d sonnet PullCount = %d, want 0", b, got)
		}
	}
}

// TestBanditModelSelector_BucketPreferredOverGlobal is the core contextual
// behaviour: once a bucket clears its own warm-up thresholds its UCB1 pick
// wins for that Bloom range, while other Bloom levels keep following the
// global selector. ExplorationCoeff=0 makes both picks deterministic.
func TestBanditModelSelector_BucketPreferredOverGlobal(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(0.0),
		MinSamplesBeforeUse:  ptr.Int(2),
		TraceDataRequirement: ptr.Int(6),
	}
	bs := newTestBanditSelector(t, cfg)
	sel := newBanditModelSelector(bs, cfg, nil)

	// Bloom 5-6 (high bucket): opus clearly best within the bucket.
	for i := 0; i < 3; i++ {
		sel.RecordResult("opus", 5, 1.0)
		sel.RecordResult("sonnet", 5, 0.5)
		sel.RecordResult("haiku", 6, 0.0)
	}
	// Global-only rewards (bloom 0) tilt the *global* averages toward
	// sonnet: opus avg = 3/10, sonnet avg ≈ 0.88, so the two selectors
	// disagree and the test can tell which one served the pick.
	for i := 0; i < 10; i++ {
		sel.RecordResult("sonnet", 0, 1.0)
	}
	for i := 0; i < 7; i++ {
		sel.RecordResult("opus", 0, 0.0)
	}

	// High bucket: total=9 >= 6, per-arm 3 >= 2 → warmed; pure exploitation → opus.
	if got := sel.SelectModel(5, "implement"); got != "opus" {
		t.Errorf("warmed bucket: SelectModel(5) = %q, want %q", got, "opus")
	}
	// Low bucket has no samples → falls back to the warmed global selector,
	// whose best average arm is sonnet.
	if got := sel.SelectModel(1, "implement"); got != "sonnet" {
		t.Errorf("cold bucket fallback: SelectModel(1) = %q, want %q", got, "sonnet")
	}
	// Out-of-range Bloom never consults a bucket.
	if got := sel.SelectModel(0, "implement"); got != "sonnet" {
		t.Errorf("invalid bloom fallback: SelectModel(0) = %q, want %q", got, "sonnet")
	}
}

// TestBanditModelSelector_BucketErrorFallsBackToGlobal verifies the
// bucket-side partial-failure path: a bucket that passes warm-up vacuously
// (zero thresholds, no arms) fails SelectArm, and the selector must fall
// through to the working global selector instead of returning "".
func TestBanditModelSelector_BucketErrorFallsBackToGlobal(t *testing.T) {
	g, err := bandit.NewSelector(1.41)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	g.AddArm("sonnet")

	buckets := make([]*bandit.Selector, bloomBucketCount)
	for i := range buckets {
		bs, err := bandit.NewSelector(1.41)
		if err != nil {
			t.Fatalf("NewSelector: %v", err)
		}
		buckets[i] = bs // intentionally arm-less so SelectArm errors
	}

	sel := &banditModelSelector{
		global:     g,
		buckets:    buckets,
		enabled:    true,
		traceReq:   0,
		minSamples: 0,
		log:        func(LogLevel, string, ...any) {},
	}

	if got := sel.SelectModel(5, "implement"); got != "sonnet" {
		t.Errorf("bucket SelectArm error: SelectModel = %q, want %q (global fallback)", got, "sonnet")
	}
}

// TestBanditModelSelector_ColdBucketFallsBackToStatic verifies layering when
// nothing is warmed: a cold bucket falls through to a cold global selector,
// which returns "" so the static BloomLevel→model mapping stays in effect.
func TestBanditModelSelector_ColdBucketFallsBackToStatic(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(1.41),
		MinSamplesBeforeUse:  ptr.Int(2),
		TraceDataRequirement: ptr.Int(50),
	}
	bs := newTestBanditSelector(t, cfg)
	sel := newBanditModelSelector(bs, cfg, nil)

	sel.RecordResult("sonnet", 5, 1.0)

	if got := sel.SelectModel(5, "implement"); got != "" {
		t.Errorf("cold selectors: SelectModel = %q, want empty (static fallback)", got)
	}
}
