package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// CleanupCommand removes all worktrees and branches for a command.
//
// Locking: acquires cmdLocks[commandID] → wm.mu to match the established
// hierarchy (see manager.go). The per-command lock serializes against any
// in-flight resolver operations for this command.
func (wm *Manager) CleanupCommand(commandID string) error {
	if err := validateIDs(commandID); err != nil {
		return err
	}
	// Acquire per-command resolver lock before wm.mu to match the
	// documented hierarchy: cmdLocks[cmd] → integrationLocks[cmd] → wm.mu.
	cl := wm.commandLock(commandID)
	cl.Lock()
	defer cl.Unlock()

	// Reserve the integration worktree so cleanup never destroys it under an
	// in-flight A/B selection (which releases wm.mu during external verify
	// runs).
	il := wm.integrationLock(commandID)
	il.Lock()
	defer il.Unlock()

	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to clean up
		}
		return fmt.Errorf("load state: %w", err)
	}

	errs := wm.cleanupCommandCore(commandID, state, true)

	// If there were failures, save state with cleanup_failed markers so
	// GC/Reconcile can retry later.
	if len(errs) > 0 {
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		state.UpdatedAt = now
		if sErr := wm.saveState(commandID, state); sErr != nil {
			wm.Log(core.LogLevelWarn, "save_cleanup_failed_state command=%s error=%v", commandID, sErr)
		}
		return fmt.Errorf("cleanup errors: %s", strings.Join(errs, "; "))
	}

	// M5: Remove per-command locks to prevent memory leak.
	// Safe after cleanup: all worktrees/branches/state are gone; any future
	// operation on the same commandID will allocate a fresh mutex via LoadOrStore.
	wm.cmdLocks.Delete(commandID)
	wm.integrationLocks.Delete(commandID)

	wm.Log(core.LogLevelInfo, "cleanup_complete command=%s", commandID)
	return nil
}

// CleanupAll removes all worktrees and their branches for all commands.
// Intended for daemon shutdown to prevent worktree accumulation across restarts.
// Respects the provided context for timeout cancellation. Individual cleanup
// failures are logged as warnings but do not prevent cleanup of remaining commands.
//
// Locking: Stage 1 holds only wm.mu to snapshot state entries. Stage 2 acquires
// cmdLocks[cmd] → wm.mu per command, matching the established hierarchy
// (see manager.go) to serialize against in-flight resolver operations.
func (wm *Manager) CleanupAll(ctx context.Context) error {
	// Stage 1: snapshot all command states under lock
	wm.mu.Lock()
	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		wm.mu.Unlock()
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read worktree state dir: %w", err)
	}

	type target struct {
		commandID string
		state     *model.WorktreeCommandState
	}
	targets := make([]target, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		state, loadErr := wm.loadStateUnlocked(commandID)
		if loadErr != nil {
			wm.Log(core.LogLevelWarn, "shutdown_cleanup_load_state command=%s error=%v", commandID, loadErr)
			continue
		}
		targets = append(targets, target{commandID: commandID, state: state})
	}
	wm.mu.Unlock()

	if len(targets) == 0 {
		return nil
	}

	wm.Log(core.LogLevelInfo, "shutdown_cleanup_start count=%d", len(targets))

	// Stage 2: cleanup each command, checking context between operations.
	// Acquire cmdLocks[cmd] → wm.mu to match the documented lock hierarchy
	// and serialize against concurrent resolver operations.
	var cleaned, failed int
	for _, t := range targets {
		select {
		case <-ctx.Done():
			wm.Log(core.LogLevelWarn, "shutdown_cleanup_timeout cleaned=%d failed=%d remaining=%d",
				cleaned, failed, len(targets)-cleaned-failed)
			return ctx.Err()
		default:
		}

		cl := wm.commandLock(t.commandID)
		cl.Lock()

		// Reserve the integration worktree (see CleanupCommand). Blocking is
		// acceptable here: shutdown cancels the scan context first, which
		// unwinds any in-flight selection promptly.
		il := wm.integrationLock(t.commandID)
		il.Lock()

		wm.mu.Lock()
		// Reload the state under lock: the Stage 1 snapshot may be stale
		// (workers/candidates added since), and a stale-state cleanup would
		// miss their worktrees and leak their branches. NotExist means a
		// concurrent path already cleaned the command up.
		state := t.state
		switch fresh, loadErr := wm.loadStateUnlocked(t.commandID); {
		case loadErr == nil:
			state = fresh
		case os.IsNotExist(loadErr):
			wm.mu.Unlock()
			il.Unlock()
			cl.Unlock()
			cleaned++
			wm.Log(core.LogLevelInfo, "shutdown_cleanup_already_done command=%s", t.commandID)
			continue
		default:
			wm.Log(core.LogLevelWarn, "shutdown_cleanup_reload_failed command=%s error=%v (using stage-1 snapshot)",
				t.commandID, loadErr)
		}
		errs := wm.cleanupCommandCore(t.commandID, state, false)
		wm.mu.Unlock()

		if len(errs) > 0 {
			wm.Log(core.LogLevelWarn, "shutdown_cleanup_failed command=%s errors=%s",
				t.commandID, strings.Join(errs, "; "))
			failed++
		} else {
			wm.Log(core.LogLevelInfo, "shutdown_cleanup_done command=%s", t.commandID)
			cleaned++
			// Safe to delete: we hold the per-command locks and cleanup
			// succeeded (all worktrees/branches/state are gone).
			wm.cmdLocks.Delete(t.commandID)
			wm.integrationLocks.Delete(t.commandID)
		}

		il.Unlock()
		cl.Unlock()
	}

	wm.Log(core.LogLevelInfo, "shutdown_cleanup_complete cleaned=%d failed=%d", cleaned, failed)
	return nil
}

