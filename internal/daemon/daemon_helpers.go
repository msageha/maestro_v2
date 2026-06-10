package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"runtime/debug"

	yamlv3 "gopkg.in/yaml.v3"

	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// spawnTracked admits a background goroutine to the daemon errgroup atomically
// with respect to Shutdown. Callers from outside the errgroup (e.g. UDS
// handlers) MUST use this helper rather than calling d.eg.Go directly: the
// naive check-then-Go pattern races with Shutdown's eg.Wait() and can either
// panic in sync.WaitGroup or leak the goroutine past Wait.
//
// If the daemon is shutting down (or already shut down), the goroutine is not
// started. The supplied function receives d.egCtx so it observes context
// cancellation. The fallback path (d.eg == nil, used by tests that bypass
// Run()) launches an untracked goroutine with d.ctx instead — preserving the
// previous behaviour for that path. Returns true iff a goroutine was started.
func (d *Daemon) spawnTracked(name string, fn func(context.Context)) bool {
	if d.eg == nil {
		if d.shuttingDown.Load() {
			return false
		}
		go func() {
			defer d.recoverPanic(name)
			fn(d.ctx)
		}()
		return true
	}

	d.egMu.Lock()
	defer d.egMu.Unlock()
	if d.shuttingDown.Load() {
		return false
	}
	d.eg.Go(func() error {
		defer d.recoverPanic(name)
		fn(d.egCtx)
		return nil
	})
	return true
}

// triggerResultWriteScan triggers an asynchronous queue scan after a result write.
// Wired in newDaemon during construction. Safe to call when d.handler is nil (no-op).
func (d *Daemon) triggerResultWriteScan(_ context.Context) {
	if d.handler == nil {
		return
	}
	d.spawnTracked("resultWriteScan", func(scanCtx context.Context) {
		if d.eg != nil {
			d.handler.PeriodicScanWithContext(scanCtx)
		} else {
			d.handler.PeriodicScan()
		}
	})
}

// recoverPanic catches panics in goroutines to prevent the daemon from crashing.
// It logs the panic with a full stack trace and initiates a graceful shutdown.
//
// Design: Shutdown is called via "go d.Shutdown()" (a new goroutine) intentionally.
// recoverPanic runs inside errgroup goroutines, and Shutdown calls eg.Wait() to
// drain the errgroup. Calling Shutdown synchronously here would deadlock because
// the errgroup cannot finish while this goroutine is blocked waiting for itself.
//
// shuttingDown is set immediately (before the async Shutdown goroutine) so that
// spawnTracked rejects new goroutines without waiting for Shutdown to acquire egMu.
func (d *Daemon) recoverPanic(goroutine string) {
	if r := recover(); r != nil {
		d.log(LogLevelError, "panic in %s: %v\n%s", goroutine, r, debug.Stack())
		d.shuttingDown.Store(true)
		go d.Shutdown()
	}
}

// validateLearningsFile checks the learnings file on daemon startup.
// If the file is corrupt, it uses the quarantine/recovery flow.
func (d *Daemon) validateLearningsFile() {
	learningsPath := learningsFilePath(d.maestroDir)
	data, err := os.ReadFile(learningsPath) //nolint:gosec // learningsPath is constructed from a controlled application state directory
	if os.IsNotExist(err) {
		return // No file yet — will be created on first write
	}
	if err != nil {
		d.log(LogLevelWarn, "learnings_startup_read error=%v", err)
		return
	}

	var lf struct {
		SchemaVersion int    `yaml:"schema_version"`
		FileType      string `yaml:"file_type"`
	}
	if err := yamlv3.Unmarshal(data, &lf); err != nil {
		d.log(LogLevelWarn, "learnings_startup_corrupt: yaml parse error: %v, recovering", err)
		if recErr := yamlutil.RecoverCorruptedFile(d.maestroDir, learningsPath, "state_learnings"); recErr != nil {
			d.log(LogLevelError, "learnings_startup_recovery_failed: %v", recErr)
		}
		return
	}
	if lf.FileType != "state_learnings" {
		d.log(LogLevelWarn, "learnings_startup_corrupt: unexpected file_type=%q (expected \"state_learnings\"), recovering", lf.FileType)
		if recErr := yamlutil.RecoverCorruptedFile(d.maestroDir, learningsPath, "state_learnings"); recErr != nil {
			d.log(LogLevelError, "learnings_startup_recovery_failed: %v", recErr)
		}
		return
	}
	if lf.SchemaVersion < 1 {
		d.log(LogLevelWarn, "learnings_startup_corrupt: invalid schema_version=%d (expected >= 1), recovering", lf.SchemaVersion)
		if recErr := yamlutil.RecoverCorruptedFile(d.maestroDir, learningsPath, "state_learnings"); recErr != nil {
			d.log(LogLevelError, "learnings_startup_recovery_failed: %v", recErr)
		}
	}
}

// log emits a daemon-component log line in the unified slog text format
// (time=... level=... msg=... component=daemon). Historically this printed a
// bespoke "<RFC3339> [LEVEL] daemon: <msg>" line, which left daemon.log with
// two interleaved formats — this one and the slog output of every other
// component — so mechanical scans (grep level=ERROR, log shippers) silently
// missed half the records. Every daemon-side legacy call funnels through
// this method (api_factory logFn closures, phaseC/reviewCoord logFunc), so
// bridging here unifies the whole file in one place.
func (d *Daemon) log(level LogLevel, format string, args ...any) {
	if level < d.logLevel {
		return
	}
	d.dlOnce.Do(func() {
		logger := d.logger
		if logger == nil {
			// Bare Daemon literals (tests) may omit the logger; the legacy
			// path would have panicked on Printf too, but a silent drop is
			// the safer contract for a logging helper.
			logger = log.New(io.Discard, "", 0)
		}
		d.dl = NewDaemonLoggerFromLegacy("daemon", logger, d.logLevel)
	})
	d.dl.Logf(level, format, args...)
}
