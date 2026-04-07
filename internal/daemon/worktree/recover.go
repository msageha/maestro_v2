package worktree

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
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
	state.Integration.StallSignaled = false
	state.UpdatedAt = now

	wm.log(core.LogLevelInfo, "unquarantine command=%s reason=%q", commandID, reason)
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// ResumeMerge resets the merge failure counter for an integration that is
// stuck in Conflict / PartialMerge / Failed and moves it to
// IntegrationStatusFailed so the next Phase A scan can re-enqueue the merge.
// It is the milder sibling of Unquarantine and does not apply to integrations
// that are already healthy or quarantined.
//
// Idempotency: a call when the integration is already Failed with
// MergeFailureCount==0 returns ErrAlreadyResolved without modifying the file.
func (wm *Manager) ResumeMerge(commandID string) error {
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
	case model.IntegrationStatusConflict,
		model.IntegrationStatusPartialMerge,
		model.IntegrationStatusFailed:
		// recoverable
	case model.IntegrationStatusQuarantined:
		return fmt.Errorf("%w: integration is quarantined; use unquarantine", ErrAlreadyResolved)
	default:
		return fmt.Errorf("%w: status=%s", ErrAlreadyResolved, s)
	}

	if s == model.IntegrationStatusFailed && state.Integration.MergeFailureCount == 0 {
		return fmt.Errorf("%w: status=failed with no pending failures", ErrAlreadyResolved)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	if s != model.IntegrationStatusFailed {
		if err := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); err != nil {
			return err
		}
	} else {
		state.Integration.UpdatedAt = now
	}
	state.Integration.MergeFailureCount = 0
	state.UpdatedAt = now

	wm.log(core.LogLevelInfo, "resume_merge command=%s prev_status=%s", commandID, s)
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// ResolveConflict marks a per-phase, per-worker merge conflict as resolved
// after an operator has manually fixed up the integration branch. It removes
// the worker from CommitFailedWorkers (the gating list that blocks
// publish-to-base) and resets the merge failure counter so that the next Phase
// A scan can re-enqueue the merge attempt for the named phase.
//
// Idempotency: returns ErrAlreadyResolved when the worker is not in the
// commit-failed list and the integration is not in a recoverable state.
func (wm *Manager) ResolveConflict(commandID, phaseID, workerID string) error {
	if err := validateIDs(commandID, phaseID, workerID); err != nil {
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

	wm.log(core.LogLevelInfo, "resolve_conflict command=%s phase=%s worker=%s", commandID, phaseID, workerID)
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}
