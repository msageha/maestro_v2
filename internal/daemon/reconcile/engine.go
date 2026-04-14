package reconcile

import (
	"fmt"
	"strings"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/core"
)

// Engine orchestrates the execution of all reconciliation patterns.
type Engine struct {
	deps     Deps
	patterns []Pattern
}

// NewEngine creates a new Engine with the given dependencies and patterns.
func NewEngine(deps Deps, patterns ...Pattern) *Engine {
	return &Engine{
		deps:     deps,
		patterns: patterns,
	}
}

// SetCanComplete sets the CanComplete function (wired after plan package init to avoid import cycles).
func (e *Engine) SetCanComplete(f core.CanCompleteFunc) {
	e.deps.CanComplete = f
}

// SetClock replaces the clock used by reconciliation patterns (for testing).
func (e *Engine) SetClock(c core.Clock) {
	e.deps.Clock = c
}

// Reconcile runs all reconciliation patterns and returns repairs and deferred notifications.
func (e *Engine) Reconcile() ([]Repair, []DeferredNotification) {
	run := newRun(&e.deps)

	var allRepairs []Repair
	var allNotifications []DeferredNotification

	for _, p := range e.patterns {
		outcome := p.Apply(run)
		allRepairs = append(allRepairs, outcome.Repairs...)
		allNotifications = append(allNotifications, outcome.Notifications...)
	}

	return allRepairs, allNotifications
}

// ExecuteDeferredNotifications sends collected Planner notifications via agent executor.
func (e *Engine) ExecuteDeferredNotifications(notifications []DeferredNotification) {
	if e.deps.ExecutorFactory == nil {
		return
	}
	for _, n := range notifications {
		switch n.Kind {
		case NotifyReFill:
			e.notifyPlannerOfReFill(n.CommandID)
		case NotifyReEvaluate:
			e.notifyPlannerOfReEvaluation(n.CommandID, n.Reason)
		case NotifyFillTimeout:
			e.notifyPlannerOfTimeout(n.CommandID, n.TimedOutPhases)
		case NotifyConflictResolution:
			e.notifyPlannerOfConflictResolution(n.CommandID, n.WorkerID)
		case NotifyConflictEscalation:
			e.notifyPlannerOfConflictEscalation(n.CommandID, n.WorkerID)
		default:
			e.deps.DL.Logf(core.LogLevelWarn, "unknown deferred notification kind=%s command=%s", n.Kind, n.CommandID)
		}
	}
}

// createExecutor creates an AgentExecutor via the factory, logging and returning
// an error if the factory fails or returns nil.
func (e *Engine) createExecutor(label string) (core.AgentExecutor, error) {
	exec, err := e.deps.ExecutorFactory(e.deps.MaestroDir, e.deps.Config.Watcher, e.deps.Config.Logging.Level)
	if err != nil {
		e.deps.DL.Logf(core.LogLevelWarn, "%s notify_planner create_executor error=%v", label, err)
		return nil, err
	}
	if exec == nil {
		e.deps.DL.Logf(core.LogLevelWarn, "%s notify_planner create_executor returned nil", label)
		return nil, fmt.Errorf("executor factory returned nil")
	}
	return exec, nil
}

// withExecutor creates an executor via the factory, runs fn, and ensures cleanup.
func (e *Engine) withExecutor(label string, fn func(exec core.AgentExecutor)) {
	exec, err := e.createExecutor(label)
	if err != nil {
		return
	}
	defer func() {
		if err := exec.Close(); err != nil {
			e.deps.DL.Logf(core.LogLevelWarn, "close executor error=%v", err)
		}
	}()
	fn(exec)
}

func (e *Engine) notifyPlannerOfReFill(commandID string) {
	e.withExecutor("R0b", func(exec core.AgentExecutor) {
		message := fmt.Sprintf("[maestro] kind:re_fill command_id:%s\nphase filling was stuck, reverted to awaiting_fill — please re-submit tasks",
			commandID)

		result := exec.Execute(agent.ExecRequest{
			AgentID:   "planner",
			Message:   message,
			Mode:      agent.ModeDeliver,
			CommandID: commandID,
		})
		if result.Error != nil {
			e.deps.DL.Logf(core.LogLevelWarn, "R0b notify_planner command=%s error=%v", commandID, result.Error)
		}
	})
}

func (e *Engine) notifyPlannerOfReEvaluation(commandID, reason string) {
	e.withExecutor("R4", func(exec core.AgentExecutor) {
		message := fmt.Sprintf("[maestro] kind:re_evaluate command_id:%s\ncan_complete failed: %s — result quarantined, please re-evaluate",
			commandID, reason)

		result := exec.Execute(agent.ExecRequest{
			AgentID:   "planner",
			Message:   message,
			Mode:      agent.ModeDeliver,
			CommandID: commandID,
		})
		if result.Error != nil {
			e.deps.DL.Logf(core.LogLevelWarn, "R4 notify_planner command=%s error=%v", commandID, result.Error)
		}
	})
}

func (e *Engine) notifyPlannerOfTimeout(commandID string, timedOutPhases map[string]bool) {
	e.withExecutor("R6", func(exec core.AgentExecutor) {
		phases := make([]string, 0, len(timedOutPhases))
		for name := range timedOutPhases {
			phases = append(phases, name)
		}
		message := fmt.Sprintf("[maestro] kind:fill_timeout command_id:%s phases:%s\nfill deadline expired, phases timed out",
			commandID, strings.Join(phases, ","))

		result := exec.Execute(agent.ExecRequest{
			AgentID:   "planner",
			Message:   message,
			Mode:      agent.ModeDeliver,
			CommandID: commandID,
		})
		if result.Error != nil {
			e.deps.DL.Logf(core.LogLevelWarn, "R6 notify_planner command=%s error=%v", commandID, result.Error)
		}
	})
}

func (e *Engine) notifyPlannerOfConflictResolution(commandID, workerID string) {
	e.withExecutor("R7", func(exec core.AgentExecutor) {
		message := fmt.Sprintf("[maestro] kind:conflict_resolution command_id:%s worker_id:%s\nmerge conflict detected — please generate a __conflict_resolution task",
			commandID, workerID)

		result := exec.Execute(agent.ExecRequest{
			AgentID:   "planner",
			Message:   message,
			Mode:      agent.ModeDeliver,
			CommandID: commandID,
		})
		if result.Error != nil {
			e.deps.DL.Logf(core.LogLevelWarn, "R7 notify_planner_resolution command=%s worker=%s error=%v", commandID, workerID, result.Error)
		}
	})
}

func (e *Engine) notifyPlannerOfConflictEscalation(commandID, workerID string) {
	e.withExecutor("R7", func(exec core.AgentExecutor) {
		message := fmt.Sprintf("[maestro] kind:conflict_escalation command_id:%s worker_id:%s\nconflict resolution attempts exhausted — escalating to planner",
			commandID, workerID)

		result := exec.Execute(agent.ExecRequest{
			AgentID:   "planner",
			Message:   message,
			Mode:      agent.ModeDeliver,
			CommandID: commandID,
		})
		if result.Error != nil {
			e.deps.DL.Logf(core.LogLevelWarn, "R7 notify_planner_escalation command=%s worker=%s error=%v", commandID, workerID, result.Error)
		}
	})
}
