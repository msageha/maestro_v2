package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// CanCompleteFunc is the signature for plan.CanComplete to avoid import cycles.
type CanCompleteFunc func(state *model.CommandState) (model.PlanStatus, error)

// Reconciler detects and repairs inconsistencies between queue/, results/, and state/ files.
// Implements patterns R0, R0b, R1-R6.
type Reconciler struct {
	maestroDir      string
	config          model.Config
	lockMap         *lock.MutexMap
	logger          *log.Logger
	logLevel        LogLevel
	resultHandler   *ResultHandler   // for R5 notification re-issue
	executorFactory ExecutorFactory  // for R6 Planner notification
	canComplete     CanCompleteFunc  // for R4 (avoids plan→daemon import cycle)
}

// ReconcileRepair describes a single repair action performed by the reconciler.
type ReconcileRepair struct {
	Pattern   string // "R0", "R0b", "R1", "R2", "R3", "R4", "R5", "R6"
	CommandID string
	TaskID    string
	Detail    string
}

// NewReconciler creates a new Reconciler.
func NewReconciler(
	maestroDir string,
	cfg model.Config,
	lockMap *lock.MutexMap,
	logger *log.Logger,
	logLevel LogLevel,
	resultHandler *ResultHandler,
	executorFactory ExecutorFactory,
) *Reconciler {
	return &Reconciler{
		maestroDir:      maestroDir,
		config:          cfg,
		lockMap:         lockMap,
		logger:          logger,
		logLevel:        logLevel,
		resultHandler:   resultHandler,
		executorFactory: executorFactory,
	}
}

// SetCanComplete sets the CanComplete function (wired after plan package init to avoid import cycles).
func (r *Reconciler) SetCanComplete(f CanCompleteFunc) {
	r.canComplete = f
}

// SetExecutorFactory overrides the executor factory for testing.
func (r *Reconciler) SetExecutorFactory(f ExecutorFactory) {
	r.executorFactory = f
}

// Reconcile runs all reconciliation patterns and returns a list of repairs made.
func (r *Reconciler) Reconcile() []ReconcileRepair {
	var repairs []ReconcileRepair

	repairs = append(repairs, r.reconcileR0()...)
	repairs = append(repairs, r.reconcileR0b()...)
	repairs = append(repairs, r.reconcileR1()...)
	repairs = append(repairs, r.reconcileR2()...)
	repairs = append(repairs, r.reconcileR3()...)
	repairs = append(repairs, r.reconcileR4()...)
	repairs = append(repairs, r.reconcileR5()...)
	repairs = append(repairs, r.reconcileR6()...)

	return repairs
}

// reconcileR0 detects plan_status: "planning" that has been stuck.
// Action: delete state file + remove queue entry. Planner will resubmit on next dispatch.
func (r *Reconciler) reconcileR0() []ReconcileRepair {
	var repairs []ReconcileRepair

	stateDir := filepath.Join(r.maestroDir, "state", "commands")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if !os.IsNotExist(err) {
			r.log(LogLevelWarn, "R0 read_state_dir error=%v", err)
		}
		return nil
	}

	threshold := r.stuckThresholdSec()

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		statePath := filepath.Join(stateDir, entry.Name())

		// Acquire per-command lock for state file access
		lockKey := "state:" + commandID
		r.lockMap.Lock(lockKey)

		state, err := r.loadState(statePath)
		if err != nil {
			r.lockMap.Unlock(lockKey)
			r.log(LogLevelWarn, "R0 load_state file=%s error=%v", entry.Name(), err)
			continue
		}

		if state.PlanStatus != model.PlanStatusPlanning {
			r.lockMap.Unlock(lockKey)
			continue
		}

		createdAt, err := time.Parse(time.RFC3339, state.CreatedAt)
		if err != nil {
			r.lockMap.Unlock(lockKey)
			r.log(LogLevelWarn, "R0 parse_created_at command=%s error=%v", state.CommandID, err)
			continue
		}

		ageSec := time.Since(createdAt).Seconds()
		if ageSec < float64(threshold) {
			r.lockMap.Unlock(lockKey)
			continue
		}

		r.log(LogLevelWarn, "R0 planning_stuck command=%s age_sec=%.0f threshold=%d",
			state.CommandID, ageSec, threshold)

		// Delete state file (rollback partial submit)
		if err := os.Remove(statePath); err != nil {
			r.lockMap.Unlock(lockKey)
			r.log(LogLevelError, "R0 delete_state command=%s error=%v", state.CommandID, err)
			continue
		}
		r.lockMap.Unlock(lockKey)

		// Remove command from planner queue entirely to prevent re-dispatch loops.
		r.removeCommandFromPlannerQueue(state.CommandID)

		// Remove any tasks from worker queues for this command
		r.removeTasksFromWorkerQueues(state.CommandID)

		repairs = append(repairs, ReconcileRepair{
			Pattern:   "R0",
			CommandID: state.CommandID,
			Detail:    fmt.Sprintf("planning stuck %.0fs, state deleted + command removed from queue + worker tasks removed", ageSec),
		})
	}

	return repairs
}

