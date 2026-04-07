package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

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

func (a *API) handleResultWrite(req *uds.Request) *uds.Response {
	d := a.d
	var params ResultWriteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	if params.Reporter == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "reporter is required")
	}
	if filepath.Base(params.Reporter) != params.Reporter || params.Reporter == "." || params.Reporter == ".." {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid reporter: %q", params.Reporter))
	}
	if params.TaskID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "task_id is required")
	}
	if params.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required")
	}
	if err := validate.ValidateID(params.CommandID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid command_id: %v", err))
	}
	if err := validate.ValidateID(params.TaskID); err != nil {
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
	resultWritePhaseAResult, err := a.resultWritePhaseA(params, resultStatus)
	if err != nil {
		rErr := &resultWriteError{}
		if errors.As(err, &rErr) {
			return uds.ErrorResponse(rErr.Code, rErr.Message)
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}
	resultID := resultWritePhaseAResult.resultID

	// Phase B: Per-command mutex (state/ updates)
	if err := a.resultWritePhaseB(params, resultID, resultStatus, resultWritePhaseAResult.queueWriteFailed); err != nil {
		d.log(LogLevelError, "result_write phase_b error task=%s command=%s: %v",
			params.TaskID, params.CommandID, err)
		return uds.ErrorResponse(uds.ErrCodeInternal,
			fmt.Sprintf("state update failed: %v (result %s committed, run 'maestro plan rebuild' to fix)", err, resultID))
	}

	// Phase A2: Retry registration (state then queue — correct lock order)
	// This runs after phaseA has released queue+result locks, so acquiring
	// state(L2) then queue(L1) does not violate canonical order.
	if resultWritePhaseAResult.retryTask != nil {
		retryTask := resultWritePhaseAResult.retryTask
		retryHandler := NewTaskRetryHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)

		// First register in state (acquires state lock)
		if err := retryHandler.RegisterRetryTaskInState(retryTask, params.CommandID); err != nil {
			d.log(LogLevelError, "register_retry_task_failed task=%s command=%s error=%v", retryTask.ID, params.CommandID, err)
		} else {
			// Then add to queue (acquires queue lock independently)
			if err := retryHandler.AddRetryTaskToQueue(retryTask, params.Reporter); err != nil {
				// M2 fix: Mark failed enqueue in state so R1 reconciler can detect orphaned retry tasks.
				// The task is registered in state as pending but has no queue entry.
				d.log(LogLevelError, "add_retry_task_failed task=%s worker=%s command=%s error=%v "+
					"(task registered in state but not enqueued; R1 reconciler should re-enqueue or mark dead_letter)",
					retryTask.ID, params.Reporter, params.CommandID, err)
				if markErr := retryHandler.MarkRetryEnqueueFailed(retryTask.ID, params.Reporter, params.CommandID); markErr != nil {
					d.log(LogLevelError, "mark_retry_enqueue_failed task=%s command=%s error=%v",
						retryTask.ID, params.CommandID, markErr)
				}
			} else {
				d.log(LogLevelInfo, "task_retry_scheduled task=%s retry_id=%s attempt=%d",
					params.TaskID, retryTask.ID, retryTask.Attempts)
			}
		}
	}

	// Best-effort lease epoch check: skip learnings/skill_candidates writes
	// if the lease epoch no longer matches (stale worker after revocation).
	// Read-only check without lock is acceptable — this is advisory, not authoritative.
	bestEffortAllowed := true
	if len(params.Learnings) > 0 || len(params.SkillCandidates) > 0 {
		if skip, reason := a.checkLeaseEpochForBestEffort(params); skip {
			d.log(LogLevelWarn, "best_effort_writes_skipped task=%s command=%s reason=%s",
				params.TaskID, params.CommandID, reason)
			bestEffortAllowed = false
		}
	}

	// Learnings: best-effort write after core phases succeed.
	if bestEffortAllowed && len(params.Learnings) > 0 && d.config.Learnings.Enabled {
		learningsPath := filepath.Join(d.maestroDir, "state", "learnings.yaml")
		if err := a.writeLearnings(params, resultID); err != nil {
			d.log(LogLevelWarn, "learnings_write_failed result=%s task=%s command=%s path=%s count=%d error=%v "+
				"(learnings data lost; core result already committed; manual recovery: re-submit result with same learnings)",
				resultID, params.TaskID, params.CommandID, learningsPath, len(params.Learnings), err)
		}
	}

	// Skill candidates: best-effort write after core phases succeed.
	if bestEffortAllowed && len(params.SkillCandidates) > 0 {
		candidatesPath := filepath.Join(d.maestroDir, "state", "skill_candidates.yaml")
		if err := a.writeSkillCandidates(params); err != nil {
			d.log(LogLevelWarn, "skill_candidates_write_failed result=%s task=%s command=%s path=%s count=%d error=%v "+
				"(skill candidates lost; core result already committed; manual recovery: re-submit result with same skill_candidates)",
				resultID, params.TaskID, params.CommandID, candidatesPath, len(params.SkillCandidates), err)
		}
	}

	// Phase C: Trigger scan (best effort dependency unblocking).
	// spawnTracked atomically checks shuttingDown and admits the goroutine to
	// the errgroup under egMu, closing the race window where Shutdown could
	// otherwise call eg.Wait() between this caller's check and the eg.Go()
	// admission. See Daemon.spawnTracked / Shutdown for the synchronization
	// contract.
	if d.handler != nil {
		d.spawnTracked("resultWriteScan", func(ctx context.Context) {
			if d.eg != nil {
				d.handler.PeriodicScanWithContext(ctx)
			} else {
				// Test/fallback path (Run() not called): preserve historical
				// behaviour of invoking the no-context variant.
				d.handler.PeriodicScan()
			}
		})
	}

	d.log(LogLevelInfo, "result_write result_id=%s task=%s command=%s status=%s reporter=%s",
		resultID, params.TaskID, params.CommandID, params.Status, params.Reporter)
	return uds.SuccessResponse(map[string]string{"result_id": resultID})
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

func (a *API) resultWritePhaseA(params ResultWriteParams, resultStatus model.Status) (*resultWritePhaseAResult, error) {
	d := a.d
	// Acquire shared file lock to serialize with QueueHandler's PeriodicScan
	a.acquireFileLock()
	defer a.releaseFileLock()

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
	d.lockMap.Lock(queueLockKey)
	defer d.lockMap.Unlock(queueLockKey)

	workerLockKey := "result:" + params.Reporter
	d.lockMap.Lock(workerLockKey)
	defer d.lockMap.Unlock(workerLockKey)

	// 1. Load result file and check idempotency
	resultPath := filepath.Join(d.maestroDir, "results", params.Reporter+".yaml")
	var rf model.TaskResultFile
	resultData, err := os.ReadFile(resultPath)
	if err == nil {
		if err := yamlv3.Unmarshal(resultData, &rf); err != nil {
			return nil, &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("parse results file: %v", err)}
		}
	} else if !os.IsNotExist(err) {
		return nil, &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("read results file: %v", err)}
	}

	for _, r := range rf.Results {
		if r.TaskID == params.TaskID {
			if r.Status == resultStatus {
				// Same status — idempotent success
				return &resultWritePhaseAResult{resultID: r.ID}, nil
			}
			// Different status — anomaly
			return nil, &resultWriteError{uds.ErrCodeDuplicate,
				fmt.Sprintf("task %s already has result with status %s, cannot report %s",
					params.TaskID, r.Status, resultStatus)}
		}
	}

	// 2. Fencing verification
	queuePath := filepath.Join(d.maestroDir, "queue", params.Reporter+".yaml")
	var tq model.TaskQueue
	queueData, err := os.ReadFile(queuePath)
	if err != nil {
		return nil, &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("read worker queue: %v", err)}
	}
	if err := yamlv3.Unmarshal(queueData, &tq); err != nil {
		return nil, &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("parse worker queue: %v", err)}
	}

	taskIdx := -1
	for i, task := range tq.Tasks {
		if task.ID == params.TaskID {
			taskIdx = i
			break
		}
	}
	if taskIdx == -1 {
		return nil, &resultWriteError{uds.ErrCodeNotFound,
			fmt.Sprintf("task %s not found in queue %s", params.TaskID, params.Reporter)}
	}

	queueTask := &tq.Tasks[taskIdx]

	// Command ID consistency check: queue task's command_id must match request
	if queueTask.CommandID != params.CommandID {
		return nil, &resultWriteError{uds.ErrCodeValidation,
			fmt.Sprintf("command_id mismatch: queue task has %q, request has %q",
				queueTask.CommandID, params.CommandID)}
	}

	// If task is already terminal with same status, treat as idempotent success
	if model.IsTerminal(queueTask.Status) {
		if queueTask.Status == resultStatus {
			for _, r := range rf.Results {
				if r.TaskID == params.TaskID {
					return &resultWritePhaseAResult{resultID: r.ID}, nil
				}
			}
			// Terminal in queue but no result entry — proceed to write result
		} else {
			return nil, &resultWriteError{uds.ErrCodeDuplicate,
				fmt.Sprintf("task %s already terminal with status %s in queue", params.TaskID, queueTask.Status)}
		}
	}

	// Fencing: task must be in_progress
	if queueTask.Status != model.StatusInProgress {
		return nil, &resultWriteError{uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s status is %s, expected in_progress", params.TaskID, queueTask.Status)}
	}

	// Fencing: lease epoch must match
	if queueTask.LeaseEpoch != params.LeaseEpoch {
		return nil, &resultWriteError{uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s lease_epoch mismatch: queue=%d, request=%d",
				params.TaskID, queueTask.LeaseEpoch, params.LeaseEpoch)}
	}

	// Fencing: lease must be held. The task is already looked up from queue/{reporter}.yaml
	// and lease_epoch matches, so the reporter identity is verified. lease_owner stores
	// "daemon:{pid}" per spec §5.8.1, not the agent ID.
	if queueTask.LeaseOwner == nil {
		return nil, &resultWriteError{uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s has no lease_owner (not dispatched)", params.TaskID)}
	}

	// 2b. Validate state existence and task registration (mandatory)
	statePath := filepath.Join(d.maestroDir, "state", "commands", params.CommandID+".yaml")
	stateData, stateErr := os.ReadFile(statePath)
	if stateErr != nil {
		if os.IsNotExist(stateErr) {
			return nil, &resultWriteError{uds.ErrCodeValidation,
				fmt.Sprintf("state not found for command %s", params.CommandID)}
		}
		return nil, &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("read state: %v", stateErr)}
	}
	var preState model.CommandState
	if err := yamlv3.Unmarshal(stateData, &preState); err != nil {
		return nil, &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("parse state: %v", err)}
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

	// 3. Generate result ID
	resultID, err := model.GenerateID(model.IDTypeResult)
	if err != nil {
		return nil, &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("generate result ID: %v", err)}
	}

	// 4. Append to results file
	if rf.SchemaVersion == 0 {
		rf.SchemaVersion = 1
		rf.FileType = "result_task"
	}

	now := d.clock.Now().UTC().Format(time.RFC3339)
	rf.Results = append(rf.Results, model.TaskResult{
		ID:                     resultID,
		TaskID:                 params.TaskID,
		CommandID:              params.CommandID,
		Status:                 resultStatus,
		Summary:                params.Summary,
		FilesChanged:           params.FilesChanged,
		PartialChangesPossible: params.PartialChangesPossible,
		RetrySafe:              params.RetrySafe,
		Notified:               false,
		CreatedAt:              now,
	})

	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		return nil, &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("write results file: %v", err)}
	}
	a.recordSelfWrite(resultPath, rf)

	// 5. Check for retry if task failed (but don't schedule yet)
	var retryTask *model.Task
	if resultStatus == model.StatusFailed && params.ExitCode != nil {
		retryHandler := NewTaskRetryHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)
		shouldRetry, reason := retryHandler.ShouldRetryTask(queueTask, *params.ExitCode, params.RetrySafe)

		if shouldRetry {
			// Create retry task
			rt, err := retryHandler.CreateRetryTask(queueTask, params.Reporter, *params.ExitCode)
			if err != nil {
				d.log(LogLevelError, "create_retry_task_failed task=%s error=%v", params.TaskID, err)
			} else {
				retryTask = rt
				// Don't add to queue yet - wait until after queue write succeeds
			}
		} else {
			d.log(LogLevelInfo, "task_retry_skipped task=%s reason=%s", params.TaskID, reason)
		}
	}

	// 6. Update queue entry to terminal
	queueTask.Status = resultStatus
	queueTask.LeaseOwner = nil
	queueTask.LeaseExpiresAt = nil
	queueTask.UpdatedAt = now

	queueWriteFailed := false
	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		// Result is already committed — retry queue write once before giving up.
		// If still failing, persist a sticky error in command state (H2) so the
		// R1 reconciler can repair the result-terminal/queue-in_progress mismatch
		// durably (across daemon restarts) instead of relying on the next
		// periodic scan reading the bare log line.
		d.log(LogLevelWarn, "result_write queue_write_failed task=%s, retrying: %v", params.TaskID, err)
		if retryErr := yamlutil.AtomicWrite(queuePath, tq); retryErr != nil {
			d.log(LogLevelError, "result_write queue_write_retry_failed task=%s result=%s: %v (sticky error recorded; R1 reconciler will repair)",
				params.TaskID, resultID, retryErr)
			queueWriteFailed = true
		} else {
			a.recordSelfWrite(queuePath, tq)
		}
	} else {
		a.recordSelfWrite(queuePath, tq)
	}

	return &resultWritePhaseAResult{resultID: resultID, retryTask: retryTask, queueWriteFailed: queueWriteFailed}, nil
}

