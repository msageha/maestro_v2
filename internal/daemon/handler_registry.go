package daemon

import (
	"context"
	"sync/atomic"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/events"
)

// SetStateReader wires the state manager for dependency resolution (Phase 6).
// Must be called before PeriodicScan starts.
func (qh *QueueHandler) SetStateReader(reader StateManager) {
	qh.initMu.Lock()
	defer qh.initMu.Unlock()
	qh.dependencyResolver = NewDependencyResolver(reader, qh.logger, qh.logLevel)
	qh.cancelHandler.SetStateReader(reader)
	// If the worktree manager has already been wired, re-install the merge
	// gate on the freshly-created resolver. Supports init orders where
	// SetStateReader is (re-)called after SetWorktreeManager.
	if qh.worktreeManager != nil {
		qh.dependencyResolver.SetMergeGate(qh.isPhaseMergeRecorded)
	}
}

// SetCanComplete wires the CanComplete function for R4 reconciliation.
func (qh *QueueHandler) SetCanComplete(f CanCompleteFunc) {
	qh.reconciler.SetCanComplete(f)
}

// SetDeferredPlanCompleter wires the function that auto-completes a plan
// after worktree publish succeeds. Must be called before Run() starts.
func (qh *QueueHandler) SetDeferredPlanCompleter(f DeferredPlanCompleterFunc) {
	qh.initMu.Lock()
	defer qh.initMu.Unlock()
	qh.deferredPlanCompleter = f
}

// SetCircuitBreaker wires the circuit breaker handler for periodic scan integration.
// Must be called before Run() starts.
func (qh *QueueHandler) SetCircuitBreaker(cb *circuitbreaker.Handler) {
	qh.initMu.Lock()
	defer qh.initMu.Unlock()
	qh.circuitBreaker = cb
}

// SetAdmissionController wires the admission controller for concurrency limiting.
// Must be called before Run() starts.
func (qh *QueueHandler) SetAdmissionController(ac *admission.Controller) {
	qh.initMu.Lock()
	defer qh.initMu.Unlock()
	qh.admissionCtrl = ac
}

// SetPhaseDiagnoser wires the phase diagnosis function for completed phase analysis.
// Must be called before Run() starts.
func (qh *QueueHandler) SetPhaseDiagnoser(fn PhaseDiagnoserFunc) {
	qh.initMu.Lock()
	defer qh.initMu.Unlock()
	qh.scanExecutor.phaseDiagnoser = fn
}

// SetWorktreeManager wires the worktree manager for worker isolation.
// Must be called before Run() starts.
func (qh *QueueHandler) SetWorktreeManager(wm *WorktreeManager) {
	qh.initMu.Lock()
	defer qh.initMu.Unlock()
	qh.worktreeManager = wm
	qh.dispatcher.SetWorktreeManager(wm)
	qh.cancelHandler.SetWorktreeManager(wm)
	// Wire the worktree merge gate so the dependency resolver defers
	// PhaseStatusCompleted reflection/publication until the merge has been
	// recorded by Phase C. Without this, pending phases downstream of a
	// merge-gated phase activate prematurely and verification tasks run on
	// the worker worktree instead of the integration branch.
	if qh.dependencyResolver != nil {
		qh.dependencyResolver.SetMergeGate(qh.isPhaseMergeRecorded)
	}
}

// SetShutdownGuard wires the daemon's shutdown context, advisory flag, and
// shutdown callback so that debounce callbacks respect context cancellation,
// shutdown state, and trigger daemon shutdown on panic.
// Must be called before Run() starts.
func (qh *QueueHandler) SetShutdownGuard(ctx context.Context, shuttingDown *atomic.Bool, shutdownFn func()) {
	qh.initMu.Lock()
	defer qh.initMu.Unlock()
	qh.shutdownCtx = ctx
	qh.shuttingDown = shuttingDown
	qh.scanExecutor.debounce.SetShutdownGuard(ctx, shuttingDown, shutdownFn)
	// Propagate the shutdown context to the result handler so its inline
	// notify-retry loops cancel promptly when the daemon is tearing down.
	qh.resultHandler.SetShutdownContext(ctx)
}

// SetSessionLostFlag wires the daemon's session-lost flag so that dispatch
// is paused when the tmux session disappears. Must be called before Run() starts.
func (qh *QueueHandler) SetSessionLostFlag(flag *atomic.Bool) {
	qh.initMu.Lock()
	defer qh.initMu.Unlock()
	qh.sessionLost = flag
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

// SetPhaseCManager wires the Phase C component bundle (complexity, bandit,
// feature gate, fingerprint DB, search tree, evolution) to the QueueHandler
// so that dispatch, result processing, and reviewer flows can consult them.
// Safe to call before or after Run(); nil-safe at consumer call sites.
func (qh *QueueHandler) SetPhaseCManager(m *PhaseCManager) {
	qh.initMu.Lock()
	defer qh.initMu.Unlock()
	qh.phaseC = m
	qh.resultHandler.SetPhaseCManager(m)
}

// SetModelSelector wires the adaptive model selector into the result handler
// so that task outcomes can feed rewards back into the bandit. Safe to call
// before or after Run(); nil-safe at consumer call sites.
func (qh *QueueHandler) SetModelSelector(s *banditModelSelector) {
	qh.initMu.Lock()
	defer qh.initMu.Unlock()
	qh.resultHandler.SetModelSelector(s)
}
