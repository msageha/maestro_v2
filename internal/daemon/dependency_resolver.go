package daemon

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// ErrStateNotFound is returned by StateReader methods when the state file does not exist
// (i.e., the command has not been submitted yet). Callers can use errors.Is to distinguish
// this from other read errors (e.g., parse failures on an existing file).
var ErrStateNotFound = errors.New("state not found")

// ErrPhaseNotFound is returned when a specific phase ID is not present in a command's
// phase list. Unlike ErrStateNotFound (state file missing), this means the state file
// exists but the requested phase does not.
var ErrPhaseNotFound = errors.New("phase not found")

// StateReader provides read access to command state (state/commands/{command_id}.yaml).
// Phase 6 implements the concrete version; Phase 5 uses this interface for decoupling.
type StateReader interface {
	// GetTaskState returns the status of a task from the command state.
	GetTaskState(commandID, taskID string) (model.Status, error)
	// GetCommandPhases returns phases for a command.
	GetCommandPhases(commandID string) ([]PhaseInfo, error)
	// GetTaskDependencies returns task IDs that the given task depends on.
	GetTaskDependencies(commandID, taskID string) ([]string, error)
	// IsSystemCommitReady checks if the given task is a system commit task and whether
	// all user phases (or user tasks for non-phased commands) are terminal.
	// Returns (isSystemCommit=false, ready=false, nil) for non-system-commit tasks.
	IsSystemCommitReady(commandID, taskID string) (isSystemCommit bool, ready bool, err error)
	// ApplyPhaseTransition persists a phase status change to state/commands/.
	ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error
	// UpdateTaskState updates a single task's status and optionally records a cancelled reason.
	UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error
	// IsCommandCancelRequested checks the state file for cancel.requested flag.
	IsCommandCancelRequested(commandID string) (bool, error)
	// GetCircuitBreakerState returns the circuit breaker state for a command.
	GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error)
	// TripCircuitBreaker sets the circuit breaker to tripped and issues a cancel request on the command.
	// progressTimeoutMinutes is re-validated under lock to prevent TOCTOU race; pass 0 to skip re-validation.
	TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error
}

// PhaseInfo represents phase metadata from command state.
type PhaseInfo struct {
	ID               string
	Name             string
	Status           model.PhaseStatus
	DependsOn        []string // phase IDs
	FillDeadlineAt   *string
	RequiredTaskIDs  []string
	SystemCommitTask bool
}

// DependencyResolver handles blocked_by dependency checking and phase transitions.
type DependencyResolver struct {
	stateReader StateReader
	dl          *DaemonLogger
	logger      *log.Logger
	logLevel    LogLevel
	clock       Clock
	eventBus    *events.Bus
}

// NewDependencyResolver creates a new DependencyResolver.
func NewDependencyResolver(reader StateReader, logger *log.Logger, logLevel LogLevel) *DependencyResolver {
	return &DependencyResolver{
		stateReader: reader,
		dl:          NewDaemonLoggerFromLegacy("dependency_resolver", logger, logLevel),
		logger:      logger,
		logLevel:    logLevel,
		clock:       RealClock{},
	}
}

// SetEventBus sets the event bus for publishing events.
func (dr *DependencyResolver) SetEventBus(bus *events.Bus) {
	dr.eventBus = bus
}

// IsTaskBlocked checks if a task's blocked_by dependencies are all resolved.
// Returns true if the task is still blocked.
func (dr *DependencyResolver) IsTaskBlocked(task *model.Task) (bool, error) {
	if len(task.BlockedBy) == 0 {
		return false, nil
	}

	if dr.stateReader == nil {
		// Without state reader, assume unblocked if blocked_by is empty
		return len(task.BlockedBy) > 0, nil
	}

	for _, depTaskID := range task.BlockedBy {
		status, err := dr.stateReader.GetTaskState(task.CommandID, depTaskID)
		if err != nil {
			dr.log(LogLevelWarn, "dependency_check task=%s dep=%s error=%v", task.ID, depTaskID, err)
			return true, err
		}
		if status != model.StatusCompleted {
			dr.log(LogLevelDebug, "task_blocked task=%s blocked_by=%s dep_status=%s",
				task.ID, depTaskID, status)
			return true, nil
		}
	}

	dr.log(LogLevelDebug, "task_unblocked task=%s", task.ID)
	return false, nil
}

// CheckDependencyFailure checks if any of a task's dependencies have failed.
// Returns the failed dependency ID and status, or empty string if none failed.
func (dr *DependencyResolver) CheckDependencyFailure(task *model.Task) (string, model.Status, error) {
	if len(task.BlockedBy) == 0 || dr.stateReader == nil {
		return "", "", nil
	}

	for _, depTaskID := range task.BlockedBy {
		status, err := dr.stateReader.GetTaskState(task.CommandID, depTaskID)
		if err != nil {
			return "", "", err
		}
		if status == model.StatusFailed || status == model.StatusCancelled {
			return depTaskID, status, nil
		}
	}
	return "", "", nil
}