// GC removes old worktrees that exceed TTL or max_worktrees limit.
//
// Design: GC uses a three-stage snapshot→release→reacquire pattern to minimize
// the time wm.mu is held, allowing concurrent worktree operations to proceed
// between stages.
//
//   - Stage 1 (snapshot): Lock → read state entries + build snapshot → Unlock
//   - Stage 2 (cleanup): For each cleanup target, lock → cleanup → unlock (per-command granularity)
//   - Stage 3 (health check): Lock → orphan detection + bak sweep → Unlock
func (wm *Manager) GC() error {
	if !wm.config.GC.Enabled {
		return nil
	}

	// Stage 1: snapshot state entries under lock
	type stateEntry struct {
		commandID string
		createdAt time.Time
		state     *model.WorktreeCommandState
	}
	wm.mu.Lock()
	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		wm.mu.Unlock()
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read worktree state dir: %w", err)
	}
	allStates := make([]stateEntry, 0, len(entries))

	ttl := time.Duration(wm.config.GC.EffectiveTTLHours()) * time.Hour
	maxWorktrees := wm.config.GC.EffectiveMaxWorktrees()
	now := wm.clock.Now()

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		state, loadErr := wm.loadStateUnlocked(commandID)
		if loadErr != nil {
			continue
		}
		created, parseErr := time.Parse(time.RFC3339, state.CreatedAt)
		if parseErr != nil {
			wm.Log(core.LogLevelWarn, "gc_created_at_parse_failed command=%s value=%q error=%v, falling back to mtime", commandID, state.CreatedAt, parseErr)
			statePath := filepath.Join(stateDir, entry.Name())
			info, statErr := os.Stat(statePath)
			if statErr != nil {
				wm.Log(core.LogLevelWarn, "gc_mtime_fallback_failed command=%s error=%v, skipping", commandID, statErr)
				continue
			}
			created = info.ModTime()
		}
		allStates = append(allStates, stateEntry{commandID: commandID, createdAt: created, state: state})
	}
	wm.mu.Unlock()

	// Stage 2: cleanup without holding wm.mu for the entire duration
	remaining := make([]stateEntry, 0, len(allStates))
	for _, se := range allStates {
		if now.Sub(se.createdAt) > ttl {
			isTerminal := model.IsIntegrationTerminal(se.state.Integration.Status)
			isFailed := se.state.Integration.Status == model.IntegrationStatusFailed
			if !isTerminal && !isFailed {
				wm.Log(core.LogLevelInfo, "gc_ttl_skip_active command=%s status=%s age=%s",
					se.commandID, se.state.Integration.Status, now.Sub(se.createdAt))
				remaining = append(remaining, se)
				continue
			}
			// Failed integration is non-terminal but retryable. It is still
			// GC-able after the TTL to avoid leaking worktrees of abandoned
			// commands, but not while the conflict-resolution pipeline still
			// owns a worker (Conflict/Resolving): cleanupCommandUnlocked runs
			// `git worktree remove --force` without respawning the worker pane
			// first (unlike the Phase B teardown path), so removing a worktree
			// mid-resolution would corrupt an in-progress resume-merge. Defer
			// to a later GC pass once the resolution settles.
			if isFailed && hasInFlightWorker(se.state) {
				wm.Log(core.LogLevelInfo,
					"gc_ttl_skip_failed_inflight command=%s age=%s (failed integration but a worker is mid conflict-resolution)",
					se.commandID, now.Sub(se.createdAt))
				remaining = append(remaining, se)
				continue
			}
			if isFailed {
				wm.Log(core.LogLevelInfo, "gc_cleanup_failed_worktree command=%s age=%s", se.commandID, now.Sub(se.createdAt))
			} else {
				wm.Log(core.LogLevelInfo, "gc_ttl_expired command=%s age=%s", se.commandID, now.Sub(se.createdAt))
			}
			wm.gcCleanupCommand(se.commandID, gcTTLCleanupEligible)
			continue
		}
		remaining = append(remaining, se)
	}

	// Max worktrees limit
	if len(remaining) > maxWorktrees {
		sort.Slice(remaining, func(i, j int) bool {
			return remaining[i].createdAt.Before(remaining[j].createdAt)
		})
		for i := 0; i < len(remaining)-maxWorktrees; i++ {
			if !model.IsIntegrationTerminal(remaining[i].state.Integration.Status) {
				wm.Log(core.LogLevelWarn, "gc_max_skip_active command=%s status=%s",
					remaining[i].commandID, remaining[i].state.Integration.Status)
				continue
			}
			wm.Log(core.LogLevelInfo, "gc_max_exceeded command=%s", remaining[i].commandID)
			wm.gcCleanupCommand(remaining[i].commandID, func(st *model.WorktreeCommandState) bool {
				return model.IsIntegrationTerminal(st.Integration.Status)
			})
		}
	}

	// Stage 3: health check under lock
	wm.mu.Lock()
	defer wm.mu.Unlock()

	wm.detectAndRemoveOrphanWorktrees()
	wm.gcBakFiles()
	wm.gcOrphanedCmdLocks()

	return nil
}

