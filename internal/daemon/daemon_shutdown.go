package daemon

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
)

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
		// shutdownDone unblocks this goroutine when Shutdown completes,
		// preventing a leak if no second signal arrives.
		shutdownDone := make(chan struct{})
		defer close(shutdownDone)
		go func() {
			select {
			case <-sigCh:
				d.log(LogLevelWarn, "received second signal, forcing exit")
				d.forceExit.Store(true)
				d.closeExecutors()
				d.cleanup()
				os.Exit(1)
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

		totalTimeout := d.config.Daemon.ShutdownTimeoutSec
		if totalTimeout <= 0 {
			totalTimeout = 30
		}
		totalDuration := time.Duration(totalTimeout) * time.Second

		// 1. Set advisory flag — spawners will skip new work.
		d.shuttingDown.Store(true)

		// 2. Stop producers — no new work will be enqueued.
		d.ticker.Stop()
		if d.handler != nil {
			d.handler.Stop()
		}
		if d.watcher != nil {
			if err := d.watcher.Close(); err != nil {
				d.log(LogLevelError, "shutdown watcher_close error=%v", err)
			}
		}
		if d.server != nil {
			if err := d.server.Stop(); err != nil {
				d.log(LogLevelError, "shutdown server_stop error=%v", err)
			}
		}

		// Unsubscribe from event bus and stop event processing.
		d.bridge.unsubscribeAll()
		if d.eventBus != nil {
			d.eventBus.Close()
		}
		if d.qualityGateDaemon != nil {
			_ = d.qualityGateDaemon.Stop()
		}

		// 3. Cancel context — forces loops and handlers to exit.
		d.cancel()

		// 4. Wait for all errgroup goroutines with timeout.
		if d.eg != nil {
			done := make(chan struct{})
			go func() {
				_ = d.eg.Wait()
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
			}
		}

		// ── Cleanup ─────────────────────────────────────────────────

		d.closeExecutors()
		d.log(LogLevelInfo, "daemon stopped")
		d.cleanup()
	})
}

// closeExecutors closes shared executor instances to release log file handles.
// Safe to call from both graceful and force-exit paths via sync.Once.
func (d *Daemon) closeExecutors() {
	d.closeExecutorsOnce.Do(func() {
		if d.handler != nil {
			d.handler.dispatcher.CloseExecutor()
			d.handler.resultHandler.CloseExecutor()
			d.handler.cancelHandler.CloseExecutor()
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
				fmt.Fprintf(os.Stderr, "cleanup: close tmux log file: %v\n", err)
			}
		}
		if d.logFile != nil {
			if err := d.logFile.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "cleanup: close log file: %v\n", err)
			}
		}
	})
}
