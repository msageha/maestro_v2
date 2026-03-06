package plan

import (
	"fmt"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// SetCancelRequested marks a command state as cancel-requested. The operation is idempotent.
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

// IsCancelRequested returns true if a cancellation has been requested for the command.
func IsCancelRequested(state *model.CommandState) bool {
	return state.Cancel.Requested
}

// ValidateNotCancelled returns an error if the command has been cancelled.
func ValidateNotCancelled(state *model.CommandState) error {
	if state.Cancel.Requested {
		return &PlanValidationError{Msg: fmt.Sprintf("command %s has been cancelled", state.CommandID)}
	}
	return nil
}
