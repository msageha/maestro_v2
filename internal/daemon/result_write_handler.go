package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/daemonapi"
	"github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
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

type autoRecoverWorktreeManager interface {
	AutoRecoverAfterResolution(ctx context.Context, commandID, workerID string, runOnIntegration bool) (worktree.AutoRecoverAction, error)
	ResetResolvingWorkerToConflict(commandID, workerID string) error
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

// ResultWriteAPI owns result-write domain processing after daemonapi has
// decoded and shallow-validated the UDS request. Keep transport concerns in
// daemonapi; keep durable state transitions and their lock ordering here until
// they can be split into dedicated phase services without widening public APIs.
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
	// worktreeManager drives the post-completion AutoRecover hook that
	// closes the merge_conflict / publish_conflict recovery loop without
	// depending on the Planner agent to call resume-merge / retry-publish
	// explicitly. nil disables the hook (legacy tests + worktree-disabled
	// runs); production startup wires this via SetWorktreeManager.
	worktreeManager autoRecoverWorktreeManager
	// verifyAsync controls whether the Verification Runner second pass is
	// admitted as daemon background work. Production enables this so the worker's
	// result_write CLI does not block on a potentially long build/test command.
	// Unit tests leave it false and keep the deterministic synchronous path.
	verifyAsync bool
	// admissionCtrl enforces max_concurrent_verify for daemon-owned background
	// verification work. Queue-dispatched verify tasks are admitted in
	// queue_scan_collect; result_write async verify is not a queue task, so it
	// must acquire/release its own verify slot here.
	admissionCtrl *admission.Controller
	statusSink    AgentStatusSink
}

// SetVerifyRunner overrides the VerifyRunner used by this handler. Intended
// for tests that need to exercise the verify_pending → repair_pending path
// deterministically. Production wiring injects a RealVerifyRunner that executes
// the command-scoped verify snapshot or the project fallback; when
// SetVerifyRunner is never called, the
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

// SetWorktreeManager wires the WorktreeManager used by the post-completion
// AutoRecover hook (see maybeAutoRecoverAfterResolution). Production startup
// passes the same Manager that owns the worktree state files; tests that do
// not exercise the recovery loop may leave this nil to disable the hook.
func (h *ResultWriteAPI) SetWorktreeManager(wm *WorktreeManager) {
	h.worktreeManager = wm
}

// SetVerifyAsync enables or disables background verification after the core
// result has been committed.
func (h *ResultWriteAPI) SetVerifyAsync(enabled bool) {
	h.verifyAsync = enabled
}

func (h *ResultWriteAPI) SetAdmissionController(ac *admission.Controller) {
	h.admissionCtrl = ac
}

