package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// Lock ordering for result_write_phase_a.go
//
// This file participates in the daemon-wide canonical lock order defined in
// doc.go (queue → state → result). The specific acquisition pattern is:
//
//   fileLock       — shared file mutex (serializes with PeriodicScan)
//   queue:{worker} — level 1 per-worker queue lock
//   result:{worker} — level 3 per-worker result lock
//
// State (level 2) is NOT acquired in this phase. It is deferred to
// resultWritePhaseB (for state updates) and the caller's retry registration
// (state + queue), both of which run after phaseA releases all locks.
// This avoids holding queue + state simultaneously, consistent with the
// reconcile package convention (see reconcile/run.go).
//
// See doc.go for the full canonical lock order and nesting rules.

// resultWritePhaseAResult holds the output of resultWritePhaseA.
type resultWritePhaseAResult struct {
	resultID         string
	retryTask        *model.Task // non-nil if a retry should be scheduled (caller handles registration)
	sourceTask       *model.Task // queue task snapshot used for post-verify repair scheduling
	queueWriteFailed bool        // true when result was committed but queue terminal write failed (H2 sticky error)
	originalTaskID   string      // non-empty if this task is a retry of another (for lineage update in Phase B)
	// abortByMaxRepair indicates the failed task was rejected for retry
	// because definition_of_abort.max_repair_count was reached (§S2-2). The
	// caller routes the task through verify_pending → repair_pending →
	// paused_for_replan so the planner can pick up replanning, instead of
	// landing at the generic failed terminal.
	abortByMaxRepair bool
	// duplicate is set when Phase A detected an idempotent/duplicate submission
	// (either in the result file, as a terminal queue entry, or via
	// AppliedResultIDs). The caller must short-circuit subsequent phases to
	// avoid emitting misleading "result_write" audit log lines as if this were
	// a fresh write (Bug H).
	duplicate bool
	// taskRunOnIntegration mirrors Task.RunOnIntegration of the queue entry
	// being terminated by this result_write. It is propagated out of Phase A
	// so the post-result AutoRecover hook (in handleResultWrite) can identify
	// publish_conflict resolution completions without re-loading the queue
	// file under a different lock order. Only meaningful for fresh
	// (non-duplicate) writes; on duplicate paths Phase A returns early
	// before this is populated.
	taskRunOnIntegration bool
}

