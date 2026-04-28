package daemonapi

import (
	"encoding/json"
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// ResultWriteParams is the decoded body of a result-write UDS request
// (the worker's "I finished my task" call).
type ResultWriteParams struct {
	Reporter               string   `json:"reporter"`
	TaskID                 string   `json:"task_id"`
	CommandID              string   `json:"command_id"`
	LeaseEpoch             int      `json:"lease_epoch"`
	Status                 string   `json:"status"`
	Summary                string   `json:"summary"`
	FilesChanged           []string `json:"files_changed,omitempty"`
	PartialChangesPossible bool     `json:"partial_changes_possible,omitempty"`
	RetrySafe              bool     `json:"retry_safe,omitempty"`
	ExitCode               *int     `json:"exit_code,omitempty"`
	Learnings              []string `json:"learnings,omitempty"`
	SkillCandidates        []string `json:"skill_candidates,omitempty"`
}

// ResultWriteFunc is the daemon-side backend that persists the result
// after the request body has been validated.
type ResultWriteFunc func(ResultWriteParams, model.Status) *uds.Response

// ResultWrite handles the result-write UDS request: it validates the
// request body and forwards to the configured ResultWriteFunc.
type ResultWrite struct {
	write ResultWriteFunc
}

// NewResultWrite constructs a ResultWrite handler bound to write.
func NewResultWrite(write ResultWriteFunc) *ResultWrite {
	return &ResultWrite{write: write}
}

// Handle implements the uds.Handler contract by delegating validation
// to ValidateResultWriteRequest and the persistence call to the
// configured ResultWriteFunc.
func (h *ResultWrite) Handle(req *uds.Request) *uds.Response {
	params, status, resp := ValidateResultWriteRequest(req)
	if resp != nil {
		return resp
	}
	return h.write(params, status)
}

// ValidateResultWriteRequest unmarshals and validates the request body,
// returning the decoded params, normalized model.Status, and a non-nil
// *uds.Response when the request is malformed (in which case the caller
// must surface it verbatim).
func ValidateResultWriteRequest(req *uds.Request) (ResultWriteParams, model.Status, *uds.Response) {
	var params ResultWriteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return params, "", uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	if params.Reporter == "" {
		return params, "", uds.ErrorResponse(uds.ErrCodeValidation, "reporter is required")
	}
	if !validate.IsValidBaseName(params.Reporter) {
		return params, "", uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid reporter: %q", params.Reporter))
	}
	if params.TaskID == "" {
		return params, "", uds.ErrorResponse(uds.ErrCodeValidation, "task_id is required")
	}
	if params.CommandID == "" {
		return params, "", uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required")
	}
	if err := validate.ID(params.CommandID); err != nil {
		return params, "", uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid command_id: %v", err))
	}
	if err := validate.ID(params.TaskID); err != nil {
		return params, "", uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid task_id: %v", err))
	}

	resultStatus := model.Status(params.Status)
	switch resultStatus {
	case model.StatusCompleted, model.StatusFailed:
	default:
		return params, "", uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("status must be completed|failed, got %q", params.Status))
	}

	return params, resultStatus, nil
}
