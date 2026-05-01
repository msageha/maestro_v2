package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/envelope"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// maxBackoffDuration is the upper bound for exponential backoff delays.
const maxBackoffDuration = 30 * time.Second

// sleepWithContext sleeps for the given duration or returns ctx.Err() if the
// context is canceled before the duration elapses.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Dispatcher handles priority sorting, agent_executor dispatch, quality gate
// evaluation, worktree path resolution, and event publication.
// mu protects eventBus, qualityGate, worktreeManager, gateEvaluator,
// taskAliveChecker.
type Dispatcher struct {
	maestroDir   string
	config       model.Config
	dl           *core.DaemonLogger
	logger       *log.Logger
	logLevel     core.LogLevel
	clock        core.Clock
	execProvider ExecutorGetter
	mu           sync.RWMutex

	eventBus         *events.Bus
	qualityGate      GateChecker
	gateEvaluator    PreTaskGateEvaluator
	worktreeManager  WorktreeResolver
	taskAliveChecker TaskAliveChecker
}

// New creates a new Dispatcher.
func New(maestroDir string, cfg model.Config, logger *log.Logger, logLevel core.LogLevel, ep ExecutorGetter, clock core.Clock) *Dispatcher {
	dl := core.NewDaemonLoggerFromLegacy("dispatcher", logger, logLevel)
	disp := &Dispatcher{
		maestroDir:   maestroDir,
		config:       cfg,
		dl:           dl,
		logger:       logger,
		logLevel:     logLevel,
		clock:        clock,
		execProvider: ep,
	}
	disp.gateEvaluator = NewQualityGateEvaluator(cfg, clock, dl, disp.getQualityGate)
	return disp
}

// SetEventBus sets the event bus for publishing events.
func (disp *Dispatcher) SetEventBus(bus *events.Bus) {
	disp.mu.Lock()
	defer disp.mu.Unlock()
	disp.eventBus = bus
}

// SetQualityGate sets the quality gate checker for the dispatcher.
func (disp *Dispatcher) SetQualityGate(qg GateChecker) {
	disp.mu.Lock()
	defer disp.mu.Unlock()
	disp.qualityGate = qg
}

// SetWorktreeManager wires the worktree resolver for worker path resolution during dispatch.
func (disp *Dispatcher) SetWorktreeManager(wm WorktreeResolver) {
	disp.mu.Lock()
	defer disp.mu.Unlock()
	disp.worktreeManager = wm
}

// SetTaskAliveChecker wires the queue-state probe the inline retry loop
// consults before each retry attempt. Late-bound from daemon startup
// because the QueueHandler that backs the checker is created after the
// dispatcher.
func (disp *Dispatcher) SetTaskAliveChecker(c TaskAliveChecker) {
	disp.mu.Lock()
	defer disp.mu.Unlock()
	disp.taskAliveChecker = c
}

func (disp *Dispatcher) getTaskAliveChecker() TaskAliveChecker {
	disp.mu.RLock()
	defer disp.mu.RUnlock()
	return disp.taskAliveChecker
}

// getEventBus returns the event bus with proper synchronization.
// May return nil if SetEventBus has not been called yet.
func (disp *Dispatcher) getEventBus() *events.Bus {
	disp.mu.RLock()
	defer disp.mu.RUnlock()
	return disp.eventBus
}

// publishEvent publishes an event to the event bus if available.
// Safe to call when eventBus is nil (no-op).
func (disp *Dispatcher) publishEvent(eventType events.EventType, data map[string]interface{}) {
	if bus := disp.getEventBus(); bus != nil {
		bus.Publish(eventType, data)
	}
}

// getQualityGate returns the quality gate checker with proper synchronization.
func (disp *Dispatcher) getQualityGate() GateChecker {
	disp.mu.RLock()
	defer disp.mu.RUnlock()
	return disp.qualityGate
}

