package lock

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func writeLockFile(t *testing.T, path string, mtimeAge time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if mtimeAge > 0 {
		old := time.Now().Add(-mtimeAge)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
}

func TestGCStaleLockFiles_RemovesOldUnheldFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	old := filepath.Join(dir, "queue:planner_signals.flock")
	young := filepath.Join(dir, "queue:planner.flock")
	writeLockFile(t, old, 8*24*time.Hour)
	writeLockFile(t, young, 1*time.Hour)

	stats, err := GCStaleLockFiles(dir, DefaultLockGCAge, nil, nil)
	if err != nil {
		t.Fatalf("GCStaleLockFiles: %v", err)
	}
	if stats.Removed != 1 {
		t.Errorf("Removed = %d, want 1", stats.Removed)
	}
	if stats.SkippedYoung != 1 {
		t.Errorf("SkippedYoung = %d, want 1", stats.SkippedYoung)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, stat err=%v", old, err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("expected %s to survive, stat err=%v", young, err)
	}
}

func TestGCStaleLockFiles_ProtectsListedNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "daemon.lock")
	writeLockFile(t, target, 30*24*time.Hour)

	stats, err := GCStaleLockFiles(dir, DefaultLockGCAge, []string{"daemon.lock"}, nil)
	if err != nil {
		t.Fatalf("GCStaleLockFiles: %v", err)
	}
	if stats.Removed != 0 {
		t.Errorf("protected file was removed (Removed=%d)", stats.Removed)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("daemon.lock should survive: %v", err)
	}
}

func TestGCStaleLockFiles_SkipsHeldLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	heldPath := filepath.Join(dir, "queue:held.flock")
	writeLockFile(t, heldPath, 30*24*time.Hour)

	// Hold the flock for the duration of GC.
	f, err := os.OpenFile(heldPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open held: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock held: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	})

	stats, err := GCStaleLockFiles(dir, DefaultLockGCAge, nil, nil)
	if err != nil {
		t.Fatalf("GCStaleLockFiles: %v", err)
	}
	if stats.Removed != 0 {
		t.Errorf("held lock should not be removed; Removed=%d", stats.Removed)
	}
	if stats.SkippedHeld != 1 {
		t.Errorf("SkippedHeld = %d, want 1", stats.SkippedHeld)
	}
	if _, err := os.Stat(heldPath); err != nil {
		t.Errorf("held lock should survive: %v", err)
	}
}

func TestGCStaleLockFiles_SkipsSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "real.flock")
	link := filepath.Join(dir, "symlink.flock")
	writeLockFile(t, target, 30*24*time.Hour)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	// Make symlink mtime old too (lchtimes would be ideal but Go's std lib
	// does not expose it on all platforms; the symlink is filtered by Lstat
	// regardless of mtime).
	if _, err := GCStaleLockFiles(dir, DefaultLockGCAge, nil, nil); err != nil {
		t.Fatalf("GCStaleLockFiles: %v", err)
	}
	// real.flock is unprotected and old → removed; symlink itself is still
	// present (or absent only because the GC didn't classify it as a regular
	// file). Either way, the symlink must NOT be followed and removed.
	if _, err := os.Lstat(link); os.IsNotExist(err) {
		t.Errorf("symlink %s was removed (must be skipped)", link)
	}
}

func TestGCStaleLockFiles_MissingDirNoError(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "nonexistent")
	stats, err := GCStaleLockFiles(dir, DefaultLockGCAge, nil, nil)
	if err != nil {
		t.Errorf("missing dir should not error: %v", err)
	}
	if stats.Removed != 0 {
		t.Errorf("Removed = %d on missing dir", stats.Removed)
	}
}
