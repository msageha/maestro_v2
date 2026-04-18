package daemon

import (
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
)

// stepIdleStatusSync sets @status="idle" for agents that have no in_progress
// queue items. This corrects the display status for agents whose @status was
// set to "busy" during delivery but never reverted after task completion.
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
		if !hasInProgressTasks(tq.Queue.Tasks) {
			syncAgentIdle(workerID, qh)
		}
	}

	// Planner: check command queue
	if !hasInProgressCommands(s.commands.Data.Commands) {
		syncAgentIdle("planner", qh)
	}

	// Orchestrator: check notification queue
	if !hasInProgressNotifications(s.notifications.Data.Notifications) {
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

// syncIdleAfterPhaseC re-checks agent idle status after Phase C has applied
// dispatch and busy-check results. Phase A's stepIdleStatusSync runs before
// Phase B dispatches, so agents whose dispatches completed in Phase C still
// have @status="busy" in tmux. This additional sync ensures the tmux status
// reflects the post-Phase-C queue state, keeping dashboard and `maestro status`
// consistent within the same scan cycle.
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
		if !hasInProgressTasks(tq.Queue.Tasks) {
			syncAgentIdle(workerID, qh)
		}
	}
	if !hasInProgressCommands(commandQueue.Commands) {
		syncAgentIdle("planner", qh)
	}
	if !hasInProgressNotifications(notificationQueue.Notifications) {
		syncAgentIdle("orchestrator", qh)
	}
}

// syncAgentIdle sets the @status tmux user variable to "idle" for the given
// agent. Best-effort: errors are logged at debug level to avoid log noise
// during normal operation (e.g. agent pane not found after shutdown).
func syncAgentIdle(agentID string, qh *QueueHandler) {
	paneTarget, err := tmux.FindPaneByAgentID(agentID)
	if err != nil {
		// Agent pane not found — expected when agents are not running.
		return
	}
	if err := tmux.SetUserVar(paneTarget, "status", "idle"); err != nil {
		qh.log(LogLevelDebug, "idle_status_sync_failed agent=%s: %v", agentID, err)
	}
}
