package worktree

// AutoRecover dispatch + state-driven action selection. This file owns the
// idempotent, scan/event-driven recovery layer that decides whether to call
// ResumeMerge or RetryPublish based on the current integration status, plus
// the small pure helpers (selectAutoRecoverAction / publishBackoffElapsed)
// that make the dispatch decision unit-testable without touching git.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// AutoRecoverAction names the recovery path AutoRecover selected, or
// AutoRecoverNone when no recovery applied.
type AutoRecoverAction string

const (
	// AutoRecoverNone indicates that the integration is not in a state that
	// any idempotent recovery path handles automatically (e.g. already merged,
	// published, quarantined, or in an in-flight transition).
	AutoRecoverNone AutoRecoverAction = ""
	// AutoRecoverResumeMerge indicates that AutoRecover dispatched to
	// ResumeMerge AND the merge actually progressed — at least one resolving
	// worker was promoted out of WorktreeStatusResolving (to Integrated, or
	// Active in the legacy fallback).
	AutoRecoverResumeMerge AutoRecoverAction = "resume_merge"
	// AutoRecoverResumeMergeDeferred indicates that AutoRecover dispatched
	// ResumeMerge but the inner tryMergeWorker took the
	// resume_merge_deferred_resolution_in_flight path: the reporter worker
	// is still in Resolving with no committed edits yet, so the merge will
	// be re-attempted from a future result_write once the dispatched
	// __conflict_resolution task lands its commit. This distinct outcome
	// lets the caller log "deferred" rather than "completed", which would
	// be misread as a successful publish.
	AutoRecoverResumeMergeDeferred AutoRecoverAction = "resume_merge_deferred"
	// AutoRecoverRetryPublish indicates that AutoRecover dispatched to
	// RetryPublish for a publish_failed state whose NextPublishRetryAt
	// backoff has elapsed.
	AutoRecoverRetryPublish AutoRecoverAction = "retry_publish"
	// AutoRecoverDirtyQuarantine indicates that AutoRecover unquarantined a
	// transiently-poisoned integration worktree (typical cause:
	// RunOnIntegration verify scattering build artefacts that the merge
	// guard then refused to absorb). Recovery cleans the worktree and
	// drops the integration back to IntegrationStatusFailed so subsequent
	// scans can re-attempt the merge. Reserved for QuarantineSource=Merge
	// quarantines whose reason class is auto-recoverable; never applied
	// to publish-side quarantines or operator-managed states.
	AutoRecoverDirtyQuarantine AutoRecoverAction = "dirty_quarantine_recovered"
)

// AutoRecover inspects the worktree integration status for commandID and
// dispatches to the appropriate idempotent recovery method. It is safe to call
// on any commandID and on any schedule — states that don't match a recovery
// path return (AutoRecoverNone, nil), and dispatched calls that race with a
// manual recovery are absorbed via the underlying ErrAlreadyResolved sentinel.
//
// Policy:
//   - conflict, partial_merge                      → ResumeMerge
//   - failed with conflict/resolving workers OR a non-zero MergeFailureCount
//     → ResumeMerge
//   - publish_failed with elapsed NextPublishRetryAt → RetryPublish
//   - quarantined                                 → skipped (operator must
//     unquarantine explicitly; AutoRecover never escalates a terminal state)
//   - any other status                            → AutoRecoverNone
//
// The caller (plan handler, reconcile notifier, scan loop) supplies the wall
// clock for backoff evaluation. Returns the action actually attempted plus any
// error from the dispatched recovery. ErrAlreadyResolved is swallowed and
// reported as AutoRecoverNone with a nil error, since by definition there is
// nothing left to recover.
func (wm *Manager) AutoRecover(ctx context.Context, commandID string) (AutoRecoverAction, error) {
	if err := validateIDs(commandID); err != nil {
		return AutoRecoverNone, err
	}

	state, err := wm.GetCommandState(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AutoRecoverNone, ErrNoWorktreeState
		}
		return AutoRecoverNone, fmt.Errorf("load state: %w", err)
	}

	action := wm.selectAutoRecoverAction(state)
	switch action {
	case AutoRecoverResumeMerge:
		if err := wm.ResumeMerge(ctx, commandID); err != nil {
			if errors.Is(err, ErrAlreadyResolved) {
				return AutoRecoverNone, nil
			}
			return AutoRecoverResumeMerge, err
		}
		return AutoRecoverResumeMerge, nil
	case AutoRecoverRetryPublish:
		if err := wm.RetryPublish(commandID); err != nil {
			if errors.Is(err, ErrAlreadyResolved) {
				return AutoRecoverNone, nil
			}
			return AutoRecoverRetryPublish, err
		}
		return AutoRecoverRetryPublish, nil
	case AutoRecoverDirtyQuarantine:
		if err := wm.RecoverDirtyWorktreeQuarantine(commandID); err != nil {
			if errors.Is(err, ErrAlreadyResolved) {
				return AutoRecoverNone, nil
			}
			return AutoRecoverDirtyQuarantine, err
		}
		return AutoRecoverDirtyQuarantine, nil
	default:
		return AutoRecoverNone, nil
	}
}