// getWorktreeManager returns the worktree resolver with proper synchronization.
func (disp *Dispatcher) getWorktreeManager() WorktreeResolver {
	disp.mu.RLock()
	defer disp.mu.RUnlock()
	return disp.worktreeManager
}

// SortPendingTasks sorts tasks by effective_priority ASC → created_at ASC → id ASC.
func (disp *Dispatcher) SortPendingTasks(tasks []model.Task) []int {
	return SortPendingIndices(tasks, func(t model.Task) SortKey {
		return SortKey{Status: t.Status, Priority: t.Priority, CreatedAt: t.CreatedAt, ID: t.ID}
	}, disp.config.Queue.PriorityAgingSec)
}

// SortPendingCommands sorts commands by effective_priority ASC → created_at ASC → id ASC.
func (disp *Dispatcher) SortPendingCommands(commands []model.Command) []int {
	return SortPendingIndices(commands, func(c model.Command) SortKey {
		return SortKey{Status: c.Status, Priority: c.Priority, CreatedAt: c.CreatedAt, ID: c.ID}
	}, disp.config.Queue.PriorityAgingSec)
}

// SortPendingNotifications sorts notifications by effective_priority ASC → created_at ASC → id ASC.
func (disp *Dispatcher) SortPendingNotifications(notifications []model.Notification) []int {
	return SortPendingIndices(notifications, func(n model.Notification) SortKey {
		return SortKey{Status: n.Status, Priority: n.Priority, CreatedAt: n.CreatedAt, ID: n.ID}
	}, disp.config.Queue.PriorityAgingSec)
}

// executeDispatch obtains an executor and runs the given request.
// On failure it logs a structured error with the provided label and entity ID.
// On success it logs a structured info message.
//
// Returns (err, retryable). When retryable is false, the caller MUST NOT retry
// (Bug L: e.g. SetStatus failure after successful delivery — re-sending would
// double-submit the same envelope to the planner/worker). Failures from
// executor creation are treated as retryable since they predate any send.
func (disp *Dispatcher) executeDispatch(req agent.ExecRequest, logLabel, entityID, logExtra string) (retryable bool, err error) {
	exec, err := disp.execProvider.GetExecutor()
	if err != nil {
		return true, fmt.Errorf("create executor: %w", err)
	}
	result := exec.Execute(req)
	if result.Error != nil {
		// 2026-04-30 e2e regression: ErrSubmitConfirmUncertain is a
		// false-negative-prone signal — the paste already landed and the
		// downstream agent will start processing, but the post-paste
		// probe failed to see a Claude UI marker within its 6s budget.
		// Logging at ERROR pollutes the operator dashboard with
		// "dispatch_*_failed" noise that critical-alert channels treat
		// as a real failure even though the user observation was that
		// the dispatch ultimately succeeded. Demote to WARN so the
		// signal is visible but does not page; the
		// dispatch_uncertain_assume_running path that runs immediately
		// after in queue_scan_apply.go is the canonical operator-facing
		// breadcrumb for this case.
		level := core.LogLevelError
		if errors.Is(result.Error, agent.ErrSubmitConfirmUncertain) {
			level = core.LogLevelWarn
		}
		disp.dl.Logf(level, "dispatch_%s_failed id=%s%s error=%v retryable=%v",
			logLabel, entityID, logExtra, result.Error, result.Retryable)
		return result.Retryable, result.Error
	}
	disp.dl.Logf(core.LogLevelInfo, "dispatch_%s_success id=%s%s epoch=%d",
		logLabel, entityID, logExtra, req.LeaseEpoch)
	return false, nil
}

