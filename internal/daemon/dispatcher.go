package daemon

import (
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// Dispatcher handles priority sorting and agent_executor dispatch.
type Dispatcher struct {
	maestroDir        string
	config            model.Config
	leaseManager      *LeaseManager
	logger            *log.Logger
	logLevel          LogLevel
	executorFactory   ExecutorFactory
	eventBus          *events.Bus
	qualityGate       *QualityGateDaemon
	gateEvaluations   map[string]*model.QualityGateEvaluation // task_id -> evaluation
	gateEvalMutex     sync.RWMutex
}

// ExecutorFactory creates agent executors. Allows testing without tmux.
type ExecutorFactory func(maestroDir string, watcherCfg model.WatcherConfig, logLevel string) (AgentExecutor, error)

// AgentExecutor is the interface for agent message delivery.
type AgentExecutor interface {
	Execute(req agent.ExecRequest) agent.ExecResult
	Close() error
}

// NewDispatcher creates a new Dispatcher.
func NewDispatcher(maestroDir string, cfg model.Config, lm *LeaseManager, logger *log.Logger, logLevel LogLevel) *Dispatcher {
	return &Dispatcher{
		maestroDir:      maestroDir,
		config:          cfg,
		leaseManager:    lm,
		logger:          logger,
		logLevel:        logLevel,
		gateEvaluations: make(map[string]*model.QualityGateEvaluation),
		executorFactory: func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
			return agent.NewExecutor(dir, wcfg, level)
		},
	}
}

// SetExecutorFactory overrides the executor factory for testing.
func (d *Dispatcher) SetExecutorFactory(f ExecutorFactory) {
	d.executorFactory = f
}

// SetEventBus sets the event bus for publishing events.
func (d *Dispatcher) SetEventBus(bus *events.Bus) {
	d.eventBus = bus
}

// SetQualityGate sets the quality gate daemon for the dispatcher.
func (d *Dispatcher) SetQualityGate(qg *QualityGateDaemon) {
	d.qualityGate = qg
}

// sortableEntry wraps a queue entry with computed effective priority for sorting.
type sortableEntry struct {
	Index             int
	EffectivePriority int
	CreatedAt         time.Time
	ID                string
	Type              string // "command", "task", "notification"
}

// EffectivePriority computes the aging-adjusted priority.
// effective_priority = max(0, priority - floor(age_seconds / priority_aging_sec))
func EffectivePriority(priority int, createdAt string, priorityAgingSec int) int {
	if priorityAgingSec <= 0 {
		return priority
	}
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return priority
	}
	ageSec := time.Since(created).Seconds()
	aging := int(math.Floor(ageSec / float64(priorityAgingSec)))
	result := priority - aging
	if result < 0 {
		return 0
	}
	return result
}

