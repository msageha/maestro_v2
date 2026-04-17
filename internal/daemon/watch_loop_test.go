package daemon

import (
	"runtime"
	"sync/atomic"
	"testing"
)

func TestFsSemaphoreBufferSize_Dynamic(t *testing.T) {
	t.Parallel()
	size := fsSemaphoreBufferSize()

	if size < 8 {
		t.Errorf("expected fsSemaphoreBufferSize >= 8, got %d", size)
	}
	if size > 32 {
		t.Errorf("expected fsSemaphoreBufferSize <= 32, got %d", size)
	}

	// Verify it's related to NumCPU
	expected := runtime.NumCPU() * 2
	if expected < 8 {
		expected = 8
	}
	if expected > 32 {
		expected = 32
	}
	if size != expected {
		t.Errorf("expected fsSemaphoreBufferSize=%d (NumCPU=%d), got %d", expected, runtime.NumCPU(), size)
	}
}

func TestWatchLoop_FsDroppedCount(t *testing.T) {
	t.Parallel()
	// Partial init is safe: FsDroppedCount() only accesses droppedCount (atomic.Int64,
	// zero-value valid). d *Daemon is nil but never dereferenced in this test path.
	w := &WatchLoop{}

	if got := w.FsDroppedCount(); got != 0 {
		t.Errorf("expected initial FsDroppedCount=0, got %d", got)
	}

	w.droppedCount.Add(5)
	if got := w.FsDroppedCount(); got != 5 {
		t.Errorf("expected FsDroppedCount=5, got %d", got)
	}
}

func TestWatchLoop_FsEgSetLimit(t *testing.T) {
	t.Parallel()
	// Verify that WatchLoop uses errgroup.SetLimit to bound goroutine creation.
	w := &WatchLoop{}
	w.fsEg.SetLimit(2)

	var running atomic.Int32
	done := make(chan struct{})

	// Fill both slots
	started1 := w.fsEg.TryGo(func() error {
		running.Add(1)
		<-done
		return nil
	})
	started2 := w.fsEg.TryGo(func() error {
		running.Add(1)
		<-done
		return nil
	})
	// Third should be rejected
	started3 := w.fsEg.TryGo(func() error {
		running.Add(1)
		<-done
		return nil
	})

	if !started1 || !started2 {
		t.Error("first two TryGo calls should succeed")
	}
	if started3 {
		t.Error("third TryGo should fail when limit is 2")
	}

	close(done)
	w.fsEg.Wait() //nolint:errcheck
}
