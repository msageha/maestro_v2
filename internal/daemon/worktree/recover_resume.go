package worktree

// ResumeMerge pipeline + worker-resolved merge primitives. This file owns
// the "operator/agent triggered ResumeMerge" code path and its helpers
// (attemptResolvedMerges -> tryMergeWorker -> mergeResolvedWorker /
// abortAndReturnMergeError / checkoutResolvedFilesFromBranch) so the
// integration-branch reflow logic stays close together.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// ResumeMerge attempts to merge conflict-resolved workers directly into the
// integration branch using -X theirs to prefer the worker's committed
// resolution for conflicting hunks. This handles add/add and other structural
// conflicts that would reproduce the same conflict on normal re-merge.
//
// MergeFailureCount is reset only when at least one worker is successfully
// integrated. If all merge attempts fail, the count is incremented toward the
// quarantine threshold to prevent unbounded retry.
//
// If the integration worktree is unavailable or dirty, ResumeMerge falls back
// to the legacy behavior of resetting workers to active so MergeToIntegration
// can re-attempt the merge (this fallback exists for backward compatibility
// with tests and edge cases where the worktree does not exist).
//
// Idempotency: a call when the integration is already Failed with
// MergeFailureCount==0 and no conflict/resolving workers returns
// ErrAlreadyResolved without modifying the file.
func (wm *Manager) ResumeMerge(ctx context.Context, commandID string) error {
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
	case model.IntegrationStatusPublishFailed:
		return fmt.Errorf("%w: integration is in publish_failed state; publish retry will handle recovery automatically", ErrAlreadyResolved)
	case model.IntegrationStatusQuarantined:
		return fmt.Errorf("%w: integration is quarantined; use unquarantine", ErrAlreadyResolved)
	default:
		return fmt.Errorf("%w: status=%s", ErrAlreadyResolved, s)
	}

	// Check if there's actually something to resume: either pending failures
	// or workers stuck in conflict/resolving.
	hasConflictWorkers := false
	for _, ws := range state.Workers {
		if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
			hasConflictWorkers = true
			break
		}
	}

	if s == model.IntegrationStatusFailed && state.Integration.MergeFailureCount == 0 && !hasConflictWorkers {
		return fmt.Errorf("%w: status=failed with no pending failures and no conflict workers", ErrAlreadyResolved)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)

	// Collect conflict/resolving workers for merge attempt.
	var toResolve []*model.WorktreeState
	for i := range state.Workers {
		ws := &state.Workers[i]
		if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
			toResolve = append(toResolve, ws)
		}
	}

	// Attempt merge with resolution for conflict/resolving workers.
	if len(toResolve) > 0 {
		wm.attemptResolvedMerges(ctx, state, commandID, toResolve, now)
	}

	// Check merge outcomes to decide MergeFailureCount handling.
	// Distinguish three cases:
	//   - legacyFallback: worktree unavailable → workers reset to active; reset count
	//   - anyIntegrated: at least one merge succeeded; reset count
	//   - all merges failed (workers reverted to conflict); increment toward quarantine
	legacyFallback := false
	anyResolved := false
	for _, ws := range toResolve {
		switch ws.Status {
		case model.WorktreeStatusActive:
			legacyFallback = true
		case model.WorktreeStatusIntegrated:
			anyResolved = true
		}
	}
	var transitionErr error
	if anyResolved || len(toResolve) == 0 || legacyFallback {
		state.Integration.MergeFailureCount = 0
	} else {
		// All per-worker merge attempts failed — count toward quarantine.
		if err := wm.recordMergeFailure(state, "resume_merge_all_failed", now); err != nil {
			wm.Log(core.LogLevelWarn, "resume_merge_failure_record command=%s error=%v", commandID, err)
			transitionErr = errors.Join(transitionErr, fmt.Errorf("record merge failure: %w", err))
		}
	}

	if err := wm.finalizeResumeMergeIntegrationStatus(ctx, state, commandID, len(toResolve), now); err != nil {
		wm.Log(core.LogLevelWarn, "resume_merge_finalize_status command=%s error=%v", commandID, err)
		transitionErr = errors.Join(transitionErr, err)
	}

	// If not merged (and not quarantined by recordMergeFailure), ensure we
	// end in failed state so Phase A can re-enqueue merge attempts.
	if state.Integration.Status != model.IntegrationStatusMerged &&
		state.Integration.Status != model.IntegrationStatusQuarantined {
		if state.Integration.Status != model.IntegrationStatusFailed {
			if err := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); err != nil {
				wm.Log(core.LogLevelWarn, "resume_merge_failed_transition command=%s error=%v", commandID, err)
				transitionErr = errors.Join(transitionErr, fmt.Errorf("transition integration to failed: %w", err))
			}
		} else {
			state.Integration.UpdatedAt = now
		}
	}

	state.UpdatedAt = now

	wm.Log(core.LogLevelInfo, "resume_merge command=%s prev_status=%s resolved_workers=%d",
		commandID, s, len(toResolve))
	if err := wm.saveState(commandID, state); err != nil {
		return errors.Join(transitionErr, fmt.Errorf("save state: %w", err))
	}
	return transitionErr
}

