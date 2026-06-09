package worktree

// Forward-merge primitives. Covers the base → integration forward merge
// that runs at the start of PublishToBase to keep integration up-to-date
// with the base branch, including in-flight merge re-entry, daemon-side
// staging of worker-resolved conflict files, and conflict-marker detection.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

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
// resolve them in-place via --run-on-integration. If we aborted here, the
// worker would see a clean worktree and report "nothing to do" while the
// underlying conflict still blocks publish.
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

	// Ensure the integration worktree is actually on the integration branch
	// before merging base into it. A prior crashed/aborted publish can leave
	// the worktree on the `_publish` temp branch or detached: performPublishMerge
	// checks out `_publish`, then is supposed to restore the integration branch,
	// but a crash between those steps — or a restore failure that detaches HEAD
	// (publish.go fallback) — bypasses the restore. Without this guard the
	// `git merge baseBranch` below lands the forward-merge on the wrong ref
	// while the real integration branch stays behind, losing forward-merge
	// progress or producing a recurring publish-conflict loop. The in-flight
	// merge case is handled by the re-entry block above (a worktree mid-merge on
	// the integration branch is already on the right branch, so this is a no-op).
	if err := wm.ensureIntegrationBranchCheckedOutLocked(state, commandID); err != nil {
		return fmt.Errorf("forward-merge: ensure integration branch checked out: %w", err)
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
		return wm.recordForwardMergeConflict(state, commandID, integrationPath, baseBranch, now)
	}

	// Forward-merge succeeded — clear any previous conflict files.
	state.Integration.PublishConflictFiles = nil
	wm.Log(core.LogLevelInfo, "forward_merge_succeeded command=%s", commandID)
	return nil
}

// recordForwardMergeConflict captures the conflict file set produced by a
// failed `git merge` of baseBranch into integration and persists it on
// `state` so the publish_conflict planner signal can be emitted on the next
// scan.
//
// Critical invariants preserved from the inline implementation:
//   - The merge is NOT aborted. Conflict markers must remain in the
//     integration worktree so the Planner-dispatched resolution worker
//     (--run-on-integration) can resolve them in place. See
//     templates/instructions/planner.md publish_conflict handler.
//   - PublishConflictSignaled is reset to false only when the conflict
//     file set has changed compared to the prior scan, so re-entry after
//     an unresolved conflict does not re-emit the signal on every cycle.
func (wm *Manager) recordForwardMergeConflict(
	state *model.WorktreeCommandState,
	commandID, integrationPath, baseBranch, now string,
) error {
	conflictFiles, cfErr := wm.getConflictFilesInDir(integrationPath)
	if cfErr != nil {
		wm.Log(core.LogLevelWarn, "forward_merge_conflict_files command=%s error=%v", commandID, cfErr)
	}

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
//     the conflict; the daemon staged and created the merge commit). Caller returns
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
		conflictFiles, cfErr := wm.getConflictFilesInDir(integrationPath)
		if cfErr != nil {
			wm.Log(core.LogLevelWarn, "forward_merge_reentry_conflict_files command=%s error=%v", commandID, cfErr)
		}
		staged, stageErr := wm.stageResolvedForwardMergeFiles(integrationPath, conflictFiles)
		if stageErr != nil {
			return false, stageErr
		}
		if staged {
			hasConflict, probeErr = wm.hasUnmergedFiles(integrationPath)
			if probeErr != nil {
				return false, fmt.Errorf("forward-merge re-entry probe after daemon staging: %w", probeErr)
			}
			if !hasConflict {
				goto finalizeMerge
			}
			conflictFiles, cfErr = wm.getConflictFilesInDir(integrationPath)
			if cfErr != nil {
				wm.Log(core.LogLevelWarn, "forward_merge_reentry_conflict_files_after_stage command=%s error=%v", commandID, cfErr)
			}
		}
		// Worker has not yet resolved the conflict. Return the conflict error
		// WITHOUT clearing PublishConflictSignaled: the signal was already
		// emitted on the first round and the Planner has a task in flight.
		// Re-emitting would spam the signal queue on every scan.
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

finalizeMerge:
	// No unmerged entries but MERGE_HEAD still exists → the daemon can finalize
	// the merge commit so the integration branch absorbs base.
	if commitErr := wm.gitRunInDir(integrationPath, "commit", "--no-edit"); commitErr != nil {
		// A `git commit --no-edit` during an in-flight merge can fail if the
		// resolution produced no content changes. In that case, allow-empty
		// finalizes the merge without producing a content-changing commit.
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

func (wm *Manager) stageResolvedForwardMergeFiles(integrationPath string, conflictFiles []string) (bool, error) {
	if len(conflictFiles) == 0 {
		return false, nil
	}
	if err := ensureWithinProjectRoot(wm.projectRoot, integrationPath); err != nil {
		return false, fmt.Errorf("forward-merge daemon-stage path guard: %w", err)
	}
	for _, name := range conflictFiles {
		if name == "" || filepath.IsAbs(name) || strings.HasPrefix(filepath.Clean(name), "..") {
			return false, fmt.Errorf("forward-merge daemon-stage invalid conflict path %q", name)
		}
		path := filepath.Join(integrationPath, name)
		// name passed the path-traversal guard above (no absolute paths,
		// no ".." prefix), and integrationPath is the controlled
		// integration-worktree root, so gosec G304 (file inclusion) does
		// not apply.
		data, err := os.ReadFile(path) //nolint:gosec // name guarded above; path rooted at integration worktree
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("read resolved conflict file %s: %w", name, err)
		}
		if bytesContainConflictMarkers(data) {
			return false, nil
		}
	}
	args := append([]string{"add", "--"}, conflictFiles...)
	if err := wm.gitRunInDir(integrationPath, args...); err != nil {
		return false, fmt.Errorf("daemon stage publish conflict resolution: %w", err)
	}
	wm.Log(core.LogLevelInfo, "forward_merge_reentry_daemon_staged files=%v", conflictFiles)
	return true, nil
}

func bytesContainConflictMarkers(data []byte) bool {
	s := string(data)
	if strings.Contains(s, "<<<<<<<") ||
		strings.Contains(s, "\n=======") ||
		strings.Contains(s, "\n>>>>>>>") {
		return true
	}
	// Pathological edge case: a file that opens with the divider or closing
	// marker on the very first byte has no leading newline, so the "\n..."
	// substrings above miss it. Cover that case explicitly.
	if bytes.HasPrefix(data, []byte("=======")) ||
		bytes.HasPrefix(data, []byte(">>>>>>>")) {
		return true
	}
	return false
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