// reconcileR0b detects phases stuck in "filling" status.
// Action: revert to awaiting_fill, remove partially added tasks.
func (r *Reconciler) reconcileR0b() []ReconcileRepair {
	var repairs []ReconcileRepair

	stateDir := filepath.Join(r.maestroDir, "state", "commands")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}

	threshold := r.stuckThresholdSec()

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		statePath := filepath.Join(stateDir, entry.Name())

		r.lockMap.Lock("state:" + strings.TrimSuffix(entry.Name(), ".yaml"))

		state, err := r.loadState(statePath)
		if err != nil {
			r.lockMap.Unlock("state:" + strings.TrimSuffix(entry.Name(), ".yaml"))
			continue
		}

		modified := false
		for i := range state.Phases {
			phase := &state.Phases[i]
			if phase.Status != model.PhaseStatusFilling {
				continue
			}

			// Check if filling has been stuck based on state update time
			updatedAt, err := time.Parse(time.RFC3339, state.UpdatedAt)
			if err != nil {
				continue
			}

			ageSec := time.Since(updatedAt).Seconds()
			if ageSec < float64(threshold) {
				continue
			}

			r.log(LogLevelWarn, "R0b filling_stuck command=%s phase=%s age_sec=%.0f",
				state.CommandID, phase.Name, ageSec)

			// Remove partially added task IDs from worker queues
			for _, taskID := range phase.TaskIDs {
				r.removeTaskFromWorkerQueues(taskID)
			}

			// Clear phase task_ids and revert to awaiting_fill
			phase.TaskIDs = nil
			phase.Status = model.PhaseStatusAwaitingFill
			modified = true

			repairs = append(repairs, ReconcileRepair{
				Pattern:   "R0b",
				CommandID: state.CommandID,
				Detail:    fmt.Sprintf("phase %s filling stuck %.0fs, reverted to awaiting_fill", phase.Name, ageSec),
			})
		}

		if modified {
			now := time.Now().UTC().Format(time.RFC3339)
			state.LastReconciledAt = &now
			state.UpdatedAt = now
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				r.log(LogLevelError, "R0b write_state command=%s error=%v", state.CommandID, err)
			}
		}

		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		r.lockMap.Unlock("state:" + commandID)

		// Notify Planner to re-fill reverted phases (best-effort, outside lock)
		if modified && r.executorFactory != nil {
			r.notifyPlannerOfReFill(commandID)
		}
	}

	return repairs
}

