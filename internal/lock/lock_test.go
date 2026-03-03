package lock

import (
	"fmt"
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

func TestMutexMap_AutoCleanup(t *testing.T) {
	m := NewMutexMap()

	// Lock and unlock several keys — all entries should be cleaned up.
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("key-%d", i)
		m.Lock(key)
		m.Unlock(key)
	}

	if n := m.Len(); n != 0 {
		t.Errorf("expected 0 tracked keys after unlock, got %d", n)
	}
}

func TestMutexMap_AutoCleanupConcurrent(t *testing.T) {
	m := NewMutexMap()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Lock("shared")
			m.Unlock("shared")
		}()
	}
	wg.Wait()

	if n := m.Len(); n != 0 {
		t.Errorf("expected 0 tracked keys after concurrent lock/unlock, got %d", n)
	}
}

func TestMutexMap_TryUnlock(t *testing.T) {
	m := NewMutexMap()

	m.Lock("key1")

	if ok := m.TryUnlock("key1"); !ok {
		t.Error("TryUnlock should return true for a locked key")
	}

	// Second TryUnlock should return false (already unlocked).
	if ok := m.TryUnlock("key1"); ok {
		t.Error("TryUnlock should return false for an already-unlocked key")
	}
}

func TestMutexMap_TryUnlockNonExistent(t *testing.T) {
	m := NewMutexMap()

	// TryUnlock on a key that was never locked should return false, not panic.
	if ok := m.TryUnlock("no-such-key"); ok {
		t.Error("TryUnlock should return false for a non-existent key")
	}
}

func TestMutexMap_DoubleUnlockSafe(t *testing.T) {
	m := NewMutexMap()

	m.Lock("key1")
	m.Unlock("key1")

	// Double Unlock must not panic.
	m.Unlock("key1")

	// After double-unlock the map should still be clean.
	if n := m.Len(); n != 0 {
		t.Errorf("expected 0 tracked keys after double unlock, got %d", n)
	}
}

func TestMutexMap_UnlockNeverLocked(t *testing.T) {
	m := NewMutexMap()

	// Unlock on a key that was never locked must not panic.
	m.Unlock("phantom")

	if n := m.Len(); n != 0 {
		t.Errorf("expected 0 tracked keys, got %d", n)
	}
}

func TestMutexMap_RelockAfterAutoCleanup(t *testing.T) {
	m := NewMutexMap()

	// Lock, unlock, verify cleanup, then lock again.
	m.Lock("key1")
	m.Unlock("key1")

	if n := m.Len(); n != 0 {
		t.Fatalf("expected 0 tracked keys after unlock, got %d", n)
	}

	// Re-locking the same key after cleanup must work.
	m.Lock("key1")
	if n := m.Len(); n != 1 {
		t.Errorf("expected 1 tracked key after re-lock, got %d", n)
	}
	m.Unlock("key1")

	if n := m.Len(); n != 0 {
		t.Errorf("expected 0 tracked keys after final unlock, got %d", n)
	}
}

func TestMutexMap_ConcurrentTryUnlock(t *testing.T) {
	m := NewMutexMap()

	m.Lock("key1")

	// Multiple goroutines race to TryUnlock — exactly one should succeed.
	const N = 50
	var successCount int64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.TryUnlock("key1") {
				atomic.AddInt64(&successCount, 1)
			}
		}()
	}
	wg.Wait()

	if successCount != 1 {
		t.Errorf("expected exactly 1 successful TryUnlock, got %d", successCount)
	}

	if n := m.Len(); n != 0 {
		t.Errorf("expected 0 tracked keys, got %d", n)
	}
}

func TestMutexMap_ConcurrentMultiKeyCleanup(t *testing.T) {
	m := NewMutexMap()

	// Concurrent lock/unlock across many distinct keys.
	const N = 100
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			m.Lock(key)
			m.Unlock(key)
		}(i)
	}
	wg.Wait()

	if n := m.Len(); n != 0 {
		t.Errorf("expected 0 tracked keys after multi-key concurrent lock/unlock, got %d", n)
	}
}

func TestMutexMap_ConcurrentDoubleUnlock(t *testing.T) {
	m := NewMutexMap()

	// Ensure concurrent double-unlock calls don't panic or corrupt state.
	const rounds = 50
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Lock("shared")
			// Two goroutines racing to unlock the same acquisition.
			var inner sync.WaitGroup
			inner.Add(2)
			go func() {
				defer inner.Done()
				m.Unlock("shared")
			}()
			go func() {
				defer inner.Done()
				m.Unlock("shared")
			}()
			inner.Wait()
		}()
	}
	wg.Wait()

	if n := m.Len(); n != 0 {
		t.Errorf("expected 0 tracked keys after concurrent double unlock, got %d", n)
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
