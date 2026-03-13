package daemon

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// errExecutorInit is re-exported from core via core_aliases.go.

// maxGateEvaluations is the maximum number of gate evaluation entries kept in memory.
const maxGateEvaluations = 1000

// Dispatcher handles priority sorting and agent_executor dispatch.
type Dispatcher struct {
	maestroDir        string
	config            model.Config
	leaseManager      *LeaseManager
	dl                *DaemonLogger
	logger            *log.Logger
	logLevel          LogLevel
	clock             Clock
	execProvider      *ExecutorProvider
	envelopeBuilder   *EnvelopeBuilder
	mu                sync.RWMutex // protects eventBus, qualityGate, worktreeManager
	eventBus          *events.Bus
	qualityGate       *QualityGateDaemon
	gateEvaluations   map[string]*model.QualityGateEvaluation // task_id -> evaluation
	gateEvalMutex     sync.RWMutex

	worktreeManager *WorktreeManager
}

// ExecutorFactory, AgentExecutor are defined in internal/daemon/core
// and re-exported via core_aliases.go.

// NewDispatcher creates a new Dispatcher with a shared ExecutorProvider.
func NewDispatcher(maestroDir string, cfg model.Config, lm *LeaseManager, logger *log.Logger, logLevel LogLevel, ep *ExecutorProvider) *Dispatcher {
	dl := NewDaemonLoggerFromLegacy("dispatcher", logger, logLevel)
	return &Dispatcher{
		maestroDir:      maestroDir,
		config:          cfg,
		leaseManager:    lm,
		dl:              dl,
		logger:          logger,
		logLevel:        logLevel,
		clock:           ep.clock,
		gateEvaluations: make(map[string]*model.QualityGateEvaluation),
		execProvider:    ep,
		envelopeBuilder: NewEnvelopeBuilder(maestroDir, cfg, ep.clock, dl),
	}
}

// SetExecutorFactory overrides the executor factory for testing.
// Resets the cached executor so the new factory is used on next call.
func (disp *Dispatcher) SetExecutorFactory(f ExecutorFactory) {
	disp.execProvider.SetFactory(f)
}

// getExecutor returns the shared executor instance, creating it lazily on first call.
// On failure, the error is NOT cached — subsequent calls will retry with exponential
// backoff to avoid hammering a persistently broken resource.
// The Executor is safe for concurrent use (log.Logger uses internal mutex,
// os.File in append mode is POSIX-safe, all other fields are immutable).
//
// L-3: The exponential backoff (1s→2s→4s→8s→16s cap) and reset-on-success behavior
// is intentional for testing and production use. SetExecutorFactory resets the backoff
// state to allow immediate retry with a new factory, which is the expected behavior
// for test isolation. In production, the backoff prevents tight retry loops when the
// underlying resource (e.g., log file) is persistently broken.
func (disp *Dispatcher) getExecutor() (AgentExecutor, error) {
	return disp.execProvider.GetExecutor()
}

// CloseExecutor releases the shared executor's resources (log file handle).
// Safe to call multiple times; subsequent calls are no-ops.
func (disp *Dispatcher) CloseExecutor() {
	disp.execProvider.CloseExecutor()
}

// SetEventBus sets the event bus for publishing events.
func (disp *Dispatcher) SetEventBus(bus *events.Bus) {
	disp.mu.Lock()
	defer disp.mu.Unlock()
	disp.eventBus = bus
}

// SetQualityGate sets the quality gate daemon for the dispatcher.
func (disp *Dispatcher) SetQualityGate(qg *QualityGateDaemon) {
	disp.mu.Lock()
	defer disp.mu.Unlock()
	disp.qualityGate = qg
}

// SetWorktreeManager wires the worktree manager for worker path resolution during dispatch.
func (disp *Dispatcher) SetWorktreeManager(wm *WorktreeManager) {
	disp.mu.Lock()
	defer disp.mu.Unlock()
	disp.worktreeManager = wm
}

