package plan

import (
	"fmt"
	"strings"
)

type ValidationError struct {
	FieldPath string
	Message   string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.FieldPath, e.Message)
}

type ValidationErrors struct {
	Errors []ValidationError
}

func (ve *ValidationErrors) Add(fieldPath, message string) {
	ve.Errors = append(ve.Errors, ValidationError{FieldPath: fieldPath, Message: message})
}

func (ve *ValidationErrors) HasErrors() bool {
	return len(ve.Errors) > 0
}

func (ve *ValidationErrors) Error() string {
	msgs := make([]string, 0, len(ve.Errors))
	for _, e := range ve.Errors {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "\n")
}

func (ve *ValidationErrors) FormatStderr() string {
	var sb strings.Builder
	for _, e := range ve.Errors {
		fmt.Fprintf(&sb, "error: %s: %s\n", e.FieldPath, e.Message)
	}
	return sb.String()
}

// PlanValidationError represents a plan-level validation error
// (state precondition, cancel check, etc.) as opposed to field-level
// ValidationErrors from task/phase input validation.
// Satisfies the validationFormatter interface in daemon/plan_handler.go.
type PlanValidationError struct {
	Msg string
}

func (e *PlanValidationError) Error() string {
	return e.Msg
}

func (e *PlanValidationError) FormatStderr() string {
	return fmt.Sprintf("error: %s\n", e.Msg)
}
