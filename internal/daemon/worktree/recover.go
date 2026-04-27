package worktree

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// Sentinel errors returned by operator-recovery entry points (Unquarantine,
// ResumeMerge). They are wrapped with %w so callers can use errors.Is.
var (
	// ErrNoWorktreeState indicates that .maestro/state/worktrees/<cmd>.yaml
	// does not exist (the command did not use worktree mode, or has not
	// reached the integration phase yet).
	ErrNoWorktreeState = errors.New("no worktree state for command")

	// ErrAlreadyResolved indicates that the integration is not in a state
	// that the requested recovery operation applies to (e.g. unquarantine
	// called on a non-quarantined integration). The state file is left
	// untouched, so retrying the operation is a safe no-op.
	ErrAlreadyResolved = errors.New("integration is already resolved")
)

// Unquarantine clears the quarantine state of an integration branch and
// returns it to IntegrationStatusFailed so the next Phase A queue scan can
// re-enqueue merge attempts. Counters (MergeFailureCount, QuarantinedAt,
// QuarantineReason) are reset.
//
// This is the explicit operator escape hatch from the otherwise-terminal
// Quarantined state. Because Quarantined→Failed is intentionally absent from
// validIntegrationTransitions, the field is assigned directly rather than
// going through setIntegrationStatus.
//
// Idempotency: a second call when the integration is no longer quarantined
// returns ErrAlreadyResolved without touching the state file.
func (wm *Manager) Unquarantine(commandID string, reason string) error {
	if err := validateIDs(commandID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoWorktreeState
		}
		return fmt.Errorf("load state: %w", err)
	}
	if state.Integration.Status != model.IntegrationStatusQuarantined {
		return fmt.Errorf("%w: status=%s", ErrAlreadyResolved, state.Integration.Status)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	state.Integration.Status = model.IntegrationStatusFailed
	state.Integration.UpdatedAt = now
	state.Integration.MergeFailureCount = 0
	state.Integration.QuarantinedAt = ""
	state.Integration.QuarantineReason = ""
	state.Integration.QuarantineSource = ""
	state.Integration.StallSignaled = false
	state.UpdatedAt = now

	wm.Log(core.LogLevelInfo, "unquarantine command=%s reason=%q", commandID, reason)
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// RetryPublish resets publish failure state (PublishFailureCount,
// NextPublishRetryAt, PublishConflictFiles) and transitions the integration
// from publish_failed or quarantined (publish-related) back to merged so the
// next Phase A scan re-enqueues the publish attempt.
//
// This is the Planner-accessible recovery path for publish conflicts. After
// the Planner dispatches workers to resolve conflicts on the integration
// branch, it calls this command to trigger a re-publish.
//
// Idempotency: a call when the integration is already merged returns
// ErrAlreadyResolved without touching the state file.
func (wm *Manager) RetryPublish(commandID string) error {
	if err := validateIDs(commandID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoWorktreeState
		}
		return fmt.Errorf("load state: %w", err)
	}

	s := state.Integration.Status
	switch s {
	case model.IntegrationStatusPublishFailed:
		// recoverable
	case model.IntegrationStatusQuarantined:
		if state.Integration.QuarantineSource != model.QuarantineSourcePublish {
			return fmt.Errorf("%w: quarantine is not publish-related; use unquarantine", ErrAlreadyResolved)
		}
		// publish-related quarantine — allow recovery
	case model.IntegrationStatusMerged:
		return fmt.Errorf("%w: integration is already merged", ErrAlreadyResolved)
	default:
		return fmt.Errorf("%w: status=%s is not recoverable by retry-publish", ErrAlreadyResolved, s)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)

	// Transition to merged so Phase A re-enqueues publish.
	// For quarantined, bypass the state machine (same pattern as Unquarantine).
	if s == model.IntegrationStatusQuarantined {
		state.Integration.Status = model.IntegrationStatusMerged
		state.Integration.QuarantinedAt = ""
		state.Integration.QuarantineReason = ""
		state.Integration.QuarantineSource = ""
		state.Integration.StallSignaled = false
	} else {
		if err := wm.setIntegrationStatus(state, model.IntegrationStatusMerged, now); err != nil {
			return err
		}
	}

	state.Integration.PublishFailureCount = 0
	state.Integration.NextPublishRetryAt = ""
	state.Integration.PublishConflictFiles = nil
	state.Integration.PublishConflictSignaled = false
	state.Integration.UpdatedAt = now
	state.UpdatedAt = now

	wm.Log(core.LogLevelInfo, "retry_publish command=%s prev_status=%s", commandID, s)
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// ResumeMerge pipeline (ResumeMerge, finalizeResumeMergeIntegrationStatus,
// allWorkersMergeTerminal, revertContentMismatchedWorkers, verifyWorkersMerged,
// attemptResolvedMerges, tryMergeWorker, commitResolvedWorkerChanges,
// resetWorkersToActive, mergeResolvedWorker, abortAndReturnMergeError,
// checkoutResolvedFilesFromBranch) lives in recover_resume.go after the
// F-041 step 4 physical file split.

// ResetResolvingWorkerToConflict transitions a single worker from
// WorktreeStatusResolving back to WorktreeStatusConflict, but only when it is
// currently resolving. It is the fast cleanup path for the case where a
// merge_conflict resolution task fails: without this, the worker stays in
// resolving until R7's resolvingStallTimeout sweep (~20 minutes), blocking
// all forward progress on the command.
//
// Caller contract: invoke this only when the reporter worker has reported a
// non-completed result (failed / verify_failed / etc.) for what *was*
// dispatched as a merge_conflict resolution task — i.e. the worker was in
// resolving status at dispatch time. Calling this for unrelated tasks is
// harmless because the worker will not be in resolving status (the no-op
// branch returns nil), but the caller should still gate on the failure
// status to avoid pointless state file IO.
//
// Idempotent: returns nil when the worker is not in resolving status or does
// not exist in the state file. Returns ErrNoWorktreeState when the command
// has no worktree state at all (already torn down).
func (wm *Manager) ResetResolvingWorkerToConflict(commandID, workerID string) error {
	if err := validateIDs(commandID); err != nil {
		return err
	}
	if err := validate.ID(workerID); err != nil {
		return fmt.Errorf("invalid workerID: %w", err)
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoWorktreeState
		}
		return fmt.Errorf("load state: %w", err)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	mutated := false
	for i := range state.Workers {
		ws := &state.Workers[i]
		if ws.WorkerID != workerID {
			continue
		}
		if ws.Status != model.WorktreeStatusResolving {
			return nil // not resolving — idempotent no-op
		}
		ws.Status = model.WorktreeStatusConflict
		ws.UpdatedAt = now
		mutated = true
		break
	}
	if !mutated {
		return nil
	}
	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	wm.Log(core.LogLevelInfo, "resolving_worker_reset_to_conflict command=%s worker=%s", commandID, workerID)
	return nil
}

// AutoRecover dispatch + state-driven action selection (AutoRecoverAction
// type/const, AutoRecover, AutoRecoverAfterResolution,
// tryPublishConflictResolutionRecovery, tryMergeConflictResolutionRecovery,
// workerIsResolving, selectAutoRecoverAction, publishBackoffElapsed) live in
// recover_auto.go after the F-041 step 4 physical file split.

// ResolveConflict marks a per-phase, per-worker merge conflict as resolved
// after an operator has manually fixed up the integration branch. It removes
// the worker from CommitFailedWorkers (the gating list that blocks
// publish-to-base) and resets the merge failure counter so that the next Phase
// A scan can re-enqueue the merge attempt for the named phase.
//
// Idempotency: returns ErrAlreadyResolved when the worker is not in the
// commit-failed list and the integration is not in a recoverable state.
func (wm *Manager) ResolveConflict(commandID, phaseID, workerID string) error {
	if err := validateCommandAndPhaseIDs(commandID, phaseID); err != nil {
		return err
	}
	if err := validate.ID(workerID); err != nil {
		return fmt.Errorf("invalid workerID: %w", err)
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoWorktreeState
		}
		return fmt.Errorf("load state: %w", err)
	}

	removed := false
	filtered := state.CommitFailedWorkers[:0]
	for _, w := range state.CommitFailedWorkers {
		if w == workerID {
			removed = true
			continue
		}
		filtered = append(filtered, w)
	}
	if !removed {
		return fmt.Errorf("%w: worker %s is not in commit_failed_workers for command %s phase %s",
			ErrAlreadyResolved, workerID, commandID, phaseID)
	}
	state.CommitFailedWorkers = filtered

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	switch state.Integration.Status {
	case model.IntegrationStatusConflict,
		model.IntegrationStatusPartialMerge,
		model.IntegrationStatusFailed:
		if state.Integration.Status != model.IntegrationStatusFailed {
			if err := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); err != nil {
				return err
			}
		} else {
			state.Integration.UpdatedAt = now
		}
		state.Integration.MergeFailureCount = 0
	}
	state.UpdatedAt = now

	wm.Log(core.LogLevelInfo, "resolve_conflict command=%s phase=%s worker=%s", commandID, phaseID, workerID)
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	// H3: clear any lingering merge_conflict signal so a stale ResolutionState
	// from a split-brain dispatch (saveState succeeded but the worker state
	// revert failed) cannot block re-merge after the operator recovers.
	if wm.signalStore != nil {
		if serr := wm.signalStore.UpdateMergeConflictSignal(commandID, phaseID, workerID, func(sig *model.PlannerSignal) error {
			if sig == nil {
				return nil
			}
			sig.ResolutionState = ""
			sig.LastResolutionError = ""
			sig.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
			return nil
		}); serr != nil {
			wm.Log(core.LogLevelWarn, "resolve_conflict_signal_clear_failed command=%s worker=%s error=%v",
				commandID, workerID, serr)
		}
	}
	return nil
}
