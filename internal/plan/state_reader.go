package plan

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

const defaultCacheTTL = 2 * time.Second

type cacheEntry struct {
	state    *model.CommandState
	loadedAt time.Time
}

// PlanStateReader implements the StateReader/StateWriter interfaces by reading/writing state/commands/ YAML files.
// The name retains the Plan prefix for clarity at call sites outside this package.
// Read methods use a TTL-based cache to avoid redundant LoadState calls within a single scan cycle.
type PlanStateReader struct { //nolint:revive // stuttering name kept for clarity at external call sites
	stateManager *StateManager

	mu       sync.Mutex
	cache    map[string]*cacheEntry
	cacheTTL time.Duration
}

// NewPlanStateReader creates a PlanStateReader backed by the given StateManager.
func NewPlanStateReader(sm *StateManager) *PlanStateReader {
	return &PlanStateReader{
		stateManager: sm,
		cache:        make(map[string]*cacheEntry),
		cacheTTL:     defaultCacheTTL,
	}
}

// SetCacheTTL sets the cache time-to-live. A TTL of 0 disables caching.
func (r *PlanStateReader) SetCacheTTL(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheTTL = d
}

// InvalidateCache removes the cached state for a specific command.
func (r *PlanStateReader) InvalidateCache(commandID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, commandID)
}

// InvalidateAll clears the entire state cache.
func (r *PlanStateReader) InvalidateAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = make(map[string]*cacheEntry)
}

// loadStateWithCache returns the cached state if still within TTL, otherwise loads
// from disk via StateManager and updates the cache.
//
// TOCTOU note: There is a deliberate window between the cache-miss unlock and the
// disk read (LoadState) where another goroutine could load and cache the same command.
// This is acceptable because (a) the cache is TTL-based and a duplicate load only wastes
// a single extra read, and (b) state mutations go through StateManager under the command
// lock, so the loaded data is always a consistent snapshot. Holding the mutex across the
// disk read would serialize all readers and defeat the purpose of the cache.
func (r *PlanStateReader) loadStateWithCache(commandID string) (*model.CommandState, error) {
	now := time.Now()

	r.mu.Lock()
	if r.cacheTTL > 0 {
		if entry, ok := r.cache[commandID]; ok {
			if now.Sub(entry.loadedAt) < r.cacheTTL {
				r.mu.Unlock()
				return entry.state, nil
			}
		}
	}
	r.mu.Unlock()

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.cacheTTL > 0 {
		r.cache[commandID] = &cacheEntry{
			state:    state,
			loadedAt: now,
		}
	}
	r.mu.Unlock()

	return state, nil
}

// HasNonTerminalTaskState reports whether any task in the command state file
// is at a non-terminal status (paused_for_replan, repair_pending,
// verify_pending, in_progress, etc.). The reconciler-driven status surfaces
// (state/commands/<cmd>.yaml) and the queue files can disagree: a worker may
// have committed a "completed" queue entry that the daemon's verify path
// then transitioned to repair_pending or paused_for_replan in state without
// touching the queue. Fast-track stall cleanup must consult this view so it
// does not delete a worktree while a pending retry-task or replan signal is
// still in flight.
//
// Returns ErrStateNotFound when the state file does not exist; the caller
// can treat that as "no in-flight resolution" (commands without state files
// have nothing for the Planner to act on).
func (r *PlanStateReader) HasNonTerminalTaskState(commandID string) (bool, error) {
	state, err := r.loadStateWithCache(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, model.ErrStateNotFound
		}
		return false, err
	}
	for _, status := range state.TaskStates {
		if !model.IsTerminal(status) {
			return true, nil
		}
	}
	return false, nil
}

// GetNonTerminalTaskStates returns every TaskStates entry whose status is
// non-terminal. The result is a fresh map; mutations by the caller do not
// affect the cached state. Returns ErrStateNotFound when the state file
// does not exist.
func (r *PlanStateReader) GetNonTerminalTaskStates(commandID string) (map[string]model.Status, error) {
	state, err := r.loadStateWithCache(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, model.ErrStateNotFound
		}
		return nil, err
	}
	out := make(map[string]model.Status, len(state.TaskStates))
	for taskID, status := range state.TaskStates {
		if !model.IsTerminal(status) {
			out[taskID] = status
		}
	}
	return out, nil
}

