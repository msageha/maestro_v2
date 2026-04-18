package worktree

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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

// errIntegrationQuarantined is returned when MergeToIntegration is called on an
// integration that has been quarantined due to repeated unrecoverable failures.
// Callers must surface this to operators rather than retrying.
var errIntegrationQuarantined = errors.New("integration is quarantined; manual intervention required")

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

// mergeWorkerOutcome captures the result of merging a single worker branch.
type mergeWorkerOutcome struct {
	merged          bool                 // worker was successfully merged
	skipped         bool                 // worker was skipped (nil, no commits, log error, etc.)
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

	// Guard: ensure integration worktree is clean before merging.
	if err := wm.checkIntegrationWorktreeClean(state, commandID, integrationPath); err != nil {
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
	}

	wm.determineMergeOutcome(state, commandID, mergedCount, skippedCount, conflictSkippedCount, len(sorted), conflicts, preMergeStatus, preMergeUpdatedAt, now)

	if err := wm.saveState(commandID, state); err != nil {
		return conflicts, fmt.Errorf("save state: %w", err)
	}

	return conflicts, nil
}

// checkIntegrationWorktreeClean verifies the integration worktree has no
// uncommitted changes. A dirty worktree (e.g., from incomplete merge abort)
// would cause unpredictable merge results. Persists IntegrationStatusFailed
// to prevent stale "merged" status from triggering publish.
func (wm *Manager) checkIntegrationWorktreeClean(state *model.WorktreeCommandState, commandID, integrationPath string) error {
	dirtyOut, dirtyErr := wm.gitOutputInDir(integrationPath, "status", "--porcelain")
	if dirtyErr != nil {
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if tErr := wm.recordMergeFailure(state, "status_check_failed", now); tErr != nil {
			return fmt.Errorf("check integration worktree status: %w (transition error: %s)", dirtyErr, tErr.Error())
		}
		state.UpdatedAt = now
		if saveErr := wm.saveState(commandID, state); saveErr != nil {
			wm.Log(core.LogLevelWarn, "save_state_failed command=%s error=%v", commandID, saveErr)
		}
		return fmt.Errorf("check integration worktree status: %w", dirtyErr)
	}
	if strings.TrimSpace(dirtyOut) != "" {
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if tErr := wm.recordMergeFailure(state, "dirty_worktree", now); tErr != nil {
			return fmt.Errorf("integration worktree has uncommitted changes (transition error: %s)", tErr.Error())
		}
		state.UpdatedAt = now
		if saveErr := wm.saveState(commandID, state); saveErr != nil {
			wm.Log(core.LogLevelWarn, "save_state_failed command=%s error=%v", commandID, saveErr)
		}
		return fmt.Errorf("integration worktree has uncommitted changes; aborting merge to prevent corruption")
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
		return mergeWorkerOutcome{}
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
	// Collect per-file base/ours/theirs refs via rev-parse of index stages
	for _, cf := range conflictFiles {
		baseRef, err1 := wm.gitOutputWithRetry(ctx, integrationPath, 1, "rev-parse", ":1:"+cf)
		oursRef, err2 := wm.gitOutputWithRetry(ctx, integrationPath, 1, "rev-parse", ":2:"+cf)
		theirsRef, err3 := wm.gitOutputWithRetry(ctx, integrationPath, 1, "rev-parse", ":3:"+cf)
		if err1 == nil && err2 == nil && err3 == nil {
			// Use refs from first conflict file as representative
			mc.BaseRef = strings.TrimSpace(baseRef)
			mc.OursRef = strings.TrimSpace(oursRef)
			mc.TheirsRef = strings.TrimSpace(theirsRef)
			break
		}
		// Binary files or missing stages — continue to next file
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

	if mergedCount == 0 && len(conflicts) == 0 && skippedCount == 0 && conflictSkippedCount == 0 {
		// No worker had any commits to merge. Revert the Merging status to
		// the pre-merge status to avoid a false Merged signal that would
		// trigger a no-op publish.
		wm.Log(core.LogLevelInfo, "no_commits_to_merge command=%s workers=%d", commandID, totalWorkers)
		state.Integration.Status = preMergeStatus
		state.Integration.UpdatedAt = preMergeUpdatedAt
	} else if len(conflicts) == 0 && skippedCount == 0 && conflictSkippedCount == 0 {
		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusMerged, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "merge_merged_integration_transition command=%s error=%v", commandID, tErr)
		}
	} else if mergedCount == 0 && conflictSkippedCount > 0 && len(conflicts) == 0 && skippedCount == 0 {
		// Only conflict/resolving workers were present — nothing to merge.
		// Revert to pre-merge status and wait for the resolution pipeline.
		wm.Log(core.LogLevelInfo, "all_workers_conflict_skipped command=%s conflict_skipped=%d",
			commandID, conflictSkippedCount)
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
		ws := wm.findWorker(state, workerID)
		if ws == nil {
			continue
		}

		// M2: Skip conflict-state workers
		if ws.Status == model.WorktreeStatusConflict {
			wm.Log(core.LogLevelWarn, "sync_skip_conflict command=%s worker=%s status=%s",
				commandID, workerID, ws.Status)
			continue
		}

		// Skip workers that should not be synced back to active:
		// - integrated/published: changes already in integration branch;
		//   reverting to active would lose post-merge progress
		// - resolving: in conflict-resolution pipeline (same rationale as conflict skip)
		// - failed: not syncable (failed→active is not a valid transition)
		// - cleanup_done/cleanup_failed: terminal states
		if ws.Status == model.WorktreeStatusIntegrated ||
			ws.Status == model.WorktreeStatusPublished ||
			ws.Status == model.WorktreeStatusResolving ||
			ws.Status == model.WorktreeStatusFailed ||
			ws.Status == model.WorktreeStatusCleanupDone ||
			ws.Status == model.WorktreeStatusCleanupFailed {
			wm.Log(core.LogLevelDebug, "sync_skip_non_syncable command=%s worker=%s status=%s",
				commandID, workerID, ws.Status)
			continue
		}

		// M3: Skip dirty worktrees (uncommitted changes)
		statusOut, statusErr := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
		if statusErr == nil && strings.TrimSpace(statusOut) != "" {
			wm.Log(core.LogLevelWarn, "sync_skip_dirty command=%s worker=%s",
				commandID, workerID)
			continue
		}

		// Capture pre-merge HEAD so we can recover if merge --abort fails
		preMergeHEAD, headErr := wm.gitOutputInDir(ws.Path, "rev-parse", "HEAD")
		if headErr != nil {
			wm.Log(core.LogLevelWarn, "sync_pre_head_failed command=%s worker=%s error=%v", commandID, workerID, headErr)
			continue
		}
		preMergeHEAD = strings.TrimSpace(preMergeHEAD)

		// Merge integration branch into worker worktree (no retry — merge itself is not retried)
		err := wm.gitRunInDir(ws.Path, "merge", state.Integration.Branch,
			"-m", fmt.Sprintf("[maestro] sync integration into %s", workerID))
		if err != nil {
			// Classify error: check for unmerged index entries to distinguish
			// true merge conflicts from fatal git errors.
			hasConflict, probeErr := wm.hasUnmergedFiles(ws.Path)
			if probeErr != nil {
				wm.Log(core.LogLevelWarn, "sync_probe_failed command=%s worker=%s probe_error=%v merge_error=%v",
					commandID, workerID, probeErr, err)
			}

			// Collect conflict files BEFORE aborting (abort clears unmerged state)
			var conflictFiles []string
			if hasConflict {
				var cfErr error
				conflictFiles, cfErr = wm.getConflictFilesInDir(ws.Path)
				if cfErr != nil {
					wm.Log(core.LogLevelWarn, "sync_get_conflict_files command=%s worker=%s error=%v",
						commandID, workerID, cfErr)
				}
			}

			// Abort the merge to restore worktree state
			if abortErr := wm.gitRunInDir(ws.Path, "merge", "--abort"); abortErr != nil {
				wm.Log(core.LogLevelWarn, "sync_merge_abort_failed command=%s worker=%s error=%v",
					commandID, workerID, abortErr)
				// Fallback recovery: reset --hard + clean -fd + verify
				if recoveryErr := wm.recoverWorktreeAfterMerge(ws.Path, preMergeHEAD, commandID, workerID); recoveryErr != nil {
					wm.Log(core.LogLevelError, "sync_recovery_failed command=%s worker=%s error=%v",
						commandID, workerID, recoveryErr)
					if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
						wm.Log(core.LogLevelWarn, "sync_recovery_fail_transition command=%s worker=%s error=%v",
							commandID, workerID, tErr)
					}
					syncErrors = append(syncErrors, fmt.Errorf("worker %s: sync recovery failed: %w", workerID, recoveryErr))
					continue
				}
			}

			if hasConflict {
				wm.Log(core.LogLevelWarn, "sync_conflict command=%s worker=%s files=%v",
					commandID, workerID, conflictFiles)
				if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); tErr != nil {
					wm.Log(core.LogLevelWarn, "sync_conflict_transition command=%s worker=%s error=%v",
						commandID, workerID, tErr)
				}
			} else {
				wm.Log(core.LogLevelWarn, "sync_from_integration command=%s worker=%s error=%v",
					commandID, workerID, err)
				if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
					wm.Log(core.LogLevelWarn, "sync_failed_transition command=%s worker=%s error=%v",
						commandID, workerID, tErr)
				}
			}
			continue
		}

		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusActive, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "sync_active_transition command=%s worker=%s error=%v",
				commandID, workerID, tErr)
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

