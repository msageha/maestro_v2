package daemon

import (
	"context"
	"sync"
	"runtime/debug"
	"sync/atomic"
	"time"
)

const (
	// defaultDebounceSec is the fallback debounce delay when the config value
	// is zero or negative. 500ms balances responsiveness with burst coalescing.
	defaultDebounceSec = 0.5
)

// DebounceController manages filesystem event debouncing for scan coalescing.
// It batches rapid filesystem events into a single scan callback, with
// starvation protection to guarantee eventual execution under sustained bursts.
type DebounceController struct {
	debounceSec float64
	dl          *DaemonLogger
	scanFn      func(ctx context.Context) // callback to execute the actual scan

	mu           sync.Mutex
	timer        *time.Timer
	done         chan struct{} // closed when the in-flight callback finishes
	firstTrigger time.Time     // tracks first trigger in a debounce window for maxWait
	running      atomic.Bool   // true while debounced callback is executing

	// Shutdown guard: wired via SetShutdownGuard after construction.
	shutdownCtx  context.Context
	shuttingDown *atomic.Bool
	shutdownFn   func() // triggers daemon shutdown on panic
}

// NewDebounceController creates a DebounceController.
// debounceSec is the configured delay; if <= 0, defaultDebounceSec is used.
// scanFn is called when the debounce timer fires.
func NewDebounceController(debounceSec float64, dl *DaemonLogger, scanFn func(ctx context.Context)) *DebounceController {
	if debounceSec <= 0 {
		debounceSec = defaultDebounceSec
	}
	return &DebounceController{
		debounceSec: debounceSec,
		dl:          dl,
		scanFn:      scanFn,
	}
}

// SetShutdownGuard wires the daemon's shutdown context, advisory flag, and
// shutdown callback so that debounce callbacks respect context cancellation,
// shutdown state, and trigger daemon shutdown on panic.
func (dc *DebounceController) SetShutdownGuard(ctx context.Context, shuttingDown *atomic.Bool, shutdownFn func()) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.shutdownCtx = ctx
	dc.shuttingDown = shuttingDown
	dc.shutdownFn = shutdownFn
}

// Trigger starts or resets the debounce timer. When the timer fires, scanFn
// is called. Burst protection: if a scan is already running, new triggers are
// skipped. Starvation protection: if the first trigger in a debounce window was
// more than maxDebounceSec ago, the callback fires immediately.
func (dc *DebounceController) Trigger(trigger string) {
	// If a scan callback is already executing, skip — it will see latest state.
	if dc.running.Load() {
		return
	}

	dc.mu.Lock()
	defer dc.mu.Unlock()

	now := time.Now()

	// Track the first trigger in this debounce window.
	if dc.firstTrigger.IsZero() {
		dc.firstTrigger = now
	}

	// Starvation guard: if we've been deferring too long, fire now.
	elapsed := now.Sub(dc.firstTrigger).Seconds()
	delay := time.Duration(dc.debounceSec * float64(time.Second))
	if elapsed >= maxDebounceSec {
		delay = 0 // fire immediately
	}

	// Stop previous timer if any.
	if dc.timer != nil {
		dc.timer.Stop()
	}

	done := make(chan struct{})
	dc.done = done

	dc.timer = time.AfterFunc(
		delay,
		func() {
			defer close(done)

			// Guard: if shutting down, bail out immediately.
			if dc.shuttingDown != nil && dc.shuttingDown.Load() {
				return
			}

			// Check context cancellation before starting the scan.
			if dc.shutdownCtx != nil {
				select {
				case <-dc.shutdownCtx.Done():
					return
				default:
				}
			}

			// Mark scan as running; if already running, skip.
			if !dc.running.CompareAndSwap(false, true) {
				dc.dl.Logf(LogLevelDebug, "debounce_scan_skipped reason=scan_already_running trigger=%s", trigger)
				return
			}
			defer dc.running.Store(false)

			// Re-check shutdown after CAS to narrow the TOCTOU window between
			// the initial shuttingDown check and scan execution.
			if dc.shuttingDown != nil && dc.shuttingDown.Load() {
				return
			}

			// Reset first-trigger tracking for next debounce window.
			dc.mu.Lock()
			dc.firstTrigger = time.Time{}
			dc.mu.Unlock()

			defer func() {
				if r := recover(); r != nil {
					dc.dl.Logf(LogLevelError, "panic in debounceAndScan: %v\n%s", r, debug.Stack())
					if dc.shutdownFn != nil {
						dc.shutdownFn()
					}
				}
			}()
			dc.dl.Logf(LogLevelDebug, "debounced_scan trigger=%s", trigger)

			// shutdownCtx is nil only in unit tests where the controller is
			// constructed without a full daemon context. context.Background()
			// is a safe fallback.
			ctx := dc.shutdownCtx
			if ctx == nil {
				ctx = context.Background()
			}
			dc.scanFn(ctx)
		},
	)
}

// Stop cancels any pending debounce timer and waits for any in-flight
// callback goroutine to finish, ensuring no goroutine leak on shutdown.
func (dc *DebounceController) Stop() {
	dc.mu.Lock()
	done := dc.done
	timerWasPending := false
	if dc.timer != nil {
		timerWasPending = dc.timer.Stop()
		dc.timer = nil
	}
	dc.mu.Unlock()

	// Only wait if the timer had already fired (callback may be in-flight).
	// If Stop() returned true the callback will never run, so waiting would hang.
	if done != nil && !timerWasPending {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			dc.dl.Logf(LogLevelWarn, "debounce_stop_timeout: callback did not finish within 5s")
		}
	}
}
