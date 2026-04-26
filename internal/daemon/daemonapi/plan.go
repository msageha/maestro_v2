package daemonapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/msageha/maestro_v2/internal/daemon/apipolicy"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/plan"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

type validationFormatter interface {
	error
	FormatStderr() string
}

type codedFormatter interface {
	error
	FormatStderr() string
	ErrorCode() string
}

type PlanRecoveryWorktreeManager interface {
	Unquarantine(commandID string, reason string) error
	ResumeMerge(ctx context.Context, commandID string) error
	RetryPublish(commandID string) error
	AutoRecover(ctx context.Context, commandID string) (worktree.AutoRecoverAction, error)
	ResolveConflict(commandID, phaseID, workerID string) error
}

type Plan struct {
	maestroDir       string
	executor         core.PlanExecutor
	worktreeManager  PlanRecoveryWorktreeManager
	lock             func()
	unlock           func()
	commandStatePath func(maestroDir, commandID string) string
	publishQueue     func(source string)
	logInfof         Logf
	logWarnf         Logf
}

func NewPlan(
	maestroDir string,
	lock func(),
	unlock func(),
	commandStatePath func(maestroDir, commandID string) string,
	publishQueue func(source string),
	logInfof Logf,
	logWarnf Logf,
) *Plan {
	return &Plan{
		maestroDir:       maestroDir,
		lock:             lock,
		unlock:           unlock,
		commandStatePath: commandStatePath,
		publishQueue:     publishQueue,
		logInfof:         logInfof,
		logWarnf:         logWarnf,
	}
}

func (h *Plan) SetExecutor(pe core.PlanExecutor) {
	h.executor = pe
}

func (h *Plan) SetWorktreeManager(wm PlanRecoveryWorktreeManager) {
	h.worktreeManager = wm
}

func (h *Plan) Handle(req *uds.Request) *uds.Response {
	var params struct {
		Operation string          `json:"operation"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	switch params.Operation {
	case "unquarantine", "resume_merge", "resolve_conflict", "retry_publish", "auto_recover":
		if !uds.ValidCallerRoles[req.CallerRole] {
			return uds.ErrorResponse(uds.ErrCodeValidation,
				fmt.Sprintf("operation %q requires a valid caller role, got %q", params.Operation, req.CallerRole))
		}
		if req.CallerRole == uds.RoleWorker {
			return uds.ErrorResponse(uds.ErrCodeValidation,
				fmt.Sprintf("operation %q is not permitted for caller role %q", params.Operation, req.CallerRole))
		}
		if params.Operation == "unquarantine" && req.CallerRole == uds.RolePlanner {
			return uds.ErrorResponse(uds.ErrCodeValidation,
				"operation \"unquarantine\" is restricted to operator role; Planner is not permitted")
		}
		return h.handleWorktreeRecovery(params.Operation, params.Data)
	}

	if h.executor == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "plan executor not configured")
	}

	h.lock()
	defer h.unlock()

	var result json.RawMessage
	var err error

	switch params.Operation {
	case "submit":
		if resp := apipolicy.RequireCallerRole(req, "plan submit", uds.RolePlanner, uds.RoleCLI); resp != nil {
			return resp
		}
		result, err = h.executor.Submit(params.Data)
	case "complete":
		if resp := apipolicy.RequireCallerRole(req, "plan complete", uds.RolePlanner, uds.RoleCLI); resp != nil {
			return resp
		}
		result, err = h.executor.Complete(params.Data)
	case "add_retry_task":
		if resp := apipolicy.RequireCallerRole(req, "plan add_retry_task", uds.RolePlanner, uds.RoleCLI); resp != nil {
			return resp
		}
		result, err = h.executor.AddRetryTask(params.Data)
	case "add_task":
		if resp := apipolicy.RequireCallerRole(req, "plan add_task", uds.RolePlanner, uds.RoleCLI); resp != nil {
			return resp
		}
		result, err = h.executor.AddTask(params.Data)
	case "rebuild":
		if resp := apipolicy.RequireCallerRole(req, "plan rebuild", uds.RolePlanner, uds.RoleCLI); resp != nil {
			return resp
		}
		result, err = h.executor.Rebuild(params.Data)
	default:
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("unknown plan operation: %q", params.Operation))
	}

	if err != nil {
		if params.Operation == "submit" && errors.Is(err, plan.ErrDoubleSubmit) {
			h.logInfof("plan_submit_duplicate_rejected error=%v", err)
		} else {
			h.logWarnf("plan_%s error=%v", params.Operation, err)
		}
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

	isDryRun := false
	if params.Operation == "submit" {
		var hint struct {
			DryRun bool `json:"dry_run"`
		}
		if json.Unmarshal(params.Data, &hint) == nil && hint.DryRun {
			isDryRun = true
		}
	}

	if isDryRun {
		h.logInfof("plan_%s_dry_run success", params.Operation)
	} else {
		h.logInfof("plan_%s success", params.Operation)
	}
	if params.Operation != "rebuild" && !isDryRun {
		h.publishQueue("plan_" + params.Operation)
	}

	return &uds.Response{Success: true, Data: result}
}

func (h *Plan) handleWorktreeRecovery(operation string, data json.RawMessage) *uds.Response {
	if h.worktreeManager == nil {
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

	h.lock()
	defer h.unlock()

	if _, err := os.Stat(h.commandStatePath(h.maestroDir, p.CommandID)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return uds.ErrorResponse(uds.ErrCodeNotFound, fmt.Sprintf("command not found: %s", p.CommandID))
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("stat command state: %v", err))
	}

	var opErr error
	var autoRecoverAction worktree.AutoRecoverAction
	switch operation {
	case "unquarantine":
		opErr = h.worktreeManager.Unquarantine(p.CommandID, p.Reason)
	case "resume_merge":
		opErr = h.worktreeManager.ResumeMerge(context.Background(), p.CommandID)
	case "retry_publish":
		opErr = h.worktreeManager.RetryPublish(p.CommandID)
	case "auto_recover":
		autoRecoverAction, opErr = h.worktreeManager.AutoRecover(context.Background(), p.CommandID)
	case "resolve_conflict":
		if p.PhaseID == "" {
			return uds.ErrorResponse(uds.ErrCodeValidation, "phase_id is required")
		}
		if err := validate.PhaseID(p.PhaseID); err != nil {
			return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid phase_id: %v", err))
		}
		if p.WorkerID == "" {
			return uds.ErrorResponse(uds.ErrCodeValidation, "worker_id is required")
		}
		if err := validate.ID(p.WorkerID); err != nil {
			return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid worker_id: %v", err))
		}
		if len(p.ConflictingFiles) > 0 {
			h.logInfof("plan_resolve_conflict command=%s phase=%s worker=%s conflicting_files=%v",
				p.CommandID, p.PhaseID, p.WorkerID, p.ConflictingFiles)
		}
		opErr = h.worktreeManager.ResolveConflict(p.CommandID, p.PhaseID, p.WorkerID)
	}

	if opErr != nil {
		h.logWarnf("plan_%s error=%v", operation, opErr)
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

	h.logInfof("plan_%s success command=%s", operation, p.CommandID)
	h.publishQueue("plan_" + operation)

	payload := map[string]string{
		"command_id": p.CommandID,
		"operation":  operation,
		"status":     "ok",
	}
	if operation == "auto_recover" {
		action := string(autoRecoverAction)
		if action == "" {
			action = string(worktree.AutoRecoverNone)
		}
		payload["action"] = action
	}
	out, _ := json.Marshal(payload)
	return &uds.Response{Success: true, Data: out}
}
