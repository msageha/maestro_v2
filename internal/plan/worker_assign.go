package plan

import (
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

type WorkerAssignment struct {
	TaskName string
	WorkerID string
	Model    string
}

type WorkerState struct {
	WorkerID     string
	Model        string
	PendingCount int
}

type TaskAssignmentRequest struct {
	Name       string
	BloomLevel int
}

func GetModelForBloomLevel(bloomLevel int, boost bool) string {
	if boost {
		return "opus"
	}
	if bloomLevel >= 1 && bloomLevel <= 3 {
		return "sonnet"
	}
	return "opus"
}

func GetWorkerModel(workerID string, config model.WorkerConfig) string {
	if config.Boost {
		return "opus"
	}
	if m, ok := config.Models[workerID]; ok {
		return m
	}
	if config.DefaultModel != "" {
		return config.DefaultModel
	}
	return "sonnet"
}

func AssignWorkers(
	config model.WorkerConfig,
	limits model.LimitsConfig,
	currentWorkerStates []WorkerState,
	tasks []TaskAssignmentRequest,
) ([]WorkerAssignment, error) {
	if len(tasks) == 0 {
		return nil, nil
	}

	// Build worker state map (copy to track incremental assignments)
	stateMap := make(map[string]*WorkerState, len(currentWorkerStates))
	for i := range currentWorkerStates {
		ws := currentWorkerStates[i]
		stateMap[ws.WorkerID] = &ws
	}

	maxPending := limits.MaxPendingTasksPerWorker
	if maxPending <= 0 {
		maxPending = 10
	}

	var assignments []WorkerAssignment
	for _, task := range tasks {
		requiredModel := GetModelForBloomLevel(task.BloomLevel, config.Boost)

		// Find eligible workers with matching model and minimum pending
		var bestWorker *WorkerState
		for _, ws := range stateMap {
			if ws.Model != requiredModel {
				continue
			}
			if ws.PendingCount >= maxPending {
				continue
			}
			if bestWorker == nil || ws.PendingCount < bestWorker.PendingCount {
				bestWorker = ws
			}
		}

		if bestWorker == nil {
			// Distinguish "no workers with matching model" from "matching workers at capacity"
			hasMatchingModel := false
			for _, ws := range stateMap {
				if ws.Model == requiredModel {
					hasMatchingModel = true
					break
				}
			}
			if !hasMatchingModel {
				return nil, fmt.Errorf("no available worker for task %q (model=%s): no workers configured for model %q (bloom_level=%d requires %s; use bloom_level 1-3 for sonnet or enable boost mode for opus)",
					task.Name, requiredModel, requiredModel, task.BloomLevel, requiredModel)
			}
			return nil, fmt.Errorf("no available worker for task %q (model=%s): all %s workers at capacity (max_pending_tasks_per_worker=%d)",
				task.Name, requiredModel, requiredModel, maxPending)
		}

		assignments = append(assignments, WorkerAssignment{
			TaskName: task.Name,
			WorkerID: bestWorker.WorkerID,
			Model:    bestWorker.Model,
		})
		bestWorker.PendingCount++
	}

	return assignments, nil
}

func BuildWorkerStates(maestroDir string, config model.WorkerConfig) ([]WorkerState, error) {
	var states []WorkerState

	for i := 1; i <= config.Count; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		workerModel := GetWorkerModel(workerID, config)

		pendingCount := 0
		queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(workerID))
		data, err := os.ReadFile(queueFile)
		if err == nil {
			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(data, &tq); err == nil {
				for _, task := range tq.Tasks {
					if task.Status == model.StatusPending {
						pendingCount++
					}
				}
			}
		}

		states = append(states, WorkerState{
			WorkerID:     workerID,
			Model:        workerModel,
			PendingCount: pendingCount,
		})
	}

	return states, nil
}

func workerIDToQueueFile(workerID string) string {
	// worker1 → worker1.yaml
	return workerID + ".yaml"
}