// finalizeResumeMergeIntegrationStatus drives the post-merge integration
// status transition after ResumeMerge has merged the resolved workers:
//
//  1. Skip if any non-terminal worker remains (allIntegrated=false) or no
//     workers were resolved this round.
//  2. Verify the merged workers' branches are actually reachable from
//     integration HEAD (verifyWorkersMerged). On mismatch, log error,
//     revert the offending workers to Conflict, and bump the merge-failure
//     counter toward quarantine.
//  3. On verification success, transition Failed → Merging → Merged.
//     If the Merged transition itself fails, revert to Failed so Phase A
//     can retry.
//
// Regression coverage: TestResumeMerge_ContentMismatchDoesNotPromoteToMerged
// in merge_conflict_test.go.
func (wm *Manager) finalizeResumeMergeIntegrationStatus(
	ctx context.Context,
	state *model.WorktreeCommandState,
	commandID string,
	resolveCount int,
	now string,
) error {
	if !allWorkersMergeTerminal(state) || resolveCount == 0 {
		return nil
	}

	contentOK, badWorkers := wm.verifyWorkersMerged(ctx, commandID, state)
	if !contentOK {
		wm.Log(core.LogLevelError,
			"resume_merge_content_mismatch command=%s bad_workers=%v (state says integrated but branch not reachable from integration HEAD; forcing status=failed)",
			commandID, badWorkers)
		wm.revertContentMismatchedWorkers(state, badWorkers, commandID, now)
		if tErr := wm.recordMergeFailure(state, "content_mismatch", now); tErr != nil {
			wm.Log(core.LogLevelWarn, "resume_merge_content_mismatch_record command=%s error=%v", commandID, tErr)
			return fmt.Errorf("record content mismatch merge failure: %w", tErr)
		}
		return nil
	}

	if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusMerging, now); tErr != nil {
		wm.Log(core.LogLevelWarn, "resume_merge_merging_transition command=%s error=%v", commandID, tErr)
		return fmt.Errorf("transition integration to merging: %w", tErr)
	}
	if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusMerged, now); tErr != nil {
		wm.Log(core.LogLevelWarn, "resume_merge_merged_transition command=%s error=%v", commandID, tErr)
		// Revert to Failed so Phase A can retry.
		if revertErr := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); revertErr != nil {
			wm.Log(core.LogLevelError, "resume_merge_failed_revert_transition command=%s error=%v", commandID, revertErr)
			return errors.Join(fmt.Errorf("transition integration to merged: %w", tErr), fmt.Errorf("revert integration to failed: %w", revertErr))
		}
		return fmt.Errorf("transition integration to merged: %w", tErr)
	}
	return nil
}

// allWorkersMergeTerminal reports whether every worker is in a status that
// counts as "done for merge purposes" — integrated, published, or one of the
// cleanup terminals.
func allWorkersMergeTerminal(state *model.WorktreeCommandState) bool {
	for _, ws := range state.Workers {
		switch ws.Status {
		case model.WorktreeStatusIntegrated, model.WorktreeStatusPublished,
			model.WorktreeStatusCleanupDone, model.WorktreeStatusCleanupFailed:
			// done for merge purposes
		default:
			return false
		}
	}
	return true
}

// revertContentMismatchedWorkers transitions each badWorker back to
// Conflict so the resolution pipeline re-attempts on the next scan instead
// of leaving them wedged in a false "integrated" state.
func (wm *Manager) revertContentMismatchedWorkers(
	state *model.WorktreeCommandState,
	badWorkers []string,
	commandID, now string,
) {
	for _, wid := range badWorkers {
		for i := range state.Workers {
			if state.Workers[i].WorkerID != wid {
				continue
			}
			if tErr := wm.setWorkerStatus(&state.Workers[i], model.WorktreeStatusConflict, now); tErr != nil {
				wm.Log(core.LogLevelWarn, "resume_merge_revert_bad_worker command=%s worker=%s error=%v",
					commandID, wid, tErr)
			}
		}
	}
}

