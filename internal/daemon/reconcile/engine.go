package reconcile

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/core"
)

// executorErrorTTL is how long a failed executor factory result is cached
// before the next getExecutor call retries creation.
const executorErrorTTL = 30 * time.Second

// Engine orchestrates the execution of all reconciliation patterns.
type Engine struct {
	deps     Deps
	patterns []Pattern

	// cachedExec is a shared Executor instance created once and reused across
	// notification calls. This avoids per-call log file Open/Close overhead.
	execMu          sync.Mutex
	cachedExec      core.AgentExecutor
	cachedExecErr   error
	cachedExecErrAt time.Time
	execInit        bool
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

// SetExecutorFactory overrides the executor factory for testing.
// Resets the cached executor so the new factory is used on next call.
func (e *Engine) SetExecutorFactory(f core.ExecutorFactory) {
	e.execMu.Lock()
	old := e.cachedExec
	e.deps.ExecutorFactory = f
	e.cachedExec = nil
	e.cachedExecErr = nil
	e.cachedExecErrAt = time.Time{}
	e.execInit = false
	e.execMu.Unlock()

	if old != nil {
		_ = old.Close()
	}
}

// getExecutor returns the shared executor instance, creating it lazily on first call.
func (e *Engine) getExecutor() (core.AgentExecutor, error) {
	e.execMu.Lock()
	defer e.execMu.Unlock()
	if e.execInit && e.cachedExecErr != nil && e.deps.Clock.Now().Sub(e.cachedExecErrAt) >= executorErrorTTL {
		e.execInit = false
	}
	if !e.execInit {
		e.cachedExec, e.cachedExecErr = e.deps.ExecutorFactory(e.deps.MaestroDir, e.deps.Config.Watcher, e.deps.Config.Logging.Level)
		e.execInit = true
		if e.cachedExecErr != nil {
			e.cachedExecErrAt = e.deps.Clock.Now()
		} else {
			e.cachedExecErrAt = time.Time{}
		}
	}
	if e.cachedExecErr != nil {
		return nil, fmt.Errorf("%w: %v", core.ErrExecutorInit, e.cachedExecErr)
	}
	return e.cachedExec, nil
}

// CloseExecutor releases the shared executor's resources.
// Safe to call multiple times; subsequent calls are no-ops.
func (e *Engine) CloseExecutor() {
	e.execMu.Lock()
	exec := e.cachedExec
	e.cachedExec = nil
	e.cachedExecErr = nil
	e.cachedExecErrAt = time.Time{}
	e.execInit = false
	e.execMu.Unlock()

	if exec != nil {
		_ = exec.Close()
	}
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
		case "re_fill":
			e.notifyPlannerOfReFill(n.CommandID)
		case "re_evaluate":
			e.notifyPlannerOfReEvaluation(n.CommandID, n.Reason)
		case "fill_timeout":
			e.notifyPlannerOfTimeout(n.CommandID, n.TimedOutPhases)
		default:
			e.deps.DL.Logf(core.LogLevelWarn, "unknown deferred notification kind=%s command=%s", n.Kind, n.CommandID)
		}
	}
}

func (e *Engine) notifyPlannerOfReFill(commandID string) {
	exec, err := e.getExecutor()
	if err != nil {
		e.deps.DL.Logf(core.LogLevelWarn, "R0b notify_planner create_executor error=%v", err)
		return
	}

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
}

func (e *Engine) notifyPlannerOfReEvaluation(commandID, reason string) {
	exec, err := e.getExecutor()
	if err != nil {
		e.deps.DL.Logf(core.LogLevelWarn, "R4 notify_planner create_executor error=%v", err)
		return
	}

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
}

func (e *Engine) notifyPlannerOfTimeout(commandID string, timedOutPhases map[string]bool) {
	exec, err := e.getExecutor()
	if err != nil {
		e.deps.DL.Logf(core.LogLevelWarn, "R6 notify_planner create_executor error=%v", err)
		return
	}

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
}
