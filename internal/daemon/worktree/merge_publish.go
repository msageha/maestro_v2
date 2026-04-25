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

// errPublishDirtyRoot is returned when publish aborts because the projectRoot
// has uncommitted changes on the base branch. This is a deterministic,
// non-retryable failure: retries with backoff will keep hitting the same guard
// until the quarantine threshold is reached (~5 min). Callers treat this as a
// terminal publish failure so the integration is quarantined immediately and
// R8 (NotifyPublishQuarantined) surfaces the condition to the Planner on the
// next reconcile pass. Operator must commit/stash the root changes and then
// invoke retry-publish to unblock.
var errPublishDirtyRoot = errors.New("publish aborted: projectRoot has uncommitted changes that would be lost by reset; please commit or stash them first")

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

// forwardMergeBaseToIntegration merges the current base branch (main) into the
// integration branch so that subsequent publish (integration → base) succeeds
// without conflict. This is called automatically at the start of PublishToBase.
//
// Returns nil if the forward-merge succeeds or is unnecessary (integration is
// already up-to-date with base). On conflict the conflicting files are stored
// in state.Integration.PublishConflictFiles and an error is returned.
//
// IMPORTANT: on conflict, the merge is NOT aborted. Conflict markers are
// preserved in the integration worktree so the Planner-dispatched worker can
// resolve them in-place via --run-on-integration (git add + git commit). If we
// aborted here, the worker would see a clean worktree and report "nothing to
// do", producing an empty resolution commit while the underlying conflict
// still blocks publish — an infinite publish_conflict recovery loop.
//
// Re-entry: if a prior call left MERGE_HEAD in place (worker has not yet
// resolved, or the daemon crashed mid-merge), this function detects the
// in-flight state and either returns the same conflict (unresolved) or
// finalizes the merge commit (worker already added their resolution).
// Caller must hold wm.mu.
func (wm *Manager) forwardMergeBaseToIntegration(
	state *model.WorktreeCommandState,
	commandID, baseBranch, now string,
) error {
	integrationPath := wm.integrationWorktreePath(commandID)

	// Re-entry handling: if the integration worktree is already mid-merge from
	// a previous forwardMergeBaseToIntegration attempt, reuse that state
	// rather than trying to start a fresh merge (git refuses "merge" while
	// MERGE_HEAD exists).
	if wm.integrationHasMergeHead(integrationPath) {
		done, err := wm.reuseInFlightForwardMerge(state, commandID, integrationPath, baseBranch, now)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		// Control falls through when the in-flight merge was recovered (e.g.
		// aborted because it had become stale) and a fresh attempt is safe.
	}

	// Check if forward-merge is needed by comparing the integration branch's
	// merge-base with baseBranch to the current baseBranch HEAD.
	baseSHA, err := wm.gitOutput("rev-parse", baseBranch)
	if err != nil {
		return fmt.Errorf("forward-merge: resolve base branch: %w", err)
	}
	baseSHA = strings.TrimSpace(baseSHA)
	if err := validateSHA(baseSHA); err != nil {
		return fmt.Errorf("forward-merge: invalid base SHA: %w", err)
	}
	// Verify the SHA exists as a commit object in the repository.
	if err := wm.gitRun("cat-file", "-e", baseSHA+"^{commit}"); err != nil {
		return fmt.Errorf("forward-merge: base SHA %s not found as commit in repository: %w", baseSHA, err)
	}

	integrationSHA, err := wm.gitOutputInDir(integrationPath, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("forward-merge: resolve integration HEAD: %w", err)
	}
	integrationSHA = strings.TrimSpace(integrationSHA)
	if err := validateSHA(integrationSHA); err != nil {
		return fmt.Errorf("forward-merge: invalid integration HEAD SHA: %w", err)
	}

	mergeBaseSHA, err := wm.gitOutput("merge-base", baseSHA, integrationSHA)
	if err != nil {
		// merge-base can fail if there's no common ancestor (shouldn't happen
		// in practice). Fall through to publish and let it handle the error.
		wm.Log(core.LogLevelWarn, "forward_merge_base_check command=%s error=%v (skipping forward-merge)",
			commandID, err)
		return nil
	}
	mergeBaseSHA = strings.TrimSpace(mergeBaseSHA)

	if mergeBaseSHA == baseSHA {
		// Integration already includes base — no forward-merge needed.
		// Clear any stale conflict files since the merge is effectively
		// complete (worker's resolution commit on a prior attempt already
		// pulled base in).
		state.Integration.PublishConflictFiles = nil
		return nil
	}

	wm.Log(core.LogLevelInfo, "forward_merge_base_to_integration command=%s base=%s integration=%s merge_base=%s",
		commandID, baseSHA[:min(len(baseSHA), 8)], integrationSHA[:min(len(integrationSHA), 8)], mergeBaseSHA[:min(len(mergeBaseSHA), 8)])

	// Attempt the forward-merge: merge baseBranch into integration.
	mergeMsg := fmt.Sprintf("[maestro] forward-merge %s into integration for publish", baseBranch)
	if err := wm.gitRunInDir(integrationPath, "merge", "--no-ff", "-m", mergeMsg, baseBranch); err != nil {
		// Merge failed — likely a content conflict. Collect conflict files.
		conflictFiles, cfErr := wm.getConflictFilesInDir(integrationPath)
		if cfErr != nil {
			wm.Log(core.LogLevelWarn, "forward_merge_conflict_files command=%s error=%v", commandID, cfErr)
		}

		// DO NOT abort the merge. The conflict markers must remain in the
		// integration worktree so the Planner-dispatched resolution worker
		// (--run-on-integration) can resolve them in place. See function-
		// level doc and templates/instructions/planner.md publish_conflict
		// handler for the end-to-end contract.

		// Store conflict files in state for signal emission. PublishConflictSignaled
		// is reset to false only on a fresh conflict round (detected by an
		// absence of prior conflict files, or a change in the file set) so
		// that re-entry after an unresolved conflict does not re-emit the
		// signal on every scan.
		if !samePublishConflictFiles(state.Integration.PublishConflictFiles, conflictFiles) {
			state.Integration.PublishConflictSignaled = false
		}
		state.Integration.PublishConflictFiles = conflictFiles
		state.UpdatedAt = now

		wm.Log(core.LogLevelWarn,
			"forward_merge_conflict command=%s files=%v (markers preserved for worker resolution via --run-on-integration)",
			commandID, conflictFiles)
		return fmt.Errorf("forward-merge %s into integration: conflict on files %v", baseBranch, conflictFiles)
	}

	// Forward-merge succeeded — clear any previous conflict files.
	state.Integration.PublishConflictFiles = nil
	wm.Log(core.LogLevelInfo, "forward_merge_succeeded command=%s", commandID)
	return nil
}

