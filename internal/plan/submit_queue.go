package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// queueFlockPath returns the flock file path for cross-process locking of a
// queue file. Lock files are kept separate from data files so that
// AtomicWrite's temp→rename does not invalidate the flock inode.
func queueFlockPath(maestroDir, queueFilename string) string {
	return filepath.Join(maestroDir, "locks", queueFilename+".flock")
}

// acquireFlock opens (or creates) a lock file and acquires a flock of the
// given type (syscall.LOCK_EX or syscall.LOCK_SH). The returned *os.File
// must be passed to releaseFlock when the protected section ends.
func acquireFlock(lockPath string, lockType int) (*os.File, error) {
	dir := filepath.Dir(lockPath)
	if err := os.MkdirAll(dir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for a locks directory
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	// lockPath is provided by callers from a controlled application directory.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // controlled path
	if err != nil {
		return nil, fmt.Errorf("open flock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), lockType); err != nil { //nolint:gosec // uintptr→int conversion for fd is safe
		_ = f.Close()
		return nil, fmt.Errorf("acquire flock %s: %w", lockPath, err)
	}
	return f, nil
}

// releaseFlock releases the flock and closes the file. Safe to call with nil.
func releaseFlock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // uintptr→int conversion for fd is safe
	_ = f.Close()
}

// ApplyTaskDefaults is retained as an extension point for future per-field
// defaults, but no longer fills required fields silently. REQUIREMENTS.md
// §S3-1 mandates that every task explicitly declare expected_paths and
// definition_of_abort. Auto-filling these used to mask missing Planner output
// and disabled Path-overlap Heuristics (A-4) and Circuit Breakers (S2-2);
// validation now rejects nil values directly so the gap is surfaced loudly.
func ApplyTaskDefaults(tasks []TaskInput) {
	_ = tasks // currently a no-op; kept so callers compile across the change.
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
			// run_on_main / run_on_integration must propagate from TaskInput so
			// that plan submit can express "main を見る verification" tasks and
			// "統合 worktree で解決" tasks without requiring a follow-up add-task.
			RunOnMain:        t.RunOnMain,
			RunOnIntegration: t.RunOnIntegration,
			Priority:         100,
			Status:           model.StatusPending,
			CreatedAt:        now,
			UpdatedAt:        now,
		}

		workerTasks[a.WorkerID] = append(workerTasks[a.WorkerID], queueTask)
	}

	// CRIT-02: Write to each worker's queue file under per-queue lock
	// to prevent concurrent read-modify-write data loss.
	// C-A3: flock(LOCK_EX) provides cross-process protection in addition
	// to the in-process MutexMap lock.
	for workerID, newTasks := range workerTasks {
		if err := func() error {
			if lockMap != nil {
				lockMap.Lock("queue:" + workerID)
				defer lockMap.Unlock("queue:" + workerID)
			}
			flockFile, flockErr := acquireFlock(
				queueFlockPath(maestroDir, workerIDToQueueFile(workerID)),
				syscall.LOCK_EX,
			)
			if flockErr != nil {
				return flockErr
			}
			defer releaseFlock(flockFile)

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
	// C-A3: flock(LOCK_EX) provides cross-process protection.
	for workerID := range workerFiles {
		if err := func() error {
			if lockMap != nil {
				lockMap.Lock("queue:" + workerID)
				defer lockMap.Unlock("queue:" + workerID)
			}
			flockFile, flockErr := acquireFlock(
				queueFlockPath(maestroDir, workerIDToQueueFile(workerID)),
				syscall.LOCK_EX,
			)
			if flockErr != nil {
				return flockErr
			}
			defer releaseFlock(flockFile)

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

	// C-A4: Acquire shared file-level lock for consistent reads.
	// AtomicWrite (temp→rename) already guarantees no partial reads on POSIX;
	// LOCK_SH provides an additional safety layer at negligible overhead,
	// ensuring we do not read during the brief rename window.
	flockFile, flockErr := acquireFlock(
		queueFlockPath(maestroDir, "planner.yaml"),
		syscall.LOCK_SH,
	)
	if flockErr != nil {
		return flockErr
	}
	defer releaseFlock(flockFile)

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
			return fmt.Errorf("%w: command %s", ErrCommandCancelled, commandID)
		}
	}
	return nil
}