// GetTaskState returns the status of a task from the command state file.
func (r *PlanStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	state, err := r.loadStateWithCache(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", model.ErrStateNotFound
		}
		return "", err
	}

	status, ok := state.TaskStates[taskID]
	if !ok {
		return "", fmt.Errorf("task %s in command %s: %w", taskID, commandID, model.ErrTaskNotFound)
	}
	return status, nil
}

// GetEffectiveTaskStatus returns the status of a task after walking
// retry_lineage forward to the latest descendant. When the predecessor was
// superseded by a successful retry/repair (cancelled with a "superseded_by_*"
// reason), the lineage successor's status is the truth — the predecessor's
// raw cancelled status is just a structural marker. Callers that want to
// know "is this lineage effectively satisfied?" should use this method
// rather than the raw GetTaskState.
//
// Falls back to GetTaskState semantics when no lineage entry maps from
// taskID, including the original ErrTaskNotFound when the task is unknown.
func (r *PlanStateReader) GetEffectiveTaskStatus(commandID, taskID string) (model.Status, error) {
	state, err := r.loadStateWithCache(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", model.ErrStateNotFound
		}
		return "", err
	}
	if _, ok := state.TaskStates[taskID]; !ok {
		return "", fmt.Errorf("task %s in command %s: %w", taskID, commandID, model.ErrTaskNotFound)
	}
	return EffectiveStatus(taskID, state.TaskStates, state.RetryLineage), nil
}

// GetEffectiveTaskStatusForCompletion returns the status through the
// completion-aware lens: like GetEffectiveTaskStatus, but additionally
// unwinds cascade-cancellations (CancelledReasons["blocked_dependency_terminal:<dep>"])
// whose upstream lineage has effectively completed. Used by plan/phase
// completion checks so cascade-stragglers whose required predecessor was
// delivered via verify-repair do not block PlanStatusCompleted (Bug-D'-prime).
//
// Falls back to GetEffectiveTaskStatus semantics for unknown tasks.
func (r *PlanStateReader) GetEffectiveTaskStatusForCompletion(commandID, taskID string) (model.Status, error) {
	state, err := r.loadStateWithCache(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", model.ErrStateNotFound
		}
		return "", err
	}
	if _, ok := state.TaskStates[taskID]; !ok {
		return "", fmt.Errorf("task %s in command %s: %w", taskID, commandID, model.ErrTaskNotFound)
	}
	return EffectiveStatusForCompletion(taskID, state), nil
}

// GetCommandPhases returns phase metadata for a command.
func (r *PlanStateReader) GetCommandPhases(commandID string) ([]model.PhaseInfo, error) {
	state, err := r.loadStateWithCache(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, model.ErrStateNotFound
		}
		return nil, err
	}

	if len(state.Phases) == 0 {
		return []model.PhaseInfo{}, nil
	}

	// Build phase-name-to-ID lookup once (not per-phase)
	phaseNameToID := make(map[string]string, len(state.Phases))
	for _, sp := range state.Phases {
		phaseNameToID[sp.Name] = sp.PhaseID
	}

	phases := make([]model.PhaseInfo, 0, len(state.Phases))
	for _, p := range state.Phases {
		var depIDs []string
		for _, depName := range p.DependsOnPhases {
			if id, ok := phaseNameToID[depName]; ok {
				depIDs = append(depIDs, id)
			}
		}

		// Collect required task IDs for this phase
		phaseTaskSet := make(map[string]bool)
		for _, tid := range p.TaskIDs {
			phaseTaskSet[tid] = true
		}
		var requiredTaskIDs []string
		for _, tid := range state.RequiredTaskIDs {
			if phaseTaskSet[tid] {
				requiredTaskIDs = append(requiredTaskIDs, tid)
			}
		}

		isSystemCommit := state.SystemCommitTaskID != nil && phaseTaskSet[*state.SystemCommitTaskID]

		// Clone phase.TaskIDs so callers can mutate the returned slice
		// without affecting the cached state struct.
		phaseTaskIDs := make([]string, len(p.TaskIDs))
		copy(phaseTaskIDs, p.TaskIDs)

		phases = append(phases, model.PhaseInfo{
			ID:                          p.PhaseID,
			Name:                        p.Name,
			TaskIDs:                     phaseTaskIDs,
			Status:                      p.Status,
			DependsOn:                   depIDs,
			FillDeadlineAt:              p.FillDeadlineAt,
			RequiredTaskIDs:             requiredTaskIDs,
			SystemCommitTask:            isSystemCommit,
			CancelledReason:             p.CancelledReason,
			AwaitingFillSince:           p.AwaitingFillSince,
			AwaitingFillStallNotifiedAt: p.AwaitingFillStallNotifiedAt,
		})
	}

	return phases, nil
}

