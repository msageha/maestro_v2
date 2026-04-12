// Package lock provides file-based locking and keyed mutex coordination.
package lock

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// refMutex is a reference-counted mutex entry. The ref field tracks the number
// of goroutines that are either waiting to acquire or currently holding the
// per-key lock. When ref drops to zero the entry is eligible for removal from
// the map, preventing memory leaks. The locked field is an atomic flag (1 =
// locked, 0 = unlocked) used to guard against double-unlock panics.
type refMutex struct {
	mu     sync.Mutex
	ref    int
	locked int32 // atomic: 1 = locked, 0 = unlocked
}

// MutexMap provides per-key mutual exclusion with automatic cleanup.
// Each key maps to a reference-counted mutex. Entries are created on first
// Lock and removed when no goroutine is waiting for or holding the lock.
//
// Lock ordering (enforced programmatically with -tags lockorder):
//
//	queue:* (level 1) → state:* (level 2) → result:* (level 3)
type MutexMap struct {
	mu      sync.Mutex
	mutexes map[string]*refMutex
	order   orderChecker
}

// NewMutexMap creates a new MutexMap ready for use.
func NewMutexMap() *MutexMap {
	return &MutexMap{
		mutexes: make(map[string]*refMutex),
		order:   newOrderChecker(),
	}
}

// Lock acquires the mutex for key, creating it if necessary. The caller must
// call Unlock (or TryUnlock) exactly once when done.
func (m *MutexMap) Lock(key string) {
	m.order.BeforeLock(key)

	m.mu.Lock()
	rm, ok := m.mutexes[key]
	if !ok {
		rm = &refMutex{}
		m.mutexes[key] = rm
	}
	rm.ref++
	m.mu.Unlock()

	rm.mu.Lock()
	atomic.StoreInt32(&rm.locked, 1)

	m.order.AfterLock(key)
}

// Unlock releases the mutex for key. It is safe to call on a key that was
// never locked or has already been unlocked (no-op in those cases).
// Returns true if the unlock was performed, false otherwise.
func (m *MutexMap) Unlock(key string) bool {
	return m.TryUnlock(key)
}

// TryUnlock attempts to release the mutex for key. It returns true if the
// unlock was performed, false if the key was not locked (never locked,
// already unlocked, or non-existent). It never panics.
func (m *MutexMap) TryUnlock(key string) bool {
	m.mu.Lock()
	rm, ok := m.mutexes[key]
	if !ok {
		m.mu.Unlock()
		return false
	}

	// Atomically clear the locked flag. If the CAS fails the mutex is not
	// currently locked (double-unlock or racing unlock) — bail out safely.
	if !atomic.CompareAndSwapInt32(&rm.locked, 1, 0) {
		m.mu.Unlock()
		return false
	}

	// Decrement reference count and clean up while still holding m.mu,
	// closing the TOCTOU window between lookup and cleanup.
	rm.ref--
	if rm.ref == 0 && m.mutexes[key] == rm {
		delete(m.mutexes, key)
	}
	m.order.BeforeUnlock(key)
	m.mu.Unlock()

	rm.mu.Unlock()

	return true
}

// Remove drops the entry for key if no goroutine currently holds or is
// waiting for the lock (ref == 0). It is a no-op when the entry is in use,
// when the key has never been seen, or when the entry was already removed
// by Unlock-time cleanup. It never blocks on the per-key mutex.
//
// Use this for keys whose lifetime is known to have ended (e.g. command-
// scoped state locks after Complete) to make cleanup intent explicit at
// the call site, complementing the implicit ref-count cleanup performed
// by Unlock/TryUnlock. Returns true when an entry was actually removed.
func (m *MutexMap) Remove(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rm, ok := m.mutexes[key]
	if !ok {
		return false
	}
	if rm.ref != 0 {
		return false
	}
	delete(m.mutexes, key)
	return true
}

// len returns the number of keys currently tracked in the map.
func (m *MutexMap) len() int { //nolint:unused // used in tests; golangci-lint runs with tests:false
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.mutexes)
}

// FileLock implements an exclusive file lock using flock(2).
type FileLock struct {
	path string
	file *os.File
}

// NewFileLock creates a FileLock for the given path. The lock is not held until TryLock is called.
func NewFileLock(path string) *FileLock {
	return &FileLock{path: path}
}

// TryLock acquires an exclusive non-blocking flock on the file. Returns an error if another process holds the lock.
func (fl *FileLock) TryLock() error {
	f, err := os.OpenFile(fl.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // uintptr->int conversion for fd is safe on all supported platforms
		_ = f.Close()
		return fmt.Errorf("acquire lock (another daemon may be running): %w", err)
	}

	// Write PID to lock file
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // uintptr->int conversion for fd is safe on all supported platforms
		_ = f.Close()
		return fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // uintptr->int conversion for fd is safe on all supported platforms
		_ = f.Close()
		return fmt.Errorf("seek lock file: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // uintptr->int conversion for fd is safe on all supported platforms
		_ = f.Close()
		return fmt.Errorf("write PID to lock file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // uintptr->int conversion for fd is safe on all supported platforms
		_ = f.Close()
		return fmt.Errorf("sync lock file: %w", err)
	}

	fl.file = f
	return nil
}

// Unlock releases the flock and closes the lock file. It is a no-op if the lock is not held.
func (fl *FileLock) Unlock() error {
	if fl.file == nil {
		return nil
	}

	if err := syscall.Flock(int(fl.file.Fd()), syscall.LOCK_UN); err != nil { //nolint:gosec // uintptr->int conversion for fd is safe on all supported platforms
		_ = fl.file.Close()
		return fmt.Errorf("release lock: %w", err)
	}

	if err := fl.file.Close(); err != nil {
		return fmt.Errorf("close lock file: %w", err)
	}

	// Do not remove the lock file. flock is inode-based: removing the path
	// allows a new file (different inode) to be created at the same path,
	// breaking mutual exclusion for subsequent lockers. Keeping the lock file
	// stable ensures all processes contend on the same inode.
	fl.file = nil
	return nil
}

// ReadLockPID reads the PID from a lock file without acquiring the lock.
// Returns 0 if the file is unreadable or does not contain a valid PID.
func ReadLockPID(path string) int {
	data, err := os.ReadFile(path) //nolint:gosec // path is a lock file path from maestroDir; caller controls path
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}