// notifyPlannerOfReFill sends a re-fill notification to Planner after R0b reversion.
func (r *Reconciler) notifyPlannerOfReFill(commandID string) {
	exec, err := r.executorFactory(r.maestroDir, r.config.Watcher, r.config.Logging.Level)
	if err != nil {
		r.log(LogLevelWarn, "R0b notify_planner create_executor error=%v", err)
		return
	}
	defer exec.Close()

	message := fmt.Sprintf("[maestro] kind:re_fill command_id:%s\nphase filling was stuck, reverted to awaiting_fill — please re-submit tasks",
		commandID)

	result := exec.Execute(agent.ExecRequest{
		AgentID:   "planner",
		Message:   message,
		Mode:      agent.ModeDeliver,
		CommandID: commandID,
	})
	if result.Error != nil {
		r.log(LogLevelWarn, "R0b notify_planner command=%s error=%v", commandID, result.Error)
	}
}

// reconcileR1 detects results/ terminal + queue/ in_progress mismatch.
// Action: update queue to terminal, clear lease. Update last_reconciled_at on state file.
func (r *Reconciler) reconcileR1() []ReconcileRepair {
	var repairs []ReconcileRepair
	repairedCommands := make(map[string]bool)

	resultsDir := filepath.Join(r.maestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := strings.TrimSuffix(name, ".yaml")
		resultPath := filepath.Join(resultsDir, name)
		queuePath := filepath.Join(r.maestroDir, "queue", name)

		resultData, err := os.ReadFile(resultPath)
		if err != nil {
			continue
		}
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(resultData, &rf); err != nil {
			continue
		}

		// Build a map of terminal results keyed by task_id
		terminalResults := make(map[string]model.Status)
		for _, result := range rf.Results {
			if model.IsTerminal(result.Status) {
				terminalResults[result.TaskID] = result.Status
			}
		}
		if len(terminalResults) == 0 {
			continue
		}

		// Load queue file
		queueData, err := os.ReadFile(queuePath)
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(queueData, &tq); err != nil {
			continue
		}

		queueModified := false
		for i := range tq.Tasks {
			task := &tq.Tasks[i]
			if task.Status != model.StatusInProgress {
				continue
			}

			resultStatus, found := terminalResults[task.ID]
			if !found {
				continue
			}

			r.log(LogLevelWarn, "R1 result_terminal_queue_inprogress worker=%s task=%s result_status=%s",
				workerID, task.ID, resultStatus)

			task.Status = resultStatus
			task.LeaseOwner = nil
			task.LeaseExpiresAt = nil
			task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			queueModified = true
			repairedCommands[task.CommandID] = true

			repairs = append(repairs, ReconcileRepair{
				Pattern:   "R1",
				CommandID: task.CommandID,
				TaskID:    task.ID,
				Detail:    fmt.Sprintf("queue %s updated from in_progress to %s", workerID, resultStatus),
			})
		}

		if queueModified {
			if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
				r.log(LogLevelError, "R1 write_queue worker=%s error=%v", workerID, err)
			}
		}
	}

	// Update last_reconciled_at on affected state files (spec: all repairs update state)
	for commandID := range repairedCommands {
		r.updateLastReconciledAt(commandID)
	}

	return repairs
}

