package daemon

import (
	"fmt"
	"os"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

func (h *ResultWriteAPI) dispatchAdvisoryReview(params ResultWriteParams, finalStatus model.Status) {
	if finalStatus != model.StatusCompleted {
		return
	}
	if rc := h.reviewCoord(); rc != nil && rc.Enabled() {
		rc.DispatchIfEligible(h.ctx(), params)
	}
}

func (h *ResultWriteAPI) resultWritePhaseB(params ResultWriteParams, resultID string, resultStatus model.Status, queueWriteFailed bool, originalTaskID string, retryScheduled, abortByMaxRepair bool) (bool, model.Status, error) {
	cmdLockKey := "state:" + params.CommandID
	h.lockMap.Lock(cmdLockKey)
	defer h.lockMap.Unlock(cmdLockKey)

	statePath := commandStatePath(h.maestroDir, params.CommandID)

	var needsVerify bool
	finalStateStatus := resultStatus
	err := updateYAMLFile(statePath, func(state *model.CommandState) error {
		if state.CommandID == "" && state.SchemaVersion == 0 && state.TaskStates == nil {
			return fmt.Errorf("state not found for command %s", params.CommandID)
		}
		if state.TaskStates == nil {
			state.TaskStates = make(map[string]model.Status)
		}

		// Idempotency check under state lock — closes the TOCTOU window between
		// Phase A's result-file check and Phase B's state update.
		if state.AppliedResultIDs != nil {
			if existingID, ok := state.AppliedResultIDs[params.TaskID]; ok {
				if existingID == resultID {
					h.logFn(LogLevelWarn, "duplicate_result_skipped task=%s result=%s command=%s (state lock idempotency)",
						params.TaskID, resultID, params.CommandID)
					return errNoUpdate
				}
				h.logFn(LogLevelWarn,
					"result_write applied_result_ids_conflict task=%s command=%s existing=%s incoming=%s "+
						"(TOCTOU race detected; incoming result rejected to preserve idempotency)",
					params.TaskID, params.CommandID, existingID, resultID)
				return errNoUpdate
			}
		}

		recordedStatus := h.resolveRecordedStatus(state, params, resultStatus)

		// Audit hook: log when the worker's reported terminal status is not a
		// direct §2.1 edge from the existing TaskStates entry. The progression
		// pipeline below routes the task through the lifecycle anyway (so
		// result/state stay in sync), but a planner or worker that lands at
		// an unexpected source state (e.g., pending → completed) is a §2.1
		// violation worth flagging.
		if existing, hadExisting := state.TaskStates[params.TaskID]; hadExisting && existing != recordedStatus {
			if err := validateReportedResultTransition(existing, recordedStatus); err != nil {
				h.logFn(LogLevelError,
					"result_write invalid_state_transition task=%s command=%s from=%s to=%s reason=%v "+
						"(applying via §2.1 progression; investigate planner/worker for state machine violation)",
					params.TaskID, params.CommandID, existing, recordedStatus, err)
			}
		}

		// Lifecycle progression. The recorded worker
		// status is mapped to a sequence of intermediate states so that
		// completed/failed/paused_for_replan are reached only via the
		// verify_pending → repair_pending pipeline that §2.1 mandates. The
		// VerifyRunner step is deferred to a second pass under a fresh state
		// lock (see applyVerifyOutcome) so the runner does not block the
		// state lock.
		needsVerify = h.applyTaskStateProgression(state, params, recordedStatus, retryScheduled, abortByMaxRepair)
		finalStateStatus = state.TaskStates[params.TaskID]

		now := h.clock.Now()
		state.UpdatedAt = now.UTC().Format(time.RFC3339)

		h.updateCircuitBreaker(state, params, resultStatus, resultID, now)

		if state.AppliedResultIDs == nil {
			state.AppliedResultIDs = make(map[string]string)
		}
		state.AppliedResultIDs[params.TaskID] = resultID

		h.recordQueueWriteFailedSticky(state, params, resultID, queueWriteFailed)

		// Update original task state for retry lineage consistency.
		if originalTaskID != "" {
			if existing, ok := state.TaskStates[originalTaskID]; ok {
				if !model.IsTerminal(existing) {
					// §2.1: Cancellation is allowed from every non-terminal
					// state, so this should always validate. Log on the off
					// chance the state machine drifts out from under us — same
					// rationale as the main path: never block the in-flight
					// retry result write.
					if err := model.ValidateTaskStateTransition(existing, model.StatusCancelled); err != nil {
						h.logFn(LogLevelError,
							"retry_lineage_invalid_transition original_task=%s retry_task=%s command=%s from=%s to=cancelled reason=%v "+
								"(applying anyway)",
							originalTaskID, params.TaskID, params.CommandID, existing, err)
					}
					state.TaskStates[originalTaskID] = model.StatusCancelled
					h.logFn(LogLevelInfo,
						"retry_lineage_superseded original_task=%s retry_task=%s command=%s",
						originalTaskID, params.TaskID, params.CommandID)
				}
			}
		}

		return nil
	})
	return needsVerify, finalStateStatus, err
}

func validateReportedResultTransition(existing, recorded model.Status) error {
	// A worker that has been handed an in-progress queue entry can land at
	// any of the dispatch-pipeline states (planned / ready / dispatched /
	// running) depending on how far the daemon's lifecycle hops have
	// caught up when it reports completion. Retry tasks specifically
	// enter the lifecycle at `planned` (AddRetryTask) and a fast worker
	// can stream a result back before R0_Dispatch has stamped the queue
	// entry up to `running`. Phase B routes the recorded terminal status
	// through the §2.1 verify_pending → completed/repair pipeline via
	// AdvanceTaskState's BFS, so all of these "still-progressing"
	// sources are expected at the result-write boundary and are NOT a
	// state-machine violation worth waking an operator over (Report
	// 2026-05-06 P1-2 — `planned → completed` ERROR log was noise).
	if recorded == model.StatusCompleted || recorded == model.StatusFailed {
		switch existing {
		case model.StatusPlanned, model.StatusReady, model.StatusDispatched, model.StatusRunning:
			return nil
		}
	}
	return model.ValidateTaskStateTransition(existing, recorded)
}

// applyTaskStateProgression maps the worker-reported recordedStatus to the
// §2.1 task-lifecycle path and applies each step via AdvanceTaskState. The
// `retryScheduled` and `abortByMaxRepair` flags come from Phase A's retry
// evaluation and disambiguate the failed branches:
//
//   - completed              → verify_pending (caller runs VerifyRunner;
//     final status is applied in applyVerifyOutcome).
//   - failed + retry         → cancelled (terminal; superseded_by_retry).
//     A replacement task is already enqueued, so the original is closed
//     immediately rather than parked at repair_pending awaiting an
//     applyVerifyOutcome that may never run when verify is disabled
//     (Report 2026-05-06 bug-1 — verify.enabled=false sites wedged
//     forever because R2 deferred to verify pipeline ownership and
//     the verify pipeline was a no-op).
//   - failed + max_repair    → paused_for_replan (Circuit Breaker
//     triggers replan; the replan path is verify-independent).
//   - failed (non-retryable) → paused_for_replan (no retry producer exists,
//     Planner must replan; verify-independent path).
//   - cancelled              → cancelled (universal transition).
//
// Returns true if the task is parked at verify_pending awaiting the
// VerifyRunner outcome. Idempotent re-submissions whose existing state is
// already terminal are no-ops.
func (h *ResultWriteAPI) applyTaskStateProgression(state *model.CommandState, params ResultWriteParams, recordedStatus model.Status, retryScheduled, abortByMaxRepair bool) bool {
	current, ok := state.TaskStates[params.TaskID]
	if !ok {
		// Defensive: validateStateRegistration in Phase A ensures the task is
		// registered, but we never want to dereference a missing key.
		state.TaskStates[params.TaskID] = recordedStatus
		return false
	}

	// Idempotent re-submission: nothing to advance.
	if model.IsTerminal(current) {
		if current != recordedStatus {
			h.logFn(LogLevelWarn,
				"result_write_advance_skip task=%s command=%s current=%s recorded=%s "+
					"(state already terminal; preserving existing value)",
				params.TaskID, params.CommandID, current, recordedStatus)
		}
		return false
	}

	switch recordedStatus {
	case model.StatusCompleted:
		// Hop to verify_pending and let the second-pass VerifyRunner outcome
		// decide between completed and repair_pending.
		h.advanceOrForce(state, params, model.StatusVerifyPending)
		return true
	case model.StatusFailed:
		switch {
		case retryScheduled:
			// A replacement task is already enqueued; this task is
			// superseded. Mark it terminal-cancelled directly so the
			// publish gate can release immediately. Routing through
			// verify_pending → repair_pending was the previous design,
			// but it relied on applyVerifyOutcome (or R9_VerifyStall)
			// to walk the task to cancelled afterward — both of those
			// are no-ops when verify.enabled=false, so the task sat at
			// repair_pending forever and R2 kept skipping it ("repair
			// pipeline owns this slot"), wedging the whole iteration
			// (Report 2026-05-06 bug-1).
			state.TaskStates[params.TaskID] = model.StatusCancelled
			if state.CancelledReasons == nil {
				state.CancelledReasons = make(map[string]string)
			}
			state.CancelledReasons[params.TaskID] = "superseded_by_retry: replacement task enqueued; original closed"
			h.logFn(LogLevelInfo,
				"result_write_superseded_by_retry task=%s command=%s "+
					"(retry already enqueued; original cancelled directly to avoid repair_pending wedge)",
				params.TaskID, params.CommandID)
		case abortByMaxRepair:
			// Max-repair abort: the Circuit Breaker has decided
			// the task cannot be recovered automatically. Park at
			// paused_for_replan so the Planner can produce a different
			// strategy. The previous detour through verify_pending →
			// repair_pending added no value and would wedge under
			// verify.enabled=false (same root cause as the retry
			// branch above).
			h.advanceOrForce(state, params, model.StatusPausedForReplan)
		default:
			// Non-retryable worker failure with no Circuit Breaker
			// trip: same handling as the max_repair branch — park at
			// paused_for_replan, let Planner replan.
			h.advanceOrForce(state, params, model.StatusPausedForReplan)
		}
		return false
	case model.StatusCancelled:
		// `* → cancelled` is a universal transition; AdvanceTaskState applies it
		// directly. Falling back to a direct write keeps the late-after-plan-
		// terminal coercion path unchanged for the idempotent case as well.
		state.TaskStates[params.TaskID] = model.StatusCancelled
		return false
	default:
		// Unexpected status (Phase A only forwards completed/failed/cancelled);
		// fall back to direct write so we do not silently drop the result.
		state.TaskStates[params.TaskID] = recordedStatus
		return false
	}
}

// advanceOrForce attempts AdvanceTaskState and falls back to a direct write
// on error. Phase A has already committed the result file, so a hard reject
// here would desynchronize result/state; we log at ERROR so the §2.1 state
// machine violation is visible in audit logs without blocking the worker.
func (h *ResultWriteAPI) advanceOrForce(state *model.CommandState, params ResultWriteParams, target model.Status) {
	if err := model.AdvanceTaskState(state.TaskStates, params.TaskID, target); err != nil {
		h.logFn(LogLevelError,
			"result_write_advance_failed task=%s command=%s target=%s reason=%v "+
				"(applying direct write; investigate planner/worker for §2.1 violation)",
			params.TaskID, params.CommandID, target, err)
		state.TaskStates[params.TaskID] = target
	}
}

// applyVerifyOutcome performs the second-pass state update after the
// VerifyRunner has finished. It is invoked outside the Phase B state lock so
// the runner can take as long as it needs without blocking other writers.
//
// The function is a no-op if the task is no longer parked at verify_pending
// (e.g., a concurrent reconcile or operator override moved it elsewhere).
//
// scanMu.RLock is held for the duration of the state write so the
// state/commands/<cmd>.yaml mutation is serialised against PeriodicScan
// Phase A. Without it, Phase A could publish a worktree (queue task
// already terminal) one scan tick after the state still showed
// verify_pending, racing the verify outcome.
func (h *ResultWriteAPI) applyVerifyOutcome(params ResultWriteParams, nextStatus model.Status, reason string) error {
	h.acquireFileLock()
	defer h.releaseFileLock()

	cmdLockKey := "state:" + params.CommandID
	h.lockMap.Lock(cmdLockKey)
	defer h.lockMap.Unlock(cmdLockKey)

	statePath := commandStatePath(h.maestroDir, params.CommandID)
	if err := updateYAMLFile(statePath, func(state *model.CommandState) error {
		if state.TaskStates == nil {
			h.logFn(LogLevelWarn,
				"verify_outcome_skipped task=%s command=%s next=%s (state has no task entries)",
				params.TaskID, params.CommandID, nextStatus)
			return errNoUpdate
		}
		current, ok := state.TaskStates[params.TaskID]
		if !ok {
			h.logFn(LogLevelWarn,
				"verify_outcome_skipped task=%s command=%s next=%s (task missing from state)",
				params.TaskID, params.CommandID, nextStatus)
			return errNoUpdate
		}
		if current != model.StatusVerifyPending {
			h.logFn(LogLevelWarn,
				"verify_outcome_skipped task=%s command=%s current=%s next=%s reason=%q "+
					"(task no longer at verify_pending; outcome ignored)",
				params.TaskID, params.CommandID, current, nextStatus, reason)
			return errNoUpdate
		}
		if err := model.AdvanceTaskState(state.TaskStates, params.TaskID, nextStatus); err != nil {
			return fmt.Errorf("apply verify outcome %s → %s: %w",
				current, nextStatus, err)
		}
		state.UpdatedAt = h.clock.Now().UTC().Format(time.RFC3339)
		h.logFn(LogLevelInfo,
			"verify_outcome_applied task=%s command=%s next=%s reason=%q",
			params.TaskID, params.CommandID, nextStatus, reason)
		return nil
	}); err != nil {
		return err
	}

	// Stamp VerifyOutcomeAppliedAt on the verify-pipeline result entry so
	// the notify gate (taskTerminalForNotify) releases it on the next scan.
	// Lifecycle invariant: every entry that arrives in result_handler with
	// RunOnIntegration / RunOnMain && Status=completed remains gated until
	// this stamp is written, regardless of any race in state file writes.
	h.markVerifyOutcomeAppliedOnResult(params.Reporter, params.TaskID, params.CommandID)
	return nil
}

// markVerifyOutcomeAppliedOnResult stamps VerifyOutcomeAppliedAt on the
// reporter's result file entry for the given task. Best-effort: failure to
// re-write the result file logs a warning but does not roll back the verify
// outcome — the next scan tick will still gate the entry on
// state[taskID]=terminal allowlist as a back-stop, and a missing stamp on a
// terminal-state task simply means the gate falls back to the legacy
// allowlist behaviour for that one entry.
func (h *ResultWriteAPI) markVerifyOutcomeAppliedOnResult(workerID, taskID, commandID string) {
	resultLockKey := "result:" + workerID
	h.lockMap.Lock(resultLockKey)
	defer h.lockMap.Unlock(resultLockKey)

	rf, err := h.fileStore.LoadResultFile(workerID)
	if err != nil {
		h.logFn(LogLevelWarn,
			"verify_outcome_stamp_skip_load worker=%s task=%s command=%s error=%v "+
				"(notify gate will fall back to state[taskID]=terminal allowlist)",
			workerID, taskID, commandID, err)
		return
	}
	now := h.clock.Now().UTC().Format(time.RFC3339)
	mutated := false
	for i := range rf.Results {
		r := &rf.Results[i]
		if r.TaskID != taskID || r.CommandID != commandID {
			continue
		}
		if r.VerifyOutcomeAppliedAt != nil {
			continue
		}
		stamp := now
		r.VerifyOutcomeAppliedAt = &stamp
		mutated = true
	}
	if !mutated {
		return
	}
	if err := h.fileStore.SaveResultFile(workerID, rf); err != nil {
		h.logFn(LogLevelWarn,
			"verify_outcome_stamp_skip_save worker=%s task=%s command=%s error=%v "+
				"(notify gate will fall back to state[taskID]=terminal allowlist)",
			workerID, taskID, commandID, err)
		return
	}
	h.recordSelfWrite(h.fileStore.ResultFilePath(workerID), rf)
	h.logFn(LogLevelDebug,
		"verify_outcome_stamped_result worker=%s task=%s command=%s applied_at=%s",
		workerID, taskID, commandID, now)
}

// resolveRecordedStatus determines the task status to record in the command
// state, handling late-arriving results and idempotent re-submissions.
//
// Late-arriving result handling: if the plan has already failed or cancelled,
// a still-pending task's late result is coerced to cancelled so the plan view
// stays consistent. Two carve-outs:
//  1. plan_status=completed is left alone.
//  2. If TaskStates[taskID] is already terminal, keep it (idempotent re-submission).
//
// The result file (audit trail) and queue entry still record the reporter's
// original status — only the plan's TaskStates view is normalized.
func (h *ResultWriteAPI) resolveRecordedStatus(state *model.CommandState, params ResultWriteParams, resultStatus model.Status) model.Status {
	existing, hadExisting := state.TaskStates[params.TaskID]
	planFailedOrCancelled := state.PlanStatus == model.PlanStatusFailed || state.PlanStatus == model.PlanStatusCancelled
	if planFailedOrCancelled && (!hadExisting || !model.IsTerminal(existing)) {
		h.logFn(LogLevelWarn,
			"result_write late_after_plan_terminal task=%s command=%s plan_status=%s reported=%s coerced=cancelled",
			params.TaskID, params.CommandID, state.PlanStatus, resultStatus)
		return model.StatusCancelled
	}
	if hadExisting && model.IsTerminal(existing) {
		h.logFn(LogLevelInfo, "result_write idempotent_resubmission task=%s command=%s existing_status=%s reported_status=%s",
			params.TaskID, params.CommandID, existing, resultStatus)
		return existing
	}
	return resultStatus
}

// updateCircuitBreaker checks task-unit idempotency and updates the circuit
// breaker counters if this is a new (non-duplicate) result. The original
// reported status (not the coerced one) is passed to the CB so metrics reflect
// actual task execution outcomes.
func (h *ResultWriteAPI) updateCircuitBreaker(state *model.CommandState, params ResultWriteParams, resultStatus model.Status, resultID string, now time.Time) {
	if recordedID, ok := state.AppliedResultIDs[params.TaskID]; ok {
		if recordedID != resultID {
			h.logFn(LogLevelWarn,
				"result_write applied_result_ids drift task=%s command=%s recorded=%s incoming=%s (skipping CB counter update)",
				params.TaskID, params.CommandID, recordedID, resultID)
		}
		return
	}

	if cb := h.circuitBreaker(); cb != nil {
		tripped, reason := cb.UpdateCounterOnResult(state, resultStatus, params.TaskID, resultID, now)
		if tripped {
			cb.TripBreaker(state, reason, now)
		}
	}
}

// recordQueueWriteFailedSticky persists the H2 sticky error marker when Phase A
// failed to update the worker queue to terminal status after retry.
func (h *ResultWriteAPI) recordQueueWriteFailedSticky(state *model.CommandState, params ResultWriteParams, resultID string, queueWriteFailed bool) {
	if !queueWriteFailed {
		return
	}
	if state.QueueWriteFailed == nil {
		state.QueueWriteFailed = make(map[string]string)
	}
	state.QueueWriteFailed[params.TaskID] = params.Reporter + ":" + resultID
	h.logFn(LogLevelWarn,
		"result_write queue_write_failed_sticky task=%s command=%s worker=%s result=%s",
		params.TaskID, params.CommandID, params.Reporter, resultID)
}

// checkLeaseEpochForBestEffort performs a read-only lease epoch check against
// the queue file. Returns (skip=true, reason) only on definitive epoch mismatch.
// On I/O errors, parse errors, or task-not-found (task may have been archived
// after completion), returns (skip=false, "") to allow writes to proceed.
//
// A try-lock on the queue key is acquired to prevent reading a partially-written
// queue file. If the lock cannot be acquired (concurrent Phase A write in
// progress), the check is skipped and writes are allowed — the next scan cycle
// will reconcile.
func (h *ResultWriteAPI) checkLeaseEpochForBestEffort(params ResultWriteParams) (skip bool, reason string) {
	queueLockKey := "queue:" + params.Reporter
	if !h.lockMap.TryLock(queueLockKey) {
		h.logFn(LogLevelInfo, "advisory_lease_check lock_contention reporter=%s task=%s (skipping; next scan will reconcile)",
			params.Reporter, params.TaskID)
		return false, ""
	}
	defer h.lockMap.Unlock(queueLockKey)

	queuePath := taskQueuePath(h.maestroDir, params.Reporter)
	data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		return false, ""
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		return false, ""
	}
	for _, task := range tq.Tasks {
		if task.ID == params.TaskID {
			if task.LeaseEpoch != params.LeaseEpoch {
				return true, fmt.Sprintf("lease_epoch_mismatch: queue=%d request=%d", task.LeaseEpoch, params.LeaseEpoch)
			}
			return false, ""
		}
	}
	// Task not found — may have been archived after completion; allow writes
	return false, ""
}
