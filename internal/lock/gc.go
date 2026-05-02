package lock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// DefaultLockGCAge is the staleness threshold used by GCStaleLockFiles when
// no explicit age is provided: lock files whose mtime is older than this
// duration are eligible for removal.
const DefaultLockGCAge = 7 * 24 * time.Hour

// GCStats summarises the outcome of a single GCStaleLockFiles run.
type GCStats struct {
	// Scanned is the total number of regular files inspected (excluding
	// symlinks and protected entries).
	Scanned int
	// Removed is the number of files actually deleted.
	Removed int
	// SkippedYoung is the count skipped because mtime was newer than maxAge.
	SkippedYoung int
	// SkippedHeld is the count skipped because another process held the
	// flock at GC time.
	SkippedHeld int
	// SkippedRaced is the count skipped because the inode at the path
	// changed between open() and the post-flock confirmation stat().
	SkippedRaced int
	// SkippedOther captures unclassified failures (read errors, non-regular
	// files, etc.). These are logged by the caller via the logf hook.
	SkippedOther int
}

// GCStaleLockFiles removes flock files in dir whose mtime is older than
// maxAge, EXCEPT for any path listed in protect. Files are removed only
// when:
//
//  1. The path is a regular file (not a symlink, directory, or device).
//  2. mtime is older than maxAge.
//  3. A non-blocking exclusive flock can be acquired (= no other process
//     holds it right now).
//  4. The dev/inode of the open file descriptor still matches the dev/inode
//     of the path (= no other process has unlinked + recreated the file
//     between open() and now).
//
// MAINTENANCE INVARIANT: callers MUST run this exactly once at startup,
// before any other lock acquisition begins. Concurrent GC introduces an
// inode-split race: after `LOCK_EX|LOCK_NB` succeeds and the file is
// unlinked, a process that had `open()`ed the path before the unlink can
// still flock the old inode while a new caller creates a fresh inode at
// the same path — splitting the mutex.
//
// All failures are returned as zero entries in GCStats and surfaced to
// the caller via the logf hook (logf may be nil to silence).
//
// `protect` should include the daemon's own primary lock (e.g. daemon.lock)
// because that file is held by the running daemon while GC runs.
func GCStaleLockFiles(dir string, maxAge time.Duration, protect []string, logf func(format string, args ...any)) (GCStats, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var stats GCStats

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return stats, nil
		}
		return stats, fmt.Errorf("read locks dir: %w", err)
	}

	protectSet := make(map[string]struct{}, len(protect))
	for _, name := range protect {
		protectSet[filepath.Base(name)] = struct{}{}
	}
	cutoff := time.Now().Add(-maxAge)

	for _, entry := range entries {
		name := entry.Name()
		if _, isProtected := protectSet[name]; isProtected {
			continue
		}
		path := filepath.Join(dir, name)
		// Use Lstat so we never follow symlinks; symlinks pointing into the
		// locks dir are out of scope and could escalate access.
		info, err := os.Lstat(path)
		if err != nil {
			stats.SkippedOther++
			logf("lock_gc lstat_failed path=%s error=%v", path, err)
			continue
		}
		if info.Mode()&os.ModeType != 0 {
			// Not a regular file (symlink, dir, device, socket, …).
			continue
		}
		stats.Scanned++
		if info.ModTime().After(cutoff) {
			stats.SkippedYoung++
			continue
		}

		removed, classification := tryRemoveStaleLock(path)
		switch classification {
		case lockGCRemoved:
			stats.Removed++
			logf("lock_gc removed path=%s mtime=%s", path, info.ModTime().Format(time.RFC3339))
		case lockGCHeld:
			stats.SkippedHeld++
		case lockGCRaced:
			stats.SkippedRaced++
			logf("lock_gc inode_changed path=%s (raced with concurrent recreate)", path)
		case lockGCOther:
			stats.SkippedOther++
			logf("lock_gc skip_path=%s reason=open_or_stat_failed", path)
		}
		_ = removed // value retained for future telemetry hooks
	}
	return stats, nil
}

type lockGCClassification int

const (
	lockGCRemoved lockGCClassification = iota
	lockGCHeld
	lockGCRaced
	lockGCOther
)

// tryRemoveStaleLock implements the four-step contract from GCStaleLockFiles
// for a single path. The bool return reports whether unlink actually ran.
func tryRemoveStaleLock(path string) (bool, lockGCClassification) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0) //nolint:gosec // path constructed from controlled directory
	if err != nil {
		return false, lockGCOther
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // uintptr→int safe on supported platforms
		return false, lockGCHeld
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }() //nolint:gosec

	// Confirm the dev/inode at the path still matches what we opened. If
	// another GC run (or a manual operator) already unlinked + recreated
	// the file between our Open and our flock, removing now would split
	// the mutex.
	pathInfo, err := os.Stat(path)
	if err != nil {
		return false, lockGCOther
	}
	openInfo, err := f.Stat()
	if err != nil {
		return false, lockGCOther
	}
	if !sameInode(pathInfo, openInfo) {
		return false, lockGCRaced
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, lockGCOther
	}
	return true, lockGCRemoved
}

// sameInode reports whether two FileInfo values reference the same dev/inode
// pair on Linux/Darwin (the only platforms maestro currently supports —
// other platforms fall back to "true" so the helper degrades gracefully).
func sameInode(a, b os.FileInfo) bool {
	sa, oka := a.Sys().(*syscall.Stat_t)
	sb, okb := b.Sys().(*syscall.Stat_t)
	if !oka || !okb {
		return true
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino
}