// verifyWorkersMerged checks whether every worker whose state says "integrated"
// is actually reachable from the current integration HEAD. This is a safety
// net against paths where tryMergeWorker marked a worker Integrated without
// its content actually reaching the branch — e.g. commitResolvedWorkerChanges
// silently failed, or mergeResolvedWorker early-returned because the worker
// branch had no new commits to merge even though resolution edits existed.
//
// Returns (true, nil) when every integrated-state worker branch is an
// ancestor of HEAD. Returns (false, badWorkerIDs) otherwise. A transient git
// error is conservatively treated as a content mismatch so we never promote
// integration to Merged on unverified data.
// Caller must hold wm.mu.
func (wm *Manager) verifyWorkersMerged(ctx context.Context, commandID string, state *model.WorktreeCommandState) (bool, []string) {
	integrationPath := wm.integrationWorktreePath(commandID)
	headOut, err := wm.gitOutputWithRetry(ctx, integrationPath, 2, "rev-parse", "HEAD")
	if err != nil {
		wm.Log(core.LogLevelWarn, "verify_workers_merged_head_failed command=%s error=%v", commandID, err)
		return false, nil
	}
	head := strings.TrimSpace(headOut)
	if err := validateSHA(head); err != nil {
		wm.Log(core.LogLevelWarn, "verify_workers_merged_invalid_head command=%s error=%v", commandID, err)
		return false, nil
	}

	bad := make([]string, 0, len(state.Workers))
	for _, ws := range state.Workers {
		switch ws.Status {
		case model.WorktreeStatusIntegrated, model.WorktreeStatusPublished,
			model.WorktreeStatusCleanupDone, model.WorktreeStatusCleanupFailed:
			// Expected to be reachable from HEAD.
		default:
			continue
		}
		if ws.Branch == "" {
			continue
		}
		// git merge-base --is-ancestor <branch> HEAD: exit 0 if ancestor, 1 if not, >1 on error.
		err := wm.gitRunInDir(integrationPath, "merge-base", "--is-ancestor", ws.Branch, head)
		if err == nil {
			continue
		}
		// Distinguish "not an ancestor" (exit 1) from a real git error (bad
		// ref, I/O, etc.): a not-an-ancestor result is what we flag; other
		// errors are logged but still treated as "bad" to fail closed.
		wm.Log(core.LogLevelWarn,
			"verify_worker_merged command=%s worker=%s branch=%s error=%v (flagging as not-merged)",
			commandID, ws.WorkerID, ws.Branch, err)
		bad = append(bad, ws.WorkerID)
	}
	return len(bad) == 0, bad
}

// attemptResolvedMerges tries to merge each conflict/resolving worker directly
// into the integration branch using -X theirs so git prefers the worker's
// committed resolution for conflicting hunks.
//
// If the integration worktree is unavailable, workers are reset to active
// (legacy fallback for backward compatibility with tests and edge cases).
// If the integration worktree is available but a per-worker merge fails,
// the worker is reverted to conflict (not active) to prevent infinite loops
// where MergeToIntegration would re-merge and reproduce the same conflict.
// Caller must hold wm.mu.
func (wm *Manager) attemptResolvedMerges(
	ctx context.Context,
	_ *model.WorktreeCommandState,
	commandID string,
	workers []*model.WorktreeState,
	now string,
) {
	integrationPath := wm.integrationWorktreePath(commandID)

	// Check integration worktree is accessible and clean.
	dirtyOut, err := wm.gitOutputInDir(integrationPath, "status", "--porcelain")
	if err != nil {
		wm.Log(core.LogLevelWarn, "resume_merge_worktree_check command=%s error=%v (falling back to active reset)",
			commandID, err)
		wm.resetWorkersToActive(workers, now, commandID)
		return
	}
	if strings.TrimSpace(dirtyOut) != "" {
		wm.Log(core.LogLevelWarn, "resume_merge_dirty_worktree command=%s (falling back to active reset)", commandID)
		wm.resetWorkersToActive(workers, now, commandID)
		return
	}

	// Path guard: refuse to run git ops outside the project root.
	if err := ensureWithinProjectRoot(wm.projectRoot, integrationPath); err != nil {
		wm.Log(core.LogLevelWarn, "resume_merge_path_guard command=%s error=%v (falling back to active reset)",
			commandID, err)
		wm.resetWorkersToActive(workers, now, commandID)
		return
	}

	for _, ws := range workers {
		wm.tryMergeWorker(ctx, integrationPath, ws, commandID, now)
	}
}

