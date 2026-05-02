package model

import (
	"errors"
	"fmt"
)

// TaskTracking groups task state management fields within CommandState.
// Embedded with yaml:",inline" to maintain flat YAML serialization.
type TaskTracking struct {
	ExpectedTaskCount  int                 `yaml:"expected_task_count"`
	RequiredTaskIDs    []string            `yaml:"required_task_ids"`
	OptionalTaskIDs    []string            `yaml:"optional_task_ids"`
	TaskDependencies   map[string][]string `yaml:"task_dependencies"`
	TaskStates         map[string]Status   `yaml:"task_states"`
	CancelledReasons   map[string]string   `yaml:"cancelled_reasons"`
	AppliedResultIDs   map[string]string   `yaml:"applied_result_ids"`
	SystemCommitTaskID *string             `yaml:"system_commit_task_id"`
	QueueWriteFailed   map[string]string   `yaml:"queue_write_failed,omitempty"` // task_id → "workerID:resultID"; set when result committed but queue terminal write failed (H2 sticky error)
	IdempotencyKeys    map[string]string   `yaml:"idempotency_keys,omitempty"`   // idempotency_key → task_id; prevents duplicate task injection on retry
}

// RetryTracking groups retry-related fields within CommandState.
// Embedded with yaml:",inline" to maintain flat YAML serialization.
type RetryTracking struct {
	RetryLineage       map[string]string `yaml:"retry_lineage"`
	RetryEnqueueFailed map[string]string `yaml:"retry_enqueue_failed,omitempty"` // task_id → worker_id; set when state registered but queue add failed
}

// PhaseTracking groups phase lifecycle fields within CommandState.
// Embedded with yaml:",inline" to maintain flat YAML serialization.
type PhaseTracking struct {
	Phases []Phase `yaml:"phases"`
	// HeavyVerifyOwners reserves "the task that runs heavy verify
	// (test/security/performance) for this phase" so the per-task
	// optimisation in result_write_handler.shouldDeferHeavyVerifyForIntermediateTask
	// cannot turn into a race where every sibling sees the others at
	// verify_pending (non-terminal) and all defer, leaving no task to
	// actually run repo-wide verification.
	//
	// Map key: Phase.PhaseID. Value: the Task.ID that has been granted
	// heavy-verify ownership under the state lock. Tasks observe this
	// field on each verify entry: if they are not the owner, they defer
	// heavy categories. Only one task per phase ever runs heavy verify,
	// guaranteed by the state-lock-protected CAS in
	// ResultWriteAPI.reserveOrDeferHeavyVerify.
	HeavyVerifyOwners map[string]string `yaml:"heavy_verify_owners,omitempty"`
}

// PhaseIndex returns the slice index for the given phaseID by linear search.
// Returns (index, true) if found, (-1, false) otherwise.
// Linear search is used instead of caching because the number of phases per
// command is typically small, and a cache would risk returning stale data
// when the Phases slice is modified.
func (pt *PhaseTracking) PhaseIndex(phaseID string) (int, bool) {
	for i := range pt.Phases {
		if pt.Phases[i].PhaseID == phaseID {
			return i, true
		}
	}
	return -1, false
}

// CommandState represents the lifecycle state for a single command.
// It tracks plan versioning, phases, task dependencies, cancellation,
// circuit-breaker state, and completion policy. Embedded sub-structures use
// yaml:",inline" to preserve the flat YAML layout.
type CommandState struct {
	SchemaVersion    int                 `yaml:"schema_version"`
	FileType         string              `yaml:"file_type"`
	CommandID        string              `yaml:"command_id"`
	PlanVersion      int                 `yaml:"plan_version"`
	PlanStatus       PlanStatus          `yaml:"plan_status"`
	CompletionPolicy CompletionPolicy    `yaml:"completion_policy"`
	Cancel           CancelState         `yaml:"cancel"`
	CircuitBreaker   CircuitBreakerState `yaml:"circuit_breaker"`
	TaskTracking     `yaml:",inline"`
	RetryTracking    `yaml:",inline"`
	PhaseTracking    `yaml:",inline"`
	LastReconciledAt *string `yaml:"last_reconciled_at"`
	CreatedAt        string  `yaml:"created_at"`
	UpdatedAt        string  `yaml:"updated_at"`
}

