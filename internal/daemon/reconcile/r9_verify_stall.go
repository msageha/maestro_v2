package reconcile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// R9VerifyStall recovers tasks that are stuck in verify_pending. The §S1-1
// verification runner is responsible for advancing such tasks to completed or
// repair_pending, but if the runner crashes mid-execution (or the daemon is
// restarted between writing the result and finishing verification) a task can
// be left dangling. R9 detects tasks whose verify_pending state has been held
// longer than VerifyDaemonConfig.StallThresholdSec and forces them to
// repair_pending so the planner / retry pipeline can construct a fresh attempt.
//
// Stall age is measured against the most recent worker result for the task —
// when result_write_phase_b records a verify_pending transition the task's
// matching TaskResult.CreatedAt is the closest available timestamp for "when
// verification started". Falling back to the state file's UpdatedAt would over-
// count edits unrelated to this task.
type R9VerifyStall struct{}

// Apply scans every command state file, finds verify_pending tasks whose
// matching worker result is older than the configured threshold, and rewrites
// their TaskStates entry to repair_pending.
func (R9VerifyStall) Apply(run *Run) Outcome {
	// verify.enabled=false means the daemon wires NewSkipVerifyRunner, so
	// no task ever enters verify_pending. Running R9 in that mode just
	// produces noisy WARN ("R9 verify_config_load_failed … no such file
	// or directory") for commands that never had a verify snapshot
	// written — Report 2026-05-05. Treat the disabled flag as the SSoT
	// and short-circuit before scanning any state files.
	if !run.Deps.Config.Verify.EffectiveEnabled() {
		return Outcome{}
	}
	thresholdSec := run.Deps.Config.Verify.EffectiveStallThresholdSec()
	if thresholdSec <= 0 {
		return Outcome{} // disabled
	}
	threshold := time.Duration(thresholdSec) * time.Second

	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.cachedReadDir(stateDir)
	if err != nil {
		return Outcome{}
	}

	resultTimestamps := r9LoadResultTimestamps(run)

	var repairs []Repair
	now := run.Deps.Clock.Now().UTC()

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(name, ".yaml")
		statePath := filepath.Join(stateDir, name)

		commandThreshold := r9EffectiveVerifyStallThreshold(run, commandID, threshold)
		commandRepairs := r9ApplyForCommand(run, statePath, commandID, resultTimestamps[commandID], now, commandThreshold)
		r9ScheduleVerifyRepairs(run, statePath, commandID, commandRepairs)
		repairs = append(repairs, commandRepairs...)
		repairs = append(repairs, r9ReclaimOrphanedRepairPending(run, statePath, commandID, resultTimestamps[commandID], now, commandThreshold)...)
	}

	return Outcome{Repairs: repairs}
}

// r9OrphanReclaimGrace is the minimum state-file quiescence before a
// repair_pending entry with no registered repair pipeline is treated as a
// crash artifact. Writing repair_pending bumps state.UpdatedAt, so a
// concurrent writer that is about to register the repair task (the window
// between its state write and its queue write) is never raced by the sweep.
const r9OrphanReclaimGrace = 60 * time.Second

