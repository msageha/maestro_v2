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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
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

	fileLock *lock.FileLock
	server   *uds.Server
	watcher  *fsnotify.Watcher
	ticker   *time.Ticker

	handler      *QueueHandler
	stateReader  StateReader
	canComplete  CanCompleteFunc
	planExecutor PlanExecutor
	lockMap      *lock.MutexMap

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	shutdown sync.Once

	cleanupOnce sync.Once
	forceExit   atomic.Bool
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
		fileLock:   lock.NewFileLock(filepath.Join(maestroDir, "locks", "daemon.lock")),
		server:     server,
		ticker:     time.NewTicker(time.Duration(scanInterval) * time.Second),
		lockMap:    lock.NewMutexMap(),
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

	// Write PID file for reliable lifecycle management
	pidPath := filepath.Join(d.maestroDir, "daemon.pid")
	if err := os.WriteFile(pidPath, fmt.Appendf(nil, "%d", os.Getpid()), 0644); err != nil {
		_ = d.fileLock.Unlock()
		return fmt.Errorf("write pid file: %w", err)
	}

	// Step 2: Init fsnotify watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		d.cleanup() // removes PID file + socket + unlocks
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	d.watcher = watcher

	// Watch queue/ and results/ directories
	queueDir := filepath.Join(d.maestroDir, "queue")
	resultsDir := filepath.Join(d.maestroDir, "results")
	for _, dir := range []string{queueDir, resultsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			d.cleanup()
			return fmt.Errorf("ensure dir %s: %w", dir, err)
		}
		if err := watcher.Add(dir); err != nil {
			d.cleanup()
			return fmt.Errorf("watch %s: %w", dir, err)
		}
	}

	// Step 3: Init queue handler
	d.handler = NewQueueHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)

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

	// Step 4: Register UDS handlers
	d.registerHandlers()

	// Step 5: Start UDS server
	if err := d.server.Start(); err != nil {
		d.cleanup()
		return fmt.Errorf("start UDS server: %w", err)
	}
	d.log(LogLevelInfo, "UDS server listening on %s", filepath.Join(d.maestroDir, uds.DefaultSocketName))

	// Step 6: Start background loops
	d.wg.Add(2)
	go d.fsnotifyLoop()
	go d.tickerLoop()

	// Step 7: Run initial scan
	d.handler.PeriodicScan()
	d.log(LogLevelInfo, "daemon ready")

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
		d.handler.PeriodicScan()
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
func (d *Daemon) handleDashboard(req *uds.Request) *uds.Response {
	d.handler.scanMu.Lock()
	defer d.handler.scanMu.Unlock()

	cq, _ := d.handler.loadCommandQueue()
	taskQueues := d.handler.loadAllTaskQueues()
	nq, _ := d.handler.loadNotificationQueue()
	resultFiles := d.handler.metricsHandler.loadAllResultFiles()

	if err := d.handler.metricsHandler.UpdateDashboardFull(cq, taskQueues, nq, resultFiles); err != nil {
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
	defer d.wg.Done()
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
				d.log(LogLevelDebug, "fsnotify event=%s file=%s", event.Op, event.Name)
				d.handler.HandleFileEvent(event.Name)
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
	defer d.wg.Done()
	defer d.recoverPanic("tickerLoop")

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-d.ticker.C:
			d.log(LogLevelDebug, "periodic scan triggered")
			d.handler.PeriodicScan()
		}
	}
}

// waitSignals blocks until a shutdown signal or context cancellation is received.
func (d *Daemon) waitSignals() {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		d.log(LogLevelInfo, "received signal=%s, initiating graceful shutdown", sig)

		// Second signal → force exit
		go func() {
			<-sigCh
			d.log(LogLevelWarn, "received second signal, forcing exit")
			d.forceExit.Store(true)
			d.cleanup()
			os.Exit(1)
		}()

		d.Shutdown()
	case <-d.ctx.Done():
		d.log(LogLevelInfo, "context cancelled, waiting for shutdown to complete")
		d.Shutdown()
	}

	signal.Stop(sigCh)
}

// Shutdown performs graceful shutdown (idempotent via sync.Once).
func (d *Daemon) Shutdown() {
	d.shutdown.Do(func() {
		d.log(LogLevelInfo, "shutdown started")

		// 1. Cancel context (stops accepting new work)
		d.cancel()

		// 2. Stop producers
		d.ticker.Stop()
		if d.handler != nil {
			d.handler.Stop()
		}
		if d.watcher != nil {
			_ = d.watcher.Close()
		}
		if d.server != nil {
			_ = d.server.Stop()
		}

		// 3. Drain in-flight with timeout
		timeout := d.config.Daemon.ShutdownTimeoutSec
		if timeout <= 0 {
			timeout = 30
		}

		done := make(chan struct{})
		go func() {
			d.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			d.log(LogLevelInfo, "all goroutines drained")
		case <-time.After(time.Duration(timeout) * time.Second):
			d.log(LogLevelWarn, "shutdown timeout after %ds, some operations may be incomplete", timeout)
		}

		// 4. Cleanup
		d.cleanup()
		d.log(LogLevelInfo, "daemon stopped")
	})
}

// cleanup releases resources. Safe to call multiple times via cleanupOnce.
func (d *Daemon) cleanup() {
	d.cleanupOnce.Do(func() {
		socketPath := filepath.Join(d.maestroDir, uds.DefaultSocketName)
		_ = os.Remove(socketPath)
		// Remove PID file while lock is still held so no concurrent starter
		// reads a stale PID between lock release and PID file removal.
		_ = os.Remove(filepath.Join(d.maestroDir, "daemon.pid"))
		_ = d.fileLock.Unlock()
		if d.logFile != nil {
			_ = d.logFile.Close()
		}
	})
}

// recoverPanic catches panics in goroutines to prevent the daemon from crashing.
// It logs the panic and initiates a graceful shutdown instead of crashing.
func (d *Daemon) recoverPanic(goroutine string) {
	if r := recover(); r != nil {
		d.log(LogLevelError, "panic in %s: %v", goroutine, r)
		go d.Shutdown()
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
	d.logger.Printf("%s %s daemon: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}
