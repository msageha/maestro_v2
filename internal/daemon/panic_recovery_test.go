package daemon

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/stretchr/testify/require"
)

const (
	// panicRecoveryTimeout is the maximum time to wait for panic recovery
	// side effects (shutdown flag, shutdownFn call) to propagate.
	panicRecoveryTimeout = 5 * time.Second
	// panicRecoveryPollInterval is the polling interval while waiting for
	// panic recovery side effects.
	panicRecoveryPollInterval = 5 * time.Millisecond
)

// syncBuffer is a thread-safe bytes.Buffer for use in tests where a log.Logger
// may write concurrently while the test reads the buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sb *syncBuffer) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *syncBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.String()
}

// TestEventBridge_PanicRecoveryCallsShutdown verifies that a panic inside an
// EventBridge callback is recovered, a stack trace is logged, and
// Daemon.Shutdown is invoked (shuttingDown flag becomes true).
func TestEventBridge_PanicRecoveryCallsShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping panic recovery test in short mode")
	}
	t.Parallel()

	var logBuf syncBuffer
	tmpDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := events.NewBus(ctx, 10)
	t.Cleanup(func() {
		if err := bus.Close(); err != nil {
			t.Errorf("bus.Close() error: %v", err)
		}
	})

	d := &Daemon{
		maestroDir: tmpDir,
		config:     model.Config{ShutdownTimeoutSec: 1},
		logLevel:   LogLevelDebug,
		logger:     log.New(&logBuf, "", 0),
		clock:      RealClock{},
		ticker:     time.NewTicker(time.Hour),
		fileLock:   lock.NewFileLock(t.TempDir() + "/daemon.lock"),
		eventBus:   bus,
	}
	d.ctx = ctx
	d.cancel = cancel
	d.bridge = &EventBridge{d: d}

	// Create a valid QualityGateDaemon so subscribeQualityGateEvents subscribes.
	qg := NewQualityGateDaemon(tmpDir, model.Config{}, lock.NewMutexMap(), d.logger, d.logLevel, ctx)
	d.qualityGateDaemon = qg

	d.bridge.subscribeQualityGateEvents()

	// Nil-out qualityGateDaemon and eventBus so the callback panics (nil
	// pointer on EmitEvent) and Shutdown can proceed without deadlocking
	// on eventBus.Close().
	d.qualityGateDaemon = nil
	d.eventBus = nil

	// Publish an event that matches the subscribed type. The callback will
	// attempt d.qualityGateDaemon.EmitEvent(...) which panics.
	bus.Publish(events.EventTaskStarted, map[string]any{
		"task_id":    "test_task",
		"command_id": "test_cmd",
		"worker_id":  "worker1",
	})

	// Wait for Shutdown to set the advisory flag.
	require.Eventually(t, func() bool {
		return d.shuttingDown.Load()
	}, panicRecoveryTimeout, panicRecoveryPollInterval, "expected shuttingDown to be true after panic recovery")

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "panic in event_bridge callback type=task_started") {
		t.Errorf("log should contain panic message, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "goroutine") {
		t.Errorf("log should contain stack trace (goroutine keyword), got: %s", logOutput)
	}
}

// TestDebounceController_PanicRecoveryCallsShutdown verifies that a panic in
// the debounced scanFn is recovered, a stack trace is logged, and the
// configured shutdownFn is called.
func TestDebounceController_PanicRecoveryCallsShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping panic recovery test in short mode")
	}
	t.Parallel()

	var logBuf bytes.Buffer
	dl := NewDaemonLoggerFromLegacy("test_debounce", log.New(&logBuf, "", 0), LogLevelDebug)

	shutdownCalled := atomic.Bool{}

	dc := NewDebounceController(0.01, dl, func(_ context.Context) {
		panic("test panic in scanFn")
	})

	shuttingDown := atomic.Bool{}
	dc.SetShutdownGuard(
		context.Background(),
		&shuttingDown,
		func() { shutdownCalled.Store(true) },
	)

	dc.Trigger("test_trigger")

	// Wait for the debounce timer to fire and the recovery to call shutdownFn.
	require.Eventually(t, func() bool {
		return shutdownCalled.Load()
	}, panicRecoveryTimeout, panicRecoveryPollInterval, "expected shutdownFn to be called after panic recovery")

	dc.Stop()

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "panic in debounceAndScan") {
		t.Errorf("log should contain panic message, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "test panic in scanFn") {
		t.Errorf("log should contain panic value, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "goroutine") {
		t.Errorf("log should contain stack trace (goroutine keyword), got: %s", logOutput)
	}
}
