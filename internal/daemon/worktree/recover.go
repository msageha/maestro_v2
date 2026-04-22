package worktree

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
	if anyResolved || len(toResolve) == 0 || legacyFallback {
		state.Integration.MergeFailureCount = 0
	} else {
		// All per-worker merge attempts failed — count toward quarantine.
		if err := wm.recordMergeFailure(state, "resume_merge_all_failed", now); err != nil {
			wm.Log(core.LogLevelWarn, "resume_merge_failure_record command=%s error=%v", commandID, err)
		}
	}

	// Determine final integration status based on worker states.
	allIntegrated := true
	for _, ws := range state.Workers {
		switch ws.Status {
		case model.WorktreeStatusIntegrated, model.WorktreeStatusPublished,
			model.WorktreeStatusCleanupDone, model.WorktreeStatusCleanupFailed:
			// done for merge purposes
		default:
			allIntegrated = false
		}
	}

	if allIntegrated && len(toResolve) > 0 {
		// Before flipping integration to Merged, cross-check git reality: every
		// worker branch whose state says "integrated" (or later) must actually
		// be reachable from integration HEAD. Without this, tryMergeWorker's
		// "already merged" early-return (mergeResolvedWorker: empty
		// preMergeHEAD..branch log) or a failed commitResolvedWorkerChanges
		// would still flip integration to Merged while the resolution content
		// never reached the branch — the operator then sees integration state
		// say "merged" while the branch diff is empty (2026-04 audit Bug 2).
		if contentOK, badWorkers := wm.verifyWorkersMerged(ctx, commandID, state); contentOK {
			// All workers are now integrated and merged — transition through merging to merged.
			if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusMerging, now); tErr == nil {
				if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusMerged, now); tErr != nil {
					wm.Log(core.LogLevelWarn, "resume_merge_merged_transition command=%s error=%v", commandID, tErr)
					// Revert to failed so Phase A can retry.
					_ = wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now)
				}
			} else {
				wm.Log(core.LogLevelWarn, "resume_merge_merging_transition command=%s error=%v", commandID, tErr)
			}
		} else {
			wm.Log(core.LogLevelError,
				"resume_merge_content_mismatch command=%s bad_workers=%v (state says integrated but branch not reachable from integration HEAD; forcing status=failed)",
				commandID, badWorkers)
			// Revert the offending workers to Conflict so the resolution
			// pipeline re-attempts on the next scan instead of leaving them
			// wedged in a false "integrated" state that would keep producing
			// the same false-Merged transition.
			for _, wid := range badWorkers {
				for i := range state.Workers {
					if state.Workers[i].WorkerID == wid {
						if tErr := wm.setWorkerStatus(&state.Workers[i], model.WorktreeStatusConflict, now); tErr != nil {
							wm.Log(core.LogLevelWarn, "resume_merge_revert_bad_worker command=%s worker=%s error=%v",
								commandID, wid, tErr)
						}
					}
				}
			}
			// Record as a merge failure so the quarantine counter advances if
			// this keeps happening.
			if tErr := wm.recordMergeFailure(state, "content_mismatch", now); tErr != nil {
				wm.Log(core.LogLevelWarn, "resume_merge_content_mismatch_record command=%s error=%v", commandID, tErr)
			}
		}
	}

	// If not merged (and not quarantined by recordMergeFailure), ensure we
	// end in failed state so Phase A can re-enqueue merge attempts.
	if state.Integration.Status != model.IntegrationStatusMerged &&
		state.Integration.Status != model.IntegrationStatusQuarantined {
		if state.Integration.Status != model.IntegrationStatusFailed {
			if err := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); err != nil {
				return err
			}
		} else {
			state.Integration.UpdatedAt = now
		}
	}

	state.UpdatedAt = now

	wm.Log(core.LogLevelInfo, "resume_merge command=%s prev_status=%s resolved_workers=%d",
		commandID, s, len(toResolve))
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
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

	var bad []string
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
	state *model.WorktreeCommandState,
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
	// the operator can inspect rather than silently promoting the branch
	// (2026-04 audit Bug 2).
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

	// Stage tracked file modifications/deletions (safe: never stages untracked files).
	if err := wm.gitRunInDir(ws.Path, "add", "-u"); err != nil {
		return fmt.Errorf("git add -u in %s: %w", ws.Path, err)
	}
	// Unstage any sensitive tracked files that were staged by git add -u.
	if err := wm.unstageSensitiveFiles(ws.Path); err != nil {
		wm.Log(core.LogLevelWarn, "resolve_unstage_sensitive command=%s worker=%s error=%v",
			commandID, ws.WorkerID, err)
	}
	// Stage untracked files that pass .gitignore and sensitive file filters.
	if err := wm.stageNewFiles(ws.Path); err != nil {
		return fmt.Errorf("stage new files in %s: %w", ws.Path, err)
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
		return fmt.Errorf("conflict probe: %w (merge error: %v)", probeErr, mergeErr)
	}
	if !hasConflict {
		// Non-conflict git error — abort and report.
		_ = wm.gitRunInDir(integrationPath, "merge", "--abort")
		return fmt.Errorf("non-conflict merge error: %w", mergeErr)
	}

	// Real merge conflict — resolve using the worker's committed content.
	conflictFiles, cfErr := wm.getConflictFilesInDir(integrationPath)
	if cfErr != nil || len(conflictFiles) == 0 {
		if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
			if recoveryErr := wm.recoverWorktreeAfterMerge(integrationPath, preMergeHEAD, commandID, ws.WorkerID); recoveryErr != nil {
				return fmt.Errorf("get conflict files: %w; merge abort failed; worktree recovery failed: %v", cfErr, recoveryErr)
			}
		}
		return fmt.Errorf("get conflict files: %w", cfErr)
	}

	for _, cf := range conflictFiles {
		// git checkout <branch> -- <file> updates both working tree and index,
		// replacing the conflicting content with the worker's resolved version.
		if checkoutErr := wm.gitRunInDir(integrationPath, "checkout", ws.Branch, "--", cf); checkoutErr != nil {
			wm.Log(core.LogLevelWarn, "resolve_checkout_failed command=%s worker=%s file=%s error=%v",
				commandID, ws.WorkerID, cf, checkoutErr)
			if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
				if recoveryErr := wm.recoverWorktreeAfterMerge(integrationPath, preMergeHEAD, commandID, ws.WorkerID); recoveryErr != nil {
					return fmt.Errorf("checkout resolved file %s from %s: %w; merge abort failed: %v; worktree recovery failed: %v", cf, ws.Branch, checkoutErr, abortErr, recoveryErr)
				}
			}
			return fmt.Errorf("checkout resolved file %s from %s: %w", cf, ws.Branch, checkoutErr)
		}
	}

	// Complete the merge commit. --no-edit reads the message from .git/MERGE_MSG
	// which was set by the initial merge command.
	if err := wm.gitRunInDir(integrationPath, "commit", "--no-edit"); err != nil {
		if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
			if recoveryErr := wm.recoverWorktreeAfterMerge(integrationPath, preMergeHEAD, commandID, ws.WorkerID); recoveryErr != nil {
				return fmt.Errorf("commit resolved merge: %w; merge abort failed: %v; worktree recovery failed: %v", err, abortErr, recoveryErr)
			}
		}
		return fmt.Errorf("commit resolved merge: %w", err)
	}

	wm.Log(core.LogLevelInfo, "conflict_resolved_merge command=%s worker=%s files=%v",
		commandID, ws.WorkerID, conflictFiles)
	return nil
}

// AutoRecoverAction names the recovery path AutoRecover selected, or
// AutoRecoverNone when no recovery applied.
type AutoRecoverAction string

const (
	// AutoRecoverNone indicates that the integration is not in a state that
	// any idempotent recovery path handles automatically (e.g. already merged,
	// published, quarantined, or in an in-flight transition).
	AutoRecoverNone AutoRecoverAction = ""
	// AutoRecoverResumeMerge indicates that AutoRecover dispatched to
	// ResumeMerge for a conflict / partial_merge / failed-with-workers state.
	AutoRecoverResumeMerge AutoRecoverAction = "resume_merge"
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