// integrationHasMergeHead returns true when the integration worktree currently
// has an in-flight merge (MERGE_HEAD present). Transient or unexpected git
// errors are treated as "no merge head" so this probe never wedges the
// publish pipeline; callers that need to distinguish errors should probe
// directly.
func (wm *Manager) integrationHasMergeHead(integrationPath string) bool {
	// `git rev-parse --verify -q MERGE_HEAD`: exit 0 when MERGE_HEAD exists,
	// non-zero (silently) otherwise. Any failure mode is treated as
	// "not merging" — the subsequent fresh `git merge` will either succeed
	// (no conflict) or surface the real issue via its own error.
	_, err := wm.gitOutputInDir(integrationPath, "rev-parse", "--verify", "-q", "MERGE_HEAD")
	return err == nil
}

// reuseInFlightForwardMerge handles re-entry into forwardMergeBaseToIntegration
// when the integration worktree still has MERGE_HEAD from a prior attempt.
//
// Return values:
//   - (true, nil)  → the in-flight merge has been finalized (worker resolved
//     and staged the conflict; we created the merge commit). Caller returns
//     nil to proceed to publish.
//   - (false, nil) → the in-flight merge was aborted because it was
//     unrecoverable (e.g., worker never ran and state is stale). Caller
//     should fall through and start a fresh merge attempt.
//   - (_, err)     → the conflict is still unresolved (unmerged entries
//     present) or an internal error occurred. Caller returns this error so
//     publish records a failure without mutating the already-signaled state.
func (wm *Manager) reuseInFlightForwardMerge(
	state *model.WorktreeCommandState,
	commandID, integrationPath, baseBranch, now string,
) (bool, error) {
	hasConflict, probeErr := wm.hasUnmergedFiles(integrationPath)
	if probeErr != nil {
		return false, fmt.Errorf("forward-merge re-entry probe: %w", probeErr)
	}
	if hasConflict {
		// Worker has not yet resolved the conflict. Return the conflict error
		// WITHOUT clearing PublishConflictSignaled: the signal was already
		// emitted on the first round and the Planner has a task in flight.
		// Re-emitting would spam the signal queue on every scan.
		conflictFiles, cfErr := wm.getConflictFilesInDir(integrationPath)
		if cfErr != nil {
			wm.Log(core.LogLevelWarn, "forward_merge_reentry_conflict_files command=%s error=%v", commandID, cfErr)
		}
		wm.Log(core.LogLevelInfo,
			"forward_merge_reentry_still_conflicting command=%s files=%v",
			commandID, conflictFiles)
		// Refresh file list in state in case it changed (conservative: don't
		// disturb PublishConflictSignaled).
		if len(conflictFiles) > 0 {
			state.Integration.PublishConflictFiles = conflictFiles
			state.UpdatedAt = now
		}
		return false, fmt.Errorf("forward-merge %s into integration: unresolved conflict on files %v",
			baseBranch, conflictFiles)
	}

	// No unmerged entries but MERGE_HEAD still exists → worker staged the
	// resolution (or the auto-merge had no real conflict on this file set).
	// Finalize the merge commit so the integration branch absorbs base.
	if commitErr := wm.gitRunInDir(integrationPath, "commit", "--no-edit"); commitErr != nil {
		// A `git commit --no-edit` during an in-flight merge can fail if the
		// worker staged no changes at all (identical content on both sides).
		// In that case, `git commit --allow-empty --no-edit` finalises the
		// merge without producing a content-changing commit.
		if retryErr := wm.gitRunInDir(integrationPath, "commit", "--allow-empty", "--no-edit"); retryErr != nil {
			wm.Log(core.LogLevelWarn,
				"forward_merge_reentry_finalize_failed command=%s error=%v retry_error=%v",
				commandID, commitErr, retryErr)
			// Conservative: don't auto-abort the worker's partial state.
			// Return an error so publish records a failure; the operator /
			// planner can inspect via dashboard and re-dispatch if needed.
			return false, fmt.Errorf("forward-merge re-entry finalize: %w", commitErr)
		}
	}
	state.Integration.PublishConflictFiles = nil
	wm.Log(core.LogLevelInfo, "forward_merge_reentry_finalized command=%s", commandID)
	return true, nil
}

