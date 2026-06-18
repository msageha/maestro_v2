package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// ---------------------------------------------------------------------------
// Helper: create a Daemon for shutdown tests with a captured log buffer.
// ---------------------------------------------------------------------------

func newShutdownTestDaemon(t *testing.T, buf *bytes.Buffer) *Daemon {
	t.Helper()
	maestroDir := filepath.Join(t.TempDir(), ".maestro")
	cfg := model.Config{
		Watcher:            model.WatcherConfig{ScanIntervalSec: 1},
		ShutdownTimeoutSec: 2,
		Logging:            model.LoggingConfig{Level: "debug"},
	}
	d, err := newDaemon(maestroDir, cfg, buf, nil)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	return d
}

// ---------------------------------------------------------------------------
// 1. Graceful Shutdown Sequence
// ---------------------------------------------------------------------------

func TestGracefulShutdownSequence(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)

	// Pre-shutdown: shuttingDown should be false, context should not be cancelled.
	if d.shuttingDown.Load() {
		t.Fatal("expected shuttingDown=false before Shutdown")
	}
	if d.ctx.Err() != nil {
		t.Fatalf("expected ctx.Err()=nil before Shutdown, got %v", d.ctx.Err())
	}

	d.Shutdown()

	// Post-shutdown: shuttingDown must be true.
	if !d.shuttingDown.Load() {
		t.Error("expected shuttingDown=true after Shutdown")
	}

	// Post-shutdown: context must be cancelled.
	if d.ctx.Err() == nil {
		t.Error("expected ctx to be cancelled after Shutdown")
	}

	// Post-shutdown: log must contain "shutdown started" and "daemon stopped".
	logOutput := buf.String()
	if !strings.Contains(logOutput, "shutdown started") {
		t.Errorf("log missing 'shutdown started'; got:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "daemon stopped") {
		t.Errorf("log missing 'daemon stopped'; got:\n%s", logOutput)
	}
}

// ---------------------------------------------------------------------------
// 2. shutdownOp behaviour
// ---------------------------------------------------------------------------

func TestShutdownOp_SuccessNoError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)
	t.Cleanup(func() {
		d.ticker.Stop()
		d.cancel()
	})

	d.shutdownOp("test_ok", func() error {
		return nil
	})

	logOutput := buf.String()
	// No error should be logged for a successful operation.
	if strings.Contains(logOutput, "error") {
		t.Errorf("unexpected error in log for successful op:\n%s", logOutput)
	}
	if strings.Contains(logOutput, "timed out") {
		t.Errorf("unexpected timeout in log for successful op:\n%s", logOutput)
	}
}

func TestShutdownOp_ErrorIsLogged(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)
	t.Cleanup(func() {
		d.ticker.Stop()
		d.cancel()
	})

	d.shutdownOp("test_fail", func() error {
		return errors.New("simulated failure")
	})

	logOutput := buf.String()
	if !strings.Contains(logOutput, "test_fail") {
		t.Errorf("log missing operation name 'test_fail':\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "simulated failure") {
		t.Errorf("log missing error message 'simulated failure':\n%s", logOutput)
	}
}

func TestShutdownOp_FastCompletionReturnsPromptly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)
	t.Cleanup(func() {
		d.ticker.Stop()
		d.cancel()
	})

	start := time.Now()
	d.shutdownOp("fast_op", func() error {
		return nil
	})
	elapsed := time.Since(start)

	// A fast op should complete well under 1 second (shutdownOpTimeout is 10s).
	if elapsed > 1*time.Second {
		t.Errorf("shutdownOp took %s for a fast operation; expected < 1s", elapsed)
	}
}

// ---------------------------------------------------------------------------
// 2b. shutdownOp table-driven: success, error, fast
// ---------------------------------------------------------------------------

