package daemon

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDebounceController_StopTimeout(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	dl := NewDaemonLoggerFromLegacy("test_debounce", log.New(&logBuf, "", 0), LogLevelDebug)

	// scanFn blocks until explicitly cancelled via context, simulating a stuck callback.
	scanCtx, scanCancel := context.WithCancel(context.Background())
	defer scanCancel()

	dc := NewDebounceController(0.01, dl, func(_ context.Context) {
		<-scanCtx.Done()
	})

	shuttingDown := atomic.Bool{}
	dc.SetShutdownGuard(
		context.Background(),
		&shuttingDown,
		func() {},
	)

	dc.Trigger("test_trigger")

	// Poll until the callback is running instead of a fixed sleep.
	require.Eventually(t, func() bool {
		return dc.running.Load()
	}, 5*time.Second, 10*time.Millisecond, "callback did not start running")

	// Stop should return within ~5s due to the timeout, not block forever.
	stopDone := make(chan struct{})
	go func() {
		dc.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		// Stop returned — expected
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() did not return within 10s; timeout mechanism may be broken")
	}

	// Unblock the callback so the goroutine can exit cleanly.
	scanCancel()

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "debounce_stop_timeout") {
		t.Errorf("expected timeout log message, got: %s", logOutput)
	}
}

func TestDebounceController_StopNormalCompletion(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	dl := NewDaemonLoggerFromLegacy("test_debounce", log.New(&logBuf, "", 0), LogLevelDebug)

	// Channel-based sync: test explicitly controls when scanFn completes,
	// replacing non-deterministic time.Sleep with deterministic signaling.
	scanStarted := make(chan struct{})
	scanRelease := make(chan struct{})

	dc := NewDebounceController(0.01, dl, func(_ context.Context) {
		close(scanStarted)
		<-scanRelease
	})

	shuttingDown := atomic.Bool{}
	dc.SetShutdownGuard(
		context.Background(),
		&shuttingDown,
		func() {},
	)

	dc.Trigger("test_trigger")

	// Wait for callback to start via channel signal.
	select {
	case <-scanStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not start within timeout")
	}

	// Release the callback so it completes normally.
	close(scanRelease)

	// Wait for callback to finish.
	require.Eventually(t, func() bool {
		return !dc.running.Load()
	}, 5*time.Second, 10*time.Millisecond, "callback did not finish")

	dc.Stop()

	logOutput := logBuf.String()
	if strings.Contains(logOutput, "debounce_stop_timeout") {
		t.Errorf("unexpected timeout log for a normally completing callback: %s", logOutput)
	}
}