// reconcileR2 detects results/worker terminal + state/ non-terminal mismatch.
// Action: update task_states + applied_result_ids in state file.
func (r *Reconciler) reconcileR2() []ReconcileRepair {
	var repairs []ReconcileRepair

	resultsDir := filepath.Join(r.maestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return nil
	}

	// Collect all terminal results by command_id
	type resultEntry struct {
		TaskID   string
		ResultID string
		Status   model.Status
	}
	commandResults := make(map[string][]resultEntry)

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		resultPath := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(resultPath)
		if err != nil {
			continue
		}
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			continue
		}

		for _, result := range rf.Results {
			if model.IsTerminal(result.Status) {
				commandResults[result.CommandID] = append(commandResults[result.CommandID], resultEntry{
					TaskID:   result.TaskID,
					ResultID: result.ID,
					Status:   result.Status,
				})
			}
		}
	}

	// Check each command's state file
	for commandID, results := range commandResults {
		statePath := filepath.Join(r.maestroDir, "state", "commands", commandID+".yaml")

		lockKey := "state:" + commandID
		r.lockMap.Lock(lockKey)

		state, err := r.loadState(statePath)
		if err != nil {
			r.lockMap.Unlock(lockKey)
			continue
		}

		if state.TaskStates == nil {
			state.TaskStates = make(map[string]model.Status)
		}
		if state.AppliedResultIDs == nil {
			state.AppliedResultIDs = make(map[string]string)
		}

		modified := false
		for _, re := range results {
			currentStatus, exists := state.TaskStates[re.TaskID]
			if exists && model.IsTerminal(currentStatus) {
				continue // Already terminal in state
			}

			r.log(LogLevelWarn, "R2 result_terminal_state_nonterminal command=%s task=%s result_status=%s state_status=%s",
				commandID, re.TaskID, re.Status, currentStatus)

			state.TaskStates[re.TaskID] = re.Status
			state.AppliedResultIDs[re.TaskID] = re.ResultID
			modified = true

			repairs = append(repairs, ReconcileRepair{
				Pattern:   "R2",
				CommandID: commandID,
				TaskID:    re.TaskID,
				Detail:    fmt.Sprintf("state task_states updated from %s to %s", currentStatus, re.Status),
			})
		}

		if modified {
			now := time.Now().UTC().Format(time.RFC3339)
			state.LastReconciledAt = &now
			state.UpdatedAt = now
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				r.log(LogLevelError, "R2 write_state command=%s error=%v", commandID, err)
			}
		}

		r.lockMap.Unlock(lockKey)
	}

	return repairs
}

// reconcileR3 detects results/planner terminal + queue/planner in_progress mismatch.
// Action: update queue to terminal, clear lease. Update last_reconciled_at on state file.
func (r *Reconciler) reconcileR3() []ReconcileRepair {
	var repairs []ReconcileRepair
	repairedCommands := make(map[string]bool)

	resultPath := filepath.Join(r.maestroDir, "results", "planner.yaml")
	rf, err := r.loadCommandResultFile(resultPath)
	if err != nil {
		return nil
	}

	// Build map of terminal results keyed by command_id
	terminalResults := make(map[string]model.Status)
	for _, result := range rf.Results {
		if model.IsTerminal(result.Status) {
			terminalResults[result.CommandID] = result.Status
		}
	}
	if len(terminalResults) == 0 {
		return nil
	}

	queuePath := filepath.Join(r.maestroDir, "queue", "planner.yaml")
	queueData, err := os.ReadFile(queuePath)
	if err != nil {
		return nil
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(queueData, &cq); err != nil {
		return nil
	}

	queueModified := false
	for i := range cq.Commands {
		cmd := &cq.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}

		resultStatus, found := terminalResults[cmd.ID]
		if !found {
			continue
		}

		r.log(LogLevelWarn, "R3 result_terminal_queue_inprogress command=%s result_status=%s",
			cmd.ID, resultStatus)

		cmd.Status = resultStatus
		cmd.LeaseOwner = nil
		cmd.LeaseExpiresAt = nil
		cmd.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		queueModified = true
		repairedCommands[cmd.ID] = true

		repairs = append(repairs, ReconcileRepair{
			Pattern:   "R3",
			CommandID: cmd.ID,
			Detail:    fmt.Sprintf("queue planner updated from in_progress to %s", resultStatus),
		})
	}

	if queueModified {
		if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
			r.log(LogLevelError, "R3 write_queue error=%v", err)
		}
	}

	for commandID := range repairedCommands {
		r.updateLastReconciledAt(commandID)
	}

	return repairs
}

