package daemon

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/msageha/maestro_v2/internal/uds"
)

// validationFormatter is satisfied by plan.ValidationErrors without importing plan.
type validationFormatter interface {
	error
	FormatStderr() string
}

// PlanExecutor executes plan operations under the daemon's file lock.
// Implementations are wired from main.go to avoid import cycles (plan → daemon).
type PlanExecutor interface {
	Submit(params json.RawMessage) (json.RawMessage, error)
	Complete(params json.RawMessage) (json.RawMessage, error)
	AddRetryTask(params json.RawMessage) (json.RawMessage, error)
	Rebuild(params json.RawMessage) (json.RawMessage, error)
}

// SetPlanExecutor wires the plan executor for UDS plan handlers.
func (d *Daemon) SetPlanExecutor(pe PlanExecutor) {
	d.planExecutor = pe
}

func (d *Daemon) handlePlan(req *uds.Request) *uds.Response {
	if d.planExecutor == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "plan executor not configured")
	}

	var params struct {
		Operation string          `json:"operation"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	d.acquireFileLock()
	defer d.releaseFileLock()

	var result json.RawMessage
	var err error

	switch params.Operation {
	case "submit":
		result, err = d.planExecutor.Submit(params.Data)
	case "complete":
		result, err = d.planExecutor.Complete(params.Data)
	case "add_retry_task":
		result, err = d.planExecutor.AddRetryTask(params.Data)
	case "rebuild":
		result, err = d.planExecutor.Rebuild(params.Data)
	default:
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("unknown plan operation: %q", params.Operation))
	}

	if err != nil {
		d.log(LogLevelWarn, "plan_%s error=%v", params.Operation, err)
		var ve validationFormatter
		if errors.As(err, &ve) {
			return uds.ErrorResponse(uds.ErrCodeValidation, ve.FormatStderr())
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}

	d.log(LogLevelInfo, "plan_%s success", params.Operation)
	return &uds.Response{Success: true, Data: result}
}