// maybeAutoRecoverAfterResolution closes the conflict / publish_conflict
// recovery loop autonomously by invoking AutoRecoverAfterResolution after a
// fresh result_write whose terminal status is `completed`, or by resetting a
// stuck `resolving` worker after a `failed` result so R7's 20-minute stall
// sweep is not the only path back to forward progress.
//
// Caller contract:
//
//   - duplicate / nil-WM paths must be filtered before calling this; the
//     handler-side gate keeps the helper itself purely concerned with the
//     state-machine shape.
//   - resultStatus is the worker-reported status (StatusCompleted /
//     StatusFailed); finalStatus is the terminal task status after
//     classifyVerifyOutcome has run (so verify-failed tasks land here as
//     StatusRepairPending, not StatusCompleted, and are skipped).
//   - taskRunOnIntegration is the queue task's RunOnIntegration flag,
//     propagated from Phase A. Worktree.Manager.AutoRecoverAfterResolution
//     uses it to disambiguate publish_conflict from merge_conflict
//     completions.
//
// All errors are logged at warn level and swallowed: a failed AutoRecover
// must never fail the result_write itself, since the result is already
// committed and propagating the error would mislead the caller into
// thinking their report was rejected.
func (h *ResultWriteAPI) maybeAutoRecoverAfterResolution(
	params ResultWriteParams,
	resultStatus, finalStatus model.Status,
	taskRunOnIntegration bool,
) {
	if h.worktreeManager == nil {
		return
	}

	switch {
	case finalStatus == model.StatusCompleted:
		// Successful completion (verify pass when verify ran, or worker
		// success when verify did not run). Try the recovery dispatch.
		action, err := h.worktreeManager.AutoRecoverAfterResolution(
			h.ctx(), params.CommandID, params.Reporter, taskRunOnIntegration)
		if err != nil {
			// AutoRecoverAfterResolution swallows ErrAlreadyResolved and
			// returns AutoRecoverNone; anything else here is a real
			// recovery failure (e.g. ResumeMerge git op error). Log loud
			// enough that operators see it, but do not fail the
			// result_write — R7 (and the next AutoRecover) can still
			// retry.
			h.logFn(LogLevelWarn,
				"auto_recover_after_resolution_failed command=%s reporter=%s task=%s action=%s error=%v "+
					"(result already committed; reconcile/scan will retry)",
				params.CommandID, params.Reporter, params.TaskID, action, err)
			return
		}
		if action != worktree.AutoRecoverNone {
			h.logFn(LogLevelInfo,
				"auto_recover_after_resolution command=%s reporter=%s task=%s action=%s",
				params.CommandID, params.Reporter, params.TaskID, action)
		}

	case resultStatus == model.StatusFailed:
		// Worker reported failure on what may have been a merge_conflict
		// resolution task. Without a hint here the worker stays in
		// resolving until R7's resolvingStallTimeout (~20 minutes) sweeps
		// it back to conflict, blocking command progress for the entire
		// interval. ResetResolvingWorkerToConflict is idempotent and
		// no-ops when the worker is not actually in resolving status, so
		// it is safe to call regardless of whether the failed task was
		// genuinely a conflict resolution task.
		if err := h.worktreeManager.ResetResolvingWorkerToConflict(
			params.CommandID, params.Reporter); err != nil {
			h.logFn(LogLevelWarn,
				"reset_resolving_worker_failed command=%s worker=%s error=%v "+
					"(R7 will sweep within resolvingStallTimeout)",
				params.CommandID, params.Reporter, err)
		}
	}
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
// VerifyWorkdirResolver. An empty string is returned only when the resolver is
// not wired (legacy test path). Once a resolver is configured, queue lookup and
// workdir resolution failures are fail-closed so production verify cannot
// silently run against the project root instead of the worker worktree.
//
// The queue task is re-read here because Phase A already released the queue
// lock before verify runs (verify executes outside the state lock per design
// — see handleResultWrite).
func (h *ResultWriteAPI) resolveVerifyWorkingDir(params ResultWriteParams) (string, error) {
	if h.verifyWorkdirResolver == nil {
		return "", nil
	}
	tq, err := h.fileStore.LoadQueueFile(params.Reporter)
	if err != nil {
		return "", fmt.Errorf("verify_workdir_load_queue_failed reporter=%s: %w", params.Reporter, err)
	}
	var task *model.Task
	for i := range tq.Tasks {
		if tq.Tasks[i].ID == params.TaskID {
			task = &tq.Tasks[i]
			break
		}
	}
	if task == nil {
		return "", fmt.Errorf("verify_workdir_task_missing reporter=%s task=%s", params.Reporter, params.TaskID)
	}
	wd, err := h.verifyWorkdirResolver.ResolveVerifyWorkdir(task, params.Reporter)
	if err != nil {
		return "", fmt.Errorf("verify_workdir_resolve_failed reporter=%s task=%s: %w", params.Reporter, params.TaskID, err)
	}
	if wd == "" {
		return "", fmt.Errorf("verify_workdir_empty reporter=%s task=%s", params.Reporter, params.TaskID)
	}
	return wd, nil
}

type ResultWriteParams = daemonapi.ResultWriteParams

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
	params, resultStatus, errResp := daemonapi.ValidateResultWriteRequest(req)
	if errResp != nil {
		return errResp
	}
	return h.handleValidatedResultWrite(params, resultStatus)
}

