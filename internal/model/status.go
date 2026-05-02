package model

import "fmt"

// Status represents the lifecycle state of a task or queue entry.
type Status string

const (
	// StatusPending indicates the item is waiting to be processed.
	StatusPending Status = "pending"
	// StatusInProgress indicates the item is currently being processed.
	StatusInProgress Status = "in_progress"
	// StatusCompleted indicates the item finished successfully.
	StatusCompleted Status = "completed"
	// StatusFailed indicates the item terminated with an error.
	StatusFailed Status = "failed"
	// StatusCancelled indicates the item was cancelled before completion.
	StatusCancelled Status = "cancelled"
	// StatusDeadLetter indicates the item exceeded max attempts and was dead-lettered.
	StatusDeadLetter Status = "dead_letter"

	// REQUIREMENTS.md §2.1: Extended task lifecycle states
	// Note: These extended statuses are used in task state transitions (validTaskStateTransitions)
	// only; they do NOT appear in queue entry transitions (validCommandTaskQueueTransitions).

	// StatusPlanned indicates a task has been planned but not yet ready to run.
	// It is the initial state in the extended lifecycle and transitions to ready,
	// paused_for_human, or aborted. Not used in queue entries.
	StatusPlanned Status = "planned"
	// StatusReady indicates a task is eligible for dispatch.
	StatusReady Status = "ready"
	// StatusDispatched indicates a task has been dispatched to a worker.
	StatusDispatched Status = "dispatched"
	// StatusRunning indicates a task is actively being executed by a worker.
	StatusRunning Status = "running"
	// StatusVerifyPending indicates a task is awaiting quality gate verification.
	StatusVerifyPending Status = "verify_pending"
	// StatusRepairPending indicates a task is queued for repair after a failure.
	StatusRepairPending Status = "repair_pending"
	// StatusPausedForReplan indicates a task is paused pending replanning.
	StatusPausedForReplan Status = "paused_for_replan"
	// StatusPausedForHuman indicates a task is paused pending human intervention.
	StatusPausedForHuman Status = "paused_for_human"
	// StatusAborted indicates a task was aborted without completing.
	StatusAborted Status = "aborted"
)

// PlanStatus represents the lifecycle state of a command plan.
type PlanStatus string

const (
	// PlanStatusPlanning indicates the plan is being constructed.
	PlanStatusPlanning PlanStatus = "planning"
	// PlanStatusSealed indicates the plan has been finalized and is ready to execute.
	PlanStatusSealed PlanStatus = "sealed"
	// PlanStatusCompleted indicates all tasks in the plan have completed successfully.
	PlanStatusCompleted PlanStatus = "completed"
	// PlanStatusFailed indicates the plan terminated due to task failures.
	PlanStatusFailed PlanStatus = "failed"
	// PlanStatusCancelled indicates the plan was cancelled before completion.
	PlanStatusCancelled PlanStatus = "cancelled"
)

// PhaseStatus represents the lifecycle state of a plan phase.
type PhaseStatus string

const (
	// PhaseStatusPending indicates the phase has not yet started.
	PhaseStatusPending PhaseStatus = "pending"
	// PhaseStatusAwaitingFill indicates the phase is waiting for worker slot allocation.
	PhaseStatusAwaitingFill PhaseStatus = "awaiting_fill"
	// PhaseStatusFilling indicates worker slots are being assigned to the phase.
	PhaseStatusFilling PhaseStatus = "filling"
	// PhaseStatusActive indicates the phase is executing with assigned workers.
	PhaseStatusActive PhaseStatus = "active"
	// PhaseStatusCompleted indicates all tasks in the phase completed successfully.
	PhaseStatusCompleted PhaseStatus = "completed"
	// PhaseStatusFailed indicates the phase terminated due to task failures.
	PhaseStatusFailed PhaseStatus = "failed"
	// PhaseStatusCancelled indicates the phase was cancelled before completion.
	PhaseStatusCancelled PhaseStatus = "cancelled"
	// PhaseStatusTimedOut indicates the phase's fill deadline expired.
	PhaseStatusTimedOut PhaseStatus = "timed_out"
)

// ContinuousStatus represents the operational state of continuous mode.
type ContinuousStatus string

