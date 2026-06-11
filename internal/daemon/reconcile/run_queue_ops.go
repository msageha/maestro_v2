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
// batchRemoveTaskIDsFromQueues removes the given task IDs from every worker
// queue, but ONLY entries still in a pre-dispatch status (pending / planned
// / ready). A dispatched / running / terminal entry means the fill
// progressed after the caller's snapshot — removing it would strand a live
// worker whose result_write then fails with task-not-found. Returns:
//
//   - removed:    IDs whose queue entries were deleted
//   - keptActive: IDs found but left in place because their status had
//     advanced past pre-dispatch (callers must not strip these from state)
func (r *Run) batchRemoveTaskIDsFromQueues(taskIDs []string) (removed, keptActive map[string]bool, err error) {
	removed = make(map[string]bool)
	keptActive = make(map[string]bool)
	if len(taskIDs) == 0 {
		return removed, keptActive, nil
	}

	removeSet := make(map[string]struct{}, len(taskIDs))
	for _, id := range taskIDs {
		removeSet[id] = struct{}{}
	}

	removable := func(s model.Status) bool {
		switch s {
		case model.StatusPending, model.StatusPlanned, model.StatusReady:
			return true
		}
		return false
	}

	queueDir := filepath.Join(r.Deps.MaestroDir, "queue")
	entries, dirErr := os.ReadDir(queueDir)
	if dirErr != nil {
		return removed, keptActive, fmt.Errorf("read queue dir: %w", dirErr)
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
				if _, remove := removeSet[task.ID]; remove {
					if removable(task.Status) {
						removed[task.ID] = true
						continue
					}
					keptActive[task.ID] = true
					r.Log(core.LogLevelWarn,
						"R0b batch_remove_skip_active file=%s task=%s status=%s (entry advanced past pre-dispatch; keeping)",
						name, task.ID, task.Status)
				}
				filtered = append(filtered, task)
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
		return removed, keptActive, errors.Join(writeErrs...)
	}
	return removed, keptActive, nil
}
