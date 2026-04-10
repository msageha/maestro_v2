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

// BusyChecker probes whether an agent is currently busy.
// Implementations are used to override the default executor-based probe in tests.
type BusyChecker interface {
	IsBusy(agentID string) bool
}

// BusyCheckerFunc adapts a plain function to the BusyChecker interface.
type BusyCheckerFunc func(agentID string) bool

// IsBusy calls the underlying function to check whether the given agent is busy.
func (f BusyCheckerFunc) IsBusy(agentID string) bool { return f(agentID) }

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

	// scanCounters accumulates counters during a single PeriodicScan cycle.
	scanCounters ScanCounters

	// debounce manages filesystem event coalescing and starvation protection.
	debounce *DebounceController

	// scanMu serializes PeriodicScan phases (exclusive) vs queue writes (shared RLock).
	// Spec §5.6: per-agent mutex — queue writes hold RLock + per-target lockMap key.
	scanMu sync.RWMutex

	// scanRunMu serializes the full PeriodicScan cycle (Phase A → B → C).
	// Prevents overlapping scans since Phase B releases scanMu.
	scanRunMu sync.Mutex

	// gcScanCounter counts periodic scans to trigger worktree GC at intervals.
	gcScanCounter uint64

	// daemonPID for lease_owner format "daemon:{pid}" per spec §5.8.1.
	daemonPID int

	// busyChecker overrides the default executor-based busy probe.
	// Used in tests to stub agent busy state. When nil, the real executor probe is used.
	busyChecker BusyChecker

	// worktreeStallMarkFn overrides the persistence step of stepWorktreeStallDetection.
	// When nil, worktreeManager.MarkIntegrationStallSignaled is used. Tests inject a
	// failing implementation to exercise the integration→Failed fallback path.
	worktreeStallMarkFn func(commandID string) error

	// Shutdown guard: wired via SetShutdownGuard after construction.
	shutdownCtx  context.Context
	shuttingDown *atomic.Bool
}

// NewQueueHandler creates a new QueueHandler with all sub-modules.
// A single shared ExecutorProvider is created and injected into all handlers
// that need executor access (Dispatcher, ResultHandler, CancelHandler).
func NewQueueHandler(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel) *QueueHandler {
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
	qh.debounce = NewDebounceController(cfg.Watcher.DebounceSec, dl, qh.PeriodicScanWithContext)
	return qh
}

// leaseOwnerID returns the lease owner identifier in "daemon:{pid}" format per spec §5.8.1.
func (qh *QueueHandler) leaseOwnerID() string {
	return fmt.Sprintf("daemon:%d", qh.daemonPID)
}

// SetStateReader wires the state manager for dependency resolution (Phase 6).
// Must be called before PeriodicScan starts.
func (qh *QueueHandler) SetStateReader(reader StateManager) {
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()
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
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()
	qh.circuitBreaker = cb
}

// SetAdmissionController wires the admission controller for concurrency limiting.
// Must be called before Run() starts.
func (qh *QueueHandler) SetAdmissionController(ac *admission.Controller) {
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()
	qh.admissionCtrl = ac
}

// SetFallbackManager wires the fallback manager for degraded-mode operation.
// Must be called before Run() starts.
func (qh *QueueHandler) SetFallbackManager(fm *fallback.Manager) {
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()
	qh.fallbackMgr = fm
}

// SetWorktreeManager wires the worktree manager for worker isolation.
// Must be called before Run() starts.
func (qh *QueueHandler) SetWorktreeManager(wm *WorktreeManager) {
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()
	qh.worktreeManager = wm
	qh.dispatcher.SetWorktreeManager(wm)
	qh.cancelHandler.SetWorktreeManager(wm)
}

// SetShutdownGuard wires the daemon's shutdown context and advisory flag
// so that debounce callbacks respect context cancellation and shutdown state.
// Must be called before Run() starts.
func (qh *QueueHandler) SetShutdownGuard(ctx context.Context, shuttingDown *atomic.Bool) {
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()
	qh.shutdownCtx = ctx
	qh.shuttingDown = shuttingDown
	qh.debounce.SetShutdownGuard(ctx, shuttingDown)
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
	qh.debounce.Stop()
}

// HandleFileEvent routes an fsnotify event to the appropriate handler.
func (qh *QueueHandler) HandleFileEvent(filePath string) {
	base := filepath.Base(filePath)
	dir := filepath.Base(filepath.Dir(filePath))

	switch dir {
	case "queue":
		qh.debounce.Trigger(base)
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

// PeriodicScanWithContext executes all scan steps with context support for
// cancellation during slow Phase B tmux I/O operations.
//
// Phase A (scanMu.Lock): Load queues, fast mutations, collect deferred work, flush.
// Phase B (no lock): Execute slow tmux I/O (interrupts, busy probes, dispatch, signals).
// Phase C (scanMu.Lock): Reload queues, apply Phase B results with fencing, flush, reconcile.
func (qh *QueueHandler) PeriodicScanWithContext(ctx context.Context) {
	// scanRunMu serializes the full A/B/C cycle so that concurrent scan triggers
	// wait for the current cycle to finish rather than overlapping with Phase B.
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()

	qh.log(LogLevelDebug, "periodic_scan start")

	pa := qh.periodicScanPhaseA()
	pb := qh.periodicScanPhaseB(ctx, pa)
	deferredNotifs := qh.periodicScanPhaseC(pa, pb)

	// Execute deferred reconciler notifications outside scanMu.Lock
	// to avoid blocking queue writes during slow tmux I/O.
	if qh.reconciler != nil && len(deferredNotifs) > 0 {
		qh.reconciler.ExecuteDeferredNotifications(deferredNotifs)
	}

	// Run worktree GC periodically as a safety net complementing the
	// immediate cleanup triggered by CleanupOnSuccess/CleanupOnFailure.
	// Why: 60 scans ≈ 5 minutes at default 5s interval, balancing GC
	// freshness against the I/O cost of scanning worktree directories.
	const gcInterval uint64 = 60
	qh.gcScanCounter++
	if qh.gcScanCounter%gcInterval == 0 && qh.worktreeManager != nil {
		if err := qh.worktreeManager.GC(); err != nil {
			qh.log(LogLevelWarn, "worktree_gc error=%v", err)
		}
	}

	qh.log(LogLevelDebug, "periodic_scan complete")
}

// LockFiles acquires a shared (read) lock for queue write handlers.
// Multiple queue writes can proceed in parallel; PeriodicScan holds exclusive lock.
func (qh *QueueHandler) LockFiles() {
	qh.scanMu.RLock()
}

// UnlockFiles releases the shared (read) lock for queue write handlers.
func (qh *QueueHandler) UnlockFiles() {
	qh.scanMu.RUnlock()
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
