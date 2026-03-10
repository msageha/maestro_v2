package daemon

import (
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/msageha/maestro_v2/internal/tmux"
)

// WatchLoop groups the fsnotify and ticker loop goroutines.
// It holds a back-pointer to Daemon for access to shared state.
type WatchLoop struct {
	d     *Daemon
	fsSem chan struct{} // bounds concurrent fsnotify handler goroutines
}

// fsnotifyLoop processes filesystem change events.
func (w *WatchLoop) fsnotifyLoop() {
	d := w.d
	defer d.recoverPanic("fsnotifyLoop")

	for {
		select {
		case <-d.egCtx.Done():
			return
		case event, ok := <-d.watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				base := filepath.Base(event.Name)
				// Skip daemon-internal temp files and backups from AtomicWrite
				if strings.HasPrefix(base, ".maestro-") || strings.HasSuffix(base, ".bak") {
					continue
				}
				// Skip self-written files: UDS handlers trigger processing via event bus
				if d.selfWrites.Consume(event.Name) {
					d.log(LogLevelDebug, "fsnotify self_write_skipped file=%s", event.Name)
					continue
				}
				d.log(LogLevelDebug, "fsnotify event=%s file=%s", event.Op, event.Name)
				// Advisory check: skip if shutting down. No mutex needed because
				// eg.Go from within this eg.Go goroutine is safe (eg.Wait cannot
				// return while this goroutine is running).
				if d.shuttingDown.Load() {
					continue
				}
				name := event.Name
				d.eg.Go(func() error {
					defer d.recoverPanic("fsnotifyHandler")
					// Bound concurrency to prevent goroutine fan-out
					// during fsnotify bursts. Drop events that exceed
					// the semaphore; periodic scan will catch up.
					select {
					case w.fsSem <- struct{}{}:
						defer func() { <-w.fsSem }()
					default:
						d.log(LogLevelDebug, "fsnotify handler dropped (semaphore full) file=%s", name)
						return nil
					}
					d.handler.HandleFileEvent(name)
					return nil
				})
			}
		case err, ok := <-d.watcher.Errors:
			if !ok {
				return
			}
			d.log(LogLevelError, "fsnotify error=%v", err)
		}
	}
}

// tickerLoop triggers periodic scans at configured intervals.
func (w *WatchLoop) tickerLoop() {
	d := w.d
	defer d.recoverPanic("tickerLoop")

	for {
		select {
		case <-d.egCtx.Done():
			return
		case <-d.ticker.C:
			d.log(LogLevelDebug, "periodic scan triggered")
			d.handler.PeriodicScanWithContext(d.egCtx)

			// Session health check: detect if tmux session disappeared
			if !tmux.SessionHealthCheck() {
				d.log(LogLevelError, "SESSION_LOST tmux session %q is no longer alive!", tmux.GetSessionName())
			}
		}
	}
}
