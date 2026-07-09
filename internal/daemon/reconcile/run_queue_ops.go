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

// removeCommandFromPlannerQueue removes a command from queue/planner.yaml
// entirely. The removed row is returned so the caller can re-insert it as
// compensation when a later phase vetoes the repair (R0's Phase 3 re-check).
// Returns (nil, nil) when the queue file or the command row does not exist.
func (r *Run) removeCommandFromPlannerQueue(commandID string) (*model.Command, error) {
	r.Deps.LockMap.Lock("queue:planner")
	defer r.Deps.LockMap.Unlock("queue:planner")

	queuePath := commandQueuePath(r.Deps.MaestroDir)
	data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read planner queue: %w", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return nil, fmt.Errorf("parse planner queue: %w", err)
	}

	var removed *model.Command
	filtered := make([]model.Command, 0, len(cq.Commands))
	for i := range cq.Commands {
		if cq.Commands[i].ID == commandID {
			cp := cq.Commands[i]
			removed = &cp
			continue
		}
		filtered = append(filtered, cq.Commands[i])
	}
	if removed == nil {
		return nil, nil
	}
	cq.Commands = filtered

	if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
		r.Log(core.LogLevelError, "R0 remove_command queue=%s error=%v", commandID, err)
		return nil, fmt.Errorf("write planner queue: %w", err)
	}
	return removed, nil
}

// restoreCommandToPlannerQueue re-inserts a command row removed by
// removeCommandFromPlannerQueue when a later phase vetoes the repair.
// Idempotent: skips when the command ID is already present.
func (r *Run) restoreCommandToPlannerQueue(cmd model.Command) {
	r.Deps.LockMap.WithLock("queue:planner", func() {
		queuePath := commandQueuePath(r.Deps.MaestroDir)
		if err := yamlutil.ReadModifyWrite(queuePath, func(cq *model.CommandQueue) error {
			for i := range cq.Commands {
				if cq.Commands[i].ID == cmd.ID {
					return yamlutil.ErrNoUpdate
				}
			}
			if cq.SchemaVersion == 0 {
				cq.SchemaVersion = 1
				cq.FileType = "queue_command"
			}
			cq.Commands = append(cq.Commands, cmd)
			return nil
		}); err != nil {
			r.Log(core.LogLevelError, "R0 command_restore_failed command=%s error=%v", cmd.ID, err)
			return
		}
		r.Log(core.LogLevelInfo, "R0 command_restored command=%s (repair vetoed; planner queue row re-inserted)", cmd.ID)
	})
}

// preDispatchRemovable reports whether a queue row is still in a pre-dispatch
// status and can be removed without stranding a live worker.
func preDispatchRemovable(s model.Status) bool {
	switch s {
	case model.StatusPending, model.StatusPlanned, model.StatusReady:
		return true
	}
	return false
}

// removeCommandTasksFromWorkerQueues removes the command's pre-dispatch
// (pending / planned / ready) tasks from every worker queue. Rows that
// advanced past pre-dispatch are left in place and reported via keptActive so
// the caller can veto the repair — deleting a dispatched row would strand a
// live worker whose result_write then fails with task-not-found. Terminal
// rows are also left in place (harmless history) and are NOT counted as
// active. Removed rows are returned for compensation re-insert via
// restoreQueueEntries.
func (r *Run) removeCommandTasksFromWorkerQueues(commandID string) (removed map[string]removedQueueEntry, keptActive map[string]bool, err error) {
	removed = make(map[string]removedQueueEntry)
	keptActive = make(map[string]bool)

	queueDir := queueDirPath(r.Deps.MaestroDir)
	entries, dirErr := os.ReadDir(queueDir)
	if dirErr != nil {
		if os.IsNotExist(dirErr) {
			return removed, keptActive, nil
		}
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
				if task.CommandID == commandID {
					if preDispatchRemovable(task.Status) {
						removed[task.ID] = removedQueueEntry{workerID: workerID, task: task}
						continue
					}
					if !model.IsTerminal(task.Status) {
						keptActive[task.ID] = true
						r.Log(core.LogLevelWarn,
							"R0 remove_tasks_skip_active file=%s task=%s status=%s (row advanced past pre-dispatch; keeping)",
							name, task.ID, task.Status)
					}
				}
				filtered = append(filtered, task)
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
		return removed, keptActive, errors.Join(writeErrs...)
	}
	return removed, keptActive, nil
}

