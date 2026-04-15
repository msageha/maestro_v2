package reconcile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// removeCommandFromPlannerQueue removes a command from queue/planner.yaml entirely.
func (r *Run) removeCommandFromPlannerQueue(commandID string) error {
	r.Deps.LockMap.Lock("queue:planner")
	defer r.Deps.LockMap.Unlock("queue:planner")

	queuePath := filepath.Join(r.Deps.MaestroDir, "queue", "planner.yaml")
	data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read planner queue: %w", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return fmt.Errorf("parse planner queue: %w", err)
	}

	filtered := make([]model.Command, 0, len(cq.Commands))
	for _, cmd := range cq.Commands {
		if cmd.ID != commandID {
			filtered = append(filtered, cmd)
		}
	}
	if len(filtered) == len(cq.Commands) {
		return nil
	}
	cq.Commands = filtered

	if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
		r.Log(core.LogLevelError, "R0 remove_command queue=%s error=%v", commandID, err)
		return fmt.Errorf("write planner queue: %w", err)
	}
	return nil
}

// removeTasksFromWorkerQueues removes all tasks for a given command from all worker queues.
func (r *Run) removeTasksFromWorkerQueues(commandID string) error {
	queueDir := filepath.Join(r.Deps.MaestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read queue dir: %w", err)
	}

	var writeErrs []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := extractWorkerID(name)
		if workerID == "" {
			continue
		}
		if err := func() error {
			r.Deps.LockMap.Lock("queue:" + workerID)
			defer r.Deps.LockMap.Unlock("queue:" + workerID)

			queuePath := filepath.Join(queueDir, name)
			data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				r.Log(core.LogLevelWarn, "R0 remove_tasks read_error file=%s command=%s error=%v", name, commandID, err)
				return nil
			}
			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(data, &tq); err != nil {
				r.Log(core.LogLevelWarn, "R0 remove_tasks parse_error file=%s command=%s error=%v", name, commandID, err)
				return nil
			}

			filtered := make([]model.Task, 0, len(tq.Tasks))
			for _, task := range tq.Tasks {
				if task.CommandID != commandID {
					filtered = append(filtered, task)
				}
			}
			if len(filtered) == len(tq.Tasks) {
				return nil
			}
			tq.Tasks = filtered

			if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
				r.Log(core.LogLevelError, "R0 remove_tasks file=%s command=%s error=%v", name, commandID, err)
				return fmt.Errorf("write %s: %w", name, err)
			}
			return nil
		}(); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}

	if len(writeErrs) > 0 {
		return writeErrs[0]
	}
	return nil
}

// batchRemoveTaskIDsFromQueues removes multiple task IDs from all worker queues in a single pass.
func (r *Run) batchRemoveTaskIDsFromQueues(taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}

	removeSet := make(map[string]struct{}, len(taskIDs))
	for _, id := range taskIDs {
		removeSet[id] = struct{}{}
	}

	queueDir := filepath.Join(r.Deps.MaestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return fmt.Errorf("read queue dir: %w", err)
	}

	var writeErrs []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := extractWorkerID(name)
		if workerID == "" {
			continue
		}
		if err := func() error {
			r.Deps.LockMap.Lock("queue:" + workerID)
			defer r.Deps.LockMap.Unlock("queue:" + workerID)

			queuePath := filepath.Join(queueDir, name)
			data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
			if err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(data, &tq); err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}

			filtered := make([]model.Task, 0, len(tq.Tasks))
			for _, task := range tq.Tasks {
				if _, remove := removeSet[task.ID]; !remove {
					filtered = append(filtered, task)
				}
			}
			if len(filtered) == len(tq.Tasks) {
				return nil
			}
			tq.Tasks = filtered

			if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
				r.Log(core.LogLevelError, "R0b batch_remove_tasks file=%s error=%v", name, err)
				return fmt.Errorf("write %s: %w", name, err)
			}
			return nil
		}(); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}

	if len(writeErrs) > 0 {
		return errors.Join(writeErrs...)
	}
	return nil
}
