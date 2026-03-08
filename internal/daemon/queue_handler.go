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

	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// QueueHandler orchestrates fsnotify event routing and periodic scan execution.
type QueueHandler struct {
	maestroDir string
	config     model.Config
	dl         *DaemonLogger
	logger     *log.Logger
	logLevel   LogLevel
	clock      Clock

	leaseManager        *LeaseManager
	dispatcher          *Dispatcher
	dependencyResolver  *DependencyResolver
	cancelHandler       *CancelHandler
	resultHandler       *ResultHandler
	reconciler          *Reconciler
	deadLetterProcessor *DeadLetterProcessor
	metricsHandler      *MetricsHandler
	circuitBreaker      *circuitbreaker.Handler
	worktreeManager     *WorktreeManager
	lockMap             *lock.MutexMap

	// scanCounters accumulates counters during a single PeriodicScan cycle.
	scanCounters ScanCounters

	// Debounce state
	debounceMu       sync.Mutex
	debounceTimer    *time.Timer
	debounceDone     chan struct{} // closed when the in-flight callback finishes
	firstTriggerAt   time.Time    // tracks first trigger in a debounce window for maxWait
	scanRunning      atomic.Bool  // true while debounced callback is executing

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

	// busyChecker is called to probe agent busy state before lease expiry release.
	// Returns true if the agent is currently busy (lease should be extended).
	busyChecker func(agentID string) bool

	// Shutdown guard: wired via SetShutdownGuard after construction.
	shutdownCtx  context.Context
	shuttingDown *atomic.Bool
}

// NewQueueHandler creates a new QueueHandler with all sub-modules.
func NewQueueHandler(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel) *QueueHandler {
	lm := NewLeaseManager(cfg.Watcher, logger, logLevel)
	dispatcher := NewDispatcher(maestroDir, cfg, lm, logger, logLevel)
	dr := NewDependencyResolver(nil, logger, logLevel) // StateReader wired in Phase 6
	ch := NewCancelHandler(maestroDir, cfg, lockMap, logger, logLevel)
	rh := NewResultHandler(maestroDir, cfg, lockMap, logger, logLevel)
	rec := NewReconciler(maestroDir, cfg, lockMap, logger, logLevel, rh, rh.executorFactory)
	dlp := NewDeadLetterProcessor(maestroDir, cfg, lockMap, logger, logLevel)
	mh := NewMetricsHandler(maestroDir, cfg, logger, logLevel)

	return &QueueHandler{
		maestroDir:          maestroDir,
		config:              cfg,
		dl:                  NewDaemonLoggerFromLegacy("queue_handler", logger, logLevel),
		logger:              logger,
		logLevel:            logLevel,
		clock:               RealClock{},
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
}

// leaseOwnerID returns the lease owner identifier in "daemon:{pid}" format per spec §5.8.1.
func (qh *QueueHandler) leaseOwnerID() string {
	return fmt.Sprintf("daemon:%d", qh.daemonPID)
}

// SetStateReader wires the state reader for dependency resolution (Phase 6).
// Must be called before PeriodicScan starts.
func (qh *QueueHandler) SetStateReader(reader StateReader) {
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()
	qh.dependencyResolver = NewDependencyResolver(reader, qh.logger, qh.logLevel)
	qh.cancelHandler.SetStateReader(reader)
}

// SetExecutorFactory overrides the executor factory for testing.
func (qh *QueueHandler) SetExecutorFactory(f ExecutorFactory) {
	qh.dispatcher.SetExecutorFactory(f)
	qh.cancelHandler.SetExecutorFactory(f)
	qh.resultHandler.SetExecutorFactory(f)
	qh.reconciler.SetExecutorFactory(f)
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

// SetWorktreeManager wires the worktree manager for worker isolation.
// Must be called before Run() starts.
func (qh *QueueHandler) SetWorktreeManager(wm *WorktreeManager) {
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()
	qh.worktreeManager = wm
	qh.dispatcher.SetWorktreeManager(wm)
}

// SetBusyChecker overrides the busy checker for testing.
// Must be called before Run() starts.
func (qh *QueueHandler) SetBusyChecker(f func(agentID string) bool) {
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()
	qh.busyChecker = f
}

// SetShutdownGuard wires the daemon's shutdown context and advisory flag
// so that debounce callbacks respect context cancellation and shutdown state.
// Must be called before Run() starts.
func (qh *QueueHandler) SetShutdownGuard(ctx context.Context, shuttingDown *atomic.Bool) {
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()
	qh.shutdownCtx = ctx
	qh.shuttingDown = shuttingDown
}

// Stop cancels any pending debounce timer and waits for any in-flight
// callback goroutine to finish, ensuring no goroutine leak on shutdown.
func (qh *QueueHandler) Stop() {
	qh.debounceMu.Lock()
	done := qh.debounceDone
	timerWasPending := false
	if qh.debounceTimer != nil {
		timerWasPending = qh.debounceTimer.Stop()
		qh.debounceTimer = nil
	}
	qh.debounceMu.Unlock()

	// Only wait if the timer had already fired (callback may be in-flight).
	// If Stop() returned true the callback will never run, so waiting would hang.
	if done != nil && !timerWasPending {
		<-done
	}
}

// GetLeaseManager returns the internal lease manager (for testing).
func (qh *QueueHandler) GetLeaseManager() *LeaseManager {
	return qh.leaseManager
}

// GetDispatcher returns the internal dispatcher (for testing).
func (qh *QueueHandler) GetDispatcher() *Dispatcher {
	return qh.dispatcher
}

// HandleFileEvent routes an fsnotify event to the appropriate handler.
func (qh *QueueHandler) HandleFileEvent(filePath string) {
	base := filepath.Base(filePath)
	dir := filepath.Base(filepath.Dir(filePath))

	switch dir {
	case "queue":
		qh.debounceAndScan(base)
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

	// Run worktree GC periodically (every 60 scans) as a safety net
	// complementing the immediate cleanup triggered by CleanupOnSuccess/CleanupOnFailure.
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

// signalDeliveryItem captures a planner signal delivery for Phase B.
type signalDeliveryItem struct {
	CommandID string
	PhaseID   string
	Kind      string
	Message   string
}

// worktreeMergeItem captures a phase-boundary worktree merge for Phase B execution.
type worktreeMergeItem struct {
	CommandID string
	PhaseID   string
	WorkerIDs []string
}

// worktreeMergeResult captures the outcome of a Phase B worktree merge.
type worktreeMergeResult struct {
	Item      worktreeMergeItem
	Conflicts []model.MergeConflict
	Error     error
}

// worktreePublishItem captures a publish-to-base operation for Phase B execution.
type worktreePublishItem struct {
	CommandID string
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
}
