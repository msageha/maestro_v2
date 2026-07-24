package model

// Improvement lifecycle statuses (C-5 friction-driven self-improvement,
// issue #26). The lifecycle is:
//
//		observed → proposed → applied → verified
//		                ↑                   │
//		                └──── reopened ←────┘  (same friction recurs)
//
//	  - observed: friction seen but below the proposal threshold.
//	  - proposed: recurring friction; surfaced to the Planner/operator as an
//	    improvement candidate. The daemon never applies template/config
//	    changes itself — presentation is the endpoint.
//	  - applied: a repair strategy for the pattern was injected into a retry
//	    task; the measurement window is open.
//	  - verified: the measurement gate passed (enough consecutive successful
//	    repairs with the strategy applied and no recurrence in between).
//	  - reopened: the same friction recurred AFTER verification; the entry is
//	    back in the learning pool (treated like proposed).
const (
	ImprovementStatusObserved = "observed"
	ImprovementStatusProposed = "proposed"
	ImprovementStatusApplied  = "applied"
	ImprovementStatusVerified = "verified"
	ImprovementStatusReopened = "reopened"
)

// ImprovementTargetWorkflowAdvice is the only improvement target the daemon
// generates today: advisory text about prompts/workflow for the Planner or
// operator. Safety parameters, gate thresholds, and daemon control logic are
// NEVER improvement targets (C-5 safety invariant; see
// SelfImprovementConfig.EffectiveExcludeTargets).
const ImprovementTargetWorkflowAdvice = "workflow_advice"

// Improvement is one friction-driven improvement idea tracked through the
// propose → apply → measure → verify (→ reopen) lifecycle. The ID equals the
// failure/friction fingerprint so entries stay 1:1 with FingerprintDB
// patterns instead of forming a parallel learning loop.
type Improvement struct {
	ID       string `yaml:"id"` // = error fingerprint (learnings.ComputeErrorFingerprint)
	Kind     string `yaml:"kind"`
	Category string `yaml:"category,omitempty"`
	Target   string `yaml:"target"`
	// Advice is the advisory text presented to the Planner/operator (the
	// proven repair strategy adopted from a successful retry summary).
	Advice          string `yaml:"advice,omitempty"`
	Status          string `yaml:"status"`
	OccurrenceCount int    `yaml:"occurrence_count"`
	ReopenCount     int    `yaml:"reopen_count,omitempty"`
	ProposedAt      string `yaml:"proposed_at,omitempty"`
	AppliedAt       string `yaml:"applied_at,omitempty"`
	VerifiedAt      string `yaml:"verified_at,omitempty"`
	ReopenedAt      string `yaml:"reopened_at,omitempty"`
	LastSeenAt      string `yaml:"last_seen_at"`

	Measure ImprovementMeasure `yaml:"measure,omitempty"`
}

// ImprovementMeasure grounds the effect measurement in observable counters:
// the friction recurrence counts tracked by the daemon plus snapshots of the
// state/metrics.yaml failure counters taken at apply/verify time. Verification
// is quantitative — an improvement is promoted only when the post-apply
// success streak reaches the configured gate with no recurrence resetting it.
type ImprovementMeasure struct {
	// BaselineOccurrences is the friction occurrence count when the
	// strategy was (first) applied. Recurrence past this baseline is the
	// primary regression signal.
	BaselineOccurrences int `yaml:"baseline_occurrences,omitempty"`
	// BaselineTasksFailed / BaselineDeadLetters snapshot the corresponding
	// state/metrics.yaml counters at apply time (best-effort; zero when the
	// metrics file was unreadable).
	BaselineTasksFailed int `yaml:"baseline_tasks_failed,omitempty"`
	BaselineDeadLetters int `yaml:"baseline_dead_letters,omitempty"`
	// PostApplySuccesses is the current streak of retries that completed
	// with the strategy applied. Reset to zero by any recurrence.
	PostApplySuccesses int `yaml:"post_apply_successes,omitempty"`
	// PostApplyRecurrences counts same-pattern friction observed after
	// apply (measurement history; never reset within a window).
	PostApplyRecurrences int `yaml:"post_apply_recurrences,omitempty"`
	// VerifiedTasksFailed / VerifiedDeadLetters snapshot the metrics
	// counters at verification time, quantifying the window.
	VerifiedTasksFailed int `yaml:"verified_tasks_failed,omitempty"`
	VerifiedDeadLetters int `yaml:"verified_dead_letters,omitempty"`
}

// ImprovementsFile is the on-disk format for .maestro/state/improvements.yaml.
type ImprovementsFile struct {
	SchemaVersion int           `yaml:"schema_version"`
	FileType      string        `yaml:"file_type"` // "state_improvements"
	Improvements  []Improvement `yaml:"improvements"`
}

// ImprovementsFileType is the file_type marker for state/improvements.yaml.
const ImprovementsFileType = "state_improvements"