// tryMergeWorker commits any uncommitted resolution changes and attempts to
// merge a single conflict/resolving worker into the integration branch.
// On success the worker is transitioned to integrated; on failure the worker
// is reverted to conflict (not active) to prevent infinite re-merge loops.
// Caller must hold wm.mu.
func (wm *Manager) tryMergeWorker(ctx context.Context, integrationPath string, ws *model.WorktreeState, commandID, now string) {
	// Distinguish "arrived already in resolving" from "freshly bumped from
	// conflict". A worker that arrives in resolving was previously dispatched
	// a __conflict_resolution task by R7 (R7MergeConflict.Apply ↑). Until that
	// task lands worker edits in their worktree, merging them would silently
	// take the *pre-resolution* worker content via `mergeResolvedWorker`'s
	// `-X theirs` strategy, overwriting the planner-supplied resolution that
	// is still in flight. We need this flag below to gate that path; freshly
	// promoted workers (conflict → resolving inline) are a separate scenario
	// where there is no in-flight resolution task to wait on, so the existing
	// `-X theirs` behaviour stands.
	arrivedResolving := ws.Status == model.WorktreeStatusResolving

	// Ensure the worker is in "resolving" status before attempting the merge.
	// Workers may arrive here in "conflict" status (e.g. ResumeMerge collects
	// both conflict and resolving workers). The valid transition path is:
	//   conflict → resolving → integrated
	// Attempting conflict → integrated directly is invalid per the state machine.
	if ws.Status == model.WorktreeStatusConflict {
		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusResolving, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "resume_merge_resolving_transition command=%s worker=%s error=%v",
				commandID, ws.WorkerID, tErr)
			return
		}
	}

	// Race guard: when a worker arrives already in resolving and has produced
	// no uncommitted edits, R7's __conflict_resolution task has not yet
	// reported back. Any non-AutoRecoverAfterResolution caller (operator
	// `plan auto_recover`, scan-driven AutoRecover, manual `plan resume_merge`)
	// hitting this state would push commitResolvedWorkerChanges into a no-op
	// and let mergeResolvedWorker promote the worker via `-X theirs` against
	// the unchanged worker branch — silently merging the original
	// pre-resolution content and discarding the resolution before it lands.
	// Skip the merge here and leave the worker in resolving so the
	// AutoRecoverAfterResolution hook (fired by the worker's own completion
	// report) can drive the merge with real edits in the worktree. Workers
	// that were just bumped from conflict are intentionally not gated: there
	// is no R7-dispatched resolution to wait on for them, and `-X theirs`
	// correctly takes their original content.
	if arrivedResolving {
		statusOut, statusErr := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
		if statusErr != nil {
			// Worktree probe failed — conservatively bail rather than risk a
			// false-success merge. The next ResumeMerge call will retry once
			// the worker reports completion (which guarantees the worktree is
			// reachable for `git status`).
			wm.Log(core.LogLevelWarn,
				"resume_merge_resolving_status_check command=%s worker=%s error=%v (skipping; will retry)",
				commandID, ws.WorkerID, statusErr)
			return
		}
		if strings.TrimSpace(statusOut) == "" {
			// No uncommitted edits in the worktree. Two sub-cases:
			//
			//   (a) Resolution task not yet completed AND the Worker has
			//       not committed anything itself → defer until
			//       AutoRecoverAfterResolution fires from the worker's
			//       completion report.
			//
			//   (b) The Worker (typically codex / gemini, which run without
			//       the Claude-only policy hook and can issue `git commit`
			//       directly) self-committed the resolution. The worktree
			//       is clean *because* the edits are already on the worker
			//       branch HEAD — going through the deferred path would
			//       leave the worker pinned at resolving forever, since
			//       the worker has nothing more to commit and
			//       AutoRecoverAfterResolution's git-status probe would
			//       loop on the same empty result. Detect (b) by comparing
			//       the worker branch HEAD with ConflictBranchHead — the
			//       SHA we snapshotted at conflict detection. If HEAD has
			//       moved past that snapshot the merge proceeds; the branch
			//       carries the resolution and -X theirs picks it up as
			//       the canonical content.
			advanced, probeErr := wm.workerBranchAdvancedSinceConflict(ws)
			if probeErr != nil {
				wm.Log(core.LogLevelWarn,
					"resume_merge_resolving_head_probe command=%s worker=%s error=%v (deferring; will retry next scan)",
					commandID, ws.WorkerID, probeErr)
				return
			}
			if !advanced {
				// "deferred", not "skipped": the merge is intentionally
				// postponed until the dispatched __conflict_resolution task
				// reports completion. AutoRecoverAfterResolution (fired by
				// that very completion report) drives the eventual merge —
				// the matching `auto_recover_after_resolution_completed
				// action=resume_merge` log line is what actually moves the
				// worker to integrated.
				wm.Log(core.LogLevelInfo,
					"resume_merge_deferred_resolution_in_flight command=%s worker=%s "+
						"(no uncommitted edits and worker branch HEAD == ConflictBranchHead; merge will be re-attempted from result_write once the resolution task completes)",
					commandID, ws.WorkerID)
				return
			}
			wm.Log(core.LogLevelInfo,
				"resume_merge_resolving_self_committed command=%s worker=%s "+
					"(worktree clean but worker branch advanced past ConflictBranchHead; treating as completed self-commit and proceeding to merge)",
				commandID, ws.WorkerID)
		}
	}

	// Commit any uncommitted resolution changes in the worker's worktree
	// before attempting the merge. When a worker resolves a conflict, their
	// edits may remain uncommitted because the resolving→committed transition
	// is not in the valid state machine. This commit ensures the worker
	// branch HEAD reflects the resolved content so mergeResolvedWorker can
	// use it via `git checkout <branch> -- <file>`.
	//
	// If the worktree had dirty files but the commit ultimately failed (e.g.
	// every file was filtered as sensitive, or the commit command itself
	// erred), proceeding with the merge risks a false-success: the worker
	// branch still holds pre-resolution content and mergeResolvedWorker's
	// "already merged" early-return would flip the worker to Integrated
	// without propagating the real resolution. Keep the worker in Conflict so
	// the operator can inspect rather than silently promoting the branch.
	if commitErr := wm.commitResolvedWorkerChanges(ws, commandID); commitErr != nil {
		wm.Log(core.LogLevelWarn, "resume_merge_commit_resolved command=%s worker=%s error=%v (reverting to conflict, skipping merge attempt)",
			commandID, ws.WorkerID, commitErr)
		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "resume_merge_commit_resolved_revert command=%s worker=%s error=%v",
				commandID, ws.WorkerID, tErr)
		}
		return
	}

	if mergeErr := wm.mergeResolvedWorker(ctx, integrationPath, ws, commandID); mergeErr != nil {
		if errors.Is(mergeErr, errStaleResolution) {
			// Lost-update guard: the integration HEAD advanced past the
			// snapshot this worker's resolution was computed against
			// (typically because another conflicted worker's resolution
			// merged first). Merging with -X theirs now would silently
			// overwrite the newer integration content with this worker's
			// stale view. Reset the worker to active instead: the standard
			// merge pipeline re-merges the branch (which carries the
			// committed resolution), either cleanly (no overlap with the
			// newer content) or by re-detecting the conflict against the
			// CURRENT integration HEAD — emitting a fresh merge_conflict
			// signal (new ConflictGeneration, fresh ours ref) so the next
			// resolution round sees the up-to-date integration content.
			// Unlike the generic failure branch below, active does NOT loop:
			// the re-merge produces a different outcome because the
			// integration HEAD (and thus the conflict refs) changed.
			wm.Log(core.LogLevelWarn,
				"resume_merge_stale_resolution command=%s worker=%s conflict_integration_head=%s error=%v "+
					"(integration advanced since conflict snapshot; resetting worker to active for re-merge against current HEAD)",
				commandID, ws.WorkerID, ws.ConflictIntegrationHead, mergeErr)
			if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusActive, now); tErr != nil {
				wm.Log(core.LogLevelWarn, "resume_merge_stale_resolution_transition command=%s worker=%s error=%v",
					commandID, ws.WorkerID, tErr)
			}
			return
		}
		// Set the worker back to conflict (not active) to prevent an
		// infinite loop: MergeToIntegration skips conflict workers, so
		// the next scan will not re-merge this worker. Setting to active
		// would cause MergeToIntegration to re-merge → same conflict →
		// resolution pipeline → resume-merge → failure → active → loop.
		wm.Log(core.LogLevelWarn, "resume_merge_resolved_failed command=%s worker=%s error=%v (reverting to conflict)",
			commandID, ws.WorkerID, mergeErr)
		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "resume_merge_fallback_transition command=%s worker=%s error=%v",
				commandID, ws.WorkerID, tErr)
		}
		return
	}
	if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusIntegrated, now); tErr != nil {
		wm.Log(core.LogLevelWarn, "resume_merge_integrated_transition command=%s worker=%s error=%v",
			commandID, ws.WorkerID, tErr)
	}
	wm.Log(core.LogLevelInfo, "resume_merge_worker_integrated command=%s worker=%s", commandID, ws.WorkerID)
}

