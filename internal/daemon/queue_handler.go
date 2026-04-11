package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/daemon/fallback"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
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
	metricsHandler      *MetricsHandler
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
	mh := NewMetricsHandler(maestroDir, cfg, logger, logLevel)

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

// SetStateReader wires the state manager for dependency resolution (Phase 6).
// Must be called before PeriodicScan starts.
func (qh *QueueHandler) SetStateReader(reader StateManager) {
	qh.scanExecutor.scanRunMu.Lock()
	defer qh.scanExecutor.scanRunMu.Unlock()
	qh.dependencyResolver = NewDependencyResolver(reader, qh.logger, qh.logLevel)
	qh.cancelHandler.SetStateReader(reader)
}

// SetCanComplete wires the CanComplete function for R4 reconciliation.
func (qh *QueueHandler) SetCanComplete(f CanCompleteFunc) {
	qh.reconciler.SetCanComplete(f)
}

// SetCircuitBreaker wires the circuit breaker handler for periodic scan integration.
// Must be called before Run() starts.
func (qh *QueueHandler) SetCircuitBreaker(cb *circuitbreaker.Handler) {
	qh.scanExecutor.scanRunMu.Lock()
	defer qh.scanExecutor.scanRunMu.Unlock()
	qh.circuitBreaker = cb
}

// SetAdmissionController wires the admission controller for concurrency limiting.
// Must be called before Run() starts.
func (qh *QueueHandler) SetAdmissionController(ac *admission.Controller) {
	qh.scanExecutor.scanRunMu.Lock()
	defer qh.scanExecutor.scanRunMu.Unlock()
	qh.admissionCtrl = ac
}

// SetFallbackManager wires the fallback manager for degraded-mode operation.
// Must be called before Run() starts.
func (qh *QueueHandler) SetFallbackManager(fm *fallback.Manager) {
	qh.scanExecutor.scanRunMu.Lock()
	defer qh.scanExecutor.scanRunMu.Unlock()
	qh.fallbackMgr = fm
}

// SetPhaseDiagnoser wires the phase diagnosis function for completed phase analysis.
// Must be called before Run() starts.
func (qh *QueueHandler) SetPhaseDiagnoser(fn PhaseDiagnoserFunc) {
	qh.scanExecutor.scanRunMu.Lock()
	defer qh.scanExecutor.scanRunMu.Unlock()
	qh.scanExecutor.phaseDiagnoser = fn
}

// SetWorktreeManager wires the worktree manager for worker isolation.
// Must be called before Run() starts.
func (qh *QueueHandler) SetWorktreeManager(wm *WorktreeManager) {
	qh.scanExecutor.scanRunMu.Lock()
	defer qh.scanExecutor.scanRunMu.Unlock()
	qh.worktreeManager = wm
	qh.dispatcher.SetWorktreeManager(wm)
	qh.cancelHandler.SetWorktreeManager(wm)
}

// SetShutdownGuard wires the daemon's shutdown context and advisory flag
// so that debounce callbacks respect context cancellation and shutdown state.
// Must be called before Run() starts.
func (qh *QueueHandler) SetShutdownGuard(ctx context.Context, shuttingDown *atomic.Bool) {
	qh.scanExecutor.scanRunMu.Lock()
	defer qh.scanExecutor.scanRunMu.Unlock()
	qh.shutdownCtx = ctx
	qh.shuttingDown = shuttingDown
	qh.scanExecutor.debounce.SetShutdownGuard(ctx, shuttingDown)
}

// SetEventBus wires the event bus for all sub-components that publish events.
func (qh *QueueHandler) SetEventBus(bus *events.Bus) {
	qh.dispatcher.SetEventBus(bus)
	qh.dependencyResolver.SetEventBus(bus)
	qh.resultHandler.SetEventBus(bus)
}

// SetQualityGate wires the quality gate daemon for the dispatcher.
func (qh *QueueHandler) SetQualityGate(qg *QualityGateDaemon) {
	qh.dispatcher.SetQualityGate(qg)
}

