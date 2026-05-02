// Package core provides shared types and interfaces used across daemon sub-packages.
// This avoids circular imports by decoupling contracts from implementations.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"

	"github.com/msageha/maestro_v2/internal/clock"
	"github.com/msageha/maestro_v2/internal/contract"
	"github.com/msageha/maestro_v2/internal/model"
)

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

// LogLevel represents the logging verbosity level.
type LogLevel int

const (
	// LogLevelDebug enables verbose debug logging.
	LogLevelDebug LogLevel = iota
	// LogLevelInfo enables informational logging.
	LogLevelInfo
	// LogLevelWarn enables warning and error logging only.
	LogLevelWarn
	// LogLevelError enables error logging only.
	LogLevelError
)

// ParseLogLevel converts a string to a LogLevel.
func ParseLogLevel(s string) LogLevel {
	switch strings.ToLower(s) {
	case "debug":
		return LogLevelDebug
	case "info":
		return LogLevelInfo
	case "warn", "warning":
		return LogLevelWarn
	case "error":
		return LogLevelError
	default:
		slog.Warn("unknown log level, defaulting to info", "level", s)
		return LogLevelInfo
	}
}

// toSlogLevel maps the LogLevel to slog.Level.
func toSlogLevel(l LogLevel) slog.Level {
	switch l {
	case LogLevelDebug:
		return slog.LevelDebug
	case LogLevelInfo:
		return slog.LevelInfo
	case LogLevelWarn:
		return slog.LevelWarn
	case LogLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// DaemonLogger provides structured logging for daemon components.
// It wraps *slog.Logger with a component name baked in.
type DaemonLogger struct {
	slogger *slog.Logger
}

// NewDaemonLogger creates a DaemonLogger for the given component.
// handler is the slog.Handler to use (e.g. slog.NewTextHandler).
func NewDaemonLogger(component string, handler slog.Handler) *DaemonLogger {
	return &DaemonLogger{
		slogger: slog.New(handler).With("component", component),
	}
}

// NewDaemonLoggerFromLegacy creates a DaemonLogger that bridges a legacy *log.Logger.
// This allows gradual migration: existing callers keep their *log.Logger and LogLevel,
// and DaemonLogger translates to slog internally.
func NewDaemonLoggerFromLegacy(component string, logger *log.Logger, minLevel LogLevel) *DaemonLogger {
	w := logger.Writer()
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: toSlogLevel(minLevel),
	})
	return NewDaemonLogger(component, handler)
}

// Logf logs a formatted message at the given level (migration shim for existing log() calls).
func (dl *DaemonLogger) Logf(level LogLevel, format string, args ...any) {
	if dl == nil || dl.slogger == nil {
		return
	}
	dl.slogger.Log(context.Background(), toSlogLevel(level), fmt.Sprintf(format, args...))
}

// Log logs a message with structured attributes.
func (dl *DaemonLogger) Log(level LogLevel, msg string, attrs ...slog.Attr) {
	if dl == nil || dl.slogger == nil {
		return
	}
	dl.slogger.LogAttrs(context.Background(), toSlogLevel(level), msg, attrs...)
}

// With returns a new DaemonLogger with additional default attributes.
func (dl *DaemonLogger) With(args ...any) *DaemonLogger {
	if dl == nil || dl.slogger == nil {
		return nil
	}
	return &DaemonLogger{slogger: dl.slogger.With(args...)}
}

// Slog returns the underlying *slog.Logger for advanced use.
func (dl *DaemonLogger) Slog() *slog.Logger {
	if dl == nil || dl.slogger == nil {
		return nil
	}
	return dl.slogger
}

// ---------------------------------------------------------------------------
// LogMixin
// ---------------------------------------------------------------------------

// LogMixin is an embeddable helper that provides a Log() convenience method
// for daemon components. Embed it to eliminate repeated log() boilerplate.
type LogMixin struct {
	DL *DaemonLogger
}

// Log logs a formatted message at the given level.
func (m *LogMixin) Log(level LogLevel, format string, args ...any) {
	if m.DL == nil {
		return
	}
	m.DL.Logf(level, format, args...)
}

// ---------------------------------------------------------------------------
// Clock
// ---------------------------------------------------------------------------

// Clock is an alias for clock.Clock kept here for backwards compatibility.
// New code should import internal/clock directly.
type Clock = clock.Clock

