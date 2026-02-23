package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
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
}

func (d *Daemon) handleResultWrite(req *uds.Request) *uds.Response {
	var params ResultWriteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	if params.Reporter == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "reporter is required")
	}
	if params.TaskID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "task_id is required")
	}
	if params.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required")
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
		if rErr, ok := err.(*resultWriteError); ok {
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

	// Phase C: Trigger scan (best effort dependency unblocking)
	if d.handler != nil {
		go d.handler.PeriodicScan()
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

	// Fencing: lease owner must match reporter
	if queueTask.LeaseOwner == nil || *queueTask.LeaseOwner != params.Reporter {
		owner := "<nil>"
		if queueTask.LeaseOwner != nil {
			owner = *queueTask.LeaseOwner
		}
		return "", &resultWriteError{uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s lease_owner mismatch: queue=%s, reporter=%s",
				params.TaskID, owner, params.Reporter)}
	}

	// 2b. Validate task is registered in state (if state file exists)
	statePath := filepath.Join(d.maestroDir, "state", "commands", params.CommandID+".yaml")
	if stateData, err := os.ReadFile(statePath); err == nil {
		var state model.CommandState
		if err := yamlv3.Unmarshal(stateData, &state); err == nil {
			if state.TaskStates != nil {
				if _, registered := state.TaskStates[params.TaskID]; !registered {
					return "", &resultWriteError{uds.ErrCodeValidation,
						fmt.Sprintf("task %s not registered in state for command %s",
							params.TaskID, params.CommandID)}
				}
			}
		}
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

	now := time.Now().UTC().Format(time.RFC3339)
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

	// 5. Update queue entry to terminal
	queueTask.Status = resultStatus
	queueTask.LeaseOwner = nil
	queueTask.LeaseExpiresAt = nil
	queueTask.UpdatedAt = now

	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		return "", &resultWriteError{uds.ErrCodeInternal, fmt.Sprintf("write worker queue: %v", err)}
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

	if state.AppliedResultIDs == nil {
		state.AppliedResultIDs = make(map[string]string)
	}
	state.AppliedResultIDs[params.TaskID] = resultID

	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	return yamlutil.AtomicWrite(statePath, state)
}
