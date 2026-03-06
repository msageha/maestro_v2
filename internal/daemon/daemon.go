// Package daemon implements the maestro background daemon for queue processing and orchestration.
package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// LogLevel, Clock, DaemonLogger, StateReader, etc. are defined in
// internal/daemon/core and re-exported via core_aliases.go.

// Daemon is the main maestro daemon process.
type Daemon struct {
	maestroDir string
	config     model.Config
	logLevel   LogLevel
	logger     *log.Logger
	logFile    io.Closer
	clock      Clock

	fileLock *lock.FileLock
	server   *uds.Server
	watcher  *fsnotify.Watcher
	ticker   *time.Ticker

	handler           *QueueHandler
	stateReader       StateReader
	canComplete       CanCompleteFunc
	planExecutor      PlanExecutor
	lockMap           *lock.MutexMap
	qualityGateDaemon *QualityGateDaemon
	circuitBreaker    *circuitbreaker.Handler
	worktreeManager   *WorktreeManager

	eventBus          *events.Bus
	eventUnsubscribers []func()
	tmuxLogFile        io.Closer // debug log for tmux operations

	dashboardMu sync.Mutex   // serializes concurrent dashboard generation
	fsSem       chan struct{} // bounds concurrent fsnotify handler goroutines

	ctx      context.Context
	cancel   context.CancelFunc
	eg       *errgroup.Group // tracks all daemon goroutines (loops + handlers)
	shutdown sync.Once

	// shuttingDown is an advisory flag read by spawners for fast-path rejection.
	// With errgroup, the TOCTOU guard (shutdownMu) is no longer needed because
	// eg.Go() called from within an eg.Go goroutine (e.g. fsnotifyLoop) is safe:
	// eg.Wait() cannot return while the calling goroutine is still running.
	shuttingDown atomic.Bool

	selfWrites *selfWriteTracker // tracks daemon-originated YAML writes for fsnotify filtering

	cleanupOnce sync.Once
	forceExit   atomic.Bool
}

// selfWriteTracker tracks files written by the daemon to filter fsnotify self-notifications.
// Uses content hashing (SHA-256) instead of TTL to reliably detect self-written files.
// When the daemon writes a YAML file via UDS handler, it records the content hash here.
// fsnotifyLoop checks this tracker by reading the file and comparing hashes.
type selfWriteTracker struct {
	mu     sync.Mutex
	stamps map[string]writeStamp
}

// writeStamp stores a content hash and a deadline for stale-entry cleanup.
type writeStamp struct {
	Hash     [sha256.Size]byte
	Deadline time.Time
}

func newSelfWriteTracker() *selfWriteTracker {
	return &selfWriteTracker{stamps: make(map[string]writeStamp)}
}

// Record stores a content hash for a self-written file.
// The data is marshaled to YAML to compute the hash (matching AtomicWrite's output).
func (t *selfWriteTracker) Record(path string, data any) {
	content, err := yamlv3.Marshal(data)
	if err != nil {
		return // best-effort
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stamps[path] = writeStamp{
		Hash:     sha256.Sum256(content),
		Deadline: time.Now().Add(30 * time.Second),
	}
	t.cleanStaleLocked()
}

// Consume checks if a file's current content matches a recorded self-write hash.
// If so, removes it from tracking and returns true.
// The file is read from disk and its hash is compared with the stored hash.
func (t *selfWriteTracker) Consume(path string) bool {
	t.mu.Lock()
	stamp, ok := t.stamps[path]
	if !ok {
		t.cleanStaleLocked()
		t.mu.Unlock()
		return false
	}
	delete(t.stamps, path)
	t.cleanStaleLocked()
	t.mu.Unlock()

	// Deadline check: discard stale stamps
	if time.Now().After(stamp.Deadline) {
		return false
	}

	// Read file and compare hash
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return sha256.Sum256(content) == stamp.Hash
}

// Len returns the number of tracked paths (for testing).
func (t *selfWriteTracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.stamps)
}

// cleanStaleLocked removes entries past their deadline. Must be called with mu held.
func (t *selfWriteTracker) cleanStaleLocked() {
	now := time.Now()
	for p, s := range t.stamps {
		if now.After(s.Deadline) {
			delete(t.stamps, p)
		}
	}
}

// SetStateReader sets the state reader for dependency resolution (Phase 6).
// Must be called before Run().
func (d *Daemon) SetStateReader(reader StateReader) {
	d.stateReader = reader
}