// AutoRecoverAfterResolution is the event-driven sibling of AutoRecover,
// invoked when a worker has just reported successful completion of a
// conflict-resolution task. Compared to AutoRecover this entry point:
//
//  1. is scoped by reporterWorkerID — ResumeMerge fires only when *that*
//     worker is currently in WorktreeStatusResolving, so an unrelated
//     completion cannot advance an in-flight resolver against the wrong
//     worker's branch
//  2. bypasses the publish backoff for IntegrationStatusPublishFailed —
//     the worker's completion is itself a fresh "the previous blocker is
//     addressed" signal, while NextPublishRetryAt exists only to throttle
//     scan-driven retries
//  3. requires taskRunOnIntegration==true to fire the publish path —
//     only publish_conflict resolution tasks are dispatched on the
//     integration worktree (see internal/daemon/dispatch/dispatcher.go), so
//     other completions cannot accidentally trigger RetryPublish
//
// This closes the conflict / publish_conflict recovery loop without depending
// on the Planner agent to call resume-merge or retry-publish explicitly. It
// is intentionally narrow: callers MUST only invoke it when the task's
// terminal status is `completed` (after VerifyRunner has run, when verify is
// required) and MUST NOT invoke it on duplicate result_write submissions.
//
// Returns AutoRecoverNone with a nil error when no recovery applies.
// Idempotent: ErrAlreadyResolved is swallowed.
func (wm *Manager) AutoRecoverAfterResolution(
	ctx context.Context,
	commandID, reporterWorkerID string,
	taskRunOnIntegration bool,
) (AutoRecoverAction, error) {
	if err := validateIDs(commandID); err != nil {
		return AutoRecoverNone, err
	}
	if err := validate.ID(reporterWorkerID); err != nil {
		return AutoRecoverNone, fmt.Errorf("invalid reporterWorkerID: %w", err)
	}

	state, err := wm.GetCommandState(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AutoRecoverNone, ErrNoWorktreeState
		}
		return AutoRecoverNone, fmt.Errorf("load state: %w", err)
	}

	// Publish-side recovery has priority over merge-side. See helper godoc.
	if action, handled, err := wm.tryPublishConflictResolutionRecovery(state, commandID, taskRunOnIntegration); handled {
		return action, err
	}
	return wm.tryMergeConflictResolutionRecovery(ctx, state, commandID, reporterWorkerID)
}

// tryPublishConflictResolutionRecovery dispatches the publish-side path of
// AutoRecoverAfterResolution. Returns handled=true when the input matches the
// publish_conflict completion triple and the caller MUST stop here. Returns
// handled=false when the merge-side path should be tried next.
//
// The publish_conflict completion triple — (taskRunOnIntegration=true,
// integration status=publish_failed, non-empty PublishConflictFiles) — is
// what distinguishes a publish_conflict resolution from any other
// completion reported on the integration worktree. Worker status is NOT
// flipped to resolving for publish_conflict (only merge_conflict R7 does
// that), so the integration-worktree dispatch flag is the key signal.
func (wm *Manager) tryPublishConflictResolutionRecovery(
	state *model.WorktreeCommandState,
	commandID string,
	taskRunOnIntegration bool,
) (action AutoRecoverAction, handled bool, err error) {
	if !taskRunOnIntegration ||
		state.Integration.Status != model.IntegrationStatusPublishFailed ||
		len(state.Integration.PublishConflictFiles) == 0 {
		return AutoRecoverNone, false, nil
	}
	// Bypass NextPublishRetryAt: a worker completion is an explicit
	// event-driven trigger; the backoff exists to throttle scan-driven
	// retries, not to delay event-driven resolution.
	if err := wm.RetryPublish(commandID); err != nil {
		if errors.Is(err, ErrAlreadyResolved) {
			return AutoRecoverNone, true, nil
		}
		return AutoRecoverRetryPublish, true, err
	}
	return AutoRecoverRetryPublish, true, nil
}

