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

func (e *ActionRequiredError) Error() string {
	return fmt.Sprintf("action required (%s): phase %q is %s, next_action: %s",
		e.Reason, e.PhaseName, e.PhaseStatus, e.NextAction)
}

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

func (e *ActionRequiredError) ErrorCode() string {
	return "ACTION_REQUIRED"
}
