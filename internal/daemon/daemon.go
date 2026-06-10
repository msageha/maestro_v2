// Package daemon implements the maestro background daemon for queue
// processing and orchestration. It is the composition root: UDS endpoint
// adapters live in daemonapi, and the major domains (worktree, reconcile,
// dispatch, lease, …) live in sub-packages.
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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/daemon/fallback"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// LogLevel, Clock, DaemonLogger, StateReader, etc. are defined in
// internal/daemon/core and re-exported via core_aliases.go.

// fsSemaphoreBufferSize returns the semaphore capacity for concurrent fsnotify
// handler goroutines, dynamically sized based on available CPUs.
// Range: [8, 32] — clamped to prevent both starvation and excessive fan-out.
func fsSemaphoreBufferSize() int {
	n := runtime.NumCPU() * 2
	if n < 8 {
		return 8
	}
	if n > 32 {
		return 32
	}
	return n
}

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

	handler               *QueueHandler
	stateReader           StateManager
	canComplete           CanCompleteFunc
	deferredPlanCompleter DeferredPlanCompleterFunc
	phaseDiagnoser        PhaseDiagnoserFunc
	planExecutor          PlanExecutor
	lockMap               *lock.MutexMap
	qualityGateDaemon     *QualityGateDaemon
	circuitBreaker        *circuitbreaker.Handler
	admissionCtrl         *admission.Controller
	fallbackMgr           *fallback.Manager
	worktreeManager       *WorktreeManager
	continuousHandler     *ContinuousHandler

	// Phase C components (grouped in PhaseCManager)
	phaseC *PhaseCManager

	// Adaptive model selector wrapping phaseC.BanditSelector. Built after
	// phaseC init and handed to both the PlanExecutor (for SelectModel on
	// the inbound path) and the result handler (for UpdateReward on the
	// outbound path). Nil when the bandit feature is disabled.
	modelSelector *banditModelSelector

	// Review pipeline (grouped in ReviewCoordinator)
	reviewCoord *ReviewCoordinator

	eventBus    *events.Bus
	traceWriter *TraceWriter      // JSONL trace writer for event persistence
	tmuxLogFile io.Closer         // debug log for tmux operations
	selfWrites  *selfWriteTracker // tracks daemon-originated YAML writes for fsnotify filtering

	ctx    context.Context
	cancel context.CancelFunc
	eg     *errgroup.Group // tracks all daemon goroutines (loops + handlers)
	egCtx  context.Context // errgroup-derived context; use inside eg.Go goroutines
	// egMu serializes admission of new goroutines via spawnTracked against the
	// shutdown flag flip. Without it, an untracked caller (e.g. a UDS handler)
	// could observe shuttingDown=false, then race with Shutdown's eg.Wait():
	// either the new eg.Go() panics ("WaitGroup is reused before previous Wait
	// has returned") or the goroutine leaks past eg.Wait. Shutdown takes the
	// lock briefly to flip the flag — it must NOT be held across eg.Wait, since
	// tracked goroutines may need to spawn children via spawnTracked during
	// unwind (those callers will then observe shuttingDown=true and skip).
	egMu     sync.Mutex
	shutdown sync.Once

	// shuttingDown is an advisory flag read by spawners for fast-path rejection.
	shuttingDown atomic.Bool

	// sessionLost is set when the tmux session is detected as missing.
	// When true, dispatch of new tasks/commands is paused to avoid futile
	// delivery attempts. Cleared automatically when session recovery is detected.
	sessionLost atomic.Bool

	cleanupOnce        sync.Once
	closeExecutorsOnce sync.Once
	watcherCloseOnce   sync.Once
	exitFn             func(int) // os.Exit replacement; nil defaults to os.Exit
	forceExit          atomic.Bool

	// startupReconcileHook, when non-nil, is called instead of
	// worktreeManager.Reconcile() in startRuntime(). It allows tests to inject
	// slow reconciliation without a real git repository. Must be nil in
	// production.
	startupReconcileHook func()

	// --- Components ---
	api    *API
	watch  *WatchLoop
	bridge *EventBridge
}

// reviewTaskInfo tracks the source task of a dispatched review request.
type reviewTaskInfo struct {
	taskID    string
	commandID string
}

// SetStateReader sets the state manager for dependency resolution (Phase 6).
// Must be called before Run().
func (d *Daemon) SetStateReader(reader StateManager) {
	d.stateReader = reader
}

