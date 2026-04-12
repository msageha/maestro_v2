package daemon

import (
	"runtime"
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
	w := &WatchLoop{
		fsSem: make(chan struct{}, 1),
	}

	if got := w.FsDroppedCount(); got != 0 {
		t.Errorf("expected initial FsDroppedCount=0, got %d", got)
	}

	w.droppedCount.Add(5)
	if got := w.FsDroppedCount(); got != 5 {
		t.Errorf("expected FsDroppedCount=5, got %d", got)
	}
}
