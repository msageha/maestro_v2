package daemon

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
)

// shutdownOpTimeout is the maximum time allowed for individual shutdown
// operations (e.g., watcher close, server stop). If an operation exceeds
// this timeout, a warning is logged and shutdown proceeds to the next step.
const shutdownOpTimeout = 10 * time.Second

// shutdownGracePeriod is the extra time allowed after the main shutdown timeout
// for defers and in-flight cleanup to complete before the process is forcibly
// terminated via os.Exit.
const shutdownGracePeriod = 5 * time.Second

// waitSignals blocks until a shutdown signal or context cancellation is received.
func (d *Daemon) waitSignals() {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	d.log(LogLevelInfo, "waitSignals: listening for SIGTERM/SIGINT")

	select {
	case sig := <-sigCh:
		d.log(LogLevelInfo, "received signal=%s, initiating graceful shutdown (session_alive=%v)", sig, tmux.SessionExists())

		// Second signal → force exit.
		// This goroutine is not tracked by errgroup and has no explicit join.
		// This is intentional: it only exists to handle a second signal during
		// shutdown, and shutdownDone is closed when Shutdown completes, causing
		// this goroutine to return. On process exit, the Go runtime reclaims it.
		shutdownDone := make(chan struct{})
		var closeShutdownDone sync.Once
		defer closeShutdownDone.Do(func() { close(shutdownDone) })
		go func() {
			select {
			case <-sigCh:
				d.log(LogLevelWarn, "received second signal, forcing exit")
				d.forceExit.Store(true)
				_ = d.closeWatcher() // forced-exit path: log warn already emitted by closeWatcher on error
				d.closeExecutors()
				d.cleanup()
				d.doExit(1)
			case <-shutdownDone:
				return
			}
		}()

		d.Shutdown()
	case <-d.ctx.Done():
		d.log(LogLevelInfo, "context cancelled, waiting for shutdown to complete")
		d.Shutdown()
	}
}

// Shutdown performs graceful shutdown (idempotent via sync.Once).
//
// 1. Set shuttingDown flag to reject new work.
// 2. Stop producers (ticker, watcher, server, events).
// 3. Cancel context to force all goroutines to exit.
// 4. Wait for errgroup (all goroutines) with timeout.
// 5. Dump goroutine stacks on timeout for debugging.
func (d *Daemon) Shutdown() {
	d.shutdown.Do(func() {
		d.log(LogLevelInfo, "shutdown started session_alive=%v", tmux.SessionExists())

		totalTimeout := d.config.ShutdownTimeoutSec
		if totalTimeout <= 0 {
			totalTimeout = model.DefaultShutdownTimeoutSec
		}
		totalDuration := time.Duration(totalTimeout) * time.Second
		shutdownStart := time.Now()

		// 1. Set advisory flag — spawners will skip new work.
		// Hold egMu across the flag flip so any spawnTracked caller currently
		// inside its critical section finishes (its eg.Go has completed and
		// the WaitGroup counter is incremented before we proceed to eg.Wait).
		// Subsequent spawnTracked calls will observe shuttingDown=true and
		// skip. Release the lock before eg.Wait so tracked goroutines that
		// internally spawn children via spawnTracked do not deadlock.
		d.egMu.Lock()
		d.shuttingDown.Store(true)
		d.egMu.Unlock()

		// 2. Stop producers — no new work will be enqueued.
		// Each operation has an individual timeout (shutdownOpTimeout) to prevent
		// a single hung component from consuming the entire global timeout budget.
		d.ticker.Stop()
		if d.handler != nil {
			d.handler.Stop()
		}
		if d.watcher != nil {
			d.shutdownOp("watcher_close", func() error { return d.closeWatcher() })
		}
		if d.server != nil {
			d.shutdownOp("server_stop", func() error { return d.server.Stop() })
		}

		// Unsubscribe from event bus and stop event processing.
		d.bridge.unsubscribeAll()
		if d.eventBus != nil {
			d.shutdownOp("event_bus_close", func() error { return d.eventBus.Close() })
		}
		if d.traceWriter != nil {
			d.shutdownOp("trace_writer_close", func() error { return d.traceWriter.Close() })
		}
		if d.qualityGateDaemon != nil {
			d.shutdownOp("quality_gate_stop", func() error { return d.qualityGateDaemon.Stop() })
		}

		// Log warning for active rollout groups that will be abandoned.
		if d.rolloutManager != nil {
			activeCount := d.rolloutManager.ActiveGroupCount()
			if activeCount > 0 {
				d.log(LogLevelWarn, "shutdown abandoning %d active rollout groups", activeCount)
			}
		}

		// Close review coordinator: waits for in-flight reviews, then closes
		// the results channel so MonitorResults exits cleanly.
		d.reviewCoord.Close()

		// Phase C cleanup: log stats for stateful components.
		d.phaseC.LogShutdownStats(d.log)

		// 3. Cancel context — forces loops and handlers to exit.
		d.cancel()

		// 4. Wait for all errgroup goroutines with timeout.
		if d.eg != nil {
			done := make(chan struct{})
			go func() {
				if err := d.eg.Wait(); err != nil {
					d.log(LogLevelWarn, "shutdown errgroup returned error: %v", err)
				}
				close(done)
			}()

			select {
			case <-done:
				d.log(LogLevelInfo, "shutdown all_goroutines_drained")
			case <-time.After(totalDuration):
				buf := make([]byte, 256*1024)
				n := runtime.Stack(buf, true)
				d.log(LogLevelWarn, "shutdown timeout after %ds, dumping %d bytes of goroutine stacks:\n%s",
					totalTimeout, n, string(buf[:n]))
				d.log(LogLevelWarn, "WARNING: shutdown timed out, entering grace period for cleanup")
				// Schedule hard exit as safety net. This goroutine is intentionally
				// detached — it guarantees process termination if cleanup hangs.
				go func() {
					time.Sleep(shutdownGracePeriod)
					d.doExit(1)
				}()
				// Fall through to cleanup — defers in callers will execute during
				// the grace period before the hard exit fires.
			}
		}

		// 5. Cleanup worktrees to prevent accumulation across restarts.
		// Uses remaining shutdown budget as timeout so the total shutdown
		// stays within shutdown_timeout_sec. Failures are logged but do not
		// block shutdown.
		if d.worktreeManager != nil {
			remaining := totalDuration - time.Since(shutdownStart)
			if remaining > time.Second {
				d.log(LogLevelInfo, "shutdown worktree_cleanup_start budget=%s", remaining.Truncate(time.Millisecond))
				wtCtx, wtCancel := context.WithTimeout(context.Background(), remaining)
				if err := d.worktreeManager.CleanupAll(wtCtx); err != nil {
					d.log(LogLevelWarn, "shutdown worktree_cleanup error=%v", err)
				}
				wtCancel()
			} else {
				d.log(LogLevelWarn, "shutdown worktree_cleanup_skipped no_time_remaining")
			}
		}

		// ── Cleanup ─────────────────────────────────────────────────

		d.closeExecutors()
		d.log(LogLevelInfo, "daemon stopped")
		d.cleanup()
	})
}

