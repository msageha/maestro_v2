package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/daemon/fallback"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
)

// PhaseDiagnoserFunc produces a diagnosis prompt string from a completed phase's tasks.
// Returns "" if diagnosis yields no actionable information.
type PhaseDiagnoserFunc func(phase model.Phase, tasks []model.Task, results []model.TaskResult) string

// BusyChecker probes whether an agent is currently busy.
// Implementations are used to override the default executor-based probe in tests.
type BusyChecker interface {
	IsBusy(agentID string) bool
}

// BusyCheckerFunc adapts a plain function to the BusyChecker interface.
type BusyCheckerFunc func(agentID string) bool

// IsBusy calls the underlying function to check whether the given agent is busy.
func (f BusyCheckerFunc) IsBusy(agentID string) bool { return f(agentID) }

// QueueHandlerOption configures a QueueHandler after construction.
type QueueHandlerOption func(*QueueHandler)

// WithBusyChecker injects a BusyChecker to override the default executor-based probe.
func WithBusyChecker(bc BusyChecker) QueueHandlerOption {
	return func(qh *QueueHandler) {
		qh.scanExecutor.busyChecker = bc
	}
}

// QueueHandler orchestrates fsnotify event routing and periodic scan execution.
type QueueHandler struct {
	maestroDir string
	config     model.Config
	dl         *DaemonLogger
	logger     *log.Logger
	logLevel   LogLevel
	clock      Clock

	execProvider          *ExecutorProvider // shared across dispatcher, resultHandler, cancelHandler
	queueStore            QueueStore
	leaseManager          QueueLeaseManager
	dispatcher            QueueDispatcher
	dependencyResolver    QueueDependencyResolver
	cancelHandler         *CancelHandler
	resultHandler         *ResultHandler
	reconciler            *Reconciler
	deadLetterProcessor   *DeadLetterProcessor
	metricsHandler        *metrics.Handler
	circuitBreaker        *circuitbreaker.Handler
	admissionCtrl         *admission.Controller
	fallbackMgr           *fallback.Manager
	worktreeManager       QueueWorktreeManager
	deferredPlanCompleter DeferredPlanCompleterFunc
	lockMap               *lock.MutexMap

	// scanExecutor handles periodic scan orchestration and scan-specific state.
	scanExecutor *ScanPhaseExecutor

	// scanRunMu exposes the scan run mutex for test cleanup synchronization.
	scanRunMu *sync.Mutex

	// daemonPID for lease_owner format "daemon:{pid}" per spec §5.8.1.
	daemonPID int

	// initMu protects Set* initialization methods independently from scan execution.
	// All Set* methods in handler_registry.go acquire this mutex instead of scanRunMu.
	initMu sync.Mutex

	// Shutdown guard: wired via SetShutdownGuard after construction.
	shutdownCtx  context.Context
	shuttingDown *atomic.Bool

	// sessionLost is set when the tmux session disappears. When true,
	// dispatch of new tasks/commands is paused.
	sessionLost *atomic.Bool

	// undecidedTracker tracks consecutive undecided busy-probe results per agent.
	undecidedTracker *undecidedTracker

	// timeCache caches time.Parse(time.RFC3339, ...) results within a scan
	// cycle to avoid repeated parsing of identical timestamp strings.
	timeCache *timeParseCache

	// phaseC exposes Phase C components (complexity scoring, feature gating,
	// bandit, fingerprint DB, etc.) to the dispatch pipeline. Wired via
	// SetPhaseCManager after daemon startup; nil-safe at all call sites.
	phaseC *PhaseCManager

	// consecutiveCascadeBreakScans tracks how many recent scan ticks ended
	// with the signal cascade-break tripped. Persists across scans (the
	// per-tick signalCascadeTracker is local to stepDeliverSignals) so the
	// daemon can surface the meta-circuit "tmux delivery has been degraded
	// for N consecutive ticks" instead of only the per-tick message. The
	// historical R-2 gap from the 2026-04 review was that single-tick
	// cascade-break did not escalate when the underlying tmux degradation
	// persisted across many ticks; this counter is the primitive an
	// operator-facing meta-circuit reads.
	consecutiveCascadeBreakScans atomic.Int32
}

// NewQueueHandler creates a new QueueHandler with all sub-modules.
// A single shared ExecutorProvider is created and injected into all handlers
// that need executor access (Dispatcher, ResultHandler, CancelHandler).
func NewQueueHandler(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel, opts ...QueueHandlerOption) *QueueHandler {
	components := newQueueComponents(maestroDir, cfg, lockMap, logger, logLevel)
	qh := &QueueHandler{
		maestroDir:          maestroDir,
		config:              cfg,
		dl:                  components.dl,
		logger:              logger,
		logLevel:            logLevel,
		clock:               components.clock,
		execProvider:        components.execProvider,
		queueStore:          components.queueStore,
		leaseManager:        components.leaseManager,
		dispatcher:          components.dispatcher,
		dependencyResolver:  components.dependencyResolver,
		cancelHandler:       components.cancelHandler,
		resultHandler:       components.resultHandler,
		reconciler:          components.reconciler,
		deadLetterProcessor: components.deadLetterProcessor,
		metricsHandler:      components.metricsHandler,
		lockMap:             lockMap,
		daemonPID:           os.Getpid(),
	}
	qh.undecidedTracker = newUndecidedTracker()
	qh.timeCache = newTimeParseCache()
	se := newScanPhaseExecutor(qh)
	se.debounce = NewDebounceController(cfg.Watcher.DebounceSec, components.dl, qh.PeriodicScanWithContext)
	qh.scanExecutor = se
	qh.scanRunMu = &se.scanRunMu
	// Inline-retry abort hook: lets the dispatcher's per-task retry loop
	// short-circuit when the queue entry is no longer in_progress at the
	// expected lease epoch. Wired here (rather than via a SetXxx setter)
	// because qh and dispatcher are both visible to NewQueueHandler and
	// the relationship is intrinsic — the dispatcher belongs to qh.
	if qh.dispatcher != nil {
		qh.dispatcher.SetTaskAliveChecker(newQueueTaskAliveChecker(qh))
	}
	for _, opt := range opts {
		opt(qh)
	}
	return qh
}

// leaseOwnerID returns the lease owner identifier in "daemon:{pid}" format per spec §5.8.1.
func (qh *QueueHandler) leaseOwnerID() string {
	return fmt.Sprintf("daemon:%d", qh.daemonPID)
}
