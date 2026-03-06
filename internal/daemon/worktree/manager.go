package worktree

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// Manager manages git worktree lifecycle for Worker isolation.
// All git operations are serialized through this manager (Single-Writer pattern).
type Manager struct {
	maestroDir  string
	projectRoot string
	config      model.WorktreeConfig
	dl          *core.DaemonLogger
	clock       core.Clock
	mu          sync.Mutex // serializes all git operations
}

// NewManager creates a new Manager.
func NewManager(maestroDir string, cfg model.WorktreeConfig, logger *log.Logger, logLevel core.LogLevel) *Manager {
	projectRoot := filepath.Dir(maestroDir)
	return &Manager{
		maestroDir:  maestroDir,
		projectRoot: projectRoot,
		config:      cfg,
		dl:          core.NewDaemonLoggerFromLegacy("worktree_manager", logger, logLevel),
		clock:       core.RealClock{},
	}
}

// CreateForCommand creates worktrees for all workers and an integration branch for a command.
func (wm *Manager) CreateForCommand(commandID string, workerIDs []string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	now := wm.clock.Now().UTC().Format(time.RFC3339)

	// Get base SHA from configured base branch (not HEAD which may be on a different branch)
	baseBranch := wm.config.EffectiveBaseBranch()
	baseSHA, err := wm.gitOutput("rev-parse", baseBranch)
	if err != nil {
		return fmt.Errorf("get base SHA from %s: %w", baseBranch, err)
	}
	baseSHA = strings.TrimSpace(baseSHA)

	// Create integration branch
	integrationBranch := fmt.Sprintf("maestro/%s/integration", commandID)
	if err := wm.gitRun("branch", integrationBranch, baseSHA); err != nil {
		return fmt.Errorf("create integration branch %s: %w", integrationBranch, err)
	}

	// Create integration worktree (H3: all merge/publish ops happen here, not in projectRoot)
	integrationPath := wm.integrationWorktreePath(commandID)
	if err := os.MkdirAll(filepath.Dir(integrationPath), 0755); err != nil {
		_ = wm.gitRun("branch", "-D", integrationBranch)
		return fmt.Errorf("create integration worktree parent dir: %w", err)
	}
	if err := wm.gitRun("worktree", "add", integrationPath, integrationBranch); err != nil {
		_ = wm.gitRun("branch", "-D", integrationBranch)
		return fmt.Errorf("create integration worktree: %w", err)
	}

	state := model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID: commandID,
			Branch:    integrationBranch,
			BaseSHA:   baseSHA,
			Status:    model.IntegrationStatusCreated,
			CreatedAt: now,
			UpdatedAt: now,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	// rollbackCreated cleans up all resources created so far on failure (best-effort).
	type createdWT struct{ path, branch string }
	var createdWorktrees []createdWT
	rollbackCreated := func() {
		for _, wt := range createdWorktrees {
			if rbErr := wm.gitRun("worktree", "remove", "--force", wt.path); rbErr != nil {
				wm.log(core.LogLevelWarn, "rollback_worktree_remove command=%s path=%s error=%v", commandID, wt.path, rbErr)
			}
			_ = wm.gitRun("branch", "-D", wt.branch)
		}
		// Remove integration worktree before branch deletion
		_ = wm.gitRun("worktree", "remove", "--force", integrationPath)
		if rbErr := wm.gitRun("branch", "-D", integrationBranch); rbErr != nil {
			wm.log(core.LogLevelWarn, "rollback_integration_branch command=%s error=%v", commandID, rbErr)
		}
		wtDir := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID)
		_ = os.RemoveAll(wtDir)
		// Remove state file if partially written
		statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
		_ = os.Remove(statePath)
	}

	// Create worker worktrees
	for _, workerID := range workerIDs {
		workerBranch := fmt.Sprintf("maestro/%s/%s", commandID, workerID)
		wtPath := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID, workerID)

		if err := os.MkdirAll(filepath.Dir(wtPath), 0755); err != nil {
			rollbackCreated()
			return fmt.Errorf("create worktree parent dir for %s: %w", workerID, err)
		}

		if err := wm.gitRun("worktree", "add", "-b", workerBranch, wtPath, baseSHA); err != nil {
			rollbackCreated()
			return fmt.Errorf("create worktree for %s: %w", workerID, err)
		}

		createdWorktrees = append(createdWorktrees, createdWT{path: wtPath, branch: workerBranch})
		state.Workers = append(state.Workers, model.WorktreeState{
			CommandID: commandID,
			WorkerID:  workerID,
			Path:      wtPath,
			Branch:    workerBranch,
			BaseSHA:   baseSHA,
			Status:    model.WorktreeStatusCreated,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}

	// Persist state
	if err := wm.saveState(commandID, &state); err != nil {
		rollbackCreated()
		return fmt.Errorf("save worktree state: %w", err)
	}

	wm.log(core.LogLevelInfo, "worktrees_created command=%s workers=%d base=%s",
		commandID, len(workerIDs), baseSHA[:8])
	return nil
}