func (a *API) resultWritePhaseB(params ResultWriteParams, resultID string, resultStatus model.Status, queueWriteFailed bool) error {
	d := a.d
	cmdLockKey := "state:" + params.CommandID
	d.lockMap.Lock(cmdLockKey)
	defer d.lockMap.Unlock(cmdLockKey)

	statePath := filepath.Join(d.maestroDir, "state", "commands", params.CommandID+".yaml")

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
		if planFailedOrCancelled && !(hadExisting && model.IsTerminal(existing)) {
			d.log(LogLevelWarn,
				"result_write late_after_plan_terminal task=%s command=%s plan_status=%s reported=%s coerced=cancelled",
				params.TaskID, params.CommandID, state.PlanStatus, resultStatus)
			recordedStatus = model.StatusCancelled
		} else if hadExisting && model.IsTerminal(existing) {
			// Idempotent re-submission: keep settled terminal state.
			recordedStatus = existing
		}
		state.TaskStates[params.TaskID] = recordedStatus

		now := d.clock.Now()
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
				d.log(LogLevelWarn,
					"result_write applied_result_ids drift task=%s command=%s recorded=%s incoming=%s (skipping CB counter update)",
					params.TaskID, params.CommandID, recordedID, resultID)
			}
		}

		if !alreadyApplied && d.circuitBreaker != nil {
			tripped, reason := d.circuitBreaker.UpdateCounterOnResult(state, resultStatus, resultID, now)
			if tripped {
				d.circuitBreaker.TripBreaker(state, reason, now)
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
			d.log(LogLevelWarn,
				"result_write queue_write_failed_sticky task=%s command=%s worker=%s result=%s",
				params.TaskID, params.CommandID, params.Reporter, resultID)
		}

		return nil
	})
}