// RealClock is an alias for clock.RealClock kept here for backwards compatibility.
type RealClock = clock.RealClock

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

// StateReader provides read access to command state (state/commands/{command_id}.yaml).
// Phase 6 implements the concrete version; Phase 5 uses this interface for decoupling.
type StateReader interface {
	// GetTaskState returns the status of a task from the command state.
	GetTaskState(commandID, taskID string) (model.Status, error)
	// GetEffectiveTaskStatus returns the status of a task after walking
	// retry_lineage forward to the latest descendant. When a task has been
	// superseded by a successful retry/repair (cancelled with a
	// "superseded_by_*" reason), callers that want to know "is this
	// lineage effectively satisfied?" should consult the successor's
	// status rather than the raw predecessor status. Falls back to the
	// raw GetTaskState behaviour when no lineage entry exists.
	GetEffectiveTaskStatus(commandID, taskID string) (model.Status, error)
	// GetEffectiveTaskStatusForCompletion returns the status of a task
	// through the completion-aware lens: like GetEffectiveTaskStatus, but
	// additionally unwinds cascade-cancellations (CancelledReasons[..] of
	// the form "blocked_dependency_terminal:<dep>") whose upstream lineage
	// has effectively completed. Used by phase- and plan-level completion
	// checks so a downstream cascade-straggler whose required predecessor
	// was delivered via verify-repair does not block plan completion. NOT
	// safe for dispatch-time predicates — see EffectiveStatusForCompletion.
	GetEffectiveTaskStatusForCompletion(commandID, taskID string) (model.Status, error)
	// GetCommandPhases returns phases for a command.
	GetCommandPhases(commandID string) ([]PhaseInfo, error)
	// GetTaskDependencies returns task IDs that the given task depends on.
	GetTaskDependencies(commandID, taskID string) ([]string, error)
	// IsSystemCommitReady checks if the given task is a system commit task and whether
	// all user phases (or user tasks for non-phased commands) are terminal.
	// Returns (isSystemCommit=false, ready=false, nil) for non-system-commit tasks.
	IsSystemCommitReady(commandID, taskID string) (isSystemCommit bool, ready bool, err error)
	// IsCommandCancelRequested checks the state file for cancel.requested flag.
	IsCommandCancelRequested(commandID string) (bool, error)
	// GetCircuitBreakerState returns the circuit breaker state for a command.
	GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error)
	// HasNonTerminalTaskState reports whether any task in the command state
	// is at a non-terminal status (paused_for_replan, repair_pending,
	// verify_pending, etc.). The queue side may show all tasks at terminal
	// status (e.g. completed/failed) while the state side still tracks an
	// in-flight resolution path the daemon owes the Planner — fast-track
	// cleanup must observe both sides to avoid deleting a worktree that a
	// pending retry-task dispatch will need.
	HasNonTerminalTaskState(commandID string) (bool, error)
	// GetNonTerminalTaskStates returns a snapshot of every TaskStates entry
	// at a non-terminal status, keyed by task ID. Used by the phantom-task
	// detector in fast-track cleanup: when state has non-terminal entries
	// whose IDs are absent from every worker queue file (state-only retry
	// tasks that would otherwise leave a Phase permanently blocked), the
	// cleanup step force-fails them so phase progression can resume.
	// Returns ErrStateNotFound when the state file does not exist.
	GetNonTerminalTaskStates(commandID string) (map[string]model.Status, error)
}