// SetCanComplete wires the plan.CanComplete function for R4 reconciliation.
// Must be called before Run() to avoid import cycles (daemon→plan→daemon).
func (d *Daemon) SetCanComplete(f CanCompleteFunc) {
	d.canComplete = f
}

// SetDeferredPlanCompleter wires the function that auto-completes a plan
// after worktree publish succeeds. Must be called before Run().
func (d *Daemon) SetDeferredPlanCompleter(f DeferredPlanCompleterFunc) {
	d.deferredPlanCompleter = f
}

// SetPhaseDiagnoser wires the phase diagnosis function for completed phase analysis.
// Must be called before Run() to avoid import cycles (daemon→plan).
func (d *Daemon) SetPhaseDiagnoser(fn PhaseDiagnoserFunc) {
	d.phaseDiagnoser = fn
}

// SetVerifyRunner replaces the §S1-1 Verification Runner used by the
// result-write API. Production callers wire NewRealVerifyRunner here when
// `config.Verify.enabled` is true; tests can inject NewFixedVerifyRunner to
// drive the verify_pending → repair_pending branch deterministically.
// Calling with nil is a no-op (the existing runner — typically the stub set
// by newDaemon — is preserved).
func (d *Daemon) SetVerifyRunner(r VerifyRunner) {
	if r == nil || d.api == nil || d.api.result == nil {
		return
	}
	d.api.result.SetVerifyRunner(r)
}

// SetVerifyAsync controls whether result_write returns after committing the
// worker result and lets daemon-owned background work finish verification.
func (d *Daemon) SetVerifyAsync(enabled bool) {
	if d.api == nil || d.api.result == nil {
		return
	}
	d.api.result.SetVerifyAsync(enabled)
}

// LockMap returns the daemon's shared MutexMap for coordinating state locks.
func (d *Daemon) LockMap() *lock.MutexMap {
	return d.lockMap
}

// --- Late-bound accessors for API dependency injection ---
// These methods return interface-typed values or nil, allowing API handlers to
// access Daemon components that may be initialized after newDaemon returns.
// Using named methods instead of inline closures makes the initialization
// dependency graph explicit and testable.

func (d *Daemon) eventBusAccessor() *events.Bus { return d.eventBus }
func (d *Daemon) fallbackAccessor() fallbackRecorder {
	if d.fallbackMgr != nil {
		return d.fallbackMgr
	}
	return nil
}
func (d *Daemon) circuitBreakerAccessor() circuitBreakerUpdater {
	if d.circuitBreaker != nil {
		return d.circuitBreaker
	}
	return nil
}
func (d *Daemon) reviewCoordAccessor() reviewDispatcher {
	if d.reviewCoord != nil {
		return d.reviewCoord
	}
	return nil
}
func (d *Daemon) contextAccessor() context.Context { return d.ctx }

// New creates a new Daemon instance.
func New(maestroDir string, cfg model.Config) (*Daemon, error) {
	logPath := filepath.Join(maestroDir, "logs", "daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0750); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600) //nolint:gosec // logPath is constructed from a controlled application log directory
	if err != nil {
		return nil, fmt.Errorf("open daemon log: %w", err)
	}

	d, err := newDaemon(maestroDir, cfg, logFile, logFile)
	if err != nil {
		_ = logFile.Close() // best-effort cleanup on init failure
		return nil, err
	}
	return d, nil
}

// newDaemon is the internal constructor accepting injectable dependencies
// (used by NewDaemon and tests). The (*, error) signature mirrors NewDaemon
// for caller symmetry even though the current internal path cannot fail —
// future wiring (e.g. validating injected lock paths) is expected to
// reintroduce a fallible code path here.
//
//nolint:unparam // signature kept symmetric with NewDaemon; future fallible setup is anticipated
func newDaemon(maestroDir string, cfg model.Config, w io.Writer, closer io.Closer) (*Daemon, error) {
	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // cancel is stored in d.cancel and called on shutdown

	socketPath, err := uds.SocketPath(maestroDir)
	if err != nil {
		cancel()
		return nil, err
	}
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

	d.api = newAPI(d)

	watch := &WatchLoop{d: d}
	watch.fsEg.SetLimit(fsSemaphoreBufferSize())
	d.watch = watch
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

	d.initComponents()

	if err := d.startRuntime(); err != nil {
		return err
	}

	d.handler.PeriodicScanWithContext(d.ctx)
	d.log(LogLevelInfo, "daemon ready")

	runOK = true
	d.waitSignals()

	return nil
}
