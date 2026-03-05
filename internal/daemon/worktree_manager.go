package daemon

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// WorktreeManager manages git worktree lifecycle for Worker isolation.
// All git operations are serialized through this manager (Single-Writer pattern).
type WorktreeManager struct {
	maestroDir  string
	projectRoot string
	config      model.WorktreeConfig
	dl          *DaemonLogger
	clock       Clock
	mu          sync.Mutex // serializes all git operations
}

// NewWorktreeManager creates a new WorktreeManager.
func NewWorktreeManager(maestroDir string, cfg model.WorktreeConfig, logger *log.Logger, logLevel LogLevel) *WorktreeManager {
	projectRoot := filepath.Dir(maestroDir)
	return &WorktreeManager{
		maestroDir:  maestroDir,
		projectRoot: projectRoot,
		config:      cfg,
		dl:          NewDaemonLoggerFromLegacy("worktree_manager", logger, logLevel),
		clock:       RealClock{},
	}
}

// CreateForCommand creates worktrees for all workers and an integration branch for a command.
func (wm *WorktreeManager) CreateForCommand(commandID string, workerIDs []string) error {
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
				wm.log(LogLevelWarn, "rollback_worktree_remove command=%s path=%s error=%v", commandID, wt.path, rbErr)
			}
			_ = wm.gitRun("branch", "-D", wt.branch)
		}
		// Remove integration worktree before branch deletion
		_ = wm.gitRun("worktree", "remove", "--force", integrationPath)
		if rbErr := wm.gitRun("branch", "-D", integrationBranch); rbErr != nil {
			wm.log(LogLevelWarn, "rollback_integration_branch command=%s error=%v", commandID, rbErr)
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

	wm.log(LogLevelInfo, "worktrees_created command=%s workers=%d base=%s",
		commandID, len(workerIDs), baseSHA[:8])
	return nil
}

// EnsureWorkerWorktree lazily creates a worktree for a single worker.
// If the command has no worktree state yet, it creates the integration branch
// and the worker's worktree. If state exists but the worker is missing, it adds
// the worker worktree to the existing command state.
func (wm *WorktreeManager) EnsureWorkerWorktree(commandID, workerID string) error {
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
				wm.log(LogLevelWarn, "rollback_integration_branch command=%s error=%v", commandID, rbErr)
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

func (wm *WorktreeManager) addWorkerWorktreeUnlocked(state *model.WorktreeCommandState, commandID, workerID, baseSHA, now string) error {
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

	wm.log(LogLevelInfo, "worker_worktree_created command=%s worker=%s", commandID, workerID)
	return nil
}

// GetWorkerPath returns the worktree path for a specific worker.
func (wm *WorktreeManager) GetWorkerPath(commandID, workerID string) (string, error) {
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
func (wm *WorktreeManager) CommitWorkerChanges(commandID, workerID, message string) error {
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
		wm.log(LogLevelDebug, "no_changes_to_commit command=%s worker=%s", commandID, workerID)
		return nil
	}

	// Stage all changes and commit
	if err := wm.gitRunInDir(ws.Path, "add", "-A"); err != nil {
		return fmt.Errorf("git add in %s: %w", ws.Path, err)
	}
	if err := wm.gitRunInDir(ws.Path, "commit", "-m", message); err != nil {
		return fmt.Errorf("git commit in %s: %w", ws.Path, err)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	ws.Status = model.WorktreeStatusCommitted
	ws.UpdatedAt = now
	state.UpdatedAt = now

	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	wm.log(LogLevelInfo, "worker_committed command=%s worker=%s", commandID, workerID)
	return nil
}

// MergeToIntegration merges worker branches into the integration branch.
// Returns any merge conflicts encountered. Workers are merged in deterministic order.
// All merge operations happen in the integration worktree (H3: projectRoot HEAD is never changed).
func (wm *WorktreeManager) MergeToIntegration(commandID string, workerIDs []string) ([]model.MergeConflict, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	integrationPath := wm.integrationWorktreePath(commandID)
	now := wm.clock.Now().UTC().Format(time.RFC3339)
	state.Integration.Status = model.IntegrationStatusMerging
	state.Integration.UpdatedAt = now
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
			wm.log(LogLevelWarn, "merge_log_check command=%s worker=%s error=%v", commandID, workerID, err)
			continue
		}
		if strings.TrimSpace(logOut) == "" {
			wm.log(LogLevelDebug, "no_commits_to_merge command=%s worker=%s", commandID, workerID)
			continue
		}

		// Merge worker branch in integration worktree (not projectRoot)
		strategy := wm.config.EffectiveMergeStrategy()
		mergeMsg := fmt.Sprintf("[maestro] merge %s into integration for %s", workerID, commandID)

		err = wm.gitRunInDir(integrationPath, "merge", "--no-ff", "-s", strategy, "-m", mergeMsg, ws.Branch)
		if err != nil {
			// Merge conflict detected
			conflictFiles, _ := wm.getConflictFilesInDir(integrationPath)
			conflicts = append(conflicts, model.MergeConflict{
				WorkerID:      workerID,
				ConflictFiles: conflictFiles,
				Message:       fmt.Sprintf("merge conflict: %s → integration", workerID),
			})

			// Abort the failed merge
			_ = wm.gitRunInDir(integrationPath, "merge", "--abort")

			ws.Status = model.WorktreeStatusConflict
			ws.UpdatedAt = now
			wm.log(LogLevelWarn, "merge_conflict command=%s worker=%s files=%v",
				commandID, workerID, conflictFiles)
			continue
		}

		ws.Status = model.WorktreeStatusIntegrated
		ws.UpdatedAt = now
		wm.log(LogLevelInfo, "worker_merged command=%s worker=%s", commandID, workerID)
	}

	if len(conflicts) > 0 {
		state.Integration.Status = model.IntegrationStatusConflict
	} else {
		state.Integration.Status = model.IntegrationStatusMerged
	}
	state.Integration.UpdatedAt = now
	state.UpdatedAt = now

	if err := wm.saveState(commandID, state); err != nil {
		return conflicts, fmt.Errorf("save state: %w", err)
	}

	return conflicts, nil
}