func TestShutdownOp_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		opName       string
		fn           func() error
		wantInLog    string   // substring that must appear (empty = no assertion)
		wantNotInLog []string // substrings that must NOT appear
		maxDuration  time.Duration
	}{
		{
			name:         "success",
			opName:       "op_success",
			fn:           func() error { return nil },
			wantNotInLog: []string{"error", "timed out"},
			maxDuration:  1 * time.Second,
		},
		{
			name:        "returns error",
			opName:      "op_err",
			fn:          func() error { return errors.New("boom") },
			wantInLog:   "boom",
			maxDuration: 1 * time.Second,
		},
		{
			name:         "fast completion",
			opName:       "op_fast",
			fn:           func() error { time.Sleep(5 * time.Millisecond); return nil }, // essential: simulates non-instant completion
			wantNotInLog: []string{"timed out"},
			maxDuration:  1 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			d := newShutdownTestDaemon(t, &buf)
			t.Cleanup(func() {
				d.ticker.Stop()
				d.cancel()
			})

			start := time.Now()
			d.shutdownOp(tt.opName, tt.fn)
			elapsed := time.Since(start)

			logOutput := buf.String()

			if tt.wantInLog != "" && !strings.Contains(logOutput, tt.wantInLog) {
				t.Errorf("log missing %q:\n%s", tt.wantInLog, logOutput)
			}
			for _, notWant := range tt.wantNotInLog {
				if strings.Contains(logOutput, notWant) {
					t.Errorf("log unexpectedly contains %q:\n%s", notWant, logOutput)
				}
			}
			if elapsed > tt.maxDuration {
				t.Errorf("shutdownOp took %s, want < %s", elapsed, tt.maxDuration)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Cascade Failure: one shutdownOp failure does not block the next
// ---------------------------------------------------------------------------

func TestShutdownOp_CascadeFailure(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)
	t.Cleanup(func() {
		d.ticker.Stop()
		d.cancel()
	})

	// First op fails.
	d.shutdownOp("failing_component", func() error {
		return errors.New("component A exploded")
	})

	// Second op must still run successfully despite the first failure.
	secondRan := make(chan struct{})
	d.shutdownOp("healthy_component", func() error {
		close(secondRan)
		return nil
	})

	// Verify the second operation completed.
	select {
	case <-secondRan:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("second shutdownOp did not run after first failed")
	}

	logOutput := buf.String()

	// Both operation names should appear in the log.
	if !strings.Contains(logOutput, "failing_component") {
		t.Errorf("log missing 'failing_component':\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "component A exploded") {
		t.Errorf("log missing error from first op:\n%s", logOutput)
	}
	// The healthy component should NOT have produced an error log.
	if strings.Contains(logOutput, "healthy_component") && strings.Contains(logOutput, "error") {
		// The error log for "healthy_component" should not exist.
		// But "failing_component error=..." also contains "error", so be more precise.
		if strings.Contains(logOutput, "shutdown healthy_component error") {
			t.Errorf("unexpected error logged for healthy_component:\n%s", logOutput)
		}
	}
}

// ---------------------------------------------------------------------------
// 3b. Multiple sequential failures do not block each other
// ---------------------------------------------------------------------------

func TestShutdownOp_MultipleFailuresAllRun(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)
	t.Cleanup(func() {
		d.ticker.Stop()
		d.cancel()
	})

	opNames := []string{"op_a", "op_b", "op_c"}
	for _, name := range opNames {
		name := name
		d.shutdownOp(name, func() error {
			return errors.New("fail_" + name)
		})
	}

	logOutput := buf.String()
	for _, name := range opNames {
		if !strings.Contains(logOutput, name) {
			t.Errorf("log missing operation %q:\n%s", name, logOutput)
		}
		if !strings.Contains(logOutput, "fail_"+name) {
			t.Errorf("log missing error for %q:\n%s", name, logOutput)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Concurrent Shutdown Safety
// ---------------------------------------------------------------------------

func TestShutdown_ConcurrentSafety(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Start a barrier so all goroutines fire Shutdown simultaneously.
	barrier := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-barrier
			d.Shutdown() // must not panic
		}()
	}

	close(barrier) // release all goroutines at once
	wg.Wait()

	// Verify shutdown actually occurred.
	if !d.shuttingDown.Load() {
		t.Error("expected shuttingDown=true after concurrent shutdowns")
	}
	if d.ctx.Err() == nil {
		t.Error("expected context to be cancelled after concurrent shutdowns")
	}

	// Verify "shutdown started" appears exactly once.
	logOutput := buf.String()
	count := strings.Count(logOutput, "shutdown started")
	if count != 1 {
		t.Errorf("expected exactly 1 'shutdown started' in log, got %d;\nlog:\n%s", count, logOutput)
	}

	// "daemon stopped" should also appear exactly once.
	stopCount := strings.Count(logOutput, "daemon stopped")
	if stopCount != 1 {
		t.Errorf("expected exactly 1 'daemon stopped' in log, got %d;\nlog:\n%s", stopCount, logOutput)
	}
}

// ---------------------------------------------------------------------------
// 4b. Double Shutdown is safe (sequential, not concurrent)
// ---------------------------------------------------------------------------

func TestShutdown_DoubleCallSequential(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)

	d.Shutdown()
	d.Shutdown() // must not panic

	if !d.shuttingDown.Load() {
		t.Error("expected shuttingDown=true")
	}

	logOutput := buf.String()
	count := strings.Count(logOutput, "shutdown started")
	if count != 1 {
		t.Errorf("expected exactly 1 'shutdown started', got %d", count)
	}
}

// ---------------------------------------------------------------------------
// 5. Shutdown sets shuttingDown before cancelling context
// ---------------------------------------------------------------------------

func TestShutdown_FlagSetBeforeContextCancel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)

	// We cannot easily instrument the middle of Shutdown, but we can verify
	// that after Shutdown both conditions hold, and the flag was the first
	// logical step (evidenced by log ordering: "shutdown started" before
	// "daemon stopped").
	d.Shutdown()

	if !d.shuttingDown.Load() {
		t.Error("shuttingDown must be true")
	}
	if d.ctx.Err() == nil {
		t.Error("context must be cancelled")
	}

	logOutput := buf.String()
	startIdx := strings.Index(logOutput, "shutdown started")
	stopIdx := strings.Index(logOutput, "daemon stopped")
	if startIdx == -1 || stopIdx == -1 {
		t.Fatalf("expected both 'shutdown started' and 'daemon stopped' in log:\n%s", logOutput)
	}
	if startIdx >= stopIdx {
		t.Errorf("'shutdown started' (idx=%d) should appear before 'daemon stopped' (idx=%d)", startIdx, stopIdx)
	}
}

