package plan

import (
	"errors"
	"fmt"
	"log"
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
	constraints        []string
	blockedBy          []string
	bloomLevel         int
	toolsHint          []string
	personaHint        string
	skillRefs          []string
	workerID           string
}

func writeRetryQueueEntry(maestroDir string, task retryQueueTask, now string, lockMap *lock.MutexMap) error {
	// CRIT-02: Lock per-queue to prevent concurrent read-modify-write data loss.
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
			Constraints:        task.constraints,
			BlockedBy:          task.blockedBy,
			BloomLevel:         task.bloomLevel,
			ToolsHint:          task.toolsHint,
			PersonaHint:        task.personaHint,
			SkillRefs:          task.skillRefs,
			Priority:           100,
			Status:             model.StatusPending,
			CreatedAt:          now,
			UpdatedAt:          now,
		})
	})
}

func loadOriginalTasksFromQueue(maestroDir string, commandID string) (map[string]model.Task, error) {
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
		filePath := filepath.Join(queueDir, name)
		data, err := os.ReadFile(filePath) //nolint:gosec // filePath is constructed from a controlled application queue directory
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // file removed between ReadDir and ReadFile; race-safe
			}
			return nil, fmt.Errorf("read queue file %s: %w", name, err)
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			log.Printf("[WARN] loadOriginalTasksFromQueue: skipping corrupt queue file %s: %v", name, err)
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

	// CRIT-02: Lock per-queue to prevent concurrent read-modify-write data loss.
	for _, rb := range workerMap {
		func() {
			if lockMap != nil {
				lockMap.Lock("queue:" + rb.workerID)
				defer lockMap.Unlock("queue:" + rb.workerID)
			}

			queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(rb.workerID))
			data, err := os.ReadFile(queueFile) //nolint:gosec // queueFile is constructed from a controlled application queue directory
			if err != nil {
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
				log.Printf("[WARN] rollback: write queue %s: %v", queueFile, writeErr)
			}
		}()
	}
}

func copyState(state *model.CommandState) ([]byte, error) {
	return yamlv3.Marshal(state)
}

func restoreState(state *model.CommandState, data []byte) {
	var restored model.CommandState
	if err := yamlv3.Unmarshal(data, &restored); err != nil {
		log.Printf("[ERROR] restoreState: failed to unmarshal state snapshot: %v", err)
		return
	}
	*state = restored
}

// cancelOriginalTaskInQueue scans all worker queues and sets the original
// (retried) task's status to cancelled, preventing checkCommandTasksTerminal
// from treating it as a failure after the retry supersedes it.
func cancelOriginalTaskInQueue(maestroDir string, taskID string, commandID string, now string, lockMap *lock.MutexMap) error {
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
					tq.Tasks[i].Status = model.StatusCancelled
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
			return fmt.Errorf("cancel original task %s in queue %s: %w", taskID, workerID, err)
		}
		if found {
			return nil
		}
	}
	return nil // task not found — may have been archived or cleaned up
}

// restoreOriginalTaskInQueue is the rollback counterpart of cancelOriginalTaskInQueue.
// It restores the original task's queue status (best-effort, used only on SaveState failure).
func restoreOriginalTaskInQueue(maestroDir string, taskID string, commandID string, status model.Status, now string, lockMap *lock.MutexMap) error {
	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
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
			return fmt.Errorf("restore task %s in queue %s: %w", taskID, workerID, err)
		}
		if found {
			return nil
		}
	}
	return nil
}
