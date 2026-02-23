package daemon

import (
	"fmt"
	"log"
	"math"
	"sort"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// Dispatcher handles priority sorting and agent_executor dispatch.
type Dispatcher struct {
	maestroDir      string
	config          model.Config
	leaseManager    *LeaseManager
	logger          *log.Logger
	logLevel        LogLevel
	executorFactory ExecutorFactory
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
		maestroDir:   maestroDir,
		config:       cfg,
		leaseManager: lm,
		logger:       logger,
		logLevel:     logLevel,
		executorFactory: func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
			return agent.NewExecutor(dir, wcfg, level)
		},
	}
}

// SetExecutorFactory overrides the executor factory for testing.
func (d *Dispatcher) SetExecutorFactory(f ExecutorFactory) {
	d.executorFactory = f
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
