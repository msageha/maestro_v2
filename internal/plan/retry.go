package plan

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// RetryOptions holds the configuration for retrying a failed task.
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
	PersonaHint        string
	SkillRefs          []string
	MaestroDir         string
	Config             model.Config
	LockMap            *lock.MutexMap
}

// RetryResult contains the outcome of a task retry including any cascade-recovered tasks.
type RetryResult struct {
	TaskID           string                 `json:"task_id"`
	Worker           string                 `json:"worker"`
	Model            string                 `json:"model"`
	Replaced         string                 `json:"replaced"`
	CascadeRecovered []CascadeRecoveredTask `json:"cascade_recovered,omitempty"`
}

// CascadeRecoveredTask describes a downstream task that was automatically recovered during a retry.
type CascadeRecoveredTask struct {
	TaskID   string `json:"task_id"`
	Worker   string `json:"worker"`
	Model    string `json:"model"`
	Replaced string `json:"replaced"`
}

// AddRetryTask creates a replacement task for a failed task, rewires dependencies, and performs cascade recovery.
func AddRetryTask(opts RetryOptions) (*RetryResult, error) {
	if opts.LockMap == nil {
		return nil, fmt.Errorf("LockMap is required")
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	// Validate
	rc, err := validateRetryRequest(sm, opts)
	if err != nil {
		return nil, fmt.Errorf("validate retry: %w", err)
	}
	state := rc.state

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

	// Generate new task ID
	newTaskID, err := model.NewTaskID(model.TaskIDCallerPlannerRetry)
	if err != nil {
		return nil, fmt.Errorf("generate task ID: %w", err)
	}

	now := nowUTC()

	// Load original task cache for cascade recovery and queue building
	origTaskCache, err := loadOriginalTasksFromQueue(opts.MaestroDir, opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("load original tasks from queue: %w", err)
	}

	// Apply state changes and cascade recovery
	cascadeRecovered, origStateBytes, err := applyRetryStateChanges(
		state, opts, newTaskID, rc, now, workerStates, origTaskCache,
	)
	if err != nil {
		return nil, fmt.Errorf("apply retry state changes: %w", err)
	}

	// Write queue entry for the primary retry task
	var writtenQueueTaskIDs []retryQueueTask // track for rollback
	primaryTask := retryQueueTask{
		taskID:             newTaskID,
		commandID:          opts.CommandID,
		purpose:            opts.Purpose,
		content:            opts.Content,
		acceptanceCriteria: opts.AcceptanceCriteria,
		constraints:        opts.Constraints,
		blockedBy:          rc.blockedBy,
		bloomLevel:         opts.BloomLevel,
		toolsHint:          opts.ToolsHint,
		personaHint:        opts.PersonaHint,
		skillRefs:          opts.SkillRefs,
		workerID:           assignment.WorkerID,
	}

	if err := writeRetryQueueEntry(opts.MaestroDir, primaryTask, now, opts.LockMap); err != nil {
		restoreState(state, origStateBytes)
		return nil, fmt.Errorf("write queue entry for %s: %w", newTaskID, err)
	}
	writtenQueueTaskIDs = append(writtenQueueTaskIDs, primaryTask)

	// Write queue entries for cascade recovered tasks
	for _, cr := range cascadeRecovered {
		crTask := buildCascadeQueueTask(cr, opts, state, origTaskCache)
		if err := writeRetryQueueEntry(opts.MaestroDir, crTask, now, opts.LockMap); err != nil {
			rollbackRetryQueueEntries(opts.MaestroDir, writtenQueueTaskIDs, opts.LockMap)
			restoreState(state, origStateBytes)
			return nil, fmt.Errorf("write queue entry for cascade %s: %w", cr.TaskID, err)
		}
		writtenQueueTaskIDs = append(writtenQueueTaskIDs, crTask)
	}

	// Cancel the original task's queue entry so that checkCommandTasksTerminal
	// does not count it as failed after the retry task supersedes it.
	if err := cancelOriginalTaskInQueue(opts.MaestroDir, opts.RetryOf, opts.CommandID, now, opts.LockMap); err != nil {
		rollbackRetryQueueEntries(opts.MaestroDir, writtenQueueTaskIDs, opts.LockMap)
		restoreState(state, origStateBytes)
		return nil, fmt.Errorf("cancel original task in queue: %w", err)
	}

	// Save state
	if err := sm.SaveState(state); err != nil {
		// Best-effort restore of original task status in queue.
		if restoreErr := restoreOriginalTaskInQueue(opts.MaestroDir, opts.RetryOf, opts.CommandID, model.StatusFailed, now, opts.LockMap); restoreErr != nil {
			log.Printf("[WARN] failed to restore original task %s queue status: %v", opts.RetryOf, restoreErr)
		}
		rollbackRetryQueueEntries(opts.MaestroDir, writtenQueueTaskIDs, opts.LockMap)
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

// retryContext holds validated state and metadata for a retry operation.
type retryContext struct {
	state     *model.CommandState
	phase     *model.Phase
	phaseIdx  int
	blockedBy []string
}

// validateRetryRequest loads and validates state, task status, phase membership, and blocked_by references.
func validateRetryRequest(sm *StateManager, opts RetryOptions) (*retryContext, error) {
	if opts.BloomLevel < BloomLevelMin || opts.BloomLevel > BloomLevelMax {
		return nil, &PlanValidationError{Msg: fmt.Sprintf("bloom_level must be between %d and %d, got %d", BloomLevelMin, BloomLevelMax, opts.BloomLevel)}
	}

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	if state.PlanStatus != model.PlanStatusSealed {
		return nil, &PlanValidationError{Msg: fmt.Sprintf("plan_status must be sealed, got %s", state.PlanStatus)}
	}

	if err := ValidateNotCancelled(state); err != nil {
		return nil, fmt.Errorf("validate not cancelled: %w", err)
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
			var err error
			blockedBy, err = resolveBlockedByViaLineage(deps, state.RetryLineage)
			if err != nil {
				return nil, fmt.Errorf("resolve blocked_by via lineage: %w", err)
			}
		}
	}

	// Validate blocked_by references
	if len(blockedBy) > 0 {
		if err := validateRetryBlockedBy(state, blockedBy, phase); err != nil {
			return nil, fmt.Errorf("validate blocked_by: %w", err)
		}
	}

	return &retryContext{
		state:     state,
		phase:     phase,
		phaseIdx:  phaseIdx,
		blockedBy: blockedBy,
	}, nil
}

// validateRetryBlockedBy checks that blocked_by references are valid within the phase or command scope.
func validateRetryBlockedBy(state *model.CommandState, blockedBy []string, phase *model.Phase) error {
	if phase != nil {
		// Phase-scoped: blocked_by must be within same phase (or system commit)
		phaseTaskSet := make(map[string]bool)
		for _, tid := range phase.TaskIDs {
			phaseTaskSet[tid] = true
		}
		for _, dep := range blockedBy {
			if !phaseTaskSet[dep] {
				if state.SystemCommitTaskID == nil || dep != *state.SystemCommitTaskID {
					return &PlanValidationError{Msg: fmt.Sprintf("blocked_by task %s is not in phase %q", dep, phase.Name)}
				}
			}
		}
	} else {
		// No phase: blocked_by must exist in command's task states
		for _, dep := range blockedBy {
			if _, ok := state.TaskStates[dep]; !ok {
				return &PlanValidationError{Msg: fmt.Sprintf("blocked_by task %s not found in command state", dep)}
			}
		}
	}
	return nil
}

// applyRetryStateChanges modifies state for the retry task and performs cascade recovery.
// Returns the cascade recovered tasks and the original state bytes for rollback.
func applyRetryStateChanges(
	state *model.CommandState, opts RetryOptions, newTaskID string,
	rc *retryContext, now string,
	workerStates []WorkerState, origTaskCache map[string]model.Task,
) ([]CascadeRecoveredTask, []byte, error) {

	origStateBytes, err := copyState(state)
	if err != nil {
		return nil, nil, fmt.Errorf("copy state for rollback: %w", err)
	}

	// Replace in required/optional task IDs
	if err := replaceInRequiredOrOptional(state, opts.RetryOf, newTaskID); err != nil {
		restoreState(state, origStateBytes)
		return nil, nil, fmt.Errorf("replace in required/optional: %w", err)
	}

	// Record retry lineage
	state.RetryLineage[newTaskID] = opts.RetryOf

	// Rewrite dependencies
	rewriteDependencies(state, opts.RetryOf, newTaskID)

	// Set new task state
	state.TaskStates[newTaskID] = model.StatusPending
	state.TaskDependencies[newTaskID] = rc.blockedBy

	// Add to phase
	if rc.phase != nil {
		state.Phases[rc.phaseIdx].TaskIDs = append(state.Phases[rc.phaseIdx].TaskIDs, newTaskID)

		// Reopen phase if failed
		if rc.phase.Status == model.PhaseStatusFailed {
			if err := reopenPhase(state, rc.phaseIdx, now); err != nil {
				restoreState(state, origStateBytes)
				return nil, nil, fmt.Errorf("reopen phase: %w", err)
			}
		}
	}

	// Cascade recovery
	cascadeRecovered, err := cascadeRecover(
		state, opts.RetryOf, newTaskID,
		opts.Config.Agents.Workers, opts.Config.Limits, workerStates, origTaskCache,
	)
	if err != nil {
		restoreState(state, origStateBytes)
		return nil, nil, fmt.Errorf("cascade recovery: %w", err)
	}

	// Post-recovery DAG validation
	allNames := make([]string, 0, len(state.TaskStates))
	for k := range state.TaskStates {
		allNames = append(allNames, k)
	}
	if _, err := ValidateTaskDAG(allNames, state.TaskDependencies); err != nil {
		restoreState(state, origStateBytes)
		return nil, nil, fmt.Errorf("post-recovery DAG validation: %w", err)
	}

	state.UpdatedAt = now
	return cascadeRecovered, origStateBytes, nil
}

// buildCascadeQueueTask constructs a retryQueueTask for a cascade-recovered task,
// inheriting content from the original task when available.
func buildCascadeQueueTask(cr CascadeRecoveredTask, opts RetryOptions, state *model.CommandState, origTaskCache map[string]model.Task) retryQueueTask {
	purpose := "cascade recovery of " + cr.Replaced
	content := purpose
	acceptanceCriteria := purpose
	bloomLevel := opts.BloomLevel
	var constraints []string
	var toolsHint []string
	var personaHint string
	var skillRefs []string

	if orig, ok := origTaskCache[cr.Replaced]; ok {
		purpose = orig.Purpose
		content = orig.Content
		acceptanceCriteria = orig.AcceptanceCriteria
		bloomLevel = orig.BloomLevel
		constraints = orig.Constraints
		toolsHint = orig.ToolsHint
		personaHint = orig.PersonaHint
		skillRefs = orig.SkillRefs
	}

	return retryQueueTask{
		taskID:             cr.TaskID,
		commandID:          opts.CommandID,
		purpose:            purpose,
		content:            content,
		acceptanceCriteria: acceptanceCriteria,
		constraints:        constraints,
		blockedBy:          state.TaskDependencies[cr.TaskID],
		bloomLevel:         bloomLevel,
		toolsHint:          toolsHint,
		personaHint:        personaHint,
		skillRefs:          skillRefs,
		workerID:           cr.Worker,
	}
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

func replaceInRequiredOrOptional(state *model.CommandState, oldID, newID string) error {
	found := false
	for i, id := range state.RequiredTaskIDs {
		if id == oldID {
			state.RequiredTaskIDs[i] = newID
			found = true
			break
		}
	}
	if !found {
		for i, id := range state.OptionalTaskIDs {
			if id == oldID {
				state.OptionalTaskIDs[i] = newID
				found = true
				break
			}
		}
	}
	// Update SystemCommitTaskID if it matches the old ID.
	if state.SystemCommitTaskID != nil && *state.SystemCommitTaskID == oldID {
		state.SystemCommitTaskID = &newID
		found = true
	}
	if !found {
		return fmt.Errorf("task %s not found in required, optional, or system_commit_task_id", oldID)
	}
	return nil
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
	// CR-020: Work on a copy of workerStates so that on failure the caller's
	// slice is not left in a partially-mutated state.
	ws := make([]WorkerState, len(workerStates))
	copy(ws, workerStates)
	var recovered []CascadeRecoveredTask
	return cascadeRecoverRecursive(state, failedTaskID, newRetryTaskID, workerConfig, limits, ws, recovered, origTaskCache)
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
		newTaskID, err := model.NewTaskID(model.TaskIDCallerPlannerRetry)
		if err != nil {
			return recovered, fmt.Errorf("generate cascade recovery ID: %w", err)
		}

		// Inherit original task's blocked_by, mapped through lineage
		origDeps := state.TaskDependencies[cancelledTaskID]
		newDeps, err := resolveBlockedByViaLineage(origDeps, state.RetryLineage)
		if err != nil {
			return recovered, fmt.Errorf("resolve blocked_by via lineage for cascade %s: %w", cancelledTaskID, err)
		}

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
		if err := replaceInRequiredOrOptional(state, cancelledTaskID, newTaskID); err != nil {
			return recovered, fmt.Errorf("cascade replace in required/optional: %w", err)
		}
		state.RetryLineage[newTaskID] = cancelledTaskID
		rewriteDependencies(state, cancelledTaskID, newTaskID)
		state.TaskStates[newTaskID] = model.StatusPending
		state.TaskDependencies[newTaskID] = newDeps

		// Add to phase
		if phase, phaseIdx := findPhaseForTask(state, cancelledTaskID); phase != nil {
			state.Phases[phaseIdx].TaskIDs = append(state.Phases[phaseIdx].TaskIDs, newTaskID)
			if phase.Status == model.PhaseStatusFailed {
				now := nowUTC()
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

func resolveBlockedByViaLineage(blockedBy []string, lineage map[string]string) ([]string, error) {
	// Build reverse lineage map once (O(m)) instead of per-element (was O(n*m)).
	// lineage maps new -> old, reverse maps old -> new.
	reverseLineage := make(map[string]string, len(lineage))
	for newID, oldID := range lineage {
		reverseLineage[oldID] = newID
	}

	resolved := make([]string, len(blockedBy))
	for i, dep := range blockedBy {
		desc, err := getLatestDescendant(dep, reverseLineage)
		if err != nil {
			return nil, err
		}
		resolved[i] = desc
	}
	return resolved, nil
}

func getLatestDescendant(taskID string, reverseLineage map[string]string) (string, error) {
	visited := make(map[string]bool)
	current := taskID
	for {
		if visited[current] {
			log.Printf("[WARN] lineage cycle detected starting from task %s, revisited %s", taskID, current)
			return "", fmt.Errorf("lineage cycle detected: task %s revisited while resolving lineage from %s", current, taskID)
		}
		visited[current] = true
		next, ok := reverseLineage[current]
		if !ok {
			return current, nil
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
	personaHint        string
	skillRefs          []string
	workerID           string
}

func writeRetryQueueEntry(maestroDir string, task retryQueueTask, now string, lockMap *lock.MutexMap) error {
	// CRIT-02: Lock per-queue to prevent concurrent read-modify-write data loss.
	if lockMap != nil {
		lockMap.Lock("queue:" + task.workerID)
		defer lockMap.Unlock("queue:" + task.workerID)
	}

	return readModifyWriteQueue(maestroDir, task.workerID, func(tq *model.TaskQueue) {
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
			PersonaHint:        task.personaHint,
			SkillRefs:          task.skillRefs,
			Priority:           100,
			Status:             model.StatusPending,
			CreatedAt:          now,
			UpdatedAt:          now,
		})
	})
}

func loadOriginalTasksFromQueue(maestroDir string, commandID string) (map[string]model.Task, error) {
	result := make(map[string]model.Task)
	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return nil, fmt.Errorf("read queue directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		filePath := filepath.Join(queueDir, name)
		data, err := os.ReadFile(filePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // file removed between ReadDir and ReadFile; race-safe
			}
			return nil, fmt.Errorf("read queue file %s: %w", name, err)
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			log.Printf("[WARN] loadOriginalTasksFromQueue: skipping corrupt queue file %s: %v", name, err)
			continue
		}
		for _, task := range tq.Tasks {
			if task.CommandID == commandID {
				result[task.ID] = task
			}
		}
	}
	return result, nil
}

func rollbackRetryQueueEntries(maestroDir string, written []retryQueueTask, lockMap *lock.MutexMap) {
	// Group task IDs by worker
	type workerRollback struct {
		workerID string
		taskIDs  map[string]bool
	}
	workerMap := make(map[string]*workerRollback) // workerID → rollback data
	for _, t := range written {
		if workerMap[t.workerID] == nil {
			workerMap[t.workerID] = &workerRollback{
				workerID: t.workerID,
				taskIDs:  make(map[string]bool),
			}
		}
		workerMap[t.workerID].taskIDs[t.taskID] = true
	}

	// CRIT-02: Lock per-queue to prevent concurrent read-modify-write data loss.
	for _, rb := range workerMap {
		func() {
			if lockMap != nil {
				lockMap.Lock("queue:" + rb.workerID)
				defer lockMap.Unlock("queue:" + rb.workerID)
			}

			queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(rb.workerID))
			data, err := os.ReadFile(queueFile)
			if err != nil {
				return
			}
			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(data, &tq); err != nil {
				return
			}
			var kept []model.Task
			for _, task := range tq.Tasks {
				if !rb.taskIDs[task.ID] {
					kept = append(kept, task)
				}
			}
			tq.Tasks = kept
			if writeErr := yamlutil.AtomicWrite(queueFile, tq); writeErr != nil {
				log.Printf("[WARN] rollback: write queue %s: %v", queueFile, writeErr)
			}
		}()
	}
}

func copyState(state *model.CommandState) ([]byte, error) {
	return yamlv3.Marshal(state)
}

func restoreState(state *model.CommandState, data []byte) {
	var restored model.CommandState
	if err := yamlv3.Unmarshal(data, &restored); err != nil {
		log.Printf("[ERROR] restoreState: failed to unmarshal state snapshot: %v", err)
		return
	}
	*state = restored
}

// cancelOriginalTaskInQueue scans all worker queues and sets the original
// (retried) task's status to cancelled, preventing checkCommandTasksTerminal
// from treating it as a failure after the retry supersedes it.
func cancelOriginalTaskInQueue(maestroDir string, taskID string, commandID string, now string, lockMap *lock.MutexMap) error {
	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read queue directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		workerID := strings.TrimSuffix(name, ".yaml")

		if lockMap != nil {
			lockMap.Lock("queue:" + workerID)
		}
		found := false
		err := readModifyWriteQueue(maestroDir, workerID, func(tq *model.TaskQueue) {
			for i := range tq.Tasks {
				if tq.Tasks[i].ID == taskID && tq.Tasks[i].CommandID == commandID {
					tq.Tasks[i].Status = model.StatusCancelled
					tq.Tasks[i].UpdatedAt = now
					found = true
					return
				}
			}
		})
		if lockMap != nil {
			lockMap.Unlock("queue:" + workerID)
		}
		if err != nil {
			return fmt.Errorf("cancel original task %s in queue %s: %w", taskID, workerID, err)
		}
		if found {
			return nil
		}
	}
	return nil // task not found — may have been archived or cleaned up
}

// restoreOriginalTaskInQueue is the rollback counterpart of cancelOriginalTaskInQueue.
// It restores the original task's queue status (best-effort, used only on SaveState failure).
func restoreOriginalTaskInQueue(maestroDir string, taskID string, commandID string, status model.Status, now string, lockMap *lock.MutexMap) error {
	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return fmt.Errorf("read queue directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		workerID := strings.TrimSuffix(name, ".yaml")

		if lockMap != nil {
			lockMap.Lock("queue:" + workerID)
		}
		found := false
		err := readModifyWriteQueue(maestroDir, workerID, func(tq *model.TaskQueue) {
			for i := range tq.Tasks {
				if tq.Tasks[i].ID == taskID && tq.Tasks[i].CommandID == commandID {
					tq.Tasks[i].Status = status
					tq.Tasks[i].UpdatedAt = now
					found = true
					return
				}
			}
		})
		if lockMap != nil {
			lockMap.Unlock("queue:" + workerID)
		}
		if err != nil {
			return fmt.Errorf("restore task %s in queue %s: %w", taskID, workerID, err)
		}
		if found {
			return nil
		}
	}
	return nil
}
