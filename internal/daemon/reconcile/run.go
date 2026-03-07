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
	Deps     *Deps
	dirCache map[string][]os.DirEntry
}

func newRun(deps *Deps) *Run {
	return &Run{
		Deps:     deps,
		dirCache: make(map[string][]os.DirEntry, 4),
	}
}

// CachedReadDir returns cached directory entries for the given path within a single Reconcile scan.
func (r *Run) CachedReadDir(dir string) ([]os.DirEntry, error) {
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

// LoadState loads and parses a command state YAML file.
func (r *Run) LoadState(path string) (*model.CommandState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// StuckThresholdSec returns the threshold in seconds for considering a state "stuck".
func (r *Run) StuckThresholdSec() int {
	leaseSec := r.Deps.Config.Watcher.DispatchLeaseSec
	if leaseSec <= 0 {
		leaseSec = 300
	}
	return leaseSec * 2
}

// LoadCommandResultFile loads results/planner.yaml.
func (r *Run) LoadCommandResultFile(path string) (*model.CommandResultFile, error) {
	var rf model.CommandResultFile
	data, err := os.ReadFile(path)
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

// QuarantineCommandResult removes a specific result from results/planner.yaml and writes it to quarantine/.
func (r *Run) QuarantineCommandResult(resultPath string, result model.CommandResult) error {
	quarantineDir := filepath.Join(r.Deps.MaestroDir, "quarantine")
	if err := os.MkdirAll(quarantineDir, 0755); err != nil {
		return fmt.Errorf("create quarantine dir: %w", err)
	}

	quarantineFile := filepath.Join(quarantineDir,
		fmt.Sprintf("res_%s_%s.yaml", r.Deps.Clock.Now().UTC().Format("20060102T150405Z"), result.ID))

	if err := yamlutil.AtomicWrite(quarantineFile, result); err != nil {
		return fmt.Errorf("write quarantine file: %w", err)
	}

	r.Deps.LockMap.Lock("result:planner")
	defer r.Deps.LockMap.Unlock("result:planner")

	rf, err := r.LoadCommandResultFile(resultPath)
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

// RemoveCommandFromPlannerQueue removes a command from queue/planner.yaml entirely.
func (r *Run) RemoveCommandFromPlannerQueue(commandID string) error {
	r.Deps.LockMap.Lock("queue:planner")
	defer r.Deps.LockMap.Unlock("queue:planner")

	queuePath := filepath.Join(r.Deps.MaestroDir, "queue", "planner.yaml")
	data, err := os.ReadFile(queuePath)
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

// RemoveTasksFromWorkerQueues removes all tasks for a given command from all worker queues.
func (r *Run) RemoveTasksFromWorkerQueues(commandID string) error {
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

		workerID := ExtractWorkerID(name)
		if workerID == "" {
			continue
		}
		if err := func() error {
			r.Deps.LockMap.Lock("queue:" + workerID)
			defer r.Deps.LockMap.Unlock("queue:" + workerID)

			queuePath := filepath.Join(queueDir, name)
			data, err := os.ReadFile(queuePath)
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

// BatchRemoveTaskIDsFromQueues removes multiple task IDs from all worker queues in a single pass.
func (r *Run) BatchRemoveTaskIDsFromQueues(taskIDs []string) error {
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
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("batch_remove read queue dir: %w", err)
	}

	var writeErrs []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := ExtractWorkerID(name)
		if workerID == "" {
			continue
		}
		if err := func() error {
			r.Deps.LockMap.Lock("queue:" + workerID)
			defer r.Deps.LockMap.Unlock("queue:" + workerID)

			queuePath := filepath.Join(queueDir, name)
			data, err := os.ReadFile(queuePath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
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
		return writeErrs[0]
	}
	return nil
}

// UpdateLastReconciledAt updates last_reconciled_at on a state file.
func (r *Run) UpdateLastReconciledAt(commandID string) {
	statePath := filepath.Join(r.Deps.MaestroDir, "state", "commands", commandID+".yaml")

	lockKey := "state:" + commandID
	r.Deps.LockMap.Lock(lockKey)
	defer r.Deps.LockMap.Unlock(lockKey)

	state, err := r.LoadState(statePath)
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

// Log writes a log message via the DaemonLogger.
func (r *Run) Log(level core.LogLevel, format string, args ...any) {
	r.Deps.DL.Logf(level, format, args...)
}

// ExtractWorkerID extracts the worker ID from a queue filename.
func ExtractWorkerID(filename string) string {
	if !strings.HasPrefix(filename, "worker") || !strings.HasSuffix(filename, ".yaml") {
		return ""
	}
	return strings.TrimSuffix(filename, ".yaml")
}

// RemoveFromSlice removes a target string from a slice.
func RemoveFromSlice(s []string, target string) []string {
	var result []string
	for _, v := range s {
		if v != target {
			result = append(result, v)
		}
	}
	return result
}
