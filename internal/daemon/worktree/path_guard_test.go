package worktree

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEnsureWithinProjectRoot_Inside(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureWithinProjectRoot(root, sub); err != nil {
		t.Fatalf("expected inside, got error: %v", err)
	}
	if err := ensureWithinProjectRoot(root, root); err != nil {
		t.Fatalf("root itself should be allowed, got: %v", err)
	}
}

func TestEnsureWithinProjectRoot_AbsoluteOutside(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	if err := ensureWithinProjectRoot(root, other); err == nil {
		t.Fatalf("expected error for outside path, got nil")
	}
}

func TestEnsureWithinProjectRoot_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideTarget := filepath.Join(outside, "secret")
	if err := os.MkdirAll(outsideTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outsideTarget, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureWithinProjectRoot(root, link); err == nil {
		t.Fatalf("expected error for symlink escape, got nil")
	}
}

func TestEnsureWithinProjectRoot_EmptyArgs(t *testing.T) {
	if err := ensureWithinProjectRoot("", "/tmp"); err == nil {
		t.Fatalf("expected error for empty root")
	}
	root := t.TempDir()
	if err := ensureWithinProjectRoot(root, ""); err == nil {
		t.Fatalf("expected error for empty target")
	}
}
