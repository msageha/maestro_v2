package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// fallbackRecorder records worker success/failure for health monitoring.
type fallbackRecorder interface {
	RecordSuccess(workerID string)
	RecordFailure(workerID string)
}

// circuitBreakerUpdater updates circuit breaker counters on result.
type circuitBreakerUpdater interface {
	UpdateCounterOnResult(state *model.CommandState, resultStatus model.Status, resultID string, now time.Time) (bool, string)
	TripBreaker(state *model.CommandState, reason string, now time.Time)
}

// reviewDispatcher dispatches review requests for completed tasks.
type reviewDispatcher interface {
	Enabled() bool
	DispatchIfEligible(ctx context.Context, params ResultWriteParams)
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

func (h *ResultWriteAPI) handleResultWrite(req *uds.Request) *uds.Response {
	var params ResultWriteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	if params.Reporter == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "reporter is required")
	}
	if !validate.IsValidBaseName(params.Reporter) {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid reporter: %q", params.Reporter))
	}
	if params.TaskID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "task_id is required")
	}
	if params.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required")
	}
	if err := validate.ID(params.CommandID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid command_id: %v", err))
	}
	if err := validate.ID(params.TaskID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid task_id: %v", err))
	}

	resultStatus := model.Status(params.Status)
	switch resultStatus {
	case model.StatusCompleted, model.StatusFailed:
		// valid terminal statuses for worker result reporting
	default:
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("status must be completed|failed, got %q", params.Status))
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

	// Phase B: Per-command mutex (state/ updates)
	if err := h.resultWritePhaseB(params, resultID, resultStatus, resultWritePhaseAResult.queueWriteFailed); err != nil {
		h.logFn(LogLevelError, "result_write phase_b error task=%s command=%s: %v",
			params.TaskID, params.CommandID, err)
		return uds.ErrorResponse(uds.ErrCodeInternal,
			fmt.Sprintf("state update failed: %v (result %s committed, run 'maestro plan rebuild' to fix)", err, resultID))
	}

	// Fallback tracking: record success/failure for worker health monitoring.
	if fm := h.fallbackMgr(); fm != nil {
		switch resultStatus {
		case model.StatusCompleted:
			fm.RecordSuccess(params.Reporter)
		case model.StatusFailed:
			fm.RecordFailure(params.Reporter)
		}
	}

	// Retry registration (state then queue — correct lock order).
	// Runs after phaseA has released queue+result locks, so acquiring
	// state(L2) then queue(L1) does not violate canonical order.
	if resultWritePhaseAResult.retryTask != nil {
		retryTask := resultWritePhaseAResult.retryTask
		retryHandler := NewTaskRetryHandler(h.maestroDir, *h.config, h.lockMap, h.logger, h.logLevel)

		// First register in state (acquires state lock)
		if err := retryHandler.RegisterRetryTaskInState(retryTask, params.CommandID); err != nil {
			h.logFn(LogLevelError, "register_retry_task_failed task=%s command=%s error=%v", retryTask.ID, params.CommandID, err)
		} else {
			// Then add to queue (acquires queue lock independently)
			if err := retryHandler.AddRetryTaskToQueue(retryTask, params.Reporter); err != nil {
				// M2 note: A compensating delete on the state entry was considered
				// but is unsafe. AddRetryTaskToQueue ultimately calls AtomicWrite,
				// which can return an error after os.Rename has already committed
				// the new queue file (e.g. when the post-rename syncDir() fails).
				// In that case the queue would already contain the retry task and
				// rolling back the state entry would orphan the queue entry. Since
				// the daemon cannot distinguish a pre-rename failure from a
				// post-rename failure, we leave the state entry in place and mark
				// it as RetryEnqueueFailed so the R1 reconciler can either
				// re-enqueue it or transition it to dead_letter.
				h.logFn(LogLevelError, "add_retry_task_failed task=%s worker=%s command=%s error=%v "+
					"(task registered in state but enqueue failed; R1 reconciler will re-enqueue or mark failed)",
					retryTask.ID, params.Reporter, params.CommandID, err)
				if markErr := retryHandler.MarkRetryEnqueueFailed(retryTask.ID, params.Reporter, params.CommandID); markErr != nil {
					h.logFn(LogLevelError, "mark_retry_enqueue_failed task=%s command=%s error=%v",
						retryTask.ID, params.CommandID, markErr)
				}
			} else {
				h.logFn(LogLevelInfo, "task_retry_scheduled task=%s retry_id=%s attempt=%d",
					params.TaskID, retryTask.ID, retryTask.Attempts)
			}
		}
	}

	// Best-effort lease epoch check: skip learnings/skill_candidates writes
	// if the lease epoch no longer matches (stale worker after revocation).
	// Read-only check without lock is acceptable — this is advisory, not authoritative.
	// On detection, we re-check authoritatively under canonical locks and
	// persist a RejectedSubmission so the loss is auditable / user-visible.
	bestEffortAllowed := true
	var rejectionID string
	// Only treat advisory skip as a "rejected drop" when at least one
	// best-effort write would actually have occurred. If learnings are
	// disabled config-side, the learnings payload is not "lost" — it was
	// never going to be written. Skill candidates have no enable flag, so
	// any skill_candidates payload does count.
	hasMeaningfulBestEffort := (len(params.Learnings) > 0 && h.config.Learnings.Enabled) ||
		len(params.SkillCandidates) > 0
	if hasMeaningfulBestEffort {
		if skip, reason := h.checkLeaseEpochForBestEffort(params); skip {
			h.logFn(LogLevelWarn, "best_effort_writes_skipped task=%s command=%s reason=%s",
				params.TaskID, params.CommandID, reason)
			bestEffortAllowed = false
			if id, persisted, perr := h.persistLeaseRejection(params, reason); perr != nil {
				h.logFn(LogLevelError,
					"lease_rejection_persist_failed task=%s command=%s reporter=%s error=%v "+
						"(learnings/skill_candidates lost; not recorded in audit trail)",
					params.TaskID, params.CommandID, params.Reporter, perr)
			} else if !persisted {
				// Authoritative re-check showed lease epoch matches under
				// the lock — the advisory read was racing a queue write.
				// Re-allow the best-effort writes; the loss was a false
				// positive.
				h.logFn(LogLevelInfo,
					"lease_rejection_recheck_cleared task=%s command=%s reporter=%s "+
						"(advisory mismatch resolved under lock; best-effort writes re-enabled)",
					params.TaskID, params.CommandID, params.Reporter)
				bestEffortAllowed = true
			} else {
				rejectionID = id
				h.logFn(LogLevelWarn,
					"lease_rejection_persisted rejection_id=%s task=%s command=%s reporter=%s reason=%s "+
						"learnings_count=%d skill_candidate_count=%d",
					rejectionID, params.TaskID, params.CommandID, params.Reporter, reason,
					len(params.Learnings), len(params.SkillCandidates))
			}
		}
	}

	// Learnings: best-effort write after core phases succeed.
	if bestEffortAllowed && len(params.Learnings) > 0 && h.config.Learnings.Enabled {
		learningsPath := filepath.Join(h.maestroDir, "state", "learnings.yaml")
		if err := h.writeLearnings(params, resultID); err != nil {
			h.logFn(LogLevelWarn, "learnings_write_failed result=%s task=%s command=%s path=%s count=%d error=%v "+
				"(learnings data lost; core result already committed; manual recovery: re-submit result with same learnings)",
				resultID, params.TaskID, params.CommandID, learningsPath, len(params.Learnings), err)
		}
	}

	// Skill candidates: best-effort write after core phases succeed.
	if bestEffortAllowed && len(params.SkillCandidates) > 0 {
		candidatesPath := filepath.Join(h.maestroDir, "state", "skill_candidates.yaml")
		if err := h.writeSkillCandidates(params); err != nil {
			h.logFn(LogLevelWarn, "skill_candidates_write_failed result=%s task=%s command=%s path=%s count=%d error=%v "+
				"(skill candidates lost; core result already committed; manual recovery: re-submit result with same skill_candidates)",
				resultID, params.TaskID, params.CommandID, candidatesPath, len(params.SkillCandidates), err)
		}
	}

	// Advisory review dispatch: non-blocking, best-effort.
	// Only dispatch for completed tasks when reviews are enabled and the task qualifies.
	if rc := h.reviewCoord(); resultStatus == model.StatusCompleted && rc != nil && rc.Enabled() {
		rc.DispatchIfEligible(h.ctx(), params)
	}

	// Phase C: Trigger scan (best effort dependency unblocking).
	if h.triggerScan != nil {
		h.triggerScan(h.ctx())
	}

	h.logFn(LogLevelInfo, "result_write result_id=%s task=%s command=%s status=%s reporter=%s",
		resultID, params.TaskID, params.CommandID, params.Status, params.Reporter)
	respPayload := map[string]string{"result_id": resultID}
	if rejectionID != "" {
		respPayload["lease_rejection_id"] = rejectionID
		respPayload["lease_rejection_warning"] =
			"learnings/skill_candidates rejected: lease revoked; recorded as " + rejectionID
	}
	return uds.SuccessResponse(respPayload)
}

