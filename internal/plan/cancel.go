package plan

import (
	"fmt"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

func SetCancelRequested(state *model.CommandState, requestedBy, reason string) error {
	if state.Cancel.Requested {
		return nil // idempotent
	}
	now := time.Now().UTC().Format(time.RFC3339)
	state.Cancel.Requested = true
	state.Cancel.RequestedAt = &now
	state.Cancel.RequestedBy = &requestedBy
	state.Cancel.Reason = &reason
	state.UpdatedAt = now
	return nil
}

func IsCancelRequested(state *model.CommandState) bool {
	return state.Cancel.Requested
}

func ValidateNotCancelled(state *model.CommandState) error {
	if state.Cancel.Requested {
		return &PlanValidationError{Msg: fmt.Sprintf("command %s has been cancelled", state.CommandID)}
	}
	return nil
}
