package daemon

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// StateReader, PhaseInfo, ErrStateNotFound, ErrPhaseNotFound are defined in
// internal/daemon/core and re-exported via core_aliases.go.

// MergeGateFunc reports whether a phase's pending worktree merge has been
// recorded. Used to gate PhaseStatusCompleted application and event publication
// so that downstream effects (phase_diagnosis emission, pending-phase
// activation cascades) wait for the merge to record a potential merge_conflict
// before treating the phase as fully complete. Returning true means the gate
// is satisfied (no worktree merge pending, or merge already recorded); false
// means the transition must be deferred.
type MergeGateFunc func(commandID, phaseID string) bool

// DependencyResolver handles blocked_by dependency checking and phase transitions.
type DependencyResolver struct {
	stateManager StateManager
	dl           *DaemonLogger
	clock        Clock
	mu           sync.RWMutex // protects eventBus and mergeGate
	eventBus     *events.Bus
	mergeGate    MergeGateFunc
}

// NewDependencyResolver creates a new DependencyResolver.
func NewDependencyResolver(reader StateManager, logger *log.Logger, logLevel LogLevel) *DependencyResolver {
	return &DependencyResolver{
		stateManager: reader,
		dl:           NewDaemonLoggerFromLegacy("dependency_resolver", logger, logLevel),
		clock:        RealClock{},
	}
}

// SetEventBus sets the event bus for publishing events.
func (dr *DependencyResolver) SetEventBus(bus *events.Bus) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	dr.eventBus = bus
}

// SetMergeGate wires the worktree merge gate. When set, PhaseStatusCompleted
// transitions for phases whose worktree merge has not yet been recorded are
// still surfaced to the caller (so it can log/defer) but are not reflected
// into the in-scan phase map nor published as phase_transition events. This
// prevents premature pending-phase activation and duplicate phase_diagnosis
// emission while the merge is in flight.
func (dr *DependencyResolver) SetMergeGate(fn MergeGateFunc) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	dr.mergeGate = fn
}

// mergeGateAllows returns true when the configured merge gate permits the
// Completed transition to apply (no gate set → always allowed).
func (dr *DependencyResolver) mergeGateAllows(commandID, phaseID string) bool {
	dr.mu.RLock()
	gate := dr.mergeGate
	dr.mu.RUnlock()
	if gate == nil {
		return true
	}
	return gate(commandID, phaseID)
}