// SyncFromIntegration updates worker worktrees with the latest integration branch state.
func (wm *WorktreeManager) SyncFromIntegration(commandID string, workerIDs []string) error {
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
			wm.log(LogLevelWarn, "sync_skip_conflict command=%s worker=%s status=%s",
				commandID, workerID, ws.Status)
			continue
		}

		// M3: Skip dirty worktrees (uncommitted changes)
		statusOut, statusErr := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
		if statusErr == nil && strings.TrimSpace(statusOut) != "" {
			wm.log(LogLevelWarn, "sync_skip_dirty command=%s worker=%s",
				commandID, workerID)
			continue
		}

		// Merge integration branch into worker worktree
		err := wm.gitRunInDir(ws.Path, "merge", state.Integration.Branch,
			"-m", fmt.Sprintf("[maestro] sync integration into %s", workerID))
		if err != nil {
			wm.log(LogLevelWarn, "sync_from_integration command=%s worker=%s error=%v",
				commandID, workerID, err)
			// Abort on conflict
			_ = wm.gitRunInDir(ws.Path, "merge", "--abort")
			continue
		}

		ws.Status = model.WorktreeStatusActive
		ws.UpdatedAt = now
	}

	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	return nil
}

// PublishToBase merges the integration branch into the base branch.
// Uses a temporary branch in the integration worktree to avoid changing projectRoot HEAD (H3).
func (wm *WorktreeManager) PublishToBase(commandID string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	state.Integration.Status = model.IntegrationStatusPublishing
	state.Integration.UpdatedAt = now

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
		state.Integration.Status = model.IntegrationStatusConflict
		state.Integration.UpdatedAt = now
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

	state.Integration.Status = model.IntegrationStatusPublished
	state.Integration.UpdatedAt = now
	state.UpdatedAt = now

	// Mark all workers as published
	for i := range state.Workers {
		state.Workers[i].Status = model.WorktreeStatusPublished
		state.Workers[i].UpdatedAt = now
	}

	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	wm.log(LogLevelInfo, "published_to_base command=%s branch=%s", commandID, baseBranch)
	return nil
}

// CleanupCommand removes all worktrees and branches for a command.
func (wm *WorktreeManager) CleanupCommand(commandID string) error {
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
			ws.Status = model.WorktreeStatusCleanupFailed
		} else {
			ws.Status = model.WorktreeStatusCleanupDone
		}
		ws.UpdatedAt = now

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

	wm.log(LogLevelInfo, "cleanup_complete command=%s", commandID)
	return nil
}

