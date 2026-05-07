package daemon

import (
	"context"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/model"
)

func (h *ResultWriteAPI) runResultVerification(
	params ResultWriteParams,
	phaseA *resultWritePhaseAResult,
	needsVerify bool,
	phaseBStatus model.Status,
) (model.Status, bool) {
	finalStatus := phaseBStatus
	if !needsVerify {
		return finalStatus, false
	}

	// Per-task daemon verify is intentionally skipped for normal worker
	// tasks (Report 2026-05-05 P0-A regression). The worker has already
	// self-verified via skill-driven CLI execution before reporting
	// completion, and running the daemon's §S1-1 VerifyRunner over a
	// project_root that does not contain the worker's diff would let
	// broken-syntax changes pass through (the canonical reproduction
	// pinned a `broken_probe.go` task whose verify ran at project root,
	// passed, and was published before `go test ./...` failed). Running
	// at the worker worktree was the prior alternative but failed for
	// every package-managed language because gitignored dep caches
	// (`node_modules/`, `.gradle/`, `vendor/`, …) are absent in linked
	// worktrees.
	//
	// Pre-publish verify is the daemon's responsibility: tasks marked
	// RunOnIntegration / RunOnMain still execute the VerifyRunner so
	// operators can author a final "verify the integrated state" task
	// in the plan. That task runs against the integration worktree (or
	// project root for RunOnMain) where the operator can ensure deps
	// are installed.
	if phaseA != nil && phaseA.sourceTask != nil {
		st := phaseA.sourceTask
		if !st.RunOnIntegration && !st.RunOnMain {
			if applyErr := h.applyVerifyOutcome(params, model.StatusCompleted,
				"verify_skipped_normal_worker_task: worker self-verification covers the per-task signal; daemon verify runs only on RunOnIntegration / RunOnMain"); applyErr != nil {
				h.logFn(LogLevelWarn,
					"verify_outcome_apply_failed_skip task=%s command=%s error=%v "+
						"(task remains at verify_pending; reconcile/operator can re-drive)",
					params.TaskID, params.CommandID, applyErr)
				return model.StatusVerifyPending, false
			}
			return model.StatusCompleted, false
		}
	}

	verifyWillRunAsync := h.verifyAsync && h.spawnTask != nil
	verifyInput := resultVerifyInput{
		params:               params,
		sourceTask:           phaseA.sourceTask,
		taskRunOnIntegration: phaseA.taskRunOnIntegration,
	}
	if verifyWillRunAsync {
		h.runVerifyAsync(params, verifyInput)
		return model.StatusVerifyPending, true
	}
	return h.runVerifySecondPass(h.ctx(), verifyInput), false
}

func (h *ResultWriteAPI) runVerifyAsync(params ResultWriteParams, input resultVerifyInput) {
	if !h.spawnTask("resultWriteVerify", func(ctx context.Context) {
		if !h.acquireVerifyBackgroundSlot(ctx, params) {
			return
		}
		defer h.releaseVerifyBackgroundSlot()
		h.runVerifySecondPass(ctx, input)
	}) {
		h.logFn(LogLevelWarn,
			"verify_background_not_started task=%s command=%s "+
				"(daemon shutting down; task remains at verify_pending for reconcile)",
			params.TaskID, params.CommandID)
	}
}

func (h *ResultWriteAPI) acquireVerifyBackgroundSlot(ctx context.Context, params ResultWriteParams) bool {
	if h.admissionCtrl == nil {
		return true
	}
	for {
		if h.admissionCtrl.TryAcquireBackground(admission.OpVerify) {
			return true
		}
		h.logFn(LogLevelDebug,
			"verify_background_waiting_for_admission task=%s command=%s",
			params.TaskID, params.CommandID)
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			h.logFn(LogLevelWarn,
				"verify_background_cancelled_before_admission task=%s command=%s error=%v "+
					"(task remains at verify_pending for reconcile)",
				params.TaskID, params.CommandID, ctx.Err())
			return false
		case <-timer.C:
		}
	}
}

func (h *ResultWriteAPI) releaseVerifyBackgroundSlot() {
	if h.admissionCtrl != nil {
		h.admissionCtrl.ReleaseBackground(admission.OpVerify)
	}
}
