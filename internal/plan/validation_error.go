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
	var msgs []string
	for _, e := range ve.Errors {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "\n")
}

func (ve *ValidationErrors) FormatStderr() string {
	var sb strings.Builder
	for _, e := range ve.Errors {
		sb.WriteString(fmt.Sprintf("error: %s: %s\n", e.FieldPath, e.Message))
	}
	return sb.String()
}