// gcTTLCleanupEligible is the TTL-path re-validation applied to the FRESH
// state in gcCleanupCommand: terminal integrations are always reclaimable;
// failed ones only while no worker is mid conflict-resolution (same rules as
// the Stage 1 snapshot checks).
func gcTTLCleanupEligible(st *model.WorktreeCommandState) bool {
	if model.IsIntegrationTerminal(st.Integration.Status) {
		return true
	}
	return st.Integration.Status == model.IntegrationStatusFailed && !hasInFlightWorker(st)
}

// gcCleanupCommand re-acquires locks, RELOADS the command state, and
// re-validates the cleanup preconditions before destroying anything. The
// Stage 1 snapshot is taken without holding wm.mu across Stage 2, so by the
// time a target is processed another code path may have added workers or
// candidates — a stale-snapshot cleanup would miss their worktrees, remove
// the state file, and let Stage 3 orphan detection force-remove them while
// their branches leak — or advanced the integration status out of
// eligibility.
func (wm *Manager) gcCleanupCommand(commandID string, eligible func(*model.WorktreeCommandState) bool) {
	if wm.testGCStage2Hook != nil {
		wm.testGCStage2Hook(commandID)
	}

	// TryLock (not Lock): an in-flight A/B selection, merge or publish may
	// hold the integration lock for many minutes; GC must not stall behind
	// it. A skipped target is retried on the next GC cycle.
	il := wm.integrationLock(commandID)
	if !il.TryLock() {
		wm.Log(core.LogLevelInfo,
			"gc_skip_integration_busy command=%s (integration worktree reserved; retrying next cycle)", commandID)
		return
	}
	defer il.Unlock()

	wm.mu.Lock()
	defer wm.mu.Unlock()

	fresh, err := wm.loadStateUnlocked(commandID)
	if err != nil {
		if !os.IsNotExist(err) {
			wm.Log(core.LogLevelWarn, "gc_reload_state_failed command=%s error=%v", commandID, err)
		}
		return // already cleaned up, or unreadable — retry next cycle
	}
	if !eligible(fresh) {
		wm.Log(core.LogLevelInfo, "gc_skip_state_changed command=%s status=%s (state advanced between GC stages)",
			commandID, fresh.Integration.Status)
		return
	}
	if err := wm.cleanupCommandUnlocked(commandID, fresh); err != nil {
		wm.Log(core.LogLevelWarn, "gc_cleanup_failed command=%s error=%v", commandID, err)
		return
	}
	// Cleanup succeeded — drop the per-command integration lock entry (we
	// hold it) to mirror the cmdLocks lifecycle.
	wm.integrationLocks.Delete(commandID)
}

