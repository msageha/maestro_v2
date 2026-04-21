package daemon

import (
	"context"
	"path/filepath"
)

// Stop cancels any pending debounce timer and waits for any in-flight
// callback goroutine to finish, ensuring no goroutine leak on shutdown.
func (qh *QueueHandler) Stop() {
	qh.scanExecutor.Stop()
}

// HandleFileEvent routes an fsnotify event to the appropriate handler.
func (qh *QueueHandler) HandleFileEvent(filePath string) {
	base := filepath.Base(filePath)
	dir := filepath.Base(filepath.Dir(filePath))

	switch dir {
	case "queue":
		qh.scanExecutor.debounce.Trigger(base)
	case "results":
		qh.log(LogLevelDebug, "result_event file=%s", base)
		if qh.resultHandler != nil {
			qh.resultHandler.HandleResultFileEvent(filePath)
		}
	}
}

// PeriodicScan executes all scan steps in a three-phase pattern to avoid
// holding scanMu during slow tmux I/O operations.
// This is the backward-compatible wrapper; callers with a context should use
// PeriodicScanWithContext.
//
// When the daemon's shutdown context has been wired via SetShutdownGuard,
// it is used so a scan in progress cancels promptly on shutdown; otherwise
// a fresh Background context is used (tests exercise this path).
func (qh *QueueHandler) PeriodicScan() {
	ctx := qh.shutdownCtx
	if ctx == nil {
		ctx = context.Background()
	}
	qh.PeriodicScanWithContext(ctx)
}

// PeriodicScanWithContext delegates to ScanPhaseExecutor for the three-phase scan cycle.
func (qh *QueueHandler) PeriodicScanWithContext(ctx context.Context) {
	qh.scanExecutor.Execute(ctx)
}

// periodicScanPhaseA delegates to ScanPhaseExecutor for test access.
func (qh *QueueHandler) periodicScanPhaseA() phaseAResult {
	return qh.scanExecutor.periodicScanPhaseA()
}

// periodicScanPhaseB delegates to ScanPhaseExecutor for test access.
func (qh *QueueHandler) periodicScanPhaseB(ctx context.Context, pa phaseAResult) phaseBResult {
	return qh.scanExecutor.periodicScanPhaseB(ctx, pa)
}

// periodicScanPhaseC delegates to ScanPhaseExecutor for test access.
func (qh *QueueHandler) periodicScanPhaseC(pa phaseAResult, pb phaseBResult) []DeferredNotification {
	return qh.scanExecutor.periodicScanPhaseC(pa, pb)
}

// LockFiles acquires a shared (read) lock for queue write handlers.
// Multiple queue writes can proceed in parallel; PeriodicScan holds exclusive lock.
func (qh *QueueHandler) LockFiles() {
	qh.scanExecutor.LockFiles()
}

// UnlockFiles releases the shared (read) lock for queue write handlers.
func (qh *QueueHandler) UnlockFiles() {
	qh.scanExecutor.UnlockFiles()
}

func (qh *QueueHandler) log(level LogLevel, format string, args ...any) {
	qh.dl.Logf(level, format, args...)
}
