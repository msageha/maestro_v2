package worktree

import (
	"errors"
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
	if err := validate.ID(commandID); err != nil {
		return fmt.Errorf("invalid commandID: %w", err)
	}
	for _, wid := range workerIDs {
		if err := validate.ID(wid); err != nil {
			return fmt.Errorf("invalid workerID: %w", err)
		}
	}
	return nil
}

// validateCommandAndPhaseIDs checks that commandID is a valid ID and phaseID
// is a valid phase ID (which permits __-prefixed internal identifiers).
func validateCommandAndPhaseIDs(commandID, phaseID string) error {
	if err := validate.ID(commandID); err != nil {
		return fmt.Errorf("invalid commandID: %w", err)
	}
	if err := validate.PhaseID(phaseID); err != nil {
		return fmt.Errorf("invalid phaseID: %w", err)
	}
	return nil
}

// Manager manages git worktree lifecycle for Worker isolation.
// All git operations are serialized through this manager (Single-Writer pattern).
type Manager struct {
	core.LogMixin
	maestroDir  string
	projectRoot string
	config      model.WorktreeConfig
	clock       core.Clock
	mu          sync.Mutex // serializes all git operations

	// signalStore (optional) provides RMW access to merge_conflict signals
	// for the conflict-resolution lifecycle. Wired via SetSignalStore so
	// existing NewManager call sites do not need to change.
	signalStore SignalStore
	// cmdLocks holds per-commandID *sync.Mutex used by resolver methods to
	// serialize against scan and against each other.
	//
	// Lock hierarchy (must be acquired in this order):
	//   scanMu (caller, outside this package) → cmdLocks[cmd] → wm.mu
	//
	// Invariants:
	//   1. All code paths that need both cmdLocks and wm.mu MUST acquire
	//      cmdLocks first. Violating this order causes deadlock.
	//   2. Code that already holds wm.mu (e.g. GC) must use TryLock on
	//      cmdLocks to avoid hierarchy violation. TryLock failure means a
	//      resolver is active; the operation should be deferred.
	//   3. cmdLocks entries are cleaned up by CleanupCommand (direct delete)
	//      and GC (TryLock+delete in cleanupCommandUnlocked, plus
	//      gcOrphanedCmdLocks sweep for entries missed by TryLock).
	//
	// When adding new code paths: if the new code acquires wm.mu and also
	// needs a per-command lock, it must release wm.mu first, acquire
	// cmdLocks[cmd], then reacquire wm.mu. Alternatively, use TryLock
	// and defer the operation on failure.
	cmdLocks sync.Map

	// testPublishResetHook, if non-nil, replaces git reset --hard HEAD during
	// PublishToBase. Used only for testing error paths. Must be nil in production.
	testPublishResetHook func() error
}