// SetCanComplete wires the plan.CanComplete function for R4 reconciliation.
// Must be called before Run() to avoid import cycles (daemon→plan→daemon).
func (d *Daemon) SetCanComplete(f CanCompleteFunc) {
	d.canComplete = f
}

// LockMap returns the daemon's shared MutexMap for coordinating state locks.
func (d *Daemon) LockMap() *lock.MutexMap {
	return d.lockMap
}

// New creates a new Daemon instance.
func New(maestroDir string, cfg model.Config) (*Daemon, error) {
	logPath := filepath.Join(maestroDir, "logs", "daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open daemon log: %w", err)
	}

	return newDaemon(maestroDir, cfg, logFile, logFile)
}

// newDaemon is the internal constructor for testing.
func newDaemon(maestroDir string, cfg model.Config, w io.Writer, closer io.Closer) (*Daemon, error) {
	ctx, cancel := context.WithCancel(context.Background())

	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	server := uds.NewServer(socketPath)

	scanInterval := cfg.Watcher.ScanIntervalSec
	if scanInterval <= 0 {
		scanInterval = 10
	}

	d := &Daemon{
		maestroDir: maestroDir,
		config:     cfg,
		logLevel:   parseLogLevel(cfg.Logging.Level),
		logger:     log.New(w, "", 0),
		logFile:    closer,
		clock:      RealClock{},
		fileLock:   lock.NewFileLock(filepath.Join(maestroDir, "locks", "daemon.lock")),
		server:     server,
		ticker:     time.NewTicker(time.Duration(scanInterval) * time.Second),
		lockMap:    lock.NewMutexMap(),
		fsSem:      make(chan struct{}, 8),
		selfWrites: newSelfWriteTracker(),
		ctx:        ctx,
		cancel:     cancel,
	}

	return d, nil
}

