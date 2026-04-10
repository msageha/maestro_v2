package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/bandit"
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/daemon/complexity"
	"github.com/msageha/maestro_v2/internal/daemon/evolution"
	"github.com/msageha/maestro_v2/internal/daemon/fallback"
	"github.com/msageha/maestro_v2/internal/daemon/featuregate"
	"github.com/msageha/maestro_v2/internal/daemon/judge"
	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/daemon/reviewer"
	"github.com/msageha/maestro_v2/internal/daemon/rollout"
	"github.com/msageha/maestro_v2/internal/daemon/search"
	"github.com/msageha/maestro_v2/internal/daemon/verification"
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
	queueDir := filepath.Join(d.maestroDir, "queue")
	resultsDir := filepath.Join(d.maestroDir, "results")
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
	d.handler.SetShutdownGuard(d.ctx, &d.shuttingDown)

	if d.stateReader != nil {
		d.handler.SetStateReader(d.stateReader)
	}
	if d.canComplete != nil {
		d.handler.SetCanComplete(d.canComplete)
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
		d.log(LogLevelInfo, "worktree isolation enabled base_branch=%s", d.config.Worktree.EffectiveBaseBranch())
		d.worktreeManager.Reconcile()
	}

	// Review dispatcher: only when enabled
	if d.config.Review.Enabled {
		d.reviewDispatcher = reviewer.NewReviewDispatcher(d.config.Review)
		d.reviewRequests = make(map[string]reviewTaskInfo)

		stateDir := filepath.Join(d.maestroDir, "state")
		tracker, err := reviewer.NewUsefulnessTracker(stateDir)
		if err != nil {
			d.log(LogLevelWarn, "usefulness_tracker_init_failed error=%v (reviews will run without tracking)", err)
		} else {
			d.usefulnessTracker = tracker
		}
		d.log(LogLevelInfo, "review_dispatcher enabled models=%v min_bloom=%d max_concurrent=%d",
			d.config.Review.Models,
			d.config.Review.EffectiveMinBloomLevel(),
			d.config.Review.EffectiveMaxConcurrentReviews())
	}

	d.initPhaseB()
	d.initPhaseC()

	d.eventBus = events.NewBus(d.ctx, 100)
	d.handler.SetEventBus(d.eventBus)
	if d.qualityGateDaemon != nil {
		d.handler.SetQualityGate(d.qualityGateDaemon)
	}

	if d.qualityGateDaemon != nil {
		d.bridge.subscribeQualityGateEvents()
	}
	d.bridge.subscribeQueueWrittenEvents()
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

	if d.qualityGateDaemon != nil {
		if err := d.qualityGateDaemon.Start(); err != nil {
			return fmt.Errorf("start quality gate daemon: %w", err)
		}
	}

	// Start review results monitoring goroutine
	if d.reviewDispatcher != nil {
		d.eg.Go(func() error { d.monitorReviewResults(); return nil })
	}

	return nil
}

// monitorReviewResults drains the review dispatcher's results channel and
// records each result in the usefulness tracker. Runs until the channel is
// closed (by ReviewDispatcher.Close during shutdown).
func (d *Daemon) monitorReviewResults() {
	for result := range d.reviewDispatcher.Results() {
		d.log(LogLevelInfo, "review_result_received request=%s model=%s status=%s findings=%d",
			result.RequestID, result.ReviewerModel, result.Status, len(result.Findings))

		if d.usefulnessTracker == nil {
			continue
		}

		// Extract taskID from the requestID format "review-{taskID}-{nanoTimestamp}"
		taskID := extractTaskIDFromRequestID(result.RequestID)

		d.reviewReqMu.Lock()
		info, ok := d.reviewRequests[taskID]
		if ok {
			delete(d.reviewRequests, taskID)
		}
		d.reviewReqMu.Unlock()

		if !ok {
			d.log(LogLevelWarn, "review_result_orphaned request=%s task=%s (no matching dispatch record)",
				result.RequestID, taskID)
			continue
		}

		// Convert to tracker format
		trackerResult := reviewer.ReviewResult{
			ReviewerModel: result.ReviewerModel,
			TaskID:        info.taskID,
			CommandID:     info.commandID,
		}
		for _, f := range result.Findings {
			trackerResult.FindingIDs = append(trackerResult.FindingIDs, f.FilePath+":"+f.Message)
		}

		if err := d.usefulnessTracker.RecordResult(trackerResult, nil); err != nil {
			d.log(LogLevelWarn, "usefulness_record_failed request=%s error=%v", result.RequestID, err)
		}
	}
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
	}
}

