package worktree

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

func newTestWorktreeManager(t *testing.T, projectRoot string) *Manager {
	t.Helper()
	maestroDir := filepath.Join(projectRoot, ".maestro")
	if err := os.MkdirAll(maestroDir, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := model.WorktreeConfig{
		Enabled:          true,
		BaseBranch:       "main",
		PathPrefix:       ".maestro/worktrees",
		AutoCommit:       true,
		AutoMerge:        true,
		MergeStrategy:    "ort",
		CleanupOnSuccess: true,
		CleanupOnFailure: false,
		GC: model.WorktreeGCConfig{
			Enabled:      true,
			TTLHours:     ptr.Int(24),
			MaxWorktrees: ptr.Int(32),
		},
	}

	logger := log.New(os.Stderr, "", 0)
	return NewManager(maestroDir, cfg, logger, core.LogLevelError)
}

// createForCommand is a test helper that replicates the removed CreateForCommand method.
func createForCommand(wm *Manager, commandID string, workerIDs []string) error {
	if err := validateIDs(commandID, workerIDs...); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	now := wm.clock.Now().UTC().Format(time.RFC3339)

	baseBranch := wm.config.EffectiveBaseBranch()
	baseSHA, err := wm.gitOutput("rev-parse", baseBranch)
	if err != nil {
		return fmt.Errorf("get base SHA from %s: %w", baseBranch, err)
	}
	baseSHA = strings.TrimSpace(baseSHA)
	if err := validateSHA(baseSHA); err != nil {
		return fmt.Errorf("base SHA from %s: %w", baseBranch, err)
	}

	integrationBranch := fmt.Sprintf("maestro/%s/integration", commandID)
	if err := wm.gitRun("branch", integrationBranch, baseSHA); err != nil {
		return fmt.Errorf("create integration branch %s: %w", integrationBranch, err)
	}

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

	type createdWT struct{ path, branch string }
	var createdWorktrees []createdWT
	rollbackCreated := func() {
		for _, wt := range createdWorktrees {
			_ = wm.gitRun("worktree", "remove", "--force", wt.path)
			_ = wm.gitRun("branch", "-D", wt.branch)
		}
		_ = wm.gitRun("worktree", "remove", "--force", integrationPath)
		_ = wm.gitRun("branch", "-D", integrationBranch)
		wtDir := filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID)
		_ = os.RemoveAll(wtDir)
		statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
		_ = os.Remove(statePath)
	}

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

	if err := wm.saveState(commandID, &state); err != nil {
		rollbackCreated()
		return fmt.Errorf("save worktree state: %w", err)
	}
	return nil
}

// getState is a test helper that replicates the removed GetState method.
func getState(wm *Manager, commandID, workerID string) (*model.WorktreeState, error) {
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

// cleanupAll is a test helper that replicates the removed CleanupAll method.
func cleanupAll(wm *Manager) error {
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

	_ = wm.gitRun("worktree", "prune")

	if len(errs) > 0 {
		return fmt.Errorf("cleanup_all errors: %s", strings.Join(errs, "; "))
	}
	return nil
}
