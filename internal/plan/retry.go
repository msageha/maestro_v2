package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

type RetryOptions struct {
	CommandID          string
	RetryOf            string
	Purpose            string
	Content            string
	AcceptanceCriteria string
	Constraints        []string
	BlockedBy          []string // task IDs (not names)
	BloomLevel         int
	ToolsHint          []string
	MaestroDir         string
	Config             model.Config
	LockMap            *lock.MutexMap
}

type RetryResult struct {
	TaskID           string                 `json:"task_id"`
	Worker           string                 `json:"worker"`
	Model            string                 `json:"model"`
	Replaced         string                 `json:"replaced"`
	CascadeRecovered []CascadeRecoveredTask `json:"cascade_recovered,omitempty"`
}

type CascadeRecoveredTask struct {
	TaskID   string `json:"task_id"`
	Worker   string `json:"worker"`
	Model    string `json:"model"`
	Replaced string `json:"replaced"`
}

func AddRetryTask(opts RetryOptions) (*RetryResult, error) {
	if opts.LockMap == nil {
		return nil, fmt.Errorf("LockMap is required")
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	// Validation
	if state.PlanStatus != model.PlanStatusSealed {
		return nil, &PlanValidationError{Msg: fmt.Sprintf("plan_status must be sealed, got %s", state.PlanStatus)}
	}

	if err := ValidateNotCancelled(state); err != nil {
		return nil, err
	}

	retryOfStatus, ok := state.TaskStates[opts.RetryOf]
	if !ok {
		return nil, &PlanValidationError{Msg: fmt.Sprintf("task %s not found in state", opts.RetryOf)}
	}
	if retryOfStatus != model.StatusFailed {
		return nil, &PlanValidationError{Msg: fmt.Sprintf("retry-of task %s must be failed, got %s", opts.RetryOf, retryOfStatus)}
	}

	// Find phase membership
	phase, phaseIdx := findPhaseForTask(state, opts.RetryOf)
	if phase != nil {
		if phase.Status != model.PhaseStatusActive && phase.Status != model.PhaseStatusFailed {
			return nil, &PlanValidationError{Msg: fmt.Sprintf("phase %q status must be active or failed, got %s",
				phase.Name, phase.Status)}
		}
	}

	// Resolve blocked_by: default to original task's dependencies
	blockedBy := opts.BlockedBy
	if len(blockedBy) == 0 {
		if deps, ok := state.TaskDependencies[opts.RetryOf]; ok {
			blockedBy = resolveBlockedByViaLineage(deps, state.RetryLineage)
		}
	}

	// Validate blocked_by references
	if len(blockedBy) > 0 {
		if phase != nil {
			// Phase-scoped: blocked_by must be within same phase (or system commit)
			phaseTaskSet := make(map[string]bool)
			for _, tid := range phase.TaskIDs {
				phaseTaskSet[tid] = true
			}
			for _, dep := range blockedBy {
				if !phaseTaskSet[dep] {
					if state.SystemCommitTaskID == nil || dep != *state.SystemCommitTaskID {
						return nil, &PlanValidationError{Msg: fmt.Sprintf("blocked_by task %s is not in phase %q", dep, phase.Name)}
					}
				}
			}
		} else {
			// No phase: blocked_by must exist in command's task states
			for _, dep := range blockedBy {
				if _, ok := state.TaskStates[dep]; !ok {
					return nil, &PlanValidationError{Msg: fmt.Sprintf("blocked_by task %s not found in command state", dep)}
				}
			}
		}
	}

	// Worker assignment
	workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
	if err != nil {
		return nil, fmt.Errorf("build worker states: %w", err)
	}

	assignReqs := []TaskAssignmentRequest{{Name: "__retry", BloomLevel: opts.BloomLevel}}
	assignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, workerStates, assignReqs)
	if err != nil {
		return nil, fmt.Errorf("worker assignment: %w", err)
	}
	assignment := assignments[0]

	// Save original state for rollback
	origStateBytes, _ := copyState(state)

	// Generate new task ID
	newTaskID, err := model.GenerateID(model.IDTypeTask)
	if err != nil {
		return nil, fmt.Errorf("generate task ID: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Replace in required/optional task IDs
	replaceInRequiredOrOptional(state, opts.RetryOf, newTaskID)

	// Record retry lineage
	state.RetryLineage[newTaskID] = opts.RetryOf

	// Rewrite dependencies
	rewriteDependencies(state, opts.RetryOf, newTaskID)

	// Set new task state
	state.TaskStates[newTaskID] = model.StatusPending
	state.TaskDependencies[newTaskID] = blockedBy

	// Add to phase
	if phase != nil {
		state.Phases[phaseIdx].TaskIDs = append(state.Phases[phaseIdx].TaskIDs, newTaskID)

		// Reopen phase if failed
		if phase.Status == model.PhaseStatusFailed {
			if err := reopenPhase(state, phaseIdx, now); err != nil {
				restoreState(state, origStateBytes)
				return nil, fmt.Errorf("reopen phase: %w", err)
			}
		}
	}

	// Cascade recovery
	origTaskCache := loadOriginalTasksFromQueue(opts.MaestroDir, opts.CommandID)
	cascadeRecovered, err := cascadeRecover(
		state, opts.RetryOf, newTaskID,
		opts.Config.Agents.Workers, opts.Config.Limits, workerStates, origTaskCache,
	)
	if err != nil {
		restoreState(state, origStateBytes)
		return nil, fmt.Errorf("cascade recovery: %w", err)
	}

	// Post-recovery DAG validation
	allNames := make([]string, 0, len(state.TaskDependencies))
	for k := range state.TaskStates {
		allNames = append(allNames, k)
	}
	if _, err := ValidateTaskDAG(allNames, state.TaskDependencies); err != nil {
		restoreState(state, origStateBytes)
		return nil, fmt.Errorf("post-recovery DAG validation: %w", err)
	}

	state.UpdatedAt = now

	// Write queue entry for the primary retry task
	var writtenQueueTaskIDs []retryQueueTask // track for rollback
	primaryTask := retryQueueTask{
		taskID:             newTaskID,
		commandID:          opts.CommandID,
		purpose:            opts.Purpose,
		content:            opts.Content,
		acceptanceCriteria: opts.AcceptanceCriteria,
		constraints:        opts.Constraints,
		blockedBy:          blockedBy,
		bloomLevel:         opts.BloomLevel,
		toolsHint:          opts.ToolsHint,
		workerID:           assignment.WorkerID,
	}

	if err := writeRetryQueueEntry(opts.MaestroDir, primaryTask, now); err != nil {
		restoreState(state, origStateBytes)
		return nil, fmt.Errorf("write queue entry for %s: %w", newTaskID, err)
	}
	writtenQueueTaskIDs = append(writtenQueueTaskIDs, primaryTask)

	// Write queue entries for cascade recovered tasks
	for _, cr := range cascadeRecovered {
		// Inherit content from original task if available
		purpose := "cascade recovery of " + cr.Replaced
		content := purpose
		acceptanceCriteria := purpose
		bloomLevel := opts.BloomLevel
		var constraints []string
		var toolsHint []string

		if orig, ok := origTaskCache[cr.Replaced]; ok {
			purpose = orig.Purpose
			content = orig.Content
			acceptanceCriteria = orig.AcceptanceCriteria
			bloomLevel = orig.BloomLevel
			constraints = orig.Constraints
			toolsHint = orig.ToolsHint
		}

		crTask := retryQueueTask{
			taskID:             cr.TaskID,
			commandID:          opts.CommandID,
			purpose:            purpose,
			content:            content,
			acceptanceCriteria: acceptanceCriteria,
			constraints:        constraints,
			blockedBy:          state.TaskDependencies[cr.TaskID],
			bloomLevel:         bloomLevel,
			toolsHint:          toolsHint,
			workerID:           cr.Worker,
		}
		if err := writeRetryQueueEntry(opts.MaestroDir, crTask, now); err != nil {
			rollbackRetryQueueEntries(opts.MaestroDir, writtenQueueTaskIDs)
			restoreState(state, origStateBytes)
			return nil, fmt.Errorf("write queue entry for cascade %s: %w", cr.TaskID, err)
		}
		writtenQueueTaskIDs = append(writtenQueueTaskIDs, crTask)
	}

	// Save state
	if err := sm.SaveState(state); err != nil {
		rollbackRetryQueueEntries(opts.MaestroDir, writtenQueueTaskIDs)
		restoreState(state, origStateBytes)
		return nil, fmt.Errorf("save state: %w", err)
	}

	return &RetryResult{
		TaskID:           newTaskID,
		Worker:           assignment.WorkerID,
		Model:            assignment.Model,
		Replaced:         opts.RetryOf,
		CascadeRecovered: cascadeRecovered,
	}, nil
}

func findPhaseForTask(state *model.CommandState, taskID string) (*model.Phase, int) {
	for i := range state.Phases {
		for _, tid := range state.Phases[i].TaskIDs {
			if tid == taskID {
				return &state.Phases[i], i
			}
		}
	}
	return nil, -1
}

func replaceInRequiredOrOptional(state *model.CommandState, oldID, newID string) {
	for i, id := range state.RequiredTaskIDs {
		if id == oldID {
			state.RequiredTaskIDs[i] = newID
			return
		}
	}
	for i, id := range state.OptionalTaskIDs {
		if id == oldID {
			state.OptionalTaskIDs[i] = newID
			return
		}
	}
}

func rewriteDependencies(state *model.CommandState, oldID, newID string) {
	for taskID, deps := range state.TaskDependencies {
		for i, dep := range deps {
			if dep == oldID {
				state.TaskDependencies[taskID][i] = newID
			}
		}
	}
}

func cascadeRecover(
	state *model.CommandState,
	failedTaskID, newRetryTaskID string,
	workerConfig model.WorkerConfig,
	limits model.LimitsConfig,
	workerStates []WorkerState,
	origTaskCache map[string]model.Task,
) ([]CascadeRecoveredTask, error) {
	var recovered []CascadeRecoveredTask
	return cascadeRecoverRecursive(state, failedTaskID, newRetryTaskID, workerConfig, limits, workerStates, recovered, origTaskCache)
}

func cascadeRecoverRecursive(
	state *model.CommandState,
	failedTaskID, newRetryTaskID string,
	workerConfig model.WorkerConfig,
	limits model.LimitsConfig,
	workerStates []WorkerState,
	recovered []CascadeRecoveredTask,
	origTaskCache map[string]model.Task,
) ([]CascadeRecoveredTask, error) {
	candidates := findCascadeCandidates(state, failedTaskID)

	for _, cancelledTaskID := range candidates {
		// Skip command-cancel-requested tasks
		reason := state.CancelledReasons[cancelledTaskID]
		if reason == "command_cancel_requested" {
			continue
		}

		// Generate new task ID
		newTaskID, err := model.GenerateID(model.IDTypeTask)
		if err != nil {
			return recovered, fmt.Errorf("generate cascade recovery ID: %w", err)
		}

		// Inherit original task's blocked_by, mapped through lineage
		origDeps := state.TaskDependencies[cancelledTaskID]
		newDeps := resolveBlockedByViaLineage(origDeps, state.RetryLineage)

		// Replace the failed dependency with the new retry task
		for i, dep := range newDeps {
			if dep == failedTaskID {
				newDeps[i] = newRetryTaskID
			}
		}

		// Worker assignment for recovered task — inherit bloom_level from original task
		bloomLevel := 3 // default fallback
		if origTask, ok := origTaskCache[cancelledTaskID]; ok {
			bloomLevel = origTask.BloomLevel
		}
		assignReqs := []TaskAssignmentRequest{{Name: "__cascade_recovery", BloomLevel: bloomLevel}}
		assignments, err := AssignWorkers(workerConfig, limits, workerStates, assignReqs)
		if err != nil {
			return recovered, fmt.Errorf("worker assignment for cascade %s: %w", cancelledTaskID, err)
		}
		assignment := assignments[0]

		// Update state
		replaceInRequiredOrOptional(state, cancelledTaskID, newTaskID)
		state.RetryLineage[newTaskID] = cancelledTaskID
		rewriteDependencies(state, cancelledTaskID, newTaskID)
		state.TaskStates[newTaskID] = model.StatusPending
		state.TaskDependencies[newTaskID] = newDeps

		// Add to phase
		if phase, phaseIdx := findPhaseForTask(state, cancelledTaskID); phase != nil {
			state.Phases[phaseIdx].TaskIDs = append(state.Phases[phaseIdx].TaskIDs, newTaskID)
			if phase.Status == model.PhaseStatusFailed {
				now := time.Now().UTC().Format(time.RFC3339)
				if err := reopenPhase(state, phaseIdx, now); err != nil {
					return recovered, fmt.Errorf("reopen phase for cascade: %w", err)
				}
			}
		}

		recovered = append(recovered, CascadeRecoveredTask{
			TaskID:   newTaskID,
			Worker:   assignment.WorkerID,
			Model:    assignment.Model,
			Replaced: cancelledTaskID,
		})

		// Update worker states for further assignments
		for i := range workerStates {
			if workerStates[i].WorkerID == assignment.WorkerID {
				workerStates[i].PendingCount++
				break
			}
		}

		// Recurse for downstream cancelled tasks
		var err2 error
		recovered, err2 = cascadeRecoverRecursive(
			state, cancelledTaskID, newTaskID,
			workerConfig, limits, workerStates, recovered, origTaskCache,
		)
		if err2 != nil {
			return recovered, err2
		}
	}

	return recovered, nil
}

func findCascadeCandidates(state *model.CommandState, failedTaskID string) []string {
	expectedReason := fmt.Sprintf("blocked_dependency_terminal:%s", failedTaskID)
	var candidates []string
	for taskID, reason := range state.CancelledReasons {
		if reason == expectedReason {
			candidates = append(candidates, taskID)
		}
	}
	return candidates
}

func resolveBlockedByViaLineage(blockedBy []string, lineage map[string]string) []string {
	resolved := make([]string, len(blockedBy))
	for i, dep := range blockedBy {
		resolved[i] = getLatestDescendant(dep, lineage)
	}
	return resolved
}

func getLatestDescendant(taskID string, lineage map[string]string) string {
	// lineage maps new -> old, so we need to build reverse map
	reverseLineage := make(map[string]string)
	for newID, oldID := range lineage {
		reverseLineage[oldID] = newID
	}

	current := taskID
	for {
		next, ok := reverseLineage[current]
		if !ok {
			return current
		}
		current = next
	}
}

func reopenPhase(state *model.CommandState, phaseIdx int, now string) error {
	phase := &state.Phases[phaseIdx]
	if phase.Status != model.PhaseStatusFailed {
		return fmt.Errorf("cannot reopen phase %q from status %s (only failed→active allowed)",
			phase.Name, phase.Status)
	}
	phase.Status = model.PhaseStatusActive
	phase.ReopenedAt = &now
	phase.CompletedAt = nil
	return nil
}

type retryQueueTask struct {
	taskID             string
	commandID          string
	purpose            string
	content            string
	acceptanceCriteria string
	constraints        []string
	blockedBy          []string
	bloomLevel         int
	toolsHint          []string
	workerID           string
}

func writeRetryQueueEntry(maestroDir string, task retryQueueTask, now string) error {
	queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(task.workerID))

	var tq model.TaskQueue
	data, err := os.ReadFile(queueFile)
	if err == nil {
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			return fmt.Errorf("parse existing queue %s: %w", task.workerID, err)
		}
	}
	if tq.SchemaVersion == 0 {
		tq.SchemaVersion = 1
		tq.FileType = "queue_task"
	}

	tq.Tasks = append(tq.Tasks, model.Task{
		ID:                 task.taskID,
		CommandID:          task.commandID,
		Purpose:            task.purpose,
		Content:            task.content,
		AcceptanceCriteria: task.acceptanceCriteria,
		Constraints:        task.constraints,
		BlockedBy:          task.blockedBy,
		BloomLevel:         task.bloomLevel,
		ToolsHint:          task.toolsHint,
		Priority:           100,
		Status:             model.StatusPending,
		CreatedAt:          now,
		UpdatedAt:          now,
	})

	return yamlutil.AtomicWrite(queueFile, tq)
}

