package daemonapi

import (
	"encoding/json"
	"fmt"

	"github.com/msageha/maestro_v2/internal/daemon/apipolicy"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// QueueWriteParams is the decoded body of a queue-write UDS request.
// One struct covers all four queue targets (command / task /
// notification / cancel-request); fields not relevant to the active
// target are zero-valued and ignored by the dispatch func.
type QueueWriteParams struct {
	Target             string                   `json:"target"`
	Type               string                   `json:"type"`
	CommandID          string                   `json:"command_id,omitempty"`
	Content            string                   `json:"content,omitempty"`
	Purpose            string                   `json:"purpose,omitempty"`
	AcceptanceCriteria string                   `json:"acceptance_criteria,omitempty"`
	DefinitionOfDone   []string                 `json:"definition_of_done,omitempty"`
	BloomLevel         int                      `json:"bloom_level,omitempty"`
	BlockedBy          []string                 `json:"blocked_by,omitempty"`
	Constraints        []string                 `json:"constraints,omitempty"`
	ToolsHint          []string                 `json:"tools_hint,omitempty"`
	PersonaHint        string                   `json:"persona_hint,omitempty"`
	SkillRefs          []string                 `json:"skill_refs,omitempty"`
	ExpectedPaths      []string                 `json:"expected_paths,omitempty"`
	DefinitionOfAbort  *model.DefinitionOfAbort `json:"definition_of_abort,omitempty"`
	RunOnMain          bool                     `json:"run_on_main,omitempty"`
	RunOnIntegration   bool                     `json:"run_on_integration,omitempty"`
	OperationType      string                   `json:"operation_type,omitempty"`
	Priority           int                      `json:"priority"`
	SourceResultID     string                   `json:"source_result_id,omitempty"`
	NotificationType   string                   `json:"notification_type,omitempty"`
	RequestedBy        string                   `json:"requested_by,omitempty"`
	Reason             string                   `json:"reason,omitempty"`
	SystemCaller       string                   `json:"system_caller,omitempty"`
}

// QueueWriteFunc is the per-target backend a QueueWrite handler routes
// to once role and parameters have been validated.
type QueueWriteFunc func(QueueWriteParams) *uds.Response

// QueueWrite handles UDS queue-write requests by dispatching to the
// per-target backend (command / task / notification / cancel-request).
type QueueWrite struct {
	command       QueueWriteFunc
	task          QueueWriteFunc
	notification  QueueWriteFunc
	cancelRequest QueueWriteFunc
}

// NewQueueWrite constructs a QueueWrite handler bound to the four
// per-target backends. Any of the funcs may be nil; in that case the
// matching request type returns a clear "not implemented" error rather
// than panicking on a nil call.
func NewQueueWrite(command, task, notification, cancelRequest QueueWriteFunc) *QueueWrite {
	return &QueueWrite{
		command:       command,
		task:          task,
		notification:  notification,
		cancelRequest: cancelRequest,
	}
}

// Handle decodes the request body, applies the role policy for the
// requested target, and forwards to the appropriate backend func.
func (h *QueueWrite) Handle(req *uds.Request) *uds.Response {
	var params QueueWriteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	switch params.Type {
	case "command":
		if resp := apipolicy.RequireCallerRole(req, "queue_write command", uds.RoleOrchestrator, uds.RoleCLI); resp != nil {
			return resp
		}
		return h.command(params)
	case "task":
		if resp := apipolicy.RequireCallerRole(req, "queue_write task", uds.RoleCLI); resp != nil {
			return resp
		}
		return h.task(params)
	case "notification":
		if resp := apipolicy.RequireCallerRole(req, "queue_write notification", uds.RoleCLI); resp != nil {
			return resp
		}
		return h.notification(params)
	case "cancel-request":
		if resp := apipolicy.RequireCallerRole(req, "queue_write cancel-request", uds.RoleOrchestrator, uds.RoleCLI); resp != nil {
			return resp
		}
		return h.cancelRequest(params)
	default:
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("invalid type: %q, must be command|task|notification|cancel-request", params.Type))
	}
}
