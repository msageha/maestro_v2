package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// ApplyTaskDefaults fills in default values for required fields that may be
// omitted in Planner input for backward compatibility.
// ExpectedPaths is set to an empty slice if nil; DefinitionOfAbort is set to
// DefaultDefinitionOfAbort() if nil.
func ApplyTaskDefaults(tasks []TaskInput) {
	for i := range tasks {
		if tasks[i].ExpectedPaths == nil {
			tasks[i].ExpectedPaths = []string{}
		}
		if tasks[i].DefinitionOfAbort == nil {
			d := model.DefaultDefinitionOfAbort()
			tasks[i].DefinitionOfAbort = &d
		}
	}
}

func writeQueueEntries(maestroDir string, assignments []WorkerAssignment, tasks []TaskInput, nameToID map[string]string, commandID string, now string, lockMap *lock.MutexMap) error {
	// Group tasks by worker
	workerTasks := make(map[string][]model.Task)
	taskMap := make(map[string]TaskInput)
	for _, t := range tasks {
		taskMap[t.Name] = t
	}

	for _, a := range assignments {
		t := taskMap[a.TaskName]
		taskID := nameToID[a.TaskName]

		depIDs := make([]string, 0, len(t.BlockedBy))
		for _, depName := range t.BlockedBy {
			depID, ok := nameToID[depName]
			if !ok {
				return fmt.Errorf("blocked_by references unknown task %q in queue entry for task %q", depName, a.TaskName)
			}
			depIDs = append(depIDs, depID)
		}

		queueTask := model.Task{
			ID:                 taskID,
			CommandID:          commandID,
			Purpose:            t.Purpose,
			Content:            t.Content,
			AcceptanceCriteria: t.AcceptanceCriteria,
			Constraints:        t.Constraints,
			BlockedBy:          depIDs,
			BloomLevel:         t.BloomLevel,
			ToolsHint:          t.ToolsHint,
			PersonaHint:        t.PersonaHint,
			SkillRefs:          t.SkillRefs,
			ExpectedPaths:      t.ExpectedPaths,
			DefinitionOfAbort:  t.DefinitionOfAbort,
			Priority:           100,
			Status:             model.StatusPending,
			CreatedAt:          now,
			UpdatedAt:          now,
		}

		workerTasks[a.WorkerID] = append(workerTasks[a.WorkerID], queueTask)
	}

	// CRIT-02: Write to each worker's queue file under per-queue lock
	// to prevent concurrent read-modify-write data loss.
	for workerID, newTasks := range workerTasks {
		if err := func() error {
			if lockMap != nil {
				lockMap.Lock("queue:" + workerID)
				defer lockMap.Unlock("queue:" + workerID)
			}
			return readModifyWriteQueue(maestroDir, workerID, func(tq *model.TaskQueue) {
				tq.Tasks = append(tq.Tasks, newTasks...)
			})
		}(); err != nil {
			return err
		}
	}

	return nil
}

func rollbackQueueEntries(maestroDir string, tasks []TaskInput, nameToID map[string]string, assignMap map[string]WorkerAssignment, lockMap *lock.MutexMap) error {
	taskIDs := make(map[string]bool)
	for _, t := range tasks {
		taskIDs[nameToID[t.Name]] = true
	}

	// Group by worker
	workerFiles := make(map[string]bool)
	for _, a := range assignMap {
		workerFiles[a.WorkerID] = true
	}

	var errs []error

	// CRIT-02: Lock per-queue to prevent concurrent read-modify-write data loss.
	for workerID := range workerFiles {
		if err := func() error {
			if lockMap != nil {
				lockMap.Lock("queue:" + workerID)
				defer lockMap.Unlock("queue:" + workerID)
			}

			queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(workerID))

			data, err := os.ReadFile(queueFile) //nolint:gosec // queueFile is constructed from a controlled application queue directory
			if err != nil {
				return fmt.Errorf("read queue %s: %w", workerID, err)
			}

			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(data, &tq); err != nil {
				return fmt.Errorf("parse queue %s: %w", workerID, err)
			}

			var kept []model.Task
			for _, t := range tq.Tasks {
				if !taskIDs[t.ID] {
					kept = append(kept, t)
				}
			}
			tq.Tasks = kept

			if writeErr := yamlutil.AtomicWrite(queueFile, tq); writeErr != nil {
				return fmt.Errorf("write queue %s: %w", workerID, writeErr)
			}
			return nil
		}(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func rollbackPhaseFillState(state *model.CommandState, phaseIdx int, tasks []TaskInput, nameToID map[string]string) {
	for _, t := range tasks {
		taskID := nameToID[t.Name]

		// Remove from task_ids
		var filtered []string
		for _, id := range state.Phases[phaseIdx].TaskIDs {
			if id != taskID {
				filtered = append(filtered, id)
			}
		}
		state.Phases[phaseIdx].TaskIDs = filtered

		// Remove from required/optional
		state.RequiredTaskIDs = removeFromSlice(state.RequiredTaskIDs, taskID)
		state.OptionalTaskIDs = removeFromSlice(state.OptionalTaskIDs, taskID)

		delete(state.TaskStates, taskID)
		delete(state.TaskDependencies, taskID)
	}
	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)
}

func removeFromSlice(s []string, target string) []string {
	var result []string
	for _, v := range s {
		if v != target {
			result = append(result, v)
		}
	}
	return result
}

func checkCommandNotCancelled(maestroDir string, commandID string) error {
	plannerQueuePath := filepath.Join(maestroDir, "queue", "planner.yaml")
	data, err := os.ReadFile(plannerQueuePath) //nolint:gosec // plannerQueuePath is constructed from a controlled application queue directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no queue file = not cancelled
		}
		return fmt.Errorf("read planner queue: %w", err)
	}

	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return fmt.Errorf("parse planner queue: %w", err)
	}

	for _, cmd := range cq.Commands {
		if cmd.ID == commandID && cmd.Status == model.StatusCancelled {
			return fmt.Errorf("command %s has been cancelled in queue", commandID)
		}
	}
	return nil
}
