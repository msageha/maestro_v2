package worktree

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// validSHAPattern matches a valid git object hash (SHA-1: 40 hex, SHA-256: 64 hex).
var validSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// validateSHA checks that s is a valid lowercase hex git hash (40 or 64 chars).
func validateSHA(s string) error {
	if !validSHAPattern.MatchString(s) {
		return fmt.Errorf("invalid git SHA format: %q", s)
	}
	return nil
}

// validateIDs checks that commandID and optional workerIDs are safe for use
// in file paths (defense-in-depth against path traversal).
func validateIDs(commandID string, workerIDs ...string) error {
	if err := validate.ValidateID(commandID); err != nil {
		return fmt.Errorf("invalid commandID: %w", err)
	}
	for _, wid := range workerIDs {
		if err := validate.ValidateID(wid); err != nil {
			return fmt.Errorf("invalid workerID: %w", err)
		}
	}
	return nil
}

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
	if err := validateIDs(commandID, workerIDs...); err != nil {
		return err
	}
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
	if err := validateSHA(baseSHA); err != nil {
		return fmt.Errorf("base SHA from %s: %w", baseBranch, err)
	}

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

	shortSHA := baseSHA
	if len(shortSHA) > 8 {
		shortSHA = shortSHA[:8]
	}
	wm.log(core.LogLevelInfo, "worktrees_created command=%s workers=%d base=%s",
		commandID, len(workerIDs), shortSHA)
	return nil
}

// EnsureWorkerWorktree lazily creates a worktree for a single worker.
// If the command has no worktree state yet, it creates the integration branch
// and the worker's worktree. If state exists but the worker is missing, it adds
// the worker worktree to the existing command state.
func (wm *Manager) EnsureWorkerWorktree(commandID, workerID string) error {
	if err := validateIDs(commandID, workerID); err != nil {
		return err
	}
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
		if err := validateSHA(baseSHA); err != nil {
			return fmt.Errorf("base SHA from %s: %w", baseBranch, err)
		}

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

	// Validate persisted baseSHA before using it for git operations
	if err := validateSHA(state.Integration.BaseSHA); err != nil {
		return fmt.Errorf("persisted base SHA for %s: %w", commandID, err)
	}

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
	if err := validateIDs(commandID, workerID); err != nil {
		return "", err
	}
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
	if err := validateIDs(commandID, workerID); err != nil {
		return err
	}
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

// GetState returns the worktree state for a specific worker in a command.
func (wm *Manager) GetState(commandID, workerID string) (*model.WorktreeState, error) {
	if err := validateIDs(commandID, workerID); err != nil {
		return nil, err
	}
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
	if err := validateIDs(commandID); err != nil {
		return nil, err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()
	return wm.loadState(commandID)
}

// HasWorktrees checks if worktrees exist for a given command.
func (wm *Manager) HasWorktrees(commandID string) bool {
	if err := validateIDs(commandID); err != nil {
		return false
	}
	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	_, err := os.Stat(statePath)
	return err == nil
}

// MarkPhaseMerged records that a phase has been merged so it won't be re-merged.
func (wm *Manager) MarkPhaseMerged(commandID, phaseID string) error {
	if err := validateIDs(commandID, phaseID); err != nil {
		return err
	}
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
	if err := validateIDs(commandID, workerID); err != nil {
		return err
	}
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

// SetClock replaces the clock (for testing).
// Must be called before any concurrent operations start.
func (wm *Manager) SetClock(c core.Clock) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.clock = c
}

// SetConfig replaces the worktree config (for testing).
// Must be called before any concurrent operations start.
func (wm *Manager) SetConfig(cfg model.WorktreeConfig) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.config = cfg
}

// SetProjectRoot overrides the project root path (for testing).
// Must be called before any concurrent operations start.
func (wm *Manager) SetProjectRoot(root string) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.projectRoot = root
}

// AutoCommit returns whether auto-commit is enabled in the worktree config.
func (wm *Manager) AutoCommit() bool { return wm.config.AutoCommit }

// AutoMerge returns whether auto-merge is enabled in the worktree config.
func (wm *Manager) AutoMerge() bool { return wm.config.AutoMerge }

func (wm *Manager) log(level core.LogLevel, format string, args ...any) {
	wm.dl.Logf(level, format, args...)
}
