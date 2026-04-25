package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/daemon/fallback"
	"github.com/msageha/maestro_v2/internal/daemon/judge"
	"github.com/msageha/maestro_v2/internal/daemon/rollout"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
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
	if tmuxLogFile, err := os.OpenFile(tmuxLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil { //nolint:gosec // 0644 is intentional for a log file that may be read by the owning user
		tmuxLogger := log.New(tmuxLogFile, "", log.LstdFlags|log.Lmicroseconds)
		tmux.SetDebugLogger(tmuxLogger)
		d.log(LogLevelInfo, "tmux debug logger initialized at %s", tmuxLogPath)
		d.tmuxLogFile = tmuxLogFile
	} else {
		d.log(LogLevelWarn, "failed to open tmux debug log: %v", err)
	}

	// Write PID file
	pidPath := filepath.Join(d.maestroDir, "daemon.pid")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0600); err != nil {
		if unlockErr := d.fileLock.Unlock(); unlockErr != nil {
			d.log(LogLevelError, "startup file_unlock error=%v", unlockErr)
		}
		return fmt.Errorf("write pid file: %w", err)
	}

	// M2: Clean up stale tmp files (older than 1 hour) that may have been
	// left behind by SIGKILL during plan submit stdin materialization.
	d.cleanStaleTmpFiles()

	// P4: Validate command state YAMLs and recover any corrupted file from
	// its sibling .bak. Failures are logged as warnings; startup continues.
	d.recoverStateFiles()

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
	queueDir := queueDirPath(d.maestroDir)
	resultsDir := resultsDirPath(d.maestroDir)
	for _, dir := range []string{queueDir, resultsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for queue/results directories
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

// cleanStaleTmpFiles removes files in .maestro/tmp/ that are older than 1 hour.
// These can be left behind when a CLI process is killed (e.g. SIGKILL) before
// the deferred os.Remove runs.
func (d *Daemon) cleanStaleTmpFiles() {
	tmpDir := filepath.Join(d.maestroDir, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		// Directory might not exist yet; that's fine.
		return
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(tmpDir, entry.Name())
			if err := os.Remove(path); err != nil {
				d.log(LogLevelWarn, "cleanup_tmp remove_failed path=%s error=%v", path, err)
			} else {
				d.log(LogLevelDebug, "cleanup_tmp removed path=%s age=%s", path, time.Since(info.ModTime()).Truncate(time.Second))
			}
		}
	}
}

