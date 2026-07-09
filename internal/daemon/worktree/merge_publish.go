package worktree

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// mergeFailureQuarantineThreshold is the number of consecutive unrecoverable
// merge attempts (abort+recover failures, dirty-worktree precheck failures, or
// permanent non-conflict git errors) after which the integration is quarantined.
// Manual operator intervention is required to recover from quarantine.
const mergeFailureQuarantineThreshold = 3

// publishFailureQuarantineThreshold is the number of consecutive publish
// failures after which the integration is quarantined. Higher than merge
// because publish failures are more likely to be transient (ref race, dirty
// root, temporary git issue).
const publishFailureQuarantineThreshold = 5

// Exponential backoff parameters for publish retries. The sequence with
// default scan interval (10s) is: 10s, 20s, 40s, 80s, 160s (then quarantine).
const (
	publishRetryInitialBackoff = 10 * time.Second
	publishRetryMaxBackoff     = 5 * time.Minute
	publishRetryMultiplier     = 2
)

// ErrIntegrationBusyForwardMerge is returned when MergeToIntegration is
// called while the integration worktree still has MERGE_HEAD from an
// in-flight base→integration forward merge (publish conflict resolution
// pending). The uncommitted resolution edits in that worktree are staged
// and finalized by reuseInFlightForwardMerge on the next publish retry;
// the pre-merge `reset --hard` + `clean -fd` would destroy them and
// restart the conflict loop. Callers should treat this as "defer to next
// scan", not as a merge failure.
var ErrIntegrationBusyForwardMerge = errors.New("integration worktree has an in-flight forward merge; deferring worker merge until publish settles")

// errIntegrationQuarantined is returned when MergeToIntegration is called on an
// integration that has been quarantined due to repeated unrecoverable failures.
// Callers must surface this to operators rather than retrying.
var errIntegrationQuarantined = errors.New("integration is quarantined; manual intervention required")

// errPublishDirtyRoot is returned when publish aborts because the projectRoot
// has uncommitted changes on the base branch. This is a deterministic,
// non-retryable failure: retries with backoff will keep hitting the same guard
// until the quarantine threshold is reached (~5 min). Callers treat this as a
// terminal publish failure so the integration is quarantined immediately and
// R8 (NotifyPublishQuarantined) surfaces the condition to the Planner on the
// next reconcile pass. Operator must commit/stash the root changes and then
// invoke retry-publish to unblock.
var errPublishDirtyRoot = errors.New("publish aborted: projectRoot has uncommitted changes that would be lost by reset; please commit or stash them first")

// errPublishRefAdvancedSyncFailed is returned when the base branch ref was
// already advanced to the publish merge SHA (update-ref CAS succeeded), but
// syncing the projectRoot working tree failed AND the CAS rollback of the ref
// also failed. The publish content is on the base branch; only the projectRoot
// checkout is stale. This is deterministic and operator-required: a generic
// retry would re-enter the publish pipeline, hit the dirty-root guard against
// the half-synced root, and quarantine with a misleading "uncommitted changes"
// reason. Quarantine immediately with an accurate reason instead. Recovery:
// fix the projectRoot checkout (e.g. `git reset --hard <base branch>` after
// confirming no genuine work is present — a pre-publish stash ref is saved
// under refs/maestro/pre-publish-stash/<commandID> when changes existed),
// then `maestro plan retry-publish` (it accepts publish-sourced quarantines
// directly; `unquarantine` also works since it restores publish_failed for
// publish-sourced quarantines). The retry converges as a no-op merge because
// base already contains the integration content.
var errPublishRefAdvancedSyncFailed = errors.New("publish ref advanced but projectRoot sync and ref rollback both failed; base branch already contains the publish merge, projectRoot checkout is stale")

// recordMergeFailure increments the merge failure counter and either persists
// IntegrationStatusFailed (when below threshold) or transitions to
// IntegrationStatusQuarantined (when at/above threshold). Callers should still
// invoke saveState afterwards to persist the change.
func (wm *Manager) recordMergeFailure(state *model.WorktreeCommandState, reason string, now string) error {
	state.Integration.MergeFailureCount++
	if state.Integration.MergeFailureCount >= mergeFailureQuarantineThreshold {
		if err := wm.setIntegrationStatus(state, model.IntegrationStatusQuarantined, now); err != nil {
			return err
		}
		state.Integration.QuarantinedAt = now
		state.Integration.QuarantineReason = fmt.Sprintf("%s (failure_count=%d)", reason, state.Integration.MergeFailureCount)
		state.Integration.QuarantineSource = model.QuarantineSourceMerge
		wm.Log(core.LogLevelError, "integration_quarantined command=%s reason=%s count=%d",
			state.CommandID, reason, state.Integration.MergeFailureCount)
		return nil
	}
	return wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now)
}

