package daemon

import (
	"errors"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// isFenceStale checks whether a queue entry has been modified since Phase A
// by comparing lease epoch, status, and expiry. Used by Phase C apply methods
// for both dispatch results and busy-check results.
func isFenceStale(status model.Status, leaseEpoch int, leaseExpiresAt *string, expectedEpoch int, expectedExpiresAt string) bool {
	return leaseEpoch != expectedEpoch ||
		status != model.StatusInProgress ||
		leaseExpiresAt == nil ||
		*leaseExpiresAt != expectedExpiresAt
}

// isMaxInProgressTimeout checks whether the elapsed time since the given
// RFC3339 timestamp exceeds maxMin minutes. Returns false if the timestamp
// cannot be parsed (scan-safe: parse errors are treated as "not timed out").
func isMaxInProgressTimeout(now time.Time, timestampRFC3339 string, maxMin int) bool {
	t, err := time.Parse(time.RFC3339, timestampRFC3339)
	if err != nil {
		return false
	}
	return now.Sub(t) >= time.Duration(maxMin)*time.Minute
}

// buildGlobalInFlightSet scans ALL task queues to find workers with in_progress tasks
// that have valid (non-expired) leases. Keyed by worker ID derived from queue file path.
func (qh *QueueHandler) buildGlobalInFlightSet(taskQueues map[string]*taskQueueEntry) map[string]bool {
	inFlight := make(map[string]bool)
	for queueFile, tq := range taskQueues {
		workerID := workerIDFromPath(queueFile)
		if workerID == "" {
			continue
		}
		for _, task := range tq.Queue.Tasks {
			if task.Status == model.StatusInProgress && !qh.leaseManager.IsLeaseExpired(task.LeaseExpiresAt) {
				inFlight[workerID] = true
				break
			}
		}
	}
	return inFlight
}

// collectWorktreePhaseMerges detects phases that just completed and collects
// merge work items for Phase B execution. Runs in Phase A under scanMu.Lock.
// Only performs fast in-memory checks — all git I/O is deferred to Phase B.
// Skips phases that have already been merged (tracked in worktree command state).
func (qh *QueueHandler) collectWorktreePhaseMerges(commandID string) []worktreeMergeItem {
	if qh.dependencyResolver.stateReader == nil || qh.worktreeManager == nil {
		return nil
	}

	phases, err := qh.dependencyResolver.stateReader.GetCommandPhases(commandID)
	if err != nil {
		return nil
	}

	// Load worktree state to check already-merged phases
	cmdState, err := qh.worktreeManager.GetCommandState(commandID)
	if err != nil {
		return nil
	}

	var items []worktreeMergeItem
	for _, phase := range phases {
		if string(phase.Status) != "completed" {
			continue
		}
		// Skip phases already merged
		if cmdState.MergedPhases != nil {
			if _, merged := cmdState.MergedPhases[phase.ID]; merged {
				continue
			}
		}
		// Only merge if this phase has tasks
		if len(phase.RequiredTaskIDs) == 0 {
			continue
		}

		// Use only workers that actually have worktrees
		var workerIDs []string
		for _, ws := range cmdState.Workers {
			workerIDs = append(workerIDs, ws.WorkerID)
		}
		if len(workerIDs) == 0 {
			continue
		}

		items = append(items, worktreeMergeItem{
			CommandID: commandID,
			PhaseID:   phase.ID,
			WorkerIDs: workerIDs,
		})
	}

	return items
}

// hasExpiredLeases checks whether any queue entry has an expired lease.
// Used to decide whether to prioritize recovery over dispatch (spec §5.8.1).
func (qh *QueueHandler) hasExpiredLeases(
	taskQueues map[string]*taskQueueEntry,
	cq *model.CommandQueue,
	nq *model.NotificationQueue,
) bool {
	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusInProgress && qh.leaseManager.IsLeaseExpired(cmd.LeaseExpiresAt) {
			return true
		}
	}
	for _, tq := range taskQueues {
		for _, task := range tq.Queue.Tasks {
			if task.Status == model.StatusInProgress && qh.leaseManager.IsLeaseExpired(task.LeaseExpiresAt) {
				return true
			}
		}
	}
	for _, ntf := range nq.Notifications {
		if ntf.Status == model.StatusInProgress && qh.leaseManager.IsLeaseExpired(ntf.LeaseExpiresAt) {
			return true
		}
	}
	return false
}