// GetTaskDependencies returns the task IDs that the given task depends on.
func (r *PlanStateReader) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	state, err := r.loadStateWithCache(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, model.ErrStateNotFound
		}
		return nil, err
	}

	deps, ok := state.TaskDependencies[taskID]
	if !ok {
		return nil, nil
	}
	return deps, nil
}

// ApplyPhaseTransition persists a phase status change under the command lock.
func (r *PlanStateReader) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	r.stateManager.LockCommand(commandID)
	defer r.stateManager.UnlockCommand(commandID)

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return err
	}

	now := nowUTC()
	idx, found := state.PhaseIndex(phaseID)
	if !found {
		return fmt.Errorf("phase %s in command %s: %w", phaseID, commandID, model.ErrPhaseNotFound)
	}

	p := &state.Phases[idx]
	if err := model.ValidatePhaseTransition(p.Status, newStatus); err != nil {
		return fmt.Errorf("phase %s in command %s: %w", phaseID, commandID, err)
	}
	wasAwaitingFill := p.Status == model.PhaseStatusAwaitingFill
	p.Status = newStatus
	if model.IsPhaseTerminal(newStatus) {
		p.CompletedAt = &now
	}
	if newStatus == model.PhaseStatusActive {
		p.ActivatedAt = &now
	}
	if newStatus == model.PhaseStatusAwaitingFill {
		// AwaitingFillSince is the watchdog clock — record entry time so
		// stepAwaitingFillWatchdog can re-prompt a stalled Planner long
		// before R6's hard fill_deadline timeout fires. Always overwrite
		// so re-entries restart the clock.
		p.AwaitingFillSince = &now
		// Reset the per-entry "watchdog already fired" marker so the
		// watchdog can fire once for this awaiting_fill window.
		p.AwaitingFillStallNotifiedAt = nil
		if p.Constraints != nil && p.Constraints.TimeoutMinutes > 0 {
			deadline := time.Now().UTC().Add(time.Duration(p.Constraints.TimeoutMinutes) * time.Minute).Format(time.RFC3339)
			p.FillDeadlineAt = &deadline
		}
	} else if wasAwaitingFill {
		// Phase exited awaiting_fill (filling, completed, cancelled, etc.).
		// Clear the watchdog tracking fields so a future re-entry starts
		// fresh and so a completed phase does not retain stale
		// "awaiting_fill_since" data that could mislead audit reads.
		p.AwaitingFillSince = nil
		p.AwaitingFillStallNotifiedAt = nil
	}

	state.UpdatedAt = now
	if err := r.stateManager.SaveState(state); err != nil {
		return err
	}
	r.InvalidateCache(commandID)
	return nil
}