// EnsureWorkerWorktree lazily creates a worktree for a single worker.
// If the command has no worktree state yet, it creates the integration branch
// and the worker's worktree. If state exists but the worker is missing, it adds
// the worker worktree to the existing command state.
func (wm *Manager) EnsureWorkerWorktree(commandID, workerID string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	now := wm.clock.Now().UTC().Format(time.RFC3339)

	state, err := wm.loadState(commandID)
	if err != nil {
		// No state yet — create everything from scratch
		baseBranch := wm.config.EffectiveBaseBranch()
		baseSHA, err := wm.gitOutput("rev-parse", baseBranch)
		if err != nil {
			return fmt.Errorf("get base SHA from %s: %w", baseBranch, err)
		}
		baseSHA = strings.TrimSpace(baseSHA)

		// Create integration branch
		integrationBranch := fmt.Sprintf("maestro/%s/integration", commandID)
		if err := wm.gitRun("branch", integrationBranch, baseSHA); err != nil {
			return fmt.Errorf("create integration branch: %w", err)
		}

		// Create integration worktree (H3: merge/publish ops happen here)
		integrationPath := wm.integrationWorktreePath(commandID)
		if err := os.MkdirAll(filepath.Dir(integrationPath), 0755); err != nil {
			_ = wm.gitRun("branch", "-D", integrationBranch)
			return fmt.Errorf("create integration worktree parent dir: %w", err)
		}
		if err := wm.gitRun("worktree", "add", integrationPath, integrationBranch); err != nil {
			_ = wm.gitRun("branch", "-D", integrationBranch)
			return fmt.Errorf("create integration worktree: %w", err)
		}

		rollbackIntegration := func() {
			_ = wm.gitRun("worktree", "remove", "--force", integrationPath)
			if rbErr := wm.gitRun("branch", "-D", integrationBranch); rbErr != nil {
				wm.log(core.LogLevelWarn, "rollback_integration_branch command=%s error=%v", commandID, rbErr)
			}
		}

		state = &model.WorktreeCommandState{
			SchemaVersion: 1,
			FileType:      "state_worktree",
			CommandID:     commandID,
			Integration: model.IntegrationState{
				CommandID: commandID,
				Branch:    integrationBranch,
				BaseSHA:   baseSHA,
				Status:    model.IntegrationStatusCreated,
				CreatedAt: now,
				UpdatedAt: now,
			},
			CreatedAt: now,
			UpdatedAt: now,
		}

		// Create the worker worktree; rollback integration on failure
		if err := wm.addWorkerWorktreeUnlocked(state, commandID, workerID, baseSHA, now); err != nil {
			rollbackIntegration()
			return err
		}

		if err := wm.saveState(commandID, state); err != nil {
			// Rollback: remove worker worktree, branch, and integration
			wm.rollbackWorkerWorktree(commandID, state, workerID)
			rollbackIntegration()
			statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
			_ = os.Remove(statePath)
			return fmt.Errorf("save worktree state: %w", err)
		}

		return nil
	}

	// State exists — check if this worker already has a worktree
	for _, ws := range state.Workers {
		if ws.WorkerID == workerID {
			return nil // already exists
		}
	}

	// Save original state for rollback if saveState fails after worktree creation
	origWorkers := make([]model.WorktreeState, len(state.Workers))
	copy(origWorkers, state.Workers)
	origUpdatedAt := state.UpdatedAt

	// Add the worker to existing state
	if err := wm.addWorkerWorktreeUnlocked(state, commandID, workerID, state.Integration.BaseSHA, now); err != nil {
		return err
	}

	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		// Rollback: remove the just-created worker worktree
		wm.rollbackWorkerWorktree(commandID, state, workerID)
		// Restore original state to fix potential partial file write
		state.Workers = origWorkers
		state.UpdatedAt = origUpdatedAt
		_ = wm.saveState(commandID, state)
		return fmt.Errorf("save worktree state: %w", err)
	}
	return nil
}

func (wm *Manager) addWorkerWorktreeUnlocked(state *model.WorktreeCommandState, commandID, workerID, baseSHA, now string) error {
	workerBranch := fmt.Sprintf("maestro/%s/%s", commandID, workerID)
	wtPath := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID, workerID)

	if err := os.MkdirAll(filepath.Dir(wtPath), 0755); err != nil {
		return fmt.Errorf("create worktree parent dir: %w", err)
	}

	if err := wm.gitRun("worktree", "add", "-b", workerBranch, wtPath, baseSHA); err != nil {
		return fmt.Errorf("create worktree for %s: %w", workerID, err)
	}

	state.Workers = append(state.Workers, model.WorktreeState{
		CommandID: commandID,
		WorkerID:  workerID,
		Path:      wtPath,
		Branch:    workerBranch,
		BaseSHA:   baseSHA,
		Status:    model.WorktreeStatusCreated,
		CreatedAt: now,
		UpdatedAt: now,
	})

	wm.log(core.LogLevelInfo, "worker_worktree_created command=%s worker=%s", commandID, workerID)
	return nil
}

// GetWorkerPath returns the worktree path for a specific worker.
func (wm *Manager) GetWorkerPath(commandID, workerID string) (string, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return "", fmt.Errorf("load worktree state: %w", err)
	}

	for _, ws := range state.Workers {
		if ws.WorkerID == workerID {
			return ws.Path, nil
		}
	}
	return "", fmt.Errorf("worktree not found for worker %s in command %s", workerID, commandID)
}

