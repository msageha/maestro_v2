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
// It acts as a composition root, owning API, WatchLoop, and EventBridge components.
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

	eventBus    *events.Bus
	tmuxLogFile io.Closer // debug log for tmux operations
	selfWrites  *selfWriteTracker // tracks daemon-originated YAML writes for fsnotify filtering

	ctx      context.Context
	cancel   context.CancelFunc
	eg       *errgroup.Group // tracks all daemon goroutines (loops + handlers)
	shutdown sync.Once

	// shuttingDown is an advisory flag read by spawners for fast-path rejection.
	// With errgroup, the TOCTOU guard (shutdownMu) is no longer needed because
	// eg.Go() called from within an eg.Go goroutine (e.g. fsnotifyLoop) is safe:
	// eg.Wait() cannot return while the calling goroutine is still running.
	shuttingDown atomic.Bool

	cleanupOnce sync.Once
	forceExit   atomic.Bool

	// --- Components ---
	api    *API
	watch  *WatchLoop
	bridge *EventBridge
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
// Uses snapshot/revalidate/consume to avoid TOCTOU: the stamp is only deleted
// after the file hash is verified to match, preventing loss of valid stamps
// when concurrent writes change the file between snapshot and read.
func (t *selfWriteTracker) Consume(path string) bool {
	// Phase 1: Snapshot stamp under lock
	t.mu.Lock()
	stamp, ok := t.stamps[path]
	if !ok {
		t.cleanStaleLocked()
		t.mu.Unlock()
		return false
	}
	if time.Now().After(stamp.Deadline) {
		delete(t.stamps, path)
		t.cleanStaleLocked()
		t.mu.Unlock()
		return false
	}
	t.mu.Unlock()

	// Phase 2: Read file outside lock (I/O)
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	fileHash := sha256.Sum256(content)

	// Phase 3: Revalidate and consume under lock
	t.mu.Lock()
	defer t.mu.Unlock()

	cur, ok := t.stamps[path]
	if !ok || cur != stamp {
		// Stamp was consumed by another goroutine or replaced by Record
		t.cleanStaleLocked()
		return false
	}
	if time.Now().After(cur.Deadline) {
		delete(t.stamps, path)
		t.cleanStaleLocked()
		return false
	}
	if fileHash != cur.Hash {
		t.cleanStaleLocked()
		return false
	}

	delete(t.stamps, path)
	t.cleanStaleLocked()
	return true
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
		selfWrites: newSelfWriteTracker(),
		ctx:        ctx,
		cancel:     cancel,
	}

	// Initialize components with back-pointers
	d.api = &API{d: d}
	d.watch = &WatchLoop{d: d, fsSem: make(chan struct{}, 8)}
	d.bridge = &EventBridge{d: d}

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

	// Step 3.8.2: Initialize WorktreeManager (default enabled)
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

	// Step 3.10: Subscribe QualityGateDaemon to events (via EventBridge)
	d.bridge.subscribeQualityGateEvents()

	// Step 3.11: Subscribe to queue write events for direct scan triggering
	// (bypasses fsnotify self-notification for daemon-originated writes)
	d.bridge.subscribeQueueWrittenEvents()

	// Step 4: Register UDS handlers (via API)
	d.api.registerHandlers()

	// Step 5: Start UDS server
	if err := d.server.Start(); err != nil {
		return fmt.Errorf("start UDS server: %w", err)
	}
	d.log(LogLevelInfo, "UDS server listening on %s", filepath.Join(d.maestroDir, uds.DefaultSocketName))

	// Step 6: Start background loops via errgroup (via WatchLoop)
	d.eg.Go(func() error { d.watch.fsnotifyLoop(); return nil })
	d.eg.Go(func() error { d.watch.tickerLoop(); return nil })

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