// reconcileR4 detects results/planner terminal + state non-terminal plan_status.
// Action: re-evaluate via plan.CanComplete. If OK, update plan_status. If NG, quarantine result.
func (r *Reconciler) reconcileR4() []ReconcileRepair {
	var repairs []ReconcileRepair

	resultPath := filepath.Join(r.maestroDir, "results", "planner.yaml")
	rf, err := r.loadCommandResultFile(resultPath)
	if err != nil {
		return nil
	}

	for _, result := range rf.Results {
		if !model.IsTerminal(result.Status) {
			continue
		}

		commandID := result.CommandID
		statePath := filepath.Join(r.maestroDir, "state", "commands", commandID+".yaml")

		lockKey := "state:" + commandID
		r.lockMap.Lock(lockKey)

		state, err := r.loadState(statePath)
		if err != nil {
			r.lockMap.Unlock(lockKey)
			continue
		}

		// Skip if plan_status is already terminal
		if model.IsPlanTerminal(state.PlanStatus) {
			r.lockMap.Unlock(lockKey)
			continue
		}

		// Skip if plan_status is "planning" (R0 handles this)
		if state.PlanStatus == model.PlanStatusPlanning {
			r.lockMap.Unlock(lockKey)
			continue
		}

		// plan_status must be "sealed" for CanComplete
		r.log(LogLevelWarn, "R4 result_terminal_state_nonterminal command=%s result_status=%s plan_status=%s",
			commandID, result.Status, state.PlanStatus)

		if r.canComplete == nil {
			r.lockMap.Unlock(lockKey)
			r.log(LogLevelWarn, "R4 skipped command=%s (canComplete not wired)", commandID)
			continue
		}

		derivedStatus, canCompleteErr := r.canComplete(state)
		if canCompleteErr != nil {
			r.lockMap.Unlock(lockKey)
			// CanComplete failed → quarantine the result entry + notify Planner
			r.log(LogLevelWarn, "R4 can_complete_failed command=%s error=%v → quarantine result + notify planner",
				commandID, canCompleteErr)
			if err := r.quarantineCommandResult(resultPath, result); err != nil {
				r.log(LogLevelError, "R4 quarantine command=%s error=%v", commandID, err)
			}
			// Notify Planner to re-evaluate the command (best-effort)
			r.notifyPlannerOfReEvaluation(commandID, canCompleteErr.Error())
			repairs = append(repairs, ReconcileRepair{
				Pattern:   "R4",
				CommandID: commandID,
				Detail:    fmt.Sprintf("can_complete failed (%v), result quarantined, planner notified", canCompleteErr),
			})
			continue
		}

		// Update plan_status to derived status
		state.PlanStatus = derivedStatus
		now := time.Now().UTC().Format(time.RFC3339)
		state.LastReconciledAt = &now
		state.UpdatedAt = now
		if err := yamlutil.AtomicWrite(statePath, state); err != nil {
			r.log(LogLevelError, "R4 write_state command=%s error=%v", commandID, err)
		}
		r.lockMap.Unlock(lockKey)

		repairs = append(repairs, ReconcileRepair{
			Pattern:   "R4",
			CommandID: commandID,
			Detail:    fmt.Sprintf("plan_status updated to %s via can_complete", derivedStatus),
		})
	}

	return repairs
}