// initComponents wires all daemon sub-components: handler, quality gate,
// circuit breaker, worktree manager, and event bus subscriptions.
func (d *Daemon) initComponents() {
	// Load verify config with fallback to project-aware defaults so non-Go
	// repositories do not silently inherit `go vet ./...` (which would always
	// fail and obscure the real issue, "verify.yaml not configured").
	projectRoot := filepath.Dir(d.maestroDir)
	vcfg, err := model.LoadOrDefaultVerifyConfigForProject(projectRoot, verifyConfigPath(d.maestroDir))
	if err != nil {
		d.log(LogLevelWarn, "verify_config load error=%v, using defaults", err)
		vcfg = model.DefaultVerifyConfigForProject(projectRoot)
	}
	d.verifyConfig = vcfg
	d.log(LogLevelInfo, "verify_config loaded commands=%d", len(vcfg.AllCommands()))

	d.handler = NewQueueHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)
	d.handler.SetShutdownGuard(d.ctx, &d.shuttingDown, d.Shutdown)
	d.handler.SetSessionLostFlag(&d.sessionLost)

	if d.stateReader != nil {
		d.handler.SetStateReader(d.stateReader)
	}
	if d.canComplete != nil {
		d.handler.SetCanComplete(d.canComplete)
	}
	if d.deferredPlanCompleter != nil {
		d.handler.SetDeferredPlanCompleter(d.deferredPlanCompleter)
	}
	if d.phaseDiagnoser != nil {
		d.handler.SetPhaseDiagnoser(d.phaseDiagnoser)
	}

	if d.config.Continuous.Enabled {
		ch := NewContinuousHandler(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel)
		d.handler.SetContinuousHandler(ch)
	}

	if d.config.QualityGates.Enabled {
		d.qualityGateDaemon = NewQualityGateDaemon(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel, d.ctx)
	}

	// Admission control: always initialized (uses effective defaults if unconfigured)
	d.admissionCtrl = admission.NewController(d.config.AdmissionControl)
	d.handler.SetAdmissionController(d.admissionCtrl)
	d.log(LogLevelInfo, "admission_control initialized verify=%d repair=%d rollout=%d",
		d.config.AdmissionControl.EffectiveMaxConcurrentVerify(),
		d.config.AdmissionControl.EffectiveMaxConcurrentRepair(),
		d.config.AdmissionControl.EffectiveMaxConcurrentRollout())

	// Fallback manager: only when enabled
	if d.config.Fallback.EffectiveEnabled() {
		d.fallbackMgr = fallback.NewManager(fallback.Config{
			Enabled:                     true,
			ConsecutiveFailureThreshold: d.config.Fallback.EffectiveConsecutiveFailureThreshold(),
			RecoveryCheckIntervalSec:    d.config.Fallback.EffectiveRecoveryCheckIntervalSec(),
			MinHealthyDurationSec:       d.config.Fallback.EffectiveMinHealthyDurationSec(),
		})
		d.handler.SetFallbackManager(d.fallbackMgr)
		d.log(LogLevelInfo, "fallback_manager enabled threshold=%d",
			d.config.Fallback.EffectiveConsecutiveFailureThreshold())
	}

	if d.config.CircuitBreaker.Enabled {
		d.circuitBreaker = circuitbreaker.NewHandler(d.config, d.logger, d.logLevel)
		if d.stateReader != nil {
			d.circuitBreaker.SetStateReader(d.stateReader)
		}
		d.handler.SetCircuitBreaker(d.circuitBreaker)
	}

	if d.config.Worktree.Enabled {
		d.worktreeManager = NewWorktreeManager(d.maestroDir, d.config.Worktree, d.logger, d.logLevel)
		// Wire the YAML-backed signal store so the resolver pipeline can
		// CAS-update merge_conflict signals on planner_signals.yaml without
		// importing daemon-internal types.
		d.worktreeManager.SetSignalStore(NewYAMLSignalStore(d.maestroDir, d.lockMap))
		d.handler.SetWorktreeManager(d.worktreeManager)
		// Wire the worktree-backed verify workdir resolver so §S1-1
		// Verification runs inside the worker worktree (or integration
		// worktree, or project root for RunOnMain) instead of the daemon's
		// CWD. Without this, verify would not see worker uncommitted changes.
		d.api.result.SetVerifyWorkdirResolver(newWorktreeVerifyWorkdirResolver(d.worktreeManager, d.maestroDir))
		// Wire the post-completion AutoRecover hook so that successful
		// merge_conflict / publish_conflict resolution tasks immediately
		// drive ResumeMerge / RetryPublish without the Planner agent
		// having to issue a separate CLI op. Worktree-disabled runs skip
		// this on purpose: there is no integration branch to advance.
		d.api.result.SetWorktreeManager(d.worktreeManager)
		d.log(LogLevelInfo, "worktree isolation enabled base_branch=%s", d.config.Worktree.EffectiveBaseBranch())
		// NOTE: Reconcile() is intentionally deferred to startRuntime() so it
		// runs after the UDS server starts listening. Reconcile may spawn git
		// subprocesses (worktree list / remove) that can take several seconds
		// when orphaned worktrees exist, which would otherwise block the UDS
		// server from starting and cause waitDaemonReady to time out.
	} else {
		// Worktree disabled: every task — including verify — runs at the
		// project root. Wire a constant resolver so the workdir is consistent
		// even when verify.yaml runs against a non-isolated checkout.
		d.api.result.SetVerifyWorkdirResolver(newProjectRootVerifyWorkdirResolver(d.maestroDir))
	}

	d.initPhaseB()
	d.phaseC = newPhaseCManager(d.config, d.getAvailableModels(), d.log)
	d.handler.SetPhaseCManager(d.phaseC)

	// Wire the adaptive model selector into the PlanExecutor (if configured).
	// The executor is set before d.Run() in cmd_daemon.go, but PhaseC state
	// (bandit selector) is constructed here, so we perform the late binding
	// now — after PhaseC is ready but before any plan traffic arrives.
	d.modelSelector = newBanditModelSelector(d.phaseC.BanditSelector, d.config.Bandit)
	if d.modelSelector != nil {
		if settable, ok := d.planExecutor.(core.PlanExecutorModelSelectorSettable); ok {
			settable.SetModelSelector(d.modelSelector)
			d.log(LogLevelInfo, "adaptive model selector wired into plan executor")
		} else {
			d.log(LogLevelDebug, "plan executor does not support SetModelSelector; static model mapping retained")
		}
		// Also make it available to the result handler so task outcomes can
		// feed back into the bandit as rewards.
		d.handler.SetModelSelector(d.modelSelector)
	}

	d.eventBus = events.NewBus(d.ctx, 100)
	d.handler.SetEventBus(d.eventBus)

	// Trace writer: persist all events to JSONL for post-mortem analysis.
	tracePath := filepath.Join(d.maestroDir, "logs", "trace.jsonl")
	if tw, err := NewTraceWriter(tracePath); err != nil {
		d.log(LogLevelWarn, "trace writer disabled: %v", err)
	} else {
		d.traceWriter = tw
		for _, et := range []events.EventType{
			events.EventTaskStarted,
			events.EventTaskCompleted,
			events.EventPhaseTransition,
			events.EventQueueWritten,
		} {
			d.eventBus.Subscribe(et, tw.HandleEvent)
		}
	}

	if d.qualityGateDaemon != nil {
		d.handler.SetQualityGate(d.qualityGateDaemon)
	}

	// Review coordinator: groups dispatcher + usefulness tracker.
	// Initialized after eventBus and other dependencies are ready.
	d.reviewCoord = newReviewCoordinator(d.config.Review, d.maestroDir, d.log)
	if d.reviewCoord.Enabled() && d.worktreeManager != nil {
		d.reviewCoord.SetWorktreeManager(d.worktreeManager)
	}

	// Wire API handler dependencies that require initComponents artifacts.
	d.api.shared.SetFileLockHolder(d.handler)
	d.api.shared.SetEventBus(func() *events.Bus { return d.eventBus })
	d.api.dashboard.handlerReady = func() bool { return d.handler != nil }
	d.api.heartbeat.leaseManager = func() QueueLeaseManager { return d.handler.leaseManager }
	d.api.heartbeat.scanMu = func() *sync.RWMutex { return &d.handler.scanExecutor.scanMu }
	d.api.plan.planExecutor = d.planExecutor
	d.api.plan.worktreeManager = d.worktreeManager
	// triggerScan is already wired in newDaemon; no re-assignment needed.

	if d.qualityGateDaemon != nil {
		d.bridge.subscribeQualityGateEvents()
	}
	d.bridge.subscribeQueueWrittenEvents()
}

