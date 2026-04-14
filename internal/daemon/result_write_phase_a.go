package daemon

import (
	"fmt"
	"os"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// resultWritePhaseAResult holds the output of resultWritePhaseA.
type resultWritePhaseAResult struct {
	resultID         string
	retryTask        *model.Task // non-nil if a retry should be scheduled (caller handles registration)
	queueWriteFailed bool        // true when result was committed but queue terminal write failed (H2 sticky error)
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
		return nil, &resultWriteError{uds.ErrCodeInternal, err.Error()}
	}

	if idempotentID, err := h.checkResultIdempotency(&rf, params, resultStatus); err != nil {
		return nil, err
	} else if idempotentID != "" {
		return &resultWritePhaseAResult{resultID: idempotentID}, nil
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
		return &resultWritePhaseAResult{resultID: idempotentID}, nil
	}

	// 3. Defensive boundary check for taskIdx
	if taskIdx < 0 || taskIdx >= len(tq.Tasks) {
		return nil, &resultWriteError{uds.ErrCodeInternal,
			fmt.Sprintf("task %s: invalid task index %d (queue size %d) for reporter %s",
				params.TaskID, taskIdx, len(tq.Tasks), params.Reporter)}
	}

	// 4. Validate state existence and task registration
	if err := h.validateStateRegistration(params); err != nil {
		return nil, err
	}

	// 5. Append result entry
	resultID, err := h.appendResultEntry(&rf, params, resultStatus)
	if err != nil {
		return nil, err
	}

	// 6. Check for retry if task failed
	retryTask := h.evaluateRetry(&tq.Tasks[taskIdx], params, resultStatus)

	// 7. Update queue entry to terminal
	now := h.clock.Now().UTC().Format(time.RFC3339)
	queueWriteFailed := h.updateQueueState(&tq, taskIdx, params, resultStatus, resultID, now)

	return &resultWritePhaseAResult{resultID: resultID, retryTask: retryTask, queueWriteFailed: queueWriteFailed}, nil
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
		if queueTask.Status == resultStatus {
			for _, r := range rf.Results {
				if r.TaskID == params.TaskID {
					return taskIdx, r.ID, nil
				}
			}
			// Terminal in queue but no result entry — proceed to write result
		} else {
			return -1, "", &resultWriteError{uds.ErrCodeDuplicate,
				fmt.Sprintf("task %s already terminal with status %s in queue", params.TaskID, queueTask.Status)}
		}
	}

	// Fencing: task must be in_progress
	if queueTask.Status != model.StatusInProgress {
		return -1, "", &resultWriteError{uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s status is %s, expected in_progress", params.TaskID, queueTask.Status)}
	}

	// Fencing: lease epoch must match
	if queueTask.LeaseEpoch != params.LeaseEpoch {
		return -1, "", &resultWriteError{uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s lease_epoch mismatch: queue=%d, request=%d",
				params.TaskID, queueTask.LeaseEpoch, params.LeaseEpoch)}
	}

	// Fencing: lease must be held
	if queueTask.LeaseOwner == nil {
		return -1, "", &resultWriteError{uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s has no lease_owner (not dispatched)", params.TaskID)}
	}

	return taskIdx, "", nil
}

// validateStateRegistration verifies that the command state file exists and
// the task is registered within it.
func (h *ResultWriteAPI) validateStateRegistration(params ResultWriteParams) error {
	preState, err := h.fileStore.LoadCommandState(params.CommandID)
	if err != nil {
		if os.IsNotExist(err) {
			return &resultWriteError{uds.ErrCodeValidation,
				fmt.Sprintf("state not found for command %s", params.CommandID)}
		}
		return &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("read state: %v", err)}
	}
	if preState.TaskStates == nil {
		return &resultWriteError{uds.ErrCodeValidation,
			fmt.Sprintf("task %s not registered in state for command %s (no tasks registered)",
				params.TaskID, params.CommandID)}
	}
	if _, registered := preState.TaskStates[params.TaskID]; !registered {
		return &resultWriteError{uds.ErrCodeValidation,
			fmt.Sprintf("task %s not registered in state for command %s",
				params.TaskID, params.CommandID)}
	}
	return nil
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

// evaluateRetry checks whether a failed task should be retried and creates
// the retry task if so. Returns nil if no retry is warranted.
func (h *ResultWriteAPI) evaluateRetry(queueTask *model.Task, params ResultWriteParams, resultStatus model.Status) *model.Task {
	if resultStatus != model.StatusFailed || params.ExitCode == nil {
		return nil
	}
	retryHandler := NewTaskRetryHandler(h.maestroDir, *h.config, h.lockMap, h.logger, h.logLevel)
	shouldRetry, reason := retryHandler.ShouldRetryTask(queueTask, *params.ExitCode, params.RetrySafe)

	if !shouldRetry {
		h.logFn(LogLevelInfo, "task_retry_skipped task=%s reason=%s", params.TaskID, reason)
		return nil
	}

	rt, err := retryHandler.CreateRetryTask(queueTask, params.Reporter, *params.ExitCode)
	if err != nil {
		h.logFn(LogLevelError, "create_retry_task_failed task=%s error=%v", params.TaskID, err)
		return nil
	}
	return rt
}

// updateQueueState transitions the queue task to its terminal status and
// persists the queue file. Returns true if the queue write failed (H2 sticky
// error scenario).
func (h *ResultWriteAPI) updateQueueState(tq *model.TaskQueue, taskIdx int, params ResultWriteParams, resultStatus model.Status, resultID string, now string) bool {
	queueTask := &tq.Tasks[taskIdx]
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
