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
		// gitWorktreeAddWithUnstattableFallback handles both:
		//   (a) the unstattable-file fallback (sandbox / SIP denying
		//       stat on a tracked file like .env.example), retrying
		//       with --no-checkout + skip-worktree marks; and
		//   (b) the partial-directory cleanup so a follow-up retry
		//       does not hit "fatal: '<path>' already exists".
		// Both pathologies were observed in 2026-05-06 P0-A/B reports.
		if err := wm.gitWorktreeAddWithUnstattableFallback(commandID,
			[]string{"worktree", "add", integrationPath, integrationBranch}); err != nil {
			// Final cleanup pass in case the fallback also failed.
			if rmErr := os.RemoveAll(integrationPath); rmErr != nil {
				wm.Log(core.LogLevelWarn,
					"integration_worktree_partial_cleanup_failed command=%s path=%s error=%v",
					commandID, integrationPath, rmErr)
			}
			_ = wm.gitRun("worktree", "prune", "-v")
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

	if err := wm.gitWorktreeAddWithUnstattableFallback(commandID,
		[]string{"worktree", "add", "-b", workerBranch, wtPath, baseSHA}); err != nil {
		// Final cleanup pass in case the fallback also failed.
		if rmErr := os.RemoveAll(wtPath); rmErr != nil {
			wm.Log(core.LogLevelWarn,
				"worker_worktree_partial_cleanup_failed command=%s worker=%s path=%s error=%v",
				commandID, workerID, wtPath, rmErr)
		}
		_ = wm.gitRun("worktree", "prune", "-v")
		_ = wm.gitRun("branch", "-D", workerBranch)
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

// largeCommitObservationThreshold is the staged-file count at which
// CommitWorkerChanges emits a loud "large commit" warning. Purely
// observational — the orchestrator never blocks the commit on file count
// (that would be language-specific gating which we removed deliberately).
// 100 is a reasonable threshold: a typical worker task touches <20 files;
// a sweep of 100+ usually means a build artefact directory slipped into
// the staging set because .gitignore is missing an entry.
const largeCommitObservationThreshold = 100

// ComputeWorkerDiff returns a unified diff representing the worker's effective
// contribution for review purposes: the working tree of the worker's worktree
// against its merge-base with the integration branch. Captures committed,
// uncommitted-tracked AND untracked changes — review must see everything the
// worker produced, including new files, otherwise tools like codex review
// false-positive on "missing" content that just hasn't been committed yet.
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
		// Worker has no worktree entry — typical for RunOnIntegration /
		// RunOnMain tasks (they execute on the integration worktree or
		// project root, never spawning a per-worker tree). Returning
		// ("", nil) lets the review coordinator fall back to the
		// summary payload, which is the correct outcome for such tasks
		// (their diff lives on the shared branch already and is not
		// attributable to a single worker).
		return "", nil
	}
	if _, err := os.Stat(ws.Path); err != nil {
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

	// Untracked files are not visible to `git diff <base>`. Add them to the
	// index intent so the diff reports them as additions, then reset the
	// intent afterwards so we never mutate the worker's actual index state.
	untrackedRaw, err := wm.gitOutputInDir(ws.Path, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", fmt.Errorf("list untracked files for diff: %w", err)
	}
	var untracked []string
	for _, name := range strings.Split(untrackedRaw, "\x00") {
		if name != "" {
			untracked = append(untracked, name)
		}
	}
	if len(untracked) > 0 {
		addArgs := append([]string{"add", "--intent-to-add", "--"}, untracked...)
		if addErr := wm.gitRunInDir(ws.Path, addArgs...); addErr != nil {
			// Non-fatal: fall back to base diff without untracked content.
			wm.Log(core.LogLevelWarn,
				"compute_worker_diff_intent_to_add_failed command=%s worker=%s error=%v",
				commandID, workerID, addErr)
		} else {
			// Restore the worker's index after the diff so we never mutate
			// its actual staging state.
			defer func() {
				resetArgs := append([]string{"reset", "--"}, untracked...)
				if rErr := wm.gitRunInDir(ws.Path, resetArgs...); rErr != nil {
					wm.Log(core.LogLevelDebug,
						"compute_worker_diff_intent_to_add_reset_failed command=%s worker=%s error=%v",
						commandID, workerID, rErr)
				}
			}()
		}
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

// CommitWorkerChanges commits every change a worker produced in its worktree
// — modifications, deletions, AND untracked files. Idempotent: returns nil
// when nothing is dirty.
//
// Autonomous LLM Orchestration policy: we deliberately do NOT gate on path
// scopes, commit-message regex, sensitive-file lists or .gitignore presence.
// The orchestrator's job is to capture worker output verbatim so phase
// boundary commits never silently leak modifications. Per-environment safety
// (e.g. "never stage credentials") is the worker's responsibility, configured
// once globally (~/.claude rules, repo .gitignore) rather than re-implemented
// here as a noisy gate that blocks legitimate work.
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
		return fmt.Errorf("worker %s in command %s: %w", workerID, commandID, model.ErrWorkerNotFound)
	}

	// Defense in depth: refuse to auto-commit workers owned by the resume-merge
	// pipeline. Those are committed via commitResolvedWorkerChanges, which
	// bypasses the `resolving → committed` transition.
	if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
		wm.Log(core.LogLevelDebug, "skip_auto_commit_resume_merge_owned command=%s worker=%s status=%s",
			commandID, workerID, ws.Status)
		return fmt.Errorf("worker %s in command %s (status=%s): %w", workerID, commandID, ws.Status, ErrWorkerOwnedByResumeMerge)
	}

	statusOut, err := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status (worker=%s, command=%s): %w", workerID, commandID, err)
	}
	if strings.TrimSpace(statusOut) == "" {
		wm.Log(core.LogLevelDebug, "no_changes_to_commit command=%s worker=%s", commandID, workerID)
		return nil
	}

	// `git add -A` captures modifications, deletions and untracked entries
	// in a single pass. Repository-level .gitignore is honoured.
	//
	// OS-level un-stat-able files (typically macOS sandbox or POSIX
	// permission denials, e.g. ~/.claude rules denying `/**/.env*` reads
	// when a worker creates `.env.example`) cause `git add -A` to fail
	// with "fatal: unable to stat '...': Operation not permitted". The
	// content is genuinely unreachable to git — no commit can include
	// it — so the orchestrator's only autonomous path forward is to
	// exclude the offending paths from the index and proceed with what
	// remains. We isolate the names from the error output, append them
	// to the repo-shared .git/info/exclude (the common-dir copy, in a
	// command-tagged marker block; never touches the repo's committed
	// .gitignore), and retry. Bounded retries prevent runaway loops on
	// truly unrecoverable file-system errors.
	if err := wm.gitAddAllWithUnstattableFallback(ws.Path, commandID, workerID); err != nil {
		return fmt.Errorf("git add -A (worker=%s, command=%s): %w", workerID, commandID, err)
	}

	stagedOut, err := wm.gitOutputInDir(ws.Path, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return fmt.Errorf("git diff --cached (worker=%s, command=%s): %w", workerID, commandID, err)
	}
	if strings.TrimRight(stagedOut, "\x00") == "" {
		// status reported dirty bits but `add -A` produced no staged change:
		// the only remaining content is gitignored. Treat as a clean no-op so
		// the phase merge proceeds; nothing the orchestrator should track.
		wm.Log(core.LogLevelDebug,
			"commit_skip_only_ignored command=%s worker=%s status_lines=%d",
			commandID, workerID, len(strings.Split(strings.TrimSpace(statusOut), "\n")))
		return nil
	}

	stagedCount := strings.Count(strings.TrimRight(stagedOut, "\x00"), "\x00") + 1
	if stagedCount >= largeCommitObservationThreshold {
		// Loud observability hook for "git add -A is sweeping in build
		// artefacts" situations. Log only — autonomous LLM Orchestration
		// must not silently strip files based on language-specific
		// heuristics; the right place to fix it is the repo's .gitignore
		// or the worker's environment. This log makes the situation
		// findable in retrospective debugging.
		wm.Log(core.LogLevelWarn,
			"large_commit_observed command=%s worker=%s staged_count=%d "+
				"(consider repo-level .gitignore or worker-side scratch-file cleanup)",
			commandID, workerID, stagedCount)
	}

	if err := model.ValidateWorktreeTransition(ws.Status, model.WorktreeStatusCommitted); err != nil {
		if resetErr := wm.gitRunInDir(ws.Path, "reset", "HEAD"); resetErr != nil {
			wm.Log(core.LogLevelWarn, "git_reset_after_transition_check command=%s worker=%s error=%v",
				commandID, workerID, resetErr)
		}
		return fmt.Errorf("worker %s: cannot commit (invalid transition from %s to committed): %w", workerID, ws.Status, err)
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

// IsWorkerAheadOrDirty reports whether a worker has content not yet present
// in the integration branch — either uncommitted dirt in the worktree or
// committed-but-unmerged commits ahead of integration HEAD. Used by the
// RunOnIntegration / RunOnMain pre-merge gate to decide whether the dep
// worker still needs to be merged.
//
// A pure status check (`worker.Status == integrated`) is insufficient
// because phase merges flip the status to `integrated` once, and the same
// worker re-used in a later phase can produce uncommitted edits without
// the status reverting. Inspecting the working tree + HEAD/merge-base
// directly is the source of truth.
//
// Returns:
//   - (true, nil) when the worker has dirt or commits not in integration
//   - (false, nil) when the worker is in lockstep with integration
//   - (false, nil) for Conflict/Resolving (resume_merge owns)
//   - (true, err) when a git probe fails — caller should treat as "needs
//     merge" because falling through the gate would dispatch a task
//     against a possibly-stale tree.
func (wm *Manager) IsWorkerAheadOrDirty(commandID, workerID string) (bool, error) {
	if err := validateIDs(commandID, workerID); err != nil {
		return false, err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return true, fmt.Errorf("load state: %w", err)
	}
	ws := wm.findWorker(state, workerID)
	if ws == nil {
		// Worker not in state — nothing to merge.
		return false, nil
	}
	if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
		// resume_merge pipeline owns this worker
		return false, nil
	}
	if ws.Path == "" {
		return false, nil
	}
	if _, statErr := os.Stat(ws.Path); statErr != nil {
		// Worktree directory missing (cleanup_done or never created):
		// nothing dirty / nothing to merge.
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return true, fmt.Errorf("stat worker worktree: %w", statErr)
	}

	statusOut, err := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
	if err != nil {
		return true, fmt.Errorf("git status worker=%s: %w", workerID, err)
	}
	if strings.TrimSpace(statusOut) != "" {
		return true, nil
	}

	if state.Integration.Branch == "" {
		// No integration branch yet (very early state): nothing to merge.
		return false, nil
	}
	workerHEAD, err := wm.gitOutputInDir(ws.Path, "rev-parse", "HEAD")
	if err != nil {
		return true, fmt.Errorf("rev-parse worker HEAD worker=%s: %w", workerID, err)
	}
	integHEAD, err := wm.gitOutputInDir(ws.Path, "rev-parse", state.Integration.Branch)
	if err != nil {
		return true, fmt.Errorf("rev-parse integration HEAD worker=%s: %w", workerID, err)
	}
	wHead := strings.TrimSpace(workerHEAD)
	iHead := strings.TrimSpace(integHEAD)
	if wHead == iHead {
		return false, nil
	}
	mb, err := wm.gitOutputInDir(ws.Path, "merge-base", wHead, iHead)
	if err != nil {
		return true, fmt.Errorf("merge-base worker=%s: %w", workerID, err)
	}
	mergeBase := strings.TrimSpace(mb)
	// Worker is ahead when its tip's merge-base with integration is
	// integration HEAD itself (worker has commits past integration).
	if mergeBase == iHead && wHead != iHead {
		return true, nil
	}
	// Diverged or strictly behind: not ahead. The diverged case is rare
	// and is handled by RefreshWorkerWorktreeToIntegrationHead's
	// auto-reset path; we don't redundantly trigger a merge here.
	return false, nil
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
	return wm.ensureIntegrationBranchCheckedOutLocked(state, commandID)
}