// MarkAwaitingFillStallNotified records that the awaiting-fill watchdog has
// emitted a stall signal for the given phase. Idempotent at the field level
// — overwriting the same timestamp is harmless. No-op when the phase is no
// longer at awaiting_fill, because the watchdog observation that drove the
// call is by definition stale in that case (a fill happened between the
// scan-time read and the per-phase write under state lock).
func (r *PlanStateReader) MarkAwaitingFillStallNotified(commandID, phaseID, notifiedAt string) error {
	r.stateManager.LockCommand(commandID)
	defer r.stateManager.UnlockCommand(commandID)

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return err
	}

	idx, found := state.PhaseIndex(phaseID)
	if !found {
		return fmt.Errorf("phase %s in command %s: %w", phaseID, commandID, model.ErrPhaseNotFound)
	}

	p := &state.Phases[idx]
	if p.Status != model.PhaseStatusAwaitingFill {
		// Phase exited awaiting_fill between the watchdog's read and this
		// write — silently drop so we do not write a stale marker that
		// would confuse a subsequent re-entry's clean-slate state.
		return nil
	}
	p.AwaitingFillStallNotifiedAt = &notifiedAt
	state.UpdatedAt = nowUTC()
	if err := r.stateManager.SaveState(state); err != nil {
		return err
	}
	r.InvalidateCache(commandID)
	return nil
}

// SetPhaseCancelledReason persists Phase.CancelledReason for a single
// phase under the command lock. Pass nil to clear the field. Used by
// the dependency resolver to record cascade-cancellation provenance
// (model.DependencyCascadeCancelPrefix) so cancelled-phase recovery
// can later distinguish auto-recoverable cascade cancellations from
// operator/manual cancellations.
//
// Idempotent: writing the same value twice is a no-op (the underlying
// SaveState still runs, but no semantic change).
func (r *PlanStateReader) SetPhaseCancelledReason(commandID, phaseID string, reason *string) error {
	r.stateManager.LockCommand(commandID)
	defer r.stateManager.UnlockCommand(commandID)

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return err
	}

	idx, found := state.PhaseIndex(phaseID)
	if !found {
		return fmt.Errorf("phase %s in command %s: %w", phaseID, commandID, model.ErrPhaseNotFound)
	}
	p := &state.Phases[idx]
	if reason == nil {
		p.CancelledReason = nil
	} else {
		// Copy so the caller can mutate the source string without
		// corrupting the persisted state.
		s := *reason
		p.CancelledReason = &s
	}
	state.UpdatedAt = nowUTC()
	if err := r.stateManager.SaveState(state); err != nil {
		return err
	}
	r.InvalidateCache(commandID)
	return nil
}

// UpdateTaskState updates a single task's status under the command lock.
func (r *PlanStateReader) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	r.stateManager.LockCommand(commandID)
	defer r.stateManager.UnlockCommand(commandID)

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return err
	}

	if state.TaskStates == nil {
		state.TaskStates = make(map[string]model.Status)
	}

	// Verify taskID is a known task (exists in required, optional, or system commit)
	if !isKnownTaskID(state, taskID) {
		return fmt.Errorf("task %s in command %s: %w", taskID, commandID, model.ErrTaskNotFound)
	}

	if currentStatus, exists := state.TaskStates[taskID]; exists {
		if err := model.ValidateTaskStateTransition(currentStatus, newStatus); err != nil {
			return fmt.Errorf("task %s in command %s: %w", taskID, commandID, err)
		}
	}

	state.TaskStates[taskID] = newStatus

	if cancelledReason != "" {
		if state.CancelledReasons == nil {
			state.CancelledReasons = make(map[string]string)
		}
		state.CancelledReasons[taskID] = cancelledReason
	}

	state.UpdatedAt = nowUTC()
	if err := r.stateManager.SaveState(state); err != nil {
		return err
	}
	r.InvalidateCache(commandID)
	return nil
}

// IsCommandCancelRequested checks the cancel.requested flag in the state file.
func (r *PlanStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	state, err := r.loadStateWithCache(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, model.ErrStateNotFound
		}
		return false, err
	}
	return state.Cancel.Requested, nil
}

