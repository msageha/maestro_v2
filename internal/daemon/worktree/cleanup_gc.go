package worktree

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// CleanupCommand removes all worktrees and branches for a command.
func (wm *Manager) CleanupCommand(commandID string) error {
	if err := validateIDs(commandID); err != nil {
		return err
	}
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

	// Remove worker worktrees (skip already-cleaned entries for retry-safety)
	for i := range state.Workers {
		ws := &state.Workers[i]
		if ws.Status == model.WorktreeStatusCleanupDone {
			continue
		}
		if err := wm.gitRun("worktree", "remove", "--force", ws.Path); err != nil {
			// Treat "not a working tree" as success (already removed in prior attempt)
			if !strings.Contains(err.Error(), "not a working tree") {
				errs = append(errs, fmt.Sprintf("remove worktree %s: %v", ws.WorkerID, err))
				if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusCleanupFailed, now); tErr != nil {
					wm.Log(core.LogLevelWarn, "cleanup_failed_transition command=%s worker=%s error=%v",
						commandID, ws.WorkerID, tErr)
				}
				continue
			}
		}
		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusCleanupDone, now); tErr != nil {
			wm.Log(core.LogLevelWarn, "cleanup_done_transition command=%s worker=%s error=%v",
				commandID, ws.WorkerID, tErr)
		}

		// Delete worker branch
		if err := wm.gitRun("branch", "-D", ws.Branch); err != nil {
			wm.Log(core.LogLevelWarn, "delete_worker_branch_failed command=%s worker=%s branch=%s error=%v",
				commandID, ws.WorkerID, ws.Branch, err)
		}
	}

	// Remove integration worktree (must be before os.RemoveAll and branch deletion)
	integrationPath := wm.integrationWorktreePath(commandID)
	if err := wm.gitRun("worktree", "remove", "--force", integrationPath); err != nil {
		errs = append(errs, fmt.Sprintf("remove integration worktree: %v", err))
	}

	// Delete integration branch
	if err := wm.gitRun("branch", "-D", state.Integration.Branch); err != nil {
		wm.Log(core.LogLevelWarn, "delete_integration_branch_failed command=%s branch=%s error=%v",
			commandID, state.Integration.Branch, err)
	}

	// Only remove directory and state file if all worktree removals succeeded.
	// If there were failures, save state with cleanup_failed markers so
	// GC/Reconcile can retry later.
	if len(errs) > 0 {
		state.UpdatedAt = now
		if sErr := wm.saveState(commandID, state); sErr != nil {
			wm.Log(core.LogLevelWarn, "save_cleanup_failed_state command=%s error=%v", commandID, sErr)
		}
		return fmt.Errorf("cleanup errors: %s", strings.Join(errs, "; "))
	}

	// All worktree removals succeeded — safe to remove directory and state file
	wtDir := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID)
	if err := os.RemoveAll(wtDir); err != nil {
		log.Printf("WARN: failed to remove worktree directory %s: %v", wtDir, err)
	}

	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	if err := os.Remove(statePath); err != nil {
		log.Printf("WARN: failed to remove state file %s: %v", statePath, err)
	}

	// M5: Remove per-command resolver lock to prevent memory leak.
	// Safe after cleanup: all worktrees/branches/state are gone; any future
	// operation on the same commandID will allocate a fresh mutex via LoadOrStore.
	wm.cmdLocks.Delete(commandID)

	wm.Log(core.LogLevelInfo, "cleanup_complete command=%s", commandID)
	return nil
}

// GC removes old worktrees that exceed TTL or max_worktrees limit.
//
// Design: GC holds wm.mu for the entire operation to prevent concurrent
// worktree creation/deletion from racing with GC's read-modify-delete cycle.
// This is intentional — GC runs infrequently and the critical section is
// bounded by the number of worktree state files, so lock contention is minimal.
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

	allStates := make([]stateEntry, 0, len(entries))

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
			wm.Log(core.LogLevelWarn, "gc_created_at_parse_failed command=%s value=%q error=%v, falling back to mtime", commandID, state.CreatedAt, err)
			statePath := filepath.Join(stateDir, entry.Name())
			info, statErr := os.Stat(statePath)
			if statErr != nil {
				wm.Log(core.LogLevelWarn, "gc_mtime_fallback_failed command=%s error=%v, skipping", commandID, statErr)
				continue
			}
			created = info.ModTime()
		}

		// TTL-based cleanup
		if now.Sub(created) > ttl {
			wm.Log(core.LogLevelInfo, "gc_ttl_expired command=%s age=%s", commandID, now.Sub(created))
			if err := wm.cleanupCommandUnlocked(commandID, state); err != nil {
				wm.Log(core.LogLevelWarn, "gc_cleanup_failed command=%s error=%v", commandID, err)
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
			wm.Log(core.LogLevelInfo, "gc_max_exceeded command=%s", allStates[i].commandID)
			if err := wm.cleanupCommandUnlocked(allStates[i].commandID, allStates[i].state); err != nil {
				wm.Log(core.LogLevelWarn, "gc_cleanup_failed command=%s error=%v", allStates[i].commandID, err)
			}
		}
	}

	// M4: Health check — cross-reference git worktree list with state files
	gitWorktrees, listErr := wm.listGitWorktreesUnlocked()
	if listErr != nil {
		wm.Log(core.LogLevelWarn, "gc_worktree_list error=%v", listErr)
		return nil
	}

	// Reuse already-loaded states from the first loop (TTL pass) and load any
	// new entries that appeared since then (e.g. concurrent creation).
	// This eliminates the second ReadDir + loadState round-trip.
	cachedStates := make(map[string]*model.WorktreeCommandState, len(allStates))
	for _, se := range allStates {
		cachedStates[se.commandID] = se.state
	}
	// Pick up entries not covered by allStates (they were TTL-cleaned or skipped)
	remainingEntries, _ := os.ReadDir(stateDir)
	for _, entry := range remainingEntries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		cmdID := strings.TrimSuffix(entry.Name(), ".yaml")
		if _, exists := cachedStates[cmdID]; exists {
			continue
		}
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
			wm.Log(core.LogLevelInfo, "gc_orphan_worktree path=%s", wtPath)
			if rmErr := wm.gitRun("worktree", "remove", "--force", wtPath); rmErr != nil {
				wm.Log(core.LogLevelWarn, "gc_remove_orphan error=%v path=%s", rmErr, wtPath)
			}
		}
	}

	// Log state entries whose worktree directories don't exist (using cached states)
	for cmdID, st := range cachedStates {
		for _, ws := range st.Workers {
			if _, statErr := os.Stat(ws.Path); os.IsNotExist(statErr) {
				if ws.Status != model.WorktreeStatusCleanupDone && ws.Status != model.WorktreeStatusCleanupFailed {
					wm.Log(core.LogLevelWarn, "gc_state_without_worktree command=%s worker=%s path=%s",
						cmdID, ws.WorkerID, ws.Path)
				}
			}
		}
	}

	// Sweep .bak orphan/expired files left behind by yaml.AtomicWrite.
	wm.gcBakFiles()

	return nil
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

