package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

type CompleteOptions struct {
	CommandID  string
	Summary    string
	MaestroDir string
	Config     model.Config
	LockMap    *lock.MutexMap
}

type CompleteResult struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`
}

func Complete(opts CompleteOptions) (*CompleteResult, error) {
	if opts.LockMap == nil {
		return nil, fmt.Errorf("LockMap is required")
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	// Idempotency: if already completed/failed/cancelled, return existing status
	if state.PlanStatus == model.PlanStatusCompleted || state.PlanStatus == model.PlanStatusFailed || state.PlanStatus == model.PlanStatusCancelled {
		return &CompleteResult{
			CommandID: opts.CommandID,
			Status:    string(state.PlanStatus),
		}, nil
	}

	// can-complete validation
	derivedPlanStatus, err := CanComplete(state)
	if err != nil {
		return nil, err
	}

	// Map PlanStatus to Status for result
	var resultStatus model.Status
	switch derivedPlanStatus {
	case model.PlanStatusCompleted:
		resultStatus = model.StatusCompleted
	case model.PlanStatusFailed:
		resultStatus = model.StatusFailed
	case model.PlanStatusCancelled:
		resultStatus = model.StatusCancelled
	default:
		return nil, fmt.Errorf("unexpected derived status: %s", derivedPlanStatus)
	}

	// Aggregate task results from results/worker{N}.yaml
	taskResults, err := aggregateTaskResults(opts.MaestroDir, opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("aggregate results: %w", err)
	}

	// Write to results/planner.yaml
	if err := writeCommandResult(opts.MaestroDir, opts.CommandID, resultStatus, opts.Summary, taskResults); err != nil {
		return nil, fmt.Errorf("write command result: %w", err)
	}

	// Update queue/planner.yaml command entry
	if err := updateCommandQueueEntry(opts.MaestroDir, opts.CommandID, resultStatus); err != nil {
		return nil, fmt.Errorf("update command queue: %w", err)
	}

	// Update state plan_status
	state.PlanStatus = derivedPlanStatus
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := sm.SaveState(state); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	return &CompleteResult{
		CommandID: opts.CommandID,
		Status:    string(derivedPlanStatus),
	}, nil
}

func aggregateTaskResults(maestroDir string, commandID string) ([]model.CommandResultTask, error) {
	resultsDir := filepath.Join(maestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return nil, fmt.Errorf("read results dir: %w", err)
	}

	var taskResults []model.CommandResultTask

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := strings.TrimSuffix(name, ".yaml")
		path := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			continue
		}

		for _, r := range rf.Results {
			if r.CommandID == commandID {
				taskResults = append(taskResults, model.CommandResultTask{
					TaskID:  r.TaskID,
					Worker:  workerID,
					Status:  r.Status,
					Summary: r.Summary,
				})
			}
		}
	}

	return taskResults, nil
}

func writeCommandResult(maestroDir string, commandID string, status model.Status, summary string, tasks []model.CommandResultTask) error {
	path := filepath.Join(maestroDir, "results", "planner.yaml")

	var rf model.CommandResultFile
	data, err := os.ReadFile(path)
	if err == nil {
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			return fmt.Errorf("parse existing result file: %w", err)
		}
	}
	if rf.SchemaVersion == 0 {
		rf.SchemaVersion = 1
		rf.FileType = "result_command"
	}

	resultID, err := model.GenerateID(model.IDTypeResult)
	if err != nil {
		return fmt.Errorf("generate result ID: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rf.Results = append(rf.Results, model.CommandResult{
		ID:        resultID,
		CommandID: commandID,
		Status:    status,
		Summary:   summary,
		Tasks:     tasks,
		Notified:  false,
		CreatedAt: now,
	})

	return yamlutil.AtomicWrite(path, rf)
}

func updateCommandQueueEntry(maestroDir string, commandID string, status model.Status) error {
	path := filepath.Join(maestroDir, "queue", "planner.yaml")

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read planner queue: %w", err)
	}

	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return fmt.Errorf("parse planner queue: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	found := false
	for i := range cq.Commands {
		if cq.Commands[i].ID == commandID {
			cq.Commands[i].Status = status
			cq.Commands[i].LeaseOwner = nil
			cq.Commands[i].LeaseExpiresAt = nil
			cq.Commands[i].UpdatedAt = now
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("command %s not found in planner queue", commandID)
	}

	return yamlutil.AtomicWrite(path, cq)
}
