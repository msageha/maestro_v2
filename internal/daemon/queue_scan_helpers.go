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
func (qh *QueueHandler) collectWorktreePhaseMerges(commandID string, taskQueues map[string]*taskQueueEntry) []worktreeMergeItem {
	if !qh.dependencyResolver.HasStateReader() || qh.worktreeManager == nil {
		return nil
	}

	phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(commandID)
	if err != nil {
		return nil
	}

	// Load worktree state to check already-merged phases
	cmdState, err := qh.worktreeManager.GetCommandState(commandID)
	if err != nil {
		return nil
	}

	// H10: do not collect any merge work for quarantined integrations.
	// Quarantined is terminal and requires manual operator intervention; without
	// this gate Phase B would re-enter MergeToIntegration on every scan and
	// either spin on the early-return error or attempt mutating git ops.
	if cmdState.Integration.Status == model.IntegrationStatusQuarantined {
		return nil
	}

	// Build workerID → task purpose map from task queues
	workerPurposes := buildWorkerPurposes(commandID, taskQueues)

	// Phase 0 件フォールバック: phases が一切定義されていない command でも
	// worker worktree への書き込みは発生しうる。この場合、全タスク終了かつ
	// 失敗なしの条件で暗黙の単一フェーズとして merge を集約する。
	if len(phases) == 0 {
		return qh.collectImplicitWorktreeMerge(commandID, cmdState, taskQueues, workerPurposes)
	}

	// Build workerIDs once outside the phase loop
	workerIDs := make([]string, 0, len(cmdState.Workers))
	for _, ws := range cmdState.Workers {
		workerIDs = append(workerIDs, ws.WorkerID)
	}
	if len(workerIDs) == 0 {
		return nil
	}

	items := make([]worktreeMergeItem, 0, len(phases))
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

		items = append(items, worktreeMergeItem{
			CommandID:      commandID,
			PhaseID:        phase.ID,
			WorkerIDs:      workerIDs,
			WorkerPurposes: workerPurposes,
		})
	}

	return items
}

// buildWorkerPurposes builds a workerID → task purpose map from task queues.
// Uses the most recently dispatched task's purpose for each worker.
// taskQueues is keyed by queue file path, so we iterate all entries.
func buildWorkerPurposes(_ string, taskQueues map[string]*taskQueueEntry) map[string]string {
	purposes := make(map[string]string)
	for _, tqEntry := range taskQueues {
		for _, task := range tqEntry.Queue.Tasks {
			if task.LeaseOwner != nil && *task.LeaseOwner != "" && task.Purpose != "" {
				purposes[*task.LeaseOwner] = task.Purpose
			}
		}
	}
	if len(purposes) == 0 {
		return nil
	}
	return purposes
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
	commandContent string,
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
	phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(commandID)
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
		// Block publish if any worker had an unresolved auto-commit failure.
		// Otherwise, partially-committed phases would publish unmerged worker changes.
		if len(cmdState.CommitFailedWorkers) > 0 {
			qh.log(LogLevelWarn, "worktree_publish_blocked_commit_failed command=%s workers=%v",
				commandID, cmdState.CommitFailedWorkers)
			return publishes, cleanups
		}
		// Ready to publish
		publishes = append(publishes, worktreePublishItem{
			CommandID:      commandID,
			PublishMessage: commandContent,
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

// collectImplicitWorktreeMerge は phases が一切定義されていない command 向けに
// 暗黙の単一フェーズ ("__implicit_phase") として worktree merge を集約する。
// 全タスク terminal かつ failed なしかつ Integration.Status==created で
// worker が登録されている場合のみ 1 件返す。それ以外は nil。
// collectWorktreePhaseMerges の phases==0 経路から呼ばれる。
func (qh *QueueHandler) collectImplicitWorktreeMerge(
	commandID string,
	cmdState *model.WorktreeCommandState,
	taskQueues map[string]*taskQueueEntry,
	workerPurposes map[string]string,
) []worktreeMergeItem {
	if cmdState == nil {
		return nil
	}
	// Allow re-collection for created, partial_merge, conflict, and failed.
	// The state transition table already permits these → merging.
	switch cmdState.Integration.Status {
	case model.IntegrationStatusCreated,
		model.IntegrationStatusPartialMerge,
		model.IntegrationStatusConflict,
		model.IntegrationStatusFailed:
		// eligible for (re-)merge collection
	default:
		return nil
	}
	// Prevent double-merge: if __implicit_phase is already in MergedPhases, skip.
	if cmdState.MergedPhases != nil {
		if _, merged := cmdState.MergedPhases["__implicit_phase"]; merged {
			return nil
		}
	}
	if len(cmdState.Workers) == 0 {
		return nil
	}
	allTerm, hasFailed := qh.checkCommandTasksTerminal(commandID, taskQueues)
	if !allTerm || hasFailed {
		return nil
	}

	workerIDs := make([]string, 0, len(cmdState.Workers))
	for _, ws := range cmdState.Workers {
		workerIDs = append(workerIDs, ws.WorkerID)
	}
	if len(workerIDs) == 0 {
		return nil
	}

	return []worktreeMergeItem{{
		CommandID:      commandID,
		PhaseID:        "__implicit_phase",
		WorkerIDs:      workerIDs,
		WorkerPurposes: workerPurposes,
	}}
}
