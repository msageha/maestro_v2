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