// GC removes old worktrees that exceed TTL or max_worktrees limit.
func (wm *WorktreeManager) GC() error {
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
			wm.log(LogLevelInfo, "gc_ttl_expired command=%s age=%s", commandID, now.Sub(created))
			if err := wm.cleanupCommandUnlocked(commandID, state); err != nil {
				wm.log(LogLevelWarn, "gc_cleanup_failed command=%s error=%v", commandID, err)
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
			wm.log(LogLevelInfo, "gc_max_exceeded command=%s", allStates[i].commandID)
			if err := wm.cleanupCommandUnlocked(allStates[i].commandID, allStates[i].state); err != nil {
				wm.log(LogLevelWarn, "gc_cleanup_failed command=%s error=%v", allStates[i].commandID, err)
			}
		}
	}

	// M4: Health check — cross-reference git worktree list with state files
	gitWorktrees, listErr := wm.listGitWorktreesUnlocked()
	if listErr != nil {
		wm.log(LogLevelWarn, "gc_worktree_list error=%v", listErr)
		return nil
	}

	// Rebuild known paths from remaining state files (some may have been cleaned up above)
	knownPaths := make(map[string]bool)
	remainingEntries, _ := os.ReadDir(stateDir)
	for _, entry := range remainingEntries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		cmdID := strings.TrimSuffix(entry.Name(), ".yaml")
		st, loadErr := wm.loadStateUnlocked(cmdID)
		if loadErr != nil {
			continue
		}
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
			wm.log(LogLevelInfo, "gc_orphan_worktree path=%s", wtPath)
			if rmErr := wm.gitRun("worktree", "remove", "--force", wtPath); rmErr != nil {
				wm.log(LogLevelWarn, "gc_remove_orphan error=%v path=%s", rmErr, wtPath)
			}
		}
	}

	// Log state entries whose worktree directories don't exist
	for _, entry := range remainingEntries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		cmdID := strings.TrimSuffix(entry.Name(), ".yaml")
		st, loadErr := wm.loadStateUnlocked(cmdID)
		if loadErr != nil {
			continue
		}
		for _, ws := range st.Workers {
			if _, statErr := os.Stat(ws.Path); os.IsNotExist(statErr) {
				if ws.Status != model.WorktreeStatusCleanupDone && ws.Status != model.WorktreeStatusCleanupFailed {
					wm.log(LogLevelWarn, "gc_state_without_worktree command=%s worker=%s path=%s",
						cmdID, ws.WorkerID, ws.Path)
				}
			}
		}
	}

	return nil
}

// GetState returns the worktree state for a specific worker in a command.
func (wm *WorktreeManager) GetState(commandID, workerID string) (*model.WorktreeState, error) {
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
func (wm *WorktreeManager) GetCommandState(commandID string) (*model.WorktreeCommandState, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	return wm.loadState(commandID)
}

// HasWorktrees checks if worktrees exist for a given command.
func (wm *WorktreeManager) HasWorktrees(commandID string) bool {
	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	_, err := os.Stat(statePath)
	return err == nil
}

// CleanupAll removes all worktrees (used by `maestro up --reset`).
func (wm *WorktreeManager) CleanupAll() error {
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
func (wm *WorktreeManager) MarkPhaseMerged(commandID, phaseID string) error {
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
func (wm *WorktreeManager) DiscardWorkerChanges(commandID, workerID string) error {
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

	// Discard tracked file changes
	if err := wm.gitRunInDir(ws.Path, "checkout", "--", "."); err != nil {
		return fmt.Errorf("discard changes in %s: %w", ws.Path, err)
	}

	wm.log(LogLevelInfo, "worker_changes_discarded command=%s worker=%s", commandID, workerID)
	return nil
}

// Reconcile checks for inconsistencies between state files and actual worktrees
// at daemon startup. This is a best-effort operation: errors are logged and
// reconciliation continues.
func (wm *WorktreeManager) Reconcile() {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	wm.log(LogLevelInfo, "reconcile_start")

	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			wm.log(LogLevelDebug, "reconcile_skip no_state_dir")
			return
		}
		wm.log(LogLevelWarn, "reconcile_read_state_dir error=%v", err)
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
			wm.log(LogLevelWarn, "reconcile_load_state command=%s error=%v", commandID, loadErr)
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
					wm.log(LogLevelWarn, "reconcile_stale_state command=%s worker=%s path=%s",
						commandID, ws.WorkerID, ws.Path)
					ws.Status = model.WorktreeStatusCleanupDone
					ws.UpdatedAt = now
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
				wm.log(LogLevelWarn, "reconcile_save_state command=%s error=%v", commandID, saveErr)
			}
		}
	}

	// Worktree exists in git but no state → remove it
	gitWorktrees, listErr := wm.listGitWorktreesUnlocked()
	if listErr != nil {
		wm.log(LogLevelWarn, "reconcile_list_worktrees error=%v", listErr)
	} else {
		pathPrefix := wm.config.EffectivePathPrefix()
		for _, wtPath := range gitWorktrees {
			relPath, relErr := filepath.Rel(wm.projectRoot, wtPath)
			if relErr != nil || !strings.HasPrefix(relPath, pathPrefix) {
				continue
			}
			if !knownPaths[wtPath] {
				wm.log(LogLevelWarn, "reconcile_orphan_worktree path=%s", wtPath)
				if rmErr := wm.gitRun("worktree", "remove", "--force", wtPath); rmErr != nil {
					wm.log(LogLevelWarn, "reconcile_remove_orphan error=%v path=%s", rmErr, wtPath)
				}
			}
		}
	}

	// Prune stale git worktree entries
	if pruneErr := wm.gitRun("worktree", "prune"); pruneErr != nil {
		wm.log(LogLevelWarn, "reconcile_prune error=%v", pruneErr)
	}

	wm.log(LogLevelInfo, "reconcile_complete")
}