// SetContinuousHandler wires the continuous handler for result processing.
func (qh *QueueHandler) SetContinuousHandler(ch *ContinuousHandler) {
	qh.resultHandler.SetContinuousHandler(ch)
}

// Stop cancels any pending debounce timer and waits for any in-flight
// callback goroutine to finish, ensuring no goroutine leak on shutdown.
func (qh *QueueHandler) Stop() {
	qh.scanExecutor.Stop()
}

// HandleFileEvent routes an fsnotify event to the appropriate handler.
func (qh *QueueHandler) HandleFileEvent(filePath string) {
	base := filepath.Base(filePath)
	dir := filepath.Base(filepath.Dir(filePath))

	switch dir {
	case "queue":
		qh.scanExecutor.debounce.Trigger(base)
	case "results":
		qh.log(LogLevelDebug, "result_event file=%s", base)
		if qh.resultHandler != nil {
			qh.resultHandler.HandleResultFileEvent(filePath)
		}
	}
}

// PeriodicScan executes all scan steps in a three-phase pattern to avoid
// holding scanMu during slow tmux I/O operations.
// This is the backward-compatible wrapper; callers with a context should use
// PeriodicScanWithContext.
func (qh *QueueHandler) PeriodicScan() {
	qh.PeriodicScanWithContext(context.Background())
}

// PeriodicScanWithContext delegates to ScanPhaseExecutor for the three-phase scan cycle.
func (qh *QueueHandler) PeriodicScanWithContext(ctx context.Context) {
	qh.scanExecutor.Execute(ctx)
}

// periodicScanPhaseA delegates to ScanPhaseExecutor for test access.
func (qh *QueueHandler) periodicScanPhaseA() phaseAResult {
	return qh.scanExecutor.periodicScanPhaseA()
}

// periodicScanPhaseB delegates to ScanPhaseExecutor for test access.
func (qh *QueueHandler) periodicScanPhaseB(ctx context.Context, pa phaseAResult) phaseBResult {
	return qh.scanExecutor.periodicScanPhaseB(ctx, pa)
}

// periodicScanPhaseC delegates to ScanPhaseExecutor for test access.
func (qh *QueueHandler) periodicScanPhaseC(pa phaseAResult, pb phaseBResult) []DeferredNotification {
	return qh.scanExecutor.periodicScanPhaseC(pa, pb)
}

// LockFiles acquires a shared (read) lock for queue write handlers.
// Multiple queue writes can proceed in parallel; PeriodicScan holds exclusive lock.
func (qh *QueueHandler) LockFiles() {
	qh.scanExecutor.LockFiles()
}

// UnlockFiles releases the shared (read) lock for queue write handlers.
func (qh *QueueHandler) UnlockFiles() {
	qh.scanExecutor.UnlockFiles()
}

func (qh *QueueHandler) log(level LogLevel, format string, args ...any) {
	qh.dl.Logf(level, format, args...)
}

// --- Deferred work types for three-phase PeriodicScan ---

// taskQueueEntry wraps a loaded task queue with its file path.
type taskQueueEntry struct {
	Queue model.TaskQueue
	Path  string
}

// dispatchItem captures a dispatch decision made in Phase A for execution in Phase B.
type dispatchItem struct {
	Kind         string              // "command", "task", "notification"
	Command      *model.Command      // snapshot for command dispatch
	Task         *model.Task         // snapshot for task dispatch
	Notification *model.Notification // snapshot for notification dispatch
	WorkerID     string              // worker ID (tasks only)
	Epoch        int                 // lease_epoch at time of decision
	ExpiresAt    string              // lease_expires_at snapshot for fencing
}

// busyCheckItem captures an expired-lease busy probe to execute in Phase B.
type busyCheckItem struct {
	Kind      string // "task", "command"
	EntryID   string
	AgentID   string
	Epoch     int
	QueueFile string // task queue file path
	UpdatedAt string // for max_in_progress_min check
	ExpiresAt string // fencing snapshot
}

// interruptItem captures a tmux interrupt to execute in Phase B.
type interruptItem struct {
	WorkerID  string
	TaskID    string
	CommandID string
	Epoch     int
}

