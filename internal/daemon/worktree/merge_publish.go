package worktree

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// MergeToIntegration merges worker branches into the integration branch.
// Returns any merge conflicts encountered. Workers are merged in deterministic order.
// All merge operations happen in the integration worktree (H3: projectRoot HEAD is never changed).
func (wm *Manager) MergeToIntegration(commandID string, workerIDs []string) ([]model.MergeConflict, error) {
	if err := validateIDs(commandID, workerIDs...); err != nil {
		return nil, err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	integrationPath := wm.integrationWorktreePath(commandID)

	// Guard: ensure integration worktree is clean before merging.
	// A dirty worktree (e.g., from incomplete merge abort) would cause unpredictable merge results.
	// Persist IntegrationStatusFailed to prevent stale "merged" status from triggering publish.
	dirtyOut, dirtyErr := wm.gitOutputInDir(integrationPath, "status", "--porcelain")
	if dirtyErr != nil {
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); tErr != nil {
			return nil, fmt.Errorf("check integration worktree status: %w (transition error: %v)", dirtyErr, tErr)
		}
		state.UpdatedAt = now
		_ = wm.saveState(commandID, state)
		return nil, fmt.Errorf("check integration worktree status: %w", dirtyErr)
	}
	if strings.TrimSpace(dirtyOut) != "" {
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); tErr != nil {
			return nil, fmt.Errorf("integration worktree has uncommitted changes (transition error: %v)", tErr)
		}
		state.UpdatedAt = now
		_ = wm.saveState(commandID, state)
		return nil, fmt.Errorf("integration worktree has uncommitted changes; aborting merge to prevent corruption")
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.setIntegrationStatus(state, model.IntegrationStatusMerging, now); err != nil {
		return nil, err
	}
	state.UpdatedAt = now

	// Save pre-merge HEAD so we can rollback on failure
	preMergeHEAD, err := wm.gitOutputInDir(integrationPath, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("save pre-merge HEAD: %w", err)
	}
	preMergeHEAD = strings.TrimSpace(preMergeHEAD)
	if err := validateSHA(preMergeHEAD); err != nil {
		return nil, fmt.Errorf("invalid pre-merge HEAD: %w", err)
	}

	// Sort worker IDs for deterministic merge order
	sorted := make([]string, len(workerIDs))
	copy(sorted, workerIDs)
	sort.Strings(sorted)

	var conflicts []model.MergeConflict
	// Track workers whose status was changed to "integrated" so we can
	// restore their previous status if a rollback occurs.
	type preMergeInfo struct {
		ws             *model.WorktreeState
		previousStatus model.WorktreeStatus
	}
	var mergedWorkers []preMergeInfo

	for _, workerID := range sorted {
		ws := wm.findWorker(state, workerID)
		if ws == nil {
			continue
		}

		// Check if worker branch has commits beyond base
		logOut, err := wm.gitOutput("log", "--oneline",
			fmt.Sprintf("%s..%s", state.Integration.BaseSHA, ws.Branch))
		if err != nil {
			wm.log(core.LogLevelWarn, "merge_log_check command=%s worker=%s error=%v", commandID, workerID, err)
			continue
		}
		if strings.TrimSpace(logOut) == "" {
			wm.log(core.LogLevelDebug, "no_commits_to_merge command=%s worker=%s", commandID, workerID)
			continue
		}

		// Merge worker branch in integration worktree (not projectRoot)
		strategy := wm.config.EffectiveMergeStrategy()
		mergeMsg := fmt.Sprintf("[maestro] merge %s into integration for %s", workerID, commandID)

		err = wm.gitRunInDir(integrationPath, "merge", "--no-ff", "-s", strategy, "-m", mergeMsg, ws.Branch)
		if err != nil {
			// Classify error: check for unmerged index entries to distinguish
			// true merge conflicts from fatal git errors (bad ref, corrupt repo, etc.)
			hasConflict, probeErr := wm.hasUnmergedFiles(integrationPath)
			if probeErr != nil {
				// Probe failed — can't classify reliably. Treat as non-conflict (fail-safe).
				wm.log(core.LogLevelWarn, "merge_probe_failed command=%s worker=%s probe_error=%v merge_error=%v",
					commandID, workerID, probeErr, err)
			}

			if hasConflict {
				// True merge conflict: unmerged entries exist
				conflictFiles, _ := wm.getConflictFilesInDir(integrationPath)
				conflicts = append(conflicts, model.MergeConflict{
					WorkerID:      workerID,
					ConflictFiles: conflictFiles,
					Message:       fmt.Sprintf("merge conflict: %s → integration", workerID),
				})

				if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
					wm.log(core.LogLevelWarn, "merge_abort_failed command=%s worker=%s error=%v",
						commandID, workerID, abortErr)
					// Fallback recovery: reset --hard + clean + verify
					if recoveryErr := wm.recoverWorktreeAfterMerge(integrationPath, preMergeHEAD, commandID, workerID); recoveryErr != nil {
						// Worktree is stuck — set worker to conflict, restore merged workers, fail
						if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); tErr != nil {
							wm.log(core.LogLevelWarn, "merge_recovery_conflict_transition command=%s worker=%s error=%v", commandID, workerID, tErr)
						}
						if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); tErr != nil {
							wm.log(core.LogLevelWarn, "merge_recovery_fail_transition command=%s error=%v", commandID, tErr)
						}
						for _, mi := range mergedWorkers {
							mi.ws.Status = mi.previousStatus
							mi.ws.UpdatedAt = now
						}
						state.UpdatedAt = now
						_ = wm.saveState(commandID, state)
						return conflicts, fmt.Errorf("worktree stuck after merge abort failure for worker %s: %w", workerID, recoveryErr)
					}
				}

				if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); tErr != nil {
					wm.log(core.LogLevelWarn, "merge_conflict_transition command=%s worker=%s error=%v",
						commandID, workerID, tErr)
				}
				wm.log(core.LogLevelWarn, "merge_conflict command=%s worker=%s files=%v",
					commandID, workerID, conflictFiles)
				continue
			}

			// Non-conflict error (or probe failure): fatal git error, bad ref, infrastructure issue.
			// Halt the merge loop — this likely indicates a repo-level problem.
			if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
				wm.log(core.LogLevelWarn, "merge_abort_failed command=%s worker=%s error=%v",
					commandID, workerID, abortErr)
				// Fallback recovery: reset --hard + clean + verify
				if recoveryErr := wm.recoverWorktreeAfterMerge(integrationPath, preMergeHEAD, commandID, workerID); recoveryErr != nil {
					// Worktree is stuck — set worker and integration status, restore merged workers, return
					if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
						wm.log(core.LogLevelWarn, "merge_recovery_worker_fail_transition command=%s worker=%s error=%v", commandID, workerID, tErr)
					}
					if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); tErr != nil {
						wm.log(core.LogLevelWarn, "merge_recovery_fail_transition command=%s error=%v", commandID, tErr)
					}
					for _, mi := range mergedWorkers {
						mi.ws.Status = mi.previousStatus
						mi.ws.UpdatedAt = now
					}
					state.UpdatedAt = now
					_ = wm.saveState(commandID, state)
					return conflicts, fmt.Errorf("worktree stuck after merge abort failure for worker %s: %w", workerID, recoveryErr)
				}
			}

			if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
				wm.log(core.LogLevelWarn, "merge_fail_transition command=%s worker=%s error=%v",
					commandID, workerID, tErr)
			}
			if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); tErr != nil {
				wm.log(core.LogLevelWarn, "merge_integration_fail_transition command=%s error=%v",
					commandID, tErr)
			}
			state.UpdatedAt = now

			wm.log(core.LogLevelError, "merge_non_conflict_error command=%s worker=%s error=%v",
				commandID, workerID, err)

			// Rollback integration branch to pre-merge HEAD
			if resetErr := wm.gitRunInDir(integrationPath, "reset", "--hard", preMergeHEAD); resetErr != nil {
				wm.log(core.LogLevelError, "merge_rollback_failed command=%s pre_merge_head=%s error=%v",
					commandID, preMergeHEAD, resetErr)
			} else {
				wm.log(core.LogLevelInfo, "merge_rollback command=%s pre_merge_head=%s reason=non_conflict_error",
					commandID, preMergeHEAD)
			}
			// Restore worker statuses that were changed to "integrated"
			for _, mi := range mergedWorkers {
				mi.ws.Status = mi.previousStatus
				mi.ws.UpdatedAt = now
				wm.log(core.LogLevelInfo, "merge_rollback_worker_status command=%s worker=%s restored=%s",
					commandID, mi.ws.WorkerID, mi.previousStatus)
			}

			if saveErr := wm.saveState(commandID, state); saveErr != nil {
				return conflicts, fmt.Errorf("save state after non-conflict merge error: %w", saveErr)
			}
			return conflicts, fmt.Errorf("non-conflict merge error for worker %s: %w", workerID, err)
		}

		prevStatus := ws.Status
		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusIntegrated, now); tErr != nil {
			wm.log(core.LogLevelWarn, "merge_integrated_transition command=%s worker=%s error=%v",
				commandID, workerID, tErr)
		} else {
			mergedWorkers = append(mergedWorkers, preMergeInfo{ws: ws, previousStatus: prevStatus})
		}
		wm.log(core.LogLevelInfo, "worker_merged command=%s worker=%s", commandID, workerID)
	}

	if len(conflicts) > 0 {
		// Rollback integration branch to pre-merge HEAD
		if resetErr := wm.gitRunInDir(integrationPath, "reset", "--hard", preMergeHEAD); resetErr != nil {
			wm.log(core.LogLevelError, "merge_rollback_failed command=%s pre_merge_head=%s error=%v",
				commandID, preMergeHEAD, resetErr)
		} else {
			wm.log(core.LogLevelInfo, "merge_rollback command=%s pre_merge_head=%s reason=conflict",
				commandID, preMergeHEAD)
		}
		// Restore worker statuses that were changed to "integrated"
		for _, mi := range mergedWorkers {
			mi.ws.Status = mi.previousStatus
			mi.ws.UpdatedAt = now
			wm.log(core.LogLevelInfo, "merge_rollback_worker_status command=%s worker=%s restored=%s",
				commandID, mi.ws.WorkerID, mi.previousStatus)
		}

		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusConflict, now); tErr != nil {
			wm.log(core.LogLevelWarn, "merge_conflict_integration_transition command=%s error=%v", commandID, tErr)
		}
	} else {
		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusMerged, now); tErr != nil {
			wm.log(core.LogLevelWarn, "merge_merged_integration_transition command=%s error=%v", commandID, tErr)
		}
	}
	state.UpdatedAt = now

	if err := wm.saveState(commandID, state); err != nil {
		return conflicts, fmt.Errorf("save state: %w", err)
	}

	return conflicts, nil
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

	for _, workerID := range workerIDs {
		ws := wm.findWorker(state, workerID)
		if ws == nil {
			continue
		}

		// M2: Skip conflict-state workers
		if ws.Status == model.WorktreeStatusConflict {
			wm.log(core.LogLevelWarn, "sync_skip_conflict command=%s worker=%s status=%s",
				commandID, workerID, ws.Status)
			continue
		}

		// M3: Skip dirty worktrees (uncommitted changes)
		statusOut, statusErr := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
		if statusErr == nil && strings.TrimSpace(statusOut) != "" {
			wm.log(core.LogLevelWarn, "sync_skip_dirty command=%s worker=%s",
				commandID, workerID)
			continue
		}

		// Merge integration branch into worker worktree
		err := wm.gitRunInDir(ws.Path, "merge", state.Integration.Branch,
			"-m", fmt.Sprintf("[maestro] sync integration into %s", workerID))
		if err != nil {
			wm.log(core.LogLevelWarn, "sync_from_integration command=%s worker=%s error=%v",
				commandID, workerID, err)
			// Abort on conflict
			_ = wm.gitRunInDir(ws.Path, "merge", "--abort")
			continue
		}

		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusActive, now); tErr != nil {
			wm.log(core.LogLevelWarn, "sync_active_transition command=%s worker=%s error=%v",
				commandID, workerID, tErr)
		}
	}

	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	return nil
}