// workerBranchAdvancedSinceConflict reports whether the worker branch's HEAD
// has moved past ConflictBranchHead — the SHA snapshotted by MergeToIntegration
// at conflict detection time. Used by tryMergeWorker's deferred-merge guard
// to distinguish two clean-worktree scenarios on a worker that arrived at
// resolving status:
//
//   - HEAD == ConflictBranchHead: the dispatched __conflict_resolution task
//     has not produced any commit yet → defer the merge until result_write
//     fires AutoRecoverAfterResolution.
//   - HEAD past ConflictBranchHead: a non-Claude Worker (codex / gemini that
//     does not run the Claude policy hook) self-committed the resolution.
//     The worktree is clean *because* the edits are already on the branch,
//     so let the merge proceed.
//
// Returns advanced=false when ConflictBranchHead is empty so legacy / mid-
// rollout state files (written before this field existed, or in code paths
// that did not populate it — e.g. resumed sessions imported from a prior
// daemon version) fall back to the conservative deferred path.
// Caller must hold wm.mu.
func (wm *Manager) workerBranchAdvancedSinceConflict(ws *model.WorktreeState) (bool, error) {
	if ws.Path == "" {
		return false, fmt.Errorf("worker %s has no worktree path", ws.WorkerID)
	}
	if ws.ConflictBranchHead == "" {
		// Legacy state without the snapshot — preserve old deferred-only
		// semantics. False here means tryMergeWorker keeps the worker in
		// resolving and waits for AutoRecoverAfterResolution.
		return false, nil
	}
	headOut, err := wm.gitOutputInDir(ws.Path, "rev-parse", "HEAD")
	if err != nil {
		return false, fmt.Errorf("git rev-parse HEAD in %s: %w", ws.Path, err)
	}
	head := strings.TrimSpace(headOut)
	if head == "" {
		return false, fmt.Errorf("git rev-parse HEAD in %s: empty output", ws.Path)
	}
	return head != ws.ConflictBranchHead, nil
}