// gcOrphanedCmdLocks removes cmdLocks / integrationLocks entries for commands
// whose state files no longer exist. This handles the memory leak when
// cleanupCommandUnlocked's TryLock fails (resolver is active): after the
// resolver completes, the entry remains because the state file has already
// been removed. Caller must hold wm.mu.
func (wm *Manager) gcOrphanedCmdLocks() {
	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	sweep := func(locks *sync.Map, label string) {
		locks.Range(func(key, value any) bool {
			commandID, ok := key.(string)
			if !ok {
				return true
			}
			statePath := filepath.Join(stateDir, commandID+".yaml")
			if _, err := os.Stat(statePath); os.IsNotExist(err) {
				mu, ok := value.(*sync.Mutex)
				if ok && mu.TryLock() {
					locks.Delete(commandID)
					mu.Unlock()
					wm.Log(core.LogLevelDebug, "gc_orphaned_%s command=%s", label, commandID)
				}
				// TryLock failure: holder still active; cleaned up next GC cycle.
			}
			return true
		})
	}
	sweep(&wm.cmdLocks, "cmd_lock")
	sweep(&wm.integrationLocks, "integration_lock")
}

// detectAndRemoveOrphanWorktrees cross-references git worktree list against
// state files and removes orphaned worktrees. Caller must hold wm.mu.
func (wm *Manager) detectAndRemoveOrphanWorktrees() {
	gitWorktrees, listErr := wm.listGitWorktreesUnlocked()
	if listErr != nil {
		wm.Log(core.LogLevelWarn, "orphan_detection_worktree_list error=%v", listErr)
		return
	}

	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if !os.IsNotExist(err) {
			wm.Log(core.LogLevelWarn, "orphan_detection_read_state_dir error=%v", err)
		}
		return
	}

	knownPaths := make(map[string]bool)
	stateLoadFailures := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		cmdID := strings.TrimSuffix(entry.Name(), ".yaml")
		st, loadErr := wm.loadStateUnlocked(cmdID)
		if loadErr != nil {
			stateLoadFailures++
			wm.Log(core.LogLevelWarn, "orphan_detection_state_load_failed command=%s error=%v", cmdID, loadErr)
			continue
		}
		for _, ws := range st.Workers {
			knownPaths[ws.Path] = true
		}
		for _, c := range st.Candidates {
			knownPaths[c.Path] = true
		}
		knownPaths[wm.integrationWorktreePath(cmdID)] = true
	}
	// Fail-safe: if any state file failed to load (corrupted YAML, transient
	// I/O error), its worktrees are missing from knownPaths and would be
	// misclassified as orphans. `worktree remove --force` destroys
	// uncommitted worker output, so skip the entire removal pass rather
	// than risk deleting live work; the next GC cycle retries.
	if stateLoadFailures > 0 {
		wm.Log(core.LogLevelError,
			"orphan_detection_skipped state_load_failures=%d (refusing to force-remove worktrees with incomplete state knowledge)",
			stateLoadFailures)
		return
	}

	pathPrefix := wm.config.EffectivePathPrefix()
	for _, wtPath := range gitWorktrees {
		relPath, relErr := filepath.Rel(wm.projectRoot, wtPath)
		if relErr != nil || !strings.HasPrefix(relPath, pathPrefix) {
			continue
		}
		if !knownPaths[wtPath] {
			wm.Log(core.LogLevelInfo, "orphan_worktree detected path=%s rel=%s", wtPath, relPath)
			if rmErr := wm.gitRun("worktree", "remove", "--force", wtPath); rmErr != nil {
				wm.Log(core.LogLevelWarn, "orphan_remove_retry path=%s first_error=%v", wtPath, rmErr)
				// Single retry: transient lock/filesystem errors may resolve on second attempt
				if retryErr := wm.gitRun("worktree", "remove", "--force", wtPath); retryErr != nil {
					wm.Log(core.LogLevelError, "orphan_remove_failed path=%s error=%v", wtPath, retryErr)
				}
			}
		}
	}
}

// bakTTL is the maximum age a .bak file may live before GC removes it.
// Backups are intended only as crash-recovery aids for the most recent
// AtomicWrite, so a 24h ceiling is sufficient.
const bakTTL = 24 * time.Hour

// bakScanSubdirs lists the .maestro subdirectories that contain control-plane
// YAML files written via yaml.AtomicWrite. The worktree source directory and
// static template directories (instructions, hooks, persona) are intentionally
// excluded so that user files cannot be touched.
var bakScanSubdirs = []string{"state", "queues", "results", "locks", "logs"}