// samePublishConflictFiles reports whether two conflict-file sets contain the
// same entries (order-insensitive). Used to decide whether a re-entered
// conflict should re-emit the publish_conflict signal (different file set) or
// suppress re-emission (same set — Planner already has a task in flight).
func samePublishConflictFiles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	counts := make(map[string]int, len(a))
	for _, f := range a {
		counts[f]++
	}
	for _, f := range b {
		counts[f]--
		if counts[f] < 0 {
			return false
		}
	}
	for _, c := range counts {
		if c != 0 {
			return false
		}
	}
	return true
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
	// commits (2026-04 audit: integration=merged with worker=resolving).
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
			// Deterministic dirty-root aborts are non-retryable: skip backoff
			// and quarantine immediately so R8 surfaces the blocker to the
			// Planner on the next reconcile pass.
			var tErr error
			if errors.Is(returnErr, errPublishDirtyRoot) {
				tErr = wm.recordPublishTerminalFailure(state, "publish_dirty_root", now)
			} else {
				tErr = wm.recordPublishFailure(state, returnErr.Error(), now)
			}
			if tErr != nil {
				wm.Log(core.LogLevelWarn, "publish_failed_transition command=%s error=%v", commandID, tErr)
			}
			state.UpdatedAt = now
			if sErr := wm.saveState(commandID, state); sErr != nil {
				wm.Log(core.LogLevelWarn, "publish_failed_save command=%s error=%v", commandID, sErr)
			}
		}
	}()

	baseBranch := wm.config.EffectiveBaseBranch()

	// Pre-publish: forward-merge base into integration to prevent conflicts
	// when base has advanced since integration was created. If forward-merge
	// fails (content conflict), record the failure and return — the conflict
	// info is stored in state for Planner signal emission.
	if fmErr := wm.forwardMergeBaseToIntegration(state, commandID, baseBranch, now); fmErr != nil {
		wm.Log(core.LogLevelWarn, "publish_forward_merge_failed command=%s error=%v", commandID, fmErr)
		if tErr := wm.recordPublishFailure(state, "publish_forward_merge_conflict", now); tErr != nil {
			wm.Log(core.LogLevelWarn, "publish_failure_transition command=%s error=%v", commandID, tErr)
		}
		state.UpdatedAt = now
		if sErr := wm.saveState(commandID, state); sErr != nil {
			wm.Log(core.LogLevelWarn, "publish_failure_save command=%s error=%v", commandID, sErr)
		}
		return fmErr
	}

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

	// Guarantee temp branch cleanup on any return path after checkout.
	// On error paths where the worktree may still have tempBranch checked out,
	// move off it first so that branch deletion succeeds.
	tempBranchCleaned := false
	defer func() {
		if tempBranchCleaned {
			return
		}
		if chkErr := wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch); chkErr != nil {
			_ = wm.gitRunInDir(integrationPath, "checkout", "--detach")
		}
		wm.deleteTempBranch(tempBranch)
	}()

	// Merge integration branch into temp branch (at baseBranch's position)
	mergeMsg := buildPublishMessage(publishMessage, baseBranch)
	if err := wm.gitRunInDir(integrationPath, "merge", "--no-ff", "-m", mergeMsg, state.Integration.Branch); err != nil {
		if tErr := wm.recordPublishFailure(state, "publish_merge_conflict", now); tErr != nil {
			wm.Log(core.LogLevelWarn, "publish_conflict_transition command=%s error=%v", commandID, tErr)
		}
		state.UpdatedAt = now
		if saveErr := wm.saveState(commandID, state); saveErr != nil {
			wm.Log(core.LogLevelError, "save_state_failed command=%s error=%v", commandID, saveErr)
		}
		if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
			wm.Log(core.LogLevelWarn, "publish_merge_abort_failed command=%s error=%v", commandID, abortErr)
		}
		// Temp branch cleanup is handled by defer.
		if checkoutErr := wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch); checkoutErr != nil {
			wm.Log(core.LogLevelWarn, "publish_checkout_failed command=%s branch=%s error=%v", commandID, state.Integration.Branch, checkoutErr)
			return "", false, errors.Join(
				fmt.Errorf("merge integration into %s: %w", baseBranch, err),
				fmt.Errorf("checkout recovery also failed: %w", checkoutErr),
			)
		}
		return "", false, fmt.Errorf("merge integration into %s: %w", baseBranch, err)
	}

	// Get the merge commit SHA
	mergeSHAOut, err := wm.gitOutputInDir(integrationPath, "rev-parse", "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("get merge commit SHA: %w", err)
	}
	mergeSHA = strings.TrimSpace(mergeSHAOut)

	// Restore integration branch checkout before updating refs
	if checkoutErr := wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch); checkoutErr != nil {
		wm.Log(core.LogLevelWarn, "publish_restore_checkout_failed command=%s branch=%s error=%v", commandID, state.Integration.Branch, checkoutErr)
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
			return "", false, fmt.Errorf("publish dirty check failed: %w", err)
		}
		if strings.TrimSpace(statusOut) != "" {
			// Deterministic, operator-action-required failure. Return the
			// sentinel so PublishToBase's deferred handler can fast-path to
			// quarantine instead of burning the retry budget.
			return "", false, errPublishDirtyRoot
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
		return "", false, fmt.Errorf("update base branch ref (CAS failed — branch may have been modified concurrently): %w", err)
	}

	// Clean up temp branch (defer is the safety net, but explicit cleanup avoids
	// leaving the branch around until function exit on the success path).
	wm.deleteTempBranch(tempBranch)
	tempBranchCleaned = true

	return mergeSHA, baseBranchCheckedOut, nil
}