func (h *ResultWriteAPI) resultWritePhaseA(params ResultWriteParams, resultStatus model.Status) (*resultWritePhaseAResult, error) {
	// Acquire shared file lock to serialize with QueueHandler's PeriodicScan
	h.acquireFileLock()
	defer h.releaseFileLock()

	// Lock queue file first (canonical order: queue → state → result).
	// Without this, handleQueueWriteTask (which locks "queue:{target}") and
	// resultWritePhaseA (which writes queue/{reporter}.yaml) can race.
	//
	// Note: this function acquires queue (level 1) then result (level 3),
	// skipping state (level 2). The state lock is acquired separately in
	// resultWritePhaseB after both locks are released. Retry registration
	// (state + queue) is also handled by the caller after phaseA returns,
	// ensuring the canonical lock order is maintained.
	queueLockKey := "queue:" + params.Reporter
	h.lockMap.Lock(queueLockKey)
	defer h.lockMap.Unlock(queueLockKey)

	workerLockKey := "result:" + params.Reporter
	h.lockMap.Lock(workerLockKey)
	defer h.lockMap.Unlock(workerLockKey)

	// 1. Load result file and check idempotency
	rf, err := h.fileStore.LoadResultFile(params.Reporter)
	if err != nil {
		return nil, &resultWriteWrappedError{
			resultWriteError: &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("load result file: %v", err)},
			err:              err,
		}
	}

	if idempotentID, err := h.checkResultIdempotency(&rf, params, resultStatus); err != nil {
		return nil, err
	} else if idempotentID != "" {
		// taskRunOnIntegration is explicitly false on duplicate paths: the
		// post-result AutoRecover hook short-circuits when phaseA.duplicate is
		// true, so the value is unused. Setting it explicitly guards against
		// silent regressions if a future caller forgets that gate.
		return &resultWritePhaseAResult{resultID: idempotentID, duplicate: true, taskRunOnIntegration: false}, nil
	}

	// 2. Fencing verification
	tq, err := h.fileStore.LoadQueueFile(params.Reporter)
	if err != nil {
		return nil, &resultWriteError{uds.ErrCodeInternal, err.Error()}
	}

	taskIdx, idempotentID, err := h.validateFencing(&tq, &rf, params, resultStatus)
	if err != nil {
		return nil, err
	}
	if idempotentID != "" {
		return &resultWritePhaseAResult{resultID: idempotentID, duplicate: true, taskRunOnIntegration: false}, nil
	}

	// 3. Defensive boundary check for taskIdx
	if taskIdx < 0 || taskIdx >= len(tq.Tasks) {
		return nil, &resultWriteError{uds.ErrCodeInternal,
			fmt.Sprintf("task %s: invalid task index %d (queue size %d) for reporter %s",
				params.TaskID, taskIdx, len(tq.Tasks), params.Reporter)}
	}

	// 4. Validate state existence and task registration
	preState, err := h.validateStateRegistration(params)
	if err != nil {
		return nil, err
	}

	// 4b. Check AppliedResultIDs for duplicate (defense-in-depth against TOCTOU)
	if preState.AppliedResultIDs != nil {
		if existingResultID, ok := preState.AppliedResultIDs[params.TaskID]; ok {
			h.logFn(LogLevelWarn, "duplicate_result_skipped task=%s existing_result=%s command=%s",
				params.TaskID, existingResultID, params.CommandID)
			return &resultWritePhaseAResult{resultID: existingResultID, duplicate: true, taskRunOnIntegration: false}, nil
		}
	}

	sourceTask := tq.Tasks[taskIdx]

	// run_on_main is the strict read-only contract (post-publish verification
	// against the merged main branch). The Worker must report empty
	// files_changed; a non-empty value indicates the LLM mistook
	// "files I inspected" for "files I modified". Strip and warn instead
	// of rejecting so the (otherwise successful) read-only check still
	// progresses; rejection here would stall the publish pipeline on
	// what is essentially a reporting bug.
	//
	// The strip MUST run before validateFilesChangedWithinExpectedPaths
	// so that run_on_main tasks with narrow expected_paths (the common
	// case — read-only verify tasks usually declare a tight surface)
	// don't get rejected on the "files I read but did not modify" data
	// before the strip can take effect.
	if sourceTask.RunOnMain && len(params.FilesChanged) > 0 {
		h.logFn(LogLevelWarn,
			"files_changed_stripped_for_run_on_main task=%s command=%s reported=%v "+
				"(run_on_main is read-only by contract; see worker.md §`--files-changed` の正しい意味)",
			params.TaskID, params.CommandID, params.FilesChanged)
		params.FilesChanged = nil
	}

	if err := validateFilesChangedWithinExpectedPaths(params.FilesChanged, sourceTask.ExpectedPaths); err != nil {
		return nil, &resultWriteError{uds.ErrCodeValidation,
			fmt.Sprintf("files_changed outside expected_paths for task %s: %v", params.TaskID, err)}
	}

	// 5. Append result entry
	resultID, err := h.appendResultEntry(&rf, params, resultStatus)
	if err != nil {
		return nil, err
	}

	// 6. Check for retry if task failed
	retryTask, abortByMaxRepair := h.evaluateRetry(&sourceTask, params, resultStatus)

	// 6b. Extract original task ID for retry lineage (Pass to Phase B)
	originalTaskID := sourceTask.OriginalTaskID

	// 7. Update queue entry to terminal
	now := h.clock.Now().UTC().Format(time.RFC3339)
	queueWriteFailed := h.updateQueueState(&tq, taskIdx, params, resultStatus, resultID, now)

	// 8. Rollback result if queue write failed (atomicity recovery)
	if queueWriteFailed {
		if rollbackErr := h.rollbackResultEntry(&rf, resultID, params); rollbackErr != nil {
			// Rollback also failed — result is orphaned. Log with full context
			// for R1 reconciler detection and proceed with sticky error path.
			h.logFn(LogLevelError,
				"result_write rollback_failed result_id=%s task=%s queue=%s error=%v "+
					"(orphaned result; R1 reconciler will repair)",
				resultID, params.TaskID, h.fileStore.QueueFilePath(params.Reporter), rollbackErr)
			// Write orphaned marker file so R1 reconciler can detect and repair
			// without cross-referencing state.QueueWriteFailed.
			h.writeOrphanedMarker(params.Reporter, resultID, params.TaskID)
		} else {
			// Rollback succeeded — return clean error so caller can retry.
			return nil, &resultWriteError{uds.ErrCodeInternal,
				fmt.Sprintf("queue write failed for task %s after result %s committed; result rolled back successfully",
					params.TaskID, resultID)}
		}
	}

	return &resultWritePhaseAResult{
		resultID:             resultID,
		retryTask:            retryTask,
		sourceTask:           &sourceTask,
		queueWriteFailed:     queueWriteFailed,
		originalTaskID:       originalTaskID,
		abortByMaxRepair:     abortByMaxRepair,
		taskRunOnIntegration: tq.Tasks[taskIdx].RunOnIntegration,
	}, nil
}

