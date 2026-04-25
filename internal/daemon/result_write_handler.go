package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// sanitizeContentForLog truncates a string to maxLen and replaces control
// characters with '?' to prevent log injection when including untrusted
// content values (task content, skill candidates, summaries) in log messages.
func sanitizeContentForLog(s string) string {
	const maxLen = 200
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune('?')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fallbackRecorder records worker success/failure for health monitoring.
type fallbackRecorder interface {
	RecordSuccess(workerID string)
	RecordFailure(workerID string)
}

// circuitBreakerUpdater updates circuit breaker counters on result.
type circuitBreakerUpdater interface {
	UpdateCounterOnResult(state *model.CommandState, resultStatus model.Status, taskID string, resultID string, now time.Time) (bool, string)
	TripBreaker(state *model.CommandState, reason string, now time.Time)
}

// reviewDispatcher dispatches review requests for completed tasks.
type reviewDispatcher interface {
	Enabled() bool
	DispatchIfEligible(ctx context.Context, params ResultWriteParams)
}

// VerifyWorkdirResolver resolves the working directory in which the
// Verification Runner must execute commands for a given task. Production
// implementations consult the worktree manager so that verify sees the
// worker's uncommitted changes (or the integration worktree, or the project
// root for RunOnMain) — running verify against the wrong directory would
// either miss broken worker output or fail against unrelated main state.
type VerifyWorkdirResolver interface {
	// ResolveVerifyWorkdir returns the absolute directory in which to execute
	// verification commands for the given task/worker. Implementations must
	// honour task.RunOnMain / task.RunOnIntegration. An empty return value
	// signals "use the runner's default" (legacy fallback for tests).
	ResolveVerifyWorkdir(task *model.Task, workerID string) (string, error)
}

// ResultWriteAPI handles the "result_write" UDS endpoint.
type ResultWriteAPI struct {
	*apiContext
	// Domain-specific deps (late-bound via closures to support test wiring
	// where Daemon fields are set after newDaemon returns).
	fallbackMgr    func() fallbackRecorder
	circuitBreaker func() circuitBreakerUpdater
	reviewCoord    func() reviewDispatcher
	triggerScan    scanTriggerFunc
	ctx            func() context.Context
	// verifyRunner runs §S1-1 Verification after a task lands at verify_pending.
	// nil indicates no runner has been wired — resolveVerifyRunner falls back
	// to a fail-closed "verify_runner_not_configured" runner so that a daemon
	// startup or test wiring miss surfaces as a verify failure rather than a
	// silent pass. Production callers MUST inject a real runner via
	// SetVerifyRunner; tests use NewFixedVerifyRunner / SetVerifyRunner with
	// an explicit recording stand-in.
	verifyRunner VerifyRunner
	// verifyWorkdirResolver returns the per-task working directory for the
	// Verification Runner. nil falls back to the runner's own projectDir,
	// which preserves legacy test behaviour but is unsafe in production
	// because verify would run against main rather than the worker worktree.
	verifyWorkdirResolver VerifyWorkdirResolver
}

// SetVerifyRunner overrides the VerifyRunner used by this handler. Intended
// for tests that need to exercise the verify_pending → repair_pending path
// deterministically. Production wiring (cmd/maestrod) injects a
// RealVerifyRunner that executes verify.yaml or the Fallback Verify
// (DefaultVerifyConfig); when SetVerifyRunner is never called, the
// fail-closed default in resolveVerifyRunner routes the task to
// repair_pending so unverified work cannot silently flow through.
func (h *ResultWriteAPI) SetVerifyRunner(r VerifyRunner) {
	h.verifyRunner = r
}

// SetVerifyWorkdirResolver wires the per-task working-directory resolver for
// the Verification Runner. Production startup injects the WorktreeManager so
// that verify executes in the worker worktree (or integration worktree, or
// project root) rather than the daemon's CWD. Tests that drive verify via
// FixedVerifyRunner / recordingVerifyRunner do not need to wire this.
func (h *ResultWriteAPI) SetVerifyWorkdirResolver(r VerifyWorkdirResolver) {
	h.verifyWorkdirResolver = r
}

// resolveVerifyRunner returns the configured runner, or a fail-closed runner
// when nothing has been wired. The fail-closed default routes the task to
// repair_pending with reason "verify_runner_not_configured" — louder than
// silently passing, so a wiring bug surfaces in the audit log instead of
// letting unverified work flow through.
func (h *ResultWriteAPI) resolveVerifyRunner() VerifyRunner {
	if h.verifyRunner != nil {
		return h.verifyRunner
	}
	h.logFn(LogLevelWarn,
		"verify_runner_not_configured task lifecycle will be routed to repair_pending; "+
			"production wiring must call SetVerifyRunner with a real or skip runner")
	return newUnconfiguredVerifyRunner()
}

// resolveVerifyWorkingDir returns the working directory for the §S1-1
// Verification Runner for the task identified by params. The result honours
// task.RunOnMain / task.RunOnIntegration via the injected
// VerifyWorkdirResolver. An empty string is returned when the resolver is
// not wired (legacy test path) — RealVerifyRunner falls back to its own
// projectDir in that case.
//
// The queue task is re-read here because Phase A already released the queue
// lock before verify runs (verify executes outside the state lock per design
// — see handleResultWrite). A best-effort read is acceptable: a stale or
// missing entry simply causes the runner to fall back to its default dir,
// which is no worse than the prior behaviour.
func (h *ResultWriteAPI) resolveVerifyWorkingDir(params ResultWriteParams) string {
	if h.verifyWorkdirResolver == nil {
		return ""
	}
	tq, err := h.fileStore.LoadQueueFile(params.Reporter)
	if err != nil {
		h.logFn(LogLevelWarn,
			"verify_workdir_load_queue_failed reporter=%s error=%v (falling back to runner default)",
			params.Reporter, err)
		return ""
	}
	var task *model.Task
	for i := range tq.Tasks {
		if tq.Tasks[i].ID == params.TaskID {
			task = &tq.Tasks[i]
			break
		}
	}
	if task == nil {
		h.logFn(LogLevelWarn,
			"verify_workdir_task_missing reporter=%s task=%s (falling back to runner default)",
			params.Reporter, params.TaskID)
		return ""
	}
	wd, err := h.verifyWorkdirResolver.ResolveVerifyWorkdir(task, params.Reporter)
	if err != nil {
		h.logFn(LogLevelWarn,
			"verify_workdir_resolve_failed reporter=%s task=%s error=%v (falling back to runner default)",
			params.Reporter, params.TaskID, err)
		return ""
	}
	return wd
}

// ResultWriteParams is the request payload for the result_write UDS command.
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

// validateResultWriteParams parses and validates the result_write request parameters.
// Returns the parsed params, the terminal status, or an error response if validation fails.
func (h *ResultWriteAPI) validateResultWriteParams(req *uds.Request) (ResultWriteParams, model.Status, *uds.Response) {
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
		// valid terminal statuses for worker result reporting
	default:
		return params, "", uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("status must be completed|failed, got %q", params.Status))
	}

	return params, resultStatus, nil
}

