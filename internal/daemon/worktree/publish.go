package worktree

// Publish-to-base pipeline extracted from merge_publish.go (F-040 step 3
// physical file split). PublishToBase is the only public entry point; the
// rest are step helpers that exist as named functions so the publish
// orchestration in PublishToBase reads top-down.
//
// Failure-tracking helpers (recordPublishFailure / recordPublishTerminalFailure)
// and merge-side counterparts stay in merge_publish.go because they are shared
// with the worker→integration merge pipeline.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

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

	mergeSHA, err = wm.mergeIntegrationIntoPublishTemp(state, commandID, integrationPath, baseBranch, publishMessage, now)
	if err != nil {
		return "", false, err
	}

	baseBranchCheckedOut, err = wm.fastForwardBaseBranchRef(state, commandID, integrationPath, baseBranch, baseSHA, mergeSHA)
	if err != nil {
		return "", false, err
	}

	// Clean up temp branch (defer is the safety net, but explicit cleanup avoids
	// leaving the branch around until function exit on the success path).
	wm.deleteTempBranch(tempBranch)
	tempBranchCleaned = true
	return mergeSHA, baseBranchCheckedOut, nil
}

// mergeIntegrationIntoPublishTemp merges the integration branch into the
// already-checked-out temp branch and returns the resulting merge commit
// SHA. On a publish-merge conflict it transitions integration into the
// publish_failed bookkeeping path before returning the wrapped error so the
// caller can fail fast. F-040 step 2 helper.
func (wm *Manager) mergeIntegrationIntoPublishTemp(
	state *model.WorktreeCommandState,
	commandID, integrationPath, baseBranch, publishMessage, now string,
) (string, error) {
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
		// Temp branch cleanup is handled by the caller's defer.
		if checkoutErr := wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch); checkoutErr != nil {
			wm.Log(core.LogLevelWarn, "publish_checkout_failed command=%s branch=%s error=%v", commandID, state.Integration.Branch, checkoutErr)
			return "", errors.Join(
				fmt.Errorf("merge integration into %s: %w", baseBranch, err),
				fmt.Errorf("checkout recovery also failed: %w", checkoutErr),
			)
		}
		return "", fmt.Errorf("merge integration into %s: %w", baseBranch, err)
	}

	mergeSHAOut, err := wm.gitOutputInDir(integrationPath, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("get merge commit SHA: %w", err)
	}
	return strings.TrimSpace(mergeSHAOut), nil
}

// fastForwardBaseBranchRef restores the integration branch checkout, runs
// the projectRoot dirty-check (when baseBranch is also checked out there),
// and then performs the CAS `update-ref` that promotes mergeSHA onto
// baseBranch. Returns whether baseBranch is currently checked out in
// projectRoot, plus any error. F-040 step 2 helper.
//
// Critical invariants preserved verbatim from the inline implementation:
//   - The dirty check runs BEFORE update-ref so a dirty projectRoot is
//     surfaced as errPublishDirtyRoot rather than being clobbered by the
//     ref move (PublishToBase's deferred handler fast-paths this to
//     quarantine instead of burning the retry budget).
//   - update-ref uses CAS (mergeSHA, baseSHA) so a concurrent baseBranch
//     mutation aborts the publish atomically.
func (wm *Manager) fastForwardBaseBranchRef(
	state *model.WorktreeCommandState,
	commandID, integrationPath, baseBranch, baseSHA, mergeSHA string,
) (bool, error) {
	if checkoutErr := wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch); checkoutErr != nil {
		wm.Log(core.LogLevelWarn, "publish_restore_checkout_failed command=%s branch=%s error=%v",
			commandID, state.Integration.Branch, checkoutErr)
		return false, fmt.Errorf("restore integration branch checkout after publish: %w", checkoutErr)
	}

	// Determine whether baseBranch is checked out in projectRoot — if so we
	// must sync the working tree after update-ref. We check BEFORE update-ref
	// so a dirty projectRoot can be surfaced as a sentinel error.
	currentBranch, _ := wm.gitOutput("symbolic-ref", "--short", "HEAD")
	baseBranchCheckedOut := strings.TrimSpace(currentBranch) == baseBranch

	if baseBranchCheckedOut {
		statusOut, err := wm.gitOutput("status", "--porcelain", "--untracked-files=no")
		if err != nil {
			return false, fmt.Errorf("publish dirty check failed: %w", err)
		}
		if strings.TrimSpace(statusOut) != "" {
			return false, errPublishDirtyRoot
		}
	}

	if err := wm.gitRun("update-ref", fmt.Sprintf("refs/heads/%s", baseBranch), mergeSHA, baseSHA); err != nil {
		return false, fmt.Errorf("update base branch ref (CAS failed — branch may have been modified concurrently): %w", err)
	}
	return baseBranchCheckedOut, nil
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
