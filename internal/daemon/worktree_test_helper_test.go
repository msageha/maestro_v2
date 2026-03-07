package daemon

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// initTestGitRepo creates a temporary git repo with an initial commit.
// Duplicated from worktree test package for daemon-level tests that
// create worktree managers (cancel_handler_test, queue_scan_phase_test).
func initTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init failed: %v\n%s", err, out)
		}
	}

	// Create initial file and commit
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmds = [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "initial commit"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git initial commit failed: %v\n%s", err, out)
		}
	}

	return dir
}

// newTestWorktreeManager creates a WorktreeManager for testing.
// Duplicated from worktree test package for daemon-level tests.
func newTestWorktreeManager(t *testing.T, projectRoot string) *WorktreeManager {
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
			TTLHours:     24,
			MaxWorktrees: 32,
		},
		CommitPolicy: model.CommitPolicyConfig{},
	}

	logger := log.New(os.Stderr, "", 0)
	return NewWorktreeManager(maestroDir, cfg, logger, LogLevelError)
}
