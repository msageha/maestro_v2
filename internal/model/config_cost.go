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
	// CollectIntervalSec is the minimum number of seconds between two
	// session-file collection passes. Collection re-stats every retained
	// session transcript, so running it on every scan tick makes daemon
	// scan I/O grow linearly with transcript retention; the interval
	// bounds that cost while keeping usage at most this many seconds
	// stale (each pass is a full recompute, so nothing is ever lost).
	// Unset defaults to DefaultCostCollectIntervalSec; 0 or negative
	// collects on every scan.
	CollectIntervalSec *int `yaml:"collect_interval_sec,omitempty"`
	// Budget holds opt-in alert thresholds. A threshold of 0 (or negative)
	// disables that alert.
	Budget CostBudgetConfig `yaml:"budget,omitempty"`
}

// DefaultCostCollectIntervalSec is the default minimum interval between
// usage collection passes (seconds).
const DefaultCostCollectIntervalSec = 60

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

// MaxCostCollectIntervalSec caps collect_interval_sec at one day. The cap
// keeps time.Duration(v)*time.Second from overflowing into a negative
// duration (which would silently disable the throttle) on absurd values.
const MaxCostCollectIntervalSec = 24 * 60 * 60

// EffectiveCollectIntervalSec returns the minimum collection interval in
// seconds. Unset defaults to DefaultCostCollectIntervalSec; explicit values
// <= 0 return 0 (collect on every scan); values above
// MaxCostCollectIntervalSec are clamped to it.
func (c CostTrackingConfig) EffectiveCollectIntervalSec() int {
	v := effectiveValue(c.CollectIntervalSec, DefaultCostCollectIntervalSec)
	if v < 0 {
		return 0
	}
	if v > MaxCostCollectIntervalSec {
		return MaxCostCollectIntervalSec
	}
	return v
}

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
