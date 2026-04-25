package daemon

import (
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
)

// stepIdleStatusSync reconciles each agent's @status pane variable with the
// presence of in_progress items in their queue file. For every agent the
// direction is:
//
//   - queue has in_progress items → @status="busy"
//   - queue has no in_progress items → @status="idle"
//
// The busy branch is a defensive sync that prevents stale "idle" displays
// when the agent-side SetStatus("busy") did not fire — e.g., when dispatch
// delivery failed after the lease was acquired (task is in_progress in the
// queue, but the worker pane was never reached). Without this sync, `maestro
// status` and `status --json` show a misleading "idle" worker while the
// queue contains an active task.
//
// The step runs at the end of Phase A and uses tmux.SetUserVar directly.
// Each SetUserVar call takes ~5ms, so the overhead for 4 agents is negligible.
func (qh *QueueHandler) stepIdleStatusSync(s *scanState) {
	// Workers: check task queues
	for queueFile, tq := range s.tasks {
		workerID := workerIDFromPath(queueFile)
		if workerID == "" {
			continue
		}
		if hasInProgressTasks(tq.Queue.Tasks) {
			syncAgentBusy(workerID, qh)
		} else {
			syncAgentIdle(workerID, qh)
		}
	}

	// Planner: check command queue
	if hasInProgressCommands(s.commands.Data.Commands) {
		syncAgentBusy("planner", qh)
	} else {
		syncAgentIdle("planner", qh)
	}

	// Orchestrator: check notification queue
	if hasInProgressNotifications(s.notifications.Data.Notifications) {
		syncAgentBusy("orchestrator", qh)
	} else {
		syncAgentIdle("orchestrator", qh)
	}
}

func hasInProgressTasks(tasks []model.Task) bool {
	for i := range tasks {
		if tasks[i].Status == model.StatusInProgress {
			return true
		}
	}
	return false
}

func hasInProgressCommands(commands []model.Command) bool {
	for i := range commands {
		if commands[i].Status == model.StatusInProgress {
			return true
		}
	}
	return false
}

func hasInProgressNotifications(notifications []model.Notification) bool {
	for i := range notifications {
		if notifications[i].Status == model.StatusInProgress {
			return true
		}
	}
	return false
}

// syncIdleAfterPhaseC re-reconciles agent @status after Phase C has applied
// dispatch and busy-check results. Phase A's stepIdleStatusSync runs before
// Phase B dispatches, so any status changes caused by Phase B (lease acquire)
// or Phase C (dispatch delivery / result write) may not yet be reflected.
// This additional sync ensures the tmux @status always matches the post-
// Phase-C queue state, keeping dashboard and `maestro status` consistent
// within the same scan cycle. Both directions (busy↔idle) are synced so that
// a dispatch failure (task in_progress but @status never set to busy by the
// agent path) is observable rather than silent.
func (qh *QueueHandler) syncIdleAfterPhaseC(
	commandQueue model.CommandQueue,
	taskQueues map[string]*taskQueueEntry,
	notificationQueue model.NotificationQueue,
) {
	for queueFile, tq := range taskQueues {
		workerID := workerIDFromPath(queueFile)
		if workerID == "" {
			continue
		}
		if hasInProgressTasks(tq.Queue.Tasks) {
			syncAgentBusy(workerID, qh)
		} else {
			syncAgentIdle(workerID, qh)
		}
	}
	if hasInProgressCommands(commandQueue.Commands) {
		syncAgentBusy("planner", qh)
	} else {
		syncAgentIdle("planner", qh)
	}
	if hasInProgressNotifications(notificationQueue.Notifications) {
		syncAgentBusy("orchestrator", qh)
	} else {
		syncAgentIdle("orchestrator", qh)
	}
}

// syncAgentIdle sets the @status tmux user variable to "idle" for the given
// agent. Best-effort: errors are logged at debug level to avoid log noise
// during normal operation (e.g. agent pane not found after shutdown).
func syncAgentIdle(agentID string, qh *QueueHandler) {
	syncAgentStatus(agentID, "idle", qh)
}

// syncAgentBusy sets the @status tmux user variable to "busy" for the given
// agent. Used to recover from scenarios where the queue entry reached
// in_progress but the agent-side SetStatus("busy") never executed (e.g.,
// dispatch delivery failure after lease acquire). Best-effort: errors are
// logged at debug level.
func syncAgentBusy(agentID string, qh *QueueHandler) {
	syncAgentStatus(agentID, "busy", qh)
}

// syncAgentStatus writes the @status tmux user variable for the given agent.
// Errors are logged at debug level (pane-not-found is expected when agents
// are not running) to avoid log noise during normal operation.
func syncAgentStatus(agentID, status string, qh *QueueHandler) {
	paneTarget, err := tmux.FindPaneByAgentID(agentID)
	if err != nil {
		// Agent pane not found — expected when agents are not running.
		return
	}
	if err := tmux.SetUserVar(paneTarget, "status", status); err != nil {
		qh.log(LogLevelDebug, "status_sync_failed agent=%s status=%s: %v", agentID, status, err)
	}
}
