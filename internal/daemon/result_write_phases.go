package daemon

import "github.com/msageha/maestro_v2/internal/model"

type resultPhaseAService struct {
	api *ResultWriteAPI
}

type resultPhaseBService struct {
	api *ResultWriteAPI
}

func newResultPhaseAService(api *ResultWriteAPI) resultPhaseAService {
	return resultPhaseAService{api: api}
}

func newResultPhaseBService(api *ResultWriteAPI) resultPhaseBService {
	return resultPhaseBService{api: api}
}

func (s resultPhaseAService) Run(params ResultWriteParams, resultStatus model.Status) (*resultWritePhaseAResult, error) {
	return s.api.resultWritePhaseA(params, resultStatus)
}

func (s resultPhaseBService) Run(
	params ResultWriteParams,
	resultID string,
	resultStatus model.Status,
	queueWriteFailed bool,
	originalTaskID string,
	retryScheduled bool,
	abortByMaxRepair bool,
) (bool, model.Status, error) {
	return s.api.resultWritePhaseB(params, resultID, resultStatus, queueWriteFailed, originalTaskID, retryScheduled, abortByMaxRepair)
}