// syncProjectRootAfterPublish syncs the projectRoot working tree and index to
// match the updated base branch ref. It syncs the index via read-tree, checks
// for genuine uncommitted changes (working tree vs index) to prevent
// false-positive stash creation, runs reset --hard, and rolls back the ref on
// failure.
func (wm *Manager) syncProjectRootAfterPublish(commandID, baseBranch, baseSHA, mergeSHA string) error {
	// Sync index and working tree to the new HEAD before stash create.
	// update-ref only moves the branch pointer; the index and working tree still
	// reflect the old commit. Without this sync, stash create sees a false diff
	// between the new HEAD and old index/working tree, creating a spurious stash
	// that accumulates as orphaned refs under refs/maestro/pre-publish-stash/.
	if err := wm.gitRun("read-tree", "--reset", "-u", "HEAD"); err != nil {
		wm.Log(core.LogLevelWarn, "publish_read_tree_sync command=%s error=%v (falling back to reset)", commandID, err)
	}

	// Guard against false-positive stash creation: after update-ref advances HEAD,
	// the index/working tree may still reflect the old commit (especially when
	// read-tree fails above). Comparing working tree vs index (git diff --quiet)
	// detects genuine uncommitted changes without being fooled by the ref
	// advancement. If working tree matches index, any divergence from HEAD is
	// solely from update-ref and stash create would produce a spurious object
	// that accumulates as an orphaned ref under refs/maestro/pre-publish-stash/.
	if wm.gitRun("diff", "--quiet") != nil {
		// Working tree differs from index — preserve genuine uncommitted changes.
		stashRef, stashErr := wm.gitOutput("stash", "create")
		if stashErr != nil {
			wm.Log(core.LogLevelWarn, "publish_stash_create_failed command=%s error=%v (continuing)", commandID, stashErr)
		} else if ref := strings.TrimSpace(stashRef); ref != "" {
			durableRef := fmt.Sprintf("refs/maestro/pre-publish-stash/%s", commandID)
			if refErr := wm.gitRun("update-ref", durableRef, ref); refErr != nil {
				wm.Log(core.LogLevelWarn, "publish_stash_save_failed command=%s ref=%s error=%v", commandID, durableRef, refErr)
			} else {
				wm.Log(core.LogLevelInfo, "publish_stash_saved command=%s ref=%s sha=%s", commandID, durableRef, ref)
			}
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
	state.Integration.PublishConflictFiles = nil
	state.Integration.PublishConflictSignaled = false
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
		// Cut at maxLen bytes, then strip any partial UTF-8 sequence at the
		// tail so multi-byte runes (e.g. Japanese) are not split mid-rune.
		// strings.ToValidUTF8 with empty replacement removes invalid trailing
		// bytes; result is always <= maxLen bytes.
		msg = strings.ToValidUTF8(msg[:maxLen], "")
	}
	return msg
}