// recordPublishFailure increments the publish failure counter, sets an
// exponential backoff for the next retry, and transitions to
// IntegrationStatusQuarantined when the threshold is reached.
// Callers should invoke saveState afterwards to persist the change.
func (wm *Manager) recordPublishFailure(state *model.WorktreeCommandState, reason string, now string) error {
	state.Integration.PublishFailureCount++
	if state.Integration.PublishFailureCount >= publishFailureQuarantineThreshold {
		if err := wm.setIntegrationStatus(state, model.IntegrationStatusQuarantined, now); err != nil {
			return err
		}
		state.Integration.QuarantinedAt = now
		state.Integration.QuarantineReason = fmt.Sprintf("publish: %s (failure_count=%d)", reason, state.Integration.PublishFailureCount)
		state.Integration.QuarantineSource = model.QuarantineSourcePublish
		state.Integration.NextPublishRetryAt = ""
		wm.Log(core.LogLevelError, "integration_quarantined_publish command=%s reason=%s count=%d",
			state.CommandID, reason, state.Integration.PublishFailureCount)
		return nil
	}

	// Calculate exponential backoff: initial * multiplier^(count-1), capped at max.
	backoff := publishRetryInitialBackoff
	for i := 1; i < state.Integration.PublishFailureCount; i++ {
		backoff *= time.Duration(publishRetryMultiplier)
		if backoff > publishRetryMaxBackoff {
			backoff = publishRetryMaxBackoff
			break
		}
	}

	nowTime, err := time.Parse(time.RFC3339, now)
	if err != nil {
		// Fallback: use initial backoff from current time if parse fails.
		nowTime = wm.clock.Now().UTC()
	}
	state.Integration.NextPublishRetryAt = nowTime.Add(backoff).UTC().Format(time.RFC3339)

	wm.Log(core.LogLevelWarn, "publish_failure_recorded command=%s count=%d next_retry_at=%s",
		state.CommandID, state.Integration.PublishFailureCount, state.Integration.NextPublishRetryAt)
	return wm.setIntegrationStatus(state, model.IntegrationStatusPublishFailed, now)
}

// recordPublishTerminalFailure transitions the integration straight to
// IntegrationStatusQuarantined without consuming the retry budget. Intended
// for deterministic, operator-required failures (e.g. dirty projectRoot)
// where retry-with-backoff cannot make progress and only delays the eventual
// R8 escalation. Callers should invoke saveState afterwards to persist.
func (wm *Manager) recordPublishTerminalFailure(state *model.WorktreeCommandState, reason string, now string) error {
	state.Integration.PublishFailureCount++
	if err := wm.setIntegrationStatus(state, model.IntegrationStatusQuarantined, now); err != nil {
		return err
	}
	state.Integration.QuarantinedAt = now
	state.Integration.QuarantineReason = fmt.Sprintf("publish: %s (failure_count=%d, non-retryable)",
		reason, state.Integration.PublishFailureCount)
	state.Integration.QuarantineSource = model.QuarantineSourcePublish
	state.Integration.NextPublishRetryAt = ""
	wm.Log(core.LogLevelError, "integration_quarantined_publish_terminal command=%s reason=%s count=%d",
		state.CommandID, reason, state.Integration.PublishFailureCount)
	return nil
}

// Forward-merge primitives (forwardMergeBaseToIntegration,
// recordForwardMergeConflict, integrationHasMergeHead, reuseInFlightForwardMerge,
// stageResolvedForwardMergeFiles, bytesContainConflictMarkers,
// samePublishConflictFiles) live in merge_forward.go.

// mergeWorkerOutcome captures the result of merging a single worker branch.
type mergeWorkerOutcome struct {
	merged          bool                 // worker was successfully merged
	skipped         bool                 // worker was skipped (nil, log error, etc.)
	noCommits       bool                 // worker had no commits ahead of integration (advanced to Integrated regardless)
	conflictSkipped bool                 // worker was skipped due to conflict/resolving state
	transientFail   bool                 // transient error, worker was skipped
	conflict        *model.MergeConflict // non-nil if merge conflict occurred
	fatalErr        error                // non-nil if fatal error; caller must abort
}