// ensureIntegrationBranchCheckedOutLocked is the lock-held core of
// EnsureIntegrationBranchCheckedOut. Caller MUST hold wm.mu and pass the
// already-loaded state. Split out so publish-path callers that already hold
// the lock (forwardMergeBaseToIntegration via PublishToBase) can reuse the
// check without re-entering the non-reentrant mutex.
func (wm *Manager) ensureIntegrationBranchCheckedOutLocked(state *model.WorktreeCommandState, commandID string) error {
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

// commitWorkerInlineForRefresh stages every dirty change (tracked,
// untracked, deletions) and creates a minimal commit so
// RefreshWorkerWorktreeToIntegrationHead can fast-forward.
//
// Wave-crossing context: same phase, two consecutive tasks on the same
// worker, the canonical phase-boundary commit has not yet fired but the
// dispatcher needs a clean worktree to refresh against integration. We
// commit here the same way the phase-boundary path does — no scope gate,
// no sensitive-file filter — because once the worker has produced files,
// the orchestrator's job is to capture them. Dropping any of them would
// reappear as "dirty residue" the next time the worker is reused and
// stall the merge.
//
// Returns committed=true when a real commit was created (i.e. the worker
// branch advanced). The caller uses this to move the worker out of any
// post-merge Integrated status — otherwise mergeWorkerBranch short-circuits
// an Integrated worker as "already merged" and the freshly committed work is
// silently dropped from integration.
//
// Caller MUST hold wm.mu (invoked from RefreshWorkerWorktreeToIntegrationHead).
func (wm *Manager) commitWorkerInlineForRefresh(ws *model.WorktreeState, commandID, workerID string) (committed bool, err error) {
	if err := wm.gitAddAllWithUnstattableFallback(ws.Path, commandID, workerID); err != nil {
		return false, fmt.Errorf("git add -A: %w", err)
	}
	stagedOut, err := wm.gitOutputInDir(ws.Path, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return false, fmt.Errorf("git diff --cached: %w", err)
	}
	if strings.TrimRight(stagedOut, "\x00") == "" {
		// Only ignored content was dirty; nothing to commit.
		return false, nil
	}
	message := fmt.Sprintf("[maestro] wave-crossing auto-commit: worker=%s command=%s", workerID, commandID)
	if err := wm.gitRunInDir(ws.Path, "commit", "-m", message); err != nil {
		return false, fmt.Errorf("git commit: %w", err)
	}
	wm.Log(core.LogLevelInfo,
		"worker_inline_committed_for_refresh command=%s worker=%s",
		commandID, workerID)
	return true, nil
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

	// Wave-crossing inline auto-commit: when a worker has just finished
	// task A in the same phase as task B about to be dispatched, Phase B's
	// canonical phase-boundary commit has not yet fired. The inline commit
	// captures the worker's full output (including untracked files) in a
	// single `git add -A`, so the worktree is clean before task B runs and
	// no dirty residue can carry into the next phase.
	statusOut, statusErr := wm.gitOutputInDir(ws.Path, "status", "--porcelain")
	if statusErr != nil {
		return fmt.Errorf("git status (worker=%s, command=%s): %w", workerID, commandID, statusErr)
	}
	if strings.TrimSpace(statusOut) != "" {
		committed, err := wm.commitWorkerInlineForRefresh(ws, commandID, workerID)
		if err != nil {
			return fmt.Errorf("worker %s worktree has uncommitted changes that could not be auto-committed inline: %w", workerID, err)
		}
		if committed {
			// The inline commit advanced the worker branch ahead of
			// integration. A worker re-used across phases is still at
			// WorktreeStatusIntegrated here; mergeWorkerBranch short-circuits
			// an Integrated worker as "already merged" BEFORE checking for new
			// commits, so without this transition the just-committed work is
			// silently dropped from integration at the next phase boundary.
			// Integrated/Active/Committed/Created → Committed are all valid
			// transitions; for any unexpected (terminal) status we log and
			// proceed rather than fail the dispatch.
			now := wm.clock.Now().UTC().Format(time.RFC3339)
			if tErr := model.ValidateWorktreeTransition(ws.Status, model.WorktreeStatusCommitted); tErr != nil {
				wm.Log(core.LogLevelWarn,
					"worker_inline_refresh_status_advance_skipped command=%s worker=%s from=%s error=%v",
					commandID, workerID, ws.Status, tErr)
			} else if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusCommitted, now); tErr != nil {
				return fmt.Errorf("worker %s: advance status after inline refresh commit: %w", workerID, tErr)
			} else {
				state.UpdatedAt = now
				if sErr := wm.saveState(commandID, state); sErr != nil {
					return fmt.Errorf("save state after inline refresh commit (worker=%s): %w", workerID, sErr)
				}
			}
		}
		statusOut, statusErr = wm.gitOutputInDir(ws.Path, "status", "--porcelain")
		if statusErr != nil {
			return fmt.Errorf("git status post-inline-commit (worker=%s, command=%s): %w", workerID, commandID, statusErr)
		}
		if strings.TrimSpace(statusOut) != "" {
			// `add -A` should consume every dirty entry except .gitignore
			// matches. If anything remains it must be ignored content;
			// log it for observability but proceed — refusing to refresh
			// here would stall the dispatch chain on benign residue that
			// will never go away on its own.
			wm.Log(core.LogLevelDebug,
				"worker_worktree_refresh_residual_ignored_dirty command=%s worker=%s status=%q",
				commandID, workerID, strings.TrimSpace(statusOut))
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
