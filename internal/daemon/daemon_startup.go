package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"

	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
)

// prepareStartup acquires the file lock, writes PID, creates the fsnotify watcher,
// and sets up watched directories.
func (d *Daemon) prepareStartup() error {
	if err := d.fileLock.TryLock(); err != nil {
		return fmt.Errorf("daemon lock: %w", err)
	}
	d.log(LogLevelInfo, "daemon starting pid=%d", os.Getpid())

	// Initialize tmux debug logger
	tmuxLogPath := filepath.Join(d.maestroDir, "logs", "tmux_debug.log")
	if tmuxLogFile, err := os.OpenFile(tmuxLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		tmuxLogger := log.New(tmuxLogFile, "", log.LstdFlags|log.Lmicroseconds)
		tmux.SetDebugLogger(tmuxLogger)
		d.log(LogLevelInfo, "tmux debug logger initialized at %s", tmuxLogPath)
		d.tmuxLogFile = tmuxLogFile
	} else {
		d.log(LogLevelWarn, "failed to open tmux debug log: %v", err)
	}

	// Write PID file
	pidPath := filepath.Join(d.maestroDir, "daemon.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		if unlockErr := d.fileLock.Unlock(); unlockErr != nil {
			d.log(LogLevelError, "startup file_unlock error=%v", unlockErr)
		}
		return fmt.Errorf("write pid file: %w", err)
	}

	// Init fsnotify watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		d.cleanup()
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	d.watcher = watcher

	// Validate learnings file
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
	// Use a separate egCtx field so d.ctx (the root daemon context) is not overwritten.
	// d.cancel() always cancels d.ctx, which cascades to egCtx.
	d.eg, d.egCtx = errgroup.WithContext(d.ctx)

	return nil
}

// initComponents wires all daemon sub-components: handler, quality gate,
// circuit breaker, worktree manager, and event bus subscriptions.
func (d *Daemon) initComponents() error {
	d.handler = NewQueueHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)
	d.handler.SetShutdownGuard(d.ctx, &d.shuttingDown)

	if d.stateReader != nil {
		d.handler.SetStateReader(d.stateReader)
	}
	if d.canComplete != nil {
		d.handler.SetCanComplete(d.canComplete)
	}

	ch := NewContinuousHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)
	d.handler.resultHandler.SetContinuousHandler(ch)

	d.qualityGateDaemon = NewQualityGateDaemon(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel, d.ctx)

	d.circuitBreaker = circuitbreaker.NewHandler(d.config, d.logger, d.logLevel)
	if d.stateReader != nil {
		d.circuitBreaker.SetStateReader(d.stateReader)
	}
	d.handler.SetCircuitBreaker(d.circuitBreaker)

	if d.config.Worktree.Enabled {
		d.worktreeManager = NewWorktreeManager(d.maestroDir, d.config.Worktree, d.logger, d.logLevel)
		d.handler.SetWorktreeManager(d.worktreeManager)
		d.log(LogLevelInfo, "worktree isolation enabled base_branch=%s", d.config.Worktree.EffectiveBaseBranch())
		d.worktreeManager.Reconcile()
	}

	d.eventBus = events.NewBus(100)
	d.handler.dispatcher.SetEventBus(d.eventBus)
	d.handler.dispatcher.SetQualityGate(d.qualityGateDaemon)
	d.handler.dependencyResolver.SetEventBus(d.eventBus)
	d.handler.resultHandler.SetEventBus(d.eventBus)

	d.bridge.subscribeQualityGateEvents()
	d.bridge.subscribeQueueWrittenEvents()

	return nil
}

// startRuntime starts the UDS server, background loops, and quality gate.
func (d *Daemon) startRuntime() error {
	d.api.registerHandlers()

	if err := d.server.Start(); err != nil {
		return fmt.Errorf("start UDS server: %w", err)
	}
	d.log(LogLevelInfo, "UDS server listening on %s", filepath.Join(d.maestroDir, uds.DefaultSocketName))

	d.eg.Go(func() error { d.watch.fsnotifyLoop(); return nil })
	d.eg.Go(func() error { d.watch.tickerLoop(); return nil })

	if err := d.qualityGateDaemon.Start(); err != nil {
		return fmt.Errorf("start quality gate daemon: %w", err)
	}

	return nil
}