// Run starts the daemon and blocks until shutdown completes.
func (d *Daemon) Run() error {
	// Ignore SIGHUP so the daemon survives terminal closure.
	// Setsid in startDaemon detaches the process group, but an explicit
	// ignore provides defense-in-depth.
	signal.Ignore(syscall.SIGHUP)

	// Step 1: Acquire file lock
	if err := d.fileLock.TryLock(); err != nil {
		return fmt.Errorf("daemon lock: %w", err)
	}
	d.log(LogLevelInfo, "daemon starting pid=%d", os.Getpid())

	// Initialize tmux debug logger for session lifecycle diagnostics
	tmuxLogPath := filepath.Join(d.maestroDir, "logs", "tmux_debug.log")
	if tmuxLogFile, err := os.OpenFile(tmuxLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		tmuxLogger := log.New(tmuxLogFile, "", log.LstdFlags|log.Lmicroseconds)
		tmux.SetDebugLogger(tmuxLogger)
		d.log(LogLevelInfo, "tmux debug logger initialized at %s", tmuxLogPath)
		// Store for cleanup
		d.tmuxLogFile = tmuxLogFile
	} else {
		d.log(LogLevelWarn, "failed to open tmux debug log: %v", err)
	}

	// Write PID file for reliable lifecycle management
	pidPath := filepath.Join(d.maestroDir, "daemon.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		if unlockErr := d.fileLock.Unlock(); unlockErr != nil {
			d.log(LogLevelError, "startup file_unlock error=%v", unlockErr)
		}
		return fmt.Errorf("write pid file: %w", err)
	}

	// Step 2: Init fsnotify watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		d.cleanup() // removes PID file + socket + unlocks
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	d.watcher = watcher

	// From this point on, use Shutdown() for cleanup on startup failure.
	// Shutdown() is idempotent (sync.Once) and handles watcher, EventBus,
	// goroutines, server, and cleanup() in the correct order.
	var runOK bool
	defer func() {
		if !runOK {
			d.Shutdown()
		}
	}()

	// Validate learnings file at startup
	if d.config.Learnings.Enabled {
		d.validateLearningsFile()
	}

	// Watch queue/ and results/ directories
	queueDir := filepath.Join(d.maestroDir, "queue")
	resultsDir := filepath.Join(d.maestroDir, "results")
	for _, dir := range []string{queueDir, resultsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("ensure dir %s: %w", dir, err)
		}
		if err := watcher.Add(dir); err != nil {
			return fmt.Errorf("watch %s: %w", dir, err)
		}
	}

	// Create errgroup derived from daemon context.
	// All daemon goroutines (loops + handlers) are tracked by this group.
	// Cancelling d.cancel() cascades to the errgroup context.
	d.eg, d.ctx = errgroup.WithContext(d.ctx)

	// Step 3: Init queue handler
	d.handler = NewQueueHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)

	// Wire shutdown guard so debounce callbacks respect shutdown
	d.handler.SetShutdownGuard(d.ctx, &d.shuttingDown)

	// Step 3.5: Wire state reader for dependency resolution (Phase 6)
	if d.stateReader != nil {
		d.handler.SetStateReader(d.stateReader)
	}

	// Step 3.6: Wire CanComplete for R4 reconciliation
	if d.canComplete != nil {
		d.handler.SetCanComplete(d.canComplete)
	}

	// Step 3.7: Wire continuous handler for iteration tracking
	ch := NewContinuousHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)
	d.handler.resultHandler.SetContinuousHandler(ch)

	// Step 3.8: Initialize QualityGateDaemon with daemon context
	d.qualityGateDaemon = NewQualityGateDaemon(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel, d.ctx)

	// Step 3.8.1: Initialize CircuitBreakerHandler
	d.circuitBreaker = circuitbreaker.NewHandler(d.config, d.logger, d.logLevel)
	if d.stateReader != nil {
		d.circuitBreaker.SetStateReader(d.stateReader)
	}
	d.handler.SetCircuitBreaker(d.circuitBreaker)

	// Step 3.8.2: Initialize WorktreeManager (opt-in)
	if d.config.Worktree.Enabled {
		d.worktreeManager = NewWorktreeManager(d.maestroDir, d.config.Worktree, d.logger, d.logLevel)
		d.handler.SetWorktreeManager(d.worktreeManager)
		d.log(LogLevelInfo, "worktree isolation enabled base_branch=%s", d.config.Worktree.EffectiveBaseBranch())

		// H2: Reconcile state/worktree inconsistencies on startup
		d.worktreeManager.Reconcile()
	}

	// Step 3.9: Initialize EventBus and wire it to components
	d.eventBus = events.NewBus(100)
	d.handler.dispatcher.SetEventBus(d.eventBus)
	d.handler.dispatcher.SetQualityGate(d.qualityGateDaemon)
	d.handler.dependencyResolver.SetEventBus(d.eventBus)
	d.handler.resultHandler.SetEventBus(d.eventBus)

	// Step 3.10: Subscribe QualityGateDaemon to events
	d.subscribeQualityGateEvents()

	// Step 3.11: Subscribe to queue write events for direct scan triggering
	// (bypasses fsnotify self-notification for daemon-originated writes)
	d.subscribeQueueWrittenEvents()

	// Step 4: Register UDS handlers
	d.registerHandlers()

	// Step 5: Start UDS server
	if err := d.server.Start(); err != nil {
		return fmt.Errorf("start UDS server: %w", err)
	}
	d.log(LogLevelInfo, "UDS server listening on %s", filepath.Join(d.maestroDir, uds.DefaultSocketName))

	// Step 6: Start background loops via errgroup
	d.eg.Go(func() error { d.fsnotifyLoop(); return nil })
	d.eg.Go(func() error { d.tickerLoop(); return nil })

	// Step 6.5: Start QualityGateDaemon
	if err := d.qualityGateDaemon.Start(); err != nil {
		return fmt.Errorf("start quality gate daemon: %w", err)
	}

	// Step 7: Run initial scan
	d.handler.PeriodicScanWithContext(d.ctx)
	d.log(LogLevelInfo, "daemon ready")

	// Startup succeeded — disable the deferred Shutdown guard.
	// From here, Shutdown will be called via waitSignals().
	runOK = true

	// Step 8: Wait for signals
	d.waitSignals()

	return nil
}

// registerHandlers registers UDS request handlers.
func (d *Daemon) registerHandlers() {
	d.server.Handle("ping", func(req *uds.Request) *uds.Response {
		return uds.SuccessResponse(map[string]string{"status": "ok"})
	})

	d.server.Handle("scan", func(req *uds.Request) *uds.Response {
		if d.handler == nil {
			return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
		}
		d.handler.PeriodicScanWithContext(d.ctx)
		return uds.SuccessResponse(map[string]string{"status": "scanned"})
	})

	d.server.Handle("shutdown", func(req *uds.Request) *uds.Response {
		d.log(LogLevelInfo, "shutdown requested via UDS")
		go func() { defer d.recoverPanic("shutdownHandler"); d.Shutdown() }()
		return uds.SuccessResponse(map[string]string{"status": "shutdown_accepted"})
	})

	d.server.Handle("queue_write", d.handleQueueWrite)
	d.server.Handle("result_write", d.handleResultWrite)
	d.server.Handle("task_heartbeat", d.handleTaskHeartbeat)
	d.server.Handle("plan", d.handlePlan)
	d.server.Handle("dashboard", d.handleDashboard)
}

