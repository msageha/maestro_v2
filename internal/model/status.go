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
	ContinuousStatusRunning ContinuousStatus = "running"
	ContinuousStatusPaused  ContinuousStatus = "paused"
	ContinuousStatusStopped ContinuousStatus = "stopped"
)

var terminalStatuses = map[Status]bool{
	StatusCompleted:  true,
	StatusFailed:     true,
	StatusCancelled:  true,
	StatusDeadLetter: true,
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
}

var validPlanTransitions = map[PlanStatus]map[PlanStatus]bool{
	PlanStatusPlanning: {
		PlanStatusSealed: true,
	},
	PlanStatusSealed: {
		PlanStatusCompleted: true,
		PlanStatusFailed:    true,
		PlanStatusCancelled: true,
	},
}

var validPhaseTransitions = map[PhaseStatus]map[PhaseStatus]bool{
	PhaseStatusPending: {
		PhaseStatusAwaitingFill: true,
		PhaseStatusCancelled:    true,
	},
	PhaseStatusAwaitingFill: {
		PhaseStatusFilling:  true,
		PhaseStatusTimedOut: true,
	},
	PhaseStatusFilling: {
		PhaseStatusActive:       true,
		PhaseStatusAwaitingFill: true, // fill failure → back to awaiting_fill
	},
	PhaseStatusActive: {
		PhaseStatusCompleted: true,
		PhaseStatusFailed:    true,
		PhaseStatusCancelled: true,
	},
	// add-retry-task can reopen failed → active
	PhaseStatusFailed: {
		PhaseStatusActive: true,
	},
}

func IsTerminal(s Status) bool {
	return terminalStatuses[s]
}

func IsPlanTerminal(s PlanStatus) bool {
	return terminalPlanStatuses[s]
}

func IsPhaseTerminal(s PhaseStatus) bool {
	return terminalPhaseStatuses[s]
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
	allowed, ok := validTaskStateTransitions[from]
	if !ok {
		return fmt.Errorf("unknown status %q", from)
	}
	if !allowed[to] {
		return fmt.Errorf("invalid task state transition: %q → %q", from, to)
	}
	return nil
}

func ValidatePlanTransition(from, to PlanStatus) error {
	if IsPlanTerminal(from) {
		return fmt.Errorf("cannot transition from terminal plan status %q", from)
	}
	allowed, ok := validPlanTransitions[from]
	if !ok {
		return fmt.Errorf("unknown plan status %q", from)
	}
	if !allowed[to] {
		return fmt.Errorf("invalid plan transition: %q → %q", from, to)
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