// DispatchCommand dispatches a command to the planner agent with inline retry.
// On transient failure, retries up to CommandDispatchInlineRetries times with
// exponential backoff to avoid the full scan-cycle delay.
func (disp *Dispatcher) DispatchCommand(ctx context.Context, cmd *model.Command) error {
	// Build enriched command content (planner skills injection)
	dispatchCmd := *cmd
	dispatchID, err := model.GenerateID(model.IDTypeDispatch)
	if err != nil {
		return fmt.Errorf("generate dispatch_id for command %s: %w", cmd.ID, err)
	}
	dispatchCmd.DispatchID = dispatchID
	enrichedContent, err := disp.BuildCommandContent(cmd)
	if err != nil {
		return fmt.Errorf("build command envelope for %s: %w", cmd.ID, err)
	}

	req := agent.ExecRequest{
		AgentID:    "planner",
		Message:    envelope.BuildPlannerEnvelope(dispatchCmd, enrichedContent, cmd.LeaseEpoch, cmd.Attempts),
		Mode:       agent.ModeDeliver,
		CommandID:  cmd.ID,
		LeaseEpoch: cmd.LeaseEpoch,
		Attempt:    cmd.Attempts,
	}

	maxRetries := disp.config.Retry.EffectiveCommandDispatchInlineRetries()
	retryDelay := time.Duration(disp.config.Retry.EffectiveCommandDispatchInlineRetryDelaySec()) * time.Second

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			disp.dl.Logf(core.LogLevelInfo, "command_dispatch_inline_retry attempt=%d/%d id=%s error=%v",
				attempt+1, maxRetries+1, cmd.ID, lastErr)
			if err := sleepWithContext(ctx, retryDelay); err != nil {
				return ctx.Err()
			}
			retryDelay = retryDelay * 2 // exponential backoff
			if retryDelay > maxBackoffDuration {
				retryDelay = maxBackoffDuration
			}
		}
		retryable, err := disp.executeDispatch(req, "command", cmd.ID, "")
		if err == nil {
			if attempt > 0 {
				disp.dl.Logf(core.LogLevelInfo, "command_dispatch_retry_success id=%s total_attempts=%d", cmd.ID, attempt+1)
			}
			return nil
		}
		lastErr = err
		// Bug L: non-retryable failures must not be retried — the prior send may
		// have already succeeded (e.g. SetStatus error after delivery), so a
		// retry would re-deliver the envelope and trigger duplicate plan submit.
		if !retryable {
			disp.dl.Logf(core.LogLevelWarn, "command_dispatch_non_retryable id=%s attempt=%d error=%v",
				cmd.ID, attempt+1, err)
			return lastErr
		}
	}
	return lastErr
}