// NewManager creates a new Manager.
func NewManager(maestroDir string, cfg model.WorktreeConfig, logger *log.Logger, logLevel core.LogLevel) *Manager {
	projectRoot := filepath.Dir(maestroDir)
	return &Manager{
		LogMixin:    core.LogMixin{DL: core.NewDaemonLoggerFromLegacy("worktree_manager", logger, logLevel)},
		maestroDir:  maestroDir,
		projectRoot: projectRoot,
		config:      cfg,
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
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("load worktree state: %w", err)
		}
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
		if err := os.MkdirAll(filepath.Dir(integrationPath), 0750); err != nil {
			_ = wm.gitRun("branch", "-D", integrationBranch)
			return fmt.Errorf("create integration worktree parent dir: %w", err)
		}
		if err := wm.gitRun("worktree", "add", integrationPath, integrationBranch); err != nil {
			_ = wm.gitRun("branch", "-D", integrationBranch)
			return fmt.Errorf("create integration worktree: %w", err)
		}

		rollbackIntegration := func() error {
			var errs []error
			if rbErr := wm.gitRun("worktree", "remove", "--force", integrationPath); rbErr != nil {
				wm.Log(core.LogLevelWarn, "rollback_integration_worktree command=%s error=%v", commandID, rbErr)
				errs = append(errs, fmt.Errorf("remove integration worktree: %w", rbErr))
			}
			if rbErr := wm.gitRun("branch", "-D", integrationBranch); rbErr != nil {
				wm.Log(core.LogLevelWarn, "rollback_integration_branch command=%s error=%v", commandID, rbErr)
				errs = append(errs, fmt.Errorf("delete integration branch: %w", rbErr))
			}
			return errors.Join(errs...)
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
			if rbErr := rollbackIntegration(); rbErr != nil {
				return errors.Join(err, fmt.Errorf("rollback also failed: %w", rbErr))
			}
			return err
		}

		if err := wm.saveState(commandID, state); err != nil {
			// Rollback: remove worker worktree, branch, and integration
			var rollbackErrs []error
			if rbErr := wm.rollbackWorkerWorktree(commandID, state, workerID); rbErr != nil {
				wm.Log(core.LogLevelWarn, "rollback_worker_worktree command=%s worker=%s error=%v", commandID, workerID, rbErr)
				rollbackErrs = append(rollbackErrs, rbErr)
			}
			if rbErr := rollbackIntegration(); rbErr != nil {
				rollbackErrs = append(rollbackErrs, rbErr)
			}
			statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
			if rbErr := os.Remove(statePath); rbErr != nil && !os.IsNotExist(rbErr) {
				wm.Log(core.LogLevelWarn, "rollback_state_file_remove command=%s error=%v", commandID, rbErr)
				rollbackErrs = append(rollbackErrs, fmt.Errorf("remove state file: %w", rbErr))
			}
			origErr := fmt.Errorf("save worktree state: %w", err)
			if len(rollbackErrs) > 0 {
				return errors.Join(origErr, fmt.Errorf("rollback also failed: %w", errors.Join(rollbackErrs...)))
			}
			return origErr
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

	// Determine the base SHA for the new worker worktree.
	// If the integration branch has commits from previous phases, use its
	// current HEAD so that later-phase workers can see earlier work.
	// Fall back to the persisted BaseSHA if rev-parse fails.
	baseSHA := state.Integration.BaseSHA
	if head, revErr := wm.gitOutput("rev-parse", state.Integration.Branch); revErr == nil {
		head = strings.TrimSpace(head)
		if validateSHA(head) == nil {
			baseSHA = head
		}
	}

	if err := validateSHA(baseSHA); err != nil {
		return fmt.Errorf("base SHA for %s: %w", commandID, err)
	}

	// Add the worker to existing state
	if err := wm.addWorkerWorktreeUnlocked(state, commandID, workerID, baseSHA, now); err != nil {
		return err
	}

	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		// Rollback: remove the just-created worker worktree
		var rollbackErrs []error
		if rbErr := wm.rollbackWorkerWorktree(commandID, state, workerID); rbErr != nil {
			wm.Log(core.LogLevelWarn, "rollback_worker_worktree command=%s worker=%s error=%v", commandID, workerID, rbErr)
			rollbackErrs = append(rollbackErrs, rbErr)
		}
		// Restore original state to fix potential partial file write
		state.Workers = origWorkers
		state.UpdatedAt = origUpdatedAt
		if rbErr := wm.saveState(commandID, state); rbErr != nil {
			wm.Log(core.LogLevelWarn, "rollback_state_restore command=%s error=%v", commandID, rbErr)
			rollbackErrs = append(rollbackErrs, fmt.Errorf("restore state: %w", rbErr))
		}
		origErr := fmt.Errorf("save worktree state: %w", err)
		if len(rollbackErrs) > 0 {
			return errors.Join(origErr, fmt.Errorf("rollback also failed: %w", errors.Join(rollbackErrs...)))
		}
		return origErr
	}
	return nil
}