// commitResolvedWorkerChanges commits any uncommitted changes in a
// conflict/resolving worker's worktree to their branch. This is necessary
// because the resolving→committed status transition is not valid in the state
// machine, so the daemon's normal auto-commit path (CommitWorkerChanges)
// cannot commit resolution edits. Without this, the worker branch still holds
// the pre-resolution content and re-merging it reproduces the same conflict.
//
// The commit bypasses the worktree status machine intentionally — the status
// will be updated to integrated (on merge success) or conflict (on fallback)
// by the caller.
// Caller must hold wm.mu.
func (wm *Manager) commitResolvedWorkerChanges(ws *model.WorktreeState, commandID string) error {
	if ws.Path == "" {
		return fmt.Errorf("worker %s has no worktree path", ws.WorkerID)
	}
	statusOut, err := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status in %s: %w", ws.Path, err)
	}
	if strings.TrimSpace(statusOut) == "" {
		return nil // nothing to commit
	}

	// Capture every dirty change — modifications, untracked, deletions —
	// in a single pass. Sensitive-path filtering belongs to the worker's
	// environment (~/.claude rules, repo .gitignore), not this gate.
	// OS-level un-stat-able files trigger the bounded fallback below.
	if err := wm.gitAddAllWithUnstattableFallback(ws.Path, commandID, ws.WorkerID); err != nil {
		return fmt.Errorf("git add -A in %s: %w", ws.Path, err)
	}
	msg := fmt.Sprintf("[maestro] conflict resolution for %s", ws.WorkerID)
	if err := wm.gitRunInDir(ws.Path, "commit", "-m", msg); err != nil {
		return fmt.Errorf("git commit in %s: %w", ws.Path, err)
	}
	wm.Log(core.LogLevelInfo, "committed_resolved_changes command=%s worker=%s", commandID, ws.WorkerID)
	return nil
}

// resetWorkersToActive transitions all given workers to active state (legacy
// fallback when merge with resolution is not possible).
// Caller must hold wm.mu.
func (wm *Manager) resetWorkersToActive(workers []*model.WorktreeState, now, commandID string) {
	for _, ws := range workers {
		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusActive, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "resume_merge_worker_reset command=%s worker=%s from=%s error=%v",
				commandID, ws.WorkerID, ws.Status, tErr)
		}
	}
}

// errStaleResolution is returned by mergeResolvedWorker when the integration
// HEAD has advanced past the ConflictIntegrationHead snapshot the worker's
// resolution was computed against. tryMergeWorker translates it into an
// active reset so the standard merge pipeline re-detects the conflict against
// the current integration HEAD instead of letting -X theirs clobber content
// merged after the snapshot.
var errStaleResolution = errors.New("stale conflict resolution: integration HEAD advanced past conflict snapshot")