// getEventBus returns the event bus with proper synchronization.
func (disp *Dispatcher) getEventBus() *events.Bus {
	disp.mu.RLock()
	defer disp.mu.RUnlock()
	return disp.eventBus
}

// getQualityGate returns the quality gate daemon with proper synchronization.
func (disp *Dispatcher) getQualityGate() *QualityGateDaemon {
	disp.mu.RLock()
	defer disp.mu.RUnlock()
	return disp.qualityGate
}

// getWorktreeManager returns the worktree manager with proper synchronization.
func (disp *Dispatcher) getWorktreeManager() *WorktreeManager {
	disp.mu.RLock()
	defer disp.mu.RUnlock()
	return disp.worktreeManager
}

// sortableEntry wraps a queue entry with computed effective priority for sorting.
type sortableEntry struct {
	Index             int
	EffectivePriority int
	CreatedAt         time.Time
	ID                string
}

// sortKey holds the fields extracted from a queue entry for pending-sort filtering.
type sortKey struct {
	Status    model.Status
	Priority  int
	CreatedAt string
	ID        string
}

// EffectivePriority computes the aging-adjusted priority.
// effective_priority = max(0, priority - floor(age_seconds / priority_aging_sec))
//
// L-6: Uses integer duration arithmetic instead of float64 to avoid overflow on
// 32-bit systems and float rounding issues with extreme age values.
func EffectivePriority(priority int, createdAt string, priorityAgingSec int) int {
	if priorityAgingSec <= 0 || priority <= 0 {
		return max(priority, 0)
	}
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return priority
	}
	age := time.Since(created)
	if age <= 0 {
		// Future createdAt — no aging applied
		return priority
	}
	// Guard against Duration overflow for very large priorityAgingSec values.
	// time.Duration is int64 nanoseconds; max safe seconds ≈ 9.2e9 (~292 years).
	const maxSafeSec = 1<<53 - 1 // well within int64 nanosecond range
	if priorityAgingSec > maxSafeSec {
		return priority
	}
	interval := time.Duration(priorityAgingSec) * time.Second
	// Integer division: equivalent to floor(age_seconds / priorityAgingSec)
	aging := int64(age / interval)
	if aging >= int64(priority) {
		return 0
	}
	return priority - int(aging)
}

// sortPendingIndices filters pending items and returns their original indices
// sorted by effective_priority ASC → created_at ASC → id ASC.
func sortPendingIndices[T any](items []T, extract func(T) sortKey, priorityAgingSec int) []int {
	var entries []sortableEntry
	for i, item := range items {
		key := extract(item)
		if key.Status != model.StatusPending {
			continue
		}
		created, _ := time.Parse(time.RFC3339, key.CreatedAt)
		entries = append(entries, sortableEntry{
			Index:             i,
			EffectivePriority: EffectivePriority(key.Priority, key.CreatedAt, priorityAgingSec),
			CreatedAt:         created,
			ID:                key.ID,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].EffectivePriority != entries[j].EffectivePriority {
			return entries[i].EffectivePriority < entries[j].EffectivePriority
		}
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.Before(entries[j].CreatedAt)
		}
		return entries[i].ID < entries[j].ID
	})

	indices := make([]int, len(entries))
	for i, e := range entries {
		indices[i] = e.Index
	}
	return indices
}

// SortPendingTasks sorts tasks by effective_priority ASC → created_at ASC → id ASC.
func (disp *Dispatcher) SortPendingTasks(tasks []model.Task) []int {
	return sortPendingIndices(tasks, func(t model.Task) sortKey {
		return sortKey{Status: t.Status, Priority: t.Priority, CreatedAt: t.CreatedAt, ID: t.ID}
	}, disp.config.Queue.PriorityAgingSec)
}

// SortPendingCommands sorts commands by effective_priority ASC → created_at ASC → id ASC.
func (disp *Dispatcher) SortPendingCommands(commands []model.Command) []int {
	return sortPendingIndices(commands, func(c model.Command) sortKey {
		return sortKey{Status: c.Status, Priority: c.Priority, CreatedAt: c.CreatedAt, ID: c.ID}
	}, disp.config.Queue.PriorityAgingSec)
}

