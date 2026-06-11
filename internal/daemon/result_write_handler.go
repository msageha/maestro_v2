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

// circuitBreakerUpdater updates circuit breaker counters on result.
type circuitBreakerUpdater interface {
	UpdateCounterOnResult(state *model.CommandState, resultStatus model.Status, taskID string, resultID string, now time.Time) (bool, string)
	TripBreaker(state *model.CommandState, reason string, now time.Time)
}

// reviewDispatcher dispatches review requests for completed tasks.
type reviewDispatcher interface {
	Enabled() bool
	DispatchIfEligible(ctx context.Context, params ResultWriteParams)
	PrecaptureDiff(taskID, commandID, reporter string)
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

// SetAdmissionController wires the admission controller used to gate
// daemon-owned background verification work. Late-bound from daemon
// startup because the controller is constructed after the API surface.
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
			// ErrNoWorktreeState is the expected outcome for any task that
			// completes after the worktree pipeline has already torn down
			// (success path: the post-publish cleanup deletes the command's
			// worktree state file before the run_on_main verification task
			// reports). There is nothing left to recover, so swallow it
			// quietly at debug — emitting at warn here drowned operators in
			// false-positive "auto_recover_after_resolution_failed" lines for
			// every successful publish. Other errors remain a real recovery
			// failure (e.g. ResumeMerge git op error) and stay at warn so
			// they are still surfaced.
			if errors.Is(err, worktree.ErrNoWorktreeState) {
				h.logFn(LogLevelDebug,
					"auto_recover_after_resolution_no_state command=%s reporter=%s task=%s "+
						"(worktree state already cleaned up; nothing to recover)",
					params.CommandID, params.Reporter, params.TaskID)
				return
			}
			// AutoRecoverAfterResolution swallows ErrAlreadyResolved and
			// returns AutoRecoverNone; anything else here is a real
			// recovery failure. Log loud enough that operators see it, but
			// do not fail the result_write — R7 (and the next AutoRecover)
			// can still retry.
			h.logFn(LogLevelWarn,
				"auto_recover_after_resolution_failed command=%s reporter=%s task=%s action=%s error=%v "+
					"(result already committed; reconcile/scan will retry)",
				params.CommandID, params.Reporter, params.TaskID, action, err)
			return
		}
		if action != worktree.AutoRecoverNone {
			// Distinguish "ResumeMerge ran and the reporter worker is no
			// longer Resolving" (key=_completed) from "ResumeMerge ran
			// but tryMergeWorker deferred the merge until the dispatched
			// resolution task lands its commit" (key=_deferred). The
			// action value encodes which branch ran:
			// AutoRecoverResumeMergeDeferred → deferred.
			key := "auto_recover_after_resolution_completed"
			if action == worktree.AutoRecoverResumeMergeDeferred {
				key = "auto_recover_after_resolution_deferred"
			}
			h.logFn(LogLevelInfo,
				"%s command=%s reporter=%s task=%s action=%s",
				key, params.CommandID, params.Reporter, params.TaskID, action)
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
			// Symmetry with the StatusCompleted branch: a missing worktree
			// state for a failed task means there is no resolving worker to
			// reset, which is the expected post-cleanup shape. Suppress the
			// warn so the failed-task log line is not paired with a noisy
			// false-positive recovery warning.
			if errors.Is(err, worktree.ErrNoWorktreeState) {
				h.logFn(LogLevelDebug,
					"reset_resolving_worker_no_state command=%s worker=%s "+
						"(worktree state already cleaned up; nothing to reset)",
					params.CommandID, params.Reporter)
				return
			}
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

// ResultWriteParams aliases daemonapi.ResultWriteParams so callers
// inside the daemon package can keep their existing import surface
// after the request-decoding logic moved into daemonapi.
type ResultWriteParams = daemonapi.ResultWriteParams

func (h *ResultWriteAPI) handleValidatedResultWrite(params ResultWriteParams, resultStatus model.Status) *uds.Response {
	// Phase A: Shared file lock + per-worker mutex (results/ + queue/ updates)
	resultWritePhaseAResult, err := newResultPhaseAService(h).Run(params, resultStatus)
	if err != nil {
		// Prefer the fencing-typed error first so the structured Details
		// payload is forwarded to the UDS response.
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

	// Capture-before-cleanup snapshot: Phase A has just transitioned the
	// queue task to terminal, which makes the command eligible for the
	// next scan's worktree cleanup. Async verify and the eventual review
	// dispatch run AFTER that, so the worker worktree may have been wiped
	// by the time ReviewCoordinator.buildDiffContent calls
	// ComputeWorkerDiff. Capturing the diff here — while the worker
	// worktree is still alive — pins a stable snapshot that the dispatch
	// path retrieves via popPrecapturedDiff. Only fired for completed
	// results; failed/cancelled results never trigger a review.
	if !isDuplicate && resultStatus == model.StatusCompleted {
		if rc := h.reviewCoord(); rc != nil && rc.Enabled() {
			rc.PrecaptureDiff(params.TaskID, params.CommandID, params.Reporter)
		}
	}

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

// reserveOrDeferHeavyVerify decides whether the verify run for sourceTask
// should run repo-wide categories (test/security/performance) or defer them
// to a sibling. Returns true when the caller must DEFER heavy verify.
//
// The decision uses a CAS-style reservation under the state lock so that at
// most one task per phase ever wins the right to run heavy verify. Without
// the lock-protected reservation, multiple sibling tasks reaching
// verify_pending in parallel would each see the others at verify_pending
// (which is not a terminal status) and all defer — leaving no task to
// actually run repo-wide verification, breaking the §S1-1 Strong Signal.
//
// Decision rules, evaluated under the state lock:
//
//   - If sourceTask is nil → run heavy (no phase context to reason about).
//   - If state load/save fails → run heavy (conservative: preserve Strong
//     Signal at the cost of possible duplicated work).
//   - If sourceTask is not a member of any phase (implicit-phase commands) →
//     run heavy (legacy behaviour).
//   - If a phase-level reservation already exists for an in-phase task and
//     it isn't sourceTask → defer.
//   - If a phase-level reservation exists but the owner is no longer a
//     member of the phase (the original owner was retried out and replaced
//     by a new task) → treat as cleared and re-evaluate.
//   - If any sibling is still active (running / dispatched / repair_pending /
//     pending — anything that is neither terminal nor verify_pending) →
//     defer; the phase isn't ready for repo-wide verification yet.
//   - Otherwise reserve sourceTask as the owner under the lock and run
//     heavy. The reservation persists across verify failures so a retry
//     does not double-charge the phase.
//
// The "verify_pending counts as ready" half of the rule is what unblocks the
// race: when N siblings sit at verify_pending simultaneously, exactly one of
// them grabs the lock first, finds no in-flight work, writes itself into
// HeavyVerifyOwners, and proceeds; subsequent siblings see the reservation
// and defer.
func (h *ResultWriteAPI) reserveOrDeferHeavyVerify(commandID string, sourceTask *model.Task) bool {
	if sourceTask == nil {
		return false
	}

	cmdLockKey := "state:" + commandID
	h.lockMap.Lock(cmdLockKey)
	defer h.lockMap.Unlock(cmdLockKey)

	statePath := commandStatePath(h.maestroDir, commandID)
	var deferHeavy bool
	updateErr := updateYAMLFile(statePath, func(state *model.CommandState) error {
		var phase *model.Phase
		for i := range state.Phases {
			for _, tid := range state.Phases[i].TaskIDs {
				if tid == sourceTask.ID {
					phase = &state.Phases[i]
					break
				}
			}
			if phase != nil {
				break
			}
		}
		if phase == nil {
			deferHeavy = false
			return errNoUpdate
		}

		ownerInPhase := func(owner string) bool {
			for _, tid := range phase.TaskIDs {
				if tid == owner {
					return true
				}
			}
			return false
		}

		// Check existing reservation. A reservation is "live" only while
		// the owner is actually in a state that exercises heavy verify:
		//   - verify_pending: owner is currently running heavy
		//   - completed: owner has already passed heavy successfully
		// In any other state — repair_pending after a heavy failure,
		// cancelled because a retry superseded the owner, failed/dead_letter/
		// aborted, etc. — the heavy verification did NOT pass for this
		// phase. The reservation is stale and must be cleared so a fresh
		// owner (typically the retry that replaced the failed one) can
		// reclaim ownership and run heavy verify. Without this clearance,
		// a failed owner would sit in phase.TaskIDs forever, retries
		// would always defer, repo-wide verify would never re-run, and a
		// half-fixed phase could merge silently.
		mutated := false
		if owner, ok := state.HeavyVerifyOwners[phase.PhaseID]; ok {
			ownerLive := false
			if ownerInPhase(owner) {
				switch state.TaskStates[owner] {
				case model.StatusVerifyPending, model.StatusCompleted:
					ownerLive = true
				}
			}
			if ownerLive {
				deferHeavy = owner != sourceTask.ID
				return errNoUpdate
			}
			delete(state.HeavyVerifyOwners, phase.PhaseID)
			mutated = true
		}

		// No live reservation. Phase is "ready for heavy verify" only
		// when every sibling has reached either a terminal status or
		// verify_pending (which signals "I'm done writing files; verify
		// is scheduled for me too"). Anything still actively running
		// means a later sibling will reach verify_pending and become a
		// more authoritative phase-final candidate.
		for _, tid := range phase.TaskIDs {
			if tid == sourceTask.ID {
				continue
			}
			s, ok := state.TaskStates[tid]
			if !ok {
				deferHeavy = false
				if mutated {
					return nil
				}
				return errNoUpdate
			}
			if !model.IsTerminal(s) && s != model.StatusVerifyPending {
				deferHeavy = true
				if mutated {
					return nil
				}
				return errNoUpdate
			}
		}

		if state.HeavyVerifyOwners == nil {
			state.HeavyVerifyOwners = make(map[string]string)
		}
		state.HeavyVerifyOwners[phase.PhaseID] = sourceTask.ID
		deferHeavy = false
		return nil
	})
	if updateErr != nil {
		h.logFn(LogLevelWarn,
			"heavy_verify_reservation_failed command=%s task=%s error=%v "+
				"(running full verify; may duplicate work but preserves Strong Signal)",
			commandID, sourceTask.ID, updateErr)
		return false
	}
	return deferHeavy
}

// computeWorkerExpectedPathsForVerify returns the union of expected_paths for
// the reporter worker that the §S1-1 Verification Runner should treat as the
// allowed write surface. Phase B's commit_policy uses the same worker-level
// union; using a single task's expected_paths here would falsely flag earlier
// sibling-task dirty files when the worker holds multiple tasks under one
// command.
//
// The union covers:
//   - the source task being verified, whose ExpectedPaths must be admitted
//     even though its queue status is still verify_pending at this point;
//   - same-phase sibling tasks at StatusCompleted on the reporter worker.
//     Within a phase, completed siblings still have *uncommitted* changes
//     in the worker worktree because Phase B's auto-commit runs only after
//     the whole phase reaches a terminal state. Verification therefore must
//     admit those paths.
//
// **Scope**: the union is intentionally restricted to tasks in the
// *same* phase as sourceTask. Earlier-phase tasks have already been
// auto-committed at their phase's merge boundary and the worktree is
// fast-forwarded to integration HEAD before the next phase's tasks
// dispatch (see internal/daemon/dispatch/dispatcher.go), so their
// ExpectedPaths must not silently widen the verify surface for the
// current phase.
//
// Falls back to the legacy "every completed task in the command" union when
// no phase context is available (implicit-phase commands or queue load
// failure), so a transient I/O hiccup does not silently widen the surface
// beyond the legacy behaviour.
func (h *ResultWriteAPI) computeWorkerExpectedPathsForVerify(commandID, workerID string, sourceTask *model.Task) []string {
	addPaths := func(seen map[string]struct{}, dst []string, paths []string) []string {
		for _, p := range paths {
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			dst = append(dst, p)
		}
		return dst
	}

	seen := make(map[string]struct{})
	var union []string
	if sourceTask != nil {
		union = addPaths(seen, union, sourceTask.ExpectedPaths)
	}

	// Build the same-phase task ID set. If we cannot identify the phase we
	// fall back to legacy "every completed task in the command" behaviour
	// so this fix never tightens further than the previous implementation
	// in the absence of a phase context.
	var samePhaseTaskIDs map[string]bool
	if sourceTask != nil {
		if state, err := h.fileStore.LoadCommandState(commandID); err == nil {
			for i := range state.Phases {
				for _, tid := range state.Phases[i].TaskIDs {
					if tid == sourceTask.ID {
						samePhaseTaskIDs = make(map[string]bool, len(state.Phases[i].TaskIDs))
						for _, sid := range state.Phases[i].TaskIDs {
							samePhaseTaskIDs[sid] = true
						}
						break
					}
				}
				if samePhaseTaskIDs != nil {
					break
				}
			}
		} else {
			h.logFn(LogLevelDebug,
				"verify_expected_paths_state_load_failed command=%s task=%s error=%v "+
					"(falling back to command-wide union)",
				commandID, sourceTask.ID, err)
		}
	}

	tq, err := h.fileStore.LoadQueueFile(workerID)
	if err != nil {
		h.logFn(LogLevelDebug,
			"verify_expected_paths_queue_load_failed worker=%s command=%s error=%v "+
				"(falling back to source task expected_paths)",
			workerID, commandID, err)
		return union
	}
	for i := range tq.Tasks {
		t := &tq.Tasks[i]
		if t.CommandID != commandID {
			continue
		}
		if t.Status != model.StatusCompleted {
			continue
		}
		// Phase scope: only include same-phase siblings whose changes are
		// still uncommitted (auto-commit runs at phase boundary).
		if samePhaseTaskIDs != nil && !samePhaseTaskIDs[t.ID] {
			continue
		}
		union = addPaths(seen, union, t.ExpectedPaths)
	}
	return union
}

// heavyVerifyFilterRunner is implemented by VerifyRunners that can defer
// repo-wide categories (test/security/performance) when the source task is
// intermediate within its phase. Production *RealVerifyRunner satisfies it;
// test/skip/unconfigured runners do not, and runVerifySecondPass falls back
// to plain Run for those.
type heavyVerifyFilterRunner interface {
	VerifyRunner
	RunSkippingHeavyCategories(ctx context.Context, taskID, commandID, workingDir string, expectedPaths []string) (VerifyOutcome, error)
}

func (h *ResultWriteAPI) runVerifySecondPass(ctx context.Context, input resultVerifyInput) model.Status {
	params := input.params
	workingDir, wdErr := h.resolveVerifyWorkingDir(params)
	runner := h.resolveVerifyRunner()
	expectedPaths := h.computeWorkerExpectedPathsForVerify(params.CommandID, params.Reporter, input.sourceTask)
	skipHeavy := h.reserveOrDeferHeavyVerify(params.CommandID, input.sourceTask)
	var outcome VerifyOutcome
	var vErr error
	if wdErr != nil {
		h.logFn(LogLevelWarn, "verify_workdir_resolution_failed task=%s command=%s error=%v", params.TaskID, params.CommandID, wdErr)
		outcome = VerifyOutcome{Passed: false, Reason: wdErr.Error()}
	} else if skipHeavy {
		if hr, ok := runner.(heavyVerifyFilterRunner); ok {
			h.logFn(LogLevelInfo,
				"verify_intermediate_task_skipping_heavy task=%s command=%s "+
					"(repo-wide test/security/performance deferred to phase-final task)",
				params.TaskID, params.CommandID)
			outcome, vErr = hr.RunSkippingHeavyCategories(ctx, params.TaskID, params.CommandID, workingDir, expectedPaths)
		} else {
			outcome, vErr = runner.Run(ctx, params.TaskID, params.CommandID, workingDir, expectedPaths)
		}
	} else {
		outcome, vErr = runner.Run(ctx, params.TaskID, params.CommandID, workingDir, expectedPaths)
	}
	nextStatus, reason := classifyVerifyOutcome(outcome, vErr)
	applied, applyErr := h.applyVerifyOutcome(params, nextStatus, reason)
	if applyErr != nil {
		h.logFn(LogLevelWarn,
			"verify_outcome_apply_failed task=%s command=%s next=%s reason=%q error=%v "+
				"(task remains at verify_pending; reconcile/operator can re-drive)",
			params.TaskID, params.CommandID, nextStatus, reason, applyErr)
		return model.StatusVerifyPending
	}
	if !applied {
		// The task left verify_pending while verify ran (cancel, R9
		// takeover, operator override). The outcome is stale — skip every
		// dependent side effect: no applied stamp, no repair registration,
		// no queue status sync (which would overwrite the newer owner's
		// terminal status, e.g. cancelled→completed), no auto-recover.
		h.logFn(LogLevelInfo,
			"verify_outcome_discarded task=%s command=%s next=%s reason=%q "+
				"(task no longer at verify_pending; side effects skipped)",
			params.TaskID, params.CommandID, nextStatus, reason)
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
			// notifyPlannerOfWorkerResult already delivered the worker-reported
			// status (commonly "completed"). Verify has now overruled it: a
			// retry has been scheduled. Surface the divergence with a dedicated
			// supplementary signal so Planner does not treat the task as done.
			// paused_for_replan path emits its own signal below — skip this one
			// there to avoid double-notification.
			if params.Status == string(model.StatusCompleted) {
				// emitVerifyOutcomeChangedPlannerSignal expects callers to
				// hold scanMu.RLock; handleVerifyRepairRegistration above has
				// already returned and released its lock, so we re-acquire
				// for this single signal write.
				h.acquireFileLock()
				h.emitVerifyOutcomeChangedPlannerSignal(params, reason, finalStatus)
				h.releaseFileLock()
			}
		} else {
			// No repair producer exists; the command state carries the
			// paused_for_replan signal while the queue history records a terminal
			// failed result for publish/merge gating.
			queueStatus = model.StatusFailed
		}
	}
	// Dispatch the advisory review BEFORE syncQueueStatusAfterVerify marks
	// the queue task terminal. Once the queue task lands at
	// completed/failed, the next queue scan may schedule a worktree
	// cleanup that wipes the worker worktree directory; the review
	// would then compute an empty diff and record status=skipped.
	// Capturing while the task is still parked at verify_pending keeps
	// the worktree gated against cleanup so the diff is real.
	h.dispatchAdvisoryReview(params, finalStatus)

	h.syncQueueStatusAfterVerify(params, queueStatus)

	h.maybeAutoRecoverAfterResolution(params, model.StatusCompleted, finalStatus,
		input.taskRunOnIntegration)
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
// context. handleValidatedResultWrite branches on this type via errors.As
// to attach the Details payload to the UDS error response so the CLI /
// Worker can read machine-readable fields without grepping Message.
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
//
// scanMu serialization: RetryTaskAtomically writes the worker queue file
// via lockMap("queue:{worker}") only. PeriodicScan Phase A loads worker
// queues into an in-memory snapshot, performs scan mutations, and
// flushes the snapshot back under the same lockMap key. Without
// scanMu.RLock here, Phase A's flush can race the retry-add and
// overwrite the file with its pre-retry snapshot — leaving a state-side
// retry task with zero queue presence which the phantom_task cleanup
// then loops on. Holding scanMu.RLock for the whole RetryTaskAtomically
// call matches the canonical "queue writes hold scanMu.RLock + lockMap"
// invariant documented in doc.go.
func (h *ResultWriteAPI) handleRetryRegistration(phaseAResult *resultWritePhaseAResult, params ResultWriteParams) {
	if phaseAResult.retryTask == nil {
		return
	}

	retryTask := phaseAResult.retryTask
	retryHandler := NewTaskRetryHandler(h.maestroDir, *h.config, h.lockMap, h.logger, h.logLevel)

	h.acquireFileLock()
	defer h.releaseFileLock()

	if err := retryHandler.RetryTaskAtomically(retryTask, params.TaskID, params.CommandID, params.Reporter); err != nil {
		h.logFn(LogLevelError, "retry_task_atomic_failed task=%s worker=%s command=%s error=%v",
			retryTask.ID, params.Reporter, params.CommandID, err)
	} else {
		h.logFn(LogLevelInfo, "task_retry_scheduled task=%s retry_id=%s attempt=%d",
			params.TaskID, retryTask.ID, retryTask.Attempts)
	}
}

func (h *ResultWriteAPI) handleVerifyRepairRegistration(sourceTask *model.Task, params ResultWriteParams, reason string) (bool, model.Status) {
	// Hold scanMu.RLock for the full repair-registration sequence so the
	// RetryTaskAtomically queue write and the advance/cancel state writes
	// below run atomically against PeriodicScan Phase A's queue flush. See
	// handleRetryRegistration for the race details — the verify-side path
	// is the same plus advanceRepairPendingToPausedForReplan and
	// cancelSupersededPredecessor, so the lock has to bracket all of them.
	h.acquireFileLock()
	defer h.releaseFileLock()

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
	if err := retryHandler.RetryTaskAtomically(repairTask, sourceTask.ID, params.CommandID, params.Reporter); err != nil {
		h.logFn(LogLevelError,
			"verify_repair_atomic_failed task=%s repair_id=%s worker=%s command=%s error=%v -> paused_for_replan",
			params.TaskID, repairTask.ID, params.Reporter, params.CommandID, err)
		h.advanceRepairPendingToPausedForReplan(params,
			fmt.Sprintf("verify_repair_enqueue_failed: %v", err))
		return false, model.StatusPausedForReplan
	}
	// Predecessor cleanup: the verify repair path leaves the original
	// task at StatusRepairPending in state. That status is non-terminal,
	// so plan/state.go:CanComplete rejects any subsequent plan_complete
	// with "phase X is terminal but task Y is non-terminal
	// (repair_pending)". Now that the repair successor has been
	// successfully enqueued, the original task is by construction
	// superseded. Eagerly transitioning it to StatusCancelled with a
	// descriptive reason unblocks plan_complete and gives operators an
	// audit trail (the cancellation reason links to the successor ID).
	supersededReason := fmt.Sprintf(
		"superseded_by_verify_repair: repair_task=%s reason=%s",
		repairTask.ID, reason,
	)
	if cancelErr := h.cancelSupersededPredecessor(params.CommandID, sourceTask.ID, supersededReason); cancelErr != nil {
		h.logFn(LogLevelWarn,
			"verify_repair_predecessor_cancel_failed task=%s command=%s repair_id=%s error=%v "+
				"(R4PlanStatus backoff will retry plan_complete; not fatal)",
			sourceTask.ID, params.CommandID, repairTask.ID, cancelErr)
	}
	h.logFn(LogLevelInfo,
		"verify_repair_scheduled task=%s repair_id=%s command=%s reason=%q",
		params.TaskID, repairTask.ID, params.CommandID, reason)
	return true, model.StatusRepairPending
}

// cancelSupersededPredecessor flips a task whose successor (retry / repair) has
// just been enqueued into StatusCancelled inside the command-state file. This
// breaks the plan-complete validation gridlock where phase=terminal but the
// predecessor task lingers at StatusRepairPending or StatusPausedForReplan.
//
// Idempotent: if the predecessor is already terminal, the YAML update is a no-op.
// Best-effort: failures are reported back to the caller (logged at warn) so that
// the legacy R4PlanStatus backoff path still gets a chance to recover.
func (h *ResultWriteAPI) cancelSupersededPredecessor(commandID, predecessorID, reason string) error {
	cmdLockKey := "state:" + commandID
	h.lockMap.Lock(cmdLockKey)
	defer h.lockMap.Unlock(cmdLockKey)
	statePath := commandStatePath(h.maestroDir, commandID)
	return updateYAMLFile(statePath, func(state *model.CommandState) error {
		if state.TaskStates == nil {
			return errNoUpdate
		}
		current, ok := state.TaskStates[predecessorID]
		if !ok {
			return errNoUpdate
		}
		if model.IsTerminal(current) {
			return errNoUpdate
		}
		if err := model.AdvanceTaskState(state.TaskStates, predecessorID, model.StatusCancelled); err != nil {
			return err
		}
		if state.CancelledReasons == nil {
			state.CancelledReasons = make(map[string]string)
		}
		state.CancelledReasons[predecessorID] = reason
		state.UpdatedAt = h.clock.Now().UTC().Format(time.RFC3339)
		return nil
	})
}

func (h *ResultWriteAPI) syncQueueStatusAfterVerify(params ResultWriteParams, nextStatus model.Status) {
	// scanMu.RLock — same rationale as applyVerifyOutcome: this queue write
	// happens after async verify finishes and would otherwise race
	// PeriodicScan Phase A's queue flush. Holding RLock + queue:{reporter}
	// keeps the daemon-wide invariant that every queue write is observed by
	// every scan cycle as either "before" or "after" — never "vanished".
	h.acquireFileLock()
	defer h.releaseFileLock()

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

// emitVerifyOutcomeChangedPlannerSignal notifies the Planner that the
// post-verify lifecycle disagreed with the worker's self-reported result.
// notifyPlannerOfWorkerResult delivers the *worker-reported* status (typically
// "completed") via the per-result task_result notification path, but does not
// re-fire when verify subsequently routes the task to repair_pending. Without
// this supplementary signal, the Planner would treat the task as finished
// while the daemon has actually scheduled a retry, and could proceed to the
// next phase while a hidden repair task kept the publish gate blocked.
//
// The dedicated paused_for_replan signal already covers the
// repair_pending → paused_for_replan branch (max_repair / non-retryable). This
// signal is intentionally limited to the repair_pending branch (a retry task
// has been scheduled but the worker still believes its task is done) so the
// Planner sees both signals only when both events apply.
func (h *ResultWriteAPI) emitVerifyOutcomeChangedPlannerSignal(params ResultWriteParams, reason string, finalStatus model.Status) {
	now := h.clock.Now().UTC().Format(time.RFC3339)
	phaseID := "__task_" + params.TaskID
	msg := fmt.Sprintf("[maestro] kind:verify_outcome_changed command_id:%s task_id:%s\nworker_reported: %s\nverify_final_status: %s\nreason: %s\nnext_action: track the scheduled retry task; the worker-reported task_result is stale",
		params.CommandID, params.TaskID, params.Status, finalStatus, reason)
	sig := model.PlannerSignal{
		Kind:      "verify_outcome_changed",
		CommandID: params.CommandID,
		PhaseID:   phaseID,
		Message:   msg,
		Reason:    reason,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Caller is responsible for holding scanMu.RLock — see emitPausedForReplanPlannerSignal
	// for the same caller-bracket convention. Function-level RLock is rejected
	// here because some callers reach this helper from inside an outer
	// scanMu.RLock (handleVerifyRepairRegistration), and Go's sync.RWMutex
	// can deadlock on recursive RLock when a writer arrives between the two
	// acquires.
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
			"verify_outcome_changed_signal_write_failed task=%s command=%s reason=%q error=%v",
			params.TaskID, params.CommandID, reason, err)
		return
	}
	h.logFn(LogLevelInfo,
		"verify_outcome_changed_signal_queued task=%s command=%s worker_reported=%s final=%s reason=%q",
		params.TaskID, params.CommandID, params.Status, finalStatus, reason)
}

// truncateRunes truncates a string to at most maxRunes runes.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}