// MergeToIntegration merges worker branches into the integration branch.
// Returns any merge conflicts encountered. Workers are merged in deterministic order.
// All merge operations happen in the integration worktree (H3: projectRoot HEAD is never changed).
// workerPurposes maps workerID to the task purpose for descriptive commit messages.
func (wm *Manager) MergeToIntegration(ctx context.Context, commandID string, workerIDs []string, workerPurposes map[string]string) ([]model.MergeConflict, error) {
	if err := validateIDs(commandID, workerIDs...); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("load state: %w", err)
	}

	// Short-circuit: a quarantined integration must never be merged again until
	// an operator has manually inspected and reset the state. Returning early
	// without any git ops or state mutations breaks the H10 retry loop.
	if state.Integration.Status == model.IntegrationStatusQuarantined {
		return nil, fmt.Errorf("%w: command=%s reason=%s", errIntegrationQuarantined, commandID, state.Integration.QuarantineReason)
	}

	integrationPath := wm.integrationWorktreePath(commandID)

	// Tripwire: refuse to run destructive git ops outside the project root.
	// MergeToIntegration's recovery paths may invoke reset --hard + clean -fd
	// on the integration worktree, so verify containment upfront.
	if err := ensureWithinProjectRoot(wm.projectRoot, integrationPath); err != nil {
		wm.Log(core.LogLevelError, "merge_integration_path_guard command=%s error=%v", commandID, err)
		return nil, fmt.Errorf("merge to integration refused: %w", err)
	}

	// In-flight publish forward merge: MERGE_HEAD (and the uncommitted
	// conflict-resolution edits) must survive until PublishToBase re-enters
	// via reuseInFlightForwardMerge. prepareIntegrationForMerge would wipe
	// both, so defer this merge cycle; workers stay in their pre-merge
	// states and the merge item is re-collected on a later scan.
	if wm.integrationHasMergeHead(integrationPath) {
		wm.Log(core.LogLevelInfo,
			"merge_deferred_inflight_forward_merge command=%s (integration has MERGE_HEAD; publish conflict resolution in flight)",
			commandID)
		return nil, fmt.Errorf("%w: command=%s", ErrIntegrationBusyForwardMerge, commandID)
	}

	// Force-clean the integration worktree before merging. The orchestrator
	// fully owns this worktree — anything not committed via the merge path
	// is by definition transient noise (most commonly RunOnIntegration
	// verify build artefacts: `out/`, `dist/`, `.turbo/`, etc.). Without
	// this auto-clean, every verify cycle would leave the worktree dirty
	// and `MergeToIntegration` would mark it Failed → Quarantined after
	// three retries, stalling final_verification phases indefinitely.
	if err := wm.prepareIntegrationForMerge(state, commandID, integrationPath); err != nil {
		return nil, err
	}

	// Save the pre-merge status so we can revert if no commits are found.
	preMergeStatus := state.Integration.Status
	preMergeUpdatedAt := state.Integration.UpdatedAt

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.setIntegrationStatus(state, model.IntegrationStatusMerging, now); err != nil {
		return nil, err
	}
	state.UpdatedAt = now

	// Sort worker IDs for deterministic merge order
	sorted := make([]string, len(workerIDs))
	copy(sorted, workerIDs)
	sort.Strings(sorted)

	var conflicts []model.MergeConflict
	var mergedCount int
	var skippedCount int
	var conflictSkippedCount int
	var noCommitsCount int

	for _, workerID := range sorted {
		outcome := wm.mergeWorkerBranch(ctx, state, commandID, workerID, integrationPath, workerPurposes, now)

		if outcome.fatalErr != nil {
			// Fatal error: save state and return immediately
			if saveErr := wm.saveState(commandID, state); saveErr != nil {
				wm.Log(core.LogLevelError, "save_state_failed command=%s error=%v", commandID, saveErr)
			}
			return conflicts, outcome.fatalErr
		}
		if outcome.conflict != nil {
			conflicts = append(conflicts, *outcome.conflict)
		}
		if outcome.merged {
			mergedCount++
		}
		if outcome.skipped {
			skippedCount++
		}
		if outcome.conflictSkipped {
			conflictSkippedCount++
		}
		if outcome.transientFail {
			skippedCount++
		}
		if outcome.noCommits {
			noCommitsCount++
		}
	}

	if noCommitsCount > 0 {
		wm.Log(core.LogLevelInfo,
			"merge_no_commits_summary command=%s no_commits=%d total_workers=%d merged=%d",
			commandID, noCommitsCount, len(sorted), mergedCount)
	}

	wm.determineMergeOutcome(state, commandID, mergedCount, skippedCount, conflictSkippedCount, len(sorted), conflicts, preMergeStatus, preMergeUpdatedAt, now)

	if err := wm.saveState(commandID, state); err != nil {
		return conflicts, fmt.Errorf("save state: %w", err)
	}

	return conflicts, nil
}

// prepareIntegrationForMerge force-cleans the integration worktree so the
// merge starts from a deterministic, dirt-free state. The orchestrator
// owns this worktree; anything outside committed-via-merge content is
// transient (most commonly RunOnIntegration verify build artefacts).
//
// Sequence:
//  1. `git reset --hard HEAD` — discard staged/tracked modifications.
//  2. `git clean -fd` — discard untracked files and empty directories
//     (gitignored content is honoured; anything in `.gitignore` stays).
//  3. status verification — defensive sanity check that the worktree is
//     clean. If still dirty (extremely rare: filesystem permission issue
//     blocking removal), record a merge failure so the publish gate
//     blocks until an operator intervenes.
//
// Invariants:
//   - operates only inside `.maestro/worktrees/<cmd>/_integration` (the
//     ensureWithinProjectRoot guard at the caller already verified this)
//   - never touches the project root
//   - never modifies the worker worktrees (different paths)
func (wm *Manager) prepareIntegrationForMerge(state *model.WorktreeCommandState, commandID, integrationPath string) error {
	// Capture what we would have logged so an operator can correlate any
	// auto-clean with the prior RunOnIntegration verify.
	preStatusOut, _ := wm.gitOutputInDir(integrationPath, "status", "--porcelain")
	preStatus := strings.TrimSpace(preStatusOut)

	if err := wm.gitRunInDir(integrationPath, "reset", "--hard", "HEAD"); err != nil {
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if tErr := wm.recordMergeFailure(state, "pre_merge_reset_failed", now); tErr != nil {
			return fmt.Errorf("pre-merge reset --hard HEAD failed: %w (transition error: %s)", err, tErr.Error())
		}
		state.UpdatedAt = now
		if saveErr := wm.saveState(commandID, state); saveErr != nil {
			wm.Log(core.LogLevelWarn, "save_state_failed command=%s error=%v", commandID, saveErr)
		}
		return fmt.Errorf("pre-merge reset --hard HEAD: %w", err)
	}
	if err := wm.gitRunInDir(integrationPath, "clean", "-fd"); err != nil {
		// Non-fatal: log and proceed to the status check. `clean -fd` can
		// occasionally fail on permission-restricted untracked entries
		// the OS refuses to remove; the subsequent status check will
		// catch that and route through the dirty-worktree failure path.
		wm.Log(core.LogLevelWarn,
			"pre_merge_clean_warning command=%s error=%v (proceeding to status check)",
			commandID, err)
	}

	dirtyOut, dirtyErr := wm.gitOutputInDir(integrationPath, "status", "--porcelain")
	if dirtyErr != nil {
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if tErr := wm.recordMergeFailure(state, "status_check_failed", now); tErr != nil {
			return fmt.Errorf("post-clean status check failed: %w (transition error: %s)", dirtyErr, tErr.Error())
		}
		state.UpdatedAt = now
		if saveErr := wm.saveState(commandID, state); saveErr != nil {
			wm.Log(core.LogLevelWarn, "save_state_failed command=%s error=%v", commandID, saveErr)
		}
		return fmt.Errorf("post-clean status check: %w", dirtyErr)
	}
	if strings.TrimSpace(dirtyOut) != "" {
		// reset+clean did not yield a clean tree. This is rare and points
		// at a real filesystem-level issue; record a merge failure so the
		// publish gate blocks until an operator intervenes.
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if tErr := wm.recordMergeFailure(state, "dirty_worktree_after_clean", now); tErr != nil {
			return fmt.Errorf("integration worktree still dirty after auto-clean (transition error: %s)", tErr.Error())
		}
		state.UpdatedAt = now
		if saveErr := wm.saveState(commandID, state); saveErr != nil {
			wm.Log(core.LogLevelWarn, "save_state_failed command=%s error=%v", commandID, saveErr)
		}
		return fmt.Errorf("integration worktree still dirty after reset --hard + clean -fd: %s",
			strings.TrimSpace(dirtyOut))
	}
	if preStatus != "" {
		wm.Log(core.LogLevelInfo,
			"integration_pre_merge_autoclean command=%s pre_status_lines=%d "+
				"(transient artefacts cleared before merge — typical cause: RunOnIntegration verify build outputs)",
			commandID, len(strings.Split(preStatus, "\n")))
	}
	return nil
}