// CircuitBreakerState tracks per-command circuit breaker counters.
type CircuitBreakerState struct {
	ConsecutiveFailures int     `yaml:"consecutive_failures"`
	LastProgressAt      *string `yaml:"last_progress_at,omitempty"`
	Tripped             bool    `yaml:"tripped"`
	TrippedAt           *string `yaml:"tripped_at,omitempty"`
	TripReason          *string `yaml:"trip_reason,omitempty"`
	HalfOpen            bool    `yaml:"half_open,omitempty"`
	HalfOpenAt          *string `yaml:"half_open_at,omitempty"`
	HalfOpenProbeActive bool    `yaml:"half_open_probe_active,omitempty"`
}

// CompletionPolicy defines how command completion is evaluated, including
// required/optional task failure behavior and dependency-failure handling.
type CompletionPolicy struct {
	Mode                    string `yaml:"mode"`
	AllowDynamicTasks       bool   `yaml:"allow_dynamic_tasks"`
	OnRequiredFailed        string `yaml:"on_required_failed"`
	OnRequiredCancelled     string `yaml:"on_required_cancelled"`
	OnOptionalFailed        string `yaml:"on_optional_failed"`
	DependencyFailurePolicy string `yaml:"dependency_failure_policy"`
}

// CancelState tracks a command cancellation request.
type CancelState struct {
	Requested   bool    `yaml:"requested"`
	RequestedAt *string `yaml:"requested_at"`
	RequestedBy *string `yaml:"requested_by"`
	Reason      *string `yaml:"reason"`
}

// Phase represents one execution phase in a command plan.
// It groups tasks, controls execution order, and records inter-phase
// dependencies.
type Phase struct {
	PhaseID          string            `yaml:"phase_id"`
	Name             string            `yaml:"name"`
	Type             string            `yaml:"type"` // "concrete" or "deferred"
	Status           PhaseStatus       `yaml:"status"`
	DependsOnPhases  []string          `yaml:"depends_on_phases"`
	TaskIDs          []string          `yaml:"task_ids"`
	Constraints      *PhaseConstraints `yaml:"constraints"`
	ActivatedAt      *string           `yaml:"activated_at"`
	CompletedAt      *string           `yaml:"completed_at"`
	FillDeadlineAt   *string           `yaml:"fill_deadline_at"`
	FillingStartedAt *string           `yaml:"filling_started_at,omitempty"`
	ReopenedAt       *string           `yaml:"reopened_at"`
	// AwaitingFillSince records the timestamp at which this phase last
	// entered the awaiting_fill status. Distinct from FillDeadlineAt (the
	// hard timeout deadline) and ActivatedAt (transition to active): the
	// awaiting-fill watchdog (queue_scan_phase_a_phase.stepAwaitingFillWatchdog)
	// uses this to fire a re-prompt long before the fill_deadline expires
	// when a Planner has stopped making progress. Cleared on transition out
	// of awaiting_fill so a re-entry (e.g., reopened phase awaiting more
	// tasks) restarts the clock.
	AwaitingFillSince *string `yaml:"awaiting_fill_since,omitempty"`
	// AwaitingFillStallNotifiedAt records the last time the watchdog emitted
	// a stall signal for this phase, so the watchdog does not refire on every
	// scan cycle (default 5s) once the threshold has elapsed. Cleared
	// together with AwaitingFillSince when the phase exits awaiting_fill.
	AwaitingFillStallNotifiedAt *string `yaml:"awaiting_fill_stall_notified_at,omitempty"`
	// CancelledReason records WHY a phase was cancelled, when set. The
	// resolver populates it on cascade-cancellations with a structured
	// prefix (see DependencyCascadeCancelPrefix) so a later scan can
	// distinguish "scheduler-derived cancellation" (reversible if the
	// cause goes away) from "operator/manual cancellation" (terminal).
	// Absence of the field is the marker for the latter — every
	// automatic cascade writes the reason. Empty/nil => not safe to
	// auto-recover.
	CancelledReason *string `yaml:"cancelled_reason,omitempty"`
}