// r9ReclaimOrphanedRepairPending recovers repair_pending tasks whose repair
// pipeline lost ownership. R9 transitions verify_pending → repair_pending and
// registers the repair task within the same Apply, and every registration
// failure path advances the slot to paused_for_replan immediately — so a
// persisting repair_pending entry is healthy only while a successor exists.
// A crash between the repair_pending state write and the repair-task
// registration leaves the entry with no successor anywhere: R9's main pass
// walks only verify_pending and R2 deliberately skips the repair slot, so
// nothing ever reclaimed it and the publish gate stayed blocked forever.
//
// Ownership probes (any of them present → the entry is owned, skip):
//   - a RetryLineage successor (repair task registered in state);
//   - a RetryEnqueueFailed marker (R1 owns the re-enqueue).
//
// Unowned entries older than the verify stall threshold (anchored on the
// task's most recent worker result, falling back to state.UpdatedAt like the
// main pass) are advanced to paused_for_replan through the same helper R9's
// failure paths use, which also emits the durable paused_for_replan signal.
func r9ReclaimOrphanedRepairPending(run *Run, statePath, commandID string, resultsForCommand map[string]time.Time, now time.Time, threshold time.Duration) []Repair {
	var orphans []struct {
		taskID string
		age    time.Duration
	}
	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			if !os.IsNotExist(err) {
				run.Log(core.LogLevelError, "R9 orphan_reclaim_load_state command=%s error=%v", commandID, err)
			}
			return
		}
		if len(state.TaskStates) == 0 {
			return
		}
		// Quiescence grace: repair_pending writes bump UpdatedAt, so a fresh
		// state file means a repair registration may still be in flight.
		if updatedAt, err := time.Parse(time.RFC3339, state.UpdatedAt); err == nil {
			if now.Sub(updatedAt) < r9OrphanReclaimGrace {
				return
			}
		}
		hasSuccessor := make(map[string]bool, len(state.RetryLineage))
		for _, predecessorID := range state.RetryLineage {
			hasSuccessor[predecessorID] = true
		}
		for taskID, status := range state.TaskStates {
			if status != model.StatusRepairPending {
				continue
			}
			if hasSuccessor[taskID] {
				continue
			}
			if _, ok := state.RetryEnqueueFailed[taskID]; ok {
				continue
			}
			anchor, anchorOk := resultsForCommand[taskID]
			if !anchorOk {
				if t, perr := time.Parse(time.RFC3339, state.UpdatedAt); perr == nil {
					anchor = t
				} else {
					continue
				}
			}
			age := now.Sub(anchor)
			if age < threshold {
				continue
			}
			orphans = append(orphans, struct {
				taskID string
				age    time.Duration
			}{taskID: taskID, age: age})
		}
	})

	var repairs []Repair
	for _, o := range orphans {
		run.Log(core.LogLevelWarn,
			"R9 repair_pending_orphaned command=%s task=%s age=%s (no repair task registered; crash between transition and registration) -> paused_for_replan",
			commandID, o.taskID, o.age.Round(time.Second))
		if !r9AdvanceRepairPendingToReplan(run, statePath, commandID, o.taskID, "repair_pending_orphaned") {
			continue
		}
		repairs = append(repairs, Repair{
			Pattern:   PatternR9,
			CommandID: commandID,
			TaskID:    o.taskID,
			Detail:    fmt.Sprintf("repair_pending orphaned for %s with no registered repair task; advanced to paused_for_replan", o.age.Round(time.Second)),
		})
	}
	return repairs
}

// r9ApplyForCommand applies R9 logic to a single command state file under its
// state lock. Returns the repairs performed (or nil when no transitions were
// needed / the write failed).
func r9ApplyForCommand(run *Run, statePath, commandID string, resultsForCommand map[string]time.Time, now time.Time, threshold time.Duration) []Repair {
	var commandRepairs []Repair

	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			if !os.IsNotExist(err) {
				run.Log(core.LogLevelError, "R9 load_state command=%s error=%v", commandID, err)
			}
			return
		}
		if state.TaskStates == nil {
			return
		}

		modified := false
		for taskID, status := range state.TaskStates {
			if status != model.StatusVerifyPending {
				continue
			}
			startedAt, ok := resultsForCommand[taskID]
			if !ok {
				// No result on disk for this task — fall back to the
				// state file's UpdatedAt to avoid permanent stuck states
				// when results are archived.
				if state.UpdatedAt == "" {
					continue
				}
				if t, perr := time.Parse(time.RFC3339, state.UpdatedAt); perr == nil {
					startedAt = t
				} else {
					continue
				}
			}
			age := now.Sub(startedAt)
			if age < threshold {
				continue
			}

			run.Log(core.LogLevelWarn,
				"R9 verify_pending_stall command=%s task=%s age=%s threshold=%s -> repair_pending",
				commandID, taskID, age.Round(time.Second), threshold)

			state.SetTaskState(taskID, model.StatusRepairPending, now.Format(time.RFC3339))
			modified = true
			commandRepairs = append(commandRepairs, Repair{
				Pattern:   PatternR9,
				CommandID: commandID,
				TaskID:    taskID,
				Detail:    fmt.Sprintf("verify_pending stalled for %s (>%s); transitioned to repair_pending (verify_runner_stall)", age.Round(time.Second), threshold),
			})
		}

		if modified {
			nowStr := now.Format(time.RFC3339)
			state.LastReconciledAt = &nowStr
			state.UpdatedAt = nowStr
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				run.Log(core.LogLevelError, "R9 write_state command=%s error=%v", commandID, err)
				commandRepairs = nil
			}
		}
	})

	return commandRepairs
}