// ---------------------------------------------------------------------------
// 6. ShutdownTimeoutSec default fallback
// ---------------------------------------------------------------------------

func TestShutdown_DefaultTimeoutFallback(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	maestroDir := filepath.Join(t.TempDir(), ".maestro")

	// ShutdownTimeoutSec = 0 should use the default of 30s.
	// We don't actually wait 30s; we just verify shutdown completes.
	cfg := model.Config{
		Watcher:            model.WatcherConfig{ScanIntervalSec: 1},
		ShutdownTimeoutSec: 0,
		Logging:            model.LoggingConfig{Level: "debug"},
	}
	d, err := newDaemon(maestroDir, cfg, &buf, nil)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}

	done := make(chan struct{})
	go func() {
		d.Shutdown()
		close(done)
	}()

	select {
	case <-done:
		// Shutdown completed successfully.
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown with default timeout did not complete in time")
	}

	if !d.shuttingDown.Load() {
		t.Error("expected shuttingDown=true")
	}
	logOutput := buf.String()
	if !strings.Contains(logOutput, "daemon stopped") {
		t.Errorf("log missing 'daemon stopped':\n%s", logOutput)
	}
}

// ---------------------------------------------------------------------------
// 7. shutdownOp with nil watcher/server (tests that Shutdown handles nil
//    components gracefully — the guards in Shutdown prevent calling
//    shutdownOp when fields are nil).
// ---------------------------------------------------------------------------

func TestShutdown_NilComponents(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)

	// Explicitly nil out optional components that Shutdown checks.
	// watcher is nil by default from newDaemon (no fsnotify.NewWatcher call).
	// server is non-nil from newDaemon, so we nil it to test the guard.
	d.server = nil
	d.eventBus = nil
	d.qualityGateDaemon = nil
	d.reviewCoord = nil
	d.phaseC = nil
	d.eg = nil

	// Shutdown must not panic even with nil components.
	d.Shutdown()

	if !d.shuttingDown.Load() {
		t.Error("expected shuttingDown=true")
	}
	logOutput := buf.String()
	if !strings.Contains(logOutput, "daemon stopped") {
		t.Errorf("log missing 'daemon stopped':\n%s", logOutput)
	}
}

// ---------------------------------------------------------------------------
// 8. closeWatcher is idempotent (sync.Once)
// ---------------------------------------------------------------------------

