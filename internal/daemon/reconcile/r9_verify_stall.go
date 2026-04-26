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
	}

	return Outcome{Repairs: repairs}
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

			state.TaskStates[taskID] = model.StatusRepairPending
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

		if err := r9RegisterRepairTask(run, statePath, commandID, workerID, &repairTask); err != nil {
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
	queueDir := filepath.Join(run.Deps.MaestroDir, "queue")
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
	const perCommandTimeout = 5 * time.Minute
	const verifyStallGrace = 30 * time.Second

	path := filepath.Join(run.Deps.MaestroDir, "state", "verify", commandID+".yaml")
	cfg, err := model.LoadVerifyConfig(path)
	if err != nil {
		if !os.IsNotExist(err) {
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

func r9RegisterRepairTask(run *Run, statePath, commandID, workerID string, task *model.Task) error {
	if err := r9RegisterRepairTaskInState(run, statePath, commandID, task.ID); err != nil {
		return err
	}
	if err := r1AddTaskToQueue(run, workerID, task); err != nil {
		if rollbackErr := r9RollbackRepairTaskState(run, statePath, commandID, task.ID); rollbackErr != nil {
			if markErr := r9MarkRetryEnqueueFailed(run, statePath, commandID, workerID, task.ID); markErr != nil {
				return fmt.Errorf("queue add failed: %w; state rollback failed: %v; mark retry enqueue failed: %v", err, rollbackErr, markErr)
			}
			return fmt.Errorf("queue add failed: %w; state rollback failed: %v; marked retry_enqueue_failed", err, rollbackErr)
		}
		return err
	}
	return nil
}

func r9RegisterRepairTaskInState(run *Run, statePath, commandID, taskID string) error {
	var writeErr error
	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			writeErr = err
			return
		}
		if state.TaskStates == nil {
			state.TaskStates = make(map[string]model.Status)
		}
		state.TaskStates[taskID] = model.StatusPlanned
		nowStr := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
		state.UpdatedAt = nowStr
		writeErr = yamlutil.AtomicWrite(statePath, state)
	})
	return writeErr
}

func r9RollbackRepairTaskState(run *Run, statePath, commandID, taskID string) error {
	var rollbackErr error
	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			rollbackErr = err
			return
		}
		if state.TaskStates == nil {
			rollbackErr = fmt.Errorf("task_states missing")
			return
		}
		delete(state.TaskStates, taskID)
		state.UpdatedAt = run.Deps.Clock.Now().UTC().Format(time.RFC3339)
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

func r9AdvanceRepairPendingToReplan(run *Run, statePath, commandID, taskID, reason string) {
	advanced := false
	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil || state.TaskStates == nil || state.TaskStates[taskID] != model.StatusRepairPending {
			return
		}
		state.TaskStates[taskID] = model.StatusPausedForReplan
		state.UpdatedAt = run.Deps.Clock.Now().UTC().Format(time.RFC3339)
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
}

func r9QueuePausedForReplanSignal(run *Run, commandID, taskID, reason string) {
	now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
	phaseID := "__task_" + taskID
	sig := model.PlannerSignal{
		Kind:      "paused_for_replan",
		CommandID: commandID,
		PhaseID:   phaseID,
		Reason:    reason,
		Message: fmt.Sprintf("[maestro] kind:paused_for_replan command_id:%s task_id:%s\nreason: %s\nnext_action: add_retry_task or fail the command",
			commandID, taskID, reason),
		CreatedAt: now,
		UpdatedAt: now,
	}
	signalPath := filepath.Join(run.Deps.MaestroDir, "queue", "planner_signals.yaml")
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
				"R9 paused_for_replan_signal_write_failed command=%s task=%s reason=%q error=%v",
				commandID, taskID, reason, err)
		}
	})
}

func r9SetQueueTaskTerminalStatus(run *Run, workerID, taskID string, status model.Status) {
	if workerID == "" || taskID == "" || !model.IsTerminal(status) {
		return
	}
	queuePath := filepath.Join(run.Deps.MaestroDir, "queue", workerID+".yaml")
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
