package model

import (
	"testing"
	"time"
)

func TestIsTerminalRolloutState(t *testing.T) {
	tests := []struct {
		state RolloutState
		want  bool
	}{
		{RolloutStatePending, false},
		{RolloutStateRunning, false},
		{RolloutStateSelecting, false},
		{RolloutStateCompleted, true},
		{RolloutStateCancelled, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := IsTerminalRolloutState(tt.state); got != tt.want {
				t.Errorf("IsTerminalRolloutState(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestRolloutGroup_Defaults(t *testing.T) {
	g := RolloutGroup{
		ID:         "rg-1",
		TaskID:     "task-1",
		CommandID:  "cmd-1",
		Candidates: nil,
		State:      RolloutStatePending,
		CreatedAt:  time.Now(),
	}

	if g.WinnerIndex != nil {
		t.Errorf("WinnerIndex = %v, want nil", g.WinnerIndex)
	}
	if g.CompletedAt != nil {
		t.Error("CompletedAt should be nil for pending group")
	}
	if len(g.Candidates) != 0 {
		t.Errorf("Candidates length = %d, want 0", len(g.Candidates))
	}
}

func TestRolloutCandidate_Fields(t *testing.T) {
	c := RolloutCandidate{
		SlotIndex:  0,
		WorkerID:   "worker1",
		BranchName: "rollout/task-1/0",
		Status:     "pending",
	}
	if c.Fitness != nil {
		t.Error("Fitness should be nil initially")
	}
	if c.Status != "pending" {
		t.Errorf("Status = %q, want %q", c.Status, "pending")
	}
}

func TestRolloutEligibility(t *testing.T) {
	e := RolloutEligibility{Eligible: true}
	if !e.Eligible {
		t.Error("expected Eligible=true")
	}
	if len(e.Reasons) != 0 {
		t.Error("expected empty Reasons")
	}

	e2 := RolloutEligibility{
		Eligible: false,
		Reasons:  []string{"bloom level too low", "no prior failures"},
	}
	if e2.Eligible {
		t.Error("expected Eligible=false")
	}
	if len(e2.Reasons) != 2 {
		t.Errorf("Reasons length = %d, want 2", len(e2.Reasons))
	}
}

func TestRolloutConfig_Defaults(t *testing.T) {
	var cfg RolloutConfig

	if cfg.EffectiveEnabled() {
		t.Error("default Enabled should be false")
	}
	if got := cfg.EffectiveMaxConcurrent(); got != 2 {
		t.Errorf("EffectiveMaxConcurrent() = %d, want 2", got)
	}
	if got := cfg.EffectiveMaxParallelPerTask(); got != 2 {
		t.Errorf("EffectiveMaxParallelPerTask() = %d, want 2", got)
	}
	if got := cfg.EffectiveMinBloomLevel(); got != 4 {
		t.Errorf("EffectiveMinBloomLevel() = %d, want 4", got)
	}
	if got := cfg.EffectiveMaxExpectedPaths(); got != 10 {
		t.Errorf("EffectiveMaxExpectedPaths() = %d, want 10", got)
	}
	if got := cfg.EffectiveMinFailureCount(); got != 1 {
		t.Errorf("EffectiveMinFailureCount() = %d, want 1", got)
	}
}

func TestRolloutConfig_Overrides(t *testing.T) {
	boolTrue := true
	v3 := 3
	v5 := 5
	v6 := 6
	v15 := 15
	v2 := 2
	cfg := RolloutConfig{
		Enabled:            &boolTrue,
		MaxConcurrent:      &v3,
		MaxParallelPerTask: &v5,
		MinBloomLevel:      &v6,
		MaxExpectedPaths:   &v15,
		MinFailureCount:    &v2,
	}

	if !cfg.EffectiveEnabled() {
		t.Error("Enabled should be true")
	}
	if got := cfg.EffectiveMaxConcurrent(); got != 3 {
		t.Errorf("EffectiveMaxConcurrent() = %d, want 3", got)
	}
	if got := cfg.EffectiveMaxParallelPerTask(); got != 5 {
		t.Errorf("EffectiveMaxParallelPerTask() = %d, want 5", got)
	}
	if got := cfg.EffectiveMinBloomLevel(); got != 6 {
		t.Errorf("EffectiveMinBloomLevel() = %d, want 6", got)
	}
	if got := cfg.EffectiveMaxExpectedPaths(); got != 15 {
		t.Errorf("EffectiveMaxExpectedPaths() = %d, want 15", got)
	}
	if got := cfg.EffectiveMinFailureCount(); got != 2 {
		t.Errorf("EffectiveMinFailureCount() = %d, want 2", got)
	}
}

func TestRolloutConfig_ExplicitZero(t *testing.T) {
	zero := 0
	cfg := RolloutConfig{
		MaxConcurrent:      &zero,
		MaxParallelPerTask: &zero,
		MinBloomLevel:      &zero,
		MaxExpectedPaths:   &zero,
		MinFailureCount:    &zero,
	}

	if got := cfg.EffectiveMaxConcurrent(); got != 0 {
		t.Errorf("EffectiveMaxConcurrent() = %d, want 0", got)
	}
	if got := cfg.EffectiveMaxParallelPerTask(); got != 0 {
		t.Errorf("EffectiveMaxParallelPerTask() = %d, want 0", got)
	}
	if got := cfg.EffectiveMinBloomLevel(); got != 0 {
		t.Errorf("EffectiveMinBloomLevel() = %d, want 0", got)
	}
	if got := cfg.EffectiveMaxExpectedPaths(); got != 0 {
		t.Errorf("EffectiveMaxExpectedPaths() = %d, want 0", got)
	}
	if got := cfg.EffectiveMinFailureCount(); got != 0 {
		t.Errorf("EffectiveMinFailureCount() = %d, want 0", got)
	}
}
