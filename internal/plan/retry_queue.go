package plan

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

type retryQueueTask struct {
	taskID             string
	commandID          string
	purpose            string
	content            string
	acceptanceCriteria string
	definitionOfDone   []string
	constraints        []string
	blockedBy          []string
	bloomLevel         int
	toolsHint          []string
	personaHint        string
	skillRefs          []string
	expectedPaths      []string
	definitionOfAbort  *model.DefinitionOfAbort
	workerID           string
	runOnMain          bool
	runOnIntegration   bool
	// operationType is the §S0-1 admission classification (verify/repair/rollout).
	// Retry tasks should set this to model.OperationTypeRepair so admission
	// control counts them against the repair concurrency bucket regardless of
	// the original task's free-form Purpose string.
	operationType string
}

func writeRetryQueueEntry(maestroDir string, task retryQueueTask, now string, lockMap *lock.MutexMap) error {
	// Lock per-queue to prevent concurrent read-modify-write data loss when
	// multiple writers append to the same worker queue.
	if lockMap != nil {
		lockMap.Lock("queue:" + task.workerID)
		defer lockMap.Unlock("queue:" + task.workerID)
	}

	return readModifyWriteQueue(maestroDir, task.workerID, func(tq *model.TaskQueue) {
		tq.Tasks = append(tq.Tasks, model.Task{
			ID:                 task.taskID,
			CommandID:          task.commandID,
			Purpose:            task.purpose,
			Content:            task.content,
			AcceptanceCriteria: task.acceptanceCriteria,
			DefinitionOfDone:   task.definitionOfDone,
			Constraints:        task.constraints,
			BlockedBy:          task.blockedBy,
			BloomLevel:         task.bloomLevel,
			ToolsHint:          task.toolsHint,
			PersonaHint:        task.personaHint,
			SkillRefs:          task.skillRefs,
			ExpectedPaths:      task.expectedPaths,
			DefinitionOfAbort:  task.definitionOfAbort,
			RunOnMain:          task.runOnMain,
			RunOnIntegration:   task.runOnIntegration,
			OperationType:      task.operationType,
			Priority:           100,
			Status:             model.StatusPending,
			CreatedAt:          now,
			UpdatedAt:          now,
		})
	})
}

func loadOriginalTasksFromQueue(maestroDir string, commandID string, lockMap *lock.MutexMap) (map[string]model.Task, error) {
	result := make(map[string]model.Task)
	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return nil, fmt.Errorf("read queue directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		workerID := strings.TrimSuffix(name, ".yaml")

		if lockMap != nil {
			lockMap.Lock("queue:" + workerID)
		}
		filePath := filepath.Join(queueDir, name)
		data, err := os.ReadFile(filePath) //nolint:gosec // filePath is constructed from a controlled application queue directory
		if lockMap != nil {
			lockMap.Unlock("queue:" + workerID)
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // file removed between ReadDir and ReadFile; race-safe
			}
			return nil, fmt.Errorf("read queue file %s: %w", name, err)
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			slog.Warn("loadOriginalTasksFromQueue: skipping corrupt queue file", "file", name, "error", err)
			continue
		}
		for _, task := range tq.Tasks {
			if task.CommandID == commandID {
				result[task.ID] = task
			}
		}
	}
	return result, nil
}

func rollbackRetryQueueEntries(maestroDir string, written []retryQueueTask, lockMap *lock.MutexMap) {
	// Group task IDs by worker
	type workerRollback struct {
		workerID string
		taskIDs  map[string]bool
	}
	workerMap := make(map[string]*workerRollback) // workerID → rollback data
	for _, t := range written {
		if workerMap[t.workerID] == nil {
			workerMap[t.workerID] = &workerRollback{
				workerID: t.workerID,
				taskIDs:  make(map[string]bool),
			}
		}
		workerMap[t.workerID].taskIDs[t.taskID] = true
	}

	// Lock per-queue to prevent concurrent read-modify-write data loss when
	// multiple writers append to the same worker queue.
	for _, rb := range workerMap {
		func() {
			if lockMap != nil {
				lockMap.Lock("queue:" + rb.workerID)
				defer lockMap.Unlock("queue:" + rb.workerID)
			}

			queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(rb.workerID))
			data, err := os.ReadFile(queueFile) //nolint:gosec // queueFile is constructed from a controlled application queue directory
			if err != nil {
				slog.Warn("rollback: failed to read queue", "file", queueFile, "error", err)
				return
			}
			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(data, &tq); err != nil {
				return
			}
			var kept []model.Task
			for _, task := range tq.Tasks {
				if !rb.taskIDs[task.ID] {
					kept = append(kept, task)
				}
			}
			tq.Tasks = kept
			if writeErr := yamlutil.AtomicWrite(queueFile, tq); writeErr != nil {
				slog.Warn("rollback: failed to write queue", "file", queueFile, "error", writeErr)
			}
		}()
	}
}

func copyState(state *model.CommandState) ([]byte, error) {
	return yamlv3.Marshal(state)
}

func restoreState(state *model.CommandState, data []byte) error {
	var restored model.CommandState
	if err := yamlv3.Unmarshal(data, &restored); err != nil {
		return fmt.Errorf("restoreState: failed to unmarshal state snapshot: %w", err)
	}
	*state = restored
	return nil
}

// updateOriginalTaskInQueue scans all worker queues and sets the specified
// task's status. Used both for cancelling the original task during retry
// (preventing checkCommandTasksTerminal from treating it as a failure) and
// for restoring the original status on rollback.
func updateOriginalTaskInQueue(maestroDir string, taskID string, commandID string, status model.Status, now string, lockMap *lock.MutexMap) error {
	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read queue directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		workerID := strings.TrimSuffix(name, ".yaml")

		if lockMap != nil {
			lockMap.Lock("queue:" + workerID)
		}
		found := false
		err := readModifyWriteQueue(maestroDir, workerID, func(tq *model.TaskQueue) {
			for i := range tq.Tasks {
				if tq.Tasks[i].ID == taskID && tq.Tasks[i].CommandID == commandID {
					tq.Tasks[i].Status = status
					tq.Tasks[i].UpdatedAt = now
					found = true
					return
				}
			}
		})
		if lockMap != nil {
			lockMap.Unlock("queue:" + workerID)
		}
		if err != nil {
			return fmt.Errorf("update task %s status to %s in queue %s: %w", taskID, status, workerID, err)
		}
		if found {
			return nil
		}
	}
	return nil // task not found — may have been archived or cleaned up
}