// gcBakFiles walks the control-plane subdirectories under maestroDir and
// removes .bak files that are either orphaned (no matching .yaml) or older
// than bakTTL. Errors are logged and never returned: the sweep is best-effort.
func (wm *Manager) gcBakFiles() {
	now := wm.clock.Now()
	for _, sub := range bakScanSubdirs {
		root := filepath.Join(wm.maestroDir, sub)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				wm.Log(core.LogLevelWarn, "gc_bak_walk_error path=%s error=%v", path, err)
				return nil
			}
			if info.IsDir() || !strings.HasSuffix(path, ".bak") {
				return nil
			}
			yamlPath := strings.TrimSuffix(path, ".bak")
			if _, statErr := os.Stat(yamlPath); os.IsNotExist(statErr) {
				if rmErr := os.Remove(path); rmErr != nil { //nolint:gosec // path is derived from trusted WalkDir traversal
					wm.Log(core.LogLevelWarn, "gc_bak_remove_orphan_failed path=%s error=%v", path, rmErr)
				} else {
					wm.Log(core.LogLevelInfo, "gc_bak_orphan_removed path=%s", path)
				}
				return nil
			}
			if now.Sub(info.ModTime()) > bakTTL {
				if rmErr := os.Remove(path); rmErr != nil { //nolint:gosec // path is derived from trusted WalkDir traversal
					wm.Log(core.LogLevelWarn, "gc_bak_remove_expired_failed path=%s error=%v", path, rmErr)
				} else {
					wm.Log(core.LogLevelInfo, "gc_bak_expired_removed path=%s age=%s", path, now.Sub(info.ModTime()))
				}
			}
			return nil
		})
		if walkErr != nil {
			wm.Log(core.LogLevelWarn, "gc_bak_walk_failed root=%s error=%v", root, walkErr)
		}
	}
}