// mergeResolvedWorker attempts to merge a conflict-resolved worker's branch
// into the integration branch using -X theirs so that git automatically
// prefers the worker's committed resolution for any conflicting hunks. This
// handles add/add conflicts (where the merge base has no file and both sides
// added it independently) without requiring manual checkout of each file.
//
// If -X theirs alone does not resolve all conflicts (e.g., binary rename/rename
// edge cases), a secondary fallback checks out each conflicting file from the
// worker branch and commits the merge.
//
// Lost-update guard: -X theirs is only safe when the integration branch is
// still at the exact HEAD the resolution was computed against
// (ws.ConflictIntegrationHead). If it advanced — e.g. several workers
// conflicted against the same snapshot and another worker's resolution merged
// first — preferring this worker's hunks would silently drop the content that
// landed in between. In that case errStaleResolution is returned without
// touching the integration branch. An empty ConflictIntegrationHead (legacy
// state written before the field existed, or conflict paths that do not
// populate it) preserves the historical -X theirs behavior.
//
// Returns nil on success. On failure, the integration worktree is restored to
// its pre-merge state.
// Caller must hold wm.mu.
func (wm *Manager) mergeResolvedWorker(
	ctx context.Context,
	integrationPath string,
	ws *model.WorktreeState,
	commandID string,
) error {
	// Record pre-merge HEAD for recovery if merge --abort fails.
	preMergeHEAD, err := wm.gitOutputWithRetry(ctx, integrationPath, 2, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("pre-merge HEAD: %w", err)
	}
	preMergeHEAD = strings.TrimSpace(preMergeHEAD)
	if err := validateSHA(preMergeHEAD); err != nil {
		return fmt.Errorf("pre-merge HEAD: %w", err)
	}

	// Check if the worker branch has commits to merge.
	logOut, err := wm.gitOutputWithRetry(ctx, integrationPath, 2, "log", "--oneline",
		fmt.Sprintf("%s..%s", preMergeHEAD, ws.Branch))
	if err != nil {
		return fmt.Errorf("check worker commits: %w", err)
	}
	if strings.TrimSpace(logOut) == "" {
		// Worker branch is already fully reachable from integration HEAD.
		wm.Log(core.LogLevelDebug, "resolved_worker_already_merged command=%s worker=%s", commandID, ws.WorkerID)
		return nil
	}

	if ws.ConflictIntegrationHead != "" && preMergeHEAD != ws.ConflictIntegrationHead {
		return fmt.Errorf("%w: integration HEAD %s != conflict snapshot %s (worker %s)",
			errStaleResolution, preMergeHEAD, ws.ConflictIntegrationHead, ws.WorkerID)
	}

	strategy := wm.config.EffectiveMergeStrategy()
	mergeMsg := fmt.Sprintf("merge: conflict-resolved %s changes", ws.WorkerID)

	// Attempt merge with -X theirs: the worker has committed their conflict
	// resolution, so we trust their content for any conflicting hunks. This
	// resolves add/add conflicts (where the merge base has no file) that would
	// otherwise reproduce the same conflict on every re-merge attempt.
	mergeErr := wm.gitRunInDir(integrationPath, "merge", "--no-ff", "-s", strategy, "-X", "theirs", "-m", mergeMsg, ws.Branch)
	if mergeErr == nil {
		return nil // clean merge, no conflict
	}

	// Check for real merge conflict vs other git error.
	hasConflict, probeErr := wm.hasUnmergedFiles(integrationPath)
	if probeErr != nil {
		_ = wm.gitRunInDir(integrationPath, "merge", "--abort")
		return errors.Join(
			fmt.Errorf("conflict probe: %w", probeErr),
			fmt.Errorf("merge error: %w", mergeErr),
		)
	}
	if !hasConflict {
		// Non-conflict git error — abort and report.
		_ = wm.gitRunInDir(integrationPath, "merge", "--abort")
		return fmt.Errorf("non-conflict merge error: %w", mergeErr)
	}

	// Real merge conflict — resolve using the worker's committed content.
	conflictFiles, cfErr := wm.getConflictFilesInDir(integrationPath)
	if cfErr != nil || len(conflictFiles) == 0 {
		return wm.abortAndReturnMergeError(integrationPath, preMergeHEAD, commandID, ws.WorkerID,
			fmt.Errorf("get conflict files: %w", cfErr))
	}

	if err := wm.checkoutResolvedFilesFromBranch(integrationPath, preMergeHEAD, commandID, ws, conflictFiles); err != nil {
		return err
	}

	// Complete the merge commit. --no-edit reads the message from .git/MERGE_MSG
	// which was set by the initial merge command.
	if err := wm.gitRunInDir(integrationPath, "commit", "--no-edit"); err != nil {
		return wm.abortAndReturnMergeError(integrationPath, preMergeHEAD, commandID, ws.WorkerID,
			fmt.Errorf("commit resolved merge: %w", err))
	}

	wm.Log(core.LogLevelInfo, "conflict_resolved_merge command=%s worker=%s files=%v",
		commandID, ws.WorkerID, conflictFiles)
	return nil
}