// CommitWorkerChanges commits all changes in a worker's worktree.
// Idempotent: if there are no changes to commit, returns nil.
func (wm *Manager) CommitWorkerChanges(commandID, workerID, message string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	ws := wm.findWorker(state, workerID)
	if ws == nil {
		return fmt.Errorf("worker %s not found in command %s", workerID, commandID)
	}

	// Check if there are changes to commit
	statusOut, err := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status in %s: %w", ws.Path, err)
	}
	if strings.TrimSpace(statusOut) == "" {
		wm.log(core.LogLevelDebug, "no_changes_to_commit command=%s worker=%s", commandID, workerID)
		return nil
	}

	// Stage tracked file modifications/deletions (safe: never stages untracked files)
	if err := wm.gitRunInDir(ws.Path, "add", "-u"); err != nil {
		return fmt.Errorf("git add -u in %s: %w", ws.Path, err)
	}

	// Unstage any sensitive tracked files that were staged by git add -u
	if err := wm.unstageSensitiveFiles(ws.Path); err != nil {
		wm.log(core.LogLevelWarn, "unstage_sensitive_files_error command=%s worker=%s error=%v", commandID, workerID, err)
	}

	// Stage untracked files that pass .gitignore and safety filters
	if err := wm.stageNewFiles(ws.Path); err != nil {
		return fmt.Errorf("stage new files in %s: %w", ws.Path, err)
	}

	// Re-check if there is anything staged after filtering
	stagedOut, err := wm.gitOutputInDir(ws.Path, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return fmt.Errorf("git diff --cached in %s: %w", ws.Path, err)
	}
	if strings.TrimRight(stagedOut, "\x00") == "" {
		wm.log(core.LogLevelDebug, "no_staged_changes_after_filter command=%s worker=%s", commandID, workerID)
		return nil
	}

	// Commit policy checks
	if violations := wm.checkCommitPolicy(ws.Path, message, stagedOut); len(violations) > 0 {
		for _, v := range violations {
			wm.log(core.LogLevelWarn, "commit_policy_violation command=%s worker=%s code=%s msg=%s",
				commandID, workerID, v.Code, v.Message)
		}
		// Reset staged changes so the worktree is left in a clean index state
		_ = wm.gitRunInDir(ws.Path, "reset", "HEAD")
		return fmt.Errorf("commit policy violation [%s]: %s", violations[0].Code, violations[0].Message)
	}

	if err := wm.gitRunInDir(ws.Path, "commit", "-m", message); err != nil {
		return fmt.Errorf("git commit in %s: %w", ws.Path, err)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.setWorkerStatus(ws, model.WorktreeStatusCommitted, now); err != nil {
		return err
	}
	state.UpdatedAt = now

	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	wm.log(core.LogLevelInfo, "worker_committed command=%s worker=%s", commandID, workerID)
	return nil
}

// MergeToIntegration merges worker branches into the integration branch.
// Returns any merge conflicts encountered. Workers are merged in deterministic order.
// All merge operations happen in the integration worktree (H3: projectRoot HEAD is never changed).
func (wm *Manager) MergeToIntegration(commandID string, workerIDs []string) ([]model.MergeConflict, error) {
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

	// Sort worker IDs for deterministic merge order
	sorted := make([]string, len(workerIDs))
	copy(sorted, workerIDs)
	sort.Strings(sorted)

	var conflicts []model.MergeConflict

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

			if saveErr := wm.saveState(commandID, state); saveErr != nil {
				return conflicts, fmt.Errorf("save state after non-conflict merge error: %w", saveErr)
			}
			return conflicts, fmt.Errorf("non-conflict merge error for worker %s: %w", workerID, err)
		}

		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusIntegrated, now); tErr != nil {
			wm.log(core.LogLevelWarn, "merge_integrated_transition command=%s worker=%s error=%v",
				commandID, workerID, tErr)
		}
		wm.log(core.LogLevelInfo, "worker_merged command=%s worker=%s", commandID, workerID)
	}

	if len(conflicts) > 0 {
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

	// Update baseBranch ref to point to the merge commit (no projectRoot HEAD change)
	if err := wm.gitRun("update-ref", fmt.Sprintf("refs/heads/%s", baseBranch), mergeSHA); err != nil {
		_ = wm.gitRun("branch", "-D", tempBranch)
		return fmt.Errorf("update base branch ref: %w", err)
	}

	// Clean up temp branch
	_ = wm.gitRun("branch", "-D", tempBranch)

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

// CleanupCommand removes all worktrees and branches for a command.
func (wm *Manager) CleanupCommand(commandID string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to clean up
		}
		return fmt.Errorf("load state: %w", err)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	var errs []string

	// Remove worker worktrees
	for i := range state.Workers {
		ws := &state.Workers[i]
		if err := wm.gitRun("worktree", "remove", "--force", ws.Path); err != nil {
			errs = append(errs, fmt.Sprintf("remove worktree %s: %v", ws.WorkerID, err))
			if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusCleanupFailed, now); tErr != nil {
				wm.log(core.LogLevelWarn, "cleanup_failed_transition command=%s worker=%s error=%v",
					commandID, ws.WorkerID, tErr)
			}
		} else {
			if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusCleanupDone, now); tErr != nil {
				wm.log(core.LogLevelWarn, "cleanup_done_transition command=%s worker=%s error=%v",
					commandID, ws.WorkerID, tErr)
			}
		}

		// Delete worker branch
		_ = wm.gitRun("branch", "-D", ws.Branch)
	}

	// Remove integration worktree (must be before os.RemoveAll and branch deletion)
	integrationPath := wm.integrationWorktreePath(commandID)
	_ = wm.gitRun("worktree", "remove", "--force", integrationPath)

	// Delete integration branch
	_ = wm.gitRun("branch", "-D", state.Integration.Branch)

	// Remove the worktree directory for this command
	wtDir := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID)
	_ = os.RemoveAll(wtDir)

	// Remove state file so HasWorktrees returns false
	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	_ = os.Remove(statePath)

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %s", strings.Join(errs, "; "))
	}

	wm.log(core.LogLevelInfo, "cleanup_complete command=%s", commandID)
	return nil
}

