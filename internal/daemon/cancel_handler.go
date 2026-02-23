package daemon

import (
	"fmt"
	"log"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// CancelHandler processes cancellation of commands and their tasks.
type CancelHandler struct {
	maestroDir      string
	config          model.Config
	logger          *log.Logger
	logLevel        LogLevel
	executorFactory ExecutorFactory
}

// NewCancelHandler creates a new CancelHandler.
func NewCancelHandler(maestroDir string, cfg model.Config, logger *log.Logger, logLevel LogLevel) *CancelHandler {
	return &CancelHandler{
		maestroDir: maestroDir,
		config:     cfg,
		logger:     logger,
		logLevel:   logLevel,
		executorFactory: func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
			return agent.NewExecutor(dir, wcfg, level)
		},
	}
}

// SetExecutorFactory overrides the executor factory for testing.
func (ch *CancelHandler) SetExecutorFactory(f ExecutorFactory) {
	ch.executorFactory = f
}

// IsCommandCancelRequested checks if a command has been marked for cancellation.
func (ch *CancelHandler) IsCommandCancelRequested(cmd *model.Command) bool {
	return cmd.CancelRequestedAt != nil
}

// CancelPendingTasks transitions pending tasks of a cancelled command to cancelled.
// Returns the number of tasks cancelled (periodic scan step 0.5).
func (ch *CancelHandler) CancelPendingTasks(tasks []model.Task, commandID string) []CancelledTaskResult {
	var results []CancelledTaskResult

	for i := range tasks {
		task := &tasks[i]
		if task.CommandID != commandID || task.Status != model.StatusPending {
			continue
		}

		if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusCancelled); err != nil {
			ch.log(LogLevelWarn, "cancel_pending_skip task=%s error=%v", task.ID, err)
			continue
		}

		task.Status = model.StatusCancelled
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

		ch.log(LogLevelInfo, "cancel_pending task=%s command=%s", task.ID, commandID)
		results = append(results, CancelledTaskResult{
			TaskID:    task.ID,
			CommandID: commandID,
			Status:    "cancelled",
			Reason:    "command_cancelled",
		})
	}

	return results
}

// InterruptInProgressTasks interrupts in_progress tasks of a cancelled command.
// Returns the number of tasks interrupted (periodic scan step 0.6).
func (ch *CancelHandler) InterruptInProgressTasks(tasks []model.Task, commandID string) []CancelledTaskResult {
	var results []CancelledTaskResult

	for i := range tasks {
		task := &tasks[i]
		if task.CommandID != commandID || task.Status != model.StatusInProgress {
			continue
		}

		// Send interrupt via agent executor
		if task.LeaseOwner != nil {
			if err := ch.interruptAgent(*task.LeaseOwner, task.ID, task.CommandID, task.LeaseEpoch); err != nil {
				ch.log(LogLevelError, "interrupt_failed task=%s worker=%s error=%v",
					task.ID, *task.LeaseOwner, err)
				// Continue to cancel even if interrupt fails
			}
		}

		if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusCancelled); err != nil {
			ch.log(LogLevelWarn, "cancel_inprogress_skip task=%s error=%v", task.ID, err)
			continue
		}

		task.Status = model.StatusCancelled
		task.LeaseOwner = nil
		task.LeaseExpiresAt = nil
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

		ch.log(LogLevelInfo, "cancel_inprogress task=%s command=%s", task.ID, commandID)
		results = append(results, CancelledTaskResult{
			TaskID:    task.ID,
			CommandID: commandID,
			Status:    "cancelled",
			Reason:    "command_cancelled_interrupt",
		})
	}

	return results
}

// CancelledTaskResult represents a synthetic cancelled result entry.
type CancelledTaskResult struct {
	TaskID    string
	CommandID string
	Status    string
	Reason    string
}

// BuildSyntheticResult creates a synthetic result for a cancelled task.
func (ch *CancelHandler) BuildSyntheticResult(r CancelledTaskResult) map[string]any {
	return map[string]any{
		"task_id":    r.TaskID,
		"command_id": r.CommandID,
		"status":     r.Status,
		"reason":     r.Reason,
		"synthetic":  true,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
}

// interruptAgent sends an interrupt to the agent running the task.
func (ch *CancelHandler) interruptAgent(workerID, taskID, commandID string, leaseEpoch int) error {
	exec, err := ch.executorFactory(ch.maestroDir, ch.config.Watcher, ch.config.Logging.Level)
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}
	defer exec.Close()

	result := exec.Execute(agent.ExecRequest{
		AgentID:    workerID,
		Mode:       agent.ModeInterrupt,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
	})

	if result.Error != nil {
		return result.Error
	}
	return nil
}

func (ch *CancelHandler) log(level LogLevel, format string, args ...any) {
	if level < ch.logLevel {
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
	ch.logger.Printf("%s %s cancel_handler: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}
