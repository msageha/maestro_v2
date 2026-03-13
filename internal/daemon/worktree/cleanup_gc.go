package worktree

import (
	"fmt"
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
					wm.log(core.LogLevelWarn, "cleanup_failed_transition command=%s worker=%s error=%v",
						commandID, ws.WorkerID, tErr)
				}
				continue
			}
		}
		if tErr := wm.setWorkerStatus(ws, model.WorktreeStatusCleanupDone, now); tErr != nil {
			wm.log(core.LogLevelWarn, "cleanup_done_transition command=%s worker=%s error=%v",
				commandID, ws.WorkerID, tErr)
		}

		// Delete worker branch
		if err := wm.gitRun("branch", "-D", ws.Branch); err != nil {
			wm.log(core.LogLevelWarn, "delete_worker_branch_failed command=%s worker=%s branch=%s error=%v",
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
		wm.log(core.LogLevelWarn, "delete_integration_branch_failed command=%s branch=%s error=%v",
			commandID, state.Integration.Branch, err)
	}

	// Only remove directory and state file if all worktree removals succeeded.
	// If there were failures, save state with cleanup_failed markers so
	// GC/Reconcile can retry later.
	if len(errs) > 0 {
		state.UpdatedAt = now
		if sErr := wm.saveState(commandID, state); sErr != nil {
			wm.log(core.LogLevelWarn, "save_cleanup_failed_state command=%s error=%v", commandID, sErr)
		}
		return fmt.Errorf("cleanup errors: %s", strings.Join(errs, "; "))
	}

	// All worktree removals succeeded — safe to remove directory and state file
	wtDir := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID)
	_ = os.RemoveAll(wtDir)

	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	_ = os.Remove(statePath)

	wm.log(core.LogLevelInfo, "cleanup_complete command=%s", commandID)
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
			wm.log(core.LogLevelWarn, "gc_created_at_parse_failed command=%s value=%q error=%v, falling back to mtime", commandID, state.CreatedAt, err)
			statePath := filepath.Join(stateDir, entry.Name())
			info, statErr := os.Stat(statePath)
			if statErr != nil {
				wm.log(core.LogLevelWarn, "gc_mtime_fallback_failed command=%s error=%v, skipping", commandID, statErr)
				continue
			}
			created = info.ModTime()
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
			wm.log(core.LogLevelWarn, "delete_worker_branch_failed command=%s worker=%s branch=%s error=%v",
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
		wm.log(core.LogLevelWarn, "delete_integration_branch_failed command=%s branch=%s error=%v",
			commandID, state.Integration.Branch, err)
	}

	// Only remove directory and state file if all worktree removals succeeded.
	// On failure, keep state file so GC/Reconcile can retry.
	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %s", strings.Join(errs, "; "))
	}

	// All removals succeeded — safe to remove directory and state file
	wtDir := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID)
	_ = os.RemoveAll(wtDir)

	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	_ = os.Remove(statePath)

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