const (
	// ContinuousStatusRunning indicates continuous mode is actively running iterations.
	ContinuousStatusRunning ContinuousStatus = "running"
	// ContinuousStatusPaused indicates continuous mode is suspended pending human review.
	ContinuousStatusPaused ContinuousStatus = "paused"
	// ContinuousStatusStopped indicates continuous mode has been terminated.
	ContinuousStatusStopped ContinuousStatus = "stopped"
)

// NotificationType represents the type of an orchestrator notification.
type NotificationType string

const (
	// NotificationTypeCommandCompleted is sent when a command finishes successfully.
	NotificationTypeCommandCompleted NotificationType = "command_completed"
	// NotificationTypeCommandFailed is sent when a command terminates with a failure.
	NotificationTypeCommandFailed NotificationType = "command_failed"
	// NotificationTypeCommandCancelled is sent when a command is cancelled before completion.
	NotificationTypeCommandCancelled NotificationType = "command_cancelled"
	// NotificationTypeContinuousPaused is emitted by the daemon when continuous
	// mode transitions to Paused (e.g. pause_on_failure triggered). The Orchestrator
	// uses this signal to surface the pause reason to the user and halt auto-generation.
	NotificationTypeContinuousPaused NotificationType = "continuous_paused"
	// NotificationTypeContinuousStopped is emitted by the daemon when continuous
	// mode transitions to Stopped (max iterations / max consecutive failures reached).
	// The Orchestrator uses this signal to finalize the run and report to the user.
	NotificationTypeContinuousStopped NotificationType = "continuous_stopped"
)

var terminalStatuses = map[Status]bool{
	StatusCompleted:  true,
	StatusFailed:     true,
	StatusCancelled:  true,
	StatusDeadLetter: true,
	StatusAborted:    true,
}

var terminalPlanStatuses = map[PlanStatus]bool{
	PlanStatusCompleted: true,
	PlanStatusFailed:    true,
	PlanStatusCancelled: true,
}

var terminalPhaseStatuses = map[PhaseStatus]bool{
	PhaseStatusCompleted: true,
	PhaseStatusFailed:    true,
	PhaseStatusCancelled: true,
	PhaseStatusTimedOut:  true,
}

var terminalWorktreeStatuses = map[WorktreeStatus]bool{
	WorktreeStatusCleanupDone: true,
	// cleanup_failed is NOT terminal: it can transition to cleanup_done on retry
}

var terminalIntegrationStatuses = map[IntegrationStatus]bool{
	IntegrationStatusPublished:   true,
	IntegrationStatusQuarantined: true, // requires manual operator intervention
	// failed is NOT terminal: it can transition to merging on retry
}

var validNotificationTypes = map[NotificationType]bool{
	NotificationTypeCommandCompleted:  true,
	NotificationTypeCommandFailed:     true,
	NotificationTypeCommandCancelled:  true,
	NotificationTypeContinuousPaused:  true,
	NotificationTypeContinuousStopped: true,
}

// Queue entry status transitions for command/task: pending ↔ in_progress → terminal.
// dead_letter only from pending (daemon detects attempts >= max_attempts before dispatch).
//
// Transition triggers:
//
//	pending → in_progress:  LeaseManager.AcquireLease (daemon dispatch)
//	pending → cancelled:    cancel handler (orchestrator cancellation request)
//	pending → dead_letter:  daemon queue scan (attempts >= max_attempts before dispatch)
//	in_progress → pending:  LeaseManager.ReleaseLease (lease timeout / explicit release)
//	in_progress → completed: result_write_handler (worker/planner reports success)
//	in_progress → failed:    result_write_handler (worker/planner reports failure)
//	in_progress → cancelled: cancel handler (orchestrator cancellation during execution)
//
// Symmetry with validTaskStateTransitions:
//   - Queue allows in_progress → pending (lease release); task state does NOT,
//     because task state tracks logical lifecycle, not queue position.
//   - Both share pending → {in_progress, cancelled, dead_letter}.
//   - Task state has extended lifecycle states (planned, ready, dispatched, etc.)
//     that exist only in task state, not in queue transitions.
var validCommandTaskQueueTransitions = map[Status]map[Status]bool{
	StatusPending: {
		StatusInProgress: true,
		StatusCancelled:  true,
		StatusDeadLetter: true,
	},
	StatusInProgress: {
		StatusPending:   true, // lease release → back to pending
		StatusCompleted: true,
		StatusFailed:    true,
		StatusCancelled: true,
	},
}

