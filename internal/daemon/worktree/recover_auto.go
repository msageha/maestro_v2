package worktree

// AutoRecover dispatch + state-driven action selection extracted from
// recover.go (F-041 step 4 physical file split). This file owns the
// idempotent, scan/event-driven recovery layer that decides whether to call
// ResumeMerge or RetryPublish based on the current integration status, plus
// the small pure helpers (selectAutoRecoverAction / publishBackoffElapsed)
// that make the dispatch decision unit-testable without touching git.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

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
	// __conflict_resolution task lands its commit. Operators reading the
	// 2026-04-28 E2E logs misread "completed action=resume_merge" as
	// "publish has run" — this distinct outcome is what lets the caller
	// log "deferred" instead.
	AutoRecoverResumeMergeDeferred AutoRecoverAction = "resume_merge_deferred"
	// AutoRecoverRetryPublish indicates that AutoRecover dispatched to
	// RetryPublish for a publish_failed state whose NextPublishRetryAt
	// backoff has elapsed.
	AutoRecoverRetryPublish AutoRecoverAction = "retry_publish"
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
// handled=false when the merge-side path should be tried next. F-041 step 3
// helper.
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
// Returns AutoRecoverNone with nil error for any non-recoverable shape,
// matching the original semantics. F-041 step 3 helper.
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
// WorktreeStatusResolving inside state. F-041 step 3 helper.
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
	default:
		// merged, publishing, published, quarantined, created, merging →
		// never auto-recover. Quarantined in particular requires an explicit
		// operator Unquarantine call.
		return AutoRecoverNone
	}
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