func (h *ResultWriteAPI) handleValidatedResultWrite(params ResultWriteParams, resultStatus model.Status) *uds.Response {
	// Phase A: Shared file lock + per-worker mutex (results/ + queue/ updates)
	resultWritePhaseAResult, err := newResultPhaseAService(h).Run(params, resultStatus)
	if err != nil {
		// F-019: prefer the fencing-typed error first so the structured
		// Details payload is forwarded to the UDS response.
		fencingErr := &resultWriteFencingError{}
		if errors.As(err, &fencingErr) {
			return uds.ErrorResponseWithDetails(fencingErr.Code, fencingErr.Message, fencingErr.Details)
		}
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
	phaseBStatus := resultStatus
	if !isDuplicate {
		// Phase B: Per-command mutex (state/ updates)
		retryScheduled := resultWritePhaseAResult.retryTask != nil
		nv, st, err := newResultPhaseBService(h).Run(params, resultID, resultStatus,
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
		phaseBStatus = st

		newResultPostProcessor(h).AfterPhaseB(resultPostPhaseBInput{
			params:       params,
			phaseA:       resultWritePhaseAResult,
			resultStatus: resultStatus,
			phaseBStatus: phaseBStatus,
		})
	}

	// finalStatus tracks the terminal task status after VerifyRunner has had a
	// chance to overrule the worker-reported status. For non-verify paths it
	// is the worker-reported resultStatus; for verify paths it is the
	// classifyVerifyOutcome result. The post-completion AutoRecover hook
	// (below) reads finalStatus so it cannot fire for a verify-failed task
	// (which would otherwise commit unverified resolution edits).
	finalStatus, verifyWillRunAsync := h.runResultVerification(params, resultWritePhaseAResult, needsVerify, phaseBStatus)

	// Best-effort writes (learnings, skill candidates) with lease epoch guard.
	// Runs on both fresh and duplicate paths so that the lease rejection audit
	// trail (H4) is preserved when a stale worker resubmits with new
	// best-effort payload. Advisory review is dispatched separately because
	// asynchronous verification must pass before a review is queued.
	rejectionID := newResultBestEffortService(h).Handle(params, resultID)
	post := newResultPostProcessor(h)
	post.AfterVerification(resultPostFinalizeInput{
		params:             params,
		phaseA:             resultWritePhaseAResult,
		resultStatus:       resultStatus,
		finalStatus:        finalStatus,
		duplicate:          isDuplicate,
		verifyWillRunAsync: verifyWillRunAsync,
	})
	post.AfterResponseSideEffects(params)

	if isDuplicate {
		h.logFn(LogLevelInfo,
			"result_write duplicate_short_circuited result_id=%s task=%s command=%s reporter=%s "+
				"(prior result already committed; Phase B skipped)",
			resultID, params.TaskID, params.CommandID, params.Reporter)
	} else {
		h.logFn(LogLevelInfo, "result_write result_id=%s task=%s command=%s status=%s reporter=%s",
			resultID, params.TaskID, params.CommandID, params.Status, params.Reporter)
	}
	return newResultWriteResponse(resultWriteResponse{
		ResultID:      resultID,
		Duplicate:     isDuplicate,
		VerifyPending: verifyWillRunAsync,
		LeaseRejectID: rejectionID,
	})
}

type resultVerifyInput struct {
	params               ResultWriteParams
	sourceTask           *model.Task
	taskRunOnIntegration bool
}

func (h *ResultWriteAPI) runVerifySecondPass(ctx context.Context, input resultVerifyInput) model.Status {
	params := input.params
	workingDir, wdErr := h.resolveVerifyWorkingDir(params)
	runner := h.resolveVerifyRunner()
	var expectedPaths []string
	if input.sourceTask != nil {
		expectedPaths = input.sourceTask.ExpectedPaths
	}
	var outcome VerifyOutcome
	var vErr error
	if wdErr != nil {
		h.logFn(LogLevelWarn, "verify_workdir_resolution_failed task=%s command=%s error=%v", params.TaskID, params.CommandID, wdErr)
		outcome = VerifyOutcome{Passed: false, Reason: wdErr.Error()}
	} else {
		outcome, vErr = runner.Run(ctx, params.TaskID, params.CommandID, workingDir, expectedPaths)
	}
	nextStatus, reason := classifyVerifyOutcome(outcome, vErr)
	if applyErr := h.applyVerifyOutcome(params, nextStatus, reason); applyErr != nil {
		h.logFn(LogLevelWarn,
			"verify_outcome_apply_failed task=%s command=%s next=%s reason=%q error=%v "+
				"(task remains at verify_pending; reconcile/operator can re-drive)",
			params.TaskID, params.CommandID, nextStatus, reason, applyErr)
		return model.StatusVerifyPending
	}

	finalStatus := nextStatus
	queueStatus := nextStatus
	if nextStatus == model.StatusRepairPending {
		repairScheduled, postRepairStatus := h.handleVerifyRepairRegistration(input.sourceTask, params, reason)
		finalStatus = postRepairStatus
		if repairScheduled {
			// The retry task is now the active queue item. Keep the original task
			// terminal in queue history so command-level queue scans do not stall
			// on a lifecycle-only repair_pending marker.
			queueStatus = model.StatusCancelled
		} else {
			// No repair producer exists; the command state carries the
			// paused_for_replan signal while the queue history records a terminal
			// failed result for publish/merge gating.
			queueStatus = model.StatusFailed
		}
	}
	h.syncQueueStatusAfterVerify(params, queueStatus)

	h.maybeAutoRecoverAfterResolution(params, model.StatusCompleted, finalStatus,
		input.taskRunOnIntegration)
	h.dispatchAdvisoryReview(params, finalStatus)
	if h.triggerScan != nil {
		h.triggerScan(ctx)
	}
	return finalStatus
}

func (h *ResultWriteAPI) setAgentIdle(agentID string) {
	if h.statusSink == nil {
		h.statusSink = tmuxAgentStatusSink{}
	}
	h.statusSink.SetIdle(agentID, h.logFn)
}

type resultWriteError struct {
	Code    string
	Message string
}

func (e *resultWriteError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type resultWriteWrappedError struct {
	*resultWriteError
	err error
}

func (e *resultWriteWrappedError) Unwrap() []error {
	return []error{e.resultWriteError, e.err}
}

// resultWriteFencingError extends resultWriteError with structured fencing
// context for F-019. handleValidatedResultWrite branches on this type via
// errors.As to attach the Details payload to the UDS error response so the
// CLI / Worker can read machine-readable fields without grepping Message.
//
// resultWriteError is embedded by pointer so existing `errors.As(err, &rErr)`
// call sites that target *resultWriteError continue to work unchanged.
type resultWriteFencingError struct {
	*resultWriteError
	Details uds.FencingDetails
}

// newFencingError is a small convenience to keep the fencing call sites
// readable. The double allocation (outer + inner) is negligible compared to
// the I/O the rejecting path is about to skip.
func newFencingError(code, message string, details uds.FencingDetails) *resultWriteFencingError {
	return &resultWriteFencingError{
		resultWriteError: &resultWriteError{Code: code, Message: message},
		Details:          details,
	}
}

// handleRetryRegistration registers a retry task in state and queue if phaseA
// determined one is needed. TaskRetryHandler does not hold state and queue
// locks at the same time, so it must not be moved under Phase A's queue/result
// locks.
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

func (h *ResultWriteAPI) handleVerifyRepairRegistration(sourceTask *model.Task, params ResultWriteParams, reason string) (bool, model.Status) {
	if sourceTask == nil {
		replanReason := fmt.Sprintf("verify_repair_source_task_missing: %s", reason)
		h.logFn(LogLevelError,
			"verify_repair_not_scheduled task=%s command=%s reason=%q source_task_missing=true -> paused_for_replan",
			params.TaskID, params.CommandID, reason)
		h.advanceRepairPendingToPausedForReplan(params, replanReason)
		return false, model.StatusPausedForReplan
	}

	retryHandler := NewTaskRetryHandler(h.maestroDir, *h.config, h.lockMap, h.logger, h.logLevel)
	shouldRepair, skipReason := retryHandler.ShouldRepairTask(sourceTask, reason)
	if !shouldRepair {
		h.logFn(LogLevelInfo, "verify_repair_skipped task=%s command=%s reason=%s",
			params.TaskID, params.CommandID, skipReason)
		h.advanceRepairPendingToPausedForReplan(params, skipReason)
		return false, model.StatusPausedForReplan
	}

	repairTask, err := retryHandler.CreateVerifyRepairTask(sourceTask, reason)
	if err != nil {
		h.logFn(LogLevelError,
			"verify_repair_create_failed task=%s command=%s error=%v -> paused_for_replan",
			params.TaskID, params.CommandID, err)
		h.advanceRepairPendingToPausedForReplan(params,
			fmt.Sprintf("verify_repair_create_failed: %v", err))
		return false, model.StatusPausedForReplan
	}
	if err := retryHandler.RetryTaskAtomically(repairTask, params.CommandID, params.Reporter); err != nil {
		h.logFn(LogLevelError,
			"verify_repair_atomic_failed task=%s repair_id=%s worker=%s command=%s error=%v -> paused_for_replan",
			params.TaskID, repairTask.ID, params.Reporter, params.CommandID, err)
		h.advanceRepairPendingToPausedForReplan(params,
			fmt.Sprintf("verify_repair_enqueue_failed: %v", err))
		return false, model.StatusPausedForReplan
	}
	h.logFn(LogLevelInfo,
		"verify_repair_scheduled task=%s repair_id=%s command=%s reason=%q",
		params.TaskID, repairTask.ID, params.CommandID, reason)
	return true, model.StatusRepairPending
}

func (h *ResultWriteAPI) syncQueueStatusAfterVerify(params ResultWriteParams, nextStatus model.Status) {
	h.lockMap.Lock("queue:" + params.Reporter)
	defer h.lockMap.Unlock("queue:" + params.Reporter)

	tq, err := h.fileStore.LoadQueueFile(params.Reporter)
	if err != nil {
		h.logFn(LogLevelWarn,
			"verify_queue_status_sync_load_failed task=%s worker=%s status=%s error=%v",
			params.TaskID, params.Reporter, nextStatus, err)
		return
	}
	for i := range tq.Tasks {
		if tq.Tasks[i].ID != params.TaskID {
			continue
		}
		if tq.Tasks[i].Status == nextStatus {
			return
		}
		tq.Tasks[i].Status = nextStatus
		tq.Tasks[i].UpdatedAt = h.clock.Now().UTC().Format(time.RFC3339)
		if err := h.fileStore.SaveQueueFile(params.Reporter, tq); err != nil {
			h.logFn(LogLevelWarn,
				"verify_queue_status_sync_save_failed task=%s worker=%s status=%s error=%v",
				params.TaskID, params.Reporter, nextStatus, err)
			return
		}
		h.recordSelfWrite(h.fileStore.QueueFilePath(params.Reporter), tq)
		return
	}
	h.logFn(LogLevelWarn,
		"verify_queue_status_sync_task_missing task=%s worker=%s status=%s",
		params.TaskID, params.Reporter, nextStatus)
}

func (h *ResultWriteAPI) advanceRepairPendingToPausedForReplan(params ResultWriteParams, reason string) {
	// Lock ordering: state and queue locks are never held simultaneously.
	// We update state under the state lock, release it, then acquire the
	// queue:planner_signals lock to emit the signal. This preserves the
	// canonical queue→state→result lock ordering documented in
	// internal/daemon/doc.go.
	cmdLockKey := "state:" + params.CommandID
	h.lockMap.Lock(cmdLockKey)
	statePath := commandStatePath(h.maestroDir, params.CommandID)
	updateErr := updateYAMLFile(statePath, func(state *model.CommandState) error {
		if state.TaskStates == nil || state.TaskStates[params.TaskID] != model.StatusRepairPending {
			return errNoUpdate
		}
		if err := model.AdvanceTaskState(state.TaskStates, params.TaskID, model.StatusPausedForReplan); err != nil {
			return err
		}
		state.UpdatedAt = h.clock.Now().UTC().Format(time.RFC3339)
		return nil
	})
	h.lockMap.Unlock(cmdLockKey)
	if updateErr != nil {
		h.logFn(LogLevelWarn,
			"verify_repair_replan_signal_failed task=%s command=%s reason=%q error=%v",
			params.TaskID, params.CommandID, reason, updateErr)
		return
	}
	h.logFn(LogLevelWarn,
		"verify_repair_replan_required task=%s command=%s reason=%q",
		params.TaskID, params.CommandID, reason)
	h.emitPausedForReplanPlannerSignal(params, reason)
}

func (h *ResultWriteAPI) emitPausedForReplanPlannerSignal(params ResultWriteParams, reason string) {
	now := h.clock.Now().UTC().Format(time.RFC3339)
	phaseID := "__task_" + params.TaskID
	msg := fmt.Sprintf("[maestro] kind:paused_for_replan command_id:%s task_id:%s\nreason: %s\nnext_action: add_retry_task or fail the command",
		params.CommandID, params.TaskID, reason)
	sig := model.PlannerSignal{
		Kind:      "paused_for_replan",
		CommandID: params.CommandID,
		PhaseID:   phaseID,
		Message:   msg,
		Reason:    reason,
		CreatedAt: now,
		UpdatedAt: now,
	}

	h.lockMap.Lock("queue:planner_signals")
	defer h.lockMap.Unlock("queue:planner_signals")
	if err := updateYAMLFile(signalQueuePath(h.maestroDir), func(sq *model.PlannerSignalQueue) error {
		index := buildSignalIndex(sq.Signals)
		key := signalDedupKey(sig)
		if _, exists := index[key]; exists {
			return errNoUpdate
		}
		if sq.SchemaVersion == 0 {
			sq.SchemaVersion = 1
			sq.FileType = "planner_signal_queue"
		}
		sq.Signals = append(sq.Signals, sig)
		return nil
	}); err != nil {
		h.logFn(LogLevelWarn,
			"paused_for_replan_signal_write_failed task=%s command=%s reason=%q error=%v",
			params.TaskID, params.CommandID, reason, err)
		return
	}
	h.logFn(LogLevelInfo,
		"paused_for_replan_signal_queued task=%s command=%s reason=%q",
		params.TaskID, params.CommandID, reason)
}

// truncateRunes truncates a string to at most maxRunes runes.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}
