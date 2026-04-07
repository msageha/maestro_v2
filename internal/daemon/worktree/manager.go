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

	// signalStore (optional) provides RMW access to merge_conflict signals
	// for the conflict-resolution lifecycle. Wired via SetSignalStore so
	// existing NewManager call sites do not need to change.
	signalStore SignalStore
	// cmdLocks holds per-commandID *sync.Mutex used by resolver methods to
	// serialize against scan and against each other. Locking order:
	// scanMu (caller, outside this package) → cmdLocks[cmd] → wm.mu.
	cmdLocks sync.Map
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
		// Worktree had dirty files but all were filtered — this is not a clean success.
		wm.log(core.LogLevelWarn, "all_files_filtered command=%s worker=%s", commandID, workerID)
		return fmt.Errorf("commit for worker %s in command %s: %w", workerID, commandID, ErrAllFilesFiltered)
	}

	// Commit policy checks
	if violations := wm.checkCommitPolicy(ws.Path, message, stagedOut); len(violations) > 0 {
		for _, v := range violations {
			wm.log(core.LogLevelWarn, "commit_policy_violation command=%s worker=%s code=%s msg=%s",
				commandID, workerID, v.Code, v.Message)
		}
		// Reset staged changes so the worktree is left in a clean index state.
		// Note: dirty files remain in the worktree after reset.
		if resetErr := wm.gitRunInDir(ws.Path, "reset", "HEAD"); resetErr != nil {
			wm.log(core.LogLevelWarn, "git_reset_after_policy_violation command=%s worker=%s error=%v",
				commandID, workerID, resetErr)
		}
		wm.log(core.LogLevelWarn, "dirty_files_remain_after_policy_reset command=%s worker=%s",
			commandID, workerID)
		return &CommitPolicyViolationError{Violations: violations}
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

// MarkIntegrationFailed transitions the integration branch status to Failed.
// Used when no worker commit succeeded for a phase, so the merge step is
// skipped entirely and the integration must be marked failed explicitly to
// distinguish a stuck "still Created/Merged" state from a permanent failure
// that the planner can react to.
func (wm *Manager) MarkIntegrationFailed(commandID string) error {
	if err := validateIDs(commandID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	// H10: quarantined is terminal — never attempt a Failed transition (which
	// would be rejected as invalid). Treat as success no-op so out-of-band or
	// stale callers cannot spam errors against quarantined integrations.
	if state.Integration.Status == model.IntegrationStatusQuarantined {
		return nil
	}
	now := wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.setIntegrationStatus(state, model.IntegrationStatusFailed, now); err != nil {
		return err
	}
	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// MarkIntegrationStallSignaled records that a worktree_stalled signal has been
// emitted for this command's integration branch. Idempotent: subsequent calls
// after the flag is set are no-ops. The integration status itself is left
// unchanged.
func (wm *Manager) MarkIntegrationStallSignaled(commandID string) error {
	if err := validateIDs(commandID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if state.Integration.StallSignaled {
		return nil
	}
	state.Integration.StallSignaled = true
	state.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
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

// AddCommitFailedWorker records a worker whose auto-commit failed for a command.
// Idempotent: duplicate IDs are deduped. Used by Phase B to block publish until cleared.
func (wm *Manager) AddCommitFailedWorker(commandID, workerID string) error {
	if err := validateIDs(commandID, workerID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return err
	}
	for _, w := range state.CommitFailedWorkers {
		if w == workerID {
			return nil
		}
	}
	state.CommitFailedWorkers = append(state.CommitFailedWorkers, workerID)
	state.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
	return wm.saveState(commandID, state)
}

// RemoveCommitFailedWorker clears a worker from the commit-failed list after a successful commit.
func (wm *Manager) RemoveCommitFailedWorker(commandID, workerID string) error {
	if err := validateIDs(commandID, workerID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return err
	}
	if len(state.CommitFailedWorkers) == 0 {
		return nil
	}
	filtered := state.CommitFailedWorkers[:0]
	changed := false
	for _, w := range state.CommitFailedWorkers {
		if w == workerID {
			changed = true
			continue
		}
		filtered = append(filtered, w)
	}
	if !changed {
		return nil
	}
	state.CommitFailedWorkers = filtered
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

	// Tripwire: refuse to run destructive git ops outside the project root.
	if err := ensureWithinProjectRoot(wm.projectRoot, ws.Path); err != nil {
		return fmt.Errorf("discard worker changes: %w", err)
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

// AutoCommit returns whether auto-commit is enabled in the worktree config.
func (wm *Manager) AutoCommit() bool { return wm.config.AutoCommit }

// AutoMerge returns whether auto-merge is enabled in the worktree config.
func (wm *Manager) AutoMerge() bool { return wm.config.AutoMerge }

func (wm *Manager) log(level core.LogLevel, format string, args ...any) {
	wm.dl.Logf(level, format, args...)
}