// handleTaskHeartbeat handles task heartbeat requests.
func (d *Daemon) handleTaskHeartbeat(req *uds.Request) *uds.Response {
	if d.handler == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
	}
	heartbeatHandler := NewTaskHeartbeatHandler(
		d.maestroDir,
		d.config,
		d.handler.leaseManager,
		d.logger,
		d.logLevel,
		&d.handler.scanMu,
		d.lockMap,
	)
	return heartbeatHandler.Handle(req.Params)
}

// handleDashboard triggers dashboard regeneration and returns the result.
// DashboardFormatter reads state from on-disk YAML files (not in-memory scan
// state), so it does not require scanMu. Holding scanMu here would block
// PeriodicScan for the duration of the file I/O. A dedicated dashboardMu
// serializes concurrent dashboard writes to prevent temp-file clobbering.
func (d *Daemon) handleDashboard(req *uds.Request) *uds.Response {
	if d.handler == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
	}

	d.dashboardMu.Lock()
	defer d.dashboardMu.Unlock()

	// Use the new dashboard formatter for human-readable output
	formatter := NewDashboardFormatter(d.maestroDir)
	if err := formatter.UpdateDashboardFile(); err != nil {
		d.log(LogLevelError, "dashboard regeneration error=%v", err)
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("dashboard generation failed: %v", err))
	}

	dashboardPath := filepath.Join(d.maestroDir, "dashboard.md")
	return uds.SuccessResponse(map[string]string{
		"status": "regenerated",
		"path":   dashboardPath,
	})
}