// FindTransitiveDependents finds all tasks that transitively depend on the given task.
func (dr *DependencyResolver) FindTransitiveDependents(commandID string, failedTaskID string, allTasks []model.Task) []string {
	// Build reverse dependency graph: task → tasks that depend on it
	dependents := make(map[string][]string)
	for _, task := range allTasks {
		for _, dep := range task.BlockedBy {
			dependents[dep] = append(dependents[dep], task.ID)
		}
	}

	// BFS from failed task
	visited := make(map[string]bool)
	queue := []string{failedTaskID}
	var result []string

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, dependent := range dependents[current] {
			if visited[dependent] {
				continue
			}
			visited[dependent] = true
			result = append(result, dependent)
			queue = append(queue, dependent)
		}
	}

	return result
}

// PhaseTransitionResult describes the outcome of a phase transition check.
type PhaseTransitionResult struct {
	PhaseID   string
	PhaseName string
	OldStatus model.PhaseStatus
	NewStatus model.PhaseStatus
	Reason    string
}

// CheckPhaseTransitions performs phase transition checks (periodic scan step 0.7).
// Returns a list of phase transitions that should be applied.
func (dr *DependencyResolver) CheckPhaseTransitions(commandID string) ([]PhaseTransitionResult, error) {
	if dr.stateReader == nil {
		return nil, nil
	}

	phases, err := dr.stateReader.GetCommandPhases(commandID)
	if err != nil {
		if errors.Is(err, ErrStateNotFound) {
			// Command not yet submitted by planner - no phases to check
			return nil, nil
		}
		return nil, fmt.Errorf("get phases for %s: %w", commandID, err)
	}

	// Build phase lookup map once for all pending-phase checks (B-006 optimization).
	phaseMap := make(map[string]PhaseInfo, len(phases))
	for _, p := range phases {
		phaseMap[p.ID] = p
	}

	var transitions []PhaseTransitionResult

	for _, phase := range phases {
		switch phase.Status {
		case model.PhaseStatusActive:
			tr := dr.checkActivePhaseCompletion(commandID, phase)
			if tr != nil {
				transitions = append(transitions, *tr)
			}

		case model.PhaseStatusPending:
			// Check for cascade cancel
			tr := dr.checkPendingPhaseCascade(phaseMap, phase)
			if tr != nil {
				transitions = append(transitions, *tr)
				continue
			}
			// Check for activation (all dependency phases completed)
			tr = dr.checkPendingPhaseActivation(phaseMap, phase)
			if tr != nil {
				transitions = append(transitions, *tr)
			}

		case model.PhaseStatusAwaitingFill:
			tr := dr.checkAwaitingFillTimeout(phase)
			if tr != nil {
				transitions = append(transitions, *tr)
			}
		}
	}

	return transitions, nil
}

// checkActivePhaseCompletion checks if an active phase is complete, failed, or cancelled.
func (dr *DependencyResolver) checkActivePhaseCompletion(commandID string, phase PhaseInfo) *PhaseTransitionResult {
	if len(phase.RequiredTaskIDs) == 0 {
		return nil
	}

	allCompleted := true
	hasFailed := false
	hasCancelled := false

	for _, taskID := range phase.RequiredTaskIDs {
		status, err := dr.stateReader.GetTaskState(commandID, taskID)
		if err != nil {
			dr.log(LogLevelWarn, "phase_check task_state error phase=%s task=%s error=%v",
				phase.ID, taskID, err)
			allCompleted = false
			continue
		}

		switch status {
		case model.StatusCompleted:
			// ok
		case model.StatusFailed:
			hasFailed = true
		case model.StatusCancelled:
			hasCancelled = true
		default:
			allCompleted = false
		}
	}

	var result *PhaseTransitionResult
	if hasFailed {
		result = &PhaseTransitionResult{
			PhaseID:   phase.ID,
			PhaseName: phase.Name,
			OldStatus: phase.Status,
			NewStatus: model.PhaseStatusFailed,
			Reason:    "required task failed",
		}
	} else if hasCancelled {
		result = &PhaseTransitionResult{
			PhaseID:   phase.ID,
			PhaseName: phase.Name,
			OldStatus: phase.Status,
			NewStatus: model.PhaseStatusCancelled,
			Reason:    "required task cancelled",
		}
	} else if allCompleted {
		result = &PhaseTransitionResult{
			PhaseID:   phase.ID,
			PhaseName: phase.Name,
			OldStatus: phase.Status,
			NewStatus: model.PhaseStatusCompleted,
			Reason:    "all required tasks completed",
		}
	}

	if result != nil {
		dr.publishPhaseTransitionEvent(commandID, *result)
	}

	return result
}

