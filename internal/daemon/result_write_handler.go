package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

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
}

func (d *Daemon) handleResultWrite(req *uds.Request) *uds.Response {
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
	resultID, err := d.resultWritePhaseA(params, resultStatus)
	if err != nil {
		rErr := &resultWriteError{}
		if errors.As(err, &rErr) {
			return uds.ErrorResponse(rErr.Code, rErr.Message)
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}

	// Phase B: Per-command mutex (state/ updates)
	if err := d.resultWritePhaseB(params, resultID, resultStatus); err != nil {
		d.log(LogLevelError, "result_write phase_b error task=%s command=%s: %v",
			params.TaskID, params.CommandID, err)
		return uds.ErrorResponse(uds.ErrCodeInternal,
			fmt.Sprintf("state update failed: %v (result %s committed, run 'maestro plan rebuild' to fix)", err, resultID))
	}

	// Learnings: best-effort write after core phases succeed.
	if len(params.Learnings) > 0 && d.config.Learnings.Enabled {
		if err := d.writeLearnings(params, resultID); err != nil {
			d.log(LogLevelError, "learnings_write_failed result=%s: %v", resultID, err)
		}
	}

	// Phase C: Trigger scan (best effort dependency unblocking).
	// Use shutdownMu read lock to atomically check shuttingDown + wg.Add,
	// preventing TOCTOU race where Shutdown sets the flag and calls wg.Wait
	// between our check and wg.Add.
	if d.handler != nil {
		d.shutdownMu.RLock()
		if !d.shuttingDown.Load() {
			d.wg.Add(1)
			d.shutdownMu.RUnlock()
			go func() {
				defer d.wg.Done()
				defer d.recoverPanic("resultWriteScan")
				d.handler.PeriodicScan()
			}()
		} else {
			d.shutdownMu.RUnlock()
		}
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

func (d *Daemon) resultWritePhaseA(params ResultWriteParams, resultStatus model.Status) (string, error) {
	// Acquire shared file lock to serialize with QueueHandler's PeriodicScan
	d.acquireFileLock()
	defer d.releaseFileLock()

	// Lock queue file first (consistent order: queue → result → state).
	// Without this, handleQueueWriteTask (which locks "queue:{target}") and
	// resultWritePhaseA (which writes queue/{reporter}.yaml) can race.
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
			return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("parse results file: %v", err)}
		}
	} else if !os.IsNotExist(err) {
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("read results file: %v", err)}
	}

	for _, r := range rf.Results {
		if r.TaskID == params.TaskID {
			if r.Status == resultStatus {
				// Same status — idempotent success
				return r.ID, nil
			}
			// Different status — anomaly
			return "", &resultWriteError{uds.ErrCodeDuplicate,
				fmt.Sprintf("task %s already has result with status %s, cannot report %s",
					params.TaskID, r.Status, resultStatus)}
		}
	}

	// 2. Fencing verification
	queuePath := filepath.Join(d.maestroDir, "queue", params.Reporter+".yaml")
	var tq model.TaskQueue
	queueData, err := os.ReadFile(queuePath)
	if err != nil {
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("read worker queue: %v", err)}
	}
	if err := yamlv3.Unmarshal(queueData, &tq); err != nil {
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("parse worker queue: %v", err)}
	}

	taskIdx := -1
	for i, task := range tq.Tasks {
		if task.ID == params.TaskID {
			taskIdx = i
			break
		}
	}
	if taskIdx == -1 {
		return "", &resultWriteError{uds.ErrCodeNotFound,
			fmt.Sprintf("task %s not found in queue %s", params.TaskID, params.Reporter)}
	}

	queueTask := &tq.Tasks[taskIdx]

	// Command ID consistency check: queue task's command_id must match request
	if queueTask.CommandID != params.CommandID {
		return "", &resultWriteError{uds.ErrCodeValidation,
			fmt.Sprintf("command_id mismatch: queue task has %q, request has %q",
				queueTask.CommandID, params.CommandID)}
	}

	// If task is already terminal with same status, treat as idempotent success
	if model.IsTerminal(queueTask.Status) {
		if queueTask.Status == resultStatus {
			for _, r := range rf.Results {
				if r.TaskID == params.TaskID {
					return r.ID, nil
				}
			}
			// Terminal in queue but no result entry — proceed to write result
		} else {
			return "", &resultWriteError{uds.ErrCodeDuplicate,
				fmt.Sprintf("task %s already terminal with status %s in queue", params.TaskID, queueTask.Status)}
		}
	}

	// Fencing: task must be in_progress
	if queueTask.Status != model.StatusInProgress {
		return "", &resultWriteError{uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s status is %s, expected in_progress", params.TaskID, queueTask.Status)}
	}

	// Fencing: lease epoch must match
	if queueTask.LeaseEpoch != params.LeaseEpoch {
		return "", &resultWriteError{uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s lease_epoch mismatch: queue=%d, request=%d",
				params.TaskID, queueTask.LeaseEpoch, params.LeaseEpoch)}
	}

	// Fencing: lease must be held. The task is already looked up from queue/{reporter}.yaml
	// and lease_epoch matches, so the reporter identity is verified. lease_owner stores
	// "daemon:{pid}" per spec §5.8.1, not the agent ID.
	if queueTask.LeaseOwner == nil {
		return "", &resultWriteError{uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s has no lease_owner (not dispatched)", params.TaskID)}
	}

	// 2b. Validate state existence and task registration (mandatory)
	statePath := filepath.Join(d.maestroDir, "state", "commands", params.CommandID+".yaml")
	stateData, stateErr := os.ReadFile(statePath)
	if stateErr != nil {
		if os.IsNotExist(stateErr) {
			return "", &resultWriteError{uds.ErrCodeValidation,
				fmt.Sprintf("state not found for command %s", params.CommandID)}
		}
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("read state: %v", stateErr)}
	}
	var preState model.CommandState
	if err := yamlv3.Unmarshal(stateData, &preState); err != nil {
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("parse state: %v", err)}
	}
	if preState.TaskStates == nil {
		return "", &resultWriteError{uds.ErrCodeValidation,
			fmt.Sprintf("task %s not registered in state for command %s (no tasks registered)",
				params.TaskID, params.CommandID)}
	}
	if _, registered := preState.TaskStates[params.TaskID]; !registered {
		return "", &resultWriteError{uds.ErrCodeValidation,
			fmt.Sprintf("task %s not registered in state for command %s",
				params.TaskID, params.CommandID)}
	}

	// 3. Generate result ID
	resultID, err := model.GenerateID(model.IDTypeResult)
	if err != nil {
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("generate result ID: %v", err)}
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
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("write results file: %v", err)}
	}

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

	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("write worker queue: %v", err)}
	}

	// 7. Add retry task to queue and state AFTER successful queue write
	if retryTask != nil {
		retryHandler := NewTaskRetryHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)

		// First register in state
		if err := retryHandler.RegisterRetryTaskInState(retryTask, params.CommandID); err != nil {
			d.log(LogLevelError, "register_retry_task_failed task=%s error=%v", retryTask.ID, err)
		} else {
			// Then add to queue (use locked variant — queue lock already held by this function)
			if err := retryHandler.addRetryTaskToQueueLocked(retryTask, params.Reporter); err != nil {
				d.log(LogLevelError, "add_retry_task_failed task=%s error=%v", retryTask.ID, err)
			} else {
				d.log(LogLevelInfo, "task_retry_scheduled task=%s retry_id=%s attempt=%d",
					params.TaskID, retryTask.ID, retryTask.Attempts)
			}
		}
	}

	return resultID, nil
}