// startRuntime starts the UDS server, background loops, and quality gate.
func (d *Daemon) startRuntime() error {
	d.api.registerHandlers(d.server, systemHandlers{
		scan: func(_ *uds.Request) *uds.Response {
			if d.handler == nil {
				return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
			}
			d.handler.PeriodicScanWithContext(d.ctx)
			return uds.SuccessResponse(map[string]string{"status": "scanned"})
		},
		shutdown: func(_ *uds.Request) *uds.Response {
			d.log(LogLevelInfo, "shutdown requested via UDS")
			go func() { defer d.recoverPanic("shutdownHandler"); d.Shutdown() }()
			return uds.SuccessResponse(map[string]string{"status": "shutdown_accepted"})
		},
	})

	if err := d.server.Start(); err != nil {
		return fmt.Errorf("start UDS server: %w", err)
	}
	d.log(LogLevelInfo, "UDS server listening on %s", filepath.Join(d.maestroDir, uds.DefaultSocketName))

	d.eg.Go(func() error { d.watch.fsnotifyLoop(); return nil })
	d.eg.Go(func() error { d.watch.tickerLoop(); return nil })

	// Run worktree reconciliation after the UDS server starts so that ping
	// requests can be answered while git operations (worktree list / remove)
	// complete in the background. This prevents waitDaemonReady from timing
	// out when orphaned worktrees cause Reconcile to take several seconds.
	// startupReconcileHook (when set in tests) replaces the real Reconcile.
	if d.worktreeManager != nil || d.startupReconcileHook != nil {
		d.eg.Go(func() error {
			if d.startupReconcileHook != nil {
				d.startupReconcileHook()
			} else {
				d.worktreeManager.Reconcile()
			}
			return nil
		})
	}

	if d.qualityGateDaemon != nil {
		if err := d.qualityGateDaemon.Start(); err != nil {
			return fmt.Errorf("start quality gate daemon: %w", err)
		}
	}

	// Start review results monitoring goroutine
	if d.reviewCoord.Enabled() {
		d.eg.Go(func() error { d.reviewCoord.MonitorResults(); return nil })
	}

	return nil
}