// cleanupCommandCore performs the core cleanup operations shared by
// CleanupCommand and cleanupCommandUnlocked. It removes worker worktrees/branches,
// the integration worktree/branch, the publish temp branch, and (if no errors)
// the worktree directory and state file.
//
// If trackWorkerStatus is true, workers are transitioned to cleanup_done/cleanup_failed
// (used by CleanupCommand for retry-safety state tracking).
// Returns collected error strings (nil if all succeeded).
// Caller must hold wm.mu.
func (wm *Manager) cleanupCommandCore(commandID string, state *model.WorktreeCommandState, trackWorkerStatus bool) []string {
	now := wm.clock.Now().UTC().Format(time.RFC3339)
	var errs []string

	// Remove A/B candidate worktrees + branches. Candidates are task-scoped
	// and never merged directly, so removal is unconditional; "already gone"
	// outcomes are tolerated inside removeCandidateArtifactsUnlocked.
	for i := range state.Candidates {
		c := &state.Candidates[i]
		if err := wm.removeCandidateArtifactsUnlocked(commandID, c.TaskID); err != nil {
			errs = append(errs, fmt.Sprintf("remove candidate %s: %v", c.TaskID, err))
		}
	}

	// Remove worker worktrees (skip already-cleaned entries for retry-safety)
	for i := range state.Workers {
		ws := &state.Workers[i]
		if ws.Status == model.WorktreeStatusCleanupDone {
			continue
		}
		if err := ensureWithinProjectRoot(wm.projectRoot, ws.Path); err != nil {
			errs = append(errs, fmt.Sprintf("path guard worktree %s: %v", ws.WorkerID, err))
			continue
		}
		if err := wm.gitRun("worktree", "remove", "--force", ws.Path); err != nil {
			// Treat "not a working tree" as success (already removed in prior attempt)
			if !strings.Contains(err.Error(), "not a working tree") {
				errs = append(errs, fmt.Sprintf("remove worktree %s: %v", ws.WorkerID, err))
				if trackWorkerStatus {
					if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusCleanupFailed, now); tErr != nil {
						wm.Log(core.LogLevelWarn, "cleanup_failed_transition command=%s worker=%s error=%v",
							commandID, ws.WorkerID, tErr)
					}
				}
				continue
			}
		}
		if trackWorkerStatus {
			if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusCleanupDone, now); tErr != nil {
				wm.Log(core.LogLevelWarn, "cleanup_done_transition command=%s worker=%s error=%v",
					commandID, ws.WorkerID, tErr)
			}
		}

		// Delete worker branch. A real deletion failure must block state
		// removal (errs non-empty keeps the state file so GC retries):
		// deleting the state while the branch survives leaves unpublished
		// commits dangling with nothing tracking them. "not found" means a
		// prior attempt already deleted it — idempotent success.
		if err := wm.gitRun("branch", "-D", ws.Branch); err != nil {
			if strings.Contains(err.Error(), "not found") {
				wm.Log(core.LogLevelDebug, "delete_worker_branch_already_gone command=%s worker=%s branch=%s",
					commandID, ws.WorkerID, ws.Branch)
			} else {
				wm.Log(core.LogLevelWarn, "delete_worker_branch_failed command=%s worker=%s branch=%s error=%v",
					commandID, ws.WorkerID, ws.Branch, err)
				errs = append(errs, fmt.Sprintf("delete worker branch %s: %v", ws.Branch, err))
			}
		}
	}

	// Remove integration worktree (must be before os.RemoveAll and branch deletion)
	integrationPath := wm.integrationWorktreePath(commandID)
	if err := ensureWithinProjectRoot(wm.projectRoot, integrationPath); err != nil {
		errs = append(errs, fmt.Sprintf("path guard integration worktree: %v", err))
	} else if err := wm.gitRun("worktree", "remove", "--force", integrationPath); err != nil {
		// Treat "not a working tree" as success (already removed in prior attempt)
		if !strings.Contains(err.Error(), "not a working tree") {
			errs = append(errs, fmt.Sprintf("remove integration worktree: %v", err))
		}
	}

	// Delete integration branch (same retry semantics as worker branches).
	if err := wm.gitRun("branch", "-D", state.Integration.Branch); err != nil {
		if strings.Contains(err.Error(), "not found") {
			wm.Log(core.LogLevelDebug, "delete_integration_branch_already_gone command=%s branch=%s",
				commandID, state.Integration.Branch)
		} else {
			wm.Log(core.LogLevelWarn, "delete_integration_branch_failed command=%s branch=%s error=%v",
				commandID, state.Integration.Branch, err)
			errs = append(errs, fmt.Sprintf("delete integration branch %s: %v", state.Integration.Branch, err))
		}
	}

	// Delete _publish temp branch if it leaked (e.g. crash after update-ref in PublishToBase)
	publishBranch := fmt.Sprintf("maestro/%s/_publish", commandID)
	if err := wm.gitRun("branch", "-D", publishBranch); err != nil {
		wm.Log(core.LogLevelDebug, "delete_publish_branch_skipped command=%s branch=%s error=%v",
			commandID, publishBranch, err)
	}

	// A leaked publish sync-pending marker (crash between update-ref and
	// project-root sync, followed by command cleanup without a publish
	// retry) is the only record that lets the stale project root self-heal:
	// the repair is keyed to this command's marker and no other command's
	// publish can consume it. Deleting it before attempting the repair
	// would leave every later publish failing the dirty-root guard with no
	// autonomous path out (operator intervention required). Try the repair
	// first — on success it deletes the marker itself; a refused repair
	// (genuine operator edits) drops the unactionable marker as before; a
	// FAILED repair (signature matched but the git op errored) keeps the
	// marker so the recovery record survives for a later attempt.
	switch wm.tryCompleteInterruptedPublishSync(commandID) {
	case publishSyncRepairRefused:
		wm.deletePublishSyncPendingRef(commandID)
	case publishSyncRepairFailed:
		// Block state removal (errs non-empty keeps the state file) so GC
		// retries the whole cleanup — and with it this resync — after the
		// transient git/path failure clears. Completing cleanup here would
		// orphan the marker: nothing keyed to this command runs again.
		wm.Log(core.LogLevelWarn,
			"publish_sync_marker_kept command=%s (cleanup-time resync failed; keeping marker and state so GC retries)",
			commandID)
		errs = append(errs, "publish sync-pending repair failed; cleanup deferred for retry")
	case publishSyncRepairNoMarker, publishSyncRepairCompleted:
		// Nothing to do: no marker, or the repair consumed it.
	}

	// Delete pre-publish-stash durable ref created by syncProjectRootAfterPublish
	prePublishStashRef := fmt.Sprintf("refs/maestro/pre-publish-stash/%s", commandID)
	if err := wm.gitRun("update-ref", "-d", prePublishStashRef); err != nil {
		wm.Log(core.LogLevelDebug, "delete_pre_publish_stash_ref_skipped command=%s ref=%s error=%v",
			commandID, prePublishStashRef, err)
	}

	// Only remove directory and state file if all worktree removals succeeded.
	// On failure, keep state file so GC/Reconcile can retry.
	if len(errs) > 0 {
		return errs
	}

	// All worktree removals succeeded — safe to remove directory and state file
	wtDir := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID)
	if err := os.RemoveAll(wtDir); err != nil {
		wm.Log(core.LogLevelWarn, "failed to remove worktree directory %s: %v", wtDir, err)
	}

	// GC the command's unstattable-fallback entries from the repo-shared
	// .git/info/exclude so they do not outlive the command.
	wm.removeMaestroExcludeBlocks(commandID)

	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	if err := os.Remove(statePath); err != nil {
		wm.Log(core.LogLevelWarn, "failed to remove state file %s: %v", statePath, err)
	}

	return nil
}