// GC removes old worktrees that exceed TTL or max_worktrees limit.
func (wm *Manager) GC() error {
	if !wm.config.GC.Enabled {
		return nil
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read worktree state dir: %w", err)
	}

	ttl := time.Duration(wm.config.GC.EffectiveTTLHours()) * time.Hour
	maxWorktrees := wm.config.GC.EffectiveMaxWorktrees()
	now := wm.clock.Now()

	type stateEntry struct {
		commandID string
		createdAt time.Time
		state     *model.WorktreeCommandState
	}

	var allStates []stateEntry

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		state, err := wm.loadStateUnlocked(commandID)
		if err != nil {
			continue
		}

		created, err := time.Parse(time.RFC3339, state.CreatedAt)
		if err != nil {
			continue
		}

		// TTL-based cleanup
		if now.Sub(created) > ttl {
			wm.log(core.LogLevelInfo, "gc_ttl_expired command=%s age=%s", commandID, now.Sub(created))
			if err := wm.cleanupCommandUnlocked(commandID, state); err != nil {
				wm.log(core.LogLevelWarn, "gc_cleanup_failed command=%s error=%v", commandID, err)
			}
			continue
		}

		allStates = append(allStates, stateEntry{commandID: commandID, createdAt: created, state: state})
	}

	// Max worktrees limit: remove oldest first
	if len(allStates) > maxWorktrees {
		sort.Slice(allStates, func(i, j int) bool {
			return allStates[i].createdAt.Before(allStates[j].createdAt)
		})
		for i := 0; i < len(allStates)-maxWorktrees; i++ {
			wm.log(core.LogLevelInfo, "gc_max_exceeded command=%s", allStates[i].commandID)
			if err := wm.cleanupCommandUnlocked(allStates[i].commandID, allStates[i].state); err != nil {
				wm.log(core.LogLevelWarn, "gc_cleanup_failed command=%s error=%v", allStates[i].commandID, err)
			}
		}
	}

	// M4: Health check — cross-reference git worktree list with state files
	gitWorktrees, listErr := wm.listGitWorktreesUnlocked()
	if listErr != nil {
		wm.log(core.LogLevelWarn, "gc_worktree_list error=%v", listErr)
		return nil
	}

	// Load remaining state files once and cache for orphan check + missing worktree logging
	remainingEntries, _ := os.ReadDir(stateDir)
	cachedStates := make(map[string]*model.WorktreeCommandState, len(remainingEntries))
	for _, entry := range remainingEntries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		cmdID := strings.TrimSuffix(entry.Name(), ".yaml")
		st, loadErr := wm.loadStateUnlocked(cmdID)
		if loadErr != nil {
			continue
		}
		cachedStates[cmdID] = st
	}

	// Build known paths from cached states
	knownPaths := make(map[string]bool)
	for cmdID, st := range cachedStates {
		for _, ws := range st.Workers {
			knownPaths[ws.Path] = true
		}
		knownPaths[wm.integrationWorktreePath(cmdID)] = true
	}

	// Remove orphaned git worktrees not tracked in any state file
	pathPrefix := wm.config.EffectivePathPrefix()
	for _, wtPath := range gitWorktrees {
		relPath, relErr := filepath.Rel(wm.projectRoot, wtPath)
		if relErr != nil || !strings.HasPrefix(relPath, pathPrefix) {
			continue
		}
		if !knownPaths[wtPath] {
			wm.log(core.LogLevelInfo, "gc_orphan_worktree path=%s", wtPath)
			if rmErr := wm.gitRun("worktree", "remove", "--force", wtPath); rmErr != nil {
				wm.log(core.LogLevelWarn, "gc_remove_orphan error=%v path=%s", rmErr, wtPath)
			}
		}
	}

	// Log state entries whose worktree directories don't exist (using cached states)
	for cmdID, st := range cachedStates {
		for _, ws := range st.Workers {
			if _, statErr := os.Stat(ws.Path); os.IsNotExist(statErr) {
				if ws.Status != model.WorktreeStatusCleanupDone && ws.Status != model.WorktreeStatusCleanupFailed {
					wm.log(core.LogLevelWarn, "gc_state_without_worktree command=%s worker=%s path=%s",
						cmdID, ws.WorkerID, ws.Path)
				}
			}
		}
	}

	return nil
}

