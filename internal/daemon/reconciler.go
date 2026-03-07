package daemon

import (
	"log"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/daemon/reconcile"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// ReconcileRepair is an alias for reconcile.Repair for backward compatibility.
type ReconcileRepair = reconcile.Repair

// DeferredNotification is an alias for reconcile.DeferredNotification for backward compatibility.
type DeferredNotification = reconcile.DeferredNotification

// Reconciler is a thin facade around reconcile.Engine.
// Preserves the existing API used by QueueHandler and tests.
type Reconciler struct {
	engine *reconcile.Engine
}

// NewReconciler creates a new Reconciler backed by a reconcile.Engine with all patterns.
func NewReconciler(
	maestroDir string,
	cfg model.Config,
	lockMap *lock.MutexMap,
	logger *log.Logger,
	logLevel core.LogLevel,
	resultHandler *ResultHandler,
	executorFactory core.ExecutorFactory,
) *Reconciler {
	// Avoid nil-interface-wrapping-nil-pointer: only assign if non-nil.
	var notifier reconcile.ResultNotifier
	if resultHandler != nil {
		notifier = resultHandler
	}

	deps := reconcile.Deps{
		MaestroDir:      maestroDir,
		Config:          cfg,
		LockMap:         lockMap,
		DL:              core.NewDaemonLoggerFromLegacy("reconciler", logger, logLevel),
		Clock:           core.RealClock{},
		ResultHandler:   notifier,
		ExecutorFactory: executorFactory,
	}

	engine := reconcile.NewEngine(deps,
		reconcile.R0PlanningStuck{},
		reconcile.R0Dispatch{},
		reconcile.R0bFillingStuck{},
		reconcile.R1ResultQueue{},
		reconcile.R2ResultState{},
		reconcile.R3PlannerQueue{},
		reconcile.R4PlanStatus{},
		reconcile.R5Notification{},
		reconcile.R6FillTimeout{},
	)

	return &Reconciler{engine: engine}
}

// SetCanComplete sets the CanComplete function (wired after plan package init to avoid import cycles).
func (r *Reconciler) SetCanComplete(f core.CanCompleteFunc) {
	r.engine.SetCanComplete(f)
}

// SetExecutorFactory overrides the executor factory for testing.
func (r *Reconciler) SetExecutorFactory(f core.ExecutorFactory) {
	r.engine.SetExecutorFactory(f)
}

// Reconcile runs all reconciliation patterns and returns repairs and deferred notifications.
func (r *Reconciler) Reconcile() ([]ReconcileRepair, []DeferredNotification) {
	return r.engine.Reconcile()
}

// ExecuteDeferredNotifications sends collected Planner notifications via agent executor.
func (r *Reconciler) ExecuteDeferredNotifications(notifications []DeferredNotification) {
	r.engine.ExecuteDeferredNotifications(notifications)
}
