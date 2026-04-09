package daemon

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
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
				if d.watcher != nil {
					if err := d.watcher.Close(); err != nil {
						log.Printf("DEBUG: failed to close watcher during force exit: %v", err)
					}
				}
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

		totalTimeout := d.config.ShutdownTimeoutSec
		if totalTimeout <= 0 {
			totalTimeout = 30
		}
		totalDuration := time.Duration(totalTimeout) * time.Second

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
			if err := d.qualityGateDaemon.Stop(); err != nil {
				log.Printf("DEBUG: failed to stop quality gate daemon during shutdown: %v", err)
			}
		}

		// Close review dispatcher: waits for in-flight reviews, then closes
		// the results channel so monitorReviewResults exits cleanly.
		if d.reviewDispatcher != nil {
			d.reviewDispatcher.Close()
		}

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
				d.log(LogLevelWarn, "WARNING: shutdown timed out, forcing exit")
				d.closeExecutors()
				d.cleanup()
				os.Exit(1)
			}
		}

		// ── Cleanup ─────────────────────────────────────────────────

		d.closeExecutors()
		d.log(LogLevelInfo, "daemon stopped")
		d.cleanup()
	})
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