// GetState returns the worktree state for a specific worker in a command.
func (wm *Manager) GetState(commandID, workerID string) (*model.WorktreeState, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return nil, err
	}

	for i := range state.Workers {
		if state.Workers[i].WorkerID == workerID {
			return &state.Workers[i], nil
		}
	}
	return nil, fmt.Errorf("worker %s not found in command %s", workerID, commandID)
}

// GetCommandState returns the full worktree state for a command.
func (wm *Manager) GetCommandState(commandID string) (*model.WorktreeCommandState, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	return wm.loadState(commandID)
}

// HasWorktrees checks if worktrees exist for a given command.
func (wm *Manager) HasWorktrees(commandID string) bool {
	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	_, err := os.Stat(statePath)
	return err == nil
}

// CleanupAll removes all worktrees (used by `maestro up --reset`).
func (wm *Manager) CleanupAll() error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read worktree state dir: %w", err)
	}

	var errs []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		state, err := wm.loadStateUnlocked(commandID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("load state %s: %v", commandID, err))
			continue
		}
		if err := wm.cleanupCommandUnlocked(commandID, state); err != nil {
			errs = append(errs, fmt.Sprintf("cleanup %s: %v", commandID, err))
		}
	}

	// Also prune any orphaned git worktrees
	_ = wm.gitRun("worktree", "prune")

	if len(errs) > 0 {
		return fmt.Errorf("cleanup_all errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// MarkPhaseMerged records that a phase has been merged so it won't be re-merged.
func (wm *Manager) MarkPhaseMerged(commandID, phaseID string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return err
	}

	if state.MergedPhases == nil {
		state.MergedPhases = make(map[string]string)
	}
	state.MergedPhases[phaseID] = wm.clock.Now().UTC().Format(time.RFC3339)
	state.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
	return wm.saveState(commandID, state)
}

// DiscardWorkerChanges discards all uncommitted changes in a worker's worktree.
// Used during cancellation to clean up in-progress work.
func (wm *Manager) DiscardWorkerChanges(commandID, workerID string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	ws := wm.findWorker(state, workerID)
	if ws == nil {
		return fmt.Errorf("worker %s not found in command %s", workerID, commandID)
	}

	// Reset staged changes so checkout can fully restore tracked files
	if err := wm.gitRunInDir(ws.Path, "reset", "HEAD"); err != nil {
		return fmt.Errorf("reset staged changes in %s: %w", ws.Path, err)
	}

	// Discard tracked file changes
	if err := wm.gitRunInDir(ws.Path, "checkout", "--", "."); err != nil {
		return fmt.Errorf("discard changes in %s: %w", ws.Path, err)
	}

	// Remove untracked files (but not .gitignore'd files)
	if err := wm.gitRunInDir(ws.Path, "clean", "-fd"); err != nil {
		return fmt.Errorf("clean untracked files in %s: %w", ws.Path, err)
	}

	wm.log(core.LogLevelInfo, "worker_changes_discarded command=%s worker=%s", commandID, workerID)
	return nil
}

// Reconcile checks for inconsistencies between state files and actual worktrees
// at daemon startup. This is a best-effort operation: errors are logged and
// reconciliation continues.
func (wm *Manager) Reconcile() {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	wm.log(core.LogLevelInfo, "reconcile_start")

	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			wm.log(core.LogLevelDebug, "reconcile_skip no_state_dir")
			return
		}
		wm.log(core.LogLevelWarn, "reconcile_read_state_dir error=%v", err)
		return
	}

	// Collect all known worktree paths from state files
	knownPaths := make(map[string]bool)

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		state, loadErr := wm.loadStateUnlocked(commandID)
		if loadErr != nil {
			wm.log(core.LogLevelWarn, "reconcile_load_state command=%s error=%v", commandID, loadErr)
			continue
		}

		stateChanged := false
		now := wm.clock.Now().UTC().Format(time.RFC3339)

		for i := range state.Workers {
			ws := &state.Workers[i]
			knownPaths[ws.Path] = true

			// State exists but worktree directory is gone → mark cleanup_done
			if _, statErr := os.Stat(ws.Path); os.IsNotExist(statErr) {
				if ws.Status != model.WorktreeStatusCleanupDone && ws.Status != model.WorktreeStatusCleanupFailed {
					wm.log(core.LogLevelWarn, "reconcile_stale_state command=%s worker=%s path=%s",
						commandID, ws.WorkerID, ws.Path)
					if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusCleanupDone, now); tErr != nil {
						wm.log(core.LogLevelWarn, "reconcile_transition command=%s worker=%s error=%v",
							commandID, ws.WorkerID, tErr)
					}
					stateChanged = true
				}
			}
		}

		// Track integration worktree path
		integrationPath := wm.integrationWorktreePath(commandID)
		knownPaths[integrationPath] = true

		if stateChanged {
			state.UpdatedAt = now
			if saveErr := wm.saveState(commandID, state); saveErr != nil {
				wm.log(core.LogLevelWarn, "reconcile_save_state command=%s error=%v", commandID, saveErr)
			}
		}
	}

	// Worktree exists in git but no state → remove it
	gitWorktrees, listErr := wm.listGitWorktreesUnlocked()
	if listErr != nil {
		wm.log(core.LogLevelWarn, "reconcile_list_worktrees error=%v", listErr)
	} else {
		pathPrefix := wm.config.EffectivePathPrefix()
		for _, wtPath := range gitWorktrees {
			relPath, relErr := filepath.Rel(wm.projectRoot, wtPath)
			if relErr != nil || !strings.HasPrefix(relPath, pathPrefix) {
				continue
			}
			if !knownPaths[wtPath] {
				wm.log(core.LogLevelWarn, "reconcile_orphan_worktree path=%s", wtPath)
				if rmErr := wm.gitRun("worktree", "remove", "--force", wtPath); rmErr != nil {
					wm.log(core.LogLevelWarn, "reconcile_remove_orphan error=%v path=%s", rmErr, wtPath)
				}
			}
		}
	}

	// Prune stale git worktree entries
	if pruneErr := wm.gitRun("worktree", "prune"); pruneErr != nil {
		wm.log(core.LogLevelWarn, "reconcile_prune error=%v", pruneErr)
	}

	wm.log(core.LogLevelInfo, "reconcile_complete")
}