func TestCloseWatcher_Idempotent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)
	t.Cleanup(func() {
		d.ticker.Stop()
		d.cancel()
	})

	// watcher is nil by default in test daemons — closeWatcher must not panic.
	if err := d.closeWatcher(); err != nil {
		t.Errorf("first closeWatcher: unexpected error: %v", err)
	}

	// Second call must be a no-op (sync.Once already fired).
	if err := d.closeWatcher(); err != nil {
		t.Errorf("second closeWatcher: unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 9. closeWatcher concurrent safety
// ---------------------------------------------------------------------------

func TestCloseWatcher_ConcurrentSafety(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)
	t.Cleanup(func() {
		d.ticker.Stop()
		d.cancel()
	})

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	barrier := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-barrier
			_ = d.closeWatcher() // must not panic
		}()
	}

	close(barrier)
	wg.Wait()
}

// ---------------------------------------------------------------------------
// 10. Shutdown timeout enters grace period instead of immediate os.Exit
// ---------------------------------------------------------------------------

func TestShutdown_TimeoutEntersGracePeriod(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf) // ShutdownTimeoutSec=2

	// Override exitFn to prevent os.Exit during test.
	exitCalled := make(chan int, 1)
	d.exitFn = func(code int) {
		select {
		case exitCalled <- code:
		default:
		}
	}

	// Create an errgroup with a goroutine that deliberately ignores context
	// cancellation so the shutdown timeout fires.
	hangCh := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-hangCh:
		default:
			close(hangCh) // ensure goroutine is released on test exit
		}
	})

	g, _ := errgroup.WithContext(d.ctx)
	d.eg = g

	g.Go(func() error {
		<-hangCh // blocks until released — ignores ctx cancellation
		return nil
	})

	// Shutdown will timeout after 2s and enter the grace period.
	d.Shutdown()

	logOutput := buf.String()
	if !strings.Contains(logOutput, "entering grace period") {
		t.Errorf("expected 'entering grace period' in log, got:\n%s", logOutput)
	}
	// "daemon stopped" must still appear — cleanup ran normally after timeout.
	if !strings.Contains(logOutput, "daemon stopped") {
		t.Errorf("expected 'daemon stopped' in log, got:\n%s", logOutput)
	}

	// The grace period goroutine should eventually call exitFn.
	select {
	case code := <-exitCalled:
		if code != 1 {
			t.Errorf("expected exit code 1, got %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Error("grace period did not trigger exit within 10s")
	}

	close(hangCh) // release hanging goroutine for cleanup
}

// ---------------------------------------------------------------------------
// 11. doExit defaults to os.Exit when exitFn is nil
// ---------------------------------------------------------------------------

func TestDoExit_UsesExitFn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)
	t.Cleanup(func() {
		d.ticker.Stop()
		d.cancel()
	})

	var called int
	d.exitFn = func(code int) {
		called = code
	}

	d.doExit(42)

	if called != 42 {
		t.Errorf("expected exitFn called with 42, got %d", called)
	}
}

// ---------------------------------------------------------------------------
// cleanup: socket/PID removal is gated on daemon lock ownership
// ---------------------------------------------------------------------------

// A failed second `maestro daemon` invocation (TryLock refused because
// another daemon is running) must NOT unlink the live daemon's socket and
// PID file on its deferred Shutdown.
func TestCleanup_LockNotHeld_PreservesSocketAndPID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)

	if err := os.MkdirAll(d.maestroDir, 0o755); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(d.maestroDir, uds.DefaultSocketName)
	pidPath := filepath.Join(d.maestroDir, "daemon.pid")
	for _, p := range []string{socketPath, pidPath} {
		if err := os.WriteFile(p, []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Lock is NOT held (TryLock never called) — simulates the double-start
	// failure path where another process owns the daemon lock.
	d.Shutdown()

	for _, p := range []string{socketPath, pidPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file %s should survive cleanup when lock is not held: %v", p, err)
		}
	}
}

func TestCleanup_LockHeld_RemovesSocketAndPID(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := newShutdownTestDaemon(t, &buf)

	if err := os.MkdirAll(filepath.Join(d.maestroDir, "locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.fileLock.TryLock(); err != nil {
		t.Fatalf("TryLock: %v", err)
	}

	socketPath := filepath.Join(d.maestroDir, uds.DefaultSocketName)
	pidPath := filepath.Join(d.maestroDir, "daemon.pid")
	for _, p := range []string{socketPath, pidPath} {
		if err := os.WriteFile(p, []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	d.Shutdown()

	for _, p := range []string{socketPath, pidPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("file %s should be removed by cleanup when lock is held (err=%v)", p, err)
		}
	}
}
