// Package lock provides file-based locking for daemon coordination.
package lock

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type MutexMap struct {
	mu      sync.Mutex
	mutexes map[string]*sync.Mutex
}

func NewMutexMap() *MutexMap {
	return &MutexMap{
		mutexes: make(map[string]*sync.Mutex),
	}
}

func (m *MutexMap) Lock(key string) {
	m.getMutex(key).Lock()
}

func (m *MutexMap) Unlock(key string) {
	m.mu.Lock()
	mu, ok := m.mutexes[key]
	m.mu.Unlock()
	if !ok {
		return // no-op if key was never locked
	}
	mu.Unlock()
}

// Remove deletes the entry for key from the map, freeing the associated memory.
// The caller must ensure no goroutine is currently holding the lock for key.
func (m *MutexMap) Remove(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mutexes, key)
}

func (m *MutexMap) getMutex(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	if mu, ok := m.mutexes[key]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	m.mutexes[key] = mu
	return mu
}

type FileLock struct {
	path string
	file *os.File
}

func NewFileLock(path string) *FileLock {
	return &FileLock{path: path}
}

func (fl *FileLock) TryLock() error {
	f, err := os.OpenFile(fl.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return fmt.Errorf("acquire lock (another daemon may be running): %w", err)
	}

	// Write PID to lock file
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return fmt.Errorf("seek lock file: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return fmt.Errorf("write PID to lock file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return fmt.Errorf("sync lock file: %w", err)
	}

	fl.file = f
	return nil
}

func (fl *FileLock) Unlock() error {
	if fl.file == nil {
		return nil
	}

	if err := syscall.Flock(int(fl.file.Fd()), syscall.LOCK_UN); err != nil {
		_ = fl.file.Close()
		return fmt.Errorf("release lock: %w", err)
	}

	if err := fl.file.Close(); err != nil {
		return fmt.Errorf("close lock file: %w", err)
	}

	_ = os.Remove(fl.path)
	fl.file = nil
	return nil
}

// ReadLockPID reads the PID from a lock file without acquiring the lock.
// Returns 0 if the file is unreadable or does not contain a valid PID.
func ReadLockPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}