// mergeWorkerBranch handles the merge of a single worker branch into the
// integration branch. It performs skip checks, commit detection, the merge
// itself, and delegates error handling to handleWorkerMergeConflict or
// handleWorkerMergeNonConflictError.
func (wm *Manager) mergeWorkerBranch(
	ctx context.Context,
	state *model.WorktreeCommandState,
	commandID, workerID, integrationPath string,
	workerPurposes map[string]string,
	now string,
) mergeWorkerOutcome {
	ws := wm.findWorker(state, workerID)
	if ws == nil {
		return mergeWorkerOutcome{}
	}

	// Skip workers already integrated — avoids redundant re-merge on
	// partial_merge/conflict recovery where some workers succeeded earlier.
	if ws.Status == model.WorktreeStatusIntegrated {
		wm.Log(core.LogLevelDebug, "skip_already_integrated command=%s worker=%s", commandID, workerID)
		return mergeWorkerOutcome{merged: true} // count as merged for final status determination
	}

	// Skip workers in conflict or resolving state. These must go through
	// the resolution pipeline (DispatchConflictResolution → resume-merge)
	// before being re-merged. Attempting to re-merge a conflict worker
	// causes invalid_worktree_transition (conflict→conflict is not valid).
	if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
		wm.Log(core.LogLevelInfo, "skip_conflict_resolving_worker command=%s worker=%s status=%s",
			commandID, workerID, ws.Status)
		return mergeWorkerOutcome{conflictSkipped: true}
	}

	// Check if worker branch has commits beyond base
	logOut, err := wm.gitOutputWithRetry(ctx, integrationPath, 2, "log", "--oneline",
		fmt.Sprintf("%s..%s", state.Integration.BaseSHA, ws.Branch))
	if err != nil {
		wm.Log(core.LogLevelWarn, "merge_log_check command=%s worker=%s error=%v", commandID, workerID, err)
		return mergeWorkerOutcome{}
	}
	if strings.TrimSpace(logOut) == "" {
		wm.Log(core.LogLevelDebug, "no_commits_to_merge command=%s worker=%s", commandID, workerID)
		// Advance the worker out of any pre-merge status (Active/Created/
		// Committed) into Integrated even though we did not run a real merge.
		// Otherwise a no-change worker stays at Active forever, the publish
		// gate keeps emitting `worktree_publish_skip_no_commits_deferred ...
		// reason=status_active` every scan, and orphan cleanup defers
		// indefinitely waiting for the worker to "finish". Reported
		// 2026-05-04 as a deferred-publish loop on a single-task command
		// whose worker output was completely empty.
		// Capture the pre-transition status BEFORE setWorkerStatus mutates ws.
		// Without this, the log printed `from=integrated` because the pointer
		// was already updated by the time `ws.Status` was formatted.
		fromStatus := ws.Status
		switch fromStatus {
		case model.WorktreeStatusActive,
			model.WorktreeStatusCreated,
			model.WorktreeStatusCommitted:
			if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusIntegrated, now); tErr != nil {
				wm.Log(core.LogLevelWarn,
					"merge_no_commits_status_advance_failed command=%s worker=%s from=%s error=%v",
					commandID, workerID, fromStatus, tErr)
			} else {
				wm.Log(core.LogLevelInfo,
					"merge_no_commits_status_advance command=%s worker=%s from=%s to=integrated",
					commandID, workerID, fromStatus)
			}
		case model.WorktreeStatusIntegrated:
			// Already integrated by an earlier merge attempt — nothing to do.
			wm.Log(core.LogLevelDebug,
				"merge_no_commits_already_integrated command=%s worker=%s",
				commandID, workerID)
		default:
			// Conflict / Resolving / terminal-* land here. They are owned by
			// other pipelines (resume-merge / cleanup) and must not be touched.
			wm.Log(core.LogLevelDebug,
				"merge_no_commits_status_unchanged command=%s worker=%s status=%s reason=non_advanceable",
				commandID, workerID, fromStatus)
		}
		return mergeWorkerOutcome{noCommits: true}
	}

	// Record per-worker pre-merge HEAD so abort recovery resets only this merge
	perWorkerPreMergeHEAD, err := wm.gitOutputWithRetry(ctx, integrationPath, 2, "rev-parse", "HEAD")
	if err != nil {
		wm.Log(core.LogLevelWarn, "merge_pre_head_failed command=%s worker=%s error=%v", commandID, workerID, err)
		return mergeWorkerOutcome{}
	}
	perWorkerPreMergeHEAD = strings.TrimSpace(perWorkerPreMergeHEAD)
	if err := validateSHA(perWorkerPreMergeHEAD); err != nil {
		wm.Log(core.LogLevelWarn, "merge_invalid_pre_head command=%s worker=%s error=%v", commandID, workerID, err)
		return mergeWorkerOutcome{}
	}

	// Merge worker branch in integration worktree (not projectRoot)
	strategy := wm.config.EffectiveMergeStrategy()
	mergeMsg := buildMergeMessage(workerID, workerPurposes)

	err = wm.gitRunInDir(integrationPath, "merge", "--no-ff", "-s", strategy, "-m", mergeMsg, ws.Branch)
	if err != nil {
		// Classify error: check for unmerged index entries to distinguish
		// true merge conflicts from fatal git errors (bad ref, corrupt repo, etc.)
		hasConflict, probeErr := wm.hasUnmergedFiles(integrationPath)
		if probeErr != nil {
			// Probe failed — can't classify reliably. Treat as non-conflict (fail-safe).
			wm.Log(core.LogLevelWarn, "merge_probe_failed command=%s worker=%s probe_error=%v merge_error=%v",
				commandID, workerID, probeErr, err)
		}

		if hasConflict {
			mc, conflictErr := wm.handleWorkerMergeConflict(ctx, state, commandID, workerID, integrationPath, perWorkerPreMergeHEAD, now, ws)
			if conflictErr != nil {
				return mergeWorkerOutcome{conflict: &mc, fatalErr: conflictErr}
			}
			return mergeWorkerOutcome{conflict: &mc}
		}

		transient, fatalErr := wm.handleWorkerMergeNonConflictError(state, commandID, workerID, integrationPath, perWorkerPreMergeHEAD, now, ws, err)
		if fatalErr != nil {
			return mergeWorkerOutcome{fatalErr: fatalErr}
		}
		if transient {
			return mergeWorkerOutcome{transientFail: true}
		}
		// Should not reach here — handleWorkerMergeNonConflictError returns either transient or fatalErr
		return mergeWorkerOutcome{}
	}

	if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusIntegrated, now); tErr != nil {
		wm.Log(core.LogLevelWarn, "merge_integrated_transition command=%s worker=%s error=%v",
			commandID, workerID, tErr)
	}
	wm.Log(core.LogLevelInfo, "worker_merged command=%s worker=%s", commandID, workerID)
	return mergeWorkerOutcome{merged: true}
}