// cancelMarkItem captures a deferred cancellation mutation to apply in Phase C
// after Phase B has interrupted the running worker (M3 + H4).
//
// Deferring the queue mutation past Phase B's interrupt resolves a race with
// result_write_handler: a worker that completes its real result before being
// interrupted can submit it via the normal path, and Phase C's apply step
// detects the now-terminal task and skips overwriting it with a synthetic
// cancelled marker.
type cancelMarkItem struct {
	QueueFile  string
	WorkerID   string
	TaskID     string
	CommandID  string
	LeaseEpoch int
}

// signalDeliveryItem captures a planner signal delivery for Phase B.
type signalDeliveryItem struct {
	CommandID string
	PhaseID   string
	Kind      string
	Message   string
}

// worktreeMergeItem captures a phase-boundary worktree merge for Phase B execution.
type worktreeMergeItem struct {
	CommandID      string
	PhaseID        string
	WorkerIDs      []string
	WorkerPurposes map[string]string // workerID -> task purpose (for commit messages)
}

// commitFailure records a worker whose CommitWorkerChanges failed.
type commitFailure struct {
	WorkerID string
	Error    error
	// Reason is a structured classification computed at commit time using
	// errors.Is/As so downstream signal emission can populate
	// PlannerSignal.Reason without re-inspecting the error chain.
	Reason string
}

// worktreeMergeResult captures the outcome of a Phase B worktree merge.
type worktreeMergeResult struct {
	Item           worktreeMergeItem
	CommitFailures []commitFailure // workers whose commit failed (excluded from merge)
	Conflicts      []model.MergeConflict
	Error          error
}

// worktreePublishItem captures a publish-to-base operation for Phase B execution.
type worktreePublishItem struct {
	CommandID      string
	PublishMessage string // command content summary for commit message
}

// worktreePublishResult captures the outcome of a Phase B publish-to-base.
type worktreePublishResult struct {
	Item  worktreePublishItem
	Error error
}

// worktreeCleanupItem captures a worktree cleanup operation for Phase B execution.
type worktreeCleanupItem struct {
	CommandID string
	Reason    string // "success" or "failure"
}

// worktreeCleanupResult captures the outcome of a Phase B worktree cleanup.
type worktreeCleanupResult struct {
	Item  worktreeCleanupItem
	Error error
}

// deferredWork collects all slow I/O operations for Phase B execution.
type deferredWork struct {
	dispatches        []dispatchItem
	interrupts        []interruptItem
	cancelMarks       []cancelMarkItem
	busyChecks        []busyCheckItem
	signals           []signalDeliveryItem
	clears            []string // agent IDs to /clear
	worktreeMerges    []worktreeMergeItem
	worktreePublishes []worktreePublishItem
	worktreeCleanups  []worktreeCleanupItem
}

// dispatchResult captures the outcome of a Phase B dispatch.
type dispatchResult struct {
	Item    dispatchItem
	Success bool
	Error   error
}

// busyCheckResult captures the outcome of a Phase B busy probe.
type busyCheckResult struct {
	Item      busyCheckItem
	Busy      bool
	Undecided bool // VerdictUndecided: neither extend nor release; defer to next scan
}

// signalDeliveryResult captures the outcome of a Phase B signal delivery.
type signalDeliveryResult struct {
	Item    signalDeliveryItem
	Success bool
	Error   error
}

// phaseAResult holds all data Phase A passes to Phase B and Phase C.
type phaseAResult struct {
	work      deferredWork
	scanStart time.Time
	counters  ScanCounters
}

// phaseBResult holds all results from Phase B for Phase C to apply.
type phaseBResult struct {
	dispatches        []dispatchResult
	busyChecks        []busyCheckResult
	signals           []signalDeliveryResult
	worktreeMerges    []worktreeMergeResult
	worktreePublishes []worktreePublishResult
	worktreeCleanups  []worktreeCleanupResult
	recoveryHints     []string // M3: recovery hints for partial failure diagnosis
}