// IsTaskBlocked checks if a task's blocked_by dependencies are all resolved.
// Returns true if the task is still blocked.
func (dr *DependencyResolver) IsTaskBlocked(task *model.Task) (bool, error) {
	if len(task.BlockedBy) == 0 {
		return false, nil
	}

	if dr.stateManager == nil {
		return len(task.BlockedBy) > 0, fmt.Errorf("state manager not configured; cannot resolve dependencies")
	}

	for _, depTaskID := range task.BlockedBy {
		status, err := dr.stateManager.GetTaskState(task.CommandID, depTaskID)
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
	if len(task.BlockedBy) == 0 || dr.stateManager == nil {
		return "", "", nil
	}

	for _, depTaskID := range task.BlockedBy {
		status, err := dr.stateManager.GetTaskState(task.CommandID, depTaskID)
		if err != nil {
			return "", "", err
		}
		if status == model.StatusFailed || status == model.StatusCancelled {
			return depTaskID, status, nil
		}
	}
	return "", "", nil
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
	if dr.stateManager == nil {
		return nil, nil
	}

	phases, err := dr.stateManager.GetCommandPhases(commandID)
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

	// Pass 1: process active and awaiting_fill phases.
	// Active-phase transitions are reflected back into phaseMap so that pass 2
	// can detect newly-eligible pending phases within the same scan cycle —
	// preventing stepWorktreeFastTrackCleanup from killing a pending phase whose
	// dependency phase completed in this very cycle before it could activate.
	//
	// Exception: if the merge gate is set and refuses a Completed transition
	// (worktree merge not yet recorded), we must NOT reflect Completed into
	// phaseMap nor publish a phase_transition event for it. Otherwise pass 2
	// would activate downstream pending phases — and the event bus would leak
	// a "completed" signal to listeners — before MergeToIntegration has had a
	// chance to record a merge_conflict. The transition is still appended to
	// the returned slice so that the caller can log the deferral; the caller
	// (stepPhaseTransitions) enforces the same gate before ApplyPhaseTransition.
	// Failed/Cancelled/TimedOut transitions are never gated: they must surface
	// immediately regardless of merge state.
	for _, phase := range phases {
		switch phase.Status {
		case model.PhaseStatusActive:
			tr := dr.checkActivePhaseCompletion(commandID, phase)
			if tr == nil {
				continue
			}
			transitions = append(transitions, *tr)
			if tr.NewStatus == model.PhaseStatusCompleted && !dr.mergeGateAllows(commandID, tr.PhaseID) {
				dr.log(LogLevelDebug,
					"phase_transition_gate_reflect_skipped command=%s phase=%s reason=awaiting_worktree_merge",
					commandID, tr.PhaseName)
				continue
			}
			// Reflect the new status in phaseMap so pending dependents
			// can detect the completion in pass 2.
			if entry, ok := phaseMap[tr.PhaseID]; ok {
				entry.Status = tr.NewStatus
				phaseMap[tr.PhaseID] = entry
			}
		case model.PhaseStatusAwaitingFill:
			tr := dr.checkAwaitingFillTimeout(phase)
			if tr != nil {
				transitions = append(transitions, *tr)
			}
		}
	}

	// Pass 2: process pending phases with the now-updated phaseMap.
	for _, phase := range phases {
		if phase.Status != model.PhaseStatusPending {
			continue
		}
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
		status, err := dr.stateManager.GetTaskState(commandID, taskID)
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
		// Suppress event publication for merge-gated Completed transitions so
		// that downstream event consumers don't see the phase as complete
		// before the worktree merge has had a chance to surface a
		// merge_conflict. Failed/Cancelled transitions are always published.
		if result.NewStatus != model.PhaseStatusCompleted || dr.mergeGateAllows(commandID, result.PhaseID) {
			dr.publishPhaseTransitionEvent(commandID, *result)
		}
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
	if dr.stateManager == nil {
		return false, false, nil
	}
	return dr.stateManager.IsSystemCommitReady(commandID, taskID)
}

// GetPhaseStatus returns the current status of a specific phase from state.
func (dr *DependencyResolver) GetPhaseStatus(commandID, phaseID string) (model.PhaseStatus, error) {
	if dr.stateManager == nil {
		return "", fmt.Errorf("no state reader")
	}
	phases, err := dr.stateManager.GetCommandPhases(commandID)
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
	dr.mu.RLock()
	bus := dr.eventBus
	dr.mu.RUnlock()
	if bus != nil {
		bus.Publish(events.EventPhaseTransition, map[string]interface{}{
			"command_id": commandID,
			"phase_id":   tr.PhaseID,
			"phase_name": tr.PhaseName,
			"old_status": string(tr.OldStatus),
			"new_status": string(tr.NewStatus),
			"reason":     tr.Reason,
		})
	}
}

// HasStateReader returns true if a StateReader has been wired.
func (dr *DependencyResolver) HasStateReader() bool {
	return dr.stateManager != nil
}

// GetStateReader returns the wired StateReader for read-only access (may be nil).
func (dr *DependencyResolver) GetStateReader() StateReader {
	return dr.stateManager
}

// GetStateManager returns the wired StateManager for read/write access (may be nil).
func (dr *DependencyResolver) GetStateManager() StateManager {
	return dr.stateManager
}

func (dr *DependencyResolver) log(level LogLevel, format string, args ...any) {
	dr.dl.Logf(level, format, args...)
}