// reconcileR5 detects results/planner terminal + notified but no orchestrator notification.
// Action: re-issue notification via writeNotificationToOrchestratorQueue.
func (r *Reconciler) reconcileR5() []ReconcileRepair {
	var repairs []ReconcileRepair

	if r.resultHandler == nil {
		return nil
	}

	resultPath := filepath.Join(r.maestroDir, "results", "planner.yaml")
	rf, err := r.loadCommandResultFile(resultPath)
	if err != nil {
		return nil
	}

	// Load orchestrator notification queue to check existing notifications
	nqPath := filepath.Join(r.maestroDir, "queue", "orchestrator.yaml")
	nqData, err := os.ReadFile(nqPath)
	if err != nil && !os.IsNotExist(err) {
		return nil
	}
	var nq model.NotificationQueue
	if err == nil {
		if err := yamlv3.Unmarshal(nqData, &nq); err != nil {
			return nil
		}
	}

	// Build set of existing source_result_ids
	existingSourceIDs := make(map[string]bool)
	for _, ntf := range nq.Notifications {
		if ntf.SourceResultID != "" {
			existingSourceIDs[ntf.SourceResultID] = true
		}
	}

	repairedCommands := make(map[string]bool)
	for _, result := range rf.Results {
		if !model.IsTerminal(result.Status) {
			continue
		}
		if !result.Notified {
			continue // Not yet notified → not R5's concern
		}

		// Check if corresponding orchestrator notification exists
		if existingSourceIDs[result.ID] {
			continue // Notification already exists
		}

		r.log(LogLevelWarn, "R5 notified_result_no_orchestrator_notification command=%s result=%s",
			result.CommandID, result.ID)

		// Re-issue notification
		if err := r.resultHandler.writeNotificationToOrchestratorQueue(result.ID, result.CommandID, result.Status); err != nil {
			r.log(LogLevelError, "R5 write_notification command=%s error=%v", result.CommandID, err)
			continue
		}

		repairedCommands[result.CommandID] = true
		repairs = append(repairs, ReconcileRepair{
			Pattern:   "R5",
			CommandID: result.CommandID,
			Detail:    fmt.Sprintf("orchestrator notification re-issued for result %s", result.ID),
		})
	}

	for commandID := range repairedCommands {
		r.updateLastReconciledAt(commandID)
	}

	return repairs
}

// reconcileR6 detects awaiting_fill + fill_deadline_at expired.
// Action: set phase to timed_out, cascade cancel downstream pending phases, notify Planner.
func (r *Reconciler) reconcileR6() []ReconcileRepair {
	var repairs []ReconcileRepair

	stateDir := filepath.Join(r.maestroDir, "state", "commands")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		statePath := filepath.Join(stateDir, entry.Name())

		lockKey := "state:" + commandID
		r.lockMap.Lock(lockKey)

		state, err := r.loadState(statePath)
		if err != nil {
			r.lockMap.Unlock(lockKey)
			continue
		}

		if len(state.Phases) == 0 {
			r.lockMap.Unlock(lockKey)
			continue
		}

		modified := false
		timedOutPhases := make(map[string]bool)

		for i := range state.Phases {
			phase := &state.Phases[i]
			if phase.Status != model.PhaseStatusAwaitingFill {
				continue
			}
			if phase.FillDeadlineAt == nil {
				continue
			}

			deadline, err := time.Parse(time.RFC3339, *phase.FillDeadlineAt)
			if err != nil {
				continue
			}

			if time.Now().UTC().Before(deadline) {
				continue
			}

			r.log(LogLevelWarn, "R6 awaiting_fill_deadline_expired command=%s phase=%s deadline=%s",
				commandID, phase.Name, *phase.FillDeadlineAt)

			phase.Status = model.PhaseStatusTimedOut
			modified = true
			timedOutPhases[phase.Name] = true

			repairs = append(repairs, ReconcileRepair{
				Pattern:   "R6",
				CommandID: commandID,
				Detail:    fmt.Sprintf("phase %s timed_out (deadline %s)", phase.Name, *phase.FillDeadlineAt),
			})
		}

		// Cascade cancel: transitive closure over downstream phases.
		// A phase is cancelled if any of its dependencies are timed_out or
		// already cascade-cancelled. We loop until no new cancellations occur.
		if len(timedOutPhases) > 0 {
			cancelledPhases := make(map[string]bool)
			for name := range timedOutPhases {
				cancelledPhases[name] = true
			}

			for {
				changed := false
				for i := range state.Phases {
					phase := &state.Phases[i]
					if phase.Status != model.PhaseStatusPending && phase.Status != model.PhaseStatusAwaitingFill {
						continue
					}
					for _, dep := range phase.DependsOnPhases {
						if cancelledPhases[dep] {
							r.log(LogLevelWarn, "R6 cascade_cancel command=%s phase=%s (depends on %s)",
								commandID, phase.Name, dep)
							phase.Status = model.PhaseStatusCancelled
							modified = true
							changed = true
							cancelledPhases[phase.Name] = true

							repairs = append(repairs, ReconcileRepair{
								Pattern:   "R6",
								CommandID: commandID,
								Detail:    fmt.Sprintf("phase %s cancelled (cascade from %s)", phase.Name, dep),
							})
							break
						}
					}
				}
				if !changed {
					break
				}
			}
		}

		if modified {
			now := time.Now().UTC().Format(time.RFC3339)
			state.LastReconciledAt = &now
			state.UpdatedAt = now
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				r.log(LogLevelError, "R6 write_state command=%s error=%v", commandID, err)
			}
		}

		r.lockMap.Unlock(lockKey)

		// Notify Planner (best-effort, outside lock)
		if modified && r.executorFactory != nil {
			r.notifyPlannerOfTimeout(commandID, timedOutPhases)
		}
	}

	return repairs
}

