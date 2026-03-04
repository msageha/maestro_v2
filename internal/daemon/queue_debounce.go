package daemon

import (
	"context"
	"time"
)

// debounceAndScan applies debounce logic before triggering a scan.
// The debounce timer callback is tracked by the daemon's WaitGroup:
//   - wg.Add(1) is called BEFORE time.AfterFunc (guarantees tracking)
//   - The callback defers wg.Done() at entry
//   - If timer.Stop() returns true (callback prevented), wg.Done() is called
//     to balance the Add, both here and in Stop()
func (qh *QueueHandler) debounceAndScan(trigger string) {
	debounceSec := qh.config.Watcher.DebounceSec
	if debounceSec <= 0 {
		debounceSec = 0.5
	}

	qh.debounceMu.Lock()
	defer qh.debounceMu.Unlock()

	// Stop previous timer. If Stop returns true, the callback was prevented
	// from running, so balance the wg.Add(1) from the previous scheduling.
	if qh.debounceTimer != nil {
		if qh.debounceTimer.Stop() && qh.debounceTracked {
			qh.shutdownWg.Done()
		}
	}

	// Track this callback in the daemon's WaitGroup BEFORE spawning
	// so that Shutdown's wg.Wait() is guaranteed to wait for it.
	qh.debounceTracked = qh.shutdownWg != nil
	if qh.debounceTracked {
		qh.shutdownWg.Add(1)
	}

	// Capture for closure safety (avoid reading struct fields that may change).
	wg := qh.shutdownWg
	tracked := qh.debounceTracked

	qh.debounceTimer = time.AfterFunc(
		time.Duration(debounceSec*float64(time.Second)),
		func() {
			// Balance the wg.Add(1) done before time.AfterFunc.
			// This must be the first defer to ensure it runs even on panic.
			if tracked {
				defer wg.Done()
			}

			// Guard: if shutting down, bail out immediately to avoid
			// running PeriodicScan after the daemon has stopped.
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

			defer func() {
				if r := recover(); r != nil {
					qh.log(LogLevelError, "panic in debounceAndScan: %v", r)
				}
			}()
			qh.log(LogLevelDebug, "debounced_scan trigger=%s", trigger)

			// SRE-002: propagate shutdownCtx to PeriodicScan for cancellation support
			ctx := qh.shutdownCtx
			if ctx == nil {
				ctx = context.Background()
			}
			qh.PeriodicScanWithContext(ctx)
		},
	)
}
