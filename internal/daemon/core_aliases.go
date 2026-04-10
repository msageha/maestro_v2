package daemon

// This file re-exports types from internal/daemon/core so that existing code
// referencing daemon.LogLevel, daemon.Clock, etc. continues to compile.

import (
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/daemon/worktree"
)

// --- Logging aliases ---

// LogLevel is an alias for core.LogLevel.
type LogLevel = core.LogLevel

const (
	// LogLevelDebug enables verbose debug logging.
	LogLevelDebug = core.LogLevelDebug
	// LogLevelInfo is the standard informational log level.
	LogLevelInfo = core.LogLevelInfo
	// LogLevelWarn indicates a potentially harmful situation.
	LogLevelWarn = core.LogLevelWarn
	// LogLevelError indicates a serious problem that requires attention.
	LogLevelError = core.LogLevelError
)

// DaemonLogger is an alias for core.DaemonLogger.
// The name retains the Daemon prefix for backward compatibility with existing callers.
type DaemonLogger = core.DaemonLogger //nolint:revive // stuttering name kept for backward compatibility

// NewDaemonLogger is an alias for core.NewDaemonLogger.
var (
	NewDaemonLogger           = core.NewDaemonLogger
	NewDaemonLoggerFromLegacy = core.NewDaemonLoggerFromLegacy
)

// parseLogLevel bridges the previously unexported function.
var parseLogLevel = core.ParseLogLevel

// --- Clock aliases ---

// Clock is an alias for core.Clock.
type Clock = core.Clock

// RealClock is an alias for core.RealClock.
type RealClock = core.RealClock

// --- State aliases ---

// StateReader is an alias for core.StateReader.
type StateReader = core.StateReader

// PhaseInfo is an alias for core.PhaseInfo.
type PhaseInfo = core.PhaseInfo

// ErrStateNotFound is an alias for core.ErrStateNotFound.
var (
	ErrStateNotFound = core.ErrStateNotFound
	ErrPhaseNotFound = core.ErrPhaseNotFound
)

// --- Executor aliases ---

// ExecutorFactory is an alias for core.ExecutorFactory.
type ExecutorFactory = core.ExecutorFactory

// AgentExecutor is an alias for core.AgentExecutor.
type AgentExecutor = core.AgentExecutor

var errExecutorInit = core.ErrExecutorInit

// --- Function type aliases ---

// CanCompleteFunc is an alias for core.CanCompleteFunc.
type CanCompleteFunc = core.CanCompleteFunc

// PlanExecutor is an alias for core.PlanExecutor.
type PlanExecutor = core.PlanExecutor

// --- Worktree aliases ---

// WorktreeManager is an alias for worktree.Manager.
type WorktreeManager = worktree.Manager

// NewWorktreeManager is an alias for worktree.NewManager.
var NewWorktreeManager = worktree.NewManager

// CommitPolicyViolation is an alias for worktree.CommitPolicyViolation.
type CommitPolicyViolation = worktree.CommitPolicyViolation

// --- Learnings aliases ---