// Notification queue transitions: terminal states are completed|dead_letter only.
//
// Transition triggers:
//
//	pending → in_progress:  LeaseManager.AcquireNotificationLease (daemon dispatch)
//	pending → dead_letter:  daemon queue scan (attempts >= max_attempts)
//	in_progress → pending:  LeaseManager.ReleaseNotificationLease (lease timeout)
//	in_progress → completed: notification delivery handler (successful delivery)
//
// Asymmetries with command/task queue transitions (intentional):
//   - No cancelled: notifications are not individually cancellable.
//   - No failed: notification delivery either succeeds or is retried/dead-lettered.
var validNotificationQueueTransitions = map[Status]map[Status]bool{
	StatusPending: {
		StatusInProgress: true,
		StatusDeadLetter: true,
	},
	StatusInProgress: {
		StatusPending:   true, // lease release → back to pending
		StatusCompleted: true,
	},
}

// Task state transitions (in state/commands/).
// These track the logical task lifecycle, separate from queue entry position.
//
// Basic lifecycle transitions:
//
//	pending → in_progress:  daemon dispatch (AcquireTaskLease)
//	pending → cancelled:    command cancellation before dispatch
//	pending → dead_letter:  daemon dead-letters when queue attempts >= max_attempts
//	in_progress → completed: result_write_handler (worker reports success)
//	in_progress → failed:    result_write_handler (worker reports failure)
//	in_progress → cancelled: command cancellation during execution
//
// Symmetry with queue transitions (validCommandTaskQueueTransitions):
//   - Queue allows in_progress → pending (lease release); task state does NOT
//     because logical task lifecycle does not regress to pending.
//   - Both share pending → {in_progress, cancelled, dead_letter}.
//   - Extended states below exist only in task state.
//
// Universal transitions (handled in ValidateTaskStateTransition before map lookup):
//   - Any non-terminal → paused_for_human (operator intervention)
//   - Any non-terminal → aborted (task abort)
var validTaskStateTransitions = map[Status]map[Status]bool{
	StatusPending: {
		StatusInProgress: true,
		StatusPlanned:    true, // §2.1 migration: legacy fixtures (TaskStates=pending) auto-promote to planned
		StatusCancelled:  true,
		StatusDeadLetter: true, // daemon dead-letters task (symmetric with queue transitions)
	},
	StatusInProgress: {
		StatusCompleted:     true,
		StatusFailed:        true,
		StatusCancelled:     true,
		StatusVerifyPending: true, // §2.1: in_progress is treated as a composite of dispatched/running for the
		// transition pipeline; verify_pending is the §2.1-mandated next step before completed/repair_pending.
	},

	// REQUIREMENTS.md §2.1: Extended task lifecycle transitions.
	// cancelled is allowed from every non-terminal extended state for command
	// cancellation symmetry (a cancelled command must cancel all its tasks).
	StatusPlanned: {
		StatusReady:          true, // planner marks task as ready for dispatch
		StatusCancelled:      true, // command cancellation before task is ready
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusReady: {
		StatusDispatched:     true, // daemon dispatches task to worker
		StatusCancelled:      true, // command cancellation before dispatch
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusDispatched: {
		StatusRunning:        true, // worker begins execution
		StatusCancelled:      true, // command cancellation after dispatch
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusRunning: {
		StatusVerifyPending:  true, // worker completes, awaiting verification
		StatusFailed:         true, // worker reports failure
		StatusCancelled:      true, // command cancellation during execution
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusVerifyPending: {
		StatusCompleted:      true, // verification passed
		StatusRepairPending:  true, // verification failed, needs repair
		StatusCancelled:      true, // command cancellation during verification
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusRepairPending: {
		StatusRunning:         true, // repair task dispatched
		StatusCancelled:       true, // command cancellation during repair wait
		StatusPausedForReplan: true, // repair requires replanning
		StatusPausedForHuman:  true,
		StatusAborted:         true,
	},
	StatusPausedForReplan: {
		StatusReady:          true, // replanning complete, task re-enters ready
		StatusCancelled:      true, // command cancellation during replan
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusPausedForHuman: {
		StatusReady:     true, // human approves, task re-enters ready
		StatusCancelled: true, // command cancellation during human review
		StatusAborted:   true,
	},
}

var validPhaseTransitions = map[PhaseStatus]map[PhaseStatus]bool{
	PhaseStatusPending: {
		PhaseStatusAwaitingFill: true,
		PhaseStatusCancelled:    true,
		PhaseStatusFailed:       true, // fast-track stall cleanup
	},
	PhaseStatusAwaitingFill: {
		PhaseStatusFilling:  true,
		PhaseStatusTimedOut: true,
		PhaseStatusFailed:   true, // fast-track stall cleanup
	},
	PhaseStatusFilling: {
		PhaseStatusActive:       true,
		PhaseStatusAwaitingFill: true, // fill failure → back to awaiting_fill
		PhaseStatusFailed:       true, // fast-track stall cleanup
	},
	PhaseStatusActive: {
		PhaseStatusCompleted: true,
		PhaseStatusFailed:    true,
		PhaseStatusCancelled: true,
	},
	// NOTE: PhaseStatusFailed → PhaseStatusActive is handled as a special case
	// in ValidatePhaseTransition() before the map lookup, so no entry is needed here.
}

// Worktree status transitions:
//
//	created → active (sync), committed (commit without sync), conflict, failed, published (bulk publish), cleanup_done/cleanup_failed (cleanup)
//	active → committed, conflict, failed, published, cleanup_done/cleanup_failed
//	committed → active (sync back), integrated (merge success), conflict (merge conflict), failed, published, cleanup_done/cleanup_failed
//	integrated → active (cross-phase sync), published, conflict, failed, cleanup_done/cleanup_failed
//	published → cleanup_done, cleanup_failed
//	conflict → active (resolved), failed, published (bulk publish), cleanup_done/cleanup_failed
//	failed → published (bulk publish), cleanup_done, cleanup_failed
//	cleanup_done → (terminal)
//	cleanup_failed → cleanup_done (retry)
var validWorktreeTransitions = map[WorktreeStatus]map[WorktreeStatus]bool{
	WorktreeStatusCreated: {
		WorktreeStatusActive:        true,
		WorktreeStatusCommitted:     true,
		WorktreeStatusConflict:      true,
		WorktreeStatusFailed:        true,
		WorktreeStatusPublished:     true,
		WorktreeStatusCleanupDone:   true,
		WorktreeStatusCleanupFailed: true,
	},
	WorktreeStatusActive: {
		WorktreeStatusActive:        true, // multiple syncs
		WorktreeStatusCommitted:     true,
		WorktreeStatusIntegrated:    true, // merge after sync (no intermediate commit needed if commits exist)
		WorktreeStatusConflict:      true,
		WorktreeStatusFailed:        true,
		WorktreeStatusPublished:     true,
		WorktreeStatusCleanupDone:   true,
		WorktreeStatusCleanupFailed: true,
	},
	WorktreeStatusCommitted: {
		WorktreeStatusActive:        true,
		WorktreeStatusCommitted:     true, // multiple commits
		WorktreeStatusIntegrated:    true,
		WorktreeStatusConflict:      true,
		WorktreeStatusFailed:        true,
		WorktreeStatusPublished:     true,
		WorktreeStatusCleanupDone:   true,
		WorktreeStatusCleanupFailed: true,
	},
	WorktreeStatusIntegrated: {
		WorktreeStatusActive:        true,
		WorktreeStatusCommitted:     true, // cross-phase: new commits after integration without intermediate sync
		WorktreeStatusPublished:     true,
		WorktreeStatusConflict:      true,
		WorktreeStatusFailed:        true,
		WorktreeStatusCleanupDone:   true,
		WorktreeStatusCleanupFailed: true,
	},
	WorktreeStatusPublished: {
		WorktreeStatusCleanupDone:   true,
		WorktreeStatusCleanupFailed: true,
	},
	WorktreeStatusConflict: {
		WorktreeStatusActive:        true,
		WorktreeStatusResolving:     true,
		WorktreeStatusFailed:        true,
		WorktreeStatusPublished:     true,
		WorktreeStatusCleanupDone:   true,
		WorktreeStatusCleanupFailed: true,
	},
	WorktreeStatusResolving: {
		WorktreeStatusActive:        true, // resume-merge resets resolving workers to active for re-merge
		WorktreeStatusIntegrated:    true,
		WorktreeStatusConflict:      true,
		WorktreeStatusFailed:        true,
		WorktreeStatusCleanupDone:   true,
		WorktreeStatusCleanupFailed: true,
	},
	WorktreeStatusFailed: {
		WorktreeStatusPublished:     true,
		WorktreeStatusCleanupDone:   true,
		WorktreeStatusCleanupFailed: true,
	},
	WorktreeStatusCleanupFailed: {
		WorktreeStatusCleanupDone: true,
	},
}

// Integration status transitions:
//
//	created → merging, failed
//	merging → merged, conflict, failed
//	merged → merging (re-merge for next phase), publishing, failed
//	publishing → published, conflict, publish_failed, failed
//	publish_failed → publishing (retry), failed, quarantined
//	conflict → merging (retry), failed
//	failed → merging (retry after failure)
var validIntegrationTransitions = map[IntegrationStatus]map[IntegrationStatus]bool{
	IntegrationStatusCreated: {
		IntegrationStatusMerging:     true,
		IntegrationStatusFailed:      true,
		IntegrationStatusQuarantined: true,
	},
	IntegrationStatusMerging: {
		IntegrationStatusMerged:       true,
		IntegrationStatusConflict:     true,
		IntegrationStatusPartialMerge: true,
		IntegrationStatusFailed:       true,
		IntegrationStatusQuarantined:  true,
	},
	IntegrationStatusMerged: {
		IntegrationStatusMerging:     true,
		IntegrationStatusPublishing:  true,
		IntegrationStatusFailed:      true,
		IntegrationStatusQuarantined: true,
	},
	IntegrationStatusPublishing: {
		IntegrationStatusPublished:     true,
		IntegrationStatusConflict:      true,
		IntegrationStatusPublishFailed: true,
		IntegrationStatusFailed:        true,
		IntegrationStatusQuarantined:   true,
	},
	IntegrationStatusPublishFailed: {
		IntegrationStatusPublishing:  true, // retry
		IntegrationStatusMerged:      true, // retry-publish recovery
		IntegrationStatusFailed:      true,
		IntegrationStatusQuarantined: true,
	},
	IntegrationStatusPartialMerge: {
		IntegrationStatusMerging:     true,
		IntegrationStatusFailed:      true,
		IntegrationStatusQuarantined: true,
	},
	IntegrationStatusConflict: {
		IntegrationStatusMerging:     true,
		IntegrationStatusFailed:      true,
		IntegrationStatusQuarantined: true,
	},
	IntegrationStatusFailed: {
		IntegrationStatusFailed:      true, // repeated failures (e.g., dirty worktree on retry)
		IntegrationStatusMerging:     true,
		IntegrationStatusQuarantined: true,
	},
}

// IsTerminal reports whether s is a terminal task/queue status.
func IsTerminal(s Status) bool {
	return terminalStatuses[s]
}

var activeStatuses = map[Status]bool{
	StatusDispatched:    true,
	StatusRunning:       true,
	StatusVerifyPending: true,
	StatusRepairPending: true,
}

// IsActiveStatus returns true for states where a task is actively being worked on.
func IsActiveStatus(s Status) bool {
	return activeStatuses[s]
}

var pausedStatuses = map[Status]bool{
	StatusPausedForReplan: true,
	StatusPausedForHuman:  true,
}

// IsPausedStatus returns true for states where a task is paused awaiting intervention.
func IsPausedStatus(s Status) bool {
	return pausedStatuses[s]
}

// IsPlanTerminal reports whether s is a terminal plan status.
func IsPlanTerminal(s PlanStatus) bool {
	return terminalPlanStatuses[s]
}

// IsPhaseTerminal reports whether s is a terminal phase status.
func IsPhaseTerminal(s PhaseStatus) bool {
	return terminalPhaseStatuses[s]
}

// IsWorktreeTerminal reports whether s is a terminal worktree status.
func IsWorktreeTerminal(s WorktreeStatus) bool {
	return terminalWorktreeStatuses[s]
}

// IsIntegrationTerminal reports whether s is a terminal integration status.
func IsIntegrationTerminal(s IntegrationStatus) bool {
	return terminalIntegrationStatuses[s]
}

// ValidateCommandTaskQueueTransition checks whether the from→to status transition is valid for command/task queues.
func ValidateCommandTaskQueueTransition(from, to Status) error {
	if IsTerminal(from) {
		return fmt.Errorf("cannot transition from terminal status %q", from)
	}
	allowed, ok := validCommandTaskQueueTransitions[from]
	if !ok {
		return fmt.Errorf("unknown status %q", from)
	}
	if !allowed[to] {
		return fmt.Errorf("invalid command/task queue transition: %q → %q", from, to)
	}
	return nil
}

// ValidateNotificationQueueTransition checks whether the from→to status transition is valid for notification queues.
func ValidateNotificationQueueTransition(from, to Status) error {
	if IsTerminal(from) {
		return fmt.Errorf("cannot transition from terminal status %q", from)
	}
	allowed, ok := validNotificationQueueTransitions[from]
	if !ok {
		return fmt.Errorf("unknown status %q", from)
	}
	if !allowed[to] {
		return fmt.Errorf("invalid notification queue transition: %q → %q", from, to)
	}
	return nil
}

// ValidateTaskStateTransition checks whether the from→to status transition is valid for task state.
func ValidateTaskStateTransition(from, to Status) error {
	if IsTerminal(from) {
		return fmt.Errorf("cannot transition from terminal status %q", from)
	}
	// §2.1: any non-terminal state can transition to paused_for_human or aborted
	if to == StatusPausedForHuman || to == StatusAborted {
		return nil
	}
	allowed, ok := validTaskStateTransitions[from]
	if !ok {
		return fmt.Errorf("unknown status %q", from)
	}
	if !allowed[to] {
		return fmt.Errorf("invalid task state transition: %q → %q", from, to)
	}
	return nil
}

// ValidatePhaseTransition checks whether the from→to phase status transition is valid.
func ValidatePhaseTransition(from, to PhaseStatus) error {
	// Special case: failed → active is allowed for add-retry-task
	if from == PhaseStatusFailed && to == PhaseStatusActive {
		return nil
	}
	// Special case (dependency-cascade recovery): cancelled → pending is
	// allowed when the dependency resolver has detected that the upstream
	// cause of a scheduler-derived cancellation is no longer
	// terminal-failed. The reversal is restricted by the resolver to
	// cancellations whose Phase.CancelledReason carries the
	// DependencyCascadeCancelPrefix marker — operator/manual cancellations
	// (no marker) stay terminal. The validation layer here only knows the
	// from/to pair, so it cannot enforce that gate; the resolver is the
	// canonical caller and is the single producer of this transition.
	// See dependency_resolver.checkCancelledPhaseRecovery.
	if from == PhaseStatusCancelled && to == PhaseStatusPending {
		return nil
	}
	if IsPhaseTerminal(from) {
		return fmt.Errorf("cannot transition from terminal phase status %q", from)
	}
	allowed, ok := validPhaseTransitions[from]
	if !ok {
		return fmt.Errorf("unknown phase status %q", from)
	}
	if !allowed[to] {
		return fmt.Errorf("invalid phase transition: %q → %q", from, to)
	}
	return nil
}

// ValidateWorktreeTransition checks whether the from→to worktree status transition is valid.
func ValidateWorktreeTransition(from, to WorktreeStatus) error {
	if IsWorktreeTerminal(from) {
		return fmt.Errorf("cannot transition from terminal worktree status %q", from)
	}
	allowed, ok := validWorktreeTransitions[from]
	if !ok {
		return fmt.Errorf("unknown worktree status %q", from)
	}
	if !allowed[to] {
		return fmt.Errorf("invalid worktree transition: %q → %q", from, to)
	}
	return nil
}

// ValidateIntegrationTransition checks whether the from→to integration status transition is valid.
func ValidateIntegrationTransition(from, to IntegrationStatus) error {
	if IsIntegrationTerminal(from) {
		return fmt.Errorf("cannot transition from terminal integration status %q", from)
	}
	allowed, ok := validIntegrationTransitions[from]
	if !ok {
		return fmt.Errorf("unknown integration status %q", from)
	}
	if !allowed[to] {
		return fmt.Errorf("invalid integration transition: %q → %q", from, to)
	}
	return nil
}

// ValidateNotificationType reports whether t is a known notification type.
func ValidateNotificationType(t NotificationType) error {
	if !validNotificationTypes[t] {
		return fmt.Errorf("invalid notification type: %q", t)
	}
	return nil
}