func (wm *Manager) cleanupCommandUnlocked(commandID string, state *model.WorktreeCommandState) error {
	var errs []string

	for _, ws := range state.Workers {
		if ws.Status == model.WorktreeStatusCleanupDone {
			continue
		}
		if err := wm.gitRun("worktree", "remove", "--force", ws.Path); err != nil {
			// Treat "not a working tree" as success (already removed)
			if !strings.Contains(err.Error(), "not a working tree") {
				errs = append(errs, fmt.Sprintf("remove worktree %s: %v", ws.WorkerID, err))
				continue
			}
		}
		if err := wm.gitRun("branch", "-D", ws.Branch); err != nil {
			wm.Log(core.LogLevelWarn, "delete_worker_branch_failed command=%s worker=%s branch=%s error=%v",
				commandID, ws.WorkerID, ws.Branch, err)
		}
	}

	// Remove integration worktree before branch deletion
	integrationPath := wm.integrationWorktreePath(commandID)
	if err := wm.gitRun("worktree", "remove", "--force", integrationPath); err != nil {
		// Treat "not a working tree" as success (already removed)
		if !strings.Contains(err.Error(), "not a working tree") {
			errs = append(errs, fmt.Sprintf("remove integration worktree: %v", err))
		}
	}

	if err := wm.gitRun("branch", "-D", state.Integration.Branch); err != nil {
		wm.Log(core.LogLevelWarn, "delete_integration_branch_failed command=%s branch=%s error=%v",
			commandID, state.Integration.Branch, err)
	}

	// Only remove directory and state file if all worktree removals succeeded.
	// On failure, keep state file so GC/Reconcile can retry.
	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %s", strings.Join(errs, "; "))
	}

	// All removals succeeded — safe to remove directory and state file
	wtDir := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID)
	if err := os.RemoveAll(wtDir); err != nil {
		log.Printf("WARN: failed to remove worktree directory %s: %v", wtDir, err)
	}

	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	if err := os.Remove(statePath); err != nil {
		log.Printf("WARN: failed to remove state file %s: %v", statePath, err)
	}

	// M5: Remove per-command resolver lock to prevent memory leak.
	wm.cmdLocks.Delete(commandID)

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

	// Collect all known worktree paths from state files
	knownPaths := make(map[string]bool)

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

		stateChanged := false
		now := wm.clock.Now().UTC().Format(time.RFC3339)

		for i := range state.Workers {
			ws := &state.Workers[i]
			knownPaths[ws.Path] = true

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

		// Track integration worktree path
		integrationPath := wm.integrationWorktreePath(commandID)
		knownPaths[integrationPath] = true

		if stateChanged {
			state.UpdatedAt = now
			if saveErr := wm.saveState(commandID, state); saveErr != nil {
				wm.Log(core.LogLevelWarn, "reconcile_save_state command=%s error=%v", commandID, saveErr)
			}
		}
	}

	// Worktree exists in git but no state → remove it
	gitWorktrees, listErr := wm.listGitWorktreesUnlocked()
	if listErr != nil {
		wm.Log(core.LogLevelWarn, "reconcile_list_worktrees error=%v", listErr)
	} else {
		pathPrefix := wm.config.EffectivePathPrefix()
		for _, wtPath := range gitWorktrees {
			relPath, relErr := filepath.Rel(wm.projectRoot, wtPath)
			if relErr != nil || !strings.HasPrefix(relPath, pathPrefix) {
				continue
			}
			if !knownPaths[wtPath] {
				wm.Log(core.LogLevelWarn, "reconcile_orphan_worktree path=%s", wtPath)
				if rmErr := wm.gitRun("worktree", "remove", "--force", wtPath); rmErr != nil {
					wm.Log(core.LogLevelWarn, "reconcile_remove_orphan error=%v path=%s", rmErr, wtPath)
				}
			}
		}
	}

	// Prune stale git worktree entries
	if pruneErr := wm.gitRun("worktree", "prune"); pruneErr != nil {
		wm.Log(core.LogLevelWarn, "reconcile_prune error=%v", pruneErr)
	}

	// Sweep .bak orphan/expired files left behind by yaml.AtomicWrite.
	wm.gcBakFiles()

	wm.Log(core.LogLevelInfo, "reconcile_complete")
}