func validateFilesChangedWithinExpectedPaths(filesChanged, expectedPaths []string) error {
	if len(filesChanged) == 0 || len(expectedPaths) == 0 {
		return nil
	}
	var outside []string
	for _, file := range filesChanged {
		if !pathAllowedByExpectedPaths(file, expectedPaths) {
			outside = append(outside, file)
		}
	}
	if len(outside) > 0 {
		return fmt.Errorf("%s", strings.Join(outside, ", "))
	}
	return nil
}

func pathAllowedByExpectedPaths(file string, expectedPaths []string) bool {
	file = filepath.ToSlash(filepath.Clean(strings.TrimSpace(file)))
	for _, exp := range expectedPaths {
		exp = filepath.ToSlash(filepath.Clean(strings.TrimSpace(exp)))
		if exp == "" {
			continue
		}
		if exp == "." {
			return true
		}
		exp = strings.TrimSuffix(exp, "/")
		if file == exp || strings.HasPrefix(file, exp+"/") {
			return true
		}
	}
	return false
}

// checkResultIdempotency checks whether a result for the given task already
// exists in the result file. Returns the existing result ID for an idempotent
// match, "" if no prior result exists, or an error for status conflicts.
func (h *ResultWriteAPI) checkResultIdempotency(rf *model.TaskResultFile, params ResultWriteParams, resultStatus model.Status) (string, error) {
	for _, r := range rf.Results {
		if r.TaskID == params.TaskID {
			if r.Status == resultStatus {
				return r.ID, nil
			}
			return "", &resultWriteError{uds.ErrCodeDuplicate,
				fmt.Sprintf("task %s already has result with status %s, cannot report %s",
					params.TaskID, r.Status, resultStatus)}
		}
	}
	return "", nil
}

