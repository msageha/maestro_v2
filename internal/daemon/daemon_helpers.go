package daemon

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"time"

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

// recoverPanic catches panics in goroutines to prevent the daemon from crashing.
// It logs the panic with a full stack trace and initiates a graceful shutdown.
//
// Design: Shutdown is called via "go d.Shutdown()" (a new goroutine) intentionally.
// recoverPanic runs inside errgroup goroutines, and Shutdown calls eg.Wait() to
// drain the errgroup. Calling Shutdown synchronously here would deadlock because
// the errgroup cannot finish while this goroutine is blocked waiting for itself.
func (d *Daemon) recoverPanic(goroutine string) {
	if r := recover(); r != nil {
		d.log(LogLevelError, "panic in %s: %v\n%s", goroutine, r, debug.Stack())
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
	if err := yamlv3.Unmarshal(data, &lf); err != nil || lf.FileType != "state_learnings" {
		d.log(LogLevelWarn, "learnings_startup_corrupt, recovering")
		if recErr := yamlutil.RecoverCorruptedFile(d.maestroDir, learningsPath, "state_learnings"); recErr != nil {
			d.log(LogLevelError, "learnings_startup_recovery_failed: %v", recErr)
		}
	}
}

func (d *Daemon) log(level LogLevel, format string, args ...any) {
	if level < d.logLevel {
		return
	}
	levelStr := "[INFO]"
	switch level {
	case LogLevelDebug:
		levelStr = "[DEBUG]"
	case LogLevelWarn:
		levelStr = "[WARN]"
	case LogLevelError:
		levelStr = "[ERROR]"
	}
	msg := fmt.Sprintf(format, args...)
	d.logger.Printf("%s %s daemon: %s", d.clock.Now().Format(time.RFC3339), levelStr, msg)
}