// hasUnmergedFiles checks if a directory has unmerged index entries (indicating a true merge conflict).
// Uses `git ls-files -u` which is more robust than checking exit codes for automation.
// Returns (hasConflict, error) — callers must handle probe errors separately.
func (wm *Manager) hasUnmergedFiles(dir string) (bool, error) {
	output, err := wm.gitOutputInDir(dir, "ls-files", "-u")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

// --- Internal helpers ---

// integrationWorktreePath returns the conventional path for the integration worktree.
func (wm *Manager) integrationWorktreePath(commandID string) string {
	return filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID, "_integration")
}

// rollbackWorkerWorktree removes a worker's worktree and branch (best-effort cleanup).
func (wm *Manager) rollbackWorkerWorktree(commandID string, state *model.WorktreeCommandState, workerID string) {
	for _, ws := range state.Workers {
		if ws.WorkerID == workerID {
			if rbErr := wm.gitRun("worktree", "remove", "--force", ws.Path); rbErr != nil {
				wm.log(core.LogLevelWarn, "rollback_worktree_remove command=%s worker=%s error=%v", commandID, workerID, rbErr)
			}
			_ = wm.gitRun("branch", "-D", ws.Branch)
			return
		}
	}
}

func (wm *Manager) findWorker(state *model.WorktreeCommandState, workerID string) *model.WorktreeState {
	for i := range state.Workers {
		if state.Workers[i].WorkerID == workerID {
			return &state.Workers[i]
		}
	}
	return nil
}

func (wm *Manager) saveState(commandID string, state *model.WorktreeCommandState) error {
	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	statePath := filepath.Join(stateDir, commandID+".yaml")
	return yamlutil.AtomicWrite(statePath, state)
}

// loadState loads state without acquiring mu. Callers that need thread-safety
// must hold wm.mu themselves. Public read-only methods like GetState and
// GetWorkerPath acquire mu before calling this.
func (wm *Manager) loadState(commandID string) (*model.WorktreeCommandState, error) {
	return wm.loadStateUnlocked(commandID)
}

const maxWorktreeStateBytes = 1 << 20 // 1MB

func (wm *Manager) loadStateUnlocked(commandID string) (*model.WorktreeCommandState, error) {
	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	fi, err := os.Stat(statePath)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxWorktreeStateBytes {
		return nil, fmt.Errorf("worktree state file too large (%d bytes > %d max)", fi.Size(), maxWorktreeStateBytes)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}
	var state model.WorktreeCommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse worktree state: %w", err)
	}
	return &state, nil
}

func (wm *Manager) cleanupCommandUnlocked(commandID string, state *model.WorktreeCommandState) error {
	var errs []string

	for _, ws := range state.Workers {
		if err := wm.gitRun("worktree", "remove", "--force", ws.Path); err != nil {
			errs = append(errs, fmt.Sprintf("remove worktree %s: %v", ws.WorkerID, err))
		}
		_ = wm.gitRun("branch", "-D", ws.Branch)
	}

	// Remove integration worktree before branch deletion
	integrationPath := wm.integrationWorktreePath(commandID)
	_ = wm.gitRun("worktree", "remove", "--force", integrationPath)

	_ = wm.gitRun("branch", "-D", state.Integration.Branch)

	// Remove directory
	wtDir := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID)
	_ = os.RemoveAll(wtDir)

	// Remove state file
	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	_ = os.Remove(statePath)

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (wm *Manager) getConflictFilesInDir(dir string) ([]string, error) {
	output, err := wm.gitOutputInDir(dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// gitTimeout returns the configured git command timeout as a time.Duration.
func (wm *Manager) gitTimeout() time.Duration {
	return time.Duration(wm.config.EffectiveGitTimeout()) * time.Second
}

// gitExec is the shared git execution helper. All git operations go through
// this method to ensure consistent timeout and error handling.
// dir specifies the working directory; if empty, projectRoot is used.
// Returns (stdout, combinedOutput, error). Callers that need only the exit
// status use gitRun/gitRunInDir; callers that need stdout use gitOutput/gitOutputInDir.
func (wm *Manager) gitExecCombined(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wm.gitTimeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Dir = wm.projectRoot
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			dirLabel := wm.projectRoot
			if dir != "" {
				dirLabel = dir
			}
			return output, fmt.Errorf("git %s (in %s): timeout after %s: %w",
				strings.Join(args, " "), dirLabel, wm.gitTimeout(), ctx.Err())
		}
		return output, err
	}
	return output, nil
}