// SortPendingNotifications sorts notifications by effective_priority ASC → created_at ASC → id ASC.
func (disp *Dispatcher) SortPendingNotifications(notifications []model.Notification) []int {
	return sortPendingIndices(notifications, func(n model.Notification) sortKey {
		return sortKey{Status: n.Status, Priority: n.Priority, CreatedAt: n.CreatedAt, ID: n.ID}
	}, disp.config.Queue.PriorityAgingSec)
}

// DispatchCommand dispatches a command to the planner agent.
func (disp *Dispatcher) DispatchCommand(cmd *model.Command) error {
	exec, err := disp.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	envelope := agent.BuildPlannerEnvelope(*cmd, cmd.LeaseEpoch, cmd.Attempts)

	result := exec.Execute(agent.ExecRequest{
		AgentID:    "planner",
		Message:    envelope,
		Mode:       agent.ModeDeliver,
		CommandID:  cmd.ID,
		LeaseEpoch: cmd.LeaseEpoch,
		Attempt:    cmd.Attempts,
	})

	if result.Error != nil {
		disp.log(LogLevelError, "dispatch_command_failed id=%s error=%v retryable=%v",
			cmd.ID, result.Error, result.Retryable)
		return result.Error
	}

	disp.log(LogLevelInfo, "dispatch_command_success id=%s epoch=%d", cmd.ID, cmd.LeaseEpoch)
	return nil
}

// DispatchTask dispatches a task to a worker agent.
func (disp *Dispatcher) DispatchTask(task *model.Task, workerID string) error {
	// Pre-task quality gate check and record evaluation result
	var gateEvaluation *model.QualityGateEvaluation
	if disp.shouldEvaluateQualityGates() && disp.config.QualityGates.Enforcement.PreTaskCheck {
		evaluation, err := disp.evaluatePreTaskGateWithResult(task, workerID)
		gateEvaluation = evaluation

		if err != nil {
			if disp.config.QualityGates.Enforcement.FailureAction == "block" {
				disp.log(LogLevelError, "dispatch_task_blocked_by_quality_gate id=%s worker=%s error=%v",
					task.ID, workerID, err)
				// Store evaluation result for later recording
				disp.storeGateEvaluation(task.ID, gateEvaluation)
				return fmt.Errorf("quality gate check failed: %w", err)
			}
			// If action is "warn", just log the violation
			disp.log(LogLevelWarn, "dispatch_task_quality_gate_violation id=%s worker=%s error=%v",
				task.ID, workerID, err)
		}
		// Store evaluation result for later recording
		disp.storeGateEvaluation(task.ID, gateEvaluation)
	} else if !disp.config.QualityGates.Enabled {
		// Record that gates were skipped
		gateEvaluation = &model.QualityGateEvaluation{
			Passed:        true,
			SkippedReason: "disabled",
			EvaluatedAt:   disp.clock.Now().Format(time.RFC3339),
		}
		disp.storeGateEvaluation(task.ID, gateEvaluation)
	} else if disp.config.QualityGates.SkipGates {
		// Record that emergency mode was used
		gateEvaluation = &model.QualityGateEvaluation{
			Passed:        true,
			SkippedReason: "emergency_mode",
			EvaluatedAt:   disp.clock.Now().Format(time.RFC3339),
		}
		disp.storeGateEvaluation(task.ID, gateEvaluation)
	}

	exec, err := disp.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	// Build enriched task content (persona, skills, learnings injection)
	dispatchTask := *task
	enrichedContent, err := disp.envelopeBuilder.BuildTaskContent(task)
	if err != nil {
		return fmt.Errorf("build task envelope for %s: %w", task.ID, err)
	}
	dispatchTask.Content = enrichedContent

	// Resolve working_dir for worktree-enabled commands (lazy creation)
	var workingDir string
	wm := disp.getWorktreeManager()
	if wm != nil {
		wtPath, err := wm.GetWorkerPath(task.CommandID, workerID)
		if err != nil {
			// Worktree doesn't exist yet — lazily create for this worker
			if createErr := wm.EnsureWorkerWorktree(task.CommandID, workerID); createErr != nil {
				disp.log(LogLevelError, "worktree_create_failed task=%s worker=%s error=%v",
					task.ID, workerID, createErr)
				return fmt.Errorf("worktree path resolution failed: %w", createErr)
			}
			wtPath, err = wm.GetWorkerPath(task.CommandID, workerID)
		}
		if err != nil {
			disp.log(LogLevelError, "worktree_path_resolve_failed task=%s worker=%s error=%v",
				task.ID, workerID, err)
			return fmt.Errorf("worktree path resolution failed: %w", err)
		}
		workingDir = wtPath
	}

	envelope := agent.BuildWorkerEnvelope(dispatchTask, workerID, task.LeaseEpoch, task.Attempts)
	if workingDir != "" {
		envelope = fmt.Sprintf("%s\nworking_dir: %s", envelope, workingDir)
	}

	result := exec.Execute(agent.ExecRequest{
		AgentID:    workerID,
		Message:    envelope,
		Mode:       agent.ModeWithClear,
		TaskID:     task.ID,
		CommandID:  task.CommandID,
		LeaseEpoch: task.LeaseEpoch,
		Attempt:    task.Attempts,
		WorkingDir: workingDir,
	})

	if result.Error != nil {
		disp.log(LogLevelError, "dispatch_task_failed id=%s worker=%s error=%v retryable=%v",
			task.ID, workerID, result.Error, result.Retryable)
		return result.Error
	}

	disp.log(LogLevelInfo, "dispatch_task_success id=%s worker=%s epoch=%d",
		task.ID, workerID, task.LeaseEpoch)

	// Publish task_started event (non-blocking, best-effort)
	if bus := disp.getEventBus(); bus != nil {
		bus.Publish(events.EventTaskStarted, map[string]interface{}{
			"task_id":    task.ID,
			"command_id": task.CommandID,
			"worker_id":  workerID,
			"epoch":      task.LeaseEpoch,
		})
	}

	return nil
}

