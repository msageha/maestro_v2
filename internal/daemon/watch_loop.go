package daemon

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"

	"github.com/msageha/maestro_v2/internal/tmux"
)

// WatchLoop groups the fsnotify and ticker loop goroutines.
// It holds a back-pointer to Daemon for access to shared state.
type WatchLoop struct {
	d            *Daemon
	fsEg         errgroup.Group // bounds concurrent fsnotify handler goroutines via SetLimit
	droppedCount atomic.Int64   // events dropped due to limit full
	fsDirty      atomic.Bool    // set when events are dropped; cleared after catch-up scan
}

// FsDroppedCount returns the total number of fsnotify events dropped due to
// semaphore saturation.
func (w *WatchLoop) FsDroppedCount() int64 {
	return w.droppedCount.Load()
}

// fsnotifyLoop processes filesystem change events.
func (w *WatchLoop) fsnotifyLoop() {
	d := w.d
	defer d.recoverPanic("fsnotifyLoop")
	defer w.fsEg.Wait() //nolint:errcheck // handlers always return nil

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
				// Advisory check: skip if shutting down.
				if d.shuttingDown.Load() {
					continue
				}
				name := event.Name
				// Bound concurrency via errgroup.SetLimit to prevent goroutine
				// fan-out during fsnotify bursts. TryGo returns false when the
				// limit is reached (non-blocking); periodic scan will catch up.
				if !w.fsEg.TryGo(func() error {
					defer d.recoverPanic("fsnotifyHandler")
					d.handler.HandleFileEvent(name)
					return nil
				}) {
					cnt := w.droppedCount.Add(1)
					w.fsDirty.Store(true)
					if cnt == 1 || cnt%100 == 0 {
						d.log(LogLevelWarn, "fsnotify handler dropped (limit full) file=%s total_dropped=%d", name, cnt)
					}
				}
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
			// Catch-up scan: if fsnotify events were dropped due to
			// semaphore saturation, force a scan to process missed changes.
			if w.fsDirty.CompareAndSwap(true, false) {
				d.log(LogLevelInfo, "catch_up_scan triggered (fsnotify events were dropped)")
			}
			d.log(LogLevelDebug, "periodic scan triggered")
			d.handler.PeriodicScanWithContext(d.egCtx)

			// Session health check: detect if tmux session disappeared
			w.checkSessionHealth()
		}
	}
}

// checkSessionHealth performs a detailed session health check with recovery detection.
// When the session disappears, it sets the sessionLost flag and logs diagnostics.
// When a previously lost session reappears, it clears the flag and logs recovery.
//
// Concurrency: d.sessionLost is an atomic.Bool accessed from tickerLoop (this
// goroutine) and read by dispatch paths to suppress new task dispatch while the
// session is lost. CompareAndSwap/Swap provide lock-free state transitions
// without requiring d.mu, avoiding contention on the hot dispatch path.
func (w *WatchLoop) checkSessionHealth() {
	d := w.d
	result := tmux.SessionHealthCheckDetailed()

	if result.Alive {
		// Session is alive — check if we're recovering from a lost state
		if d.sessionLost.CompareAndSwap(true, false) {
			d.log(LogLevelInfo, "SESSION_RECOVERED tmux session %q is alive again windows=[%s]",
				tmux.GetSessionName(), result.WindowInfo)
		}
		return
	}

	// Session is dead — set the flag and log diagnostics
	wasLost := d.sessionLost.Swap(true)

	if !wasLost {
		// First detection of session loss — log full diagnostics
		d.log(LogLevelError, "SESSION_LOST tmux session %q is no longer alive!", tmux.GetSessionName())
		d.log(LogLevelError, "SESSION_LOST_DIAG has_session_stderr=%q server_running=%v",
			result.Stderr, result.ServerRunning)

		if result.ServerRunning {
			d.log(LogLevelError, "SESSION_LOST_DIAG other_sessions=%q server_options=%q",
				result.OtherSessions, result.ServerOptions)
		}

		// Log system memory info for post-mortem analysis
		if memInfo := getMemoryInfo(); memInfo != "" {
			d.log(LogLevelError, "SESSION_LOST_DIAG system_memory=%s", memInfo)
		}

		d.log(LogLevelWarn, "SESSION_LOST dispatch paused — new task/command dispatch is suspended until session recovers")
	} else {
		// Subsequent detections — log at debug level to avoid log spam
		d.log(LogLevelDebug, "SESSION_LOST (still) session=%q server_running=%v", tmux.GetSessionName(), result.ServerRunning)
	}
}

// getMemoryInfo returns a summary of system memory usage for diagnostics.
// Returns empty string if the information cannot be obtained.
func getMemoryInfo() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	goInfo := fmt.Sprintf("go_alloc=%dMB go_sys=%dMB go_goroutines=%d",
		m.Alloc/1024/1024, m.Sys/1024/1024, runtime.NumGoroutine())

	// Attempt to get system-level memory info (macOS: vm_stat)
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("vm_stat").Output() //nolint:gosec // "vm_stat" is a fixed system command
		if err == nil {
			lines := strings.Split(string(out), "\n")
			if len(lines) > 1 {
				// Extract just the first few lines (free/active/inactive)
				var summary []string
				for _, line := range lines[1:4] {
					line = strings.TrimSpace(line)
					if line != "" {
						summary = append(summary, line)
					}
				}
				if len(summary) > 0 {
					return goInfo + " | " + strings.Join(summary, "; ")
				}
			}
		}
	}

	return goInfo
}