// DispatchTask dispatches a task to a worker agent.
func (disp *Dispatcher) DispatchTask(ctx context.Context, task *model.Task, workerID string) error {
	// §S0-1 / run_on_main hardening: defense-in-depth pre-flight check that
	// rejects destructive shell snippets in tasks targeting the main branch or
	// integration worktree. The Bash policy hook (internal/agent/policy_checker)
	// covers Claude Code workers, but Codex/Gemini bypass that hook entirely;
	// this check applies to every worker regardless of agent type.
	if err := validateRunOnMainContent(task); err != nil {
		disp.dl.Logf(core.LogLevelError,
			"dispatch_task_destructive_content_blocked id=%s worker=%s run_on_main=%t run_on_integration=%t error=%v",
			task.ID, workerID, task.RunOnMain, task.RunOnIntegration, err)
		return err
	}

	// run_on_main timing observation (2026-05-01 dispatch-loop fix): the
	// previous implementation rejected RunOnMain dispatches that arrived
	// before integration→base publish completed (ErrRunOnMainBeforePublish).
	// In practice that gate produced a self-deadlocking loop:
	//
	//   • run_on_main tasks need publish to finish before they dispatch
	//   • publish needs every phase task to terminate before it runs
	//   • a phase that contains the run_on_main task therefore never
	//     completes, publish never runs, the gate keeps rejecting, and
	//     the planner re-queues forever (epoch 1..N forever).
	//
	// The 2026-05-01 user reproduction (cmd_1777610280_4dd0b6014ace44da)
	// looped through 7 epochs before the operator killed it. Per the
	// "self-healing autonomous orchestration" design contract, a defense
	// that locks the system harder than the failure it tries to prevent
	// is a regression — drop the gate and let the worker handle stale
	// main itself (a `git fetch + git checkout main` at task start, or a
	// circuit-breaker-aware retry if the verification needs newer base
	// state). Logged as INFO so operators can still spot the pattern.
	if task != nil && task.RunOnMain {
		disp.dl.Logf(core.LogLevelInfo,
			"dispatch_run_on_main_pre_publish_observation id=%s worker=%s command=%s "+
				"(no longer blocking dispatch; worker is responsible for refreshing main if needed)",
			task.ID, workerID, task.CommandID)
	}

	if err := disp.evaluateTaskQualityGate(task, workerID); err != nil {
		return err
	}

	// Build enriched task content (persona, skills, learnings injection)
	dispatchTask := *task
	dispatchID, err := model.GenerateID(model.IDTypeDispatch)
	if err != nil {
		return fmt.Errorf("generate dispatch_id for task %s: %w", task.ID, err)
	}
	dispatchTask.DispatchID = dispatchID
	enrichedContent, err := disp.BuildTaskContent(task)
	if err != nil {
		return fmt.Errorf("build task envelope for %s: %w", task.ID, err)
	}

	workingDir, err := disp.resolveTaskWorkingDir(task, workerID)
	if err != nil {
		return err
	}

	env := envelope.BuildWorkerEnvelope(dispatchTask, enrichedContent, workerID, task.LeaseEpoch, task.Attempts)
	if workingDir != "" {
		env = fmt.Sprintf("%s\nworking_dir: %s", env, workingDir)
	}

	req := agent.ExecRequest{
		AgentID:    workerID,
		Message:    env,
		Mode:       agent.ModeWithClear,
		TaskID:     task.ID,
		CommandID:  task.CommandID,
		LeaseEpoch: task.LeaseEpoch,
		Attempt:    task.Attempts,
		WorkingDir: workingDir,
		RunOnMain:  task.RunOnMain,
	}

	maxRetries := disp.config.Retry.EffectiveTaskDispatchInlineRetries()
	retryDelay := time.Duration(disp.config.Retry.EffectiveTaskDispatchInlineRetryDelaySec()) * time.Second

	var lastErr error
	checker := disp.getTaskAliveChecker()
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Before sleeping for the backoff, short-circuit if the queue
			// entry is no longer this dispatcher's to drive: a worker that
			// completed in parallel, a fencing-bumped lease epoch, or a
			// queue entry that was removed entirely all mean further
			// paste-Enter waves would corrupt state. checker == nil
			// preserves legacy behaviour for tests that don't wire one.
			if checker != nil && !checker.IsDispatchActive(workerID, task.ID, task.LeaseEpoch) {
				disp.dl.Logf(core.LogLevelInfo,
					"task_dispatch_retry_aborted_terminal id=%s worker=%s attempt=%d/%d "+
						"(queue entry no longer in_progress at expected lease epoch — worker likely finished or was preempted)",
					task.ID, workerID, attempt, maxRetries+1)
				return lastErr
			}
			disp.dl.Logf(core.LogLevelInfo, "task_dispatch_inline_retry attempt=%d/%d id=%s worker=%s error=%v",
				attempt+1, maxRetries+1, task.ID, workerID, lastErr)
			if err := sleepWithContext(ctx, retryDelay); err != nil {
				return ctx.Err()
			}
			// Re-check after the sleep: a worker can complete during the
			// backoff window itself, especially when retryDelay has grown
			// to maxBackoffDuration.
			if checker != nil && !checker.IsDispatchActive(workerID, task.ID, task.LeaseEpoch) {
				disp.dl.Logf(core.LogLevelInfo,
					"task_dispatch_retry_aborted_terminal_post_sleep id=%s worker=%s attempt=%d/%d",
					task.ID, workerID, attempt, maxRetries+1)
				return lastErr
			}
			retryDelay = retryDelay * 2 // exponential backoff
			if retryDelay > maxBackoffDuration {
				retryDelay = maxBackoffDuration
			}
		}
		retryable, err := disp.executeDispatch(req, "task", task.ID, fmt.Sprintf(" worker=%s", workerID))
		if err == nil {
			if attempt > 0 {
				disp.dl.Logf(core.LogLevelInfo, "task_dispatch_retry_success id=%s worker=%s total_attempts=%d",
					task.ID, workerID, attempt+1)
			}

			disp.publishEvent(events.EventTaskStarted, map[string]interface{}{
				"task_id":    task.ID,
				"command_id": task.CommandID,
				"worker_id":  workerID,
				"epoch":      task.LeaseEpoch,
			})

			return nil
		}
		lastErr = err
		// Bug L: same non-retryable abort semantics as DispatchCommand.
		if !retryable {
			disp.dl.Logf(core.LogLevelWarn, "task_dispatch_non_retryable id=%s worker=%s attempt=%d error=%v",
				task.ID, workerID, attempt+1, err)
			return lastErr
		}
	}
	return lastErr
}

