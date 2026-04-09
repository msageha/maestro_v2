package model

import "fmt"

type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
	StatusDeadLetter Status = "dead_letter"

	// REQUIREMENTS.md §2.1: Extended task lifecycle states
	StatusPlanned          Status = "planned"
	StatusReady            Status = "ready"
	StatusDispatched       Status = "dispatched"
	StatusRunning          Status = "running"
	StatusVerifyPending    Status = "verify_pending"
	StatusRepairPending    Status = "repair_pending"
	StatusPausedForReplan  Status = "paused_for_replan"
	StatusPausedForHuman   Status = "paused_for_human"
	StatusAborted          Status = "aborted"
)

type PlanStatus string

const (
	PlanStatusPlanning  PlanStatus = "planning"
	PlanStatusSealed    PlanStatus = "sealed"
	PlanStatusCompleted PlanStatus = "completed"
	PlanStatusFailed    PlanStatus = "failed"
	PlanStatusCancelled PlanStatus = "cancelled"
)

type PhaseStatus string

const (
	PhaseStatusPending      PhaseStatus = "pending"
	PhaseStatusAwaitingFill PhaseStatus = "awaiting_fill"
	PhaseStatusFilling      PhaseStatus = "filling"
	PhaseStatusActive       PhaseStatus = "active"
	PhaseStatusCompleted    PhaseStatus = "completed"
	PhaseStatusFailed       PhaseStatus = "failed"
	PhaseStatusCancelled    PhaseStatus = "cancelled"
	PhaseStatusTimedOut     PhaseStatus = "timed_out"
)

type ContinuousStatus string

const (
	continuousStatusIdle    ContinuousStatus = "idle"
	ContinuousStatusRunning ContinuousStatus = "running"
	ContinuousStatusPaused  ContinuousStatus = "paused"
	ContinuousStatusStopped ContinuousStatus = "stopped"
)

// NotificationType represents the type of an orchestrator notification.
type NotificationType string

const (
	NotificationTypeCommandCompleted NotificationType = "command_completed"
	NotificationTypeCommandFailed    NotificationType = "command_failed"
	NotificationTypeCommandCancelled NotificationType = "command_cancelled"
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
	NotificationTypeCommandCompleted: true,
	NotificationTypeCommandFailed:    true,
	NotificationTypeCommandCancelled: true,
}

// Queue entry status transitions for command/task: pending ↔ in_progress → terminal
// dead_letter only from pending (daemon detects attempts >= max_attempts before dispatch)
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

// Notification queue transitions: terminal states are completed|dead_letter only
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

// Task state transitions (in state/commands/)
var validTaskStateTransitions = map[Status]map[Status]bool{
	StatusPending: {
		StatusInProgress: true,
		StatusCancelled:  true,
	},
	StatusInProgress: {
		StatusCompleted: true,
		StatusFailed:    true,
		StatusCancelled: true,
	},

	// REQUIREMENTS.md §2.1: Extended task lifecycle transitions
	StatusPlanned: {
		StatusReady:          true,
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusReady: {
		StatusDispatched:     true,
		StatusCancelled:      true,
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusDispatched: {
		StatusRunning:        true,
		StatusCancelled:      true,
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusRunning: {
		StatusVerifyPending:  true,
		StatusFailed:         true,
		StatusCancelled:      true,
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusVerifyPending: {
		StatusCompleted:      true,
		StatusRepairPending:  true,
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusRepairPending: {
		StatusRunning:         true,
		StatusPausedForReplan: true,
		StatusPausedForHuman:  true,
		StatusAborted:         true,
	},
	StatusPausedForReplan: {
		StatusReady:          true,
		StatusPausedForHuman: true,
		StatusAborted:        true,
	},
	StatusPausedForHuman: {
		StatusReady:   true,
		StatusAborted: true,
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
//   created → active (sync), committed (commit without sync), conflict, failed, published (bulk publish), cleanup_done/cleanup_failed (cleanup)
//   active → committed, conflict, failed, published, cleanup_done/cleanup_failed
//   committed → active (sync back), integrated (merge success), conflict (merge conflict), failed, published, cleanup_done/cleanup_failed
//   integrated → active (cross-phase sync), published, conflict, failed, cleanup_done/cleanup_failed
//   published → cleanup_done, cleanup_failed
//   conflict → active (resolved), failed, published (bulk publish), cleanup_done/cleanup_failed
//   failed → published (bulk publish), cleanup_done, cleanup_failed
//   cleanup_done → (terminal)
//   cleanup_failed → cleanup_done (retry)
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
//   created → merging, failed
//   merging → merged, conflict, failed
//   merged → merging (re-merge for next phase), publishing, failed
//   publishing → published, conflict, failed
//   conflict → merging (retry), failed
//   failed → merging (retry after failure)
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
		IntegrationStatusPublished:   true,
		IntegrationStatusConflict:    true,
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

func IsPlanTerminal(s PlanStatus) bool {
	return terminalPlanStatuses[s]
}

func IsPhaseTerminal(s PhaseStatus) bool {
	return terminalPhaseStatuses[s]
}

func isWorktreeTerminal(s WorktreeStatus) bool {
	return terminalWorktreeStatuses[s]
}

func IsIntegrationTerminal(s IntegrationStatus) bool {
	return terminalIntegrationStatuses[s]
}

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

func ValidatePhaseTransition(from, to PhaseStatus) error {
	// Special case: failed → active is allowed for add-retry-task
	if from == PhaseStatusFailed && to == PhaseStatusActive {
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

func ValidateWorktreeTransition(from, to WorktreeStatus) error {
	if isWorktreeTerminal(from) {
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

func ValidateNotificationType(t NotificationType) error {
	if !validNotificationTypes[t] {
		return fmt.Errorf("invalid notification type: %q", t)
	}
	return nil
}
