package worktree

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// maxResolveAttempts is the upper bound on resolver commit attempts. After
// this many failed attempts the worker is reverted to conflict and the
// integration's MergeFailureCount is incremented so the existing quarantine
// path can pick it up.
const maxResolveAttempts = 2

// ErrConflictGenerationMismatch is returned by the resolver entry points when
// the supplied conflict generation no longer matches the signal stored on
// disk — typically because the integration HEAD or worker SHA shifted under
// the resolver. The caller is expected to abort and re-fetch fresh state.
var ErrConflictGenerationMismatch = errors.New("conflict generation mismatch")

// ErrResolverPreconditionFailed is returned when one of the static
// pre-validation checks (unmerged paths, file subset, conflict markers) fails
// before any git mutation is attempted. Wrapped errors carry the detail.
var ErrResolverPreconditionFailed = errors.New("resolver precondition failed")

// ErrSignalStoreUnavailable indicates that the Manager was constructed
// without a SignalStore, so resolver methods cannot persist their CAS
// updates. The caller should ensure the daemon wires a store via
// SetSignalStore before invoking resolver entry points.
var ErrSignalStoreUnavailable = errors.New("signal store not configured on Manager")

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
// points (they will return ErrSignalStoreUnavailable).
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
// ErrConflictGenerationMismatch and no state is mutated.
//
// Locking note: scanMu (owned by the daemon scanner outside this package) MUST
// be held by the caller for the duration of this call to keep scan from
// racing the worker state transition. This method itself acquires the
// per-command resolver lock and then wm.mu.
func (wm *Manager) DispatchConflictResolution(commandID, phaseID, workerID, conflictGen string) error {
	if err := validateIDs(commandID, workerID); err != nil {
		return err
	}
	if wm.signalStore == nil {
		return ErrSignalStoreUnavailable
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
			return ErrConflictGenerationMismatch
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
		return fmt.Errorf("worker %s not found in command %s", workerID, commandID)
	}
	now := wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.setWorkerStatus(ws, model.WorktreeStatusResolving, now); err != nil {
		return err
	}
	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		// Best-effort signal revert so we don't leave ResolutionState=dispatched
		// while the worker is still in conflict (split-brain).
		if revErr := wm.signalStore.UpdateMergeConflictSignal(commandID, phaseID, workerID, func(sig *model.PlannerSignal) error {
			if sig == nil {
				return nil
			}
			sig.ResolutionState = ""
			sig.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
			return nil
		}); revErr != nil {
			wm.log(core.LogLevelError, "signal_revert_after_save_failed command=%s worker=%s err=%v", commandID, workerID, revErr)
		}
		return fmt.Errorf("save state: %w", err)
	}
	wm.log(core.LogLevelInfo, "conflict_resolution_dispatched command=%s phase=%s worker=%s", commandID, phaseID, workerID)
	return nil
}