func r9ScheduleVerifyRepairs(run *Run, statePath, commandID string, repairs []Repair) {
	if len(repairs) == 0 {
		return
	}
	for _, repair := range repairs {
		if !run.Deps.Config.Retry.TaskExecution.Enabled {
			run.Log(core.LogLevelWarn,
				"R9 verify_repair_retry_disabled command=%s task=%s -> paused_for_replan",
				commandID, repair.TaskID)
			r9AdvanceRepairPendingToReplan(run, statePath, commandID, repair.TaskID, "verify_repair_retry_disabled")
			continue
		}
		sourceTask, workerID := r9FindQueueTaskByID(run, commandID, repair.TaskID)
		if sourceTask == nil {
			run.Log(core.LogLevelWarn,
				"R9 verify_repair_source_missing command=%s task=%s -> paused_for_replan",
				commandID, repair.TaskID)
			r9AdvanceRepairPendingToReplan(run, statePath, commandID, repair.TaskID, "verify_repair_source_missing")
			continue
		}
		if !r9RepairBudgetAllows(run, sourceTask) {
			r9SetQueueTaskTerminalStatus(run, workerID, repair.TaskID, model.StatusFailed)
			r9AdvanceRepairPendingToReplan(run, statePath, commandID, repair.TaskID, "verify_repair_budget_exhausted")
			continue
		}

		repairTaskID, err := model.NewTaskID(model.TaskIDCallerDaemonRetry)
		if err != nil {
			run.Log(core.LogLevelError,
				"R9 verify_repair_id_failed command=%s task=%s error=%v -> paused_for_replan",
				commandID, repair.TaskID, err)
			r9SetQueueTaskTerminalStatus(run, workerID, repair.TaskID, model.StatusFailed)
			r9AdvanceRepairPendingToReplan(run, statePath, commandID, repair.TaskID, fmt.Sprintf("verify_repair_id_failed: %v", err))
			continue
		}
		repairTask := r1BuildRetryTask(sourceTask, repairTaskID, run.Deps.Clock)
		repairTask.Content = fmt.Sprintf(
			"Repair the previous implementation because daemon verification stalled.\n\nStall detail:\n%s\n\nOriginal task:\n%s",
			repair.Detail,
			sourceTask.Content,
		)

		if err := r9RegisterRepairTask(run, statePath, commandID, workerID, repair.TaskID, &repairTask); err != nil {
			run.Log(core.LogLevelError,
				"R9 verify_repair_schedule_failed command=%s task=%s repair_id=%s error=%v -> paused_for_replan",
				commandID, repair.TaskID, repairTask.ID, err)
			r9SetQueueTaskTerminalStatus(run, workerID, repair.TaskID, model.StatusFailed)
			r9AdvanceRepairPendingToReplan(run, statePath, commandID, repair.TaskID, fmt.Sprintf("verify_repair_schedule_failed: %v", err))
			continue
		}
		r9SetQueueTaskTerminalStatus(run, workerID, repair.TaskID, model.StatusCancelled)
		run.Log(core.LogLevelInfo,
			"R9 verify_repair_scheduled command=%s task=%s repair_id=%s worker=%s",
			commandID, repair.TaskID, repairTask.ID, workerID)
	}
}

func r9FindQueueTaskByID(run *Run, commandID, taskID string) (*model.Task, string) {
	queueDir := queueDirPath(run.Deps.MaestroDir)
	entries, err := run.cachedReadDir(queueDir)
	if err != nil {
		return nil, ""
	}
	for _, entry := range entries {
		workerID := extractWorkerID(entry.Name())
		if workerID == "" {
			continue
		}
		queuePath := filepath.Join(queueDir, entry.Name())
		run.Deps.LockMap.Lock("queue:" + workerID)
		data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is in a controlled queue directory
		run.Deps.LockMap.Unlock("queue:" + workerID)
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			continue
		}
		for i := range tq.Tasks {
			task := tq.Tasks[i]
			if task.CommandID == commandID && task.ID == taskID {
				return &task, workerID
			}
		}
	}
	return nil, ""
}

func r9RepairBudgetAllows(run *Run, task *model.Task) bool {
	if task.DefinitionOfAbort != nil && task.DefinitionOfAbort.MaxRepairCount > 0 &&
		task.ExecutionRetries >= task.DefinitionOfAbort.MaxRepairCount {
		return false
	}
	maxRetries := run.Deps.Config.Retry.TaskExecution.MaxRetries
	return maxRetries <= 0 || task.ExecutionRetries < maxRetries
}

