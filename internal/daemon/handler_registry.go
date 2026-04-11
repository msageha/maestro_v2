package daemon

import (
	"context"
	"sync/atomic"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/daemon/fallback"
	"github.com/msageha/maestro_v2/internal/events"
)

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

// SetShutdownGuard wires the daemon's shutdown context, advisory flag, and
// shutdown callback so that debounce callbacks respect context cancellation,
// shutdown state, and trigger daemon shutdown on panic.
// Must be called before Run() starts.
func (qh *QueueHandler) SetShutdownGuard(ctx context.Context, shuttingDown *atomic.Bool, shutdownFn func()) {
	qh.scanExecutor.scanRunMu.Lock()
	defer qh.scanExecutor.scanRunMu.Unlock()
	qh.shutdownCtx = ctx
	qh.shuttingDown = shuttingDown
	qh.scanExecutor.debounce.SetShutdownGuard(ctx, shuttingDown, shutdownFn)
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