// CommitResolvedConflict performs the resolver commit in the integration
// worktree after a resolver agent has staged a fix. See package doc for the
// full pre-validation contract.
//
// Locking note: like DispatchConflictResolution, the caller must hold scanMu
// for the duration of the call. This method acquires the per-command
// resolver lock and then wm.mu for state IO.
func (wm *Manager) CommitResolvedConflict(commandID, phaseID, workerID, conflictGen string, files []string) error {
	if err := validateIDs(commandID, workerID); err != nil {
		return err
	}
	if wm.signalStore == nil {
		return ErrSignalStoreUnavailable
	}
	if len(files) == 0 {
		return fmt.Errorf("%w: files list is empty", ErrResolverPreconditionFailed)
	}
	for _, f := range files {
		if f == "" || strings.HasPrefix(f, "/") || strings.Contains(f, "..") {
			return fmt.Errorf("%w: invalid file path %q (must be repo-relative, no .. or absolute)", ErrResolverPreconditionFailed, f)
		}
	}

	cl := wm.commandLock(commandID)
	cl.Lock()
	defer cl.Unlock()

	// 1. Bump attempt counter and CAS-check generation atomically with the
	// signal store so we never run a commit against stale state.
	var attempt int
	if err := wm.signalStore.UpdateMergeConflictSignal(commandID, phaseID, workerID, func(sig *model.PlannerSignal) error {
		if sig == nil {
			return fmt.Errorf("merge_conflict signal not found for command=%s phase=%s worker=%s", commandID, phaseID, workerID)
		}
		if sig.ConflictGeneration != conflictGen {
			return ErrConflictGenerationMismatch
		}
		sig.ResolveAttempt++
		attempt = sig.ResolveAttempt
		sig.ResolutionState = "resolving"
		sig.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
		return nil
	}); err != nil {
		return err
	}

	commitErr := wm.runResolverCommit(commandID, phaseID, workerID, files)
	if commitErr == nil {
		// Mark signal resolved + transition worker to integrated.
		if err := wm.signalStore.UpdateMergeConflictSignal(commandID, phaseID, workerID, func(sig *model.PlannerSignal) error {
			if sig == nil {
				return nil
			}
			sig.ResolutionState = "" // cleared on success
			sig.LastResolutionError = ""
			sig.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
			return nil
		}); err != nil {
			wm.log(core.LogLevelError, "signal_clear_after_resolve_failed command=%s worker=%s err=%v", commandID, workerID, err)
		}

		wm.mu.Lock()
		defer wm.mu.Unlock()
		state, err := wm.loadState(commandID)
		if err != nil {
			return fmt.Errorf("load state after resolver commit: %w", err)
		}
		ws := wm.findWorker(state, workerID)
		if ws == nil {
			return fmt.Errorf("worker %s not found post-resolve in command %s", workerID, commandID)
		}
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if err := wm.setWorkerStatus(ws, model.WorktreeStatusIntegrated, now); err != nil {
			return err
		}
		state.UpdatedAt = now
		if err := wm.saveState(commandID, state); err != nil {
			return fmt.Errorf("save state after resolver commit: %w", err)
		}
		wm.log(core.LogLevelInfo, "conflict_resolution_committed command=%s phase=%s worker=%s attempt=%d",
			commandID, phaseID, workerID, attempt)
		return nil
	}

	// Failure path: record the error and decide whether to retry or quarantine.
	errMsg := commitErr.Error()
	if err := wm.signalStore.UpdateMergeConflictSignal(commandID, phaseID, workerID, func(sig *model.PlannerSignal) error {
		if sig == nil {
			return nil
		}
		sig.ResolutionState = "failed"
		sig.LastResolutionError = errMsg
		sig.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
		return nil
	}); err != nil {
		wm.log(core.LogLevelError, "signal_mark_failed_failed command=%s worker=%s err=%v", commandID, workerID, err)
	}

	if attempt >= maxResolveAttempts {
		// Revert worker to conflict and bump MergeFailureCount so quarantine
		// logic picks it up on the next scan.
		wm.mu.Lock()
		defer wm.mu.Unlock()
		state, lerr := wm.loadState(commandID)
		if lerr != nil {
			return fmt.Errorf("load state after attempt-limit: %w (original: %v)", lerr, commitErr)
		}
		ws := wm.findWorker(state, workerID)
		if ws == nil {
			return fmt.Errorf("worker %s not found post-failure in command %s (original: %v)", workerID, commandID, commitErr)
		}
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		// resolving → conflict is a valid transition; ignore the error if
		// the worker has been concurrently moved (best-effort revert).
		if ws.Status == model.WorktreeStatusResolving {
			if err := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); err != nil {
				wm.log(core.LogLevelWarn, "resolver_revert_failed command=%s worker=%s error=%v", commandID, workerID, err)
			}
		}
		state.Integration.MergeFailureCount++
		state.UpdatedAt = now
		if err := wm.saveState(commandID, state); err != nil {
			return fmt.Errorf("save state after attempt-limit: %w (original: %v)", err, commitErr)
		}
		wm.log(core.LogLevelError, "conflict_resolution_attempts_exhausted command=%s phase=%s worker=%s error=%v",
			commandID, phaseID, workerID, commitErr)
		return commitErr
	}

	wm.log(core.LogLevelWarn, "conflict_resolution_failed command=%s phase=%s worker=%s attempt=%d error=%v",
		commandID, phaseID, workerID, attempt, commitErr)
	return commitErr
}