// handleWorkerMergeConflict handles a merge conflict for a single worker.
// It collects conflict details, aborts the merge, and handles recovery if
// the abort fails. Returns the MergeConflict and a non-nil error only if
// the worktree is stuck and the caller must abort the entire merge loop.
func (wm *Manager) handleWorkerMergeConflict(
	ctx context.Context,
	state *model.WorktreeCommandState,
	commandID, workerID, integrationPath, preMergeHEAD, now string,
	ws *model.WorktreeState,
) (model.MergeConflict, error) {
	// Collect conflict detail refs (base/ours/theirs) before aborting
	conflictFiles, _ := wm.getConflictFilesInDir(integrationPath)
	mc := model.MergeConflict{
		WorkerID:      workerID,
		ConflictFiles: conflictFiles,
		Message:       fmt.Sprintf("merge conflict: %s → integration", workerID),
	}
	// Capture commit SHAs for ours/theirs/base before aborting the merge.
	// HEAD = integration branch tip (ours), MERGE_HEAD = worker branch tip (theirs).
	// Using commit SHAs (not per-file blob SHAs from index stages) means workers
	// can use "git show <sha>:<file>" for any conflict file — the natural pattern
	// for referencing content at a specific commit. Blob SHAs from ":2:<file>" can
	// only be used with "git show <blobSHA>" (no path), which is non-obvious and
	// fails when workers try the commit syntax. Commit SHAs also work uniformly
	// for new files, deleted files, and binary files regardless of index stage
	// availability.
	if oursRef, err := wm.gitOutputWithRetry(ctx, integrationPath, 1, "rev-parse", "HEAD"); err == nil {
		mc.OursRef = strings.TrimSpace(oursRef)
	}
	if theirsRef, err := wm.gitOutputWithRetry(ctx, integrationPath, 1, "rev-parse", "MERGE_HEAD"); err == nil {
		mc.TheirsRef = strings.TrimSpace(theirsRef)
	}
	if mc.OursRef != "" && mc.TheirsRef != "" {
		if baseRef, err := wm.gitOutputWithRetry(ctx, integrationPath, 1, "merge-base", mc.OursRef, mc.TheirsRef); err == nil {
			mc.BaseRef = strings.TrimSpace(baseRef)
		}
	}

	if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
		wm.Log(core.LogLevelWarn, "merge_abort_failed command=%s worker=%s error=%v",
			commandID, workerID, abortErr)
		// Fallback recovery: reset --hard + clean + verify (per-worker HEAD)
		if recoveryErr := wm.recoverWorktreeAfterMerge(integrationPath, preMergeHEAD, commandID, workerID); recoveryErr != nil {
			if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); tErr != nil {
				wm.Log(core.LogLevelWarn, "merge_recovery_conflict_transition command=%s worker=%s error=%v", commandID, workerID, tErr)
			}
			if tErr := wm.recordMergeFailure(state, "abort_recover_failed_conflict", now); tErr != nil {
				wm.Log(core.LogLevelWarn, "merge_recovery_fail_transition command=%s error=%v", commandID, tErr)
			}
			state.UpdatedAt = now
			return mc, fmt.Errorf("worktree stuck after merge abort failure for worker %s: %w", workerID, recoveryErr)
		}
	}

	// Pin the worker branch HEAD at the moment of conflict detection. The
	// deferred-merge guard in tryMergeWorker uses this to distinguish a
	// non-Claude Worker that self-committed the resolution (HEAD advances
	// past this snapshot, worktree clean) from a Worker whose
	// __conflict_resolution task is still in flight (HEAD unchanged,
	// worktree clean). Without it, self-commit Workers would loop forever
	// on the deferred branch.
	ws.ConflictBranchHead = mc.TheirsRef
	// Pin the integration HEAD ("ours") the resolver will reconcile against.
	// ResumeMerge's lost-update guard compares this snapshot with the
	// integration HEAD at merge time: if another worker's resolution landed
	// in between, this worker's resolution is stale and must not be merged
	// with `-X theirs` (it would overwrite the newer integration content).
	ws.ConflictIntegrationHead = mc.OursRef
	if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); tErr != nil {
		wm.Log(core.LogLevelWarn, "merge_conflict_transition command=%s worker=%s error=%v",
			commandID, workerID, tErr)
	}
	wm.Log(core.LogLevelWarn, "merge_conflict command=%s worker=%s files=%v",
		commandID, workerID, conflictFiles)
	return mc, nil
}