// shutdownOp runs fn with shutdownOpTimeout. If fn does not complete in time,
// a warning is logged and control returns so the global shutdown timer can proceed.
func (d *Daemon) shutdownOp(name string, fn func() error) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownOpTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()

	select {
	case err := <-done:
		if err != nil {
			d.log(LogLevelError, "shutdown %s error=%v", name, err)
		}
	case <-ctx.Done():
		d.log(LogLevelWarn, "shutdown %s timed out after %s", name, shutdownOpTimeout)
		// Drain in background so the fn goroutine can complete and be GC'd
		// instead of leaking due to a blocked channel send.
		go func() { <-done }()
	}
}

// doExit terminates the process with the given exit code.
// Uses d.exitFn if set (for testing), otherwise falls back to os.Exit.
func (d *Daemon) doExit(code int) {
	fn := d.exitFn
	if fn == nil {
		fn = os.Exit
	}
	fn(code)
}

// closeWatcher closes the fsnotify watcher exactly once. Safe to call from
// both the graceful shutdown path and the force-exit signal handler.
func (d *Daemon) closeWatcher() error {
	var err error
	d.watcherCloseOnce.Do(func() {
		if d.watcher != nil {
			err = d.watcher.Close()
		}
	})
	return err
}

// closeExecutors closes the shared executor instance to release log file handles.
// Safe to call from both graceful and force-exit paths via sync.Once.
func (d *Daemon) closeExecutors() {
	d.closeExecutorsOnce.Do(func() {
		if d.handler != nil {
			d.handler.execProvider.CloseExecutor()
		}
	})
}

// cleanup releases resources. Safe to call multiple times via cleanupOnce.
func (d *Daemon) cleanup() {
	d.cleanupOnce.Do(func() {
		socketPath := filepath.Join(d.maestroDir, uds.DefaultSocketName)
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			d.log(LogLevelError, "cleanup remove_socket error=%v", err)
		}
		// Remove PID file while lock is still held so no concurrent starter
		// reads a stale PID between lock release and PID file removal.
		if err := os.Remove(filepath.Join(d.maestroDir, "daemon.pid")); err != nil && !os.IsNotExist(err) {
			d.log(LogLevelError, "cleanup remove_pid error=%v", err)
		}
		if err := d.fileLock.Unlock(); err != nil {
			d.log(LogLevelError, "cleanup file_unlock error=%v", err)
		}
		// Disable tmux debug logger before closing the file
		tmux.SetDebugLogger(nil)
		if d.tmuxLogFile != nil {
			if err := d.tmuxLogFile.Close(); err != nil {
				d.log(LogLevelError, "cleanup close_tmux_log error=%v", err)
			}
		}
		if d.logFile != nil {
			if err := d.logFile.Close(); err != nil {
				d.log(LogLevelError, "cleanup close_log error=%v", err)
			}
		}
	})
}