// checkLeaseEpochForBestEffort performs a read-only lease epoch check against
// the queue file. Returns (skip=true, reason) only on definitive epoch mismatch.
// On I/O errors, parse errors, or task-not-found (task may have been archived
// after completion), returns (skip=false, "") to allow writes to proceed.
func (a *API) checkLeaseEpochForBestEffort(params ResultWriteParams) (skip bool, reason string) {
	d := a.d
	queuePath := filepath.Join(d.maestroDir, "queue", params.Reporter+".yaml")
	data, err := os.ReadFile(queuePath)
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
func (a *API) writeLearnings(params ResultWriteParams, resultID string) error {
	d := a.d
	// Lock order: leaf lock under the state:* namespace. See doc.go — must
	// be acquired in isolation (no state:{commandID} held). Phase B has
	// already released state:{commandID} by the time this is called.
	d.lockMap.Lock("state:learnings")
	defer d.lockMap.Unlock("state:learnings")

	learningsPath := filepath.Join(d.maestroDir, "state", "learnings.yaml")
	maxEntries := d.config.Learnings.EffectiveMaxEntries()
	maxLen := d.config.Learnings.EffectiveMaxContentLength()

	// Load existing file
	var lf model.LearningsFile
	data, err := os.ReadFile(learningsPath)
	if err == nil {
		if err := yamlv3.Unmarshal(data, &lf); err != nil {
			// Corrupt file — recover via quarantine
			d.log(LogLevelWarn, "learnings_file_corrupt, recovering: %v", err)
			if recErr := yamlutil.RecoverCorruptedFile(d.maestroDir, learningsPath, "state_learnings"); recErr != nil {
				return fmt.Errorf("recover learnings file: %w", recErr)
			}
			// Re-read the recovered file (may have been restored from .bak)
			if recovered, readErr := os.ReadFile(learningsPath); readErr == nil {
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

	now := d.clock.Now().UTC().Format(time.RFC3339)
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

	d.log(LogLevelInfo, "learnings_written result=%s added=%d total=%d", resultID, added, len(lf.Learnings))
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
func (a *API) writeSkillCandidates(params ResultWriteParams) error {
	d := a.d
	// Lock order: leaf lock under the state:* namespace. See doc.go — must
	// be acquired in isolation (no state:{commandID} held). Phase B has
	// already released state:{commandID} by the time this is called.
	d.lockMap.Lock("state:skill_candidates")
	defer d.lockMap.Unlock("state:skill_candidates")

	candidatesPath := filepath.Join(d.maestroDir, "state", "skill_candidates.yaml")
	now := d.clock.Now().UTC().Format(time.RFC3339)

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
			d.log(LogLevelError, "skill_candidate_add_failed content=%q: %v", content, err)
			continue
		}
		if len(candidates) > before {
			added++
		}
	}

	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		return fmt.Errorf("write skill candidates: %w", err)
	}

	d.log(LogLevelInfo, "skill_candidates_written command=%s added=%d total=%d", params.CommandID, added, len(candidates))
	return nil
}