// handleWorkerMergeNonConflictError handles a merge error that is not a
// conflict. It classifies the error, attempts abort/recovery, and returns
// whether the error was transient (worker skipped, loop continues) or fatal
// (caller must abort the merge loop).
func (wm *Manager) handleWorkerMergeNonConflictError(
	state *model.WorktreeCommandState,
	commandID, workerID, integrationPath, preMergeHEAD, now string,
	ws *model.WorktreeState,
	mergeErr error,
) (transient bool, fatalErr error) {
	// Non-conflict error: classify as transient or permanent
	errClass := classifyGitError(mergeErr)

	if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
		wm.Log(core.LogLevelWarn, "merge_abort_failed command=%s worker=%s error=%v",
			commandID, workerID, abortErr)
		if recoveryErr := wm.recoverWorktreeAfterMerge(integrationPath, preMergeHEAD, commandID, workerID); recoveryErr != nil {
			if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
				wm.Log(core.LogLevelWarn, "merge_recovery_worker_fail_transition command=%s worker=%s error=%v", commandID, workerID, tErr)
			}
			if tErr := wm.recordMergeFailure(state, "abort_recover_failed_nonconflict", now); tErr != nil {
				wm.Log(core.LogLevelWarn, "merge_recovery_fail_transition command=%s error=%v", commandID, tErr)
			}
			state.UpdatedAt = now
			return false, fmt.Errorf("worktree stuck after merge abort failure for worker %s: %w", workerID, recoveryErr)
		}
	}

	if errClass == gitErrorTransient {
		// Transient error: skip this worker, continue loop
		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "merge_transient_fail_transition command=%s worker=%s error=%v",
				commandID, workerID, tErr)
		}
		wm.Log(core.LogLevelWarn, "merge_transient_error_skip command=%s worker=%s error=%v",
			commandID, workerID, mergeErr)
		return true, nil
	}

	// Permanent error: halt merge loop — repo-level problem
	if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
		wm.Log(core.LogLevelWarn, "merge_fail_transition command=%s worker=%s error=%v",
			commandID, workerID, tErr)
	}
	if tErr := wm.recordMergeFailure(state, "permanent_git_error", now); tErr != nil {
		wm.Log(core.LogLevelWarn, "merge_integration_fail_transition command=%s error=%v",
			commandID, tErr)
	}
	state.UpdatedAt = now

	wm.Log(core.LogLevelError, "merge_non_conflict_error command=%s worker=%s error=%v",
		commandID, workerID, mergeErr)

	return false, fmt.Errorf("non-conflict merge error for worker %s: %w", workerID, mergeErr)
}

