// Package daemon implements the maestro background daemon for queue processing and orchestration.
package daemon

import (
	"context"
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
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

func parseLogLevel(s string) LogLevel {
	switch strings.ToLower(s) {
	case "debug":
		return LogLevelDebug
	case "info":
		return LogLevelInfo
	case "warn", "warning":
		return LogLevelWarn
	case "error":
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

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
	circuitBreaker    *CircuitBreakerHandler
	worktreeManager   *WorktreeManager

	eventBus          *events.Bus
	eventUnsubscribers []func()
	tmuxLogFile        io.Closer // debug log for tmux operations

	dashboardMu sync.Mutex   // serializes concurrent dashboard generation
	fsSem       chan struct{} // bounds concurrent fsnotify handler goroutines

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup // short-lived in-flight handlers
	loopWg   sync.WaitGroup // long-lived loops (fsnotifyLoop, tickerLoop)
	shutdown sync.Once

	// shuttingDown + shutdownMu guard wg.Add against TOCTOU race with Shutdown.
	//
	// The RWMutex is essential: it ensures that checking shuttingDown and
	// calling wg.Add happen atomically with respect to Shutdown's wg.Wait.
	// Without the mutex, a goroutine could observe shuttingDown==false,
	// then Shutdown could set it to true and call wg.Wait, and then the
	// goroutine would call wg.Add — panicking because Add after Wait is
	// not allowed.
	//
	// shuttingDown is atomic.Bool (rather than plain bool) because external
	// consumers (e.g. QueueHandler) receive a *atomic.Bool pointer and may
	// read it for advisory fast-path checks outside the mutex. The
	// authoritative check-then-Add sequence in daemon.go holds shutdownMu.RLock.
	//
	// Writers (Shutdown): shutdownMu.Lock → shuttingDown.Store(true) → Unlock → wg.Wait.
	// Readers (spawners): shutdownMu.RLock → shuttingDown.Load → wg.Add(1) → RUnlock.
	shuttingDown atomic.Bool
	shutdownMu   sync.RWMutex

	selfWrites *selfWriteTracker // tracks daemon-originated YAML writes for fsnotify filtering

	cleanupOnce sync.Once
	forceExit   atomic.Bool
}

// selfWriteTracker tracks files written by the daemon to filter fsnotify self-notifications.
// When the daemon writes a YAML file via UDS handler, it records the path here.
// fsnotifyLoop checks this tracker and skips events for self-written files.
type selfWriteTracker struct {
	mu    sync.Mutex
	paths map[string]time.Time
}

func newSelfWriteTracker() *selfWriteTracker {
	return &selfWriteTracker{paths: make(map[string]time.Time)}
}

// Record marks a file path as self-written.
func (t *selfWriteTracker) Record(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paths[path] = time.Now()
	// Opportunistic cleanup of stale entries (> 30s)
	for p, ts := range t.paths {
		if time.Since(ts) > 30*time.Second {
			delete(t.paths, p)
		}
	}
}

// Consume checks if a path was recently self-written (within 10s).
// If so, removes it from tracking and returns true.
// Also performs opportunistic cleanup of stale entries to prevent accumulation
// when Record() is not called frequently.
func (t *selfWriteTracker) Consume(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts, ok := t.paths[path]
	if !ok {
		// Opportunistic cleanup of stale entries (> 30s) even on miss
		for p, pts := range t.paths {
			if time.Since(pts) > 30*time.Second {
				delete(t.paths, p)
			}
		}
		return false
	}
	delete(t.paths, path)
	// Opportunistic cleanup of other stale entries
	for p, pts := range t.paths {
		if time.Since(pts) > 30*time.Second {
			delete(t.paths, p)
		}
	}
	return time.Since(ts) < 10*time.Second
}

// Len returns the number of tracked paths (for testing).
func (t *selfWriteTracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.paths)
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

	// Step 3: Init queue handler
	d.handler = NewQueueHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)

	// Wire shutdown guard so debounce goroutines are tracked by d.wg
	d.handler.SetShutdownGuard(d.ctx, &d.wg, &d.shuttingDown, &d.shutdownMu)

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

	// Step 3.8: Initialize QualityGateDaemon
	d.qualityGateDaemon = NewQualityGateDaemon(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)

	// Step 3.8.1: Initialize CircuitBreakerHandler
	d.circuitBreaker = NewCircuitBreakerHandler(d.config, d.logger, d.logLevel)
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

	// Step 6: Start background loops (tracked by loopWg, not wg)
	d.loopWg.Add(2)
	go d.fsnotifyLoop()
	go d.tickerLoop()

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
	defer d.loopWg.Done()
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
				d.shutdownMu.RLock()
				if d.shuttingDown.Load() {
					d.shutdownMu.RUnlock()
					continue
				}
				d.wg.Add(1)
				d.shutdownMu.RUnlock()
				go func(name string) {
					defer d.wg.Done()
					defer d.recoverPanic("fsnotifyHandler")
					// Bound concurrency to prevent goroutine fan-out
					// during fsnotify bursts. Drop events that exceed
					// the semaphore; periodic scan will catch up.
					select {
					case d.fsSem <- struct{}{}:
						defer func() { <-d.fsSem }()
					default:
						d.log(LogLevelDebug, "fsnotify handler dropped (semaphore full) file=%s", name)
						return
					}
					d.handler.HandleFileEvent(name)
				}(event.Name)
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
	defer d.loopWg.Done()
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

// Shutdown performs graceful 2-phase shutdown (idempotent via sync.Once).
//
// Phase 1 (soft): Stop accepting new work and wait for in-flight handlers
// to complete naturally. Duration: 70% of shutdown_timeout_sec.
//
// Phase 2 (hard): Cancel context to force all goroutines (including loops)
// to exit. Wait for remaining goroutines. Duration: 30% of shutdown_timeout_sec.
//
// If the total timeout is exceeded, goroutine stacks are dumped for debugging.
func (d *Daemon) Shutdown() {
	d.shutdown.Do(func() {
		d.log(LogLevelInfo, "shutdown started session_alive=%v", tmux.SessionExists())

		totalTimeout := d.config.Daemon.ShutdownTimeoutSec
		if totalTimeout <= 0 {
			totalTimeout = 30
		}
		totalDuration := time.Duration(totalTimeout) * time.Second

		// Use a deadline so that time spent in producer Stop calls is
		// included in the overall budget (not added on top).
		deadline := d.clock.Now().Add(totalDuration)
		softDeadline := d.clock.Now().Add(totalDuration * 7 / 10)

		// ── Phase 1 (soft): graceful drain ──────────────────────────

		d.log(LogLevelInfo, "shutdown phase=soft deadline=%v", softDeadline.Format(time.RFC3339))

		// 1a. Set shuttingDown flag under write lock to prevent new wg.Add
		// calls from racing with wg.Wait below.
		d.shutdownMu.Lock()
		d.shuttingDown.Store(true)
		d.shutdownMu.Unlock()

		// 1b. Stop producers — no new work will be enqueued.
		// Context is NOT cancelled yet so in-flight handlers can finish.
		d.ticker.Stop()
		if d.handler != nil {
			d.handler.Stop()
		}
		if d.watcher != nil {
			// Closing the watcher also closes its Events channel,
			// which lets fsnotifyLoop exit without context cancel.
			if err := d.watcher.Close(); err != nil {
				d.log(LogLevelError, "shutdown watcher_close error=%v", err)
			}
		}
		if d.server != nil {
			if err := d.server.Stop(); err != nil {
				d.log(LogLevelError, "shutdown server_stop error=%v", err)
			}
		}

		// 1c. Unsubscribe from event bus and stop event processing.
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

		// 1d. Wait for short-lived in-flight handlers (wg) to drain.
		// Uses deadline-based remaining time so producer Stop() overhead
		// is included in the budget.
		softDone := make(chan struct{})
		go func() {
			d.wg.Wait()
			close(softDone)
		}()

		softRemaining := time.Until(softDeadline)
		if softRemaining <= 0 {
			d.log(LogLevelWarn, "shutdown phase=soft budget_exhausted_by_producer_stops, escalating immediately")
		} else {
			select {
			case <-softDone:
				d.log(LogLevelInfo, "shutdown phase=soft all_handlers_drained")
			case <-time.After(softRemaining):
				d.log(LogLevelWarn, "shutdown phase=soft timeout_exceeded, escalating to hard phase")
			}
		}

		// ── Phase 2 (hard): force stop ──────────────────────────────

		hardRemaining := time.Until(deadline)
		d.log(LogLevelInfo, "shutdown phase=hard remaining=%v", hardRemaining)

		// 2a. Cancel context — forces loops and any remaining goroutines to exit.
		d.cancel()

		if hardRemaining <= 0 {
			d.log(LogLevelWarn, "shutdown phase=hard budget_exhausted, dumping goroutine stacks")
			buf := make([]byte, 256*1024)
			n := runtime.Stack(buf, true)
			d.log(LogLevelWarn, "shutdown timeout after %ds, dumping %d bytes of goroutine stacks:\n%s",
				totalTimeout, n, string(buf[:n]))
		} else {
			// 2b. Wait for all goroutines (loops + any stragglers) with remaining budget.
			hardDone := make(chan struct{})
			go func() {
				d.loopWg.Wait()
				<-softDone
				close(hardDone)
			}()

			select {
			case <-hardDone:
				d.log(LogLevelInfo, "shutdown phase=hard all_goroutines_drained")
			case <-time.After(hardRemaining):
				// SRE-011: Dump goroutine stacks on timeout for debugging stuck shutdowns.
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
func (d *Daemon) notifySelfWrite(queuePath, writeType string) {
	d.selfWrites.Record(queuePath)
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
func (d *Daemon) recordSelfWrite(path string) {
	d.selfWrites.Record(path)
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