// evaluateTaskQualityGate runs the pre-task quality gate check and records
// the evaluation result. Returns an error only when enforcement is "block".
func (disp *Dispatcher) evaluateTaskQualityGate(task *model.Task, workerID string) error {
	if disp.gateEvaluator.ShouldEvaluate() && disp.config.QualityGates.Enforcement.PreTaskCheck {
		gateEvaluation, err := disp.gateEvaluator.EvaluatePreTask(task, workerID)
		if err != nil {
			if disp.config.QualityGates.Enforcement.FailureAction == "block" {
				disp.dl.Logf(core.LogLevelError, "dispatch_task_blocked_by_quality_gate id=%s worker=%s error=%v",
					task.ID, workerID, err)
				disp.gateEvaluator.StoreEvaluation(task.ID, gateEvaluation)
				return fmt.Errorf("quality gate check failed: %w", err)
			}
			disp.dl.Logf(core.LogLevelWarn, "dispatch_task_quality_gate_violation id=%s worker=%s error=%v",
				task.ID, workerID, err)
		}
		disp.gateEvaluator.StoreEvaluation(task.ID, gateEvaluation)
	} else if !disp.config.QualityGates.Enabled {
		disp.gateEvaluator.StoreEvaluation(task.ID, disp.gateEvaluator.SkippedEvaluation("disabled"))
	} else if disp.config.QualityGates.SkipGates {
		disp.gateEvaluator.StoreEvaluation(task.ID, disp.gateEvaluator.SkippedEvaluation("emergency_mode"))
	}
	return nil
}

