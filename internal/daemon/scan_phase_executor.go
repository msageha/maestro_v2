package daemon

import (
	"context"
	"sync"

	"github.com/msageha/maestro_v2/internal/metrics"
)

type scanPhaseHost interface {
	log(level LogLevel, format string, args ...any)
	resetScanTimeCache()
	newScanState(counters *metrics.ScanCounters) scanState
	executePhaseASteps(s *scanState)
	flushScanState(s scanState)
	executePhaseBSteps(ctx context.Context, pa *phaseAResult, result *phaseBResult)
	executeScanPhaseCBody(se *ScanPhaseExecutor, pa phaseAResult, pb phaseBResult) []DeferredNotification
	executeDeferredReconcileNotifications(notifications []DeferredNotification)
	runPeriodicWorktreeGC()
}

// ScanPhaseExecutor orchestrates the three-phase periodic scan cycle (A → B → C)
// and holds scan-specific state that was previously part of QueueHandler.
// This extraction reduces QueueHandler's field and method count by separating
// scan coordination from queue management.
type ScanPhaseExecutor struct {
	host scanPhaseHost

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

// newScanPhaseExecutor creates a ScanPhaseExecutor wired to the given scan host.
func newScanPhaseExecutor(qh *QueueHandler) *ScanPhaseExecutor {
	return &ScanPhaseExecutor{host: qh}
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

	se.host.log(LogLevelDebug, "periodic_scan start")
	se.host.resetScanTimeCache()

	pa := se.periodicScanPhaseA()
	pb := se.periodicScanPhaseB(ctx, pa)
	deferredNotifs := se.periodicScanPhaseC(pa, pb)

	// Execute deferred reconciler notifications outside scanMu.Lock
	// to avoid blocking queue writes during slow tmux I/O.
	se.host.executeDeferredReconcileNotifications(deferredNotifs)

	// Run worktree GC periodically as a safety net complementing the
	// immediate cleanup triggered by CleanupOnSuccess/CleanupOnFailure.
	// Why: 60 scans ≈ 5 minutes at default 5s interval, balancing GC
	// freshness against the I/O cost of scanning worktree directories.
	const gcInterval uint64 = 60
	se.gcScanCounter++
	if se.gcScanCounter%gcInterval == 0 {
		se.host.runPeriodicWorktreeGC()
	}

	se.host.log(LogLevelDebug, "periodic_scan complete")
}

// periodicScanPhaseA runs under scanMu.Lock. It loads queues, delegates all
// step execution to QueueHandler.executePhaseASteps, and flushes queues to disk.
func (se *ScanPhaseExecutor) periodicScanPhaseA() phaseAResult {
	se.scanMu.Lock()
	defer se.scanMu.Unlock()

	s := se.initScanState()

	// Delegate all Phase A steps to QueueHandler's single entry point.
	se.host.executePhaseASteps(&s)

	// Flush dirty queues to disk
	se.host.flushScanState(s)

	return phaseAResult{
		work:      s.work,
		scanStart: s.scanStart,
		counters:  se.scanCounters,
	}
}

// initScanState loads all queue files and initializes a scanState.
func (se *ScanPhaseExecutor) initScanState() scanState {
	se.scanCounters = metrics.ScanCounters{}
	return se.host.newScanState(&se.scanCounters)
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
	se.host.executePhaseBSteps(ctx, &pa, &result)

	return result
}

// periodicScanPhaseC is the scan apply phase. It runs under scanMu.Lock,
// reloads queues from disk, applies Phase B results with epoch fencing,
// flushes, and runs post-flush steps.
// Returns deferred notifications from reconciliation that must be executed outside the lock.
func (se *ScanPhaseExecutor) periodicScanPhaseC(pa phaseAResult, pb phaseBResult) []DeferredNotification {
	se.scanMu.Lock()
	defer se.scanMu.Unlock()

	// Do NOT overwrite se.scanCounters with pa.counters here.
	// se.scanCounters already holds Phase A values plus any increments
	// from Phase B (e.g., SignalInlineRetrySuccesses from inline signal
	// delivery retries). A raw assignment would discard Phase B increments.

	// Delegate to QueueHandler for the apply body, which depends on handler wiring.
	return se.host.executeScanPhaseCBody(se, pa, pb)
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