func r9EffectiveVerifyStallThreshold(run *Run, commandID string, configured time.Duration) time.Duration {
	const (
		perCommandTimeout = 5 * time.Minute
		verifyStallGrace  = 30 * time.Second
		// verifyStallSaneMinimum guards against operator misconfiguration
		// (e.g. config yaml with verify_stall_sec=0) that would otherwise
		// flag every in-flight verify as stalled the moment R9 runs.
		verifyStallSaneMinimum = 60 * time.Second
	)

	if configured <= 0 {
		run.Log(core.LogLevelWarn,
			"R9 verify_stall_threshold_clamped command=%s configured=%v floor=%v",
			commandID, configured, verifyStallSaneMinimum)
		configured = verifyStallSaneMinimum
	}

	path := filepath.Join(run.Deps.MaestroDir, "state", "verify", commandID+".yaml")
	cfg, err := model.LoadVerifyConfig(path)
	if err != nil {
		// LoadVerifyConfig wraps with fmt.Errorf("...: %w"), so the
		// legacy os.IsNotExist check returned false and the WARN fired
		// every scan for commands without a snapshot — including newly
		// submitted commands before Planner writes their verify config
		// (Report 2026-05-05). errors.Is unwraps the chain.
		if !errors.Is(err, fs.ErrNotExist) {
			run.Log(core.LogLevelWarn,
				"R9 verify_config_load_failed command=%s path=%s error=%v (using configured threshold)",
				commandID, path, err)
		}
		return configured
	}
	count := len(cfg.AllCommands())
	if count == 0 {
		return configured
	}
	minimum := time.Duration(count)*perCommandTimeout + verifyStallGrace
	if minimum > configured {
		return minimum
	}
	return configured
}

func r9RegisterRepairTask(run *Run, statePath, commandID, workerID, predecessorTaskID string, task *model.Task) error {
	if err := r9RegisterRepairTaskInState(run, statePath, commandID, predecessorTaskID, task.ID); err != nil {
		return err
	}
	if err := r1AddTaskToQueue(run, workerID, task); err != nil {
		if rollbackErr := r9RollbackRepairTaskState(run, statePath, commandID, predecessorTaskID, task.ID); rollbackErr != nil {
			if markErr := r9MarkRetryEnqueueFailed(run, statePath, commandID, workerID, task.ID); markErr != nil {
				return errors.Join(
					fmt.Errorf("queue add failed: %w", err),
					fmt.Errorf("state rollback failed: %w", rollbackErr),
					fmt.Errorf("mark retry enqueue failed: %w", markErr),
				)
			}
			return errors.Join(
				fmt.Errorf("queue add failed: %w", err),
				fmt.Errorf("state rollback failed: %w; marked retry_enqueue_failed", rollbackErr),
			)
		}
		return err
	}
	return nil
}

// r9RegisterRepairTaskInState wires the repair task into the command state
// via the shared model.WireRetryTaskIntoState — the same wiring the daemon
// retry pipeline uses (lineage, membership replacement, phase reopen,
// dependency inheritance). Registering only TaskStates here used to leave
// the stalled predecessor in RequiredTaskIDs without lineage, so its
// cancelled marker after the repair completed read as a hard cancel and
// cascaded the phase to Cancelled even though the repair succeeded.
func r9RegisterRepairTaskInState(run *Run, statePath, commandID, predecessorTaskID, taskID string) error {
	var writeErr error
	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			writeErr = err
			return
		}
		nowStr := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
		if !model.WireRetryTaskIntoState(state, taskID, predecessorTaskID, nowStr) {
			run.Log(core.LogLevelWarn,
				"R9 verify_repair_predecessor_unmembered command=%s repair_id=%s predecessor=%s (added to optional_task_ids as fallback)",
				commandID, taskID, predecessorTaskID)
		}
		writeErr = yamlutil.AtomicWrite(statePath, state)
	})
	return writeErr
}

func r9RollbackRepairTaskState(run *Run, statePath, commandID, predecessorTaskID, taskID string) error {
	var rollbackErr error
	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			rollbackErr = err
			return
		}
		model.UnwireRetryTaskFromState(state, taskID, predecessorTaskID,
			run.Deps.Clock.Now().UTC().Format(time.RFC3339))
		if err := yamlutil.AtomicWrite(statePath, state); err != nil {
			rollbackErr = err
			run.Log(core.LogLevelError,
				"R9 verify_repair_state_rollback_failed command=%s repair_id=%s error=%v",
				commandID, taskID, err)
		}
	})
	return rollbackErr
}

func r9MarkRetryEnqueueFailed(run *Run, statePath, commandID, workerID, taskID string) error {
	var markErr error
	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			markErr = err
			return
		}
		if state.RetryEnqueueFailed == nil {
			state.RetryEnqueueFailed = make(map[string]string)
		}
		state.RetryEnqueueFailed[taskID] = formatRetryEnqueueValue(workerID, 0)
		state.UpdatedAt = run.Deps.Clock.Now().UTC().Format(time.RFC3339)
		markErr = yamlutil.AtomicWrite(statePath, state)
	})
	return markErr
}