// initPhaseC initializes Phase C components: evolution engine, bandit selector,
// ensemble verifier, search tree, fingerprint DB, complexity scorer, and feature evaluator.
// Each component is conditionally initialized based on its config EffectiveEnabled() flag.
func (d *Daemon) initPhaseC() {
	cfg := d.config

	// C-1 Evolution Engine
	if cfg.Evolution.EffectiveEnabled() {
		strategies := make([]evolution.Strategy, 0, len(cfg.Evolution.EffectiveStrategies()))
		for _, s := range cfg.Evolution.EffectiveStrategies() {
			strategies = append(strategies, evolution.Strategy(s))
		}
		d.evolutionEngine = evolution.NewEngine(strategies, cfg.Evolution.EffectiveNoveltyThreshold())
		d.log(LogLevelInfo, "evolution engine initialized")
	}

	// C-2 Adaptive Model Selection (§4.5.1: Daemon集約, LLMトークン消費ゼロ)
	if cfg.Bandit.EffectiveEnabled() {
		d.banditSelector = bandit.NewSelector(cfg.Bandit.EffectiveExplorationCoeff())
		for _, model := range d.getAvailableModels() {
			d.banditSelector.AddArm(model)
		}
		d.log(LogLevelInfo, "bandit selector initialized arms=%d", len(d.banditSelector.GetStats()))
	}

	// C-3 Extended Verification
	if cfg.ExtendedVerification.EffectiveEnabled() {
		d.ensembleVerifier = verification.NewVerifier()
		for _, p := range d.ensembleVerifier.DefaultPerspectives() {
			d.ensembleVerifier.AddPerspective(p)
		}
		d.log(LogLevelInfo, "ensemble verifier initialized")
	}

	// C-4 Exploratory Search (§4.5.1: Daemon集約, MCTS木管理)
	if cfg.Search.EffectiveEnabled() {
		d.searchTree = search.NewTree(
			cfg.Search.EffectiveMaxDepth(),
			cfg.Search.EffectiveMaxBranching(),
			cfg.Search.EffectivePruneThreshold(),
		)
		d.searchSampler = search.NewSampler(
			cfg.Search.EffectiveThompsonAlpha(),
			cfg.Search.EffectiveThompsonBeta(),
		)
		d.log(LogLevelInfo, "search tree initialized")
	}

	// C-5 FingerprintDB (Self-Improvement)
	if cfg.SelfImprovement.EffectiveEnabled() {
		d.fingerprintDB = learnings.NewFingerprintDB(cfg.SelfImprovement.EffectiveArchiveMaxSize())
		d.log(LogLevelInfo, "fingerprint DB initialized")
	}

	// C-6 Complexity Scorer (§4.5.1: Daemon前処理)
	if cfg.Complexity.EffectiveEnabled() {
		d.complexityScorer = complexity.NewScorer(complexity.DefaultThresholds())
		d.log(LogLevelInfo, "complexity scorer initialized")
	}

	// C-8 Feature Gate (§4.5.1: Daemon集約, 機械的判定)
	d.featureEvaluator = featuregate.NewEvaluator()
	if len(cfg.FeatureProfiles) > 0 {
		profiles := make(map[string]map[string]interface{}, len(cfg.FeatureProfiles))
		for level, fp := range cfg.FeatureProfiles {
			profiles[level] = map[string]interface{}{
				"cross_agent_review":       fp.EffectiveCrossAgentReview() != "false",
				"exploratory_optimization": fp.EffectiveExploratoryOptimization(),
				"evolutionary_quality":     fp.EffectiveEvolutionaryQuality(),
				"adaptive_model_selection": fp.EffectiveAdaptiveModelSelection(),
				"self_improvement":         fp.EffectiveSelfImprovement(),
				"adaptive_depth":           fp.EffectiveAdaptiveDepth(),
			}
		}
		d.featureEvaluator.LoadProfiles(profiles)
		d.log(LogLevelInfo, "feature evaluator initialized with %d config profiles", len(cfg.FeatureProfiles))
	} else {
		d.log(LogLevelInfo, "feature evaluator initialized with default profiles")
	}
}

// getAvailableModels returns the list of model names from worker config.
func (d *Daemon) getAvailableModels() []string {
	wc := d.config.Agents.Workers
	models := make([]string, 0)
	if wc.DefaultModel != "" {
		models = append(models, wc.DefaultModel)
	}
	for name := range wc.Models {
		if name != wc.DefaultModel {
			models = append(models, name)
		}
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