func (wm *Manager) gitExecOutput(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wm.gitTimeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Dir = wm.projectRoot
	}
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			dirLabel := wm.projectRoot
			if dir != "" {
				dirLabel = dir
			}
			return nil, fmt.Errorf("git %s (in %s): timeout after %s: %w",
				strings.Join(args, " "), dirLabel, wm.gitTimeout(), ctx.Err())
		}
		return nil, err
	}
	return output, nil
}

// gitRun executes a git command in the project root.
func (wm *Manager) gitRun(args ...string) error {
	output, err := wm.gitExecCombined("", args...)
	if err != nil {
		return fmt.Errorf("git %s: %w\noutput: %s", strings.Join(args, " "), err, string(output))
	}
	return nil
}

// gitOutput executes a git command and returns stdout.
func (wm *Manager) gitOutput(args ...string) (string, error) {
	output, err := wm.gitExecOutput("", args...)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(output), nil
}

// gitRunInDir executes a git command in a specific directory.
func (wm *Manager) gitRunInDir(dir string, args ...string) error {
	output, err := wm.gitExecCombined(dir, args...)
	if err != nil {
		return fmt.Errorf("git -C %s %s: %w\noutput: %s", dir, strings.Join(args, " "), err, string(output))
	}
	return nil
}

// gitOutputInDir executes a git command in a specific directory and returns stdout.
func (wm *Manager) gitOutputInDir(dir string, args ...string) (string, error) {
	output, err := wm.gitExecOutput(dir, args...)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git -C %s %s: %w\nstderr: %s", dir, strings.Join(args, " "), err, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git -C %s %s: %w", dir, strings.Join(args, " "), err)
	}
	return string(output), nil
}

// listGitWorktreesUnlocked returns paths of all git worktrees via `git worktree list --porcelain`.
// Caller must hold wm.mu.
func (wm *Manager) listGitWorktreesUnlocked() ([]string, error) {
	output, err := wm.gitOutput("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeListPorcelain(output), nil
}

// parseWorktreeListPorcelain extracts worktree paths from `git worktree list --porcelain` output.
func parseWorktreeListPorcelain(output string) []string {
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			paths = append(paths, strings.TrimPrefix(line, "worktree "))
		}
	}
	return paths
}

// sensitiveFilePatterns lists file name patterns that should never be staged
// automatically, even if they are not covered by .gitignore.
var sensitiveFilePatterns = []string{
	".env",
	".env.*",
	"*.key",
	"*.pem",
	"*.secret",
	"*.p12",
	"*.pfx",
	"credentials.*",
}

// isSensitiveFile returns true if the filename matches a sensitive pattern
// that should not be staged automatically.
func isSensitiveFile(name string) bool {
	base := filepath.Base(name)
	for _, pattern := range sensitiveFilePatterns {
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}
	return false
}

// stageNewFiles stages untracked files that pass both .gitignore and the
// sensitive-file safety filter. Files matching sensitive patterns are logged
// but not staged. Uses NUL-separated output for safe filename handling.
func (wm *Manager) stageNewFiles(dir string) error {
	// List untracked files respecting .gitignore (NUL-separated for safety)
	output, err := wm.gitOutputInDir(dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return fmt.Errorf("list untracked files: %w", err)
	}

	var toStage []string
	for _, name := range strings.Split(output, "\x00") {
		if name == "" {
			continue
		}
		if isSensitiveFile(name) {
			wm.log(core.LogLevelWarn, "skip_sensitive_file path=%s dir=%s", name, dir)
			continue
		}
		toStage = append(toStage, name)
	}

	if len(toStage) == 0 {
		return nil
	}

	args := append([]string{"add", "--"}, toStage...)
	if err := wm.gitRunInDir(dir, args...); err != nil {
		return fmt.Errorf("git add new files: %w", err)
	}
	return nil
}