// tryMergeConflictResolutionRecovery dispatches the merge-side path of
// AutoRecoverAfterResolution. The reporter worker MUST currently be in
// WorktreeStatusResolving (R7 contract: dispatched merge_conflict
// resolution tasks flip worker status conflict → resolving before sending).
// Returns AutoRecoverNone with nil error for any non-recoverable shape.
func (wm *Manager) tryMergeConflictResolutionRecovery(
	ctx context.Context,
	state *model.WorktreeCommandState,
	commandID, reporterWorkerID string,
) (AutoRecoverAction, error) {
	if !workerIsResolving(state, reporterWorkerID) {
		return AutoRecoverNone, nil
	}
	switch state.Integration.Status {
	case model.IntegrationStatusConflict,
		model.IntegrationStatusPartialMerge,
		model.IntegrationStatusFailed:
		// recoverable: ResumeMerge will commit the worker's edits via
		// commitResolvedWorkerChanges and re-attempt the integration merge.
	default:
		// merged / publishing / published / quarantined / created / merging:
		// no merge recovery applies, even if the reporter is resolving.
		return AutoRecoverNone, nil
	}
	if err := wm.ResumeMerge(ctx, commandID); err != nil {
		if errors.Is(err, ErrAlreadyResolved) {
			return AutoRecoverNone, nil
		}
		return AutoRecoverResumeMerge, err
	}
	// Post-state probe: ResumeMerge's tryMergeWorker may have hit the
	// "deferred" branch (worker arrived in Resolving with zero committed
	// edits because the dispatched __conflict_resolution task has not
	// landed yet). In that branch tryMergeWorker returns without flipping
	// the worker out of Resolving. Re-reading the state lets us return a
	// distinct AutoRecoverResumeMergeDeferred outcome so result_write_handler
	// logs "deferred", not "completed". GetCommandState reacquires wm.mu
	// after ResumeMerge releases it; failures here fall through to the
	// optimistic AutoRecoverResumeMerge so we never lose the upstream
	// "we did dispatch a recovery" signal due to a transient state-load error.
	if post, perr := wm.GetCommandState(commandID); perr == nil && workerIsResolving(post, reporterWorkerID) {
		return AutoRecoverResumeMergeDeferred, nil
	}
	return AutoRecoverResumeMerge, nil
}

// workerIsResolving reports whether the named worker is currently in
// WorktreeStatusResolving inside state.
func workerIsResolving(state *model.WorktreeCommandState, workerID string) bool {
	for i := range state.Workers {
		ws := &state.Workers[i]
		if ws.WorkerID == workerID && ws.Status == model.WorktreeStatusResolving {
			return true
		}
	}
	return false
}

// selectAutoRecoverAction returns the recovery path that applies to the given
// state, or AutoRecoverNone if no path applies. Pure function: no mutation, no
// I/O; used directly by AutoRecover and independently unit-testable.
func (wm *Manager) selectAutoRecoverAction(state *model.WorktreeCommandState) AutoRecoverAction {
	if state == nil {
		return AutoRecoverNone
	}
	switch state.Integration.Status {
	case model.IntegrationStatusConflict, model.IntegrationStatusPartialMerge:
		return AutoRecoverResumeMerge
	case model.IntegrationStatusFailed:
		// Only dispatch if ResumeMerge would find work to do — otherwise we'd
		// just get ErrAlreadyResolved. Mirrors the gating logic inside
		// ResumeMerge so AutoRecover can report AutoRecoverNone cleanly.
		if state.Integration.MergeFailureCount > 0 {
			return AutoRecoverResumeMerge
		}
		for _, ws := range state.Workers {
			if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
				return AutoRecoverResumeMerge
			}
		}
		return AutoRecoverNone
	case model.IntegrationStatusPublishFailed:
		if !wm.publishBackoffElapsed(state.Integration.NextPublishRetryAt) {
			return AutoRecoverNone
		}
		return AutoRecoverRetryPublish
	case model.IntegrationStatusQuarantined:
		// Auto-recover narrowly-defined transient quarantines. The original
		// design pinned every Quarantined as "operator must Unquarantine
		// explicitly", but the dirty_worktree class is purely transient —
		// caused by RunOnIntegration verify scattering build artefacts in
		// the integration worktree, never by user-relevant content. Letting
		// the orchestrator clean and retry that case unblocks
		// final_verification phases that would otherwise stall forever.
		// Other quarantine reasons (publish-side failures, abort recovery
		// failures, etc.) still require an operator.
		if isAutoRecoverableQuarantine(state) {
			return AutoRecoverDirtyQuarantine
		}
		return AutoRecoverNone
	default:
		// merged, publishing, published, created, merging → never
		// auto-recover here.
		return AutoRecoverNone
	}
}

// autoRecoverableQuarantineReasonPrefixes lists the QuarantineReason text
// prefixes that the orchestrator considers safely auto-recoverable. Each
// prefix names a transient pollution path that resetting + cleaning the
// integration worktree fully addresses. Anything not in this list (e.g.
// abort_recover_failed, publish-side reasons) is left for an operator.
var autoRecoverableQuarantineReasonPrefixes = []string{
	"dirty_worktree",
	"status_check_failed",
	"pre_merge_reset_failed",
	"dirty_worktree_after_clean",
}

