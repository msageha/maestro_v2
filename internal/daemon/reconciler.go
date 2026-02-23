package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// Reconciler detects and repairs inconsistencies between queue/, results/, and state/ files.
// Phase 8 implements patterns R0, R0b, R1, R2.
type Reconciler struct {
	maestroDir string
	config     model.Config
	lockMap    *lock.MutexMap
	logger     *log.Logger
	logLevel   LogLevel
}

// ReconcileRepair describes a single repair action performed by the reconciler.
type ReconcileRepair struct {
	Pattern   string // "R0", "R0b", "R1", "R2"
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
) *Reconciler {
	return &Reconciler{
		maestroDir: maestroDir,
		config:     cfg,
		lockMap:    lockMap,
		logger:     logger,
		logLevel:   logLevel,
	}
}

// Reconcile runs all reconciliation patterns and returns a list of repairs made.
func (r *Reconciler) Reconcile() []ReconcileRepair {
	var repairs []ReconcileRepair

	repairs = append(repairs, r.reconcileR0()...)
	repairs = append(repairs, r.reconcileR0b()...)
	repairs = append(repairs, r.reconcileR1()...)
	repairs = append(repairs, r.reconcileR2()...)

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

		// Reset command in planner queue to pending (release lease for re-dispatch).
		// This serves as the "Planner re-submit notification" — the command will be
		// re-dispatched in the next scan cycle.
		r.resetCommandInPlannerQueue(state.CommandID)

		// Remove any tasks from worker queues for this command
		r.removeTasksFromWorkerQueues(state.CommandID)

		repairs = append(repairs, ReconcileRepair{
			Pattern:   "R0",
			CommandID: state.CommandID,
			Detail:    fmt.Sprintf("planning stuck %.0fs, state deleted + command reset to pending + worker tasks removed", ageSec),
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

		r.lockMap.Unlock("state:" + strings.TrimSuffix(entry.Name(), ".yaml"))
	}

	return repairs
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

// resetCommandInPlannerQueue resets a command in queue/planner.yaml to pending status.
// This releases the lease so the command gets re-dispatched to Planner (re-submit notification).
func (r *Reconciler) resetCommandInPlannerQueue(commandID string) {
	queuePath := filepath.Join(r.maestroDir, "queue", "planner.yaml")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		return
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return
	}

	found := false
	for i := range cq.Commands {
		if cq.Commands[i].ID == commandID {
			cq.Commands[i].Status = model.StatusPending
			cq.Commands[i].LeaseOwner = nil
			cq.Commands[i].LeaseExpiresAt = nil
			cq.Commands[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			found = true
			break
		}
	}
	if !found {
		return
	}

	if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
		r.log(LogLevelError, "R0 reset_command queue=%s error=%v", commandID, err)
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
