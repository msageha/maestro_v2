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

// maxReconcilePasses is the upper bound on fixpoint re-execution to prevent
// infinite loops. If repairs are still being generated after this many passes,
// the loop terminates and returns all accumulated results.
const maxReconcilePasses = 3

// Reconcile runs all reconciliation patterns with bounded fixpoint iteration.
// After the initial pass, if any repairs were generated, re-runs all patterns
// up to maxReconcilePasses total. The loop terminates early when a pass produces
// no repairs (fixpoint reached).
//
// Note: Individual patterns (e.g. R4PlanStatus) may implement their own backoff
// for repeatedly failing operations. The fixpoint loop handles cross-pattern
// convergence, while pattern-level backoff prevents hammering failing evaluators.
func (e *Engine) Reconcile() ([]Repair, []DeferredNotification) {
	var allRepairs []Repair
	var allNotifications []DeferredNotification

	for pass := 0; pass < maxReconcilePasses; pass++ {
		run := newRun(&e.deps)
		var passRepairs []Repair

		for _, p := range e.patterns {
			outcome := p.Apply(run)
			passRepairs = append(passRepairs, outcome.Repairs...)
			allNotifications = append(allNotifications, outcome.Notifications...)
		}

		allRepairs = append(allRepairs, passRepairs...)

		if len(passRepairs) == 0 {
			break // fixpoint reached — no further repairs needed
		}
		if pass > 0 {
			e.deps.DL.Logf(core.LogLevelInfo, "reconcile_fixpoint pass=%d repairs=%d", pass+1, len(passRepairs))
		}
	}

	return allRepairs, allNotifications
}

// ExecuteDeferredNotifications sends collected Planner notifications via agent executor.
// Returns the list of notifications that failed to deliver, enabling the caller to retry.
func (e *Engine) ExecuteDeferredNotifications(notifications []DeferredNotification) []DeferredNotification {
	if e.deps.ExecutorFactory == nil {
		return notifications
	}
	var failed []DeferredNotification
	for _, n := range notifications {
		var err error
		switch n.Kind {
		case NotifyReFill:
			err = e.notifyPlannerOfReFill(n.CommandID)
		case NotifyReEvaluate:
			err = e.notifyPlannerOfReEvaluation(n.CommandID, n.Reason)
		case NotifyFillTimeout:
			err = e.notifyPlannerOfTimeout(n.CommandID, n.TimedOutPhases)
		case NotifyConflictResolution:
			err = e.notifyPlannerOfConflictResolution(n.CommandID, n.WorkerID)
		case NotifyConflictEscalation:
			err = e.notifyPlannerOfConflictEscalation(n.CommandID, n.WorkerID)
		default:
			e.deps.DL.Logf(core.LogLevelWarn, "unknown deferred notification kind=%s command=%s", n.Kind, n.CommandID)
			continue
		}
		if err != nil {
			failed = append(failed, n)
		}
	}
	return failed
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
// Returns the error from fn, or the executor creation error.
func (e *Engine) withExecutor(label string, fn func(exec core.AgentExecutor) error) error {
	exec, err := e.createExecutor(label)
	if err != nil {
		return err
	}
	defer func() {
		if err := exec.Close(); err != nil {
			e.deps.DL.Logf(core.LogLevelWarn, "close executor error=%v", err)
		}
	}()
	return fn(exec)
}

func (e *Engine) notifyPlannerOfReFill(commandID string) error {
	return e.withExecutor("R0b", func(exec core.AgentExecutor) error {
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
			return result.Error
		}
		return nil
	})
}

func (e *Engine) notifyPlannerOfReEvaluation(commandID, reason string) error {
	return e.withExecutor("R4", func(exec core.AgentExecutor) error {
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
			return result.Error
		}
		return nil
	})
}

func (e *Engine) notifyPlannerOfTimeout(commandID string, timedOutPhases map[string]bool) error {
	return e.withExecutor("R6", func(exec core.AgentExecutor) error {
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
			return result.Error
		}
		return nil
	})
}

func (e *Engine) notifyPlannerOfConflictResolution(commandID, workerID string) error {
	return e.withExecutor("R7", func(exec core.AgentExecutor) error {
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
			return result.Error
		}
		return nil
	})
}

func (e *Engine) notifyPlannerOfConflictEscalation(commandID, workerID string) error {
	return e.withExecutor("R7", func(exec core.AgentExecutor) error {
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
			return result.Error
		}
		return nil
	})
}