// validateFencing finds the task in the queue and verifies fencing invariants
// (command ID consistency, terminal idempotency, in_progress status, lease
// epoch, lease ownership). Returns the task index and, for terminal-idempotent
// matches, the existing result ID (caller should return early).
func (h *ResultWriteAPI) validateFencing(tq *model.TaskQueue, rf *model.TaskResultFile, params ResultWriteParams, resultStatus model.Status) (int, string, error) {
	taskIdx := -1
	for i, task := range tq.Tasks {
		if task.ID == params.TaskID {
			taskIdx = i
			break
		}
	}
	if taskIdx == -1 {
		return -1, "", &resultWriteError{uds.ErrCodeNotFound,
			fmt.Sprintf("task %s not found in queue %s", params.TaskID, params.Reporter)}
	}

	queueTask := &tq.Tasks[taskIdx]

	// Command ID consistency check
	if queueTask.CommandID != params.CommandID {
		return -1, "", &resultWriteError{uds.ErrCodeValidation,
			fmt.Sprintf("command_id mismatch: queue task has %q, request has %q",
				queueTask.CommandID, params.CommandID)}
	}

	// If task is already terminal with same status, treat as idempotent success
	if model.IsTerminal(queueTask.Status) {
		if queueTask.Status != resultStatus {
			return -1, "", &resultWriteError{uds.ErrCodeDuplicate,
				fmt.Sprintf("task %s already terminal with status %s in queue", params.TaskID, queueTask.Status)}
		}
		for _, r := range rf.Results {
			if r.TaskID == params.TaskID {
				return taskIdx, r.ID, nil
			}
		}
		// Terminal in queue but no result entry — proceed to write result
		// and re-save the same terminal queue status below.
		return taskIdx, "", nil
	}

	// Fencing: task must be in_progress. Attach FencingDetails so the
	// CLI / Worker shell wrapper can branch on a stable schema instead
	// of grepping the message string.
	if queueTask.Status != model.StatusInProgress {
		return -1, "", newFencingError(uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s status is %s, expected in_progress", params.TaskID, queueTask.Status),
			uds.FencingDetails{
				Kind:          "fencing_status_mismatch",
				TaskID:        params.TaskID,
				WorkerID:      params.Reporter,
				CurrentStatus: string(queueTask.Status),
				CurrentEpoch:  queueTask.LeaseEpoch,
				RequestEpoch:  params.LeaseEpoch,
			})
	}

	// Fencing: lease epoch must match
	if queueTask.LeaseEpoch != params.LeaseEpoch {
		return -1, "", newFencingError(uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s lease_epoch mismatch: queue=%d, request=%d",
				params.TaskID, queueTask.LeaseEpoch, params.LeaseEpoch),
			uds.FencingDetails{
				Kind:          "fencing_epoch_mismatch",
				TaskID:        params.TaskID,
				WorkerID:      params.Reporter,
				CurrentEpoch:  queueTask.LeaseEpoch,
				RequestEpoch:  params.LeaseEpoch,
				CurrentStatus: string(queueTask.Status),
			})
	}

	// Fencing: lease must be held
	if queueTask.LeaseOwner == nil {
		return -1, "", newFencingError(uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s has no lease_owner (not dispatched)", params.TaskID),
			uds.FencingDetails{
				Kind:          "fencing_status_mismatch",
				TaskID:        params.TaskID,
				WorkerID:      params.Reporter,
				CurrentStatus: string(queueTask.Status),
				CurrentEpoch:  queueTask.LeaseEpoch,
				RequestEpoch:  params.LeaseEpoch,
			})
	}

	return taskIdx, "", nil
}

// validateStateRegistration verifies that the command state file exists and
// the task is registered within it. Returns the loaded state for additional
// idempotency checks by the caller.
func (h *ResultWriteAPI) validateStateRegistration(params ResultWriteParams) (*model.CommandState, error) {
	preState, err := h.fileStore.LoadCommandState(params.CommandID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &resultWriteError{uds.ErrCodeValidation,
				fmt.Sprintf("state not found for command %s", params.CommandID)}
		}
		return nil, &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("read state: %v", err)}
	}
	if preState.TaskStates == nil {
		return nil, &resultWriteError{uds.ErrCodeValidation,
			fmt.Sprintf("task %s not registered in state for command %s (no tasks registered)",
				params.TaskID, params.CommandID)}
	}
	if _, registered := preState.TaskStates[params.TaskID]; !registered {
		return nil, &resultWriteError{uds.ErrCodeValidation,
			fmt.Sprintf("task %s not registered in state for command %s",
				params.TaskID, params.CommandID)}
	}
	return &preState, nil
}