func loadOriginalTasksFromQueue(maestroDir string, commandID string) map[string]model.Task {
	result := make(map[string]model.Task)
	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(queueDir, name))
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if yamlv3.Unmarshal(data, &tq) != nil {
			continue
		}
		for _, task := range tq.Tasks {
			if task.CommandID == commandID {
				result[task.ID] = task
			}
		}
	}
	return result
}

func rollbackRetryQueueEntries(maestroDir string, written []retryQueueTask) {
	// Group task IDs by worker queue file
	workerTaskIDs := make(map[string]map[string]bool) // queueFile → taskID set
	for _, t := range written {
		queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(t.workerID))
		if workerTaskIDs[queueFile] == nil {
			workerTaskIDs[queueFile] = make(map[string]bool)
		}
		workerTaskIDs[queueFile][t.taskID] = true
	}

	for queueFile, taskIDs := range workerTaskIDs {
		data, err := os.ReadFile(queueFile)
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			continue
		}
		var kept []model.Task
		for _, task := range tq.Tasks {
			if !taskIDs[task.ID] {
				kept = append(kept, task)
			}
		}
		tq.Tasks = kept
		_ = yamlutil.AtomicWrite(queueFile, tq)
	}
}

func copyState(state *model.CommandState) ([]byte, error) {
	return yamlv3.Marshal(state)
}

func restoreState(state *model.CommandState, data []byte) {
	var restored model.CommandState
	if yamlv3.Unmarshal(data, &restored) == nil {
		*state = restored
	}
}