// CleanupTempPublishBranch deletes the maestro/{commandID}/_publish temporary
// branch if it exists. This is a best-effort operation intended for quarantined
// integrations where CleanupCommand is not called (worktrees are preserved for
// operator inspection) but the temporary publish branch should not leak.
//
// Historical context: a prior implementation unconditionally ran
// `git checkout --detach` in the integration worktree whenever the initial
// `branch -D` failed, including the overwhelmingly common case where the
// branch did not exist at all. Once detached, subsequent publish retries
// observed an orphaned HEAD: forward-merge read `rev-parse HEAD` and
// incorrectly concluded no forward-merge was needed, while performPublishMerge
// merged the stale integration-branch pointer and always conflicted — an
// infinite publish_conflict loop. This cleanup therefore NEVER detaches the
// integration worktree. If the branch cannot be deleted because it is checked
// out there, we restore the integration-branch checkout explicitly; any other
// failure mode is logged and left for the operator.
//
// Errors are logged but never returned. Serialized with wm.mu so it cannot
// race in-flight publish operations that mutate the same worktree.
func (wm *Manager) CleanupTempPublishBranch(commandID string) {
	if err := validateIDs(commandID); err != nil {
		return
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()

	wm.cleanupTempPublishBranchUnlocked(commandID)
}

// cleanupTempPublishBranchUnlocked is the core of CleanupTempPublishBranch.
// Also called by createTempPublishBranch to clear a stale temp branch left
// behind by a crashed publish before recreating it. Caller MUST hold wm.mu.
func (wm *Manager) cleanupTempPublishBranchUnlocked(commandID string) {
	publishBranch := fmt.Sprintf("maestro/%s/_publish", commandID)

	// Precondition: only touch anything if the branch actually exists.
	// `branch -D` on a missing branch is the trigger that previously caused
	// the `checkout --detach` recovery-loop bug.
	if err := wm.gitRun("show-ref", "--verify", "--quiet",
		fmt.Sprintf("refs/heads/%s", publishBranch)); err != nil {
		return
	}

	// Try a direct delete first. This is the fast path when _publish is not
	// checked out anywhere.
	if err := wm.gitRun("branch", "-D", publishBranch); err == nil {
		wm.Log(core.LogLevelInfo,
			"cleanup_temp_publish_branch_deleted command=%s branch=%s",
			commandID, publishBranch)
		return
	}

	// Delete failed — most likely because _publish is checked out in the
	// integration worktree (createTempPublishBranch + performPublishMerge
	// are the only code paths that check it out, and only ever in the
	// integration worktree). Try to restore the integration-branch checkout
	// so deletion can succeed without orphaning HEAD.
	integrationPath := wm.integrationWorktreePath(commandID)
	if _, statErr := os.Stat(integrationPath); statErr != nil {
		wm.Log(core.LogLevelWarn,
			"cleanup_temp_publish_branch_skipped command=%s branch=%s reason=integration_worktree_missing error=%v",
			commandID, publishBranch, statErr)
		return
	}

	currentRef, _ := wm.gitOutputInDir(integrationPath, "symbolic-ref", "--short", "HEAD")
	if strings.TrimSpace(currentRef) != publishBranch {
		// _publish is not checked out in the integration worktree. Whatever
		// prevents deletion cannot be safely resolved here without risking
		// corruption of another worktree; leave it for the operator/GC.
		wm.Log(core.LogLevelWarn,
			"cleanup_temp_publish_branch_skipped command=%s branch=%s reason=not_checked_out_in_integration current=%q",
			commandID, publishBranch, strings.TrimSpace(currentRef))
		return
	}

	state, stateErr := wm.loadState(commandID)
	if stateErr != nil || state == nil || state.Integration.Branch == "" {
		wm.Log(core.LogLevelWarn,
			"cleanup_temp_publish_branch_no_state command=%s branch=%s error=%v",
			commandID, publishBranch, stateErr)
		return
	}

	// Refuse to switch branches if the worktree has uncommitted work — a
	// clean checkout is a precondition for the operator to reason about
	// state during quarantine.
	dirtyOut, dirtyErr := wm.gitOutputInDir(integrationPath, "status", "--porcelain")
	if dirtyErr != nil {
		wm.Log(core.LogLevelWarn,
			"cleanup_temp_publish_branch_status_failed command=%s branch=%s error=%v",
			commandID, publishBranch, dirtyErr)
		return
	}
	if strings.TrimSpace(dirtyOut) != "" {
		wm.Log(core.LogLevelWarn,
			"cleanup_temp_publish_branch_dirty command=%s branch=%s dirty=%q",
			commandID, publishBranch, strings.TrimSpace(dirtyOut))
		return
	}

	if err := wm.gitRunInDir(integrationPath, "checkout", state.Integration.Branch); err != nil {
		wm.Log(core.LogLevelWarn,
			"cleanup_temp_publish_branch_restore_failed command=%s branch=%s error=%v",
			commandID, publishBranch, err)
		return
	}
	if err := wm.gitRun("branch", "-D", publishBranch); err != nil {
		wm.Log(core.LogLevelWarn,
			"cleanup_temp_publish_branch_retry_failed command=%s branch=%s error=%v",
			commandID, publishBranch, err)
		return
	}
	wm.Log(core.LogLevelInfo,
		"cleanup_temp_publish_branch_restored_and_deleted command=%s branch=%s",
		commandID, publishBranch)
}

// hasInFlightWorker reports whether any worker in the command is owned by the
// conflict-resolution pipeline (Conflict/Resolving). For those statuses the
// daemon is actively driving an in-place resolution against the worktree, so
// TTL GC must not force-remove it mid-resolution. Created/Active workers are
// deliberately NOT treated as in-flight: a worker stuck in those states past
// the GC TTL (default 24h) has long since been lease-recovered, so a Failed
// command whose workers are all settled is safe to reclaim.
func hasInFlightWorker(state *model.WorktreeCommandState) bool {
	for i := range state.Workers {
		switch state.Workers[i].Status {
		case model.WorktreeStatusResolving,
			model.WorktreeStatusConflict:
			return true
		}
	}
	return false
}

func (wm *Manager) cleanupCommandUnlocked(commandID string, state *model.WorktreeCommandState) error {
	errs := wm.cleanupCommandCore(commandID, state, false)
	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %s", strings.Join(errs, "; "))
	}

	// M5: Remove per-command resolver lock to prevent memory leak.
	// Use TryLock to avoid lock ordering violation: wm.mu is held by
	// the caller (GC), so acquiring cmdLock directly would violate the
	// cmdLocks → wm.mu hierarchy. If TryLock fails (resolver is active),
	// skip deletion; it will be cleaned up in the next GC cycle.
	if v, ok := wm.cmdLocks.Load(commandID); ok {
		mu, ok := v.(*sync.Mutex)
		if ok && mu.TryLock() {
			wm.cmdLocks.Delete(commandID)
			mu.Unlock()
		}
	}

	return nil
}