// fsnotifyLoop processes filesystem change events.
func (d *Daemon) fsnotifyLoop() {
	defer d.recoverPanic("fsnotifyLoop")

	for {
		select {
		case <-d.ctx.Done():
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
					case d.fsSem <- struct{}{}:
						defer func() { <-d.fsSem }()
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
func (d *Daemon) tickerLoop() {
	defer d.recoverPanic("tickerLoop")

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-d.ticker.C:
			d.log(LogLevelDebug, "periodic scan triggered")
			d.handler.PeriodicScanWithContext(d.ctx)

			// Session health check: detect if tmux session disappeared
			if !tmux.SessionHealthCheck() {
				d.log(LogLevelError, "SESSION_LOST tmux session %q is no longer alive!", tmux.GetSessionName())
			}
		}
	}
}

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
		go func() {
			select {
			case <-sigCh:
				d.log(LogLevelWarn, "received second signal, forcing exit")
				d.forceExit.Store(true)
				d.cleanup()
				os.Exit(1)
			case <-shutdownDone:
				return
			}
		}()

		d.Shutdown()
		close(shutdownDone)
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
		for _, unsub := range d.eventUnsubscribers {
			if unsub != nil {
				unsub()
			}
		}
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

		// Close shared executor instances to release log file handles.
		if d.handler != nil {
			d.handler.dispatcher.CloseExecutor()
			d.handler.resultHandler.CloseExecutor()
			d.handler.cancelHandler.CloseExecutor()
		}

		d.log(LogLevelInfo, "daemon stopped")
		d.cleanup()
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

// recoverPanic catches panics in goroutines to prevent the daemon from crashing.
// It logs the panic with a full stack trace and initiates a graceful shutdown.
func (d *Daemon) recoverPanic(goroutine string) {
	if r := recover(); r != nil {
		d.log(LogLevelError, "panic in %s: %v\n%s", goroutine, r, debug.Stack())
		go d.Shutdown()
	}
}

// subscribeQualityGateEvents subscribes the QualityGateDaemon to EventBus events.
// It bridges the generic event bus to the quality gate daemon's typed event channel.
// Uses safe type assertions with logging for dropped events.
func (d *Daemon) subscribeQualityGateEvents() {
	// Subscribe to task started events
	unsub1 := d.eventBus.Subscribe(events.EventTaskStarted, func(e events.Event) {
		taskID, ok1 := e.Data["task_id"].(string)
		commandID, ok2 := e.Data["command_id"].(string)
		workerID, ok3 := e.Data["worker_id"].(string)

		if !ok1 || !ok2 || !ok3 {
			d.log(LogLevelWarn, "quality_gate_event_invalid type=task_started data=%v", e.Data)
			return
		}

		d.qualityGateDaemon.EmitEvent(TaskStartEvent{
			TaskID:    taskID,
			CommandID: commandID,
			AgentID:   workerID,
			StartedAt: e.Timestamp,
		})
	})

	// Subscribe to task completed events
	unsub2 := d.eventBus.Subscribe(events.EventTaskCompleted, func(e events.Event) {
		taskID, ok1 := e.Data["task_id"].(string)
		commandID, ok2 := e.Data["command_id"].(string)
		workerID, ok3 := e.Data["worker_id"].(string)

		if !ok1 || !ok2 || !ok3 {
			d.log(LogLevelWarn, "quality_gate_event_invalid type=task_completed data=%v", e.Data)
			return
		}

		// Status can be either model.Status or string
		var status model.Status
		if s, ok := e.Data["status"].(model.Status); ok {
			status = s
		} else if s, ok := e.Data["status"].(string); ok {
			status = model.Status(s)
		} else {
			d.log(LogLevelWarn, "quality_gate_event_invalid type=task_completed status=%v", e.Data["status"])
			return
		}

		d.qualityGateDaemon.EmitEvent(TaskCompleteEvent{
			TaskID:      taskID,
			CommandID:   commandID,
			AgentID:     workerID,
			Status:      status,
			CompletedAt: e.Timestamp,
		})
	})

	// Subscribe to phase transition events
	unsub3 := d.eventBus.Subscribe(events.EventPhaseTransition, func(e events.Event) {
		phaseID, ok1 := e.Data["phase_id"].(string)
		commandID, ok2 := e.Data["command_id"].(string)
		oldStatus, ok3 := e.Data["old_status"].(string)
		newStatus, ok4 := e.Data["new_status"].(string)

		if !ok1 || !ok2 || !ok3 || !ok4 {
			d.log(LogLevelWarn, "quality_gate_event_invalid type=phase_transition data=%v", e.Data)
			return
		}

		d.qualityGateDaemon.EmitEvent(PhaseTransitionEvent{
			PhaseID:        phaseID,
			CommandID:      commandID,
			OldStatus:      model.PhaseStatus(oldStatus),
			NewStatus:      model.PhaseStatus(newStatus),
			TransitionedAt: e.Timestamp,
		})
	})

	// Store unsubscribe functions for cleanup
	d.eventUnsubscribers = []func(){unsub1, unsub2, unsub3}
}

// subscribeQueueWrittenEvents subscribes to EventQueueWritten to trigger scan
// directly via the event bus, bypassing fsnotify for daemon-originated writes.
func (d *Daemon) subscribeQueueWrittenEvents() {
	unsub := d.eventBus.Subscribe(events.EventQueueWritten, func(e events.Event) {
		if d.handler == nil || d.shuttingDown.Load() {
			return
		}
		file, _ := e.Data["file"].(string)
		d.handler.debounceAndScan("event_bus:" + file)
	})
	d.eventUnsubscribers = append(d.eventUnsubscribers, unsub)
}

// notifySelfWrite records a self-write for fsnotify filtering and publishes
// an EventQueueWritten event to trigger processing via the event bus.
// data is the object that was written (used to compute content hash).
func (d *Daemon) notifySelfWrite(queuePath, writeType string, data any) {
	d.selfWrites.Record(queuePath, data)
	if d.eventBus != nil {
		d.eventBus.Publish(events.EventQueueWritten, map[string]interface{}{
			"file":   filepath.Base(queuePath),
			"source": "uds",
			"type":   writeType,
		})
	}
}

// recordSelfWrite records a self-write for fsnotify filtering without
// publishing an event (used when the caller already triggers processing directly).
// data is the object that was written (used to compute content hash).
func (d *Daemon) recordSelfWrite(path string, data any) {
	d.selfWrites.Record(path, data)
}

// validateLearningsFile checks the learnings file on daemon startup.
// If the file is corrupt, it uses the quarantine/recovery flow.
func (d *Daemon) validateLearningsFile() {
	learningsPath := filepath.Join(d.maestroDir, "state", "learnings.yaml")
	data, err := os.ReadFile(learningsPath)
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
	levelStr := "INFO"
	switch level {
	case LogLevelDebug:
		levelStr = "DEBUG"
	case LogLevelWarn:
		levelStr = "WARN"
	case LogLevelError:
		levelStr = "ERROR"
	}
	msg := fmt.Sprintf(format, args...)
	d.logger.Printf("%s %s daemon: %s", d.clock.Now().Format(time.RFC3339), levelStr, msg)
}