// notifyPlannerOfTimeout sends a fill timeout notification to Planner via agent executor.
func (r *Reconciler) notifyPlannerOfTimeout(commandID string, timedOutPhases map[string]bool) {
	exec, err := r.executorFactory(r.maestroDir, r.config.Watcher, r.config.Logging.Level)
	if err != nil {
		r.log(LogLevelWarn, "R6 notify_planner create_executor error=%v", err)
		return
	}
	defer exec.Close()

	phases := make([]string, 0, len(timedOutPhases))
	for name := range timedOutPhases {
		phases = append(phases, name)
	}
	message := fmt.Sprintf("[maestro] kind:fill_timeout command_id:%s phases:%s\nfill deadline expired, phases timed out",
		commandID, strings.Join(phases, ","))

	result := exec.Execute(agent.ExecRequest{
		AgentID:   "planner",
		Message:   message,
		Mode:      agent.ModeDeliver,
		CommandID: commandID,
	})
	if result.Error != nil {
		r.log(LogLevelWarn, "R6 notify_planner command=%s error=%v", commandID, result.Error)
	}
}

// notifyPlannerOfReEvaluation sends a re-evaluation notification to Planner
// after R4 quarantine (best-effort).
func (r *Reconciler) notifyPlannerOfReEvaluation(commandID, reason string) {
	exec, err := r.executorFactory(r.maestroDir, r.config.Watcher, r.config.Logging.Level)
	if err != nil {
		r.log(LogLevelWarn, "R4 notify_planner create_executor error=%v", err)
		return
	}
	defer exec.Close()

	message := fmt.Sprintf("[maestro] kind:re_evaluate command_id:%s\ncan_complete failed: %s — result quarantined, please re-evaluate",
		commandID, reason)

	result := exec.Execute(agent.ExecRequest{
		AgentID:   "planner",
		Message:   message,
		Mode:      agent.ModeDeliver,
		CommandID: commandID,
	})
	if result.Error != nil {
		r.log(LogLevelWarn, "R4 notify_planner command=%s error=%v", commandID, result.Error)
	}
}

