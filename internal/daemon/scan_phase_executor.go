package daemon

import (
	"context"
	"sync"

	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
)

// ScanPhaseExecutor orchestrates the three-phase periodic scan cycle (A → B → C)
// and holds scan-specific state that was previously part of QueueHandler.
// This extraction reduces QueueHandler's field and method count by separating
// scan coordination from queue management.
type ScanPhaseExecutor struct {
	qh *QueueHandler // access to shared dependencies (handlers, managers, config)

	// scanCounters accumulates counters during a single PeriodicScan cycle.
	scanCounters metrics.ScanCounters

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

	// busyChecker overrides the default executor-based busy probe.
	// Used in tests to stub agent busy state. When nil, the real executor probe is used.
	busyChecker BusyChecker

	// phaseDiagnoser produces diagnosis prompts for completed phases.
	// Injected via SetPhaseDiagnoser; nil means diagnosis is skipped.
	phaseDiagnoser PhaseDiagnoserFunc

	// worktreeStallMarkFn overrides the persistence step of stepWorktreeStallDetection.
	// When nil, worktreeManager.MarkIntegrationStallSignaled is used. Tests inject a
	// failing implementation to exercise the integration→Failed fallback path.
	worktreeStallMarkFn func(commandID string) error
}

// newScanPhaseExecutor creates a ScanPhaseExecutor wired to the given QueueHandler.
func newScanPhaseExecutor(qh *QueueHandler) *ScanPhaseExecutor {
	return &ScanPhaseExecutor{qh: qh}
}

// Execute runs the three-phase periodic scan cycle with context support for
// cancellation during slow Phase B tmux I/O operations.
//
// Phase A (scanMu.Lock): Load queues, fast mutations, collect deferred work, flush.
// Phase B (no lock): Execute slow tmux I/O (interrupts, busy probes, dispatch, signals).
// Phase C (scanMu.Lock): Reload queues, apply Phase B results with fencing, flush, reconcile.
func (se *ScanPhaseExecutor) Execute(ctx context.Context) {
	// scanRunMu serializes the full A/B/C cycle so that concurrent scan triggers
	// wait for the current cycle to finish rather than overlapping with Phase B.
	se.scanRunMu.Lock()
	defer se.scanRunMu.Unlock()

	se.qh.log(LogLevelDebug, "periodic_scan start")

	pa := se.periodicScanPhaseA()
	pb := se.periodicScanPhaseB(ctx, pa)
	deferredNotifs := se.periodicScanPhaseC(pa, pb)

	// Execute deferred reconciler notifications outside scanMu.Lock
	// to avoid blocking queue writes during slow tmux I/O.
	if se.qh.reconciler != nil && len(deferredNotifs) > 0 {
		failed := se.qh.reconciler.ExecuteDeferredNotifications(deferredNotifs)
		if len(failed) > 0 {
			for _, n := range failed {
				se.qh.log(LogLevelWarn, "reconciler_notification_failed kind=%s command_id=%s", n.Kind, n.CommandID)
			}
			se.qh.log(LogLevelWarn, "reconciler_notifications_failed count=%d", len(failed))
		}
	}

	// Run worktree GC periodically as a safety net complementing the
	// immediate cleanup triggered by CleanupOnSuccess/CleanupOnFailure.
	// Why: 60 scans ≈ 5 minutes at default 5s interval, balancing GC
	// freshness against the I/O cost of scanning worktree directories.
	const gcInterval uint64 = 60
	se.gcScanCounter++
	if se.gcScanCounter%gcInterval == 0 && se.qh.worktreeManager != nil {
		if err := se.qh.worktreeManager.GC(); err != nil {
			se.qh.log(LogLevelWarn, "worktree_gc error=%v", err)
		}
	}

	se.qh.log(LogLevelDebug, "periodic_scan complete")
}

// periodicScanPhaseA runs under scanMu.Lock. It loads queues, delegates all
// step execution to QueueHandler.executePhaseASteps, and flushes queues to disk.
func (se *ScanPhaseExecutor) periodicScanPhaseA() phaseAResult {
	se.scanMu.Lock()
	defer se.scanMu.Unlock()

	s := se.initScanState()

	// Delegate all Phase A steps to QueueHandler's single entry point.
	se.qh.executePhaseASteps(&s)

	// Flush dirty queues to disk
	se.qh.queueStore.FlushQueues(s.commands.Data, s.commands.Path, s.commands.Dirty,
		s.tasks, s.taskDirty,
		s.notifications.Data, s.notifications.Path, s.notifications.Dirty,
		s.signals.Data, s.signals.Path, s.signals.Dirty)

	return phaseAResult{
		work:      s.work,
		scanStart: s.scanStart,
		counters:  se.scanCounters,
	}
}