// checkPendingPhaseActivation checks if a pending phase should be activated.
// phaseMap is pre-built by CheckPhaseTransitions for O(1) lookups (B-006).
func (dr *DependencyResolver) checkPendingPhaseActivation(phaseMap map[string]PhaseInfo, phase PhaseInfo) *PhaseTransitionResult {
	// DependsOn empty means no dependencies — activate immediately
	if len(phase.DependsOn) == 0 {
		dr.log(LogLevelInfo, "phase_activation phase=%s no_dependencies", phase.ID)
		return &PhaseTransitionResult{
			PhaseID:   phase.ID,
			PhaseName: phase.Name,
			OldStatus: phase.Status,
			NewStatus: model.PhaseStatusAwaitingFill,
			Reason:    "no dependency phases (immediate activation)",
		}
	}

	for _, depID := range phase.DependsOn {
		dep, ok := phaseMap[depID]
		if !ok {
			dr.log(LogLevelWarn, "phase_activation missing dep phase=%s dep=%s", phase.ID, depID)
			return nil
		}
		if dep.Status != model.PhaseStatusCompleted {
			return nil
		}
	}

	dr.log(LogLevelInfo, "phase_activation phase=%s all_deps_completed", phase.ID)
	result := &PhaseTransitionResult{
		PhaseID:   phase.ID,
		PhaseName: phase.Name,
		OldStatus: phase.Status,
		NewStatus: model.PhaseStatusAwaitingFill,
		Reason:    "all dependency phases completed",
	}
	// commandID is not available in this method, caller will publish event
	return result
}

// checkPendingPhaseCascade checks if a pending phase should be cascade-cancelled.
// phaseMap is pre-built by CheckPhaseTransitions for O(1) lookups (B-006).
func (dr *DependencyResolver) checkPendingPhaseCascade(phaseMap map[string]PhaseInfo, phase PhaseInfo) *PhaseTransitionResult {
	if len(phase.DependsOn) == 0 {
		return nil
	}

	for _, depID := range phase.DependsOn {
		dep, ok := phaseMap[depID]
		if !ok {
			continue
		}
		if dep.Status == model.PhaseStatusFailed ||
			dep.Status == model.PhaseStatusCancelled ||
			dep.Status == model.PhaseStatusTimedOut {
			result := &PhaseTransitionResult{
				PhaseID:   phase.ID,
				PhaseName: phase.Name,
				OldStatus: phase.Status,
				NewStatus: model.PhaseStatusCancelled,
				Reason:    fmt.Sprintf("dependency phase %s is %s", depID, dep.Status),
			}
			// commandID is not available in this method, caller will publish event
			return result
		}
	}

	return nil
}

// checkAwaitingFillTimeout checks if an awaiting_fill phase has timed out.
func (dr *DependencyResolver) checkAwaitingFillTimeout(phase PhaseInfo) *PhaseTransitionResult {
	if phase.FillDeadlineAt == nil {
		return nil
	}

	deadline, err := time.Parse(time.RFC3339, *phase.FillDeadlineAt)
	if err != nil {
		dr.log(LogLevelWarn, "phase_timeout invalid deadline phase=%s deadline=%s",
			phase.ID, *phase.FillDeadlineAt)
		return nil
	}

	if dr.clock.Now().UTC().After(deadline) {
		result := &PhaseTransitionResult{
			PhaseID:   phase.ID,
			PhaseName: phase.Name,
			OldStatus: phase.Status,
			NewStatus: model.PhaseStatusTimedOut,
			Reason:    "fill deadline exceeded",
		}
		// commandID is not available in this method, caller will publish event
		return result
	}

	return nil
}

// IsSystemCommitReady checks if the given task is a system commit task and whether
// all user phases/tasks are terminal (dispatch precondition for system commit tasks).
func (dr *DependencyResolver) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	if dr.stateReader == nil {
		return false, false, nil
	}
	return dr.stateReader.IsSystemCommitReady(commandID, taskID)
}

// GetPhaseStatus returns the current status of a specific phase from state.
func (dr *DependencyResolver) GetPhaseStatus(commandID, phaseID string) (model.PhaseStatus, error) {
	if dr.stateReader == nil {
		return "", fmt.Errorf("no state reader")
	}
	phases, err := dr.stateReader.GetCommandPhases(commandID)
	if err != nil {
		return "", err
	}
	for _, p := range phases {
		if p.ID == phaseID {
			return p.Status, nil
		}
	}
	return "", fmt.Errorf("phase %s in command %s: %w", phaseID, commandID, ErrPhaseNotFound)
}

// BuildAwaitingFillNotification creates the notification message for a phase entering awaiting_fill.
func (dr *DependencyResolver) BuildAwaitingFillNotification(commandID string, phase PhaseInfo) string {
	return fmt.Sprintf("phase:%s phase_id:%s status:awaiting_fill command_id:%s — plan submit --phase %s で次フェーズのタスクを投入してください",
		phase.Name, phase.ID, commandID, phase.Name)
}

func (dr *DependencyResolver) publishPhaseTransitionEvent(commandID string, tr PhaseTransitionResult) {
	if dr.eventBus != nil {
		dr.eventBus.Publish(events.EventPhaseTransition, map[string]interface{}{
			"command_id": commandID,
			"phase_id":   tr.PhaseID,
			"phase_name": tr.PhaseName,
			"old_status": string(tr.OldStatus),
			"new_status": string(tr.NewStatus),
			"reason":     tr.Reason,
		})
	}
}

func (dr *DependencyResolver) log(level LogLevel, format string, args ...any) {
	dr.dl.Logf(level, format, args...)
}
