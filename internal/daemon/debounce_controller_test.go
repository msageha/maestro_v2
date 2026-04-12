package daemon

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

	// Wait briefly for the timer to fire and the callback to start.
	time.Sleep(100 * time.Millisecond)

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

	// scanFn completes quickly.
	dc := NewDebounceController(0.01, dl, func(_ context.Context) {
		time.Sleep(50 * time.Millisecond)
	})

	shuttingDown := atomic.Bool{}
	dc.SetShutdownGuard(
		context.Background(),
		&shuttingDown,
		func() {},
	)

	dc.Trigger("test_trigger")

	// Wait for callback to finish.
	time.Sleep(200 * time.Millisecond)

	dc.Stop()

	logOutput := logBuf.String()
	if strings.Contains(logOutput, "debounce_stop_timeout") {
		t.Errorf("unexpected timeout log for a normally completing callback: %s", logOutput)
	}
}