// resolveTaskWorkingDir resolves the working directory for a task, creating
// the worktree lazily if needed. Returns empty string when worktree mode is
// not active. Returns the project root for RunOnMain tasks.
func (disp *Dispatcher) resolveTaskWorkingDir(task *model.Task, workerID string) (string, error) {
	// RunOnMain tasks (e.g. final verification) must run against the merged
	// state on the main branch, not inside a worker worktree.
	// Return the project root explicitly — "" means "no change" in ensureWorkingDir
	// and would leave the worker in a (possibly deleted) worktree cwd.
	if task.RunOnMain {
		return filepath.Dir(disp.maestroDir), nil
	}

	// RunOnIntegration tasks (e.g. publish_conflict resolution) must operate
	// directly on the integration worktree so that forward-merge conflicts can
	// be resolved on the integration branch before retry-publish.
	if task.RunOnIntegration {
		wm := disp.getWorktreeManager()
		if wm == nil {
			return "", fmt.Errorf("RunOnIntegration task requires worktree mode")
		}
		// Defense-in-depth: verify the integration worktree is on the expected
		// integration branch before dispatching. A detached or drifted HEAD
		// would cause `git merge main` in the agent to create orphan commits
		// that publish would later discard, producing a publish_conflict loop.
		if err := wm.EnsureIntegrationBranchCheckedOut(task.CommandID); err != nil {
			disp.dl.Logf(core.LogLevelError,
				"integration_branch_check_failed task=%s command=%s error=%v",
				task.ID, task.CommandID, err)
			return "", fmt.Errorf("integration branch checkout verification failed: %w", err)
		}
		intPath, err := wm.GetIntegrationPath(task.CommandID)
		if err != nil {
			disp.dl.Logf(core.LogLevelError, "integration_path_resolve_failed task=%s command=%s error=%v",
				task.ID, task.CommandID, err)
			return "", fmt.Errorf("integration worktree path resolution failed: %w", err)
		}
		return intPath, nil
	}

	wm := disp.getWorktreeManager()
	if wm == nil {
		return "", nil
	}

	wtPath, err := wm.GetWorkerPath(task.CommandID, workerID)
	if err != nil {
		if createErr := wm.EnsureWorkerWorktree(task.CommandID, workerID); createErr != nil {
			disp.dl.Logf(core.LogLevelError, "worktree_create_failed task=%s worker=%s error=%v",
				task.ID, workerID, createErr)
			return "", fmt.Errorf("worktree path resolution failed: %w", createErr)
		}
		wtPath, err = wm.GetWorkerPath(task.CommandID, workerID)
	}
	if err != nil {
		disp.dl.Logf(core.LogLevelError, "worktree_path_resolve_failed task=%s worker=%s error=%v",
			task.ID, workerID, err)
		return "", fmt.Errorf("worktree path resolution failed: %w", err)
	}

	// Repair tasks (verify_repair / plan add-retry-task) operate on top of
	// the worker's pending changes from the original task. Auto-commit only
	// fires at Phase B merge time AFTER verify succeeds; for repair tasks
	// the original task's edits remain uncommitted in the worker worktree,
	// and refusing dispatch on dirty state would dead-letter every repair.
	// The repair task is precisely the operation that fixes the failure
	// the daemon detected, so running it on top of the worker's pending
	// state is the correct semantic — refresh would either lose those
	// edits or abort the dispatch. Skip the refresh here; the repair
	// inherits the dirty worktree by design.
	if task.OperationType != model.OperationTypeRepair {
		// Fast-forward the worker worktree to integration HEAD before dispatching.
		// A worker re-used across phases retains its branch tip from a prior merge:
		// without this refresh, sibling-worker commits already on integration are
		// invisible to the worker, causing tests/builds that exercise the merged
		// state to read stale code (the alpha/beta/test workflow regression).
		// Fail-closed: any error (dirty, diverged, git failure) refuses dispatch
		// rather than running a task against unknowingly-stale state.
		if refreshErr := wm.RefreshWorkerWorktreeToIntegrationHead(task.CommandID, workerID); refreshErr != nil {
			disp.dl.Logf(core.LogLevelError,
				"worktree_refresh_failed task=%s worker=%s error=%v",
				task.ID, workerID, refreshErr)
			return "", fmt.Errorf("worker worktree refresh failed: %w", refreshErr)
		}
	}
	return wtPath, nil
}

// DispatchNotification dispatches a notification to the orchestrator agent.
// The Retryable flag is intentionally discarded here because notification
// dispatch has no inline retry loop — outer scan-cycle retry handles
// transient failures, and re-delivery of an orchestrator notification is
// safe (orchestrator side is idempotent on notification ID).
func (disp *Dispatcher) DispatchNotification(ntf *model.Notification) error {
	env := envelope.BuildOrchestratorNotificationEnvelope(ntf.CommandID, ntf.Type)
	_, err := disp.executeDispatch(agent.ExecRequest{
		AgentID:    "orchestrator",
		Message:    env,
		Mode:       agent.ModeDeliver,
		CommandID:  ntf.CommandID,
		LeaseEpoch: ntf.LeaseEpoch,
		Attempt:    ntf.Attempts,
	}, "notification", ntf.ID, fmt.Sprintf(" type=%s", ntf.Type))
	return err
}
