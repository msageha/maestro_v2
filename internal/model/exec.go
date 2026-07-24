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
	// ModeCheckAgentError probes the pane (read-only, no message sent) for a
	// visible agent-runtime API error banner (e.g. Claude Code's "API Error:
	// ..." rejection). Used by dispatch-stuck recovery to distinguish "the
	// agent is alive but never processed the message because the runtime
	// rejected it" from a genuinely wedged/dead pane, so the operator-facing
	// log names the real cause instead of a generic timeout.
	ModeCheckAgentError ExecMode = "check_agent_error"
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
	// ResumeMessage, when non-empty on a ModeWithClear request, asks the
	// executor to attempt an in-place continuation: if the target pane still
	// holds the same agent process and the same task's conversation
	// (clear_ready set, pane PID unchanged, agent process alive,
	// @last_task_id == TaskID, cwd match), this short nudge is delivered
	// WITHOUT /clear so the worker keeps its accumulated context after a
	// mid-stream turn interruption (issue #55). When any session-identity
	// check fails, the executor falls back to the normal /clear + Message
	// full delivery (fail-safe).
	ResumeMessage string
}

// ExecResult contains the outcome of an execution attempt.
type ExecResult struct {
	Success   bool
	Retryable bool
	Error     error
}