// initScanState loads all queue files and initializes a scanState.
func (se *ScanPhaseExecutor) initScanState() scanState {
	qh := se.qh
	scanStart := qh.clock.Now()
	se.scanCounters = metrics.ScanCounters{}

	commandQueue, commandPath, err := qh.queueStore.LoadCommandQueue()
	if err != nil {
		qh.log(LogLevelError, "load_command_queue_failed error=%v", err)
	}
	taskQueues, err := qh.queueStore.LoadAllTaskQueues()
	if err != nil {
		qh.log(LogLevelError, "load_task_queues_failed error=%v", err)
	}
	notificationQueue, notificationPath, err := qh.queueStore.LoadNotificationQueue()
	if err != nil {
		qh.log(LogLevelError, "load_notification_queue_failed error=%v", err)
	}
	signalQueue, signalPath, err := qh.queueStore.LoadPlannerSignalQueue()
	if err != nil {
		qh.log(LogLevelError, "load_signal_queue_failed error=%v", err)
	}

	return scanState{
		commands:      fileState[model.CommandQueue]{Data: commandQueue, Path: commandPath},
		tasks:         taskQueues,
		taskDirty:     make(map[string]bool),
		notifications: fileState[model.NotificationQueue]{Data: notificationQueue, Path: notificationPath},
		signals:       fileState[model.PlannerSignalQueue]{Data: signalQueue, Path: signalPath},
		signalIndex:   buildSignalIndex(signalQueue.Signals),
		scanStart:     scanStart,
	}
}

// periodicScanPhaseB executes all slow tmux I/O operations without holding any lock.
// Order: interrupts -> busy checks -> dispatches -> signals -> clears -> merges -> publishes -> cleanups.
func (se *ScanPhaseExecutor) periodicScanPhaseB(ctx context.Context, pa phaseAResult) phaseBResult {
	result := phaseBResult{
		busyChecks:        make([]busyCheckResult, 0, len(pa.work.busyChecks)),
		dispatches:        make([]dispatchResult, 0, len(pa.work.dispatches)),
		signals:           make([]signalDeliveryResult, 0, len(pa.work.signals)),
		recoveryHints:     make([]string, 0, len(pa.work.signals)),
		worktreeMerges:    make([]worktreeMergeResult, 0, len(pa.work.worktreeMerges)),
		worktreePublishes: make([]worktreePublishResult, 0, len(pa.work.worktreePublishes)),
		worktreeCleanups:  make([]worktreeCleanupResult, 0, len(pa.work.worktreeCleanups)+len(pa.work.worktreePublishes)),
	}

	// Delegate all Phase B steps to QueueHandler's single entry point.
	se.qh.executePhaseBSteps(ctx, &pa, &result)

	return result
}

// periodicScanPhaseC runs under scanMu.Lock. It reloads queues from disk,
// applies Phase B results with epoch fencing, flushes, and runs post-flush steps.
// Returns deferred notifications from reconciliation that must be executed outside the lock.
func (se *ScanPhaseExecutor) periodicScanPhaseC(pa phaseAResult, pb phaseBResult) []DeferredNotification {
	se.scanMu.Lock()
	defer se.scanMu.Unlock()

	// Restore counters accumulated during Phase A
	se.scanCounters = pa.counters

	// Delegate to QueueHandler for Phase C body (deeply coupled to handler dependencies).
	return se.qh.executeScanPhaseCBody(se, pa, pb)
}

// LockFiles acquires a shared (read) lock for queue write handlers.
// Multiple queue writes can proceed in parallel; PeriodicScan holds exclusive lock.
func (se *ScanPhaseExecutor) LockFiles() {
	se.scanMu.RLock()
}

// UnlockFiles releases the shared (read) lock for queue write handlers.
func (se *ScanPhaseExecutor) UnlockFiles() {
	se.scanMu.RUnlock()
}

// Stop cancels any pending debounce timer and waits for any in-flight
// callback goroutine to finish, ensuring no goroutine leak on shutdown.
func (se *ScanPhaseExecutor) Stop() {
	se.debounce.Stop()
}
