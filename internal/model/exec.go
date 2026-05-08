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
	// RunOnMain marks tasks that must run against the merged main branch
	// (e.g. final verification). When true, the executor sets the pane-scoped
	// tmux user variable @run_on_main=1 before delivery so the PreToolUse
	// hook can deny Write/Edit operations — main branch is read-only for
	// Workers in this mode (Worker safety).
	RunOnMain bool
}

// ExecResult contains the outcome of an execution attempt.
type ExecResult struct {
	Success   bool
	Retryable bool
	Error     error
}
