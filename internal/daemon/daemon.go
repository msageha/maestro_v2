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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"

	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
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
	tmuxLogFile io.Closer         // debug log for tmux operations
	selfWrites  *selfWriteTracker // tracks daemon-originated YAML writes for fsnotify filtering

	ctx      context.Context
	cancel   context.CancelFunc
	eg       *errgroup.Group // tracks all daemon goroutines (loops + handlers)
	shutdown sync.Once

	// shuttingDown is an advisory flag read by spawners for fast-path rejection.
	shuttingDown atomic.Bool

	cleanupOnce        sync.Once
	closeExecutorsOnce sync.Once
	forceExit          atomic.Bool

	// --- Components ---
	api    *API
	watch  *WatchLoop
	bridge *EventBridge
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
	signal.Ignore(syscall.SIGHUP)

	// Install shutdown guard before prepareStartup so that partial startup
	// (lock acquired, PID written, watcher created) is cleaned up on error.
	var runOK bool
	defer func() {
		if !runOK {
			d.Shutdown()
		}
	}()

	if err := d.prepareStartup(); err != nil {
		return err
	}

	if err := d.initComponents(); err != nil {
		return err
	}

	if err := d.startRuntime(); err != nil {
		return err
	}

	d.handler.PeriodicScanWithContext(d.ctx)
	d.log(LogLevelInfo, "daemon ready")

	runOK = true
	d.waitSignals()

	return nil
}
