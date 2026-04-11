package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// Run is the per-scan context created by Engine.Reconcile().
// It holds the directory cache and exposes helper methods used by patterns.
type Run struct {
	core.LogMixin
	Deps     *Deps
	dirCache map[string][]os.DirEntry
}

func newRun(deps *Deps) *Run {
	return &Run{
		LogMixin: core.LogMixin{DL: deps.DL},
		Deps:     deps,
		dirCache: make(map[string][]os.DirEntry, 4),
	}
}

// cachedReadDir returns cached directory entries for the given path within a single Reconcile scan.
func (r *Run) cachedReadDir(dir string) ([]os.DirEntry, error) {
	if entries, ok := r.dirCache[dir]; ok {
		return entries, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	r.dirCache[dir] = entries
	return entries, nil
}

// loadState loads and parses a command state YAML file.
func (r *Run) loadState(path string) (*model.CommandState, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application state directory
	if err != nil {
		return nil, err
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// stuckThresholdSec returns the threshold in seconds for considering a state "stuck".
func (r *Run) stuckThresholdSec() int {
	leaseSec := r.Deps.Config.Watcher.DispatchLeaseSec
	if leaseSec <= 0 {
		leaseSec = 300
	}
	return leaseSec * 2
}

// loadCommandResultFile loads results/planner.yaml.
func (r *Run) loadCommandResultFile(path string) (*model.CommandResultFile, error) {
	var rf model.CommandResultFile
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application results directory
	if err != nil {
		if os.IsNotExist(err) {
			return &rf, nil
		}
		return nil, err
	}
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		return nil, err
	}
	return &rf, nil
}

// quarantineCommandResult removes a specific result from results/planner.yaml and writes it to quarantine/.
func (r *Run) quarantineCommandResult(resultPath string, result model.CommandResult) error {
	quarantineDir := filepath.Join(r.Deps.MaestroDir, "quarantine")
	if err := os.MkdirAll(quarantineDir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for a quarantine directory
		return fmt.Errorf("create quarantine dir: %w", err)
	}

	quarantineFile := filepath.Join(quarantineDir,
		fmt.Sprintf("res_%s_%s.yaml", r.Deps.Clock.Now().UTC().Format("20060102T150405Z"), result.ID))

	if err := yamlutil.AtomicWrite(quarantineFile, result); err != nil {
		return fmt.Errorf("write quarantine file: %w", err)
	}

	r.Deps.LockMap.Lock("result:planner")
	defer r.Deps.LockMap.Unlock("result:planner")

	rf, err := r.loadCommandResultFile(resultPath)
	if err != nil {
		return fmt.Errorf("reload result file: %w", err)
	}

	filtered := make([]model.CommandResult, 0, len(rf.Results))
	for _, res := range rf.Results {
		if res.ID != result.ID {
			filtered = append(filtered, res)
		}
	}
	rf.Results = filtered

	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		return fmt.Errorf("write result file: %w", err)
	}

	return nil
}

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
func (r *Run) batchRemoveTaskIDsFromQueues(taskIDs []string) {
	if len(taskIDs) == 0 {
		return
	}

	removeSet := make(map[string]struct{}, len(taskIDs))
	for _, id := range taskIDs {
		removeSet[id] = struct{}{}
	}

	queueDir := filepath.Join(r.Deps.MaestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := extractWorkerID(name)
		if workerID == "" {
			continue
		}
		func() {
			r.Deps.LockMap.Lock("queue:" + workerID)
			defer r.Deps.LockMap.Unlock("queue:" + workerID)

			queuePath := filepath.Join(queueDir, name)
			data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
			if err != nil {
				return
			}
			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(data, &tq); err != nil {
				return
			}

			filtered := make([]model.Task, 0, len(tq.Tasks))
			for _, task := range tq.Tasks {
				if _, remove := removeSet[task.ID]; !remove {
					filtered = append(filtered, task)
				}
			}
			if len(filtered) == len(tq.Tasks) {
				return
			}
			tq.Tasks = filtered

			if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
				r.Log(core.LogLevelError, "R0b batch_remove_tasks file=%s error=%v", name, err)
			}
		}()
	}
}

// updateLastReconciledAt updates last_reconciled_at on a state file.
func (r *Run) updateLastReconciledAt(commandID string) {
	statePath := filepath.Join(r.Deps.MaestroDir, "state", "commands", commandID+".yaml")

	lockKey := "state:" + commandID
	r.Deps.LockMap.Lock(lockKey)
	defer r.Deps.LockMap.Unlock(lockKey)

	state, err := r.loadState(statePath)
	if err != nil {
		return
	}

	now := r.Deps.Clock.Now().UTC().Format(time.RFC3339)
	state.LastReconciledAt = &now
	state.UpdatedAt = now
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		r.Log(core.LogLevelError, "update_last_reconciled command=%s error=%v", commandID, err)
	}
}


// extractWorkerID extracts the worker ID from a queue filename.
func extractWorkerID(filename string) string {
	if !strings.HasPrefix(filename, "worker") || !strings.HasSuffix(filename, ".yaml") {
		return ""
	}
	return strings.TrimSuffix(filename, ".yaml")
}

// removeFromSlice removes a target string from a slice.
func removeFromSlice(s []string, target string) []string {
	var result []string
	for _, v := range s {
		if v != target {
			result = append(result, v)
		}
	}
	return result
}