// unstageSensitiveFiles checks the staged file list and unstages any files
// matching sensitive patterns. This prevents accidentally committing sensitive
// tracked files that were staged by git add -u.
func (wm *Manager) unstageSensitiveFiles(dir string) error {
	output, err := wm.gitOutputInDir(dir, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return fmt.Errorf("list staged files: %w", err)
	}

	var toUnstage []string
	for _, name := range strings.Split(output, "\x00") {
		if name == "" {
			continue
		}
		if isSensitiveFile(name) {
			wm.log(core.LogLevelWarn, "unstage_sensitive_tracked_file path=%s dir=%s", name, dir)
			toUnstage = append(toUnstage, name)
		}
	}

	if len(toUnstage) == 0 {
		return nil
	}

	args := append([]string{"reset", "HEAD", "--"}, toUnstage...)
	if err := wm.gitRunInDir(dir, args...); err != nil {
		return fmt.Errorf("unstage sensitive files: %w", err)
	}
	return nil
}

// CommitPolicyViolation represents a single commit policy check failure.
type CommitPolicyViolation struct {
	Code    string   // machine-readable code (e.g. "max_files_exceeded")
	Message string   // human-readable description
	Files   []string // affected files (if applicable)
}

// checkCommitPolicy validates the staged changes and commit message against the
// configured CommitPolicy. Returns an empty slice if all checks pass.
// stagedNul is the NUL-separated output from `git diff --cached --name-only -z`.
func (wm *Manager) checkCommitPolicy(worktreePath, message, stagedNul string) []CommitPolicyViolation {
	policy := wm.config.CommitPolicy
	var violations []CommitPolicyViolation

	// Parse staged file list
	var stagedFiles []string
	for _, name := range strings.Split(stagedNul, "\x00") {
		if name != "" {
			stagedFiles = append(stagedFiles, name)
		}
	}

	// Check 1: Maximum files per commit (MaxFiles=0 means unlimited)
	maxFiles := policy.MaxFiles
	if maxFiles > 0 && len(stagedFiles) > maxFiles {
		violations = append(violations, CommitPolicyViolation{
			Code:    "max_files_exceeded",
			Message: fmt.Sprintf("staged file count %d exceeds limit %d", len(stagedFiles), maxFiles),
			Files:   stagedFiles,
		})
	}

	// Check 2: .gitignore existence
	if policy.RequireGitignore {
		gitignorePath := filepath.Join(worktreePath, ".gitignore")
		if _, err := os.Stat(gitignorePath); err != nil {
			if os.IsNotExist(err) {
				violations = append(violations, CommitPolicyViolation{
					Code:    "missing_gitignore",
					Message: ".gitignore file not found in worktree root",
				})
			} else {
				violations = append(violations, CommitPolicyViolation{
					Code:    "gitignore_check_error",
					Message: fmt.Sprintf("failed to check .gitignore: %v", err),
				})
			}
		}
	}

	// Check 3: Commit message format
	pattern := policy.MessagePattern
	if pattern != "" {
		re, err := regexp.Compile(pattern)
		if err != nil {
			violations = append(violations, CommitPolicyViolation{
				Code:    "invalid_message_pattern",
				Message: fmt.Sprintf("commit message pattern %q is invalid: %v", pattern, err),
			})
		} else if !re.MatchString(message) {
			violations = append(violations, CommitPolicyViolation{
				Code:    "message_format_invalid",
				Message: fmt.Sprintf("commit message does not match required pattern %q", pattern),
			})
		}
	}

	return violations
}

// setWorkerStatus validates and applies a status transition for a worker.
func (wm *Manager) setWorkerStatus(ws *model.WorktreeState, newStatus model.WorktreeStatus, now string) error {
	if err := model.ValidateWorktreeTransition(ws.Status, newStatus); err != nil {
		wm.log(core.LogLevelError, "invalid_worktree_transition worker=%s from=%s to=%s error=%v",
			ws.WorkerID, ws.Status, newStatus, err)
		return fmt.Errorf("worker %s: %w", ws.WorkerID, err)
	}
	ws.Status = newStatus
	ws.UpdatedAt = now
	return nil
}

// setIntegrationStatus validates and applies a status transition for the integration branch.
func (wm *Manager) setIntegrationStatus(state *model.WorktreeCommandState, newStatus model.IntegrationStatus, now string) error {
	if err := model.ValidateIntegrationTransition(state.Integration.Status, newStatus); err != nil {
		wm.log(core.LogLevelError, "invalid_integration_transition command=%s from=%s to=%s error=%v",
			state.CommandID, state.Integration.Status, newStatus, err)
		return fmt.Errorf("integration %s: %w", state.CommandID, err)
	}
	state.Integration.Status = newStatus
	state.Integration.UpdatedAt = now
	return nil
}

// SetClock replaces the clock (for testing).
func (wm *Manager) SetClock(c core.Clock) { wm.clock = c }

// SetConfig replaces the worktree config (for testing).
func (wm *Manager) SetConfig(cfg model.WorktreeConfig) { wm.config = cfg }

// SetProjectRoot overrides the project root path (for testing).
func (wm *Manager) SetProjectRoot(root string) { wm.projectRoot = root }

// AutoCommit returns whether auto-commit is enabled in the worktree config.
func (wm *Manager) AutoCommit() bool { return wm.config.AutoCommit }

// AutoMerge returns whether auto-merge is enabled in the worktree config.
func (wm *Manager) AutoMerge() bool { return wm.config.AutoMerge }

func (wm *Manager) log(level core.LogLevel, format string, args ...any) {
	wm.dl.Logf(level, format, args...)
}