// determineMergeOutcome sets the final integration status based on the
// accumulated merge results. It mutates state in place.
func (wm *Manager) determineMergeOutcome(
	state *model.WorktreeCommandState,
	commandID string,
	mergedCount, skippedCount, conflictSkippedCount, totalWorkers int,
	conflicts []model.MergeConflict,
	preMergeStatus model.IntegrationStatus,
	preMergeUpdatedAt, now string,
) {
	// Determine final integration status:
	// - mergedCount=0, conflicts=0, skipped=0, conflictSkipped=0 → no commits to merge; revert
	// - conflicts=0, skipped=0, conflictSkipped=0, merged>0 → Merged (all successful)
	// - conflicts=0, skipped=0, conflictSkipped>0, merged>0 → PartialMerge (some conflict-skipped)
	// - conflicts>0, merged>0 → PartialMerge (some succeeded, some conflicted)
	// - mergedCount=0, conflictSkipped>0, conflicts=0, skipped=0 → revert (only conflict workers, wait for resolver)
	// - merged=0 (all conflict or skipped) → Conflict
	//
	// conflictSkippedCount tracks workers intentionally skipped because they are
	// in conflict/resolving state and must go through the resolution pipeline.
	// These do not indicate new failures but existing unresolved conflicts.
	//
	// Reaching this point means no unrecoverable merge failure occurred
	// (recordMergeFailure was never called), so reset the consecutive failure count.
	state.Integration.MergeFailureCount = 0

	// Detect workers elsewhere in state.Workers that are still awaiting
	// conflict resolution, even if they were intentionally excluded from
	// this merge round's workerIDs. The Phase A collector skips Conflict/
	// Resolving workers (see eligibleWorkerIDsForAutoCommit) to avoid
	// invalid CommitWorkerChanges calls, so without this cross-check the
	// outcome loop sees only already-Integrated workers → mergedCount>0,
	// everything-else=0 → the "all successful" branch flips integration to
	// Merged while the resolution pipeline is still running on the excluded
	// workers. That false-Merged transition unblocks Phase A's merge gate
	// (isPhaseMergeRecorded) and lets downstream phases and publish run
	// against an integration branch that hasn't absorbed the resolution
	// commits — leaving an integration=merged state alongside worker=resolving.
	statePendingResolution := 0
	for _, ws := range state.Workers {
		if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
			statePendingResolution++
		}
	}

	if mergedCount == 0 && len(conflicts) == 0 && skippedCount == 0 && conflictSkippedCount == 0 && statePendingResolution == 0 {
		// No worker had any commits to merge. Revert the Merging status to
		// the pre-merge status to avoid a false Merged signal that would
		// trigger a no-op publish.
		wm.Log(core.LogLevelInfo, "no_commits_to_merge command=%s workers=%d", commandID, totalWorkers)
		state.Integration.Status = preMergeStatus
		state.Integration.UpdatedAt = preMergeUpdatedAt
	} else if len(conflicts) == 0 && skippedCount == 0 && conflictSkippedCount == 0 && statePendingResolution == 0 {
		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusMerged, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "merge_merged_integration_transition command=%s error=%v", commandID, tErr)
		}
	} else if mergedCount == 0 && len(conflicts) == 0 && skippedCount == 0 &&
		(conflictSkippedCount > 0 || statePendingResolution > 0) {
		// Only conflict/resolving workers were present — nothing to merge.
		// Revert to pre-merge status and wait for the resolution pipeline.
		// statePendingResolution covers workers filtered out of workerIDs
		// by the Phase A collector but still awaiting resolution.
		wm.Log(core.LogLevelInfo, "all_workers_conflict_skipped command=%s conflict_skipped=%d state_pending_resolution=%d",
			commandID, conflictSkippedCount, statePendingResolution)
		state.Integration.Status = preMergeStatus
		state.Integration.UpdatedAt = preMergeUpdatedAt
	} else if mergedCount > 0 {
		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusPartialMerge, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "merge_partial_merge_integration_transition command=%s error=%v", commandID, tErr)
		}
	} else {
		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusConflict, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "merge_conflict_integration_transition command=%s error=%v", commandID, tErr)
		}
	}
	state.UpdatedAt = now
}

// SyncFromIntegration updates worker worktrees with the latest integration branch state.
func (wm *Manager) SyncFromIntegration(commandID string, workerIDs []string) error {
	if err := validateIDs(commandID, workerIDs...); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)

	var syncErrors []error
	for _, workerID := range workerIDs {
		if err := wm.syncWorkerFromIntegration(state, commandID, workerID, now); err != nil {
			syncErrors = append(syncErrors, err)
		}
	}

	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	if len(syncErrors) > 0 {
		return fmt.Errorf("sync failures: %w", errors.Join(syncErrors...))
	}
	return nil
}

// syncWorkerFromIntegration performs the per-worker sync step of
// SyncFromIntegration. Returns nil for normal completion (success, skipped,
// or recoverable conflict/failure that has been transitioned in state); a
// non-nil error is reserved for recovery-failure cases that the outer caller
// must aggregate.
//
// Logging keys (`sync_*`) are preserved verbatim for log-watcher
// compatibility.
func (wm *Manager) syncWorkerFromIntegration(
	state *model.WorktreeCommandState,
	commandID, workerID, now string,
) error {
	ws := wm.findWorker(state, workerID)
	if ws == nil {
		return nil
	}
	if !wm.shouldSyncWorker(ws, commandID, workerID) {
		return nil
	}

	// M3: Skip dirty worktrees (uncommitted changes).
	statusOut, statusErr := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
	if statusErr == nil && strings.TrimSpace(statusOut) != "" {
		wm.Log(core.LogLevelWarn, "sync_skip_dirty command=%s worker=%s", commandID, workerID)
		return nil
	}

	// Capture pre-merge HEAD so we can recover if merge --abort fails.
	preMergeHEAD, headErr := wm.gitOutputInDir(ws.Path, "rev-parse", "HEAD")
	if headErr != nil {
		wm.Log(core.LogLevelWarn, "sync_pre_head_failed command=%s worker=%s error=%v", commandID, workerID, headErr)
		return nil
	}
	preMergeHEAD = strings.TrimSpace(preMergeHEAD)

	// Merge integration branch into worker worktree (merge itself is not retried).
	mergeErr := wm.gitRunInDir(ws.Path, "merge", state.Integration.Branch,
		"-m", fmt.Sprintf("[maestro] sync integration into %s", workerID))
	if mergeErr == nil {
		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusActive, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "sync_active_transition command=%s worker=%s error=%v",
				commandID, workerID, tErr)
		}
		return nil
	}
	return wm.handleSyncMergeError(ws, mergeErr, preMergeHEAD, commandID, workerID, now)
}

