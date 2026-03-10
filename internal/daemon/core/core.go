// Package core provides shared types and interfaces used across daemon sub-packages.
// This avoids circular imports by decoupling contracts from implementations.
package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

// LogLevel represents the logging verbosity level.
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
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
	dl.slogger.Log(nil, toSlogLevel(level), fmt.Sprintf(format, args...))
}

// Log logs a message with structured attributes.
func (dl *DaemonLogger) Log(level LogLevel, msg string, attrs ...slog.Attr) {
	dl.slogger.LogAttrs(nil, toSlogLevel(level), msg, attrs...)
}

// With returns a new DaemonLogger with additional default attributes.
func (dl *DaemonLogger) With(args ...any) *DaemonLogger {
	return &DaemonLogger{slogger: dl.slogger.With(args...)}
}

// Slog returns the underlying *slog.Logger for advanced use.
func (dl *DaemonLogger) Slog() *slog.Logger {
	return dl.slogger
}

// ---------------------------------------------------------------------------
// Clock
// ---------------------------------------------------------------------------

// Clock abstracts time.Now() for deterministic testing.
type Clock interface {
	Now() time.Time
}

// RealClock is the production Clock that delegates to time.Now().
type RealClock struct{}

// Now returns the current time.
func (RealClock) Now() time.Time { return time.Now() }

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

// ErrStateNotFound is returned by StateReader methods when the state file does not exist
// (i.e., the command has not been submitted yet). Callers can use errors.Is to distinguish
// this from other read errors (e.g., parse failures on an existing file).
var ErrStateNotFound = errors.New("command state file not found")

// ErrPhaseNotFound is returned when a specific phase ID is not present in a command's
// phase list. Unlike ErrStateNotFound (state file missing), this means the state file
// exists but the requested phase does not.
var ErrPhaseNotFound = errors.New("command phase not found")

// StateReader provides read access to command state (state/commands/{command_id}.yaml).
// Phase 6 implements the concrete version; Phase 5 uses this interface for decoupling.
type StateReader interface {
	// GetTaskState returns the status of a task from the command state.
	GetTaskState(commandID, taskID string) (model.Status, error)
	// GetCommandPhases returns phases for a command.
	GetCommandPhases(commandID string) ([]PhaseInfo, error)
	// GetTaskDependencies returns task IDs that the given task depends on.
	GetTaskDependencies(commandID, taskID string) ([]string, error)
	// IsSystemCommitReady checks if the given task is a system commit task and whether
	// all user phases (or user tasks for non-phased commands) are terminal.
	// Returns (isSystemCommit=false, ready=false, nil) for non-system-commit tasks.
	IsSystemCommitReady(commandID, taskID string) (isSystemCommit bool, ready bool, err error)
	// ApplyPhaseTransition persists a phase status change to state/commands/.
	ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error
	// UpdateTaskState updates a single task's status and optionally records a cancelled reason.
	UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error
	// IsCommandCancelRequested checks the state file for cancel.requested flag.
	IsCommandCancelRequested(commandID string) (bool, error)
	// GetCircuitBreakerState returns the circuit breaker state for a command.
	GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error)
	// TripCircuitBreaker sets the circuit breaker to tripped and issues a cancel request on the command.
	// progressTimeoutMinutes is re-validated under lock to prevent TOCTOU race; pass 0 to skip re-validation.
	TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error
}

// PhaseInfo represents phase metadata from command state.
type PhaseInfo struct {
	ID               string
	Name             string
	Status           model.PhaseStatus
	DependsOn        []string // phase IDs
	FillDeadlineAt   *string
	RequiredTaskIDs  []string
	SystemCommitTask bool
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// ExecutorFactory creates agent executors. Allows testing without tmux.
type ExecutorFactory func(maestroDir string, watcherCfg model.WatcherConfig, logLevel string) (AgentExecutor, error)

// AgentExecutor is the interface for agent message delivery.
type AgentExecutor interface {
	Execute(req agent.ExecRequest) agent.ExecResult
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

// PlanExecutor executes plan operations under the daemon's file lock.
// Implementations are wired from main.go to avoid import cycles (plan -> daemon).
type PlanExecutor interface {
	Submit(params json.RawMessage) (json.RawMessage, error)
	Complete(params json.RawMessage) (json.RawMessage, error)
	AddRetryTask(params json.RawMessage) (json.RawMessage, error)
	Rebuild(params json.RawMessage) (json.RawMessage, error)
}