// Reconcile checks for inconsistencies between state files and actual worktrees
// at daemon startup. This is a best-effort operation: errors are logged and
// reconciliation continues.
func (wm *Manager) Reconcile() {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	wm.Log(core.LogLevelInfo, "reconcile_start")

	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			wm.Log(core.LogLevelDebug, "reconcile_skip no_state_dir")
			return
		}
		wm.Log(core.LogLevelWarn, "reconcile_read_state_dir error=%v", err)
		return
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		state, loadErr := wm.loadStateUnlocked(commandID)
		if loadErr != nil {
			wm.Log(core.LogLevelWarn, "reconcile_load_state command=%s error=%v", commandID, loadErr)
			continue
		}

		// Restore a stale in-flight A/B selection (daemon crashed while the
		// selection borrowed the integration worktree). Saves its own state;
		// runs before the worker sweep so the sweep sees the restored tree.
		wm.reconcileABSelectionMarkerUnlocked(commandID, state)

		stateChanged := false
		now := wm.clock.Now().UTC().Format(time.RFC3339)

		for i := range state.Workers {
			ws := &state.Workers[i]

			// State exists but worktree directory is gone → mark cleanup_done
			if _, statErr := os.Stat(ws.Path); os.IsNotExist(statErr) {
				if ws.Status != model.WorktreeStatusCleanupDone && ws.Status != model.WorktreeStatusCleanupFailed {
					wm.Log(core.LogLevelWarn, "reconcile_stale_state command=%s worker=%s path=%s",
						commandID, ws.WorkerID, ws.Path)
					if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusCleanupDone, now); tErr != nil {
						wm.Log(core.LogLevelWarn, "reconcile_transition command=%s worker=%s error=%v",
							commandID, ws.WorkerID, tErr)
					}
					stateChanged = true
				}
			}
		}

		if stateChanged {
			state.UpdatedAt = now
			if saveErr := wm.saveState(commandID, state); saveErr != nil {
				wm.Log(core.LogLevelWarn, "reconcile_save_state command=%s error=%v", commandID, saveErr)
			}
		}
	}

	// Worktree exists in git but no state → remove it
	wm.detectAndRemoveOrphanWorktrees()

	// Prune stale git worktree entries
	if pruneErr := wm.gitRun("worktree", "prune"); pruneErr != nil {
		wm.Log(core.LogLevelWarn, "reconcile_prune error=%v", pruneErr)
	}

	// Sweep .bak orphan/expired files left behind by yaml.AtomicWrite.
	wm.gcBakFiles()

	wm.Log(core.LogLevelInfo, "reconcile_complete")
}