type resultWriteError struct {
	Code    string
	Message string
}

func (e *resultWriteError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

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

	// 3. Validate state existence and task registration
	if err := h.validateStateRegistration(params); err != nil {
		return nil, err
	}

	// 4. Append result entry
	resultID, err := h.appendResultEntry(&rf, params, resultStatus)
	if err != nil {
		return nil, err
	}

	// 5. Check for retry if task failed
	retryTask := h.evaluateRetry(&tq.Tasks[taskIdx], params, resultStatus)

	// 6. Update queue entry to terminal
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

func (h *ResultWriteAPI) resultWritePhaseB(params ResultWriteParams, resultID string, resultStatus model.Status, queueWriteFailed bool) error {
	cmdLockKey := "state:" + params.CommandID
	h.lockMap.Lock(cmdLockKey)
	defer h.lockMap.Unlock(cmdLockKey)

	statePath := filepath.Join(h.maestroDir, "state", "commands", params.CommandID+".yaml")

	return updateYAMLFile(statePath, func(state *model.CommandState) error {
		// Guard: if state was zero-value (file not found), the command state is missing
		if state.CommandID == "" && state.SchemaVersion == 0 && state.TaskStates == nil {
			return fmt.Errorf("state not found for command %s", params.CommandID)
		}
		if state.TaskStates == nil {
			state.TaskStates = make(map[string]model.Status)
		}

		// Late-arriving result handling: if the plan has already failed or
		// cancelled (e.g. via cancel command, or because another required task
		// failed and the planner finalized the command before this dispatched
		// task reported), a still-pending task's late completed/failed result
		// must NOT be applied verbatim — that would leave
		// TaskStates[taskID] = completed while PlanStatus = failed/cancelled,
		// an inconsistent view that plan.Complete's idempotency skip path
		// (complete.go:113-118) would not re-validate. Coerce the recorded task
		// state to cancelled so the plan view stays consistent.
		//
		// Two carve-outs (per codex review):
		//  1. plan_status=completed is left alone — a plan that successfully
		//     completed reflects a terminal task view we must not regress, and
		//     duplicate result re-submission shouldn't flip completed→cancelled.
		//  2. If TaskStates[taskID] is already terminal, don't overwrite it
		//     (idempotent re-submission of the same late result).
		//
		// The result file (audit trail) and queue entry still record the
		// reporter's original status — only the plan's TaskStates view is
		// normalized. The circuit breaker still observes the original reported
		// status so its counters reflect actual task execution outcomes.
		recordedStatus := resultStatus
		existing, hadExisting := state.TaskStates[params.TaskID]
		planFailedOrCancelled := state.PlanStatus == model.PlanStatusFailed || state.PlanStatus == model.PlanStatusCancelled
		if planFailedOrCancelled && (!hadExisting || !model.IsTerminal(existing)) {
			h.logFn(LogLevelWarn,
				"result_write late_after_plan_terminal task=%s command=%s plan_status=%s reported=%s coerced=cancelled",
				params.TaskID, params.CommandID, state.PlanStatus, resultStatus)
			recordedStatus = model.StatusCancelled
		} else if hadExisting && model.IsTerminal(existing) {
			// Idempotent re-submission: keep settled terminal state.
			recordedStatus = existing
		}
		state.TaskStates[params.TaskID] = recordedStatus

		now := h.clock.Now()
		state.UpdatedAt = now.UTC().Format(time.RFC3339)

		// Task-unit idempotency: if this task already has a result recorded in
		// AppliedResultIDs, treat the current submission as a duplicate and
		// skip the circuit breaker counter update. Without this guard the
		// failure counter would inflate on every retry of the same failed
		// result (e.g. when phaseA returned the existing result_id for an
		// idempotent retry) and trip the breaker spuriously. AppliedResultIDs
		// is keyed by task_id and is the authoritative record of "already
		// applied" at task granularity, which is what phaseB cares about.
		//
		// Note: when CB does run, we pass the original reported status (not
		// the coerced one above) so metrics reflect what actually happened in
		// the worker.
		alreadyApplied := false
		if recordedID, ok := state.AppliedResultIDs[params.TaskID]; ok {
			alreadyApplied = true
			if recordedID != resultID {
				// State drift: a different result_id is recorded. Skip the CB
				// update to avoid spurious trips and log for diagnosis.
				h.logFn(LogLevelWarn,
					"result_write applied_result_ids drift task=%s command=%s recorded=%s incoming=%s (skipping CB counter update)",
					params.TaskID, params.CommandID, recordedID, resultID)
			}
		}

		if cb := h.circuitBreaker(); !alreadyApplied && cb != nil {
			tripped, reason := cb.UpdateCounterOnResult(state, resultStatus, resultID, now)
			if tripped {
				cb.TripBreaker(state, reason, now)
			}
		}

		if state.AppliedResultIDs == nil {
			state.AppliedResultIDs = make(map[string]string)
		}
		state.AppliedResultIDs[params.TaskID] = resultID

		// H2 sticky error: if Phase A failed to update the worker queue to
		// terminal after retry, persist a marker so R1 reconciler can repair
		// the result-terminal/queue-in_progress mismatch durably across daemon
		// restarts. The value encodes "workerID:resultID" so the reconciler
		// knows which queue to inspect and which result the marker corresponds
		// to.
		if queueWriteFailed {
			if state.QueueWriteFailed == nil {
				state.QueueWriteFailed = make(map[string]string)
			}
			state.QueueWriteFailed[params.TaskID] = params.Reporter + ":" + resultID
			h.logFn(LogLevelWarn,
				"result_write queue_write_failed_sticky task=%s command=%s worker=%s result=%s",
				params.TaskID, params.CommandID, params.Reporter, resultID)
		}

		return nil
	})
}

// persistLeaseRejection records a RejectedSubmission entry in the reporter's
// results file when a best-effort lease epoch check detects that the worker
// no longer holds a valid lease. The function takes the canonical
// queue→result lock order and re-verifies the lease epoch under the lock so
// the persisted record is authoritative (not based on the unlocked advisory
// read). Duplicate stale-worker retries with identical payload are
// suppressed via a content fingerprint (DedupKey) so the rejection log
// cannot grow unbounded.
//
// Returns the persisted rejection ID, or "" with an error if the persist
// failed. If the post-lock re-check shows the lease epoch now matches
// (race: revoke rolled back, or task archived), the function returns
// ("", nil) and the caller should treat best-effort writes as still
// rejected — the original advisory check has already gated them and we
// avoid creating a misleading audit record.
func (h *ResultWriteAPI) persistLeaseRejection(params ResultWriteParams, advisoryReason string) (string, bool, error) {
	h.acquireFileLock()
	defer h.releaseFileLock()

	queueLockKey := "queue:" + params.Reporter
	h.lockMap.Lock(queueLockKey)
	defer h.lockMap.Unlock(queueLockKey)

	resultLockKey := "result:" + params.Reporter
	h.lockMap.Lock(resultLockKey)
	defer h.lockMap.Unlock(resultLockKey)

	// Re-verify lease epoch mismatch authoritatively under the queue lock.
	// If the queue file is unreadable / parse-fails / task is gone, fall
	// back to the advisory reason captured by the caller — the data is
	// still considered lost.
	queuePath := filepath.Join(h.maestroDir, "queue", params.Reporter+".yaml")
	queueLeaseEpoch := -1
	authoritativeReason := advisoryReason
	if data, err := os.ReadFile(queuePath); err == nil { //nolint:gosec // queuePath is constructed from a controlled application queue directory
		var tq model.TaskQueue
		if perr := yamlv3.Unmarshal(data, &tq); perr == nil {
			found := false
			for _, task := range tq.Tasks {
				if task.ID == params.TaskID {
					found = true
					queueLeaseEpoch = task.LeaseEpoch
					if task.LeaseEpoch == params.LeaseEpoch {
						// Race: epoch matches under the lock. Tell the
						// caller this was a false positive so it can
						// re-enable best-effort writes.
						return "", false, nil
					}
					authoritativeReason = fmt.Sprintf(
						"lease_epoch_mismatch_under_lock: queue=%d request=%d",
						task.LeaseEpoch, params.LeaseEpoch)
					break
				}
			}
			if !found {
				authoritativeReason = "task_archived_after_lease_revoke"
			}
		}
	}

	resultPath := filepath.Join(h.maestroDir, "results", params.Reporter+".yaml")
	var rf model.TaskResultFile
	if data, err := os.ReadFile(resultPath); err == nil { //nolint:gosec // resultPath is constructed from a controlled application results directory
		if perr := yamlv3.Unmarshal(data, &rf); perr != nil {
			return "", false, fmt.Errorf("parse results file: %w", perr)
		}
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("read results file: %w", err)
	}

	if rf.SchemaVersion == 0 {
		rf.SchemaVersion = 1
		rf.FileType = "result_task"
	}

	// Dedup key includes queueLeaseEpoch so that a fresh revoke against
	// the same payload (e.g. queue epoch advanced 2→3) produces a new
	// audit record rather than collapsing into a stale one.
	dedupKey := computeRejectionDedupKey(params, queueLeaseEpoch)
	for _, existing := range rf.RejectedSubmissions {
		if existing.DedupKey == dedupKey {
			// Identical stale retry against the same authoritative queue
			// epoch — return the existing record's ID, don't grow the
			// audit log.
			return existing.ID, true, nil
		}
	}

	rejectionID, err := model.GenerateID(model.IDTypeResult)
	if err != nil {
		return "", false, fmt.Errorf("generate rejection id: %w", err)
	}

	// Defensive copy slices so a later mutation of params can't alias
	// into the persisted record.
	lostLearnings := append([]string(nil), params.Learnings...)
	lostSkills := append([]string(nil), params.SkillCandidates...)

	rf.RejectedSubmissions = append(rf.RejectedSubmissions, model.RejectedSubmission{
		ID:                  rejectionID,
		TaskID:              params.TaskID,
		CommandID:           params.CommandID,
		Reporter:            params.Reporter,
		Reason:              authoritativeReason,
		RequestLeaseEpoch:   params.LeaseEpoch,
		QueueLeaseEpoch:     queueLeaseEpoch,
		LostLearnings:       lostLearnings,
		LostSkillCandidates: lostSkills,
		OriginalSummary:     params.Summary,
		DedupKey:            dedupKey,
		CreatedAt:           h.clock.Now().UTC().Format(time.RFC3339),
	})

	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		return "", false, fmt.Errorf("write results file: %w", err)
	}
	h.recordSelfWrite(resultPath, rf)
	return rejectionID, true, nil
}