// recordFallback records worker success/failure for health monitoring.
func (h *ResultWriteAPI) recordFallback(params ResultWriteParams, resultStatus model.Status) {
	if fm := h.fallbackMgr(); fm != nil {
		switch resultStatus {
		case model.StatusCompleted:
			fm.RecordSuccess(params.Reporter)
		case model.StatusFailed:
			fm.RecordFailure(params.Reporter)
		}
	}
}

func (h *ResultWriteAPI) handleResultWrite(req *uds.Request) *uds.Response {
	params, resultStatus, errResp := h.validateResultWriteParams(req)
	if errResp != nil {
		return errResp
	}

	// Phase A: Shared file lock + per-worker mutex (results/ + queue/ updates)
	resultWritePhaseAResult, err := h.resultWritePhaseA(params, resultStatus)
	if err != nil {
		rErr := &resultWriteError{}
		if errors.As(err, &rErr) {
			return uds.ErrorResponse(rErr.Code, rErr.Message)
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}
	resultID := resultWritePhaseAResult.resultID
	isDuplicate := resultWritePhaseAResult.duplicate

	// Bug H: for duplicate submissions, skip Phase B (state already reflects
	// the prior write) and the normal "result_write result_id=..." audit log
	// line (it would double-record a single logical write and mislead log
	// analysis). Best-effort writes (learnings / skill_candidates) must still
	// run so that lease-revoke rejections on stale idempotent resubmissions
	// continue to be persisted (H4 invariant).
	var needsVerify bool
	if !isDuplicate {
		// Phase B: Per-command mutex (state/ updates)
		retryScheduled := resultWritePhaseAResult.retryTask != nil
		nv, err := h.resultWritePhaseB(params, resultID, resultStatus,
			resultWritePhaseAResult.queueWriteFailed,
			resultWritePhaseAResult.originalTaskID,
			retryScheduled,
			resultWritePhaseAResult.abortByMaxRepair)
		if err != nil {
			h.logFn(LogLevelError, "result_write phase_b error task=%s command=%s: %v",
				params.TaskID, params.CommandID, err)
			return uds.ErrorResponse(uds.ErrCodeInternal,
				fmt.Sprintf("state update failed: %v (result %s committed, run 'maestro plan rebuild' to fix)", err, resultID))
		}
		needsVerify = nv

		h.recordFallback(params, resultStatus)

		// Retry registration (state then queue — correct lock order).
		h.handleRetryRegistration(resultWritePhaseAResult, params)
	}

	// §S1-1 Verification Runner second pass. Runs outside the Phase B state
	// lock so the runner does not block concurrent writers; if it fails or
	// the task has moved on, applyVerifyOutcome logs and leaves the task at
	// verify_pending for reconcile/operator intervention.
	if needsVerify {
		// Resolve the per-task working directory so verify sees the worker's
		// uncommitted changes (worker worktree), the integration branch
		// (RunOnIntegration), or the project root (RunOnMain / no worktree
		// mode). Falls back to "" — the runner's own projectDir — only when
		// no resolver has been wired (legacy tests).
		workingDir := h.resolveVerifyWorkingDir(params)
		runner := h.resolveVerifyRunner()
		outcome, vErr := runner.Run(h.ctx(), params.TaskID, params.CommandID, workingDir, params.FilesChanged)
		nextStatus, reason := classifyVerifyOutcome(outcome, vErr)
		if applyErr := h.applyVerifyOutcome(params, nextStatus, reason); applyErr != nil {
			h.logFn(LogLevelWarn,
				"verify_outcome_apply_failed task=%s command=%s next=%s reason=%q error=%v "+
					"(task remains at verify_pending; reconcile/operator can re-drive)",
				params.TaskID, params.CommandID, nextStatus, reason, applyErr)
		}
	}

	// Best-effort writes (learnings, skill candidates) with lease epoch guard.
	// Runs on both fresh and duplicate paths so that the lease rejection audit
	// trail (H4) is preserved when a stale worker resubmits with new
	// best-effort payload.
	rejectionID := h.handleBestEffortWrites(params, resultID, resultStatus)

	// Set agent status to idle now that the task result is committed.
	// Best-effort: failure to update tmux status must not fail the result write.
	setAgentIdle(params.Reporter, h.logFn)

	// Phase C: Trigger scan (best effort dependency unblocking).
	if h.triggerScan != nil {
		h.triggerScan(h.ctx())
	}

	if isDuplicate {
		h.logFn(LogLevelInfo,
			"result_write duplicate_short_circuited result_id=%s task=%s command=%s reporter=%s "+
				"(prior result already committed; Phase B skipped)",
			resultID, params.TaskID, params.CommandID, params.Reporter)
	} else {
		h.logFn(LogLevelInfo, "result_write result_id=%s task=%s command=%s status=%s reporter=%s",
			resultID, params.TaskID, params.CommandID, params.Status, params.Reporter)
	}
	respPayload := map[string]string{"result_id": resultID}
	if isDuplicate {
		respPayload["duplicate"] = "true"
	}
	if rejectionID != "" {
		respPayload["lease_rejection_id"] = rejectionID
		respPayload["lease_rejection_warning"] =
			"learnings/skill_candidates rejected: lease revoked; recorded as " + rejectionID
	}
	return uds.SuccessResponse(respPayload)
}

// setAgentIdle sets the @status tmux user variable to "idle" for the given agent.
// This is best-effort: errors are logged but do not propagate.
func setAgentIdle(agentID string, logFn logFunc) {
	paneTarget, err := tmux.FindPaneByAgentID(agentID)
	if err != nil {
		logFn(LogLevelDebug, "set_agent_idle pane_not_found agent=%s: %v", agentID, err)
		return
	}
	if err := tmux.SetUserVar(paneTarget, "status", "idle"); err != nil {
		logFn(LogLevelWarn, "set_agent_idle_failed agent=%s: %v", agentID, err)
	}
}

type resultWriteError struct {
	Code    string
	Message string
}

func (e *resultWriteError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// handleRetryRegistration registers a retry task in state and queue if phaseA
// determined one is needed. Runs after phaseA has released queue+result locks,
// so acquiring state(L2) then queue(L1) does not violate canonical order.
func (h *ResultWriteAPI) handleRetryRegistration(phaseAResult *resultWritePhaseAResult, params ResultWriteParams) {
	if phaseAResult.retryTask == nil {
		return
	}

	retryTask := phaseAResult.retryTask
	retryHandler := NewTaskRetryHandler(h.maestroDir, *h.config, h.lockMap, h.logger, h.logLevel)

	if err := retryHandler.RetryTaskAtomically(retryTask, params.CommandID, params.Reporter); err != nil {
		h.logFn(LogLevelError, "retry_task_atomic_failed task=%s worker=%s command=%s error=%v",
			retryTask.ID, params.Reporter, params.CommandID, err)
	} else {
		h.logFn(LogLevelInfo, "task_retry_scheduled task=%s retry_id=%s attempt=%d",
			params.TaskID, retryTask.ID, retryTask.Attempts)
	}
}

// truncateRunes truncates a string to at most maxRunes runes.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}