// isAutoRecoverableQuarantine reports whether the integration is in a
// quarantine class that the orchestrator can safely resolve on its own
// (clean integration worktree → drop status to Failed → next merge
// reattempts). Restricted to QuarantineSource=Merge with a known transient
// reason; publish-side and operator-managed quarantines are excluded.
func isAutoRecoverableQuarantine(state *model.WorktreeCommandState) bool {
	if state == nil || state.Integration.Status != model.IntegrationStatusQuarantined {
		return false
	}
	if state.Integration.QuarantineSource != model.QuarantineSourceMerge {
		return false
	}
	reason := state.Integration.QuarantineReason
	for _, prefix := range autoRecoverableQuarantineReasonPrefixes {
		if strings.HasPrefix(reason, prefix) {
			return true
		}
	}
	return false
}

// RecoverDirtyWorktreeQuarantine clears a transient-pollution quarantine on
// the integration branch. Recovery sequence:
//
//  1. `git reset --hard HEAD` + `git clean -fd` on the integration worktree
//     so any RunOnIntegration verify build artefacts are removed.
//  2. Verify the worktree is clean — if not, abort recovery (an operator
//     must intervene because the artefact is permission-locked).
//  3. Drop integration status from Quarantined back to Failed, clear
//     QuarantinedAt/Reason/Source, reset MergeFailureCount so the
//     standard merge retry loop can proceed.
//
// Skips and returns ErrAlreadyResolved when the state is no longer in
// auto-recoverable quarantine (race with operator unquarantine, etc.).
//
// Bypasses ValidateIntegrationTransition deliberately: the static state
// machine treats Quarantined as terminal because the original design
// required operator action. Auto-recovering the narrow dirty_worktree
// class is the orchestrator owning that decision for transient pollution.
func (wm *Manager) RecoverDirtyWorktreeQuarantine(commandID string) error {
	if err := validateIDs(commandID); err != nil {
		return err
	}
	// Reserve the integration worktree: an in-flight A/B selection releases
	// wm.mu during its external verify runs, so wm.mu alone no longer
	// excludes integration mutations.
	il := wm.integrationLock(commandID)
	il.Lock()
	defer il.Unlock()

	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if !isAutoRecoverableQuarantine(state) {
		return ErrAlreadyResolved
	}

	integrationPath := wm.integrationWorktreePath(commandID)
	if err := ensureWithinProjectRoot(wm.projectRoot, integrationPath); err != nil {
		return fmt.Errorf("recover quarantine refused: %w", err)
	}

	if err := wm.gitRunInDir(integrationPath, "reset", "--hard", "HEAD"); err != nil {
		return fmt.Errorf("recovery reset --hard HEAD: %w", err)
	}
	if err := wm.gitRunInDir(integrationPath, "clean", "-fd"); err != nil {
		// Non-fatal at this layer: subsequent status check will surface a
		// real persistent dirty state.
		wm.Log(core.LogLevelWarn,
			"quarantine_recovery_clean_warning command=%s error=%v", commandID, err)
	}
	statusOut, statusErr := wm.gitOutputInDir(integrationPath, "status", "--porcelain")
	if statusErr != nil {
		return fmt.Errorf("recovery post-clean status check: %w", statusErr)
	}
	if strings.TrimSpace(statusOut) != "" {
		return fmt.Errorf("integration worktree still dirty after recovery clean: %s",
			strings.TrimSpace(statusOut))
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	previousReason := state.Integration.QuarantineReason
	state.Integration.Status = model.IntegrationStatusFailed
	state.Integration.UpdatedAt = now
	state.Integration.MergeFailureCount = 0
	state.Integration.QuarantinedAt = ""
	state.Integration.QuarantineReason = ""
	state.Integration.QuarantineSource = ""
	state.Integration.StallSignaled = false
	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state after quarantine recovery: %w", err)
	}
	wm.Log(core.LogLevelWarn,
		"integration_quarantine_auto_recovered command=%s prior_reason=%q "+
			"(orchestrator-side transient pollution cleared; status Quarantined→Failed for retry)",
		commandID, previousReason)
	return nil
}

// publishBackoffElapsed returns true when nextRetryAt is empty (no backoff set)
// or parses to a time at or before now. Unparseable timestamps are treated as
// "elapsed" so a corrupted field cannot block recovery indefinitely.
func (wm *Manager) publishBackoffElapsed(nextRetryAt string) bool {
	if nextRetryAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, nextRetryAt)
	if err != nil {
		return true
	}
	return !wm.clock.Now().Before(t)
}
