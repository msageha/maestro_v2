package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// resolverGitTimeout caps git operations issued by the resolver pipeline so a
// hung integration worktree cannot stall the daemon scan loop. This is
// intentionally independent from wm.gitTimeout() (which is config-driven and
// shared across all git ops) so the resolver always has a tight upper bound.
const resolverGitTimeout = 60 * time.Second

// ComputeConflictGeneration derives a deterministic identifier for a single
// merge_conflict instance. The generation changes whenever any input shifts
// (integration HEAD, worker SHA, phase/worker identity), which lets the
// resolver code path use it as a CAS token: a stale resolver attempt against a
// re-detected conflict will fail with errConflictGenerationMismatch instead of
// silently overwriting newer state.
func ComputeConflictGeneration(commandID, phaseID, workerID, baseRef, oursRef, theirsRef string) string {
	h := sha256.New()
	// NUL-separator prevents accidental field collisions.
	for _, s := range []string{commandID, phaseID, workerID, baseRef, oursRef, theirsRef} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// errConflictGenerationMismatch is returned by the resolver entry points when
// the supplied conflict generation no longer matches the signal stored on
// disk — typically because the integration HEAD or worker SHA shifted under
// the resolver. The caller is expected to abort and re-fetch fresh state.
var errConflictGenerationMismatch = errors.New("conflict generation mismatch")

// errSignalStoreUnavailable indicates that the Manager was constructed
// without a SignalStore, so resolver methods cannot persist their CAS
// updates. The caller should ensure the daemon wires a store via
// SetSignalStore before invoking resolver entry points.
var errSignalStoreUnavailable = errors.New("signal store not configured on Manager")

// SignalStore abstracts read/modify/write access to the planner signal queue
// for a single command. The resolver code path needs:
//
//   - to look up the current ConflictGeneration / ResolutionState / attempt
//     count for a (commandID, phaseID, workerID, kind=merge_conflict) signal
//   - to atomically update those fields after dispatching, on success or on
//     failure of the resolver commit.
//
// The contract is intentionally narrow so the daemon can supply a thin
// wrapper over its existing planner_signals.yaml IO without exposing the full
// QueueHandler. The Manager calls these methods while holding the per-command
// resolver lock, so the implementation only needs to guarantee its own
// internal serialization (file lock or similar).
type SignalStore interface {
	// UpdateMergeConflictSignal performs a read-modify-write on the
	// merge_conflict signal matching (commandID, phaseID, workerID). The
	// supplied callback receives a pointer to the in-memory signal copy and
	// may mutate any of its resolution-lifecycle fields. If the callback
	// returns an error, no write is performed and that error is propagated.
	// If no matching signal exists the callback is invoked with nil and is
	// expected to return an error to abort.
	UpdateMergeConflictSignal(commandID, phaseID, workerID string, fn func(*model.PlannerSignal) error) error
}

// SetSignalStore wires the planner-signal store used by resolver methods.
// Safe to call once during daemon startup. nil disables resolver entry
// points (they will return errSignalStoreUnavailable).
//
// Concurrency note: callers must invoke this before any resolver method runs;
// it does not take wm.mu.
func (wm *Manager) SetSignalStore(s SignalStore) {
	wm.signalStore = s
}

// commandLock returns the per-command sync.Mutex used to serialize
// conflict-resolution mutations against scan and against other resolver calls
// for the same command. Uses sync.Map.LoadOrStore so concurrent first-touches
// converge on the same instance.
//
// Locking order: callers MUST NOT hold wm.mu when acquiring this lock.
// wm.mu is a coarse, fast-released lock for git/state IO; the per-command
// lock is held across the full resolver dispatch/commit so it must sit
// strictly above wm.mu in the lock hierarchy.
func (wm *Manager) commandLock(commandID string) *sync.Mutex {
	if v, ok := wm.cmdLocks.Load(commandID); ok {
		return v.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	actual, _ := wm.cmdLocks.LoadOrStore(commandID, m)
	return actual.(*sync.Mutex)
}

// DispatchConflictResolution transitions a worker from conflict→resolving and
// marks the corresponding planner signal as dispatched. CAS-protected by
// conflictGen: if the current signal's ConflictGeneration differs, returns
// errConflictGenerationMismatch and no state is mutated.
//
// Locking note: scanMu (owned by the daemon scanner outside this package) MUST
// be held by the caller for the duration of this call to keep scan from
// racing the worker state transition. This method itself acquires the
// per-command resolver lock and then wm.mu.
//
// Lifecycle note: completion of the conflict resolution is driven externally
// via the resolve_conflict CLI op (cmd_plan.runResolveConflict →
// plan_handler.resolve_conflict → recover.ResolveConflict). That path clears
// CommitFailedWorkers, resets MergeFailureCount, and clears the lingering
// merge_conflict signal (H3). The daemon does not perform the integration
// commit itself; the resolver agent (or operator) commits in the integration
// worktree before invoking resolve_conflict.
func (wm *Manager) DispatchConflictResolution(commandID, phaseID, workerID, conflictGen string) error {
	if err := validateCommandAndPhaseIDs(commandID, phaseID); err != nil {
		return err
	}
	if err := validate.ID(workerID); err != nil {
		return fmt.Errorf("invalid workerID: %w", err)
	}
	if wm.signalStore == nil {
		return errSignalStoreUnavailable
	}

	cl := wm.commandLock(commandID)
	cl.Lock()
	defer cl.Unlock()

	// CAS + mark dispatched on the signal first; if generation has shifted we
	// fail before mutating worktree state so the worker stays in conflict.
	if err := wm.signalStore.UpdateMergeConflictSignal(commandID, phaseID, workerID, func(sig *model.PlannerSignal) error {
		if sig == nil {
			return fmt.Errorf("merge_conflict signal not found for command=%s phase=%s worker=%s", commandID, phaseID, workerID)
		}
		if sig.ConflictGeneration != conflictGen {
			return errConflictGenerationMismatch
		}
		sig.ResolutionState = "dispatched"
		sig.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
		return nil
	}); err != nil {
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
	now := wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.setWorkerStatus(ws, model.WorktreeStatusResolving, now); err != nil {
		return err
	}
	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		// Best-effort signal revert so we don't leave ResolutionState=dispatched
		// while the worker is still in conflict (split-brain). C3: also count
		// this as a resolve attempt so repeated dispatch crashes do not loop
		// forever.
		if revErr := wm.signalStore.UpdateMergeConflictSignal(commandID, phaseID, workerID, func(sig *model.PlannerSignal) error {
			if sig == nil {
				return nil
			}
			sig.ResolutionState = ""
			sig.ResolveAttempt++
			sig.LastResolutionError = fmt.Sprintf("dispatch save_state failed: %v", err)
			sig.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
			return nil
		}); revErr != nil {
			wm.Log(core.LogLevelError, "signal_revert_after_save_failed command=%s worker=%s err=%v", commandID, workerID, revErr)
		}
		return fmt.Errorf("save state: %w", err)
	}
	wm.Log(core.LogLevelInfo, "conflict_resolution_dispatched command=%s phase=%s worker=%s", commandID, phaseID, workerID)
	return nil
}

// DiscardResolverEdits discards any in-progress resolver edits in the
// integration worktree for the given command. Mirrors DiscardWorkerChanges
// but operates on the integration worktree because that is where resolver
// edits live (see SSOT). The workerID is accepted for symmetry / future
// extensibility but is currently only used for logging.
//
// Locking note: caller must hold scanMu. This method takes the per-command
// resolver lock and then wm.mu for the (read-only) state load.
func (wm *Manager) DiscardResolverEdits(commandID, workerID string) error {
	if err := validateIDs(commandID, workerID); err != nil {
		return err
	}
	cl := wm.commandLock(commandID)
	cl.Lock()
	defer cl.Unlock()

	wm.mu.Lock()
	defer wm.mu.Unlock()

	if _, err := wm.loadState(commandID); err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	intPath := wm.integrationWorktreePath(commandID)
	if _, err := os.Stat(intPath); err != nil {
		return fmt.Errorf("integration worktree path: %w", err)
	}
	// M4: defense-in-depth — refuse to operate outside .maestro/worktrees on
	// the real (symlink-resolved) filesystem so a poisoned state file cannot
	// redirect destructive git ops at the working tree.
	if err := wm.assertWorktreeContained(intPath); err != nil {
		return err
	}
	// Tripwire: additionally refuse to run destructive git ops outside the project root.
	if err := ensureWithinProjectRoot(wm.projectRoot, intPath); err != nil {
		return fmt.Errorf("discard resolver edits: %w", err)
	}
	if err := wm.resolverGitRunInDir(intPath, "reset", "HEAD"); err != nil {
		return fmt.Errorf("reset staged: %w", err)
	}
	if err := wm.resolverGitRunInDir(intPath, "checkout", "--", "."); err != nil {
		return fmt.Errorf("checkout discard: %w", err)
	}
	if err := wm.resolverGitRunInDir(intPath, "clean", "-fd"); err != nil {
		return fmt.Errorf("clean: %w", err)
	}
	wm.Log(core.LogLevelInfo, "resolver_edits_discarded command=%s worker=%s", commandID, workerID)
	return nil
}

// --- helpers ---

// assertWorktreeContained verifies that path resolves (via realpath) to a
// location strictly under <maestroDir>/worktrees. Used as a tripwire by
// destructive git operations in the resolver pipeline (M4).
func (wm *Manager) assertWorktreeContained(path string) error {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("realpath %q: %w", path, err)
	}
	worktreesRoot := filepath.Join(wm.maestroDir, "worktrees")
	realRoot, err := filepath.EvalSymlinks(worktreesRoot)
	if err != nil {
		// If the root itself doesn't resolve, fall back to lexical containment.
		realRoot = worktreesRoot
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == "" || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("refusing destructive op outside worktrees root: %s (real=%s, root=%s)",
			path, realPath, realRoot)
	}
	return nil
}

// resolverGitRunInDir is a context-bounded git wrapper for resolver-pipeline
// operations. It applies a hard resolverGitTimeout regardless of the
// configured wm.gitTimeout() so a stuck integration worktree cannot stall the
// scan loop indefinitely.
func (wm *Manager) resolverGitRunInDir(dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), resolverGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are constructed internally from validated inputs
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("git %s: timeout after %s: %w",
				strings.Join(args, " "), resolverGitTimeout, ctx.Err())
		}
		return fmt.Errorf("git %s: %w\noutput: %s",
			strings.Join(args, " "), err, sanitizeGitStderr(string(output)))
	}
	return nil
}