// runResolverCommit performs the actual git operations for a resolver commit
// in the integration worktree. Caller holds the per-command lock; this
// function does NOT take wm.mu (the underlying gitRunInDir uses exec.Cmd
// which is process-safe). Validation precedes mutation so a precondition
// failure leaves the index untouched.
func (wm *Manager) runResolverCommit(commandID, phaseID, workerID string, files []string) error {
	intPath := wm.integrationWorktreePath(commandID)
	if _, err := os.Stat(intPath); err != nil {
		return fmt.Errorf("integration worktree path: %w", err)
	}

	// (2) git ls-files -u must be empty (no unmerged paths remaining).
	unmerged, err := wm.gitOutputInDir(intPath, "ls-files", "-u")
	if err != nil {
		return fmt.Errorf("ls-files -u: %w", err)
	}
	if strings.TrimSpace(unmerged) != "" {
		return fmt.Errorf("%w: unmerged paths still present:\n%s", ErrResolverPreconditionFailed, unmerged)
	}

	// (3) staged∪unstaged file set must be a subset of `files`.
	dirtyOut, err := wm.gitOutputInDir(intPath, "status", "--porcelain", "-z")
	if err != nil {
		return fmt.Errorf("git status --porcelain: %w", err)
	}
	dirtySet := parsePorcelainZ(dirtyOut)
	allowed := make(map[string]bool, len(files))
	for _, f := range files {
		allowed[f] = true
	}
	for f := range dirtySet {
		if !allowed[f] {
			return fmt.Errorf("%w: dirty file %q is not in the allowed file set", ErrResolverPreconditionFailed, f)
		}
	}
	if len(dirtySet) == 0 {
		return fmt.Errorf("%w: nothing staged or modified — refusing to commit", ErrResolverPreconditionFailed)
	}

	// (4) Each file in `files` must contain no conflict marker. We only
	// scan files that physically exist (a deleted file in the resolution is
	// fine). Markers are checked at column 0 with the canonical 7-char form.
	for _, f := range files {
		full := joinClean(intPath, f)
		data, rerr := os.ReadFile(full)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue
			}
			return fmt.Errorf("%w: read %q: %v", ErrResolverPreconditionFailed, f, rerr)
		}
		if containsConflictMarker(data) {
			return fmt.Errorf("%w: conflict marker remaining in %q", ErrResolverPreconditionFailed, f)
		}
	}

	// Execution: git add -- <files> ; git commit -m ...
	addArgs := append([]string{"add", "--"}, files...)
	if err := wm.gitRunInDir(intPath, addArgs...); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	msg := fmt.Sprintf("[maestro] resolve: phase=%s worker=%s", phaseID, workerID)
	if err := wm.gitRunInDir(intPath, "commit", "-m", msg); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
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
	if err := wm.gitRunInDir(intPath, "reset", "HEAD"); err != nil {
		return fmt.Errorf("reset staged: %w", err)
	}
	if err := wm.gitRunInDir(intPath, "checkout", "--", "."); err != nil {
		return fmt.Errorf("checkout discard: %w", err)
	}
	if err := wm.gitRunInDir(intPath, "clean", "-fd"); err != nil {
		return fmt.Errorf("clean: %w", err)
	}
	wm.log(core.LogLevelInfo, "resolver_edits_discarded command=%s worker=%s", commandID, workerID)
	return nil
}

// --- helpers ---

// parsePorcelainZ extracts the file paths from `git status --porcelain -z`.
// Each entry is "XY <path>\0" (rename has an extra "\0<oldpath>"); we collect
// the new path on either side.
func parsePorcelainZ(out string) map[string]bool {
	set := map[string]bool{}
	parts := strings.Split(out, "\x00")
	i := 0
	for i < len(parts) {
		entry := parts[i]
		if len(entry) < 4 {
			i++
			continue
		}
		xy := entry[:2]
		path := entry[3:]
		set[path] = true
		// Renamed (R) and copied (C) entries are followed by the original path.
		if xy[0] == 'R' || xy[1] == 'R' || xy[0] == 'C' || xy[1] == 'C' {
			if i+1 < len(parts) {
				set[parts[i+1]] = true
				i += 2
				continue
			}
		}
		i++
	}
	return set
}

func containsConflictMarker(data []byte) bool {
	// Match canonical 7-character markers anywhere in the file. Conservative:
	// any occurrence is treated as unresolved.
	return bytesContains(data, []byte("<<<<<<<")) ||
		bytesContains(data, []byte("=======")) ||
		bytesContains(data, []byte(">>>>>>>"))
}

func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
outer:
	for i := 0; i <= len(haystack)-len(needle); i++ {
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return true
	}
	return false
}

// joinClean joins dir + rel safely, refusing escape via "..".
func joinClean(dir, rel string) string {
	clean := strings.TrimLeft(rel, "/")
	return dir + string(os.PathSeparator) + clean
}