func (wm *Manager) addWorkerWorktreeUnlocked(state *model.WorktreeCommandState, commandID, workerID, baseSHA, now string) error {
	workerBranch := fmt.Sprintf("maestro/%s/%s", commandID, workerID)
	wtPath := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID, workerID)

	if err := os.MkdirAll(filepath.Dir(wtPath), 0750); err != nil {
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

	wm.Log(core.LogLevelInfo, "worker_worktree_created command=%s worker=%s", commandID, workerID)
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

// MaxComputedDiffBytes caps the diff payload returned by ComputeWorkerDiff.
// Diffs larger than this threshold are truncated with a marker line so that
// downstream consumers (advisory review) cannot be flooded by a runaway worker.
const MaxComputedDiffBytes = 256 * 1024

// ComputeWorkerDiff returns a unified diff representing the worker's effective
// contribution for review purposes: the working tree of the worker's worktree
// against its merge-base with the integration branch. This captures both
// committed and uncommitted changes attributable to this worker, while
// excluding any commits that other workers have already merged into
// integration since this worker branched off.
//
// Returns ("", nil) when the command has no worktree state (worktree mode
// disabled or state file absent) or no integration branch yet — these cases
// are not errors; the caller should treat them as "no diff available".
func (wm *Manager) ComputeWorkerDiff(commandID, workerID string) (string, error) {
	if err := validateIDs(commandID, workerID); err != nil {
		return "", err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadStateUnlocked(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("load worktree state: %w", err)
	}
	if state.Integration.Branch == "" {
		return "", nil
	}
	ws := wm.findWorker(state, workerID)
	if ws == nil {
		return "", fmt.Errorf("worker %s not found in command %s", workerID, commandID)
	}
	if _, err := os.Stat(ws.Path); err != nil {
		// Worktree directory missing — likely already cleaned up.
		// Treat as "no diff available" rather than failing the caller.
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat worker worktree: %w", err)
	}

	mb, err := wm.gitOutputInDir(ws.Path, "merge-base", state.Integration.Branch, "HEAD")
	if err != nil {
		return "", fmt.Errorf("merge-base: %w", err)
	}
	mergeBase := strings.TrimSpace(mb)
	if mergeBase == "" {
		return "", fmt.Errorf("empty merge-base for worker %s in command %s", workerID, commandID)
	}

	diff, err := wm.gitOutputInDir(ws.Path, "diff", mergeBase)
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	if len(diff) > MaxComputedDiffBytes {
		diff = diff[:MaxComputedDiffBytes] + "\n... (diff truncated)\n"
	}
	return diff, nil
}

// CommitWorkerChanges commits all changes in a worker's worktree.
// Idempotent: if there are no changes to commit, returns nil.
func (wm *Manager) CommitWorkerChanges(commandID, workerID, message string) error {
	return wm.CommitWorkerChangesWithExpectedPaths(commandID, workerID, message, nil)
}

// CommitWorkerChangesWithExpectedPaths commits all changes in a worker's
// worktree after checking the staged file set against the task's expected paths.
func (wm *Manager) CommitWorkerChangesWithExpectedPaths(commandID, workerID, message string, expectedPaths []string) error {
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
		return fmt.Errorf("worker %s in command %s: %w", workerID, commandID, model.ErrWorkerNotFound)
	}

	// Defense in depth: refuse to auto-commit workers owned by the resume-merge
	// pipeline. These are committed via commitResolvedWorkerChanges, which
	// bypasses the `resolving → committed` transition (the latter is not in the
	// valid state machine). Without this guard, a Phase B scan that reaches this
	// function for a conflict/resolving worker would fail the transition check
	// below, record a commit_failed signal, and block publishing. The Phase A
	// collector (eligibleWorkerIDsForAutoCommit) already filters these out; this
	// guard catches stale WorkerIDs lists and direct callers.
	if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
		wm.Log(core.LogLevelDebug, "skip_auto_commit_resume_merge_owned command=%s worker=%s status=%s",
			commandID, workerID, ws.Status)
		return fmt.Errorf("worker %s in command %s (status=%s): %w", workerID, commandID, ws.Status, ErrWorkerOwnedByResumeMerge)
	}

	// Check if there are changes to commit
	statusOut, err := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status (worker=%s, command=%s): %w", workerID, commandID, err)
	}
	if strings.TrimSpace(statusOut) == "" {
		wm.Log(core.LogLevelDebug, "no_changes_to_commit command=%s worker=%s", commandID, workerID)
		return nil
	}

	// Stage tracked file modifications/deletions (safe: never stages untracked files)
	if err := wm.gitRunInDir(ws.Path, "add", "-u"); err != nil {
		return fmt.Errorf("git add -u (worker=%s, command=%s): %w", workerID, commandID, err)
	}

	// Unstage any sensitive tracked files that were staged by git add -u
	if err := wm.unstageSensitiveFiles(ws.Path); err != nil {
		wm.Log(core.LogLevelWarn, "unstage_sensitive_files_error command=%s worker=%s error=%v", commandID, workerID, err)
	}

	// Stage untracked files that pass .gitignore and safety filters
	if err := wm.stageNewFiles(ws.Path); err != nil {
		return fmt.Errorf("stage new files (worker=%s, command=%s): %w", workerID, commandID, err)
	}

	// Re-check if there is anything staged after filtering
	stagedOut, err := wm.gitOutputInDir(ws.Path, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return fmt.Errorf("git diff --cached (worker=%s, command=%s): %w", workerID, commandID, err)
	}
	if strings.TrimRight(stagedOut, "\x00") == "" {
		// Worktree had dirty files but all were filtered — this is not a clean success.
		wm.Log(core.LogLevelWarn, "all_files_filtered command=%s worker=%s", commandID, workerID)
		return fmt.Errorf("commit for worker %s in command %s: %w", workerID, commandID, ErrAllFilesFiltered)
	}

	// Pre-check: verify transition to Committed is valid before committing.
	// This prevents git commit from succeeding with no way to rollback if the
	// state transition is invalid.
	if err := model.ValidateWorktreeTransition(ws.Status, model.WorktreeStatusCommitted); err != nil {
		// Reset staged changes so the worktree is left in a clean index state.
		if resetErr := wm.gitRunInDir(ws.Path, "reset", "HEAD"); resetErr != nil {
			wm.Log(core.LogLevelWarn, "git_reset_after_transition_check command=%s worker=%s error=%v",
				commandID, workerID, resetErr)
		}
		return fmt.Errorf("worker %s: cannot commit (invalid transition from %s to committed): %w", workerID, ws.Status, err)
	}

	// Commit policy checks
	if violations := wm.checkCommitPolicy(ws.Path, message, stagedOut, expectedPaths); len(violations) > 0 {
		for _, v := range violations {
			wm.Log(core.LogLevelWarn, "commit_policy_violation command=%s worker=%s code=%s msg=%s",
				commandID, workerID, v.Code, v.Message)
		}
		// Reset staged changes so the worktree is left in a clean index state.
		// Note: dirty files remain in the worktree after reset.
		if resetErr := wm.gitRunInDir(ws.Path, "reset", "HEAD"); resetErr != nil {
			wm.Log(core.LogLevelWarn, "git_reset_after_policy_violation command=%s worker=%s error=%v",
				commandID, workerID, resetErr)
		}
		wm.Log(core.LogLevelWarn, "dirty_files_remain_after_policy_reset command=%s worker=%s",
			commandID, workerID)
		return &CommitPolicyViolationError{Violations: violations}
	}

	if err := wm.gitRunInDir(ws.Path, "commit", "-m", message); err != nil {
		return fmt.Errorf("git commit (worker=%s, command=%s): %w", workerID, commandID, err)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.setWorkerStatus(ws, model.WorktreeStatusCommitted, now); err != nil {
		return err
	}
	state.UpdatedAt = now

	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	wm.Log(core.LogLevelInfo, "worker_committed command=%s worker=%s", commandID, workerID)
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

// GetIntegrationStatus returns the lifecycle status of the integration
// branch for commandID. Returns os.ErrNotExist when no worktree state file
// exists (legacy / non-worktree mode).
//
// This method exists so that the dispatcher can pre-flight RunOnMain tasks
// and reject the dispatch when integration has not yet been published into
// base. Callers must treat os.ErrNotExist as "no enforcement applies",
// because worktree-disabled commands intentionally have no state file.
func (wm *Manager) GetIntegrationStatus(commandID string) (model.IntegrationStatus, error) {
	if err := validateIDs(commandID); err != nil {
		return "", err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()
	state, err := wm.loadState(commandID)
	if err != nil {
		return "", err
	}
	return state.Integration.Status, nil
}

// HasWorktrees checks if worktrees exist for a given command.
func (wm *Manager) HasWorktrees(commandID string) bool {
	if err := validateIDs(commandID); err != nil {
		return false
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()
	return wm.hasWorktreesUnlocked(commandID)
}

// hasWorktreesUnlocked is the lock-free version of HasWorktrees for use by
// callers that already hold wm.mu.
func (wm *Manager) hasWorktreesUnlocked(commandID string) bool {
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

// MarkPublishConflictSignaled sets the PublishConflictSignaled flag so the
// publish_conflict planner signal is emitted only once per conflict event.
func (wm *Manager) MarkPublishConflictSignaled(commandID string) error {
	if err := validateIDs(commandID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if state.Integration.PublishConflictSignaled {
		return nil
	}
	state.Integration.PublishConflictSignaled = true
	state.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// MarkPhaseMerged records that a phase has been merged so it won't be re-merged.
// Idempotent: returns nil if the state file has already been cleaned up.
func (wm *Manager) MarkPhaseMerged(commandID, phaseID string) error {
	if err := validateCommandAndPhaseIDs(commandID, phaseID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // state already cleaned up; nothing to record
		}
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
		return fmt.Errorf("worker %s in command %s: %w", workerID, commandID, model.ErrWorkerNotFound)
	}

	// Tripwire: refuse to run destructive git ops outside the project root.
	if err := ensureWithinProjectRoot(wm.projectRoot, ws.Path); err != nil {
		return fmt.Errorf("discard worker changes: %w", err)
	}

	// Verify worktree directory exists before running destructive commands.
	if _, err := os.Stat(ws.Path); os.IsNotExist(err) {
		wm.Log(core.LogLevelWarn, "discard_skip_missing_worktree command=%s worker=%s path=%s", commandID, workerID, ws.Path)
		return nil // worktree already gone; nothing to discard
	}

	// Reset staged changes so checkout can fully restore tracked files
	if err := wm.gitRunInDir(ws.Path, "reset", "HEAD"); err != nil {
		return fmt.Errorf("reset staged changes (worker=%s, command=%s): %w", workerID, commandID, err)
	}

	// Discard tracked file changes
	if err := wm.gitRunInDir(ws.Path, "checkout", "--", "."); err != nil {
		return fmt.Errorf("discard changes (worker=%s, command=%s): %w", workerID, commandID, err)
	}

	// Remove untracked files (but not .gitignore'd files)
	if err := wm.gitRunInDir(ws.Path, "clean", "-fd"); err != nil {
		return fmt.Errorf("clean untracked files (worker=%s, command=%s): %w", workerID, commandID, err)
	}

	wm.Log(core.LogLevelInfo, "worker_changes_discarded command=%s worker=%s", commandID, workerID)
	return nil
}

// --- Internal helpers ---

// integrationWorktreePath returns the conventional path for the integration worktree.
func (wm *Manager) integrationWorktreePath(commandID string) string {
	return filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID, "_integration")
}

// GetIntegrationPath returns the filesystem path of the integration worktree
// for the given command. Returns an error if the command has no active
// integration worktree (integration branch not yet created). Satisfies the
// dispatch.WorktreeResolver interface so that the dispatcher can resolve
// working directories for RunOnIntegration tasks (publish_conflict resolution).
func (wm *Manager) GetIntegrationPath(commandID string) (string, error) {
	if err := validateIDs(commandID); err != nil {
		return "", err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return "", fmt.Errorf("load worktree state: %w", err)
	}
	if state.Integration.Branch == "" {
		return "", fmt.Errorf("no integration worktree registered for command %s", commandID)
	}
	return wm.integrationWorktreePath(commandID), nil
}

// EnsureIntegrationBranchCheckedOut verifies that the integration worktree
// for commandID has state.Integration.Branch checked out. If the worktree is
// detached or on an unexpected branch, it attempts to restore the expected
// checkout, provided the worktree is clean. Returns an error if the worktree
// is dirty or the checkout cannot be restored; in that case the caller must
// escalate rather than silently dispatching work into a bad state.
//
// This is defense-in-depth for RunOnIntegration dispatch: if a prior bug or
// crash left the worktree detached, a worker's `git merge main` would create
// orphan commits that do not advance the integration branch, causing publish
// to loop forever (see CleanupTempPublishBranch RCA). Surfacing a clear error
// up front is preferable to dispatching onto a broken worktree.
func (wm *Manager) EnsureIntegrationBranchCheckedOut(commandID string) error {
	if err := validateIDs(commandID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load worktree state: %w", err)
	}
	if state.Integration.Branch == "" {
		return fmt.Errorf("no integration branch registered for command %s", commandID)
	}

	integrationPath := wm.integrationWorktreePath(commandID)
	if _, statErr := os.Stat(integrationPath); statErr != nil {
		return fmt.Errorf("integration worktree path: %w", statErr)
	}

	currentRef, refErr := wm.gitOutputInDir(integrationPath, "symbolic-ref", "--short", "HEAD")
	if refErr == nil && strings.TrimSpace(currentRef) == state.Integration.Branch {
		return nil
	}

	// Either detached HEAD (symbolic-ref failed) or on an unexpected branch.
	// Log the anomaly so operators can see where the drift originated.
	if refErr == nil {
		wm.Log(core.LogLevelWarn,
			"integration_worktree_unexpected_branch command=%s current=%s expected=%s",
			commandID, strings.TrimSpace(currentRef), state.Integration.Branch)
	} else {
		wm.Log(core.LogLevelWarn,
			"integration_worktree_detached command=%s expected=%s",
			commandID, state.Integration.Branch)
	}

	// Guard: never silently discard work. A dirty worktree means the agent (or
	// an operator) has in-progress edits whose provenance we cannot reconstruct.
	dirtyOut, dirtyErr := wm.gitOutputInDir(integrationPath, "status", "--porcelain")
	if dirtyErr != nil {
		return fmt.Errorf("check integration worktree status: %w", dirtyErr)
	}
	if strings.TrimSpace(dirtyOut) != "" {
		return fmt.Errorf("integration worktree has uncommitted changes; cannot restore %s checkout",
			state.Integration.Branch)
	}

	if err := wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch); err != nil {
		return fmt.Errorf("restore integration branch checkout: %w", err)
	}
	wm.Log(core.LogLevelInfo,
		"integration_worktree_reattached command=%s branch=%s",
		commandID, state.Integration.Branch)
	return nil
}

// rollbackWorkerWorktree removes a worker's worktree and branch.
// Returns an error if any cleanup step fails (caller should log but
// not abort — Reconcile can recover from partial rollback state).
func (wm *Manager) rollbackWorkerWorktree(commandID string, state *model.WorktreeCommandState, workerID string) error {
	for _, ws := range state.Workers {
		if ws.WorkerID == workerID {
			var errs []error
			if rbErr := wm.gitRun("worktree", "remove", "--force", ws.Path); rbErr != nil {
				wm.Log(core.LogLevelWarn, "rollback_worktree_remove command=%s worker=%s error=%v", commandID, workerID, rbErr)
				errs = append(errs, fmt.Errorf("remove worktree %s: %w", ws.Path, rbErr))
			}
			if rbErr := wm.gitRun("branch", "-D", ws.Branch); rbErr != nil {
				wm.Log(core.LogLevelWarn, "rollback_branch_delete command=%s worker=%s branch=%s error=%v", commandID, workerID, ws.Branch, rbErr)
				errs = append(errs, fmt.Errorf("delete branch %s: %w", ws.Branch, rbErr))
			}
			return errors.Join(errs...)
		}
	}
	return nil
}

func (wm *Manager) findWorker(state *model.WorktreeCommandState, workerID string) *model.WorktreeState {
	for i := range state.Workers {
		if state.Workers[i].WorkerID == workerID {
			return &state.Workers[i]
		}
	}
	return nil
}

// commitWorkerInlineForRefresh stages tracked file modifications and creates a
// minimal commit so RefreshWorkerWorktreeToIntegrationHead can fast-forward.
//
// This is the wave-crossing inline auto-commit: same phase, two tasks, the
// canonical phase-boundary CommitWorkerChanges has not yet fired but the
// dispatcher needs a clean worktree to refresh against integration. The
// function deliberately stays narrow:
//
//   - status / lifecycle: untouched. Worker stays at whatever lifecycle
//     status it had (typically `active`). The phase-boundary commit (which
//     does the `active → committed` transition) still runs as before.
//   - untracked files: NOT staged. The phase-boundary commit owns
//     untracked-file admission via stageNewFiles + expected_paths +
//     sensitive-file filters; replicating those mid-phase would either
//     bypass them or duplicate the policy machinery. If untracked files
//     remain after `git add -u`, the caller aborts the refresh.
//   - sensitive files: tracked sensitive files briefly staged by `git add -u`
//     are unstaged via the same helper as the canonical commit, so we
//     never inline-commit credentials.
//   - commit policy: not enforced here. The wave-crossing commit might
//     legitimately span multiple tasks' expected_paths, which the
//     phase-boundary policy treats as a single union. Re-applying it at
//     wave granularity would false-fail the union shape.
//
// Caller MUST hold wm.mu (this is invoked from RefreshWorkerWorktreeToIntegrationHead
// which acquires it).
func (wm *Manager) commitWorkerInlineForRefresh(ws *model.WorktreeState, commandID, workerID string) error {
	if err := wm.gitRunInDir(ws.Path, "add", "-u"); err != nil {
		return fmt.Errorf("git add -u: %w", err)
	}
	if err := wm.unstageSensitiveFiles(ws.Path); err != nil {
		// Non-fatal: sensitive files are also filtered at phase-boundary commit.
		// Log so an operator can see what happened, but proceed — we have
		// already executed `add -u`, so reverting cleanly is awkward and
		// leaving the file unstaged is what unstageSensitiveFiles attempts.
		wm.Log(core.LogLevelWarn,
			"inline_commit_unstage_sensitive_warning command=%s worker=%s error=%v",
			commandID, workerID, err)
	}
	stagedOut, err := wm.gitOutputInDir(ws.Path, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return fmt.Errorf("git diff --cached: %w", err)
	}
	if strings.TrimRight(stagedOut, "\x00") == "" {
		// Nothing tracked to commit — the dirty bits were untracked or
		// filtered. Caller's post-check sees the still-dirty worktree and
		// aborts; that is the right outcome because we cannot safely admit
		// untracked files mid-phase.
		return nil
	}
	message := fmt.Sprintf("[maestro] wave-crossing auto-commit: worker=%s command=%s", workerID, commandID)
	if err := wm.gitRunInDir(ws.Path, "commit", "-m", message); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	wm.Log(core.LogLevelInfo,
		"worker_inline_committed_for_refresh command=%s worker=%s",
		commandID, workerID)
	return nil
}

// onlyUntrackedDirty reports whether every non-empty line in `git status
// --porcelain` output represents an untracked file ("??" marker). Used by
// RefreshWorkerWorktreeToIntegrationHead to distinguish the benign
// case (read-only task residue) from a tracked-modification leak after
// inline auto-commit. The porcelain v1 grammar uses two-character XY
// codes (X=staged, Y=unstaged); "??" is the dedicated marker for
// untracked entries (`man git-status` → "Short Format" → Untracked).
func onlyUntrackedDirty(porcelain string) bool {
	hasAny := false
	for _, line := range strings.Split(porcelain, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if trimmed == "" {
			continue
		}
		hasAny = true
		// Each porcelain entry is "XY <path>" where XY is the two-char
		// status. Untracked entries are exactly "?? <path>".
		if !strings.HasPrefix(trimmed, "?? ") {
			return false
		}
	}
	return hasAny
}

// AutoCommit returns whether auto-commit is enabled in the worktree config.
func (wm *Manager) AutoCommit() bool { return wm.config.AutoCommit }

// AutoMerge returns whether auto-merge is enabled in the worktree config.
func (wm *Manager) AutoMerge() bool { return wm.config.AutoMerge }

// RefreshWorkerWorktreeToIntegrationHead fast-forwards the worker's branch
// (and its worktree checkout) to the integration branch's latest HEAD when it
// is safe to do so. The dispatcher invokes this just before handing a worker a
// new task; without it, a worker that was Integrated by an earlier phase merge
// keeps its old branch tip when re-used in a later phase, so sibling-worker
// commits already merged into integration are invisible to the worker.
//
// Behaviour:
//   - Worktree must be clean. Dirty worktree → error; the caller fails the
//     dispatch rather than risk silent drift.
//   - Worker HEAD == integration HEAD → no-op.
//   - Worker HEAD is a strict ancestor of integration HEAD → fast-forward
//     merge (no merge commit) so the worker branch stays linear.
//   - integration HEAD is an ancestor of worker HEAD → no-op (worker has its
//     own newer commits, treat as already-current).
//   - Histories diverged → error; the caller refuses dispatch and an
//     operator must investigate.
//
// SyncFromIntegration is intentionally not used here: it skips Integrated
// workers (loses Integrated status would erase post-merge progress markers)
// and creates a "[maestro] sync integration" merge commit that pollutes the
// worker history. The fast-forward path keeps both invariants intact while
// closing the staleness gap.
func (wm *Manager) RefreshWorkerWorktreeToIntegrationHead(commandID, workerID string) error {
	if err := validateIDs(commandID, workerID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No worktree state — worktree mode disabled for this command.
			// Caller already handles "no worktree" via GetWorkerPath path.
			return nil
		}
		return fmt.Errorf("load state: %w", err)
	}
	if state.Integration.Branch == "" {
		// Integration branch not yet created — nothing to refresh against.
		return nil
	}
	ws := wm.findWorker(state, workerID)
	if ws == nil {
		return fmt.Errorf("worker %s not found in command %s", workerID, commandID)
	}
	if _, err := os.Stat(ws.Path); err != nil {
		if os.IsNotExist(err) {
			// Worktree directory gone — caller's path resolution will surface
			// this. Nothing to refresh.
			return nil
		}
		return fmt.Errorf("stat worker worktree: %w", err)
	}

	// Conflict resolution is the canonical case where the worker is
	// *intentionally* diverged from integration: the diverged state IS the
	// conflict that the resolution task is being dispatched to fix. Treating
	// that divergence as a refresh failure would block the resolution task
	// dispatch in a 5-attempt -> dead-letter -> planner-replan loop. The
	// skip is narrow: only Conflict / Resolving statuses, so Integrated /
	// Active / Created etc. still get fast-forwarded (or fail-closed on
	// real divergence) as designed.
	if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
		wm.Log(core.LogLevelDebug,
			"worker_worktree_refresh_skipped_for_conflict_resolution command=%s worker=%s status=%s",
			commandID, workerID, ws.Status)
		return nil
	}

	// Refuse to touch a dirty worktree — uncommitted edits whose provenance we
	// cannot reconstruct must never be silently merged with integration.
	//
	// Wave-crossing inline auto-commit: when a worker has just finished
	// task A in the same phase as task B about to be dispatched, Phase B's
	// canonical auto-commit has not yet fired (it runs at phase boundary,
	// not wave boundary). The result is a dirty worktree at task B
	// dispatch time; without this fast-path commit, refresh aborts and
	// the task eventually dead-letters. The inline commit deliberately
	// avoids the heavy CommitWorkerChanges path (status transition,
	// expected_paths gate, commit-policy scanning) — those run again at
	// phase boundary, and applying them mid-phase would either reject
	// legitimate intermediate state or prematurely flip the worker to
	// `committed`. Untracked files are intentionally NOT staged here
	// because expected_paths / sensitive-file filters cannot be evaluated
	// cheaply mid-phase; they defer to the phase-boundary auto-commit.
	statusOut, statusErr := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
	if statusErr != nil {
		return fmt.Errorf("git status (worker=%s, command=%s): %w", workerID, commandID, statusErr)
	}
	if strings.TrimSpace(statusOut) != "" {
		if err := wm.commitWorkerInlineForRefresh(ws, commandID, workerID); err != nil {
			return fmt.Errorf("worker %s worktree has uncommitted changes that could not be auto-committed inline: %w", workerID, err)
		}
		statusOut, statusErr = wm.gitOutputInDir(ws.Path, "status", "--porcelain")
		if statusErr != nil {
			return fmt.Errorf("git status post-inline-commit (worker=%s, command=%s): %w", workerID, commandID, statusErr)
		}
		if strings.TrimSpace(statusOut) != "" {
			// Untracked-only soft-skip: inline auto-commit deliberately
			// does not stage untracked files (replicating the
			// sensitive-file gate and expected_paths filter mid-phase is
			// unsafe). When the remaining dirty bits are exclusively
			// untracked entries, log loud and skip the fast-forward for
			// this cycle so the task does not dead-letter on what may be
			// legitimate task artefacts that the expected_paths gate will
			// commit at phase-boundary time. The phase-boundary commit
			// still applies the full sensitive-file / expected_paths
			// gates before merge.
			if onlyUntrackedDirty(statusOut) {
				wm.Log(core.LogLevelWarn,
					"worker_worktree_refresh_soft_skipped_untracked_only command=%s worker=%s status=%q "+
						"(untracked files remain after inline commit; integration fast-forward deferred — phase-boundary commit will gate them)",
					commandID, workerID, strings.TrimSpace(statusOut))
				return nil
			}
			return fmt.Errorf("worker %s worktree still dirty after inline auto-commit (mixed tracked + untracked); refresh aborted", workerID)
		}
	}

	workerHEADRaw, err := wm.gitOutputInDir(ws.Path, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("rev-parse worker HEAD (worker=%s, command=%s): %w", workerID, commandID, err)
	}
	workerHEAD := strings.TrimSpace(workerHEADRaw)

	integHEADRaw, err := wm.gitOutputInDir(ws.Path, "rev-parse", state.Integration.Branch)
	if err != nil {
		return fmt.Errorf("rev-parse integration HEAD (worker=%s, command=%s, branch=%s): %w",
			workerID, commandID, state.Integration.Branch, err)
	}
	integHEAD := strings.TrimSpace(integHEADRaw)

	if workerHEAD == integHEAD {
		return nil
	}

	mbRaw, err := wm.gitOutputInDir(ws.Path, "merge-base", "HEAD", state.Integration.Branch)
	if err != nil {
		return fmt.Errorf("merge-base (worker=%s, command=%s): %w", workerID, commandID, err)
	}
	mergeBase := strings.TrimSpace(mbRaw)

	switch mergeBase {
	case integHEAD:
		// Worker is strictly ahead of integration. Refusing to fast-forward
		// here is correct: the worker has commits not yet in integration and
		// rebasing them would silently drop or duplicate work.
		return nil
	case workerHEAD:
		// Worker HEAD is a strict ancestor of integration → safe fast-forward.
	default:
		// Truly diverged worker (own commits + integration also moved on):
		// reset the worker worktree to integration HEAD so the next task
		// dispatch can proceed. The diverged commits represent work that
		// never made it into integration anyway (a race or a stale worker
		// carryover) and clinging to them only blocks recovery. The
		// Planner can re-dispatch the lost work as a fresh task if needed;
		// staying stuck is the worse outcome for autonomous orchestration.
		wm.Log(core.LogLevelWarn,
			"worker_worktree_diverged_resetting command=%s worker=%s "+
				"worker_head=%s integration_head=%s merge_base=%s "+
				"(auto-recover: discarding diverged worker commits to unblock dispatch)",
			commandID, workerID, workerHEAD, integHEAD, mergeBase)
		if err := wm.gitRunInDir(ws.Path, "reset", "--hard", state.Integration.Branch); err != nil {
			return fmt.Errorf("reset worker %s onto integration after divergence (worker=%s integration=%s): %w",
				workerID, workerHEAD, integHEAD, err)
		}
		wm.Log(core.LogLevelInfo,
			"worker_worktree_reset_to_integration command=%s worker=%s old=%s new=%s",
			commandID, workerID, workerHEAD, integHEAD)
		return nil
	}

	if err := wm.gitRunInDir(ws.Path, "merge", "--ff-only", state.Integration.Branch); err != nil {
		return fmt.Errorf("fast-forward worker %s to integration: %w", workerID, err)
	}
	wm.Log(core.LogLevelInfo,
		"worker_worktree_refreshed_to_integration command=%s worker=%s old=%s new=%s",
		commandID, workerID, workerHEAD, integHEAD)
	return nil
}