// loadCommandResultFile loads results/planner.yaml.
func (r *Reconciler) loadCommandResultFile(path string) (*model.CommandResultFile, error) {
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

// quarantineCommandResult removes a specific result from results/planner.yaml and writes it to quarantine/.
func (r *Reconciler) quarantineCommandResult(resultPath string, result model.CommandResult) error {
	// Write to quarantine directory
	quarantineDir := filepath.Join(r.maestroDir, "quarantine")
	if err := os.MkdirAll(quarantineDir, 0755); err != nil {
		return fmt.Errorf("create quarantine dir: %w", err)
	}

	quarantineFile := filepath.Join(quarantineDir,
		fmt.Sprintf("res_%s_%s.yaml", time.Now().UTC().Format("20060102T150405Z"), result.ID))

	if err := yamlutil.AtomicWrite(quarantineFile, result); err != nil {
		return fmt.Errorf("write quarantine file: %w", err)
	}

	// Remove from results/planner.yaml
	r.lockMap.Lock("result:planner")
	defer r.lockMap.Unlock("result:planner")

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

// --- Helpers ---

// stuckThresholdSec returns the threshold in seconds for considering a state "stuck".
// Uses dispatch_lease_sec * 2 as a generous threshold.
func (r *Reconciler) stuckThresholdSec() int {
	leaseSec := r.config.Watcher.DispatchLeaseSec
	if leaseSec <= 0 {
		leaseSec = 300
	}
	return leaseSec * 2
}

func (r *Reconciler) loadState(path string) (*model.CommandState, error) {
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

// removeCommandFromPlannerQueue removes a command from queue/planner.yaml entirely.
func (r *Reconciler) removeCommandFromPlannerQueue(commandID string) {
	queuePath := filepath.Join(r.maestroDir, "queue", "planner.yaml")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		return
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return
	}

	filtered := make([]model.Command, 0, len(cq.Commands))
	for _, cmd := range cq.Commands {
		if cmd.ID != commandID {
			filtered = append(filtered, cmd)
		}
	}
	if len(filtered) == len(cq.Commands) {
		return // not found
	}
	cq.Commands = filtered

	if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
		r.log(LogLevelError, "R0 remove_command queue=%s error=%v", commandID, err)
	}
}

// removeTasksFromWorkerQueues removes all tasks for a given command from all worker queues.
func (r *Reconciler) removeTasksFromWorkerQueues(commandID string) {
	queueDir := filepath.Join(r.maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		queuePath := filepath.Join(queueDir, name)
		data, err := os.ReadFile(queuePath)
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			continue
		}

		filtered := make([]model.Task, 0, len(tq.Tasks))
		for _, task := range tq.Tasks {
			if task.CommandID != commandID {
				filtered = append(filtered, task)
			}
		}
		if len(filtered) == len(tq.Tasks) {
			continue // No tasks removed
		}
		tq.Tasks = filtered

		if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
			r.log(LogLevelError, "R0 remove_tasks file=%s command=%s error=%v", name, commandID, err)
		}
	}
}

// removeTaskFromWorkerQueues removes a specific task ID from all worker queues.
func (r *Reconciler) removeTaskFromWorkerQueues(taskID string) {
	queueDir := filepath.Join(r.maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		queuePath := filepath.Join(queueDir, name)
		data, err := os.ReadFile(queuePath)
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			continue
		}

		filtered := make([]model.Task, 0, len(tq.Tasks))
		for _, task := range tq.Tasks {
			if task.ID != taskID {
				filtered = append(filtered, task)
			}
		}
		if len(filtered) == len(tq.Tasks) {
			continue
		}
		tq.Tasks = filtered

		if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
			r.log(LogLevelError, "R0b remove_task file=%s task=%s error=%v", name, taskID, err)
		}
	}
}

// updateLastReconciledAt updates last_reconciled_at on a state file.
// Used by R1 (and any future patterns) that don't directly modify the state file.
func (r *Reconciler) updateLastReconciledAt(commandID string) {
	statePath := filepath.Join(r.maestroDir, "state", "commands", commandID+".yaml")

	lockKey := "state:" + commandID
	r.lockMap.Lock(lockKey)
	defer r.lockMap.Unlock(lockKey)

	state, err := r.loadState(statePath)
	if err != nil {
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	state.LastReconciledAt = &now
	state.UpdatedAt = now
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		r.log(LogLevelError, "update_last_reconciled command=%s error=%v", commandID, err)
	}
}

// --- Logging ---

func (r *Reconciler) log(level LogLevel, format string, args ...any) {
	if level < r.logLevel {
		return
	}
	levelStr := "INFO"
	switch level {
	case LogLevelDebug:
		levelStr = "DEBUG"
	case LogLevelWarn:
		levelStr = "WARN"
	case LogLevelError:
		levelStr = "ERROR"
	}
	msg := fmt.Sprintf(format, args...)
	r.logger.Printf("%s %s reconciler: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}