// batchRemoveTaskIDsFromQueues removes multiple task IDs from all worker queues in a single pass.
// batchRemoveTaskIDsFromQueues removes the given task IDs from every worker
// queue, but ONLY entries still in a pre-dispatch status (pending / planned
// / ready). A dispatched / running / terminal entry means the fill
// progressed after the caller's snapshot — removing it would strand a live
// worker whose result_write then fails with task-not-found. Returns:
//
//   - removed:    deleted entries by task ID, with their full task struct
//     and owning worker so callers can re-insert them as compensation when
//     a sibling veto invalidates the removal (a phase's pre-dispatch rows
//     may already be gone by the time a later row of the same phase turns
//     out to be active)
//   - keptActive: IDs found but left in place because their status had
//     advanced past pre-dispatch (callers must not strip these from state)
func (r *Run) batchRemoveTaskIDsFromQueues(taskIDs []string) (removed map[string]removedQueueEntry, keptActive map[string]bool, err error) {
	removed = make(map[string]removedQueueEntry)
	keptActive = make(map[string]bool)
	if len(taskIDs) == 0 {
		return removed, keptActive, nil
	}

	removeSet := make(map[string]struct{}, len(taskIDs))
	for _, id := range taskIDs {
		removeSet[id] = struct{}{}
	}

	queueDir := queueDirPath(r.Deps.MaestroDir)
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
					if preDispatchRemovable(task.Status) {
						removed[task.ID] = removedQueueEntry{workerID: workerID, task: task}
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

// upsertPlannerSignal appends sig to queue/planner_signals.yaml unless a
// signal with the same canonical dedup key — (Kind, CommandID, PhaseID,
// WorkerID, ConflictGeneration), see the daemon's signalDedupKey — already
// exists. The signal queue is the durable at-least-once Planner delivery
// channel (retained with retry/backoff until delivered, removed when stale),
// used by reconcile rules whose one-shot notification would otherwise be
// lost on a transient pane failure.
func upsertPlannerSignal(run *Run, sig model.PlannerSignal) {
	signalPath := signalQueuePath(run.Deps.MaestroDir)
	run.Deps.LockMap.WithLock("queue:planner_signals", func() {
		if err := yamlutil.ReadModifyWrite(signalPath, func(sq *model.PlannerSignalQueue) error {
			for _, existing := range sq.Signals {
				if existing.Kind == sig.Kind && existing.CommandID == sig.CommandID &&
					existing.PhaseID == sig.PhaseID && existing.WorkerID == sig.WorkerID &&
					existing.ConflictGeneration == sig.ConflictGeneration {
					return yamlutil.ErrNoUpdate
				}
			}
			if sq.SchemaVersion == 0 {
				sq.SchemaVersion = 1
				sq.FileType = "planner_signal_queue"
			}
			sq.Signals = append(sq.Signals, sig)
			return nil
		}); err != nil {
			run.Log(core.LogLevelWarn,
				"planner_signal_upsert_failed kind=%s command=%s phase=%s error=%v",
				sig.Kind, sig.CommandID, sig.PhaseID, err)
		}
	})
}

// removePlannerSignal deletes the signal matching (kind, commandID, workerID)
// from queue/planner_signals.yaml. Compensation counterpart of
// upsertPlannerSignal for WAL-first rules (R7/R8) whose candidate moved on
// between the signal enqueue and the guard write: their signal kinds are
// command-scoped, so the scan loop's staleness filter never removes them and
// a leftover entry would nag the Planner about a resolved condition.
func removePlannerSignal(run *Run, kind, commandID, workerID string) {
	signalPath := signalQueuePath(run.Deps.MaestroDir)
	run.Deps.LockMap.WithLock("queue:planner_signals", func() {
		if err := yamlutil.ReadModifyWrite(signalPath, func(sq *model.PlannerSignalQueue) error {
			kept := sq.Signals[:0]
			for _, existing := range sq.Signals {
				if existing.Kind == kind && existing.CommandID == commandID && existing.WorkerID == workerID {
					continue
				}
				kept = append(kept, existing)
			}
			if len(kept) == len(sq.Signals) {
				return yamlutil.ErrNoUpdate
			}
			sq.Signals = kept
			return nil
		}); err != nil {
			run.Log(core.LogLevelWarn,
				"planner_signal_remove_failed kind=%s command=%s worker=%s error=%v",
				kind, commandID, workerID, err)
		}
	})
	run.Log(core.LogLevelInfo,
		"planner_signal_compensated kind=%s command=%s worker=%s (candidate moved on before the guard write)",
		kind, commandID, workerID)
}

// removedQueueEntry captures a queue row deleted by
// batchRemoveTaskIDsFromQueues so a caller can re-insert it (compensation)
// when a later veto invalidates the removal.
type removedQueueEntry struct {
	workerID string
	task     model.Task
}

// restoreQueueEntries re-inserts previously removed queue rows into their
// original worker queues. Idempotent: an entry whose ID already exists in
// the queue is skipped (someone re-registered it in the meantime).
func (r *Run) restoreQueueEntries(entries []removedQueueEntry) {
	byWorker := make(map[string][]model.Task)
	for _, e := range entries {
		byWorker[e.workerID] = append(byWorker[e.workerID], e.task)
	}
	queueDir := queueDirPath(r.Deps.MaestroDir)
	for workerID, tasks := range byWorker {
		r.Deps.LockMap.WithLock("queue:"+workerID, func() {
			queuePath := filepath.Join(queueDir, workerID+".yaml")
			data, err := os.ReadFile(queuePath) //nolint:gosec // controlled queue directory
			if err != nil && !os.IsNotExist(err) {
				r.Log(core.LogLevelWarn, "R0b queue_restore_read_failed worker=%s error=%v", workerID, err)
				return
			}
			var tq model.TaskQueue
			if len(data) > 0 {
				if err := yamlv3.Unmarshal(data, &tq); err != nil {
					r.Log(core.LogLevelWarn, "R0b queue_restore_parse_failed worker=%s error=%v", workerID, err)
					return
				}
			}
			existing := make(map[string]bool, len(tq.Tasks))
			for i := range tq.Tasks {
				existing[tq.Tasks[i].ID] = true
			}
			restored := 0
			for _, task := range tasks {
				if existing[task.ID] {
					continue
				}
				tq.Tasks = append(tq.Tasks, task)
				restored++
			}
			if restored == 0 {
				return
			}
			if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
				r.Log(core.LogLevelWarn, "R0b queue_restore_write_failed worker=%s error=%v", workerID, err)
				return
			}
			r.Log(core.LogLevelInfo, "R0b queue_entries_restored worker=%s count=%d (phase veto compensation)", workerID, restored)
		})
	}
}