// collectWorktreePublishAndCleanup checks if a command is ready for worktree
// publish-to-base or cleanup. Returns publish and cleanup items for Phase B.
// Runs in Phase A under scanMu.Lock — only fast checks and YAML reads.
func (qh *QueueHandler) collectWorktreePublishAndCleanup(
	commandID string,
	taskQueues map[string]*taskQueueEntry,
) ([]worktreePublishItem, []worktreeCleanupItem) {
	// Load worktree state
	cmdState, err := qh.worktreeManager.GetCommandState(commandID)
	if err != nil {
		return nil, nil
	}

	// Check if all tasks for this command are terminal
	allTerminal, hasFailed := qh.checkCommandTasksTerminal(commandID, taskQueues)
	if !allTerminal {
		return nil, nil
	}

	// For phased commands, also verify all phases are terminal.
	// Errors fail closed (skip publish) to avoid premature publishing.
	phases, err := qh.dependencyResolver.stateReader.GetCommandPhases(commandID)
	if err != nil {
		if !errors.Is(err, ErrStateNotFound) {
			qh.log(LogLevelWarn, "worktree_publish_phase_check_failed command=%s error=%v", commandID, err)
		}
		return nil, nil
	}
	for _, phase := range phases {
		if !model.IsPhaseTerminal(phase.Status) {
			return nil, nil
		}
	}

	var publishes []worktreePublishItem
	var cleanups []worktreeCleanupItem

	if hasFailed {
		// Don't publish if any task failed — partial results stay on integration branch
		qh.log(LogLevelInfo, "worktree_publish_skip_failed command=%s", commandID)
		if qh.config.Worktree.CleanupOnFailure {
			cleanups = append(cleanups, worktreeCleanupItem{
				CommandID: commandID,
				Reason:    "failure",
			})
		}
		return publishes, cleanups
	}

	// No failures — check integration status to decide action
	switch cmdState.Integration.Status {
	case model.IntegrationStatusMerged:
		// Ready to publish
		publishes = append(publishes, worktreePublishItem{
			CommandID: commandID,
		})
		qh.log(LogLevelInfo, "worktree_publish_collected command=%s", commandID)
	case model.IntegrationStatusPublished:
		// Already published — collect cleanup if configured and not yet cleaned
		if qh.config.Worktree.CleanupOnSuccess {
			cleanups = append(cleanups, worktreeCleanupItem{
				CommandID: commandID,
				Reason:    "success",
			})
		}
	default:
		// Not ready (created, merging, conflict, publishing, failed)
		qh.log(LogLevelDebug, "worktree_publish_not_ready command=%s integration_status=%s",
			commandID, cmdState.Integration.Status)
	}

	return publishes, cleanups
}

// checkCommandTasksTerminal checks if all tasks for a command across all task
// queues are in terminal state. Returns (allTerminal, hasFailed).
// Runs in Phase A under scanMu.Lock — iterates already-loaded in-memory queues.
func (qh *QueueHandler) checkCommandTasksTerminal(
	commandID string,
	taskQueues map[string]*taskQueueEntry,
) (bool, bool) {
	taskCount := 0
	hasFailed := false

	for _, tq := range taskQueues {
		for _, task := range tq.Queue.Tasks {
			if task.CommandID != commandID {
				continue
			}
			taskCount++
			if !model.IsTerminal(task.Status) {
				return false, false
			}
			if task.Status == model.StatusFailed || task.Status == model.StatusDeadLetter {
				hasFailed = true
			}
		}
	}

	if taskCount == 0 {
		return false, false // No tasks found — command not ready
	}
	return true, hasFailed
}