// DispatchNotification dispatches a notification to the orchestrator agent.
func (disp *Dispatcher) DispatchNotification(ntf *model.Notification) error {
	exec, err := disp.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	envelope := agent.BuildOrchestratorNotificationEnvelope(ntf.CommandID, ntf.Type)

	result := exec.Execute(agent.ExecRequest{
		AgentID:    "orchestrator",
		Message:    envelope,
		Mode:       agent.ModeDeliver,
		CommandID:  ntf.CommandID,
		LeaseEpoch: ntf.LeaseEpoch,
		Attempt:    ntf.Attempts,
	})

	if result.Error != nil {
		disp.log(LogLevelError, "dispatch_notification_failed id=%s error=%v retryable=%v",
			ntf.ID, result.Error, result.Retryable)
		return result.Error
	}

	disp.log(LogLevelInfo, "dispatch_notification_success id=%s type=%s epoch=%d",
		ntf.ID, ntf.Type, ntf.LeaseEpoch)
	return nil
}

func (disp *Dispatcher) log(level LogLevel, format string, args ...any) {
	disp.dl.Logf(level, format, args...)
}

// shouldEvaluateQualityGates determines if quality gates should be evaluated
func (disp *Dispatcher) shouldEvaluateQualityGates() bool {
	// Skip if quality gates are disabled
	if !disp.config.QualityGates.Enabled {
		return false
	}

	// Skip if emergency mode is enabled (--skip-gates)
	if disp.config.QualityGates.SkipGates {
		disp.log(LogLevelInfo, "quality_gates_skipped reason=emergency_mode")
		return false
	}

	// Skip if quality gate daemon is not available
	if disp.getQualityGate() == nil {
		disp.log(LogLevelDebug, "quality_gates_skipped reason=daemon_not_available")
		return false
	}

	return true
}

