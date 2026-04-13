package worktree

import (
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

// MergeToIntegration merges worker branches into the integration branch.
// Returns any merge conflicts encountered. Workers are merged in deterministic order.
// All merge operations happen in the integration worktree (H3: projectRoot HEAD is never changed).
// workerPurposes maps workerID to the task purpose for descriptive commit messages.
func (wm *Manager) MergeToIntegration(commandID string, workerIDs []string, workerPurposes map[string]string) ([]model.MergeConflict, error) {
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
	// A dirty worktree (e.g., from incomplete merge abort) would cause unpredictable merge results.
	// Persist IntegrationStatusFailed to prevent stale "merged" status from triggering publish.
	dirtyOut, dirtyErr := wm.gitOutputInDir(integrationPath, "status", "--porcelain")
	if dirtyErr != nil {
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if tErr := wm.recordMergeFailure(state, "status_check_failed", now); tErr != nil {
			return nil, fmt.Errorf("check integration worktree status: %w (transition error: %s)", dirtyErr, tErr.Error())
		}
		state.UpdatedAt = now
		if saveErr := wm.saveState(commandID, state); saveErr != nil {
			wm.Log(core.LogLevelWarn, "save_state_failed command=%s error=%v", commandID, saveErr)
		}
		return nil, fmt.Errorf("check integration worktree status: %w", dirtyErr)
	}
	if strings.TrimSpace(dirtyOut) != "" {
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if tErr := wm.recordMergeFailure(state, "dirty_worktree", now); tErr != nil {
			return nil, fmt.Errorf("integration worktree has uncommitted changes (transition error: %s)", tErr.Error())
		}
		state.UpdatedAt = now
		if saveErr := wm.saveState(commandID, state); saveErr != nil {
			wm.Log(core.LogLevelWarn, "save_state_failed command=%s error=%v", commandID, saveErr)
		}
		return nil, fmt.Errorf("integration worktree has uncommitted changes; aborting merge to prevent corruption")
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
		ws := wm.findWorker(state, workerID)
		if ws == nil {
			continue
		}

		// Skip workers already integrated — avoids redundant re-merge on
		// partial_merge/conflict recovery where some workers succeeded earlier.
		if ws.Status == model.WorktreeStatusIntegrated {
			wm.Log(core.LogLevelDebug, "skip_already_integrated command=%s worker=%s", commandID, workerID)
			mergedCount++ // count as merged for final status determination
			continue
		}

		// Skip workers in conflict or resolving state. These must go through
		// the resolution pipeline (DispatchConflictResolution → resume-merge)
		// before being re-merged. Attempting to re-merge a conflict worker
		// causes invalid_worktree_transition (conflict→conflict is not valid).
		if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
			wm.Log(core.LogLevelInfo, "skip_conflict_resolving_worker command=%s worker=%s status=%s",
				commandID, workerID, ws.Status)
			conflictSkippedCount++
			continue
		}

		// Check if worker branch has commits beyond base
		logOut, err := wm.gitOutputWithRetry(integrationPath, 2, "log", "--oneline",
			fmt.Sprintf("%s..%s", state.Integration.BaseSHA, ws.Branch))
		if err != nil {
			wm.Log(core.LogLevelWarn, "merge_log_check command=%s worker=%s error=%v", commandID, workerID, err)
			continue
		}
		if strings.TrimSpace(logOut) == "" {
			wm.Log(core.LogLevelDebug, "no_commits_to_merge command=%s worker=%s", commandID, workerID)
			continue
		}

		// Record per-worker pre-merge HEAD so abort recovery resets only this merge
		perWorkerPreMergeHEAD, err := wm.gitOutputWithRetry(integrationPath, 2, "rev-parse", "HEAD")
		if err != nil {
			wm.Log(core.LogLevelWarn, "merge_pre_head_failed command=%s worker=%s error=%v", commandID, workerID, err)
			continue
		}
		perWorkerPreMergeHEAD = strings.TrimSpace(perWorkerPreMergeHEAD)
		if err := validateSHA(perWorkerPreMergeHEAD); err != nil {
			wm.Log(core.LogLevelWarn, "merge_invalid_pre_head command=%s worker=%s error=%v", commandID, workerID, err)
			continue
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
				// Collect conflict detail refs (base/ours/theirs) before aborting
				conflictFiles, _ := wm.getConflictFilesInDir(integrationPath)
				mc := model.MergeConflict{
					WorkerID:      workerID,
					ConflictFiles: conflictFiles,
					Message:       fmt.Sprintf("merge conflict: %s → integration", workerID),
				}
				// Collect per-file base/ours/theirs refs via rev-parse of index stages
				for _, cf := range conflictFiles {
					baseRef, err1 := wm.gitOutputWithRetry(integrationPath, 1, "rev-parse", ":1:"+cf)
					oursRef, err2 := wm.gitOutputWithRetry(integrationPath, 1, "rev-parse", ":2:"+cf)
					theirsRef, err3 := wm.gitOutputWithRetry(integrationPath, 1, "rev-parse", ":3:"+cf)
					if err1 == nil && err2 == nil && err3 == nil {
						// Use refs from first conflict file as representative
						mc.BaseRef = strings.TrimSpace(baseRef)
						mc.OursRef = strings.TrimSpace(oursRef)
						mc.TheirsRef = strings.TrimSpace(theirsRef)
						break
					}
					// Binary files or missing stages — continue to next file
				}
				conflicts = append(conflicts, mc)

				if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
					wm.Log(core.LogLevelWarn, "merge_abort_failed command=%s worker=%s error=%v",
						commandID, workerID, abortErr)
					// Fallback recovery: reset --hard + clean + verify (per-worker HEAD)
					if recoveryErr := wm.recoverWorktreeAfterMerge(integrationPath, perWorkerPreMergeHEAD, commandID, workerID); recoveryErr != nil {
						if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); tErr != nil {
							wm.Log(core.LogLevelWarn, "merge_recovery_conflict_transition command=%s worker=%s error=%v", commandID, workerID, tErr)
						}
						if tErr := wm.recordMergeFailure(state, "abort_recover_failed_conflict", now); tErr != nil {
							wm.Log(core.LogLevelWarn, "merge_recovery_fail_transition command=%s error=%v", commandID, tErr)
						}
						state.UpdatedAt = now
						_ = wm.saveState(commandID, state)
						return conflicts, fmt.Errorf("worktree stuck after merge abort failure for worker %s: %w", workerID, recoveryErr)
					}
				}

				if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); tErr != nil {
					wm.Log(core.LogLevelWarn, "merge_conflict_transition command=%s worker=%s error=%v",
						commandID, workerID, tErr)
				}
				wm.Log(core.LogLevelWarn, "merge_conflict command=%s worker=%s files=%v",
					commandID, workerID, conflictFiles)
				continue
			}

			// Non-conflict error: classify as transient or permanent
			errClass := classifyGitError(err)

			if abortErr := wm.gitRunInDir(integrationPath, "merge", "--abort"); abortErr != nil {
				wm.Log(core.LogLevelWarn, "merge_abort_failed command=%s worker=%s error=%v",
					commandID, workerID, abortErr)
				if recoveryErr := wm.recoverWorktreeAfterMerge(integrationPath, perWorkerPreMergeHEAD, commandID, workerID); recoveryErr != nil {
					if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
						wm.Log(core.LogLevelWarn, "merge_recovery_worker_fail_transition command=%s worker=%s error=%v", commandID, workerID, tErr)
					}
					if tErr := wm.recordMergeFailure(state, "abort_recover_failed_nonconflict", now); tErr != nil {
						wm.Log(core.LogLevelWarn, "merge_recovery_fail_transition command=%s error=%v", commandID, tErr)
					}
					state.UpdatedAt = now
					_ = wm.saveState(commandID, state)
					return conflicts, fmt.Errorf("worktree stuck after merge abort failure for worker %s: %w", workerID, recoveryErr)
				}
			}

			if errClass == gitErrorTransient {
				// Transient error: skip this worker, continue loop
				if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusFailed, now); tErr != nil {
					wm.Log(core.LogLevelWarn, "merge_transient_fail_transition command=%s worker=%s error=%v",
						commandID, workerID, tErr)
				}
				skippedCount++
				wm.Log(core.LogLevelWarn, "merge_transient_error_skip command=%s worker=%s error=%v",
					commandID, workerID, err)
				continue
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
				commandID, workerID, err)

			if saveErr := wm.saveState(commandID, state); saveErr != nil {
				return conflicts, fmt.Errorf("save state after non-conflict merge error: %w", saveErr)
			}
			return conflicts, fmt.Errorf("non-conflict merge error for worker %s: %w", workerID, err)
		}

		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusIntegrated, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "merge_integrated_transition command=%s worker=%s error=%v",
				commandID, workerID, tErr)
		}
		mergedCount++
		wm.Log(core.LogLevelInfo, "worker_merged command=%s worker=%s", commandID, workerID)
	}

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
		wm.Log(core.LogLevelInfo, "no_commits_to_merge command=%s workers=%d", commandID, len(sorted))
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
			wm.Log(core.LogLevelWarn, "sync_skip_conflict command=%s worker=%s status=%s",
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
			if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusPublishFailed, now); tErr != nil {
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
	mergeMsg := buildPublishMessage(publishMessage, baseBranch)
	if err := wm.gitRunInDir(integrationPath, "merge", "--no-ff", "-m", mergeMsg, state.Integration.Branch); err != nil {
		if tErr := wm.setIntegrationStatus(state, model.IntegrationStatusConflict, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "publish_conflict_transition command=%s error=%v", commandID, tErr)
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
	}

	if err := wm.setIntegrationStatus(state, model.IntegrationStatusPublished, now); err != nil {
		return err
	}
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