// appendResultEntry generates a result ID, appends the new result entry to
// the result file, and persists it to disk via the FileStore.
func (h *ResultWriteAPI) appendResultEntry(rf *model.TaskResultFile, params ResultWriteParams, resultStatus model.Status) (string, error) {
	resultID, err := model.GenerateID(model.IDTypeResult)
	if err != nil {
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("generate result ID: %v", err)}
	}

	if rf.SchemaVersion == 0 {
		rf.SchemaVersion = 1
		rf.FileType = "result_task"
	}

	now := h.clock.Now().UTC().Format(time.RFC3339)
	rf.Results = append(rf.Results, model.TaskResult{
		ID:                     resultID,
		TaskID:                 params.TaskID,
		CommandID:              params.CommandID,
		Status:                 resultStatus,
		Summary:                params.Summary,
		FilesChanged:           params.FilesChanged,
		PartialChangesPossible: params.PartialChangesPossible,
		RetrySafe:              params.RetrySafe,
		CreatedAt:              now,
	})

	if err := h.fileStore.SaveResultFile(params.Reporter, *rf); err != nil {
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("write results file: %v", err)}
	}
	h.recordSelfWrite(h.fileStore.ResultFilePath(params.Reporter), *rf)

	return resultID, nil
}

// rollbackResultEntry removes a previously appended result entry from the
// result file. Called when the queue write fails after the result was committed,
// to restore atomicity between result and queue state.
func (h *ResultWriteAPI) rollbackResultEntry(rf *model.TaskResultFile, resultID string, params ResultWriteParams) error {
	filtered := make([]model.TaskResult, 0, len(rf.Results))
	for _, r := range rf.Results {
		if r.ID != resultID {
			filtered = append(filtered, r)
		}
	}
	rf.Results = filtered
	if err := h.fileStore.SaveResultFile(params.Reporter, *rf); err != nil {
		return fmt.Errorf("rollback save: %w", err)
	}
	h.recordSelfWrite(h.fileStore.ResultFilePath(params.Reporter), *rf)
	h.logFn(LogLevelWarn,
		"result_write result_rolled_back result_id=%s task=%s reporter=%s",
		resultID, params.TaskID, params.Reporter)
	return nil
}

// evaluateRetry checks whether a failed task should be retried and creates
// the retry task if so. The second return value is true when retry was
// rejected because definition_of_abort.max_repair_count was reached, in which
// case the caller MUST route the task to paused_for_replan per §S2-2 instead
// of treating it as a plain non-retryable failure.
func (h *ResultWriteAPI) evaluateRetry(queueTask *model.Task, params ResultWriteParams, resultStatus model.Status) (*model.Task, bool) {
	if resultStatus != model.StatusFailed || params.ExitCode == nil {
		return nil, false
	}
	retryHandler := NewTaskRetryHandler(h.maestroDir, *h.config, h.lockMap, h.logger, h.logLevel)
	shouldRetry, reason := retryHandler.ShouldRetryTask(queueTask, *params.ExitCode, params.RetrySafe)

	if !shouldRetry {
		h.logFn(LogLevelInfo, "task_retry_skipped task=%s reason=%s", params.TaskID, reason)
		return nil, IsAbortByMaxRepair(reason)
	}

	rt, err := retryHandler.CreateRetryTask(queueTask, params.Reporter, *params.ExitCode)
	if err != nil {
		h.logFn(LogLevelError, "create_retry_task_failed task=%s error=%v", params.TaskID, err)
		return nil, false
	}
	return rt, false
}

