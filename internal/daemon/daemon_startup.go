package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/apipolicy"
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/daemon/complexity"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/daemon/featuregate"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
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

	// N-2: GC abandoned per-worker CODEX_HOME tempdirs from prior daemon
	// PIDs. prepareCodexHomeForCurrentWorker creates
	// <TempDir>/maestro-codex-<pid>-<agent_id> and never cleans them up
	// itself (worker pane lifecycle ends with exec, no defer). Without
	// this GC, long-running CI / connectivity testing accumulates one dir
	// per worker per daemon restart. Skipped at compile time? No — this
	// is cheap (read TempDir + signal-0 PID checks).
	if removed, errs := agent.GCStaleCodexHomes(); removed > 0 || len(errs) > 0 {
		d.log(LogLevelInfo, "codex_home_gc removed=%d errors=%d", removed, len(errs))
		for _, gcErr := range errs {
			d.log(LogLevelWarn, "codex_home_gc remove_failed: %v", gcErr)
		}
	}

	// P4: Validate command state YAMLs and recover any corrupted file from
	// its sibling .bak. Failures are logged as warnings; startup continues.
	d.recoverStateFiles()

	// Same recovery for queue YAMLs. A truncated planner.yaml /
	// worker{N}.yaml / planner_signals.yaml / orchestrator.yaml would
	// otherwise look "empty" and the daemon would silently lose the
	// in-flight lease_epoch history.
	d.recoverQueueFiles()

	// GC stale flock files in .maestro/locks/. Run exactly once, here at
	// startup, before any other lock acquisition begins. Concurrent GC
	// is unsafe (inode-split race after unlink + recreate). daemon.lock
	// is excluded because the running daemon holds it.
	locksDir := filepath.Join(d.maestroDir, "locks")
	stats, gcErr := lock.GCStaleLockFiles(locksDir, lock.DefaultLockGCAge,
		[]string{"daemon.lock"},
		func(format string, args ...any) { d.log(LogLevelInfo, format, args...) })
	if gcErr != nil {
		d.log(LogLevelWarn, "lock_gc failed dir=%s error=%v", locksDir, gcErr)
	} else if stats.Removed > 0 || stats.SkippedHeld > 0 || stats.SkippedRaced > 0 {
		d.log(LogLevelInfo,
			"lock_gc summary scanned=%d removed=%d skipped_young=%d skipped_held=%d skipped_raced=%d skipped_other=%d",
			stats.Scanned, stats.Removed, stats.SkippedYoung, stats.SkippedHeld, stats.SkippedRaced, stats.SkippedOther)
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
		// Retained on the daemon so the ticker loop can run the continuous
		// stall watchdog (CheckStall) alongside periodic scans.
		d.continuousHandler = ch
	}

	if d.config.QualityGates.Enabled {
		d.qualityGateDaemon = NewQualityGateDaemon(d.maestroDir, d.config, d.lockMap, d.logger, d.logLevel, d.ctx)
	}

	// Admission control: always initialized (uses effective defaults if unconfigured)
	// WithLogger wires the persistent-saturation WARN path; without it the
	// controller's saturation telemetry can never fire (logger stays nil).
	d.admissionCtrl = admission.NewController(d.config.AdmissionControl, admission.WithLogger(d.logger))
	d.handler.SetAdmissionController(d.admissionCtrl)
	d.api.result.SetAdmissionController(d.admissionCtrl)
	d.log(LogLevelInfo, "admission_control initialized verify=%d repair=%d",
		d.config.AdmissionControl.EffectiveMaxConcurrentVerify(),
		d.config.AdmissionControl.EffectiveMaxConcurrentRepair())

	// Degraded-mode worker blacklisting (the old fallback manager) is
	// intentionally absent — see the retirement note in
	// model/config_types.go. Autonomous LLM Orchestration recovers via
	// per-task retry, repair tasks, and circuit breakers; it must not
	// silence whole workers.

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
		d.logNonClaudeWorkerAdvisory()
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

	d.phaseC = newPhaseCManager(d.config, d.maestroDir, d.getAvailableModels(), d.log)
	d.handler.SetPhaseCManager(d.phaseC)

	// Phase C-3: hand the EnsembleVerifier perspectives to the verify
	// runner so extended_verification.security_check / performance_bench
	// actually drive the verify executed by result_write. Without this
	// the verifier was constructed but never consulted by the runner;
	// the perspective set silently never executed and operators saw
	// "ensemble verifier security perspective enabled" in the startup
	// log followed by no security commands actually running. The wiring
	// is gated on the runner having type *RealVerifyRunner — the skip /
	// fixed runners used by tests intentionally bypass extended verify.
	if d.phaseC != nil && d.phaseC.EnsembleVerifier != nil {
		if rvr, ok := d.api.result.verifyRunner.(*RealVerifyRunner); ok {
			rvr.SetEnsembleVerifier(d.phaseC.EnsembleVerifier)
			d.log(LogLevelInfo,
				"verify_runner_ensemble_wired perspectives=%d max_auto_retries=%d",
				len(d.phaseC.EnsembleVerifier.Perspectives()),
				d.phaseC.EnsembleVerifier.MaxAutoRetries())
		}
	}

	// Wire the adaptive model selector into the PlanExecutor (if configured).
	// The executor is set before d.Run() in cmd_daemon.go, but PhaseC state
	// (bandit selector) is constructed here, so we perform the late binding
	// now — after PhaseC is ready but before any plan traffic arrives.
	d.modelSelector = newBanditModelSelector(d.phaseC.BanditSelector, d.config.Bandit, d.log)
	if d.modelSelector != nil {
		// Restore persisted arm statistics so warm-up survives restarts —
		// without this the trace/min-sample thresholds reset every boot and
		// adaptive selection never activated in practice.
		d.modelSelector.LoadState(banditStatePath(d.maestroDir))
		// Gate SelectModel on the feature_profiles config: adaptive model
		// selection only applies at difficulty levels whose profile enables
		// it. The level is derived from the task's Bloom level (the only
		// complexity signal available at the selection call site); reward
		// recording stays ungated so learning continues while gated off.
		phaseC := d.phaseC
		d.modelSelector.featureEnabled = func(bloomLevel int) bool {
			level := phaseC.EvaluateLevel(complexity.Input{BloomLevel: bloomLevel})
			return phaseC.IsFeatureEnabled(level, featuregate.FeatureAdaptiveModelSelection)
		}
		if settable, ok := d.planExecutor.(core.PlanExecutorModelSelectorSettable); ok {
			settable.SetModelSelector(d.modelSelector)
			d.log(LogLevelInfo, "adaptive model selector wired into plan executor (contextual buckets=%d)", bloomBucketCount)
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
	d.api.plan.SetExecutor(d.planExecutor)
	d.api.plan.SetWorktreeManager(d.worktreeManager)
	// triggerScan is already wired in newDaemon; no re-assignment needed.

	if d.qualityGateDaemon != nil {
		d.bridge.subscribeQualityGateEvents()
	}
	d.bridge.subscribeQueueWrittenEvents()
}

// startRuntime starts the UDS server, background loops, and quality gate.
func (d *Daemon) startRuntime() error {
	d.api.registerHandlers(d.server, systemHandlers{
		scan: func(req *uds.Request) *uds.Response {
			if resp := apipolicy.RequireCallerRole(req, "scan", uds.RoleCLI, uds.RoleOrchestrator); resp != nil {
				return resp
			}
			if d.handler == nil {
				return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
			}
			d.handler.PeriodicScanWithContext(d.ctx)
			return uds.SuccessResponse(map[string]string{"status": "scanned"})
		},
		shutdown: func(req *uds.Request) *uds.Response {
			if resp := apipolicy.RequireCallerRole(req, "shutdown", uds.RoleCLI); resp != nil {
				return resp
			}
			// Surface the calling context so an unexpected shutdown can be
			// traced to its origin. Earlier the log line was just
			// "shutdown requested via UDS" with no attribution; an
			// operator who saw their daemon stop seconds after `maestro
			// up -d` had no way to tell whether a stale CLI process,
			// another daemon-start race, or a runaway script issued the
			// shutdown (Report 2026-05-06 P2 issue-4).
			d.log(LogLevelInfo,
				"shutdown requested via UDS caller_role=%q protocol_version=%d params_bytes=%d "+
					"(daemon will now drain and exit; investigate this caller if the shutdown was unexpected)",
				req.CallerRole, req.ProtocolVersion, len(req.Params))
			go func() { defer d.recoverPanic("shutdownHandler"); d.Shutdown() }()
			return uds.SuccessResponse(map[string]string{"status": "shutdown_accepted"})
		},
	})

	if err := d.server.Start(); err != nil {
		return fmt.Errorf("start UDS server: %w", err)
	}
	socketPath, err := uds.SocketPath(d.maestroDir)
	if err != nil {
		return fmt.Errorf("resolve UDS socket path: %w", err)
	}
	d.log(LogLevelInfo, "UDS server listening on %s", socketPath)

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

// collectNonClaudeWorkers returns the per-worker "id=runtime/model" labels for
// every entry in wc whose resolved runtime is not claude-code. The list is
// deterministic in worker number order so log output is stable across runs;
// claude-code workers are omitted entirely.
//
// Pulled out as a pure function for testability — the daemon-level wrapper
// just shells the result into a structured log line.
func collectNonClaudeWorkers(wc model.WorkerConfig) []string {
	out := make([]string, 0)
	seen := map[string]bool{}
	check := func(workerID, modelName string) {
		if modelName == "" {
			return
		}
		runtime, _ := model.ParseRuntimeFromModel(modelName)
		if runtime == model.RuntimeClaudeCode {
			return
		}
		if seen[workerID] {
			return
		}
		seen[workerID] = true
		out = append(out, fmt.Sprintf("%s=%s/%s", workerID, runtime, modelName))
	}
	for i := 1; i <= wc.Count; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		if specific, ok := wc.Models[workerID]; ok {
			check(workerID, specific)
			continue
		}
		check(workerID, wc.DefaultModel)
	}
	return out
}

// logNonClaudeWorkerAdvisory emits a one-shot operator-facing advisory at
// startup whenever the worker pool resolves to codex / gemini for any worker.
// Non-claude runtimes intentionally run with broad sandbox bypass flags
// (--dangerously-bypass-approvals-and-sandbox / --yolo) and do not run under
// the Claude-only Worker policy hook, so the only cross-runtime safety nets
// are the worktree isolation boundary and the validate_run_on_main destructive-
// content pre-flight in dispatch. This log is purely informational — it does
// not gate or downgrade non-claude runtimes (those are first-class production
// targets) and is here so an operator scanning daemon.log sees the
// constraint surface explicitly when a config arrives with codex / gemini in
// agents.workers.models.
func (d *Daemon) logNonClaudeWorkerAdvisory() {
	nonClaudeWorkers := collectNonClaudeWorkers(d.config.Agents.Workers)
	if len(nonClaudeWorkers) == 0 {
		return
	}
	d.log(LogLevelInfo,
		"non_claude_worker_runtime_advisory workers=%v "+
			"(no Claude policy hook applies — cross-runtime safety net = worktree isolation + validate_run_on_main destructive-content pre-flight)",
		nonClaudeWorkers)
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

// extractTaskIDFromRequestID extracts the taskID from a review request ID
// with format "review-{taskID}-{nanoTimestamp}".
func extractTaskIDFromRequestID(requestID string) string {
	trimmed := strings.TrimPrefix(requestID, "review-")
	if lastDash := strings.LastIndex(trimmed, "-"); lastDash > 0 {
		return trimmed[:lastDash]
	}
	return trimmed
}
