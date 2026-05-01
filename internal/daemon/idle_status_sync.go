package daemon

import (
	"strconv"

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
// Configured-worker enumeration (2026-04-29 e2e regression): the prior
// implementation iterated only the queue files that actually existed in
// s.tasks. Workers whose queue files were absent (e.g. a worker that has
// never been dispatched to in this daemon lifetime, or whose queue file
// was cleared by an earlier cleanup) were never visited, so a stale
// `@status=busy` left over from a previous session stuck around forever
// and `maestro status --json` reported the worker as busy with no work
// in flight. We now derive the canonical worker set from
// qh.config.Agents.Workers.Count and reset every configured worker
// without queue presence to idle. The non-existent queue file is treated
// as "queue is empty" because, for status semantics, the absence of work
// is exactly that.
//
// The step runs at the end of Phase A and uses tmux.SetUserVar directly.
// Each SetUserVar call takes ~5ms, so the overhead for 4 agents is negligible.
func (qh *QueueHandler) stepIdleStatusSync(s *scanState) {
	syncAllConfiguredWorkers(qh, s.tasks)

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

// syncAllConfiguredWorkers walks the canonical worker IDs derived from
// config.Agents.Workers.Count (worker1..workerN) and syncs each to busy or
// idle based on whether its queue file shows an in_progress task. Workers
// whose queue file does not exist in tasks (the scan loader skips missing
// files) are forced to idle — without this, a stale @status=busy from an
// earlier dispatch failure or daemon restart would never get cleared.
func syncAllConfiguredWorkers(qh *QueueHandler, tasks map[string]*taskQueueEntry) {
	// Build a workerID → task queue lookup. Keys in tasks are file paths;
	// we want a clean ID lookup.
	queuesByWorker := make(map[string]*taskQueueEntry, len(tasks))
	for queueFile, tq := range tasks {
		workerID := workerIDFromPath(queueFile)
		if workerID == "" {
			continue
		}
		queuesByWorker[workerID] = tq
	}

	count := qh.config.Agents.Workers.Count
	if count <= 0 {
		// Defensive: even with no configured workers, keep the previous
		// behaviour of syncing whatever queue files happen to exist so we
		// do not regress operator workflows that bypass config_count.
		for workerID, tq := range queuesByWorker {
			syncWorkerByQueue(workerID, tq, qh)
		}
		return
	}

	for i := 1; i <= count; i++ {
		workerID := "worker" + strconv.Itoa(i)
		syncWorkerByQueue(workerID, queuesByWorker[workerID], qh)
		delete(queuesByWorker, workerID)
	}
	// Cover any worker queue file outside the configured range (e.g.
	// operator added a stray queue file). Sync it too so its @status
	// reflects reality even though it is not formally configured.
	for workerID, tq := range queuesByWorker {
		syncWorkerByQueue(workerID, tq, qh)
	}
}

// syncWorkerByQueue routes a single worker to busy/idle based on its
// queue contents. A nil tq is treated as "queue empty / file absent" and
// drives the worker to idle.
func syncWorkerByQueue(workerID string, tq *taskQueueEntry, qh *QueueHandler) {
	if tq != nil && hasInProgressTasks(tq.Queue.Tasks) {
		syncAgentBusy(workerID, qh)
		return
	}
	syncAgentIdle(workerID, qh)
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
	syncAllConfiguredWorkers(qh, taskQueues)
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
//
// The implementation is dispatched through agentStatusSetter so tests can
// observe the (agentID, status) pairs without spawning a real tmux session.
// Production assigns realAgentStatusSetter on init; test code may swap in a
// recorder.
func syncAgentStatus(agentID, status string, qh *QueueHandler) {
	agentStatusSetter(agentID, status, qh)
}

// agentStatusSetter is the indirection point used by syncAgentStatus.
// Tests overwrite it (and restore it via t.Cleanup) so they can assert
// which workers got busy/idle without requiring a tmux server.
var agentStatusSetter = realAgentStatusSetter

// realAgentStatusSetter is the production implementation backing
// agentStatusSetter. Kept as a named function so tests can refer to the
// "real" path when they want to measure overhead.
func realAgentStatusSetter(agentID, status string, qh *QueueHandler) {
	paneTarget, err := tmux.FindPaneByAgentID(agentID)
	if err != nil {
		// Agent pane not found — expected when agents are not running.
		return
	}
	if err := tmux.SetUserVar(paneTarget, "status", status); err != nil {
		qh.log(LogLevelDebug, "status_sync_failed agent=%s status=%s: %v", agentID, status, err)
	}
}