func (d *Daemon) resultWritePhaseB(params ResultWriteParams, resultID string, resultStatus model.Status) error {
	cmdLockKey := "state:" + params.CommandID
	d.lockMap.Lock(cmdLockKey)
	defer d.lockMap.Unlock(cmdLockKey)

	statePath := filepath.Join(d.maestroDir, "state", "commands", params.CommandID+".yaml")
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("state not found for command %s", params.CommandID)
		}
		return fmt.Errorf("read state: %w", err)
	}

	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse state: %w", err)
	}

	if state.TaskStates == nil {
		state.TaskStates = make(map[string]model.Status)
	}
	state.TaskStates[params.TaskID] = resultStatus

	now := d.clock.Now()
	state.UpdatedAt = now.UTC().Format(time.RFC3339)

	// Circuit breaker: update counter BEFORE AppliedResultIDs so the idempotency
	// check correctly detects duplicate results against the old map.
	if d.circuitBreaker != nil {
		tripped, reason := d.circuitBreaker.UpdateCounterOnResult(&state, resultStatus, resultID, now)
		if tripped {
			d.circuitBreaker.TripBreaker(&state, reason, now)
		}
	}

	if state.AppliedResultIDs == nil {
		state.AppliedResultIDs = make(map[string]string)
	}
	state.AppliedResultIDs[params.TaskID] = resultID

	return yamlutil.AtomicWrite(statePath, state)
}

// writeLearnings appends learning entries to .maestro/state/learnings.yaml.
// Best-effort: errors are logged but do not fail the result_write.
func (d *Daemon) writeLearnings(params ResultWriteParams, resultID string) error {
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
			ResultID:  resultID,
			CommandID: params.CommandID,
			Content:   truncated,
			CreatedAt: now,
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
