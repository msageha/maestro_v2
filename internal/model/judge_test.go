package model

import (
	"context"
	"testing"
	"time"
)

func TestJudgeDecision_Fields(t *testing.T) {
	d := JudgeDecision{
		WinnerIndex: 1,
		Reasoning:   "candidate 1 has cleaner diff",
		Model:       "opus",
		Duration:    2 * time.Second,
	}
	if d.WinnerIndex != 1 {
		t.Errorf("WinnerIndex = %d, want 1", d.WinnerIndex)
	}
	if d.Reasoning == "" {
		t.Error("Reasoning should not be empty")
	}
	if d.Model != "opus" {
		t.Errorf("Model = %q, want %q", d.Model, "opus")
	}
	if d.Duration != 2*time.Second {
		t.Errorf("Duration = %v, want 2s", d.Duration)
	}
}

func TestJudgeFunc_Signature(t *testing.T) {
	var fn JudgeFunc = func(_ context.Context, scores []FitnessScore, metadata []map[string]string) (JudgeDecision, error) {
		return JudgeDecision{WinnerIndex: len(scores) - 1}, nil
	}

	scores := []FitnessScore{{Passed: true}, {Passed: true}}
	decision, err := fn(context.Background(), scores, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision.WinnerIndex != 1 {
		t.Errorf("WinnerIndex = %d, want 1", decision.WinnerIndex)
	}
}

func TestJudgeConfig_Defaults(t *testing.T) {
	var cfg JudgeConfig

	if cfg.EffectiveEnabled() {
		t.Error("default Enabled should be false")
	}
	if got := cfg.EffectiveModel(); got != "opus" {
		t.Errorf("EffectiveModel() = %q, want %q", got, "opus")
	}
	if got := cfg.EffectiveTimeoutSec(); got != 60 {
		t.Errorf("EffectiveTimeoutSec() = %d, want 60", got)
	}
}

func TestJudgeConfig_Overrides(t *testing.T) {
	boolTrue := true
	model := "sonnet"
	timeout := 120
	cfg := JudgeConfig{
		Enabled:    &boolTrue,
		Model:      &model,
		TimeoutSec: &timeout,
	}

	if !cfg.EffectiveEnabled() {
		t.Error("Enabled should be true")
	}
	if got := cfg.EffectiveModel(); got != "sonnet" {
		t.Errorf("EffectiveModel() = %q, want %q", got, "sonnet")
	}
	if got := cfg.EffectiveTimeoutSec(); got != 120 {
		t.Errorf("EffectiveTimeoutSec() = %d, want 120", got)
	}
}

func TestJudgeConfig_ExplicitZeroTimeout(t *testing.T) {
	zero := 0
	cfg := JudgeConfig{TimeoutSec: &zero}
	if got := cfg.EffectiveTimeoutSec(); got != 0 {
		t.Errorf("EffectiveTimeoutSec() = %d, want 0", got)
	}
}