// PublishToBase merges the integration branch into the base branch.
// Uses a temporary branch in the integration worktree to avoid changing projectRoot HEAD (H3).
// publishMessage is used as the commit message summary; falls back to a default if empty.
func (wm *Manager) PublishToBase(commandID string, publishMessage string) (returnErr error) {
	if err := validateIDs(commandID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.setIntegrationStatus(state, model.IntegrationStatusPublishing, now); err != nil {
		return err
	}

	// If PublishToBase returns an error while still in "publishing" state,
	// transition to "publish_failed" and persist to disk. Without this,
	// the on-disk state remains "merged" and Phase A re-collects the publish
	// item on every scan, causing an infinite loop.
	// The conflict path (which transitions to "conflict" and saves state itself)
	// and the success path (which transitions to "published") are not affected
	// because the status is no longer "publishing" when they return.
	defer func() {
		if returnErr != nil && state.Integration.Status == model.IntegrationStatusPublishing {
			if tErr := wm.recordPublishFailure(state, returnErr.Error(), now); tErr != nil {
				wm.Log(core.LogLevelWarn, "publish_failed_transition command=%s error=%v", commandID, tErr)
			}
			state.UpdatedAt = now
			if sErr := wm.saveState(commandID, state); sErr != nil {
				wm.Log(core.LogLevelWarn, "publish_failed_save command=%s error=%v", commandID, sErr)
			}
		}
	}()

	baseBranch := wm.config.EffectiveBaseBranch()
	integrationPath := wm.integrationWorktreePath(commandID)

	tempBranch, baseSHA, err := wm.createTempPublishBranch(commandID, baseBranch)
	if err != nil {
		return err
	}

	mergeSHA, baseBranchCheckedOut, err := wm.performPublishMerge(state, commandID, integrationPath, tempBranch, baseBranch, baseSHA, publishMessage, now)
	if err != nil {
		return err
	}

	// If baseBranch is checked out in projectRoot, sync the working tree and index
	// to match the updated ref. Without this, the working tree would remain at the
	// old commit state, appearing as a staged revert of all published changes.
	if baseBranchCheckedOut {
		if err := wm.syncProjectRootAfterPublish(commandID, baseBranch, baseSHA, mergeSHA); err != nil {
			return err
		}
	}

	return wm.finalizePublishState(state, commandID, baseBranch, now)
}

// createTempPublishBranch creates a temporary branch from baseBranch for the
// publish merge. Returns the temp branch name, the base SHA, or an error.
func (wm *Manager) createTempPublishBranch(commandID, baseBranch string) (tempBranch, baseSHA string, err error) {
	// Create a temporary branch from baseBranch for the merge.
	// We cannot checkout baseBranch directly because it may be checked out in projectRoot.
	tempBranch = fmt.Sprintf("maestro/%s/_publish", commandID)
	baseSHA, err = wm.gitOutput("rev-parse", baseBranch)
	if err != nil {
		return "", "", fmt.Errorf("get base SHA for publish: %w", err)
	}
	baseSHA = strings.TrimSpace(baseSHA)
	if err := validateSHA(baseSHA); err != nil {
		return "", "", fmt.Errorf("base SHA for publish: %w", err)
	}

	if err := wm.gitRun("branch", tempBranch, baseSHA); err != nil {
		return "", "", fmt.Errorf("create temp publish branch: %w", err)
	}
	return tempBranch, baseSHA, nil
}

// deleteTempBranch removes a temporary publish branch and logs a warning if deletion fails.
func (wm *Manager) deleteTempBranch(tempBranch string) {
	if err := wm.gitRun("branch", "-D", tempBranch); err != nil {
		wm.Log(core.LogLevelWarn, "delete_temp_branch_failed branch=%s error=%v", tempBranch, err)
	}
}

// performPublishMerge checks out the temp branch in the integration worktree,
// merges the integration branch, gets the merge SHA, restores the integration
// branch checkout, checks for dirty projectRoot, updates the base branch ref
// via CAS, and cleans up the temp branch. Returns the merge SHA and whether
// baseBranch is checked out in projectRoot.
func (wm *Manager) performPublishMerge(
	state *model.WorktreeCommandState,
	commandID, integrationPath, tempBranch, baseBranch, baseSHA, publishMessage, now string,
) (mergeSHA string, baseBranchCheckedOut bool, err error) {
	// Checkout temp branch in integration worktree
	if err := wm.gitRunInDir(integrationPath, "checkout", tempBranch); err != nil {
		wm.deleteTempBranch(tempBranch)
		return "", false, fmt.Errorf("checkout temp publish branch: %w", err)
	}

	// Merge integration branch into temp branch (at baseBranch's position)
	mergeMsg := buildPublishMessage(publishMessage, baseBranch)
	if err := wm.gitRunInDir(integrationPath, "merge", "--no-ff", "-m", mergeMsg, state.Integration.Branch); err != nil {
		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusConflict, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "publish_conflict_transition command=%s error=%v", commandID, tErr)
		}
		state.UpdatedAt = now
		if saveErr := wm.saveState(commandID, state); saveErr != nil {
			wm.Log(core.LogLevelError, "save_state_failed command=%s error=%v", commandID, saveErr)
		}
		if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
			wm.Log(core.LogLevelWarn, "publish_merge_abort_failed command=%s error=%v", commandID, abortErr)
		}
		if checkoutErr := wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch); checkoutErr != nil {
			// Checkout failed — branch state is indeterminate. Skip tempBranch
			// deletion to avoid operating on an unexpected HEAD. The leaked
			// tempBranch will be cleaned up by CleanupCommand/GC.
			wm.Log(core.LogLevelWarn, "publish_checkout_failed command=%s branch=%s error=%v", commandID, state.Integration.Branch, checkoutErr)
			return "", false, fmt.Errorf("merge integration into %s: %w (checkout recovery also failed: %v)", baseBranch, err, checkoutErr)
		}
		wm.deleteTempBranch(tempBranch)
		return "", false, fmt.Errorf("merge integration into %s: %w", baseBranch, err)
	}

	// Get the merge commit SHA
	mergeSHAOut, err := wm.gitOutputInDir(integrationPath, "rev-parse", "HEAD")
	if err != nil {
		_ = wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch)
		wm.deleteTempBranch(tempBranch)
		return "", false, fmt.Errorf("get merge commit SHA: %w", err)
	}
	mergeSHA = strings.TrimSpace(mergeSHAOut)

	// Restore integration branch checkout before updating refs
	if checkoutErr := wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch); checkoutErr != nil {
		wm.Log(core.LogLevelWarn, "publish_restore_checkout_failed command=%s branch=%s error=%v", commandID, state.Integration.Branch, checkoutErr)
		wm.deleteTempBranch(tempBranch)
		return "", false, fmt.Errorf("restore integration branch checkout after publish: %w", checkoutErr)
	}

	// Check if baseBranch is currently checked out in projectRoot BEFORE updating the ref.
	// We need this to know whether to sync the working tree afterwards.
	currentBranch, _ := wm.gitOutput("symbolic-ref", "--short", "HEAD")
	baseBranchCheckedOut = strings.TrimSpace(currentBranch) == baseBranch

	// If baseBranch is checked out, we'll need to reset the working tree after update-ref.
	// Check for uncommitted changes BEFORE update-ref to avoid data loss.
	if baseBranchCheckedOut {
		statusOut, err := wm.gitOutput("status", "--porcelain", "--untracked-files=no")
		if err != nil {
			wm.deleteTempBranch(tempBranch)
			return "", false, fmt.Errorf("publish dirty check failed: %w", err)
		}
		if strings.TrimSpace(statusOut) != "" {
			wm.deleteTempBranch(tempBranch)
			return "", false, fmt.Errorf("publish aborted: projectRoot has uncommitted changes that would be lost by reset; please commit or stash them first")
		}
	}

	// Update baseBranch ref to point to the merge commit.
	// NOTE: git update-ref only moves the branch pointer — it does NOT update the
	// working tree or index. If baseBranch is checked out in projectRoot, the index
	// and working tree will still reflect the old commit, causing a "revert" state
	// where `git status` shows the new changes as staged deletions.
	// Use compare-and-swap: update-ref will fail atomically if baseBranch
	// has been modified since we read baseSHA, preventing TOCTOU races.
	if err := wm.gitRun("update-ref", fmt.Sprintf("refs/heads/%s", baseBranch), mergeSHA, baseSHA); err != nil {
		wm.deleteTempBranch(tempBranch)
		return "", false, fmt.Errorf("update base branch ref (CAS failed — branch may have been modified concurrently): %w", err)
	}

	// Clean up temp branch
	wm.deleteTempBranch(tempBranch)

	return mergeSHA, baseBranchCheckedOut, nil
}

