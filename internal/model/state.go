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
}

// PhaseInfo represents phase metadata from command state.
// Used by StateReader implementations to return phase data without exposing
// the full Phase struct or YAML serialization details.
type PhaseInfo struct {
	ID               string
	Name             string
	Status           PhaseStatus
	DependsOn        []string // phase IDs
	FillDeadlineAt   *string
	RequiredTaskIDs  []string
	SystemCommitTask bool
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
