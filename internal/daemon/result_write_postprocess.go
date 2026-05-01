package daemon

import "github.com/msageha/maestro_v2/internal/model"

type resultPostProcessor struct {
	api *ResultWriteAPI
}

type resultPostPhaseBInput struct {
	params       ResultWriteParams
	phaseA       *resultWritePhaseAResult
	resultStatus model.Status
	phaseBStatus model.Status
}

type resultPostFinalizeInput struct {
	params             ResultWriteParams
	phaseA             *resultWritePhaseAResult
	resultStatus       model.Status
	finalStatus        model.Status
	duplicate          bool
	verifyWillRunAsync bool
}

func newResultPostProcessor(api *ResultWriteAPI) resultPostProcessor {
	return resultPostProcessor{api: api}
}

func (p resultPostProcessor) AfterPhaseB(input resultPostPhaseBInput) {
	h := p.api
	h.recordFallback(input.params, input.resultStatus)
	h.handleRetryRegistration(input.phaseA, input.params)
	if input.phaseBStatus == model.StatusPausedForReplan {
		// emitPausedForReplanPlannerSignal requires callers to hold
		// scanMu.RLock so the signal-queue write does not race
		// PeriodicScan's flush. handleRetryRegistration above takes the
		// lock internally and has released it by this point, so we acquire
		// our own bracket for this signal emission.
		h.acquireFileLock()
		h.emitPausedForReplanPlannerSignal(input.params, "worker result requires replanning")
		h.releaseFileLock()
	}
}

// AfterVerification routes the post-verification side effects
// (auto-recover-after-resolution, advisory review dispatch) to the sync
// path. F-012: the prior implementation guarded each side effect with the
// same condition (`!duplicate && !verifyWillRunAsync`); this version
// extracts the predicate into shouldRunAfterVerificationSync so the
// invariant is unit-testable AND removes the duplicated condition that
// previously made it possible (in principle) to forget one of the two
// guards when a third side effect was added.
func (p resultPostProcessor) AfterVerification(input resultPostFinalizeInput) {
	if !shouldRunAfterVerificationSync(input) {
		return
	}
	h := p.api
	h.maybeAutoRecoverAfterResolution(input.params, input.resultStatus, input.finalStatus,
		input.phaseA.taskRunOnIntegration)
	h.dispatchAdvisoryReview(input.params, input.finalStatus)
}

// shouldRunAfterVerificationSync reports whether the sync-path
// AfterVerification side effects must run. Both side effects share the same
// gate: skip when the result is a duplicate (state already reflects the
// prior write — Bug H), or when verification was scheduled to run async (in
// which case the async completion path invokes the same side effects
// directly to preserve the exactly-once contract). F-012.
func shouldRunAfterVerificationSync(input resultPostFinalizeInput) bool {
	return !input.duplicate && !input.verifyWillRunAsync
}

func (p resultPostProcessor) AfterResponseSideEffects(params ResultWriteParams) {
	h := p.api
	h.setAgentIdle(params.Reporter)
	if h.triggerScan != nil {
		h.triggerScan(h.ctx())
	}
}
