package daemon

// This file re-exports types from internal/daemon/core so that existing code
// referencing daemon.LogLevel, daemon.Clock, etc. continues to compile.

import (
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/daemon/dashboard"
	"github.com/msageha/maestro_v2/internal/daemon/deadletter"
	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/daemon/qualitygate"
	"github.com/msageha/maestro_v2/internal/daemon/worktree"
)

// --- Logging aliases ---

type LogLevel = core.LogLevel

const (
	LogLevelDebug = core.LogLevelDebug
	LogLevelInfo  = core.LogLevelInfo
	LogLevelWarn  = core.LogLevelWarn
	LogLevelError = core.LogLevelError
)

type DaemonLogger = core.DaemonLogger

var (
	NewDaemonLogger           = core.NewDaemonLogger
	NewDaemonLoggerFromLegacy = core.NewDaemonLoggerFromLegacy
)

// parseLogLevel bridges the previously unexported function.
var parseLogLevel = core.ParseLogLevel

// --- Clock aliases ---

type Clock = core.Clock
type RealClock = core.RealClock

// --- State aliases ---

type StateReader = core.StateReader
type PhaseInfo = core.PhaseInfo

var (
	ErrStateNotFound = core.ErrStateNotFound
	ErrPhaseNotFound = core.ErrPhaseNotFound
)

// --- Executor aliases ---

type ExecutorFactory = core.ExecutorFactory
type AgentExecutor = core.AgentExecutor

var errExecutorInit = core.ErrExecutorInit

// --- Function type aliases ---

type CanCompleteFunc = core.CanCompleteFunc
type PlanExecutor = core.PlanExecutor

// --- Worktree aliases ---

type WorktreeManager = worktree.Manager

var NewWorktreeManager = worktree.NewManager

type CommitPolicyViolation = worktree.CommitPolicyViolation

// --- Circuit Breaker aliases ---

type CircuitBreakerHandler = circuitbreaker.Handler

var NewCircuitBreakerHandler = circuitbreaker.NewHandler

// --- Learnings aliases ---

var readTopKLearnings = learnings.ReadTopKLearnings
var formatLearningsSection = learnings.FormatLearningsSection

// --- Dead Letter aliases ---

type DeadLetterProcessor = deadletter.Processor
type DeadLetterResult = deadletter.Result

var NewDeadLetterProcessor = deadletter.NewProcessor

// --- Quality Gate aliases ---

type QualityGateDaemon = qualitygate.Daemon
type QualityGateEvent = qualitygate.Event
type TaskStartEvent = qualitygate.TaskStartEvent
type TaskCompleteEvent = qualitygate.TaskCompleteEvent
type PhaseTransitionEvent = qualitygate.PhaseTransitionEvent
type QualityGateMetrics = qualitygate.Metrics

var NewQualityGateDaemon = qualitygate.NewDaemon

// --- Dashboard aliases ---

type DashboardFormatter = dashboard.Formatter
type DashboardStats = dashboard.Stats
type DashboardEvent = dashboard.Event
type DashboardData = dashboard.Data
type DashboardQueueInfo = dashboard.QueueInfo
type DashboardAgentInfo = dashboard.AgentInfo
type ActiveCommandInfo = dashboard.ActiveCommandInfo
type WorkerSummary = dashboard.WorkerSummary

var NewDashboardFormatter = dashboard.NewFormatter
