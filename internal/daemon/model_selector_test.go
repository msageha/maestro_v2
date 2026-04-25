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
	sel := newBanditModelSelector(nil, cfg)
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
	sel := newBanditModelSelector(nil, cfg)
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
	sel := newBanditModelSelector(bs, cfg)

	for i := 0; i < 3; i++ {
		sel.RecordResult("sonnet", 1.0)
		sel.RecordResult("opus", 0.8)
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
	sel := newBanditModelSelector(bs, cfg)

	for i := 0; i < 5; i++ {
		sel.RecordResult("sonnet", 1.0)
		sel.RecordResult("opus", 0.8)
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
	sel := newBanditModelSelector(bs, cfg)

	for i := 0; i < 5; i++ {
		sel.RecordResult("sonnet", 1.0)
		sel.RecordResult("opus", 0.5)
		sel.RecordResult("haiku", 0.1)
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
	sel := newBanditModelSelector(bs, cfg)

	sel.RecordResult("sonnet", 1.0)
	sel.RecordResult("sonnet", 0.6)

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
	sel := newBanditModelSelector(nil, cfg)

	// Must be safe even when sel is nil (disabled path).
	sel.RecordResult("sonnet", 1.0)
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
	sel := newBanditModelSelector(bs, cfg)

	sel.RecordResult("", 1.0)

	for arm, stats := range bs.GetStats() {
		if stats.PullCount != 0 {
			t.Errorf("empty model name leaked into arm %q (PullCount=%d)", arm, stats.PullCount)
		}
	}
}

// TestBanditModelSelector_SelectArmErrorReturnsEmpty exercises the safety
// path where bandit.SelectArm fails (no arms registered). The selector must
// return "" so AssignWorkers' static fallback applies, never propagate the
// error.
func TestBanditModelSelector_SelectArmErrorReturnsEmpty(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(1.41),
		MinSamplesBeforeUse:  ptr.Int(0),
		TraceDataRequirement: ptr.Int(0),
	}
	bs := newTestBanditSelector(t, cfg)
	sel := newBanditModelSelector(bs, cfg)

	bs.Reset() // wipe arms so SelectArm errors

	if got := sel.SelectModel(5, "implement"); got != "" {
		t.Errorf("SelectArm error path: SelectModel = %q, want empty", got)
	}
}
