package model

// CostTrackingConfig controls per-command / per-worker token & cost tracking
// (issue #32). Opt-in: when disabled (the default) the daemon performs no
// session-file reads and state/metrics.yaml carries no usage section, so
// scan/dispatch/verify behaviour is unchanged.
//
// Collection is limited to paths that can be read reliably from local files
// (currently claude-code session records under ~/.claude/projects). Runtimes
// without such a path (codex / gemini) are reported as "unknown" rather than
// zero, and totals are flagged as partial.
type CostTrackingConfig struct {
	// Enabled turns collection on. Default false.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Budget holds opt-in alert thresholds. A threshold of 0 (or negative)
	// disables that alert.
	Budget CostBudgetConfig `yaml:"budget,omitempty"`
}

// CostBudgetConfig holds budget alert thresholds in USD. Thresholds compare
// against the *known* (claude-code) estimated cost only; exceeding one emits
// a WARN log and a dashboard alert — nothing is throttled or stopped.
type CostBudgetConfig struct {
	// TotalUSD alerts when the summed estimated cost across all agents
	// exceeds this value. 0 = disabled.
	TotalUSD *float64 `yaml:"total_usd,omitempty"`
	// PerCommandUSD alerts when any single command's estimated cost
	// exceeds this value. 0 = disabled.
	PerCommandUSD *float64 `yaml:"per_command_usd,omitempty"`
}

// EffectiveEnabled returns Enabled, defaulting to false when unset.
func (c CostTrackingConfig) EffectiveEnabled() bool { return effectiveValue(c.Enabled, false) }

// EffectiveTotalUSD returns the total budget threshold; values <= 0 (and
// unset) mean the alert is disabled and 0 is returned.
func (c CostBudgetConfig) EffectiveTotalUSD() float64 {
	v := effectiveValue(c.TotalUSD, 0.0)
	if v < 0 {
		return 0
	}
	return v
}

// EffectivePerCommandUSD returns the per-command budget threshold; values
// <= 0 (and unset) mean the alert is disabled and 0 is returned.
func (c CostBudgetConfig) EffectivePerCommandUSD() float64 {
	v := effectiveValue(c.PerCommandUSD, 0.0)
	if v < 0 {
		return 0
	}
	return v
}