// --- Internal helpers ---

// integrationWorktreePath returns the conventional path for the integration worktree.
func (wm *WorktreeManager) integrationWorktreePath(commandID string) string {
	return filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID, "_integration")
}

// rollbackWorkerWorktree removes a worker's worktree and branch (best-effort cleanup).
func (wm *WorktreeManager) rollbackWorkerWorktree(commandID string, state *model.WorktreeCommandState, workerID string) {
	for _, ws := range state.Workers {
		if ws.WorkerID == workerID {
			if rbErr := wm.gitRun("worktree", "remove", "--force", ws.Path); rbErr != nil {
				wm.log(LogLevelWarn, "rollback_worktree_remove command=%s worker=%s error=%v", commandID, workerID, rbErr)
			}
			_ = wm.gitRun("branch", "-D", ws.Branch)
			return
		}
	}
}

func (wm *WorktreeManager) findWorker(state *model.WorktreeCommandState, workerID string) *model.WorktreeState {
	for i := range state.Workers {
		if state.Workers[i].WorkerID == workerID {
			return &state.Workers[i]
		}
	}
	return nil
}

func (wm *WorktreeManager) saveState(commandID string, state *model.WorktreeCommandState) error {
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
func (wm *WorktreeManager) loadState(commandID string) (*model.WorktreeCommandState, error) {
	return wm.loadStateUnlocked(commandID)
}

func (wm *WorktreeManager) loadStateUnlocked(commandID string) (*model.WorktreeCommandState, error) {
	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
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

func (wm *WorktreeManager) cleanupCommandUnlocked(commandID string, state *model.WorktreeCommandState) error {
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

func (wm *WorktreeManager) getConflictFilesInDir(dir string) ([]string, error) {
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

// gitRun executes a git command in the project root.
func (wm *WorktreeManager) gitRun(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = wm.projectRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\noutput: %s", strings.Join(args, " "), err, string(output))
	}
	return nil
}

// gitOutput executes a git command and returns stdout.
func (wm *WorktreeManager) gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = wm.projectRoot
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(output), nil
}

// gitRunInDir executes a git command in a specific directory.
func (wm *WorktreeManager) gitRunInDir(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git -C %s %s: %w\noutput: %s", dir, strings.Join(args, " "), err, string(output))
	}
	return nil
}

// gitOutputInDir executes a git command in a specific directory and returns stdout.
func (wm *WorktreeManager) gitOutputInDir(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
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
func (wm *WorktreeManager) listGitWorktreesUnlocked() ([]string, error) {
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

func (wm *WorktreeManager) log(level LogLevel, format string, args ...any) {
	wm.dl.Logf(level, format, args...)
}