// StateWriter provides write access to command state.
type StateWriter interface {
	// ApplyPhaseTransition persists a phase status change to state/commands/.
	ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error
	// SetPhaseCancelledReason persists Phase.CancelledReason for a phase.
	// Pass nil to clear the field. Used by the dependency resolver to
	// distinguish cascade-cancellations (auto-recoverable) from
	// operator/manual cancels — see model.DependencyCascadeCancelPrefix.
	// Idempotent: setting the same value twice is a no-op write.
	SetPhaseCancelledReason(commandID, phaseID string, reason *string) error
	// UpdateTaskState updates a single task's status and optionally records a cancelled reason.
	UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error
	// TripCircuitBreaker sets the circuit breaker to tripped and issues a cancel request on the command.
	// progressTimeoutMinutes is re-validated under lock to prevent TOCTOU race; pass 0 to skip re-validation.
	TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error
	// MarkAwaitingFillStallNotified records that the awaiting-fill watchdog
	// has emitted a stall signal for the given phase, so subsequent scan
	// cycles do not re-fire the same signal until the phase transitions
	// out of awaiting_fill (which clears the marker via ApplyPhaseTransition).
	// No-op when the phase is no longer at awaiting_fill (the watchdog
	// observation is stale by the time the write reaches state).
	MarkAwaitingFillStallNotified(commandID, phaseID, notifiedAt string) error
	// MarkCircuitBreakerProgress refreshes circuit_breaker.last_progress_at
	// when the daemon observes a liveness signal that does NOT come from
	// task completion (e.g. the worker pane shows cross-scan activity).
	// Without this hook a long-running task whose execution legitimately
	// exceeds progress_timeout_minutes would trip the breaker on the
	// progress-timeout path even though the worker is visibly working.
	// Idempotent: ErrStateNotFound is silently ignored — the breaker
	// can only meaningfully advance when state already exists.
	MarkCircuitBreakerProgress(commandID string) error
}

// StateManager combines StateReader and StateWriter for components that need both
// read and write access to command state.
type StateManager interface {
	StateReader
	StateWriter
}

// PhaseInfo is an alias for model.PhaseInfo.
// Kept for backward compatibility with existing callers.
type PhaseInfo = model.PhaseInfo

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// ExecutorFactory creates agent executors. Allows testing without tmux.
type ExecutorFactory func(maestroDir string, watcherCfg model.WatcherConfig, logLevel string) (AgentExecutor, error)

// AgentExecutor is the interface for agent message delivery.
//
// RespawnPaneToProjectRoot is the lifecycle hook the daemon's Phase B
// invokes before tearing down a command's worktree directory. The Worker
// pane's cwd is the worktree path, and removing it out from under a still
// running claude-code process produces "ENOENT, posix_spawn '/bin/sh'"
// from any subsequent hook (Stop hook in particular) that node.js runs
// with the now-deleted cwd. Respawning the pane to the project root
// before `git worktree remove` keeps the pane in a real directory across
// the cleanup transition. Implementations MUST be a no-op for unknown
// worker IDs and tolerate "no pane found" — daemons may call this for
// workers whose pane was never started.
type AgentExecutor interface {
	Execute(req model.ExecRequest) model.ExecResult
	RespawnPaneToProjectRoot(workerID string) error
	Close() error
}

// ErrExecutorInit is a sentinel returned by getExecutor when the cached executor
// failed to initialise (sync.Once captured the error on first call).
var ErrExecutorInit = errors.New("agent executor initialization failed")

// ---------------------------------------------------------------------------
// Function types (dependency inversion)
// ---------------------------------------------------------------------------

// CanCompleteFunc is the signature for plan.CanComplete to avoid import cycles.
type CanCompleteFunc func(state *model.CommandState) (model.PlanStatus, error)

// DeferredPlanCompleterFunc attempts to complete a plan that was deferred pending
// worktree publish. Returns (true, nil) if a deferred intent was found and
// completion succeeded, (false, nil) if no deferred intent exists, or
// (false, error) if completion failed.
type DeferredPlanCompleterFunc func(commandID string) (bool, error)

// PlanExecutor executes plan operations under the daemon's file lock.
// Implementations are wired from main.go to avoid import cycles (plan -> daemon).
type PlanExecutor interface {
	Submit(params json.RawMessage) (json.RawMessage, error)
	Complete(params json.RawMessage) (json.RawMessage, error)
	AddRetryTask(params json.RawMessage) (json.RawMessage, error)
	AddTask(params json.RawMessage) (json.RawMessage, error)
	Rebuild(params json.RawMessage) (json.RawMessage, error)
}

// ModelSelector is the method set an adaptive model selector must expose to
// participate in worker assignment. Aliased to contract.ModelSelector so
// daemon/core and plan share a single definition; the bridge layer can
// then hand a selector across the boundary without an adapter shim.
type ModelSelector = contract.ModelSelector

// PlanExecutorModelSelectorSettable is an optional extension of PlanExecutor
// that lets the daemon inject an adaptive model selector post-startup.
// Executors that do not implement it are left on the static mapping.
type PlanExecutorModelSelectorSettable interface {
	SetModelSelector(ModelSelector)
}
