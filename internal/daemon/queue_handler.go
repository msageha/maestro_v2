package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/msageha/maestro_v2/internal/agent"
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

	execProvider        *ExecutorProvider // shared across dispatcher, resultHandler, cancelHandler
	leaseManager        QueueLeaseManager
	dispatcher          QueueDispatcher
	dependencyResolver  QueueDependencyResolver
	cancelHandler       *CancelHandler
	resultHandler       *ResultHandler
	reconciler          *Reconciler
	deadLetterProcessor *DeadLetterProcessor
	metricsHandler      *metrics.Handler
	circuitBreaker      *circuitbreaker.Handler
	admissionCtrl       *admission.Controller
	fallbackMgr         *fallback.Manager
	worktreeManager     QueueWorktreeManager
	lockMap             *lock.MutexMap

	// scanExecutor handles periodic scan orchestration and scan-specific state.
	scanExecutor *ScanPhaseExecutor

	// scanRunMu exposes the scan run mutex for test cleanup synchronization.
	scanRunMu *sync.Mutex

	// daemonPID for lease_owner format "daemon:{pid}" per spec §5.8.1.
	daemonPID int

	// Shutdown guard: wired via SetShutdownGuard after construction.
	shutdownCtx  context.Context
	shuttingDown *atomic.Bool
}

// NewQueueHandler creates a new QueueHandler with all sub-modules.
// A single shared ExecutorProvider is created and injected into all handlers
// that need executor access (Dispatcher, ResultHandler, CancelHandler).
func NewQueueHandler(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel, opts ...QueueHandlerOption) *QueueHandler {
	clock := RealClock{}
	factory := ExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return agent.NewExecutor(dir, wcfg, level)
	})
	ep := NewExecutorProvider(maestroDir, cfg.Watcher, cfg.Logging.Level, factory, clock)

	lm := NewLeaseManager(cfg.Watcher, logger, logLevel)
	dispatcher := NewDispatcher(maestroDir, cfg, lm, logger, logLevel, ep, clock)
	dr := NewDependencyResolver(nil, logger, logLevel) // StateReader wired in Phase 6
	ch := NewCancelHandler(maestroDir, cfg, lockMap, logger, logLevel, ep)
	rh := NewResultHandler(maestroDir, cfg, lockMap, logger, logLevel, ep, clock)
	rec := NewReconciler(maestroDir, cfg, lockMap, logger, logLevel, rh, ep.Factory())
	dlp := NewDeadLetterProcessor(maestroDir, cfg, lockMap, logger, logLevel)
	mh := metrics.NewHandler(maestroDir, cfg, logger, logLevel)

	dl := NewDaemonLoggerFromLegacy("queue_handler", logger, logLevel)
	qh := &QueueHandler{
		maestroDir:          maestroDir,
		config:              cfg,
		dl:                  dl,
		logger:              logger,
		logLevel:            logLevel,
		clock:               clock,
		execProvider:        ep,
		leaseManager:        lm,
		dispatcher:          dispatcher,
		dependencyResolver:  dr,
		cancelHandler:       ch,
		resultHandler:       rh,
		reconciler:          rec,
		deadLetterProcessor: dlp,
		metricsHandler:      mh,
		lockMap:             lockMap,
		daemonPID:           os.Getpid(),
	}
	se := newScanPhaseExecutor(qh)
	se.debounce = NewDebounceController(cfg.Watcher.DebounceSec, dl, qh.PeriodicScanWithContext)
	qh.scanExecutor = se
	qh.scanRunMu = &se.scanRunMu
	for _, opt := range opts {
		opt(qh)
	}
	return qh
}

// leaseOwnerID returns the lease owner identifier in "daemon:{pid}" format per spec §5.8.1.
func (qh *QueueHandler) leaseOwnerID() string {
	return fmt.Sprintf("daemon:%d", qh.daemonPID)
}
