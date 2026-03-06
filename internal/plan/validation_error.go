package plan

import (
	"fmt"
	"strings"
)

// ValidationError represents a single field-level validation error with its path and message.
type ValidationError struct {
	FieldPath string
	Message   string
}

// Error returns a formatted string combining the field path and message.
func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.FieldPath, e.Message)
}

// ValidationErrors collects multiple field-level validation errors.
type ValidationErrors struct {
	Errors []ValidationError
}

// Add appends a new validation error for the given field path and message.
func (ve *ValidationErrors) Add(fieldPath, message string) {
	ve.Errors = append(ve.Errors, ValidationError{FieldPath: fieldPath, Message: message})
}

// HasErrors returns true if any validation errors have been recorded.
func (ve *ValidationErrors) HasErrors() bool {
	return len(ve.Errors) > 0
}

// Error returns all validation errors joined by newlines, satisfying the error interface.
func (ve *ValidationErrors) Error() string {
	msgs := make([]string, 0, len(ve.Errors))
	for _, e := range ve.Errors {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "\n")
}

// FormatStderr returns all validation errors formatted for stderr output.
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

// Error returns the validation error message.
func (e *PlanValidationError) Error() string {
	return e.Msg
}

// FormatStderr returns the error formatted for stderr output.
func (e *PlanValidationError) FormatStderr() string {
	return fmt.Sprintf("error: %s\n", e.Msg)
}

// ActionRequiredError indicates that the plan cannot complete because
// a specific agent action is required first. Provides structured guidance
// that LLM agents can parse to determine their next action.
type ActionRequiredError struct {
	Reason      string // e.g. "PHASE_AWAITING_FILL"
	CommandID   string
	PhaseID     string
	PhaseName   string
	PhaseStatus string
	NextAction  string // e.g. "maestro plan submit --command-id <cmd> --phase <name> --tasks-file plan.yaml"
}

// Error returns a summary of the required action including phase status and next step.
func (e *ActionRequiredError) Error() string {
	return fmt.Sprintf("action required (%s): phase %q is %s, next_action: %s",
		e.Reason, e.PhaseName, e.PhaseStatus, e.NextAction)
}

// FormatStderr returns structured action-required details formatted for stderr output.
func (e *ActionRequiredError) FormatStderr() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "error: action required (%s)\n", e.Reason)
	fmt.Fprintf(&sb, "command_id: %s\n", e.CommandID)
	fmt.Fprintf(&sb, "phase_id: %s\n", e.PhaseID)
	fmt.Fprintf(&sb, "phase: %s\n", e.PhaseName)
	fmt.Fprintf(&sb, "phase_status: %s\n", e.PhaseStatus)
	fmt.Fprintf(&sb, "next_action: %s\n", e.NextAction)
	return sb.String()
}

// ErrorCode returns the machine-readable error code "ACTION_REQUIRED".
func (e *ActionRequiredError) ErrorCode() string {
	return "ACTION_REQUIRED"
}
