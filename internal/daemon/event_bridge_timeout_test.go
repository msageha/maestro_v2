package daemon

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer wraps bytes.Buffer with a mutex for concurrent access.
// Prevents DATA RACE between log writes (drain goroutine) and test reads.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sb *safeBuffer) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *safeBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.String()
}

func TestEventBridge_RunWithTimeout_FastCallback(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	d := &Daemon{
		logLevel: LogLevelDebug,
		logger:   logger,
		clock:    RealClock{},
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	defer d.cancel()

	eb := &EventBridge{d: d}

	ok := eb.runWithTimeout("test_fast", func(_ context.Context) {
		// completes instantly
	})
	if !ok {
		t.Error("expected fast callback to complete within timeout")
	}
}

func TestEventBridge_CallbackTimeoutConstant(t *testing.T) {
	if eventBridgeCallbackTimeout != 10*time.Second {
		t.Errorf("expected eventBridgeCallbackTimeout=10s, got %s", eventBridgeCallbackTimeout)
	}
}

// TestEventBridge_RunWithTimeout_DrainGoroutineLogsCompletion verifies that
// when a callback times out, a drain goroutine waits for it to finish and
// logs the late completion. This prevents goroutine leaks (H-bug1).
func TestEventBridge_RunWithTimeout_DrainGoroutineLogsCompletion(t *testing.T) {
	t.Parallel()

	var logBuf safeBuffer
	logger := log.New(&logBuf, "", 0)

	d := &Daemon{
		logLevel: LogLevelDebug,
		logger:   logger,
		clock:    RealClock{},
	}
	// Very short parent context so the derived timeout fires quickly.
	d.ctx, d.cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer d.cancel()

	eb := &EventBridge{d: d}

	unblock := make(chan struct{})
	ok := eb.runWithTimeout("test_drain", func(_ context.Context) {
		<-unblock // blocks until closed, ignoring ctx
	})
	if ok {
		t.Fatal("expected timeout, got success")
	}

	// Unblock fn so the drain goroutine can observe completion.
	close(unblock)

	// Poll for the drain goroutine's log message instead of sleeping a fixed duration.
	deadline := time.After(5 * time.Second)
	for {
		if strings.Contains(logBuf.String(), "completed after timeout") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for drain goroutine log, got: %s", logBuf.String())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
