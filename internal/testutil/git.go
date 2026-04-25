package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// InitTestGitRepo creates a temporary git repo with an initial commit.
// The repo includes a .gitignore that ignores the .maestro/ directory,
// which mirrors the real project setup and prevents worktree directories
// from appearing as untracked in git status --porcelain.
func InitTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // hard-coded test commands
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init failed: %v\n%s", err, out)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".maestro/\n"), 0644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}

	cmds = [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "initial commit"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // hard-coded test commands
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git initial commit failed: %v\n%s", err, out)
		}
	}

	return dir
}