// evaluatePreTaskGateWithResult evaluates quality gates before task execution and returns the result.
//
// L-7: There is a TOCTOU window between this pre-task gate check and actual task dispatch.
// A gate condition could change between evaluation and dispatch. This is acceptable because:
//   - Phase C's epoch fencing is the authoritative guard against stale dispatches.
//   - The pre-task gate check is a best-effort early filter (advisory), not a guarantee.
//   - Making the check and dispatch fully atomic would require holding scanMu across the
//     entire dispatch path, which would serialize all dispatches and hurt throughput.
//
// The three-phase design (Phase A: scan, Phase B: dispatch, Phase C: commit with fencing)
// ensures correctness even when pre-task checks are slightly stale.
func (disp *Dispatcher) evaluatePreTaskGateWithResult(task *model.Task, workerID string) (*model.QualityGateEvaluation, error) {
	qg := disp.getQualityGate()
	if qg == nil {
		return nil, nil
	}

	// NOTE: We do NOT emit TaskStartEvent here. The EventBus publish in
	// DispatchTask (after successful dispatch) triggers the subscription
	// in subscribeQualityGateEvents, which forwards the event to
	// QualityGateDaemon. Emitting here would cause duplicate delivery.

	// Perform synchronous evaluation
	context := map[string]interface{}{
		"task_id":    task.ID,
		"command_id": task.CommandID,
		"agent_id":   workerID,
		"priority":   task.Priority,
		"attempts":   task.Attempts,
	}

	// Use the evaluateGateWithResult method for synchronous evaluation
	result, err := qg.evaluateGateWithResult("pre_task", context)

	// Convert to model.QualityGateEvaluation
	evaluation := &model.QualityGateEvaluation{
		Passed:      result != nil && result.Passed,
		EvaluatedAt: disp.clock.Now().Format(time.RFC3339),
	}

	if result != nil {
		evaluation.Action = string(result.Action)
		if len(result.FailedGates) > 0 {
			evaluation.FailedGates = make([]string, len(result.FailedGates))
			for i, gate := range result.FailedGates {
				evaluation.FailedGates[i] = gate
			}
		}
	}

	if err != nil {
		return evaluation, fmt.Errorf("evaluation failed: %w", err)
	}

	if result == nil {
		return evaluation, fmt.Errorf("evaluation returned nil result")
	}

	if !result.Passed {
		return evaluation, fmt.Errorf("quality gate check failed: %v", result.FailedGates)
	}

	return evaluation, nil
}

// storeGateEvaluation stores the gate evaluation for a task.
// Evicts oldest entries when the map exceeds maxGateEvaluations.
func (disp *Dispatcher) storeGateEvaluation(taskID string, evaluation *model.QualityGateEvaluation) {
	if evaluation == nil {
		return
	}

	disp.gateEvalMutex.Lock()
	defer disp.gateEvalMutex.Unlock()
	disp.gateEvaluations[taskID] = evaluation

	if len(disp.gateEvaluations) > maxGateEvaluations {
		disp.evictOldGateEvaluationsLocked()
	}
}

// evictOldGateEvaluationsLocked removes the oldest gate evaluation entries to bring
// the map back to maxGateEvaluations/2. Caller must hold gateEvalMutex.
func (disp *Dispatcher) evictOldGateEvaluationsLocked() {
	type entry struct {
		taskID      string
		evaluatedAt time.Time
	}
	entries := make([]entry, 0, len(disp.gateEvaluations))
	for id, eval := range disp.gateEvaluations {
		t, err := time.Parse(time.RFC3339, eval.EvaluatedAt)
		if err != nil {
			// Malformed timestamp — evict immediately
			t = time.Time{}
		}
		entries = append(entries, entry{taskID: id, evaluatedAt: t})
	}

	// Sort oldest first
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].evaluatedAt.Before(entries[j].evaluatedAt)
	})

	// Remove oldest entries until we reach half the cap
	target := maxGateEvaluations / 2
	for i := 0; i < len(entries) && len(disp.gateEvaluations) > target; i++ {
		delete(disp.gateEvaluations, entries[i].taskID)
	}
}
