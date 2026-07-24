package model

import (
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

func TestCostTrackingConfig_Defaults(t *testing.T) {
	t.Parallel()
	var cfg Config
	if cfg.CostTracking.EffectiveEnabled() {
		t.Error("cost tracking must default to disabled")
	}
	if got := cfg.CostTracking.Budget.EffectiveTotalUSD(); got != 0 {
		t.Errorf("total budget default = %v, want 0 (disabled)", got)
	}
	if got := cfg.CostTracking.Budget.EffectivePerCommandUSD(); got != 0 {
		t.Errorf("per-command budget default = %v, want 0 (disabled)", got)
	}
}

func TestCostTrackingConfig_Decode(t *testing.T) {
	t.Parallel()
	data := `
cost_tracking:
  enabled: true
  budget:
    total_usd: 12.5
    per_command_usd: 3
`
	var cfg Config
	if err := yamlv3.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.CostTracking.EffectiveEnabled() {
		t.Error("enabled not decoded")
	}
	if got := cfg.CostTracking.Budget.EffectiveTotalUSD(); got != 12.5 {
		t.Errorf("total_usd = %v, want 12.5", got)
	}
	if got := cfg.CostTracking.Budget.EffectivePerCommandUSD(); got != 3 {
		t.Errorf("per_command_usd = %v, want 3", got)
	}
}

func TestCostTrackingConfig_NegativeBudgetDisabled(t *testing.T) {
	t.Parallel()
	data := `
cost_tracking:
  enabled: true
  budget:
    total_usd: -1
    per_command_usd: -0.5
`
	var cfg Config
	if err := yamlv3.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.CostTracking.Budget.EffectiveTotalUSD(); got != 0 {
		t.Errorf("negative total_usd = %v, want 0 (disabled)", got)
	}
	if got := cfg.CostTracking.Budget.EffectivePerCommandUSD(); got != 0 {
		t.Errorf("negative per_command_usd = %v, want 0 (disabled)", got)
	}
}

func TestCostTrackingConfig_UnknownFieldsTolerated(t *testing.T) {
	t.Parallel()
	// config.yaml decode stays non-strict: future/typo keys must not fail.
	data := `
cost_tracking:
  enabled: true
  some_future_field: "x"
`
	var cfg Config
	if err := yamlv3.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("non-strict decode must tolerate unknown fields: %v", err)
	}
	if !cfg.CostTracking.EffectiveEnabled() {
		t.Error("enabled not decoded alongside unknown field")
	}
}