// shouldSyncWorker returns false for workers that must not be synced back
// to active. Logging is performed in-place so the caller does not need to
// reproduce the diagnostic vocabulary.
func (wm *Manager) shouldSyncWorker(ws *model.WorktreeState, commandID, workerID string) bool {
	if ws.Status == model.WorktreeStatusConflict {
		wm.Log(core.LogLevelWarn, "sync_skip_conflict command=%s worker=%s status=%s",
			commandID, workerID, ws.Status)
		return false
	}
	// integrated/published → changes already in integration branch; reverting to
	//   active would lose post-merge progress.
	// resolving           → in conflict-resolution pipeline (same rationale as conflict skip).
	// failed              → not syncable (failed→active is not a valid transition).
	// cleanup_done/cleanup_failed → terminal states.
	switch ws.Status {
	case model.WorktreeStatusIntegrated,
		model.WorktreeStatusPublished,
		model.WorktreeStatusResolving,
		model.WorktreeStatusFailed,
		model.WorktreeStatusCleanupDone,
		model.WorktreeStatusCleanupFailed:
		wm.Log(core.LogLevelDebug, "sync_skip_non_syncable command=%s worker=%s status=%s",
			commandID, workerID, ws.Status)
		return false
	}
	return true
}

// handleSyncMergeError classifies a failed `git merge integration` and
// drives the recovery / state transition. Returns a non-nil error only when
// the abort-then-recover fallback fails (the outer caller aggregates these
// into the SyncFromIntegration return).
func (wm *Manager) handleSyncMergeError(
	ws *model.WorktreeState,
	mergeErr error,
	preMergeHEAD, commandID, workerID, now string,
) error {
	hasConflict, probeErr := wm.hasUnmergedFiles(ws.Path)
	if probeErr != nil {
		wm.Log(core.LogLevelWarn, "sync_probe_failed command=%s worker=%s probe_error=%v merge_error=%v",
			commandID, workerID, probeErr, mergeErr)
	}

	// Collect conflict files BEFORE aborting (abort clears unmerged state).
	var conflictFiles []string
	if hasConflict {
		var cfErr error
		conflictFiles, cfErr = wm.getConflictFilesInDir(ws.Path)
		if cfErr != nil {
			wm.Log(core.LogLevelWarn, "sync_get_conflict_files command=%s worker=%s error=%v",
				commandID, workerID, cfErr)
		}
	}

	if abortErr := wm.gitRunInDir(ws.Path, "merge", "--abort"); abortErr != nil {
		wm.Log(core.LogLevelWarn, "sync_merge_abort_failed command=%s worker=%s error=%v",
			commandID, workerID, abortErr)
		if recoveryErr := wm.recoverWorktreeAfterMerge(ws.Path, preMergeHEAD, commandID, workerID); recoveryErr != nil {
			wm.Log(core.LogLevelError, "sync_recovery_failed command=%s worker=%s error=%v",
				commandID, workerID, recoveryErr)
			if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
				wm.Log(core.LogLevelWarn, "sync_recovery_fail_transition command=%s worker=%s error=%v",
					commandID, workerID, tErr)
			}
			return fmt.Errorf("worker %s: sync recovery failed: %w", workerID, recoveryErr)
		}
	}

	if hasConflict {
		wm.Log(core.LogLevelWarn, "sync_conflict command=%s worker=%s files=%v",
			commandID, workerID, conflictFiles)
		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "sync_conflict_transition command=%s worker=%s error=%v",
				commandID, workerID, tErr)
		}
		return nil
	}
	wm.Log(core.LogLevelWarn, "sync_from_integration command=%s worker=%s error=%v",
		commandID, workerID, mergeErr)
	if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
		wm.Log(core.LogLevelWarn, "sync_failed_transition command=%s worker=%s error=%v",
			commandID, workerID, tErr)
	}
	return nil
}

// Publish-to-base pipeline (PublishToBase, createTempPublishBranch,
// deleteTempBranch, performPublishMerge, mergeIntegrationIntoPublishTemp,
// fastForwardBaseBranchRef, syncProjectRootAfterPublish, finalizePublishState)
// lives in publish.go.

const mergePublishMaxLen = 72

// buildMergeMessage creates a merge commit message from the worker's task purpose.
func buildMergeMessage(workerID string, workerPurposes map[string]string) string {
	prefix := "merge: "
	if workerPurposes != nil {
		if purpose, ok := workerPurposes[workerID]; ok && purpose != "" {
			return truncateMessage(prefix, purpose, mergePublishMaxLen)
		}
	}
	return prefix + workerID + " changes"
}

// buildPublishMessage creates a publish commit message from the command content summary.
func buildPublishMessage(publishMessage, baseBranch string) string {
	prefix := "publish: "
	if publishMessage != "" {
		return truncateMessage(prefix, publishMessage, mergePublishMaxLen)
	}
	return prefix + "integrate changes to " + baseBranch
}

// truncateMessage builds "prefix + body" and truncates to maxLen runes.
// The limit is rune-based, not byte-based: a 72-byte cut leaves only ~21
// Japanese characters, destroying most of the summary ("publish: 前回の
// コマンド cmd_xxx が parti" — E2E 2026-06-11). Rune counting keeps the
// informational budget comparable across scripts while staying within
// git's subject-line conventions for ASCII messages.
// If body contains newlines, only the first line is used.
func truncateMessage(prefix, body string, maxLen int) string {
	// Use only the first line
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		body = body[:idx]
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return prefix
	}
	msg := prefix + body
	if utf8.RuneCountInString(msg) > maxLen {
		runes := []rune(msg)
		msg = string(runes[:maxLen])
	}
	return msg
}
