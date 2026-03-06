package daemon

import (
	"context"
	"time"
)

// debounceAndScan applies debounce logic before triggering a scan.
// The debounce timer callback checks context cancellation and the advisory
// shuttingDown flag to skip scans during shutdown.
func (qh *QueueHandler) debounceAndScan(trigger string) {
	debounceSec := qh.config.Watcher.DebounceSec
	if debounceSec <= 0 {
		debounceSec = 0.5
	}

	qh.debounceMu.Lock()
	defer qh.debounceMu.Unlock()

	// Stop previous timer if any.
	if qh.debounceTimer != nil {
		qh.debounceTimer.Stop()
	}

	qh.debounceTimer = time.AfterFunc(
		time.Duration(debounceSec*float64(time.Second)),
		func() {
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

			defer func() {
				if r := recover(); r != nil {
					qh.log(LogLevelError, "panic in debounceAndScan: %v", r)
				}
			}()
			qh.log(LogLevelDebug, "debounced_scan trigger=%s", trigger)

			ctx := qh.shutdownCtx
			if ctx == nil {
				ctx = context.Background()
			}
			qh.PeriodicScanWithContext(ctx)
		},
	)
}
