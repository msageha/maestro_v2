package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// validationFormatter is satisfied by plan.ValidationErrors without importing plan.
type validationFormatter interface {
	error
	FormatStderr() string
}

// codedFormatter extends validationFormatter with a custom error code.
// Satisfied by plan.ActionRequiredError.
type codedFormatter interface {
	error
	FormatStderr() string
	ErrorCode() string
}

// PlanExecutor is defined in internal/daemon/core and re-exported via core_aliases.go.

// SetPlanExecutor wires the plan executor for UDS plan handlers.
func (d *Daemon) SetPlanExecutor(pe PlanExecutor) {
	d.planExecutor = pe
}

func (a *API) handlePlan(req *uds.Request) *uds.Response {
	d := a.d

	var params struct {
		Operation string          `json:"operation"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	// Operations that route through the worktree manager rather than the
	// plan executor (operator-recovery commands). Trust boundary: only
	// known, authenticated roles may invoke these. Workers are explicitly
	// blocked even if they bypass the launcher --disallowedTools and policy
	// hook layers. Empty or unknown CallerRole is rejected to prevent
	// unauthenticated shell invocations from reaching recovery endpoints.
	switch params.Operation {
	case "unquarantine", "resume_merge", "resolve_conflict":
		if !isValidCallerRole(req.CallerRole) {
			return uds.ErrorResponse(uds.ErrCodeValidation,
				fmt.Sprintf("operation %q requires a valid caller role, got %q", params.Operation, req.CallerRole))
		}
		if req.CallerRole == "worker" {
			return uds.ErrorResponse(uds.ErrCodeValidation,
				fmt.Sprintf("operation %q is not permitted for caller role %q", params.Operation, req.CallerRole))
		}
		return a.handlePlanWorktreeRecovery(params.Operation, params.Data)
	}

	if d.planExecutor == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "plan executor not configured")
	}

	a.acquireFileLock()
	defer a.releaseFileLock()

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
		var cf codedFormatter
		if errors.As(err, &cf) {
			return uds.ErrorResponse(cf.ErrorCode(), cf.FormatStderr())
		}
		var ve validationFormatter
		if errors.As(err, &ve) {
			return uds.ErrorResponse(uds.ErrCodeValidation, ve.FormatStderr())
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}

	d.log(LogLevelInfo, "plan_%s success", params.Operation)

	// Trigger an immediate queue scan for operations that write to worker/planner
	// queue files. Without this, the daemon relies on fsnotify (which may miss
	// AtomicWrite's os.Rename on macOS) or the 60-second periodic scan, causing
	// significant dispatch delay. "rebuild" only updates state and needs no scan.
	if params.Operation != "rebuild" {
		a.publishQueueWritten("plan_" + params.Operation)
	}

	return &uds.Response{Success: true, Data: result}
}

// handlePlanWorktreeRecovery serves the operator-recovery plan operations
// (unquarantine, resume_merge). Both delegate to the worktree manager rather
// than the plan executor and produce uniform error mapping for the CLI.
func (a *API) handlePlanWorktreeRecovery(operation string, data json.RawMessage) *uds.Response {
	d := a.d
	if d.worktreeManager == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "worktree manager not configured (worktree.enabled=false?)")
	}

	var p struct {
		CommandID        string   `json:"command_id"`
		Reason           string   `json:"reason"`
		PhaseID          string   `json:"phase_id"`
		WorkerID         string   `json:"worker_id"`
		ConflictingFiles []string `json:"conflicting_files"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}
	if p.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required")
	}
	if err := validate.ID(p.CommandID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid command_id: %v", err))
	}

	a.acquireFileLock()
	defer a.releaseFileLock()

	// Check that the command itself exists (state/commands/<id>.yaml) under
	// the file lock to avoid a TOCTOU window with concurrent queue scans.
	// This distinguishes "no such command" from "command exists but never
	// used worktree mode" so the CLI can surface accurate error messages.
	commandStatePath := filepath.Join(d.maestroDir, "state", "commands", p.CommandID+".yaml")
	if _, err := os.Stat(commandStatePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return uds.ErrorResponse(uds.ErrCodeNotFound, fmt.Sprintf("command not found: %s", p.CommandID))
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("stat command state: %v", err))
	}

	var opErr error
	switch operation {
	case "unquarantine":
		opErr = d.worktreeManager.Unquarantine(p.CommandID, p.Reason)
	case "resume_merge":
		opErr = d.worktreeManager.ResumeMerge(p.CommandID)
	case "resolve_conflict":
		if p.PhaseID == "" {
			return uds.ErrorResponse(uds.ErrCodeValidation, "phase_id is required")
		}
		if err := validate.ID(p.PhaseID); err != nil {
			return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid phase_id: %v", err))
		}
		if p.WorkerID == "" {
			return uds.ErrorResponse(uds.ErrCodeValidation, "worker_id is required")
		}
		if err := validate.ID(p.WorkerID); err != nil {
			return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid worker_id: %v", err))
		}
		// conflicting_files is an optional operator-supplied hint about which
		// paths are in conflict. It is recorded in the daemon log so that the
		// resolution can be correlated with the operator's intent, but the
		// underlying worktree.ResolveConflict signature is intentionally not
		// extended here (out of scope for this task).
		if len(p.ConflictingFiles) > 0 {
			d.log(LogLevelInfo, "plan_resolve_conflict command=%s phase=%s worker=%s conflicting_files=%v",
				p.CommandID, p.PhaseID, p.WorkerID, p.ConflictingFiles)
		}
		opErr = d.worktreeManager.ResolveConflict(p.CommandID, p.PhaseID, p.WorkerID)
	}

	if opErr != nil {
		d.log(LogLevelWarn, "plan_%s error=%v", operation, opErr)
		switch {
		case errors.Is(opErr, worktree.ErrNoWorktreeState):
			return uds.ErrorResponse(uds.ErrCodeNotFound,
				fmt.Sprintf("no worktree state for command %s (worktree mode unused or not yet initialized)", p.CommandID))
		case errors.Is(opErr, worktree.ErrAlreadyResolved):
			return uds.ErrorResponse(uds.ErrCodeActionRequired, opErr.Error())
		default:
			return uds.ErrorResponse(uds.ErrCodeInternal, opErr.Error())
		}
	}

	d.log(LogLevelInfo, "plan_%s success command=%s", operation, p.CommandID)
	a.publishQueueWritten("plan_" + operation)

	out, _ := json.Marshal(map[string]string{
		"command_id": p.CommandID,
		"operation":  operation,
		"status":     "ok",
	})
	return &uds.Response{Success: true, Data: out}
}

// validCallerRoles is the authoritative set of roles that may invoke
// operator-recovery plan operations. The set is intentionally small and
// must be maintained explicitly — any new role requires a conscious addition.
var validCallerRoles = map[string]bool{
	"orchestrator": true,
	"planner":      true,
	"worker":       true,
	"operator":     true,
}

// isValidCallerRole returns true if role is a known, non-empty caller role.
func isValidCallerRole(role string) bool {
	return validCallerRoles[role]
}
