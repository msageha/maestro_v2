package plan

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
)

// ValidateNotCancelled returns an error if the command has been cancelled.
func ValidateNotCancelled(state *model.CommandState) error {
	if state.Cancel.Requested {
		return &PlanValidationError{Msg: fmt.Sprintf("command %s has been cancelled", state.CommandID)}
	}
	return nil
}