// r9AdvanceRepairPendingToReplan moves a repair_pending task to
// paused_for_replan and queues the durable Planner signal. Returns whether
// the transition was applied (false when the slot already moved on or the
// state write failed).
func r9AdvanceRepairPendingToReplan(run *Run, statePath, commandID, taskID, reason string) bool {
	advanced := false
	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil || state.TaskStates == nil || state.TaskStates[taskID] != model.StatusRepairPending {
			return
		}
		now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
		state.SetTaskState(taskID, model.StatusPausedForReplan, now)
		state.UpdatedAt = now
		if err := yamlutil.AtomicWrite(statePath, state); err != nil {
			run.Log(core.LogLevelWarn,
				"R9 verify_repair_replan_signal_failed command=%s task=%s error=%v",
				commandID, taskID, err)
			return
		}
		advanced = true
	})
	if advanced {
		r9QueuePausedForReplanSignal(run, commandID, taskID, reason)
	}
	return advanced
}

func r9QueuePausedForReplanSignal(run *Run, commandID, taskID, reason string) {
	now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
	// Planner-signal dedup is keyed on (Kind, CommandID, PhaseID, WorkerID,
	// ConflictGeneration) — see plannerSignalDuplicate / signalDedupKey for
	// the canonical definition. paused_for_replan signals embed the task ID
	// in PhaseID via the "__task_" prefix so each task gets its own
	// dedup slot even when multiple tasks under the same command stall in
	// the same scan cycle. WorkerID and ConflictGeneration are zero-valued
	// for this signal kind, which is intentional.
	phaseID := "__task_" + taskID
	upsertPlannerSignal(run, model.PlannerSignal{
		Kind:      "paused_for_replan",
		CommandID: commandID,
		PhaseID:   phaseID,
		Reason:    reason,
		Message: fmt.Sprintf("[maestro] kind:paused_for_replan command_id:%s task_id:%s\nreason: %s\nnext_action: add_retry_task or fail the command",
			commandID, taskID, reason),
		CreatedAt: now,
		UpdatedAt: now,
	})
}

func r9SetQueueTaskTerminalStatus(run *Run, workerID, taskID string, status model.Status) {
	if workerID == "" || taskID == "" || !model.IsTerminal(status) {
		return
	}
	queuePath := taskQueuePath(run.Deps.MaestroDir, workerID)
	run.Deps.LockMap.WithLock("queue:"+workerID, func() {
		data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is in a controlled queue directory
		if err != nil {
			if !os.IsNotExist(err) {
				run.Log(core.LogLevelWarn,
					"R9 verify_queue_status_load_failed worker=%s task=%s status=%s error=%v",
					workerID, taskID, status, err)
			}
			return
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			run.Log(core.LogLevelWarn,
				"R9 verify_queue_status_parse_failed worker=%s task=%s status=%s error=%v",
				workerID, taskID, status, err)
			return
		}
		for i := range tq.Tasks {
			if tq.Tasks[i].ID != taskID {
				continue
			}
			if model.IsTerminal(tq.Tasks[i].Status) {
				return
			}
			tq.Tasks[i].Status = status
			tq.Tasks[i].UpdatedAt = run.Deps.Clock.Now().UTC().Format(time.RFC3339)
			if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
				run.Log(core.LogLevelWarn,
					"R9 verify_queue_status_write_failed worker=%s task=%s status=%s error=%v",
					workerID, taskID, status, err)
			}
			return
		}
	})
}

// r9LoadResultTimestamps walks results/worker*.yaml and returns the most
// recent CreatedAt for each (commandID, taskID) pair. The latest result is
// preferred because verify_pending is set immediately after a worker writes a
// result; older results from earlier attempts must not gate stall detection.
func r9LoadResultTimestamps(run *Run) map[string]map[string]time.Time {
	out := make(map[string]map[string]time.Time)

	resultsDir := filepath.Join(run.Deps.MaestroDir, "results")
	entries, err := run.cachedReadDir(resultsDir)
	if err != nil {
		return out
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		path := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(path) //nolint:gosec // path is in a controlled application directory
		if err != nil {
			continue
		}
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			continue
		}
		for _, res := range rf.Results {
			if res.CreatedAt == "" {
				continue
			}
			t, perr := time.Parse(time.RFC3339, res.CreatedAt)
			if perr != nil {
				continue
			}
			byCommand, ok := out[res.CommandID]
			if !ok {
				byCommand = make(map[string]time.Time)
				out[res.CommandID] = byCommand
			}
			if existing, exists := byCommand[res.TaskID]; !exists || t.After(existing) {
				byCommand[res.TaskID] = t
			}
		}
	}

	return out
}