// abortAndReturnMergeError aborts the in-flight merge and returns primaryErr.
// If `merge --abort` itself fails, recoverWorktreeAfterMerge is attempted.
// When the recovery also fails, all three errors are joined and returned.
func (wm *Manager) abortAndReturnMergeError(
	integrationPath, preMergeHEAD, commandID, workerID string,
	primaryErr error,
) error {
	abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort")
	if abortErr == nil {
		return primaryErr
	}
	if recoveryErr := wm.recoverWorktreeAfterMerge(integrationPath, preMergeHEAD, commandID, workerID); recoveryErr != nil {
		return errors.Join(
			primaryErr,
			fmt.Errorf("merge abort failed: %w", abortErr),
			fmt.Errorf("worktree recovery failed: %w", recoveryErr),
		)
	}
	return primaryErr
}

// checkoutResolvedFilesFromBranch runs `git checkout <branch> -- <file>` for
// every conflict file so the conflicting hunks are replaced with the
// worker's resolved version (working tree + index).
//
// Fallback for "pathspec did not match" errors: when the file isn't tracked
// on the worker's branch the worker effectively considers it deleted (or
// it never existed there). The previous implementation aborted the merge
// in that case, which loops forever for build artefacts that one worker
// generated and another didn't (the same file appears as add/add conflict
// on the integration side but doesn't exist on either branch's earlier
// commit). We instead accept the deletion via `git rm` so the merge can
// proceed; if that also fails (the file is purely untracked in the index)
// we remove it from the worktree directly. Only the `pathspec did not
// match` class of errors triggers the fallback — any other checkout
// failure still aborts.
func (wm *Manager) checkoutResolvedFilesFromBranch(
	integrationPath, preMergeHEAD, commandID string,
	ws *model.WorktreeState,
	conflictFiles []string,
) error {
	for _, cf := range conflictFiles {
		checkoutErr := wm.gitRunInDir(integrationPath, "checkout", ws.Branch, "--", cf)
		if checkoutErr == nil {
			continue
		}
		if !isPathspecMissingError(checkoutErr) {
			wm.Log(core.LogLevelWarn, "resolve_checkout_failed command=%s worker=%s file=%s error=%v",
				commandID, ws.WorkerID, cf, checkoutErr)
			return wm.abortAndReturnMergeError(integrationPath, preMergeHEAD, commandID, ws.WorkerID,
				fmt.Errorf("checkout resolved file %s from %s: %w", cf, ws.Branch, checkoutErr))
		}
		if rmErr := wm.gitRunInDir(integrationPath, "rm", "-f", "--", cf); rmErr == nil {
			wm.Log(core.LogLevelInfo,
				"resolve_checkout_to_rm_fallback command=%s worker=%s file=%s "+
					"(file absent on worker branch; accepting delete to clear conflict)",
				commandID, ws.WorkerID, cf)
			continue
		}
		// Last resort: remove from the working tree directly.
		fsPath := filepath.Join(integrationPath, cf)
		if removeErr := os.RemoveAll(fsPath); removeErr == nil {
			wm.Log(core.LogLevelInfo,
				"resolve_checkout_to_filesystem_remove command=%s worker=%s file=%s",
				commandID, ws.WorkerID, cf)
			continue
		}
		wm.Log(core.LogLevelWarn, "resolve_checkout_failed_after_fallback command=%s worker=%s file=%s error=%v",
			commandID, ws.WorkerID, cf, checkoutErr)
		return wm.abortAndReturnMergeError(integrationPath, preMergeHEAD, commandID, ws.WorkerID,
			fmt.Errorf("checkout resolved file %s from %s (fallback rm/remove also failed): %w", cf, ws.Branch, checkoutErr))
	}
	return nil
}

// isPathspecMissingError detects the canonical "git checkout / git rm: error:
// pathspec ... did not match any file(s) known to git" failure mode emitted
// when the named path is not present in the source ref's tree. We pattern-
// match the message because git does not expose a stable error code for it.
func isPathspecMissingError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "did not match any file") ||
		strings.Contains(msg, "pathspec") && strings.Contains(msg, "did not match")
}
