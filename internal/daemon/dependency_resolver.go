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
//
// Lineage-aware: when a recorded BlockedBy entry has been superseded by a
// retry/repair successor, the successor's effective status is what counts.
// This lets a queue task whose declared dep is now cancelled (with a
// "superseded_by_*" reason) recognise its lineage as still in flight or
// completed without needing the queue's BlockedBy slice to be rewritten.
//
// Integration-merge aware: tasks that execute on the integration worktree
// (RunOnIntegration) or the main branch (RunOnMain) need their cross-phase
// dependencies' commits to actually be present in integration, not just for
// the dep task to be queue-completed. We additionally gate on the merge
// gate (the same gate isPhaseMergeRecorded uses) so a verify_repair retry
// with RunOnIntegration=true does not run against a stale integration tree.
// Same-phase deps are intentionally NOT gated here: phase merge happens at
// phase boundary, so requiring it for a same-phase dep would deadlock.
func (dr *DependencyResolver) IsTaskBlocked(task *model.Task) (bool, error) {
	if len(task.BlockedBy) == 0 {
		return false, nil
	}

	if dr.stateManager == nil {
		return len(task.BlockedBy) > 0, fmt.Errorf("state manager not configured; cannot resolve dependencies")
	}

	for _, depTaskID := range task.BlockedBy {
		status, err := dr.stateManager.GetEffectiveTaskStatus(task.CommandID, depTaskID)
		if err != nil {
			dr.log(LogLevelWarn, "dependency_check task=%s dep=%s error=%v", task.ID, depTaskID, err)
			return true, err
		}
		if status != model.StatusCompleted {
			dr.log(LogLevelDebug, "task_blocked task=%s blocked_by=%s effective_status=%s",
				task.ID, depTaskID, status)
			return true, nil
		}
	}

	if task.RunOnIntegration || task.RunOnMain {
		if blocked, depID, depPhase := dr.isBlockedOnIntegrationMerge(task); blocked {
			dr.log(LogLevelInfo,
				"task_blocked_on_integration_merge task=%s run_on_integration=%v run_on_main=%v dep=%s dep_phase=%s "+
					"(awaiting phase merge so dep commits reach integration)",
				task.ID, task.RunOnIntegration, task.RunOnMain, depID, depPhase)
			return true, nil
		}
	}

	dr.log(LogLevelDebug, "task_unblocked task=%s", task.ID)
	return false, nil
}

// isBlockedOnIntegrationMerge reports whether `task` (which is RunOnIntegration
// or RunOnMain) has any cross-phase dep whose phase merge has not yet been
// recorded. Returns the offending dep+phase for logging.
//
// Same-phase deps are skipped: requiring phase merge for an in-phase dep
// would deadlock — phase merge happens only when every required task is
// terminal, including this one.
//
// Phaseless commands (commands without declared phases) are always allowed:
// the merge gate has nothing meaningful to say there.
func (dr *DependencyResolver) isBlockedOnIntegrationMerge(task *model.Task) (bool, string, string) {
	phaseByTask, err := dr.buildTaskToPhaseMap(task.CommandID)
	if err != nil || len(phaseByTask) == 0 {
		return false, "", ""
	}
	// Tasks don't carry a PhaseID field directly — derive it from the
	// task-to-phase map we already built.
	currentPhaseID := phaseByTask[task.ID]
	for _, depTaskID := range task.BlockedBy {
		depPhaseID := phaseByTask[depTaskID]
		if depPhaseID == "" || depPhaseID == currentPhaseID {
			continue
		}
		if !dr.mergeGateAllows(task.CommandID, depPhaseID) {
			return true, depTaskID, depPhaseID
		}
	}
	return false, "", ""
}