// PublishToBase merges the integration branch into the base branch.
// Uses a temporary branch in the integration worktree to avoid changing projectRoot HEAD (H3).
func (wm *Manager) PublishToBase(commandID string) error {
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

	baseBranch := wm.config.EffectiveBaseBranch()
	integrationPath := wm.integrationWorktreePath(commandID)

	// Create a temporary branch from baseBranch for the merge.
	// We cannot checkout baseBranch directly because it may be checked out in projectRoot.
	tempBranch := fmt.Sprintf("maestro/%s/_publish", commandID)
	baseSHA, err := wm.gitOutput("rev-parse", baseBranch)
	if err != nil {
		return fmt.Errorf("get base SHA for publish: %w", err)
	}
	baseSHA = strings.TrimSpace(baseSHA)
	if err := validateSHA(baseSHA); err != nil {
		return fmt.Errorf("base SHA for publish: %w", err)
	}

	if err := wm.gitRun("branch", tempBranch, baseSHA); err != nil {
		return fmt.Errorf("create temp publish branch: %w", err)
	}

	// Checkout temp branch in integration worktree
	if err := wm.gitRunInDir(integrationPath, "checkout", tempBranch); err != nil {
		_ = wm.gitRun("branch", "-D", tempBranch)
		return fmt.Errorf("checkout temp publish branch: %w", err)
	}

	// Merge integration branch into temp branch (at baseBranch's position)
	mergeMsg := fmt.Sprintf("[maestro] publish %s integration to %s", commandID, baseBranch)
	if err := wm.gitRunInDir(integrationPath, "merge", "--no-ff", "-m", mergeMsg, state.Integration.Branch); err != nil {
		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusConflict, now); tErr != nil {
			wm.log(core.LogLevelWarn, "publish_conflict_transition command=%s error=%v", commandID, tErr)
		}
		state.UpdatedAt = now
		_ = wm.saveState(commandID, state)
		_ = wm.gitRunInDir(integrationPath, "merge", "--abort")
		_ = wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch)
		_ = wm.gitRun("branch", "-D", tempBranch)
		return fmt.Errorf("merge integration into %s: %w", baseBranch, err)
	}

	// Get the merge commit SHA
	mergeSHA, err := wm.gitOutputInDir(integrationPath, "rev-parse", "HEAD")
	if err != nil {
		_ = wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch)
		_ = wm.gitRun("branch", "-D", tempBranch)
		return fmt.Errorf("get merge commit SHA: %w", err)
	}
	mergeSHA = strings.TrimSpace(mergeSHA)

	// Restore integration branch checkout before updating refs
	_ = wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch)

	// Check if baseBranch is currently checked out in projectRoot BEFORE updating the ref.
	// We need this to know whether to sync the working tree afterwards.
	currentBranch, _ := wm.gitOutput("symbolic-ref", "--short", "HEAD")
	baseBranchCheckedOut := strings.TrimSpace(currentBranch) == baseBranch

	// If baseBranch is checked out, we'll need to reset the working tree after update-ref.
	// Check for uncommitted changes BEFORE update-ref to avoid data loss.
	if baseBranchCheckedOut {
		statusOut, err := wm.gitOutput("status", "--porcelain")
		if err != nil {
			_ = wm.gitRun("branch", "-D", tempBranch)
			return fmt.Errorf("publish dirty check failed: %w", err)
		}
		if strings.TrimSpace(statusOut) != "" {
			_ = wm.gitRun("branch", "-D", tempBranch)
			return fmt.Errorf("publish aborted: projectRoot has uncommitted changes that would be lost by reset; please commit or stash them first")
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
		_ = wm.gitRun("branch", "-D", tempBranch)
		return fmt.Errorf("update base branch ref (CAS failed — branch may have been modified concurrently): %w", err)
	}

	// Clean up temp branch
	_ = wm.gitRun("branch", "-D", tempBranch)

	// If baseBranch is checked out in projectRoot, sync the working tree and index
	// to match the updated ref. Without this, the working tree would remain at the
	// old commit state, appearing as a staged revert of all published changes.
	if baseBranchCheckedOut {
		// Uncommitted changes were already checked before update-ref above.
		// Use git reset --hard to sync index + working tree to the new HEAD.
		if resetErr := wm.gitRun("reset", "--hard", "HEAD"); resetErr != nil {
			wm.log(core.LogLevelWarn, "publish_reset_working_tree command=%s error=%v", commandID, resetErr)
			// Non-fatal: the ref update succeeded, so the branch is at the right commit.
			// The working tree mismatch can be fixed manually with `git reset --hard HEAD`.
		}
	}

	if err := wm.setIntegrationStatus(state, model.IntegrationStatusPublished, now); err != nil {
		return err
	}
	state.UpdatedAt = now

	// Mark all workers as published
	for i := range state.Workers {
		if tErr := wm.setWorkerStatus(&state.Workers[i], model.WorktreeStatusPublished, now); tErr != nil {
			wm.log(core.LogLevelWarn, "publish_worker_transition command=%s worker=%s error=%v",
				commandID, state.Workers[i].WorkerID, tErr)
		}
	}

	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	wm.log(core.LogLevelInfo, "published_to_base command=%s branch=%s", commandID, baseBranch)
	return nil
}
