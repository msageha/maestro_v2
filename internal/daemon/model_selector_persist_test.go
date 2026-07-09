package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

// TestBanditModelSelector_SaveLoadState_WarmupSurvivesRestart pins the X-A1
// fix: arm statistics persisted at shutdown restore into a fresh selector,
// so the warm-up thresholds do not reset on every daemon restart.
func TestBanditModelSelector_SaveLoadState_WarmupSurvivesRestart(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(0.0),
		MinSamplesBeforeUse:  ptr.Int(2),
		TraceDataRequirement: ptr.Int(6),
	}
	path := filepath.Join(t.TempDir(), "bandit_state.json")

	first := newBanditModelSelector(newTestBanditSelector(t, cfg), cfg, nil)
	for i := 0; i < 5; i++ {
		first.RecordResult("sonnet", 1, 1.0)
		first.RecordResult("opus", 1, 0.5)
		first.RecordResult("haiku", 1, 0.1)
	}
	if got := first.SelectModel(2, "implement"); got != "sonnet" {
		t.Fatalf("pre-restart SelectModel = %q, want sonnet", got)
	}
	if err := first.SaveState(path); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// "Restart": fresh selectors, restore from disk.
	second := newBanditModelSelector(newTestBanditSelector(t, cfg), cfg, nil)
	if got := second.SelectModel(2, "implement"); got != "" {
		t.Fatalf("fresh selector must be in warm-up, got %q", got)
	}
	second.LoadState(path)
	if got := second.SelectModel(2, "implement"); got != "sonnet" {
		t.Errorf("post-restore SelectModel = %q, want sonnet (warm-up must survive restart)", got)
	}
	// Bucket stats travel too: bloom 1 routed rewards into the low bucket.
	if second.buckets == nil || !second.warmedUp(second.buckets[bloomBucketLow]) {
		t.Error("low bucket must be warmed up after restore")
	}
}

// TestBanditModelSelector_LoadState_MissingOrCorruptStartsFresh verifies the
// advisory nature of the snapshot: a missing file is a clean first start and
// a corrupt file degrades to fresh statistics instead of failing startup.
func TestBanditModelSelector_LoadState_MissingOrCorruptStartsFresh(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(0.0),
		MinSamplesBeforeUse:  ptr.Int(1),
		TraceDataRequirement: ptr.Int(1),
	}
	dir := t.TempDir()

	sel := newBanditModelSelector(newTestBanditSelector(t, cfg), cfg, nil)
	sel.LoadState(filepath.Join(dir, "missing.json")) // no-op, no panic

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	sel.LoadState(corrupt) // warn + fresh, no panic
	if got := sel.SelectModel(2, "implement"); got != "" {
		t.Errorf("selector after corrupt load must still be in warm-up, got %q", got)
	}
}

// TestBanditModelSelector_FeatureGate_BlocksSelectionNotLearning pins the
// stage-2 featuregate wiring: SelectModel returns "" for gated-off levels
// while RecordResult keeps feeding the arms, so learning continues even
// where adaptive selection is disabled by the profile.
func TestBanditModelSelector_FeatureGate_BlocksSelectionNotLearning(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(0.0),
		MinSamplesBeforeUse:  ptr.Int(2),
		TraceDataRequirement: ptr.Int(6),
	}
	sel := newBanditModelSelector(newTestBanditSelector(t, cfg), cfg, nil)
	gateOpen := false
	sel.featureEnabled = func(int) bool { return gateOpen }

	for i := 0; i < 5; i++ {
		sel.RecordResult("sonnet", 0, 1.0)
		sel.RecordResult("opus", 0, 0.5)
		sel.RecordResult("haiku", 0, 0.1)
	}
	if got := sel.SelectModel(2, "implement"); got != "" {
		t.Fatalf("gated-off SelectModel = %q, want empty (static mapping)", got)
	}
	gateOpen = true
	if got := sel.SelectModel(2, "implement"); got != "sonnet" {
		t.Errorf("gated-on SelectModel = %q, want sonnet (learning accrued while gated off)", got)
	}
}
