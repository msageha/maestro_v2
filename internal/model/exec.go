package model

import "context"

// ExecMode represents the agent execution mode.
type ExecMode string

const (
	// ModeDeliver sends a message with busy check (used by Planner/Orchestrator).
	ModeDeliver ExecMode = "deliver"
	// ModeWithClear sends /clear before delivery (used by Workers).
	ModeWithClear ExecMode = "with_clear"
	// ModeInterrupt interrupts a running task.
	ModeInterrupt ExecMode = "interrupt"
	// ModeIsBusy queries the busy state without delivering.
	ModeIsBusy ExecMode = "is_busy"
	// ModeClear resets context without delivery.
	ModeClear ExecMode = "clear"
)

// ExecRequest contains parameters for executing a message delivery.
type ExecRequest struct {
	Context    context.Context // nil defaults to context.Background()
	AgentID    string
	Message    string
	Mode       ExecMode
	TaskID     string
	CommandID  string
	LeaseEpoch int
	Attempt    int
	WorkingDir string // Target working directory (worktree mode). Empty = no change.
}

// ExecResult contains the outcome of an execution attempt.
type ExecResult struct {
	Success   bool
	Retryable bool
	Error     error
}