// buildTaskToPhaseMap returns a {taskID -> phaseID} map for every required
// task across all of the command's phases. Returns an empty map (with nil
// error) for phaseless commands so callers can short-circuit cleanly.
func (dr *DependencyResolver) buildTaskToPhaseMap(commandID string) (map[string]string, error) {
	phases, err := dr.stateManager.GetCommandPhases(commandID)
	if err != nil {
		if errors.Is(err, ErrStateNotFound) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := make(map[string]string, len(phases)*4)
	for _, p := range phases {
		for _, tid := range p.RequiredTaskIDs {
			out[tid] = p.ID
		}
	}
	return out, nil
}

// CheckDependencyFailure checks if any of a task's dependencies have failed.
// Returns the failed dependency ID and status, or empty string if none failed.
//
// Lineage-aware: a dep cancelled with a "superseded_by_*" reason is NOT a
// failure when its successor is still running or has completed. Only when
// the latest descendant in the lineage chain is itself failed/cancelled is
// the dependency treated as terminal-failure for cascade purposes. This
// prevents the daemon from cancelling a downstream task while a verify-repair
// retry of its upstream is in flight or has already succeeded.
func (dr *DependencyResolver) CheckDependencyFailure(task *model.Task) (string, model.Status, error) {
	if len(task.BlockedBy) == 0 || dr.stateManager == nil {
		return "", "", nil
	}

	for _, depTaskID := range task.BlockedBy {
		status, err := dr.stateManager.GetEffectiveTaskStatus(task.CommandID, depTaskID)
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
	// CancelledReason is the structured cancellation marker the resolver
	// wants to persist alongside this transition. Only populated when
	// NewStatus==PhaseStatusCancelled and the cancellation is
	// resolver-driven (cascade). Apply paths copy this into
	// Phase.CancelledReason; nil/empty means leave the field unchanged.
	CancelledReason string
	// ClearCancelledReason asks the apply path to clear
	// Phase.CancelledReason as part of this transition. Used when the
	// resolver flips a cascade-cancelled phase back to pending — the
	// stale dep-id marker would otherwise mislead a future cascade
	// recovery into reusing it.
	ClearCancelledReason bool
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

	// Pass 3: process cancelled phases that may have been cascaded from a
	// dependency that has since recovered. Without this pass, a phase
	// cancelled because its upstream failed stays cancelled forever even
	// when an add-retry-task reopens that upstream — the planner's plan
	// gets stuck waiting for a downstream phase that is structurally
	// dead. Restricting the recovery to cancellations carrying
	// model.DependencyCascadeCancelPrefix in CancelledReason ensures
	// operator/manual cancellations (no marker) remain terminal.
	for _, phase := range phases {
		if phase.Status != model.PhaseStatusCancelled {
			continue
		}
		tr := dr.checkCancelledPhaseRecovery(commandID, phaseMap, phase)
		if tr != nil {
			transitions = append(transitions, *tr)
		}
	}

	return transitions, nil
}

// checkActivePhaseCompletion checks if an active phase is complete, failed, or cancelled.
//
// Lineage-aware: when a task has been superseded by a verify-repair or
// planner-driven retry, the predecessor's raw cancelled status would
// otherwise drive the phase to PhaseStatusCancelled — and the dependency
// resolver would then cascade-cancel every downstream phase. The plan-
// level DeriveStatus already walks retry_lineage; the same effective view
// must drive phase-level transitions or the phase machine produces
// decisions the plan machine then has to undo.
//
// Observes phase.TaskIDs (the full set of tasks attached to the phase),
// not just RequiredTaskIDs, so an all-optional phase still completes
// when its work is done. RequiredTaskIDs still informs plan-level
// pass/fail policy via DeriveStatus; phase-level transitions stay
// neutral on the required/optional distinction.
func (dr *DependencyResolver) checkActivePhaseCompletion(commandID string, phase PhaseInfo) *PhaseTransitionResult {
	taskIDs := phase.TaskIDs
	if len(taskIDs) == 0 {
		// Backwards-compat fallback: callers that have not yet been
		// updated to populate PhaseInfo.TaskIDs (mostly older test mocks)
		// fall through to the legacy RequiredTaskIDs view rather than
		// silently returning nil.
		taskIDs = phase.RequiredTaskIDs
	}
	if len(taskIDs) == 0 {
		return nil
	}

	// A phase's Failed/Cancelled transition is driven only by its REQUIRED
	// tasks, mirroring DeriveStatus (which computes hasFailed/hasCancelled
	// from state.RequiredTaskIDs alone). An optional task that failed or was
	// cancelled is terminal but tolerated: forcing the whole phase to Failed
	// on an optional failure contradicts CompletionPolicy.OnOptionalFailed
	// (default "ignore") and used to cascade-cancel/block dependent phases for
	// a plan the policy says should still succeed. The rare
	// OnOptionalFailed="fail_command" still fails the *command* at the plan
	// level via DeriveStatus, so leaving the phase non-failed here is safe.
	requiredSet := make(map[string]bool, len(phase.RequiredTaskIDs))
	for _, id := range phase.RequiredTaskIDs {
		requiredSet[id] = true
	}

	allCompleted := true
	hasFailed := false
	hasCancelled := false

	for _, taskID := range taskIDs {
		// Completion-aware lens: cascade-cancelled tasks whose upstream
		// lineage has effectively completed are treated as Completed so
		// the phase machine can transition out of Active even when the
		// daemon-side verify-repair path delivered the upstream work
		// without going through cascadeRecover. Without this, the phase
		// stays Active forever and downstream phases / plan completion
		// stall.
		status, err := dr.stateManager.GetEffectiveTaskStatusForCompletion(commandID, taskID)
		if err != nil {
			dr.log(LogLevelWarn, "phase_check task_state error phase=%s task=%s error=%v",
				phase.ID, taskID, err)
			allCompleted = false
			continue
		}

		// RequiredTaskIDs may be empty for an all-optional phase; in that case
		// every task is optional and the phase still completes when its work is
		// done (the historical "all-optional phase completes" behaviour).
		isRequired := requiredSet[taskID]

		switch status {
		case model.StatusCompleted:
			// ok
		case model.StatusFailed:
			if isRequired {
				hasFailed = true
			}
		case model.StatusCancelled:
			if isRequired {
				hasCancelled = true
			}
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
			// Encode the upstream dep into the human Reason via the
			// canonical helper so cancelled-phase recovery (Pass 3
			// above) can extract it without parsing free-form text.
			result := &PhaseTransitionResult{
				PhaseID:         phase.ID,
				PhaseName:       phase.Name,
				OldStatus:       phase.Status,
				NewStatus:       model.PhaseStatusCancelled,
				Reason:          fmt.Sprintf("dependency phase %s is %s", depID, dep.Status),
				CancelledReason: model.NewDependencyCascadeCancelReason(depID),
			}
			// commandID is not available in this method, caller will publish event
			return result
		}
	}

	return nil
}

// checkCancelledPhaseRecovery undoes a previously-cascaded cancellation
// when the upstream dependency has been reopened (typically via an
// add-retry-task that brought a failed phase back to active). The
// recovery is gated by Phase.CancelledReason so operator/manual
// cancellations — which never carry the model.DependencyCascadeCancelPrefix
// marker — stay terminal.
//
// The reopened upstream's status must be live OR completed: active,
// awaiting_fill, filling, or completed. If the upstream is still
// pending we leave the downstream cancelled — it will recover on the
// next scan once the upstream activates, avoiding a cancelled→pending
// flip while the upstream is still dependency-bound itself.
func (dr *DependencyResolver) checkCancelledPhaseRecovery(commandID string, phaseMap map[string]PhaseInfo, phase PhaseInfo) *PhaseTransitionResult {
	depID, ok := model.DependencyCascadeDepID(phase.CancelledReason)
	if !ok {
		// Operator/manual cancel or pre-marker legacy state: never auto-recover.
		return nil
	}
	dep, ok := phaseMap[depID]
	if !ok {
		// Recorded dep no longer exists in the plan — the prior
		// cancellation reference is stale and unverifiable. Leave
		// terminal.
		return nil
	}
	switch dep.Status {
	case model.PhaseStatusActive,
		model.PhaseStatusAwaitingFill,
		model.PhaseStatusFilling,
		model.PhaseStatusCompleted:
		// Cause invalidated → recover.
	default:
		return nil
	}
	dr.log(LogLevelInfo,
		"phase_cancellation_recovered command=%s phase=%s recorded_dep=%s dep_status=%s",
		commandID, phase.ID, depID, dep.Status)
	return &PhaseTransitionResult{
		PhaseID:   phase.ID,
		PhaseName: phase.Name,
		OldStatus: phase.Status,
		NewStatus: model.PhaseStatusPending,
		Reason: fmt.Sprintf(
			"cascade-cancellation recovered: dependency phase %s is now %s",
			depID, dep.Status,
		),
		// Clear the marker so a subsequent operator-initiated cancel
		// does not get confused with the cascade case.
		ClearCancelledReason: true,
	}
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
