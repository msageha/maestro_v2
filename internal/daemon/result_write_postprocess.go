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
		h.emitPausedForReplanPlannerSignal(input.params, "worker result requires replanning")
	}
}

func (p resultPostProcessor) AfterVerification(input resultPostFinalizeInput) {
	h := p.api
	if !input.duplicate && !input.verifyWillRunAsync {
		h.maybeAutoRecoverAfterResolution(input.params, input.resultStatus, input.finalStatus,
			input.phaseA.taskRunOnIntegration)
	}
	if !input.duplicate && !input.verifyWillRunAsync {
		h.dispatchAdvisoryReview(input.params, input.finalStatus)
	}
}

func (p resultPostProcessor) AfterResponseSideEffects(params ResultWriteParams) {
	h := p.api
	h.setAgentIdle(params.Reporter)
	if h.triggerScan != nil {
		h.triggerScan(h.ctx())
	}
}