// DependencyCascadeCancelPrefix tags Phase.CancelledReason values that
// were written by the dependency resolver when cascading a cancellation
// from an upstream failed/cancelled/timed-out phase. The full reason has
// the form "<prefix><dep_phase_id>" so the dep can be recovered without
// parsing free-form text. NewDependencyCascadeCancelReason and
// DependencyCascadeDepID encapsulate the format so the prefix string
// only appears in those helpers.
const DependencyCascadeCancelPrefix = "dependency_phase_cascaded:"

// NewDependencyCascadeCancelReason returns the canonical CancelledReason
// string the resolver writes when cascading a cancellation from depID.
func NewDependencyCascadeCancelReason(depID string) string {
	return DependencyCascadeCancelPrefix + depID
}

// DependencyCascadeDepID extracts the upstream dep phase ID from a
// CancelledReason that was written by the resolver. Returns ("", false)
// when reason is nil, empty, or not a cascade reason — that case means
// the cancellation was operator/manual, NOT eligible for auto-recovery.
func DependencyCascadeDepID(reason *string) (string, bool) {
	if reason == nil || *reason == "" {
		return "", false
	}
	r := *reason
	if len(r) <= len(DependencyCascadeCancelPrefix) {
		return "", false
	}
	if r[:len(DependencyCascadeCancelPrefix)] != DependencyCascadeCancelPrefix {
		return "", false
	}
	return r[len(DependencyCascadeCancelPrefix):], true
}

// PhaseInfo represents phase metadata from command state.
// Used by StateReader implementations to return phase data without exposing
// the full Phase struct or YAML serialization details.
type PhaseInfo struct {
	ID     string
	Name   string
	Status PhaseStatus
	// TaskIDs lists every task in the phase regardless of required/optional
	// classification — it mirrors Phase.TaskIDs verbatim. The dependency
	// resolver's phase-completion check uses this set as the authoritative
	// "what must finish before the phase can transition" view, falling back
	// to RequiredTaskIDs only when an explicit required-only judgment is
	// needed (which is rare). Phase-level transitions must observe every
	// task in the phase: a phase whose tasks were all classified as
	// optional would otherwise have an empty RequiredTaskIDs slice and
	// never transition. Plan-level required/optional policy still drives
	// whether the *plan* succeeds or fails (DeriveStatus).
	TaskIDs          []string
	DependsOn        []string // phase IDs
	FillDeadlineAt   *string
	RequiredTaskIDs  []string
	SystemCommitTask bool
	// CancelledReason mirrors Phase.CancelledReason so the dependency
	// resolver can distinguish cascade-cancellations (auto-recoverable
	// when the upstream cause goes away) from operator/manual cancels
	// (terminal). Nil/empty means "no machine-readable reason" → treat
	// as terminal.
	CancelledReason *string
	// AwaitingFillSince mirrors Phase.AwaitingFillSince so the
	// awaiting-fill watchdog can compute elapsed time without re-loading
	// state directly. Nil for phases not currently at awaiting_fill.
	AwaitingFillSince *string
	// AwaitingFillStallNotifiedAt mirrors Phase.AwaitingFillStallNotifiedAt
	// so the watchdog can dedup re-emissions across scan cycles. Nil
	// means the watchdog has not yet fired for the current awaiting_fill
	// window.
	AwaitingFillStallNotifiedAt *string
}

// PhaseConstraints defines limits applied to a phase, including maximum task
// count, allowed Bloom levels, and timeout.
type PhaseConstraints struct {
	MaxTasks           int   `yaml:"max_tasks"`
	AllowedBloomLevels []int `yaml:"allowed_bloom_levels"`
	TimeoutMinutes     int   `yaml:"timeout_minutes"`
}

// Validate checks that PhaseConstraints fields are within valid ranges.
// MaxTasks and TimeoutMinutes must be positive (> 0).
func (pc PhaseConstraints) Validate() error {
	var errs []error
	if pc.MaxTasks <= 0 {
		errs = append(errs, fmt.Errorf("phase_constraints.max_tasks: must be > 0, got %d", pc.MaxTasks))
	}
	if pc.TimeoutMinutes <= 0 {
		errs = append(errs, fmt.Errorf("phase_constraints.timeout_minutes: must be > 0, got %d", pc.TimeoutMinutes))
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