// IsSystemCommitReady checks if a task is a system commit task and whether
// all user tasks/phases are terminal.
func (r *PlanStateReader) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	state, err := r.loadStateWithCache(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, model.ErrStateNotFound
		}
		return false, false, err
	}

	if state.SystemCommitTaskID == nil || *state.SystemCommitTaskID != taskID {
		return false, false, nil
	}

	// Non-phased command: check all user tasks (except self) are terminal
	if len(state.Phases) == 0 {
		for tid, s := range state.TaskStates {
			if tid == taskID {
				continue
			}
			if !model.IsTerminal(s) {
				return true, false, nil
			}
		}
		return true, true, nil
	}

	// Phased command: check all user phases are terminal
	for _, phase := range state.Phases {
		if !model.IsPhaseTerminal(phase.Status) {
			return true, false, nil
		}
	}
	return true, true, nil
}

// MarkCircuitBreakerProgress refreshes CircuitBreaker.LastProgressAt to "now"
// under the command lock. Used by the periodic scan when it observes a
// liveness signal (e.g. pane-active extension) so the progress-timeout
// path does not falsely trip on a long-running task whose execution
// legitimately exceeds progress_timeout_minutes. ErrStateNotFound is
// returned for unknown commands so callers can ignore it as a no-op.
func (r *PlanStateReader) MarkCircuitBreakerProgress(commandID string) error {
	r.stateManager.LockCommand(commandID)
	defer r.stateManager.UnlockCommand(commandID)

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return model.ErrStateNotFound
		}
		return err
	}

	now := nowUTC()
	state.CircuitBreaker.LastProgressAt = &now
	state.UpdatedAt = now
	if err := r.stateManager.SaveState(state); err != nil {
		return err
	}
	r.InvalidateCache(commandID)
	return nil
}

// GetCircuitBreakerState returns the circuit breaker state for a command.
func (r *PlanStateReader) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	state, err := r.loadStateWithCache(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, model.ErrStateNotFound
		}
		return nil, err
	}
	cb := state.CircuitBreaker
	return &cb, nil
}

// TripCircuitBreaker sets the circuit breaker to tripped state under the command lock.
func (r *PlanStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	r.stateManager.LockCommand(commandID)
	defer r.stateManager.UnlockCommand(commandID)

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return err
	}

	if state.CircuitBreaker.Tripped {
		return nil // already tripped, idempotent
	}

	// Re-validate progress timeout under lock to prevent TOCTOU race:
	// A concurrent success write may have updated LastProgressAt since the unlocked read.
	if progressTimeoutMinutes > 0 && state.CircuitBreaker.LastProgressAt != nil {
		lastProgress, parseErr := time.Parse(time.RFC3339, *state.CircuitBreaker.LastProgressAt)
		if parseErr == nil && time.Since(lastProgress) < time.Duration(progressTimeoutMinutes)*time.Minute {
			return nil // timeout no longer exceeded
		}
	}

	now := nowUTC()
	state.CircuitBreaker.Tripped = true
	state.CircuitBreaker.TrippedAt = &now
	state.CircuitBreaker.TripReason = &reason

	// Set cancel request so existing cancel flow handles task cancellation
	if !state.Cancel.Requested {
		state.Cancel.Requested = true
		state.Cancel.RequestedAt = &now
		state.Cancel.RequestedBy = ptr.String("circuit_breaker")
		state.Cancel.Reason = &reason
	}

	state.UpdatedAt = now
	if err := r.stateManager.SaveState(state); err != nil {
		return err
	}
	r.InvalidateCache(commandID)
	return nil
}

// isKnownTaskID checks whether taskID belongs to the command's known tasks
// (required, optional, or system commit).
func isKnownTaskID(state *model.CommandState, taskID string) bool {
	for _, id := range state.RequiredTaskIDs {
		if id == taskID {
			return true
		}
	}
	for _, id := range state.OptionalTaskIDs {
		if id == taskID {
			return true
		}
	}
	if state.SystemCommitTaskID != nil && *state.SystemCommitTaskID == taskID {
		return true
	}
	return false
}