// SortPendingTasks sorts tasks by effective_priority ASC → created_at ASC → id ASC.
func (d *Dispatcher) SortPendingTasks(tasks []model.Task) []int {
	priorityAgingSec := d.config.Queue.PriorityAgingSec

	var entries []sortableEntry
	for i, task := range tasks {
		if task.Status != model.StatusPending {
			continue
		}
		created, _ := time.Parse(time.RFC3339, task.CreatedAt)
		entries = append(entries, sortableEntry{
			Index:             i,
			EffectivePriority: EffectivePriority(task.Priority, task.CreatedAt, priorityAgingSec),
			CreatedAt:         created,
			ID:                task.ID,
			Type:              "task",
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

// SortPendingCommands sorts commands by effective_priority ASC → created_at ASC → id ASC.
func (d *Dispatcher) SortPendingCommands(commands []model.Command) []int {
	priorityAgingSec := d.config.Queue.PriorityAgingSec

	var entries []sortableEntry
	for i, cmd := range commands {
		if cmd.Status != model.StatusPending {
			continue
		}
		created, _ := time.Parse(time.RFC3339, cmd.CreatedAt)
		entries = append(entries, sortableEntry{
			Index:             i,
			EffectivePriority: EffectivePriority(cmd.Priority, cmd.CreatedAt, priorityAgingSec),
			CreatedAt:         created,
			ID:                cmd.ID,
			Type:              "command",
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

// SortPendingNotifications sorts notifications by effective_priority ASC → created_at ASC → id ASC.
func (d *Dispatcher) SortPendingNotifications(notifications []model.Notification) []int {
	priorityAgingSec := d.config.Queue.PriorityAgingSec

	var entries []sortableEntry
	for i, ntf := range notifications {
		if ntf.Status != model.StatusPending {
			continue
		}
		created, _ := time.Parse(time.RFC3339, ntf.CreatedAt)
		entries = append(entries, sortableEntry{
			Index:             i,
			EffectivePriority: EffectivePriority(ntf.Priority, ntf.CreatedAt, priorityAgingSec),
			CreatedAt:         created,
			ID:                ntf.ID,
			Type:              "notification",
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

// DispatchCommand dispatches a command to the planner agent.
func (d *Dispatcher) DispatchCommand(cmd *model.Command) error {
	exec, err := d.executorFactory(d.maestroDir, d.config.Watcher, d.config.Logging.Level)
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}
	defer func() { _ = exec.Close() }()

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
		d.log(LogLevelError, "dispatch_command_failed id=%s error=%v retryable=%v",
			cmd.ID, result.Error, result.Retryable)
		return result.Error
	}

	d.log(LogLevelInfo, "dispatch_command_success id=%s epoch=%d", cmd.ID, cmd.LeaseEpoch)
	return nil
}

// DispatchTask dispatches a task to a worker agent.
func (d *Dispatcher) DispatchTask(task *model.Task, workerID string) error {
	// Pre-task quality gate check and record evaluation result
	var gateEvaluation *model.QualityGateEvaluation
	if d.shouldEvaluateQualityGates() && d.config.QualityGates.Enforcement.PreTaskCheck {
		evaluation, err := d.evaluatePreTaskGateWithResult(task, workerID)
		gateEvaluation = evaluation

		if err != nil {
			if d.config.QualityGates.Enforcement.FailureAction == "block" {
				d.log(LogLevelError, "dispatch_task_blocked_by_quality_gate id=%s worker=%s error=%v",
					task.ID, workerID, err)
				// Store evaluation result for later recording
				d.storeGateEvaluation(task.ID, gateEvaluation)
				return fmt.Errorf("quality gate check failed: %w", err)
			}
			// If action is "warn", just log the violation
			d.log(LogLevelWarn, "dispatch_task_quality_gate_violation id=%s worker=%s error=%v",
				task.ID, workerID, err)
		}
		// Store evaluation result for later recording
		d.storeGateEvaluation(task.ID, gateEvaluation)
	} else if !d.config.QualityGates.Enabled {
		// Record that gates were skipped
		gateEvaluation = &model.QualityGateEvaluation{
			Passed:        true,
			SkippedReason: "disabled",
			EvaluatedAt:   time.Now().Format(time.RFC3339),
		}
		d.storeGateEvaluation(task.ID, gateEvaluation)
	} else if d.config.QualityGates.SkipGates {
		// Record that emergency mode was used
		gateEvaluation = &model.QualityGateEvaluation{
			Passed:        true,
			SkippedReason: "emergency_mode",
			EvaluatedAt:   time.Now().Format(time.RFC3339),
		}
		d.storeGateEvaluation(task.ID, gateEvaluation)
	}

	exec, err := d.executorFactory(d.maestroDir, d.config.Watcher, d.config.Logging.Level)
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}
	defer func() { _ = exec.Close() }()

	envelope := agent.BuildWorkerEnvelope(*task, workerID, task.LeaseEpoch, task.Attempts)

	result := exec.Execute(agent.ExecRequest{
		AgentID:    workerID,
		Message:    envelope,
		Mode:       agent.ModeWithClear,
		TaskID:     task.ID,
		CommandID:  task.CommandID,
		LeaseEpoch: task.LeaseEpoch,
		Attempt:    task.Attempts,
	})

	if result.Error != nil {
		d.log(LogLevelError, "dispatch_task_failed id=%s worker=%s error=%v retryable=%v",
			task.ID, workerID, result.Error, result.Retryable)
		return result.Error
	}

	d.log(LogLevelInfo, "dispatch_task_success id=%s worker=%s epoch=%d",
		task.ID, workerID, task.LeaseEpoch)

	// Publish task_started event (non-blocking, best-effort)
	if d.eventBus != nil {
		d.eventBus.Publish(events.EventTaskStarted, map[string]interface{}{
			"task_id":    task.ID,
			"command_id": task.CommandID,
			"worker_id":  workerID,
			"epoch":      task.LeaseEpoch,
		})
	}

	return nil
}

// DispatchNotification dispatches a notification to the orchestrator agent.
func (d *Dispatcher) DispatchNotification(ntf *model.Notification) error {
	exec, err := d.executorFactory(d.maestroDir, d.config.Watcher, d.config.Logging.Level)
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}
	defer func() { _ = exec.Close() }()

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
		d.log(LogLevelError, "dispatch_notification_failed id=%s error=%v retryable=%v",
			ntf.ID, result.Error, result.Retryable)
		return result.Error
	}

	d.log(LogLevelInfo, "dispatch_notification_success id=%s type=%s epoch=%d",
		ntf.ID, ntf.Type, ntf.LeaseEpoch)
	return nil
}

func (d *Dispatcher) log(level LogLevel, format string, args ...any) {
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
	d.logger.Printf("%s %s dispatcher: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}

// shouldEvaluateQualityGates determines if quality gates should be evaluated
func (d *Dispatcher) shouldEvaluateQualityGates() bool {
	// Skip if quality gates are disabled
	if !d.config.QualityGates.Enabled {
		return false
	}

	// Skip if emergency mode is enabled (--skip-gates)
	if d.config.QualityGates.SkipGates {
		d.log(LogLevelInfo, "quality_gates_skipped reason=emergency_mode")
		return false
	}

	// Skip if quality gate daemon is not available
	if d.qualityGate == nil {
		d.log(LogLevelDebug, "quality_gates_skipped reason=daemon_not_available")
		return false
	}

	return true
}

// evaluatePreTaskGateWithResult evaluates quality gates before task execution and returns the result
func (d *Dispatcher) evaluatePreTaskGateWithResult(task *model.Task, workerID string) (*model.QualityGateEvaluation, error) {
	if d.qualityGate == nil {
		return nil, nil
	}

	// Emit task start event for quality gate evaluation
	event := TaskStartEvent{
		TaskID:    task.ID,
		CommandID: task.CommandID,
		AgentID:   workerID,
		StartedAt: time.Now(),
	}

	d.qualityGate.EmitEvent(event)

	// Perform synchronous evaluation
	context := map[string]interface{}{
		"task_id":    task.ID,
		"command_id": task.CommandID,
		"agent_id":   workerID,
		"priority":   task.Priority,
		"attempts":   task.Attempts,
	}

	// Use the evaluateGateWithResult method for synchronous evaluation
	result, err := d.qualityGate.evaluateGateWithResult("pre_task", context)

	// Convert to model.QualityGateEvaluation
	evaluation := &model.QualityGateEvaluation{
		Passed:      result != nil && result.Passed,
		EvaluatedAt: time.Now().Format(time.RFC3339),
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

	if !result.Passed {
		return evaluation, fmt.Errorf("quality gate check failed: %v", result.FailedGates)
	}

	return evaluation, nil
}

// storeGateEvaluation stores the gate evaluation for a task
func (d *Dispatcher) storeGateEvaluation(taskID string, evaluation *model.QualityGateEvaluation) {
	if evaluation == nil {
		return
	}

	d.gateEvalMutex.Lock()
	defer d.gateEvalMutex.Unlock()
	d.gateEvaluations[taskID] = evaluation
}

// GetGateEvaluation retrieves the gate evaluation for a task
func (d *Dispatcher) GetGateEvaluation(taskID string) *model.QualityGateEvaluation {
	d.gateEvalMutex.RLock()
	defer d.gateEvalMutex.RUnlock()
	return d.gateEvaluations[taskID]
}
