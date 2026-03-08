package daemon

import (
	"context"
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
)

// periodicScanPhaseB executes all slow tmux I/O operations without holding any lock.
// Order: interrupts → busy checks → dispatches → signals (per Codex review).
// SRE-002: accepts context for cancellation support during slow I/O.
func (qh *QueueHandler) periodicScanPhaseB(ctx context.Context, pa phaseAResult) phaseBResult {
	var result phaseBResult

	// 1. Execute interrupts first (before dispatches to avoid killing new tasks)
	forEachUntilCanceled(ctx, pa.work.interrupts, func(item interruptItem) {
		if err := qh.cancelHandler.interruptAgent(item.WorkerID, item.TaskID, item.CommandID, item.Epoch); err != nil {
			qh.log(LogLevelWarn, "phase_b_interrupt worker=%s task=%s error=%v", item.WorkerID, item.TaskID, err)
		}
	})

	// 2. Execute busy probes for expired leases
	forEachUntilCanceled(ctx, pa.work.busyChecks, func(item busyCheckItem) {
		busy, undecided := qh.isAgentBusy(ctx, item.AgentID)
		result.busyChecks = append(result.busyChecks, busyCheckResult{
			Item:      item,
			Busy:      busy,
			Undecided: undecided,
		})
	})

	// 3. Execute dispatches
	forEachUntilCanceled(ctx, pa.work.dispatches, func(item dispatchItem) {
		var err error
		switch item.Kind {
		case "command":
			err = qh.dispatcher.DispatchCommand(item.Command)
		case "task":
			err = qh.dispatcher.DispatchTask(item.Task, item.WorkerID)
		case "notification":
			err = qh.dispatcher.DispatchNotification(item.Notification)
		}
		result.dispatches = append(result.dispatches, dispatchResult{
			Item:    item,
			Success: err == nil,
			Error:   err,
		})
	})

	// 4. Execute signal deliveries
	forEachUntilCanceled(ctx, pa.work.signals, func(item signalDeliveryItem) {
		err := qh.deliverPlannerSignal(ctx, item.CommandID, item.Message)
		result.signals = append(result.signals, signalDeliveryResult{
			Item:    item,
			Success: err == nil,
			Error:   err,
		})
	})

	// 5. Execute agent clears (fire-and-forget)
	forEachUntilCanceled(ctx, pa.work.clears, func(agentID string) {
		qh.clearAgent(ctx, agentID)
	})

	// 6. Execute worktree merges (slow git I/O, outside scanMu.Lock)
	forEachUntilCanceled(ctx, pa.work.worktreeMerges, func(item worktreeMergeItem) {
		mr := worktreeMergeResult{Item: item}

		// First commit worker changes
		if qh.worktreeManager != nil && qh.worktreeManager.AutoCommit() {
			for _, workerID := range item.WorkerIDs {
				msg := fmt.Sprintf("[maestro] auto-commit phase %s worker %s for %s",
					item.PhaseID, workerID, item.CommandID)
				if err := qh.worktreeManager.CommitWorkerChanges(item.CommandID, workerID, msg); err != nil {
					qh.log(LogLevelWarn, "worktree_auto_commit command=%s worker=%s error=%v",
						item.CommandID, workerID, err)
				}
			}
		}

		// Then merge to integration
		if qh.worktreeManager != nil && qh.worktreeManager.AutoMerge() {
			conflicts, err := qh.worktreeManager.MergeToIntegration(item.CommandID, item.WorkerIDs)
			mr.Conflicts = conflicts
			mr.Error = err

			if len(conflicts) == 0 && err == nil {
				if syncErr := qh.worktreeManager.SyncFromIntegration(item.CommandID, item.WorkerIDs); syncErr != nil {
					qh.log(LogLevelWarn, "worktree_sync_failed command=%s error=%v", item.CommandID, syncErr)
				}
			}
		}

		result.worktreeMerges = append(result.worktreeMerges, mr)
	})

	// 7. Execute worktree publishes (slow git I/O, outside scanMu.Lock)
	var additionalCleanups []worktreeCleanupItem
	forEachUntilCanceled(ctx, pa.work.worktreePublishes, func(item worktreePublishItem) {
		pr := worktreePublishResult{Item: item}
		if qh.worktreeManager != nil {
			cmdState, err := qh.worktreeManager.GetCommandState(item.CommandID)
			if err != nil || cmdState.Integration.Status != model.IntegrationStatusMerged {
				qh.log(LogLevelWarn, "worktree_publish_skip_stale command=%s status=%v err=%v",
					item.CommandID, func() string {
						if cmdState != nil {
							return string(cmdState.Integration.Status)
						}
						return "unknown"
					}(), err)
				pr.Error = fmt.Errorf("integration status no longer merged")
			} else {
				pr.Error = qh.worktreeManager.PublishToBase(item.CommandID)
			}
		}
		result.worktreePublishes = append(result.worktreePublishes, pr)

		if pr.Error == nil && qh.config.Worktree.CleanupOnSuccess {
			additionalCleanups = append(additionalCleanups, worktreeCleanupItem{
				CommandID: item.CommandID,
				Reason:    "success",
			})
		}
	})

	// 8. Execute worktree cleanups (Phase A collected + post-publish)
	allCleanups := append(pa.work.worktreeCleanups, additionalCleanups...)
	forEachUntilCanceled(ctx, allCleanups, func(item worktreeCleanupItem) {
		cr := worktreeCleanupResult{Item: item}
		if qh.worktreeManager != nil {
			cr.Error = qh.worktreeManager.CleanupCommand(item.CommandID)
		}
		result.worktreeCleanups = append(result.worktreeCleanups, cr)
	})

	return result
}
