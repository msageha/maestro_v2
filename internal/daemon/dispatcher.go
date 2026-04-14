package daemon

import (
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/envelope"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// errExecutorInit is re-exported from core via core_aliases.go.

// Dispatcher handles priority sorting, agent_executor dispatch, quality gate
// evaluation, worktree path resolution, and event publication.
// baseHandler.mu protects eventBus, qualityGate, worktreeManager.
type Dispatcher struct {
	baseHandler
	leaseManager    QueueLeaseManager
	eventBus        *events.Bus
	qualityGate     *QualityGateDaemon
	gateEvaluator   *QualityGateEvaluator
	worktreeManager *WorktreeManager
}

// ExecutorFactory, AgentExecutor are defined in internal/daemon/core
// and re-exported via core_aliases.go.

// NewDispatcher creates a new Dispatcher.
func NewDispatcher(maestroDir string, cfg model.Config, lm QueueLeaseManager, logger *log.Logger, logLevel LogLevel, ep ExecutorGetter, clock Clock) *Dispatcher {
	dl := NewDaemonLoggerFromLegacy("dispatcher", logger, logLevel)
	disp := &Dispatcher{
		baseHandler: baseHandler{
			maestroDir:   maestroDir,
			config:       cfg,
			dl:           dl,
			logger:       logger,
			logLevel:     logLevel,
			clock:        clock,
			execProvider: ep,
		},
		leaseManager: lm,
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
	// Why: 2^53-1 is the largest integer exactly representable in float64,
	// ensuring no precision loss when converting to time.Duration nanoseconds.
	const maxSafeSec = 1<<53 - 1
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
	entries := make([]sortableEntry, 0, len(items))
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

	// Build enriched command content (planner skills injection)
	dispatchCmd := *cmd
	enrichedContent, err := disp.BuildCommandContent(cmd)
	if err != nil {
		return fmt.Errorf("build command envelope for %s: %w", cmd.ID, err)
	}
	dispatchCmd.Content = enrichedContent

	envelope := envelope.BuildPlannerEnvelope(dispatchCmd, cmd.LeaseEpoch, cmd.Attempts)

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
	if err := disp.evaluateTaskQualityGate(task, workerID); err != nil {
		return err
	}

	exec, err := disp.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	// Build enriched task content (persona, skills, learnings injection)
	dispatchTask := *task
	enrichedContent, err := disp.BuildTaskContent(task)
	if err != nil {
		return fmt.Errorf("build task envelope for %s: %w", task.ID, err)
	}
	dispatchTask.Content = enrichedContent

	workingDir, err := disp.resolveTaskWorkingDir(task, workerID)
	if err != nil {
		return err
	}

	env := envelope.BuildWorkerEnvelope(dispatchTask, workerID, task.LeaseEpoch, task.Attempts)
	if workingDir != "" {
		env = fmt.Sprintf("%s\nworking_dir: %s", env, workingDir)
	}

	result := exec.Execute(agent.ExecRequest{
		AgentID:    workerID,
		Message:    env,
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

	disp.publishEvent(events.EventTaskStarted, map[string]interface{}{
		"task_id":    task.ID,
		"command_id": task.CommandID,
		"worker_id":  workerID,
		"epoch":      task.LeaseEpoch,
	})

	return nil
}

// evaluateTaskQualityGate runs the pre-task quality gate check and records
// the evaluation result. Returns an error only when enforcement is "block".
func (disp *Dispatcher) evaluateTaskQualityGate(task *model.Task, workerID string) error {
	if disp.gateEvaluator.ShouldEvaluate() && disp.config.QualityGates.Enforcement.PreTaskCheck {
		gateEvaluation, err := disp.gateEvaluator.EvaluatePreTask(task, workerID)
		if err != nil {
			if disp.config.QualityGates.Enforcement.FailureAction == "block" {
				disp.log(LogLevelError, "dispatch_task_blocked_by_quality_gate id=%s worker=%s error=%v",
					task.ID, workerID, err)
				disp.gateEvaluator.StoreEvaluation(task.ID, gateEvaluation)
				return fmt.Errorf("quality gate check failed: %w", err)
			}
			disp.log(LogLevelWarn, "dispatch_task_quality_gate_violation id=%s worker=%s error=%v",
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
// not active.
func (disp *Dispatcher) resolveTaskWorkingDir(task *model.Task, workerID string) (string, error) {
	wm := disp.getWorktreeManager()
	if wm == nil {
		return "", nil
	}

	wtPath, err := wm.GetWorkerPath(task.CommandID, workerID)
	if err != nil {
		if createErr := wm.EnsureWorkerWorktree(task.CommandID, workerID); createErr != nil {
			disp.log(LogLevelError, "worktree_create_failed task=%s worker=%s error=%v",
				task.ID, workerID, createErr)
			return "", fmt.Errorf("worktree path resolution failed: %w", createErr)
		}
		wtPath, err = wm.GetWorkerPath(task.CommandID, workerID)
	}
	if err != nil {
		disp.log(LogLevelError, "worktree_path_resolve_failed task=%s worker=%s error=%v",
			task.ID, workerID, err)
		return "", fmt.Errorf("worktree path resolution failed: %w", err)
	}
	return wtPath, nil
}

// DispatchNotification dispatches a notification to the orchestrator agent.
func (disp *Dispatcher) DispatchNotification(ntf *model.Notification) error {
	exec, err := disp.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	envelope := envelope.BuildOrchestratorNotificationEnvelope(ntf.CommandID, ntf.Type)

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