// initPhaseB initializes Phase B components: rollout manager and judge.
func (d *Daemon) initPhaseB() {
	cfg := d.config

	// Rollout Manager initialization
	if cfg.Rollout.EffectiveEnabled() {
		maxParallel := cfg.Rollout.EffectiveMaxParallelPerTask()
		d.rolloutManager = rollout.NewManager(maxParallel)
		d.log(LogLevelInfo, "rollout_manager initialized maxParallel=%d", maxParallel)
	}

	// Judge initialization
	if cfg.Judge.EffectiveEnabled() {
		timeout := time.Duration(cfg.Judge.EffectiveTimeoutSec()) * time.Second
		model := cfg.Judge.EffectiveModel()
		stubCaller := &logOnlyCaller{logger: d.logger}
		d.judgeCaller = judge.NewJudge(stubCaller, model, timeout)
		d.log(LogLevelInfo, "judge initialized model=%s timeout=%s", model, timeout)
		// Phase C is not yet wired to a real LLM caller — surface the stub
		// status loudly so operators who enable judge in config do not assume
		// the comparison is meaningful. Without this warning the WARN-level
		// signal would only appear after the first prompt is dispatched.
		d.log(LogLevelWarn,
			"judge_stub_active: judge.Caller is a logOnlyCaller stub (always returns winner_index=0); "+
				"rollout judging is non-functional until a real LLM caller is wired in")
	}
}

// getAvailableModels returns the deduplicated set of model names referenced
// by worker config. wc.Models maps workerID → modelName, so the model arms
// supplied to the bandit selector come from the *values* of that map, not
// the keys. Iterating over keys would feed worker IDs ("worker1", ...) to
// the selector as if they were model names — every selection would then
// reference a non-existent arm. wc.DefaultModel is included first because
// it is the canonical fallback when a worker has no explicit override.
func (d *Daemon) getAvailableModels() []string {
	wc := d.config.Agents.Workers
	seen := make(map[string]bool, len(wc.Models)+1)
	models := make([]string, 0, len(wc.Models)+1)
	if wc.DefaultModel != "" {
		models = append(models, wc.DefaultModel)
		seen[wc.DefaultModel] = true
	}
	for _, m := range wc.Models {
		if m == "" || seen[m] {
			continue
		}
		models = append(models, m)
		seen[m] = true
	}
	return models
}

// logOnlyCaller is a stub implementation of judge.Caller that logs the prompt
// and returns a fallback JSON response. It will be replaced by a real LLM
// client in a future phase.
type logOnlyCaller struct {
	logger *log.Logger
}

func (c *logOnlyCaller) Call(_ context.Context, prompt string) (string, error) {
	if c.logger != nil {
		c.logger.Printf("[INFO] judge_stub_call prompt_len=%d", len(prompt))
	}
	resp, _ := json.Marshal(map[string]any{
		"winner_index": 0,
		"reasoning":    "stub: no LLM backend configured",
	})
	return string(resp), nil
}

// extractTaskIDFromRequestID extracts the taskID from a review request ID
// with format "review-{taskID}-{nanoTimestamp}".
func extractTaskIDFromRequestID(requestID string) string {
	trimmed := strings.TrimPrefix(requestID, "review-")
	if lastDash := strings.LastIndex(trimmed, "-"); lastDash > 0 {
		return trimmed[:lastDash]
	}
	return trimmed
}