// syncProjectRootAfterPublish syncs the projectRoot working tree and index to
// match the updated base branch ref. It creates a safety stash, runs
// reset --hard, and rolls back the ref on failure.
func (wm *Manager) syncProjectRootAfterPublish(commandID, baseBranch, baseSHA, mergeSHA string) error {
	// Before reset --hard, create a durable ref to preserve any working tree state.
	// git stash create builds a stash commit without modifying the working tree or index.
	// If there are no changes (expected after dirty check above), stash create returns empty.
	stashRef, stashErr := wm.gitOutput("stash", "create")
	if stashErr != nil {
		wm.Log(core.LogLevelWarn, "publish_stash_create_failed command=%s error=%v (continuing)", commandID, stashErr)
	} else if ref := strings.TrimSpace(stashRef); ref != "" {
		// Unexpected: dirty check passed but stash create found changes.
		// Save as a durable ref so the data survives GC and can be recovered manually.
		durableRef := fmt.Sprintf("refs/maestro/pre-publish-stash/%s", commandID)
		if refErr := wm.gitRun("update-ref", durableRef, ref); refErr != nil {
			wm.Log(core.LogLevelWarn, "publish_stash_save_failed command=%s ref=%s error=%v", commandID, durableRef, refErr)
		} else {
			wm.Log(core.LogLevelInfo, "publish_stash_saved command=%s ref=%s sha=%s", commandID, durableRef, ref)
		}
	}

	// Tripwire: refuse to run destructive git ops outside the project root
	// (projectRoot is the working tree target here since gitRun runs there).
	if guardErr := ensureWithinProjectRoot(wm.projectRoot, wm.projectRoot); guardErr != nil {
		wm.Log(core.LogLevelError, "publish_reset_path_guard command=%s error=%v", commandID, guardErr)
		return fmt.Errorf("publish reset refused: %w", guardErr)
	}
	// Uncommitted changes were already checked before update-ref above.
	// Use git reset --hard to sync index + working tree to the new HEAD.
	var resetErr error
	if wm.testPublishResetHook != nil {
		resetErr = wm.testPublishResetHook()
	} else {
		resetErr = wm.gitRun("reset", "--hard", "HEAD")
	}
	if resetErr != nil {
		durableRef := fmt.Sprintf("refs/maestro/pre-publish-stash/%s", commandID)
		wm.Log(core.LogLevelWarn, "publish_reset_working_tree command=%s error=%v recovery_ref=%s — attempting CAS rollback",
			commandID, resetErr, durableRef)

		// CAS rollback: restore baseBranch to baseSHA only if still at mergeSHA.
		refSpec := fmt.Sprintf("refs/heads/%s", baseBranch)
		rollbackErr := wm.gitRun("update-ref", refSpec, baseSHA, mergeSHA)
		if rollbackErr != nil {
			wm.Log(core.LogLevelError, "publish_ref_rollback_failed command=%s error=%v", commandID, rollbackErr)
			return errors.Join(
				fmt.Errorf("working tree sync failed: %w", resetErr),
				fmt.Errorf("CAS rollback of update-ref also failed: %w", rollbackErr),
			)
		}
		wm.Log(core.LogLevelInfo, "publish_ref_rollback_success command=%s branch=%s restored_to=%s",
			commandID, baseBranch, baseSHA)
		return fmt.Errorf("working tree sync failed (ref rolled back to %s): %w", baseSHA, resetErr)
	}
	return nil
}

// finalizePublishState sets the integration to published, marks integrated
// workers as published, saves state, and logs completion.
func (wm *Manager) finalizePublishState(state *model.WorktreeCommandState, commandID, baseBranch, now string) error {
	if err := wm.setIntegrationStatus(state, model.IntegrationStatusPublished, now); err != nil {
		return err
	}
	// Reset publish failure tracking on successful publish.
	state.Integration.PublishFailureCount = 0
	state.Integration.NextPublishRetryAt = ""
	state.UpdatedAt = now

	// Mark only integrated workers as published; preserve conflict/failed statuses
	for i := range state.Workers {
		if state.Workers[i].Status != model.WorktreeStatusIntegrated {
			continue
		}
		if tErr := wm.setWorkerStatus(&state.Workers[i], model.WorktreeStatusPublished, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "publish_worker_transition command=%s worker=%s error=%v",
				commandID, state.Workers[i].WorkerID, tErr)
		}
	}

	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	wm.Log(core.LogLevelInfo, "published_to_base command=%s branch=%s", commandID, baseBranch)
	return nil
}

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

// truncateMessage builds "prefix + body" and truncates to maxLen if needed.
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
	if len(msg) > maxLen {
		msg = msg[:maxLen]
	}
	return msg
}
