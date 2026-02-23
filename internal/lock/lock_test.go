package lock

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMutexMap_LockUnlock(t *testing.T) {
	m := NewMutexMap()

	m.Lock("agent1")
	m.Unlock("agent1")

	// Should be able to lock again
	m.Lock("agent1")
	m.Unlock("agent1")
}

func TestMutexMap_DifferentKeys(t *testing.T) {
	m := NewMutexMap()

	done := make(chan struct{})

	m.Lock("agent1")
	go func() {
		// agent2 should not be blocked by agent1
		m.Lock("agent2")
		m.Unlock("agent2")
		close(done)
	}()

	<-done
	m.Unlock("agent1")
}

func TestMutexMap_Concurrent(t *testing.T) {
	m := NewMutexMap()
	var counter int64

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Lock("shared")
			atomic.AddInt64(&counter, 1)
			m.Unlock("shared")
		}()
	}
	wg.Wait()

	if counter != 100 {
		t.Errorf("expected counter=100, got %d", counter)
	}
}

func TestFileLock_TryLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "daemon.lock")

	fl := NewFileLock(lockPath)
	if err := fl.TryLock(); err != nil {
		t.Fatalf("TryLock failed: %v", err)
	}
	defer fl.Unlock()
}

func TestFileLock_DoubleLockRejected(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "daemon.lock")

	fl1 := NewFileLock(lockPath)
	if err := fl1.TryLock(); err != nil {
		t.Fatalf("first TryLock failed: %v", err)
	}
	defer fl1.Unlock()

	fl2 := NewFileLock(lockPath)
	if err := fl2.TryLock(); err == nil {
		fl2.Unlock()
		t.Fatal("expected second TryLock to fail")
	}
}

func TestFileLock_UnlockAllowsRelock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "daemon.lock")

	fl1 := NewFileLock(lockPath)
	if err := fl1.TryLock(); err != nil {
		t.Fatalf("first TryLock failed: %v", err)
	}
	if err := fl1.Unlock(); err != nil {
		t.Fatalf("Unlock failed: %v", err)
	}

	fl2 := NewFileLock(lockPath)
	if err := fl2.TryLock(); err != nil {
		t.Fatalf("re-lock after unlock failed: %v", err)
	}
	fl2.Unlock()
}

func TestFileLock_DoubleUnlockSafe(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "daemon.lock")

	fl := NewFileLock(lockPath)
	fl.TryLock()
	fl.Unlock()
	// Double unlock should be safe
	if err := fl.Unlock(); err != nil {
		t.Fatalf("double unlock should be safe, got: %v", err)
	}
}