// computeRejectionDedupKey produces a deterministic content fingerprint of
// the rejected submission so that an identical stale-worker retry collapses
// to the existing record. Order of learnings/skill_candidates is preserved
// (matching how the worker submitted them).
func computeRejectionDedupKey(params ResultWriteParams, queueLeaseEpoch int) string {
	h := sha256.New()
	h.Write([]byte(params.TaskID))
	h.Write([]byte{0})
	h.Write([]byte(params.CommandID))
	h.Write([]byte{0})
	h.Write([]byte(params.Reporter))
	h.Write([]byte{0})
	_, _ = fmt.Fprintf(h, "%d", params.LeaseEpoch)
	h.Write([]byte{0})
	_, _ = fmt.Fprintf(h, "%d", queueLeaseEpoch)
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(params.Learnings, "\x01")))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(params.SkillCandidates, "\x01")))
	return hex.EncodeToString(h.Sum(nil))
}

// checkLeaseEpochForBestEffort performs a read-only lease epoch check against
// the queue file. Returns (skip=true, reason) only on definitive epoch mismatch.
// On I/O errors, parse errors, or task-not-found (task may have been archived
// after completion), returns (skip=false, "") to allow writes to proceed.
func (h *ResultWriteAPI) checkLeaseEpochForBestEffort(params ResultWriteParams) (skip bool, reason string) {
	queuePath := filepath.Join(h.maestroDir, "queue", params.Reporter+".yaml")
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

// writeLearnings appends learning entries to .maestro/state/learnings.yaml.
// Best-effort: errors are logged but do not fail the result_write.
func (h *ResultWriteAPI) writeLearnings(params ResultWriteParams, resultID string) error {
	// Lock order: leaf lock under the state:* namespace. See doc.go — must
	// be acquired in isolation (no state:{commandID} held). Phase B has
	// already released state:{commandID} by the time this is called.
	h.lockMap.Lock("state:learnings")
	defer h.lockMap.Unlock("state:learnings")

	learningsPath := filepath.Join(h.maestroDir, "state", "learnings.yaml")
	maxEntries := h.config.Learnings.EffectiveMaxEntries()
	maxLen := h.config.Learnings.EffectiveMaxContentLength()

	// Load existing file
	var lf model.LearningsFile
	data, err := os.ReadFile(learningsPath) //nolint:gosec // learningsPath is constructed from a controlled application state directory
	if err == nil {
		if err := yamlv3.Unmarshal(data, &lf); err != nil {
			// Corrupt file — recover via quarantine
			h.logFn(LogLevelWarn, "learnings_file_corrupt, recovering: %v", err)
			if recErr := yamlutil.RecoverCorruptedFile(h.maestroDir, learningsPath, "state_learnings"); recErr != nil {
				return fmt.Errorf("recover learnings file: %w", recErr)
			}
			// Re-read the recovered file (may have been restored from .bak)
			if recovered, readErr := os.ReadFile(learningsPath); readErr == nil { //nolint:gosec // learningsPath is constructed from a controlled application state directory
				if parseErr := yamlv3.Unmarshal(recovered, &lf); parseErr != nil {
					// Recovery produced an unreadable file — start fresh
					lf = model.LearningsFile{SchemaVersion: 1, FileType: "state_learnings"}
				}
			} else {
				lf = model.LearningsFile{SchemaVersion: 1, FileType: "state_learnings"}
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read learnings file: %w", err)
	}

	if lf.SchemaVersion == 0 {
		lf.SchemaVersion = 1
		lf.FileType = "state_learnings"
	}

	// Build dedup set: result_id + content
	type dedupKey struct {
		resultID string
		content  string
	}
	existing := make(map[dedupKey]bool, len(lf.Learnings))
	for _, l := range lf.Learnings {
		existing[dedupKey{l.ResultID, l.Content}] = true
	}

	now := h.clock.Now().UTC().Format(time.RFC3339)
	added := 0
	for _, content := range params.Learnings {
		if content == "" {
			continue
		}
		// Truncate content at max length (rune-safe)
		truncated := truncateRunes(content, maxLen)
		key := dedupKey{resultID, truncated}
		if existing[key] {
			continue
		}
		existing[key] = true
		lf.Learnings = append(lf.Learnings, model.Learning{
			ResultID:     resultID,
			CommandID:    params.CommandID,
			Content:      truncated,
			CreatedAt:    now,
			SourceWorker: params.Reporter,
		})
		added++
	}

	if added == 0 {
		return nil
	}

	// FIFO eviction
	if len(lf.Learnings) > maxEntries {
		lf.Learnings = lf.Learnings[len(lf.Learnings)-maxEntries:]
	}

	if err := yamlutil.AtomicWrite(learningsPath, lf); err != nil {
		return fmt.Errorf("write learnings file: %w", err)
	}

	h.logFn(LogLevelInfo, "learnings_written result=%s added=%d total=%d", resultID, added, len(lf.Learnings))
	return nil
}

// truncateRunes truncates a string to at most maxRunes runes.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}

// writeSkillCandidates merges skill candidate entries into .maestro/state/skill_candidates.yaml.
// Best-effort: errors are logged but do not fail the result_write.
func (h *ResultWriteAPI) writeSkillCandidates(params ResultWriteParams) error {
	// Lock order: leaf lock under the state:* namespace. See doc.go — must
	// be acquired in isolation (no state:{commandID} held). Phase B has
	// already released state:{commandID} by the time this is called.
	h.lockMap.Lock("state:skill_candidates")
	defer h.lockMap.Unlock("state:skill_candidates")

	candidatesPath := filepath.Join(h.maestroDir, "state", "skill_candidates.yaml")
	now := h.clock.Now().UTC().Format(time.RFC3339)

	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		return fmt.Errorf("read skill candidates: %w", err)
	}

	idFunc := func() (string, error) {
		return model.GenerateID(model.IDTypeSkillCandidate)
	}

	added := 0
	for _, content := range params.SkillCandidates {
		if content == "" {
			continue
		}
		before := len(candidates)
		candidates, err = skill.AddOrUpdateCandidate(candidates, content, params.CommandID, now, idFunc)
		if err != nil {
			h.logFn(LogLevelError, "skill_candidate_add_failed content=%q: %v", content, err)
			continue
		}
		if len(candidates) > before {
			added++
		}
	}

	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		return fmt.Errorf("write skill candidates: %w", err)
	}

	h.logFn(LogLevelInfo, "skill_candidates_written command=%s added=%d total=%d", params.CommandID, added, len(candidates))
	return nil
}

