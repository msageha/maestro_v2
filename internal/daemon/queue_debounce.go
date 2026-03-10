package daemon

import (
	"context"
	"time"
)

// maxDebounceSec is the upper bound on how long triggers can be coalesced
// before forcing a scan, preventing indefinite starvation under sustained bursts.
const maxDebounceSec = 5.0

// debounceAndScan applies debounce logic before triggering a scan.
// The debounce timer callback checks context cancellation and the advisory
// shuttingDown flag to skip scans during shutdown.
//
// Burst protection: if a scan is already running (scanRunning flag), new
// triggers are skipped — the running scan will pick up the latest state.
// Starvation protection: if the first trigger in a debounce window was more
// than maxDebounceSec ago, the callback fires immediately instead of resetting.
func (qh *QueueHandler) debounceAndScan(trigger string) {
	// If a scan callback is already executing, skip — it will see latest state.
	if qh.scanRunning.Load() {
		return
	}

	debounceSec := qh.config.Watcher.DebounceSec
	if debounceSec <= 0 {
		debounceSec = 0.5
	}

	qh.debounceMu.Lock()
	defer qh.debounceMu.Unlock()

	now := time.Now()

	// Track the first trigger in this debounce window.
	if qh.firstTriggerAt.IsZero() {
		qh.firstTriggerAt = now
	}

	// Starvation guard: if we've been deferring too long, fire now.
	elapsed := now.Sub(qh.firstTriggerAt).Seconds()
	delay := time.Duration(debounceSec * float64(time.Second))
	if elapsed >= maxDebounceSec {
		delay = 0 // fire immediately
	}

	// Stop previous timer if any.
	if qh.debounceTimer != nil {
		qh.debounceTimer.Stop()
	}

	done := make(chan struct{})
	qh.debounceDone = done

	qh.debounceTimer = time.AfterFunc(
		delay,
		func() {
			defer close(done)

			// Guard: if shutting down, bail out immediately.
			if qh.shuttingDown != nil && qh.shuttingDown.Load() {
				return
			}

			// Check context cancellation before starting the scan.
			if qh.shutdownCtx != nil {
				select {
				case <-qh.shutdownCtx.Done():
					return
				default:
				}
			}

			// Mark scan as running; if already running, skip.
			if !qh.scanRunning.CompareAndSwap(false, true) {
				return
			}
			defer qh.scanRunning.Store(false)

			// Re-check shutdown after CAS to narrow the TOCTOU window between
			// the initial shuttingDown check and scan execution.
			if qh.shuttingDown != nil && qh.shuttingDown.Load() {
				return
			}

			// Reset first-trigger tracking for next debounce window.
			qh.debounceMu.Lock()
			qh.firstTriggerAt = time.Time{}
			qh.debounceMu.Unlock()

			defer func() {
				if r := recover(); r != nil {
					qh.log(LogLevelError, "panic in debounceAndScan: %v", r)
				}
			}()
			qh.log(LogLevelDebug, "debounced_scan trigger=%s", trigger)

			// shutdownCtx is nil only in unit tests where QueueHandler is
			// constructed without a full daemon context. context.Background()
			// is a safe fallback — test teardown handles cancellation separately.
			ctx := qh.shutdownCtx
			if ctx == nil {
				ctx = context.Background()
			}
			qh.PeriodicScanWithContext(ctx)
		},
	)
}