// writeOrphanedMarker writes a sidecar marker file when a result entry cannot
// be rolled back after a queue write failure. The marker enables the R1
// reconciler to detect orphaned results without cross-referencing
// state.QueueWriteFailed. Each line contains: result_id task_id orphaned_at.
func (h *ResultWriteAPI) writeOrphanedMarker(reporter, resultID, taskID string) {
	orphanPath := h.fileStore.ResultFilePath(reporter) + ".orphaned"
	f, err := os.OpenFile(orphanPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // orphanPath is constructed from controlled result file directory
	if err != nil {
		h.logFn(LogLevelError,
			"orphaned_marker_create_failed reporter=%s result=%s task=%s error=%v",
			reporter, resultID, taskID, err)
		return
	}
	defer func() { _ = f.Close() }() // best-effort: append-only marker file
	now := h.clock.Now().UTC().Format(time.RFC3339)
	if _, wErr := fmt.Fprintf(f, "%s %s %s\n", resultID, taskID, now); wErr != nil {
		h.logFn(LogLevelError,
			"orphaned_marker_write_failed reporter=%s result=%s task=%s error=%v",
			reporter, resultID, taskID, wErr)
	}
}

// updateQueueState transitions the queue task to its terminal status and
// persists the queue file. Returns true if the queue write failed (H2 sticky
// error scenario).
//
// Phase A intentionally bypasses lease.Manager.releaseLease here. The
// canonical release path transitions in_progress→pending, but a worker
// result is committing a terminal status (completed/failed/cancelled/
// dead_letter), so the lease lifecycle is collapsed in-place. LeaseEpoch
// is kept as-is (it is the fencing key for any late heartbeat) and only
// the owner/expiry that lose meaning at terminal are cleared.
func (h *ResultWriteAPI) updateQueueState(tq *model.TaskQueue, taskIdx int, params ResultWriteParams, resultStatus model.Status, resultID string, now string) bool {
	queueTask := &tq.Tasks[taskIdx]

	// Validate the in_progress→terminal transition before mutating.
	// validateFencing has already accepted the result, but a parallel
	// reconciler (e.g. R1 clearing queue_write_failed) may have moved the
	// task back to pending or another non-in_progress state between the
	// fencing check and now. ValidateCommandTaskQueueTransition rejects
	// completed/failed/cancelled/dead_letter from non-in_progress origins;
	// when that happens we leave the queue file alone and let the
	// reconciler converge — the result file is already committed so the
	// command-level invariant is preserved.
	if err := model.ValidateCommandTaskQueueTransition(queueTask.Status, resultStatus); err != nil {
		h.logFn(LogLevelWarn,
			"result_write_invalid_transition task=%s from=%s to=%s result=%s error=%v (queue write skipped; reconciler will repair)",
			params.TaskID, queueTask.Status, resultStatus, resultID, err)
		// Surface as queueWriteFailed so the H2 sticky-error machinery picks
		// it up; R1 reconciler is the authoritative repair path for this
		// race window.
		return true
	}

	queueTask.Status = resultStatus
	queueTask.LeaseOwner = nil
	queueTask.LeaseExpiresAt = nil
	queueTask.UpdatedAt = now

	if err := h.fileStore.SaveQueueFile(params.Reporter, *tq); err != nil {
		// Result is already committed — retry queue write once before giving up.
		h.logFn(LogLevelWarn, "result_write queue_write_failed task=%s, retrying: %v", params.TaskID, err)
		if retryErr := h.fileStore.SaveQueueFile(params.Reporter, *tq); retryErr != nil {
			h.logFn(LogLevelError, "result_write queue_write_retry_failed task=%s result=%s: %v (sticky error recorded; R1 reconciler will repair)",
				params.TaskID, resultID, retryErr)
			return true
		}
		h.recordSelfWrite(h.fileStore.QueueFilePath(params.Reporter), *tq)
		return false
	}
	h.recordSelfWrite(h.fileStore.QueueFilePath(params.Reporter), *tq)
	return false
}
