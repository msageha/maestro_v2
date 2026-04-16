package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/model"
)

// classifyCommitError converts a CommitWorkerChanges error into a structured
// machine-readable Reason for commit_failed signals. The Reason lets the
// planner act on the failure category without parsing the error message.
func classifyCommitError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, worktree.ErrAllFilesFiltered) {
		return "all_files_filtered"
	}
	var policyErr *worktree.CommitPolicyViolationError
	if errors.As(err, &policyErr) {
		if len(policyErr.Violations) > 0 {
			return "policy_violation:" + policyErr.Violations[0].Code
		}
		return "policy_violation:unknown"
	}
	return "generic:" + err.Error()
}

// --- Phase B: entry point and step functions ---
// periodicScanPhaseB lives in ScanPhaseExecutor (scan_phase_executor.go).
// QueueHandler provides executePhaseBSteps as the single entry point.

// executePhaseBSteps runs all Phase B steps in the prescribed order.
// This is the single entry point called by ScanPhaseExecutor.periodicScanPhaseB.
func (qh *QueueHandler) executePhaseBSteps(ctx context.Context, pa *phaseAResult, result *phaseBResult) {
	qh.stepInterruptAgents(ctx, pa)
	qh.stepProbeBusyAgents(ctx, pa, result)
	qh.stepDispatchWork(ctx, pa, result)
	qh.stepDeliverSignals(ctx, pa, result)
	qh.stepLogPartialFailures(result)
	qh.stepClearAgents(ctx, pa)
	qh.stepCommitAndMergeWorktrees(ctx, pa, result)
	additionalCleanups := qh.stepPublishWorktrees(ctx, pa, result)
	qh.stepCleanupWorktrees(ctx, pa, result, additionalCleanups)
}

// stepInterruptAgents executes interrupt requests before dispatches to avoid
// killing newly dispatched tasks. After each interrupt, discards the worker's
// uncommitted worktree changes (H4).
func (qh *QueueHandler) stepInterruptAgents(ctx context.Context, pa *phaseAResult) {
	if err := forEachUntilCanceled(ctx, pa.work.interrupts, func(item interruptItem) {
		if err := qh.cancelHandler.interruptAgent(item.WorkerID, item.TaskID, item.CommandID, item.Epoch); err != nil {
			qh.log(LogLevelWarn, "phase_b_interrupt worker=%s task=%s error=%v", item.WorkerID, item.TaskID, err)
		}
		if qh.worktreeManager != nil && item.WorkerID != "" {
			if err := qh.worktreeManager.DiscardWorkerChanges(item.CommandID, item.WorkerID); err != nil {
				qh.log(LogLevelWarn, "phase_b_worktree_discard worker=%s task=%s error=%v",
					item.WorkerID, item.TaskID, err)
			}
		}
	}); err != nil {
		qh.log(LogLevelInfo, "phase_b_interrupts_canceled: %v", err)
	}
}

// stepProbeBusyAgents executes busy probes for expired leases.
func (qh *QueueHandler) stepProbeBusyAgents(ctx context.Context, pa *phaseAResult, result *phaseBResult) {
	if err := forEachUntilCanceled(ctx, pa.work.busyChecks, func(item busyCheckItem) {
		busy, undecided := qh.isAgentBusy(ctx, item.AgentID)
		result.busyChecks = append(result.busyChecks, busyCheckResult{
			Item:      item,
			Busy:      busy,
			Undecided: undecided,
		})
	}); err != nil {
		qh.log(LogLevelInfo, "phase_b_busy_checks_canceled: %v", err)
	}
}

// stepDispatchWork executes command, task, and notification dispatches.
func (qh *QueueHandler) stepDispatchWork(ctx context.Context, pa *phaseAResult, result *phaseBResult) {
	if err := forEachUntilCanceled(ctx, pa.work.dispatches, func(item dispatchItem) {
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
	}); err != nil {
		qh.log(LogLevelInfo, "phase_b_dispatches_canceled: %v", err)
	}
}

// stepDeliverSignals executes planner signal deliveries via tmux.
func (qh *QueueHandler) stepDeliverSignals(ctx context.Context, pa *phaseAResult, result *phaseBResult) {
	if err := forEachUntilCanceled(ctx, pa.work.signals, func(item signalDeliveryItem) {
		err := qh.deliverPlannerSignal(ctx, item.CommandID, item.Message)
		result.signals = append(result.signals, signalDeliveryResult{
			Item:    item,
			Success: err == nil,
			Error:   err,
		})
		if err != nil {
			qh.log(LogLevelWarn, "phase_b_signal_failed command=%s error=%v", item.CommandID, err)
			result.recoveryHints = append(result.recoveryHints,
				fmt.Sprintf("signal_delivery_failed command=%s: signal will be retried next scan, but planner may have stale view until then", item.CommandID))
		}
	}); err != nil {
		qh.log(LogLevelInfo, "phase_b_signals_canceled: %v", err)
	}
}

// stepLogPartialFailures logs a summary when dispatches or signals partially failed.
func (qh *QueueHandler) stepLogPartialFailures(result *phaseBResult) {
	if len(result.dispatches) == 0 && len(result.signals) == 0 {
		return
	}
	failedDispatches := 0
	for _, dr := range result.dispatches {
		if !dr.Success {
			failedDispatches++
		}
	}
	failedSignals := 0
	for _, sr := range result.signals {
		if !sr.Success {
			failedSignals++
		}
	}
	if failedDispatches > 0 || failedSignals > 0 {
		qh.log(LogLevelWarn, "phase_b_partial_failures dispatches_failed=%d/%d signals_failed=%d/%d",
			failedDispatches, len(result.dispatches), failedSignals, len(result.signals))
	}
}

// stepClearAgents executes agent clear operations (fire-and-forget).
func (qh *QueueHandler) stepClearAgents(ctx context.Context, pa *phaseAResult) {
	if err := forEachUntilCanceled(ctx, pa.work.clears, func(agentID string) {
		qh.clearAgent(ctx, agentID)
	}); err != nil {
		qh.log(LogLevelInfo, "phase_b_clears_canceled: %v", err)
	}
}

// stepCommitAndMergeWorktrees executes worktree commit and merge operations
// (slow git I/O, outside scanMu.Lock).
func (qh *QueueHandler) stepCommitAndMergeWorktrees(ctx context.Context, pa *phaseAResult, result *phaseBResult) {
	if err := forEachUntilCanceled(ctx, pa.work.worktreeMerges, func(item worktreeMergeItem) {
		mr := worktreeMergeResult{Item: item}

		// First commit worker changes, tracking failures
		committedWorkerIDs := item.WorkerIDs
		if qh.worktreeManager != nil && qh.worktreeManager.AutoCommit() {
			var succeeded []string
			for _, workerID := range item.WorkerIDs {
				msg := workerCommitMessage(item.WorkerPurposes, workerID)
				if qh.handleWorkerCommit(item.CommandID, workerID, msg, &mr) {
					succeeded = append(succeeded, workerID)
				}
			}
			committedWorkerIDs = succeeded
		}

		// Then merge to integration (only workers that committed successfully)
		if qh.worktreeManager != nil && qh.worktreeManager.AutoMerge() && len(committedWorkerIDs) > 0 {
			conflicts, err := qh.worktreeManager.MergeToIntegration(ctx, item.CommandID, committedWorkerIDs, item.WorkerPurposes)
			mr.Conflicts = conflicts
			mr.Error = err

			if len(conflicts) == 0 && err == nil {
				// Sync only the workers that successfully committed; workers
				// excluded due to commit failure must not be sync targets
				// because their dirty worktrees would block the merge anyway
				// and we do not want to advance their state.
				if syncErr := qh.worktreeManager.SyncFromIntegration(item.CommandID, committedWorkerIDs); syncErr != nil {
					qh.log(LogLevelWarn, "worktree_sync_failed command=%s workers=%v error=%v", item.CommandID, committedWorkerIDs, syncErr)
				}
			}
		}

		// All-failed transition: if every worker's commit failed there is
		// nothing to merge. Mark the integration branch Failed explicitly so
		// the planner can detect a permanent stall instead of seeing the
		// status frozen at Created/Merged.
		if qh.worktreeManager != nil && len(mr.CommitFailures) > 0 && len(committedWorkerIDs) == 0 {
			if markErr := qh.worktreeManager.MarkIntegrationFailed(item.CommandID); markErr != nil {
				qh.log(LogLevelWarn, "worktree_mark_integration_failed command=%s error=%v",
					item.CommandID, markErr)
			}
		}

		result.worktreeMerges = append(result.worktreeMerges, mr)
	}); err != nil {
		qh.log(LogLevelInfo, "phase_b_worktree_merges_canceled: %v", err)
	}
}

// stepPublishWorktrees executes worktree publish operations (slow git I/O).
// Returns additional cleanup items for successfully published commands.
func (qh *QueueHandler) stepPublishWorktrees(ctx context.Context, pa *phaseAResult, result *phaseBResult) []worktreeCleanupItem {
	var additionalCleanups []worktreeCleanupItem
	if err := forEachUntilCanceled(ctx, pa.work.worktreePublishes, func(item worktreePublishItem) {
		pr := worktreePublishResult{Item: item}
		if qh.worktreeManager != nil {
			cmdState, err := qh.worktreeManager.GetCommandState(item.CommandID)
			if err != nil || cmdState.Integration.Status != model.IntegrationStatusMerged {
				qh.log(LogLevelInfo, "worktree_publish_skip_stale command=%s status=%v err=%v",
					item.CommandID, func() string {
						if cmdState != nil {
							return string(cmdState.Integration.Status)
						}
						return "unknown"
					}(), err)
				pr.Error = fmt.Errorf("integration status no longer merged")
			} else {
				pr.Error = qh.worktreeManager.PublishToBase(item.CommandID, item.PublishMessage)
			}
		}
		result.worktreePublishes = append(result.worktreePublishes, pr)

		if pr.Error == nil && qh.config.Worktree.CleanupOnSuccess {
			additionalCleanups = append(additionalCleanups, worktreeCleanupItem{
				CommandID: item.CommandID,
				Reason:    "success",
			})
		}
	}); err != nil {
		qh.log(LogLevelInfo, "worktree_publishes_canceled: %v", err)
	}
	return additionalCleanups
}

// stepCleanupWorktrees executes worktree cleanup operations
// (Phase A collected items + post-publish additional cleanups).
func (qh *QueueHandler) stepCleanupWorktrees(ctx context.Context, pa *phaseAResult, result *phaseBResult, additionalCleanups []worktreeCleanupItem) {
	allCleanups := append(pa.work.worktreeCleanups, additionalCleanups...)
	if err := forEachUntilCanceled(ctx, allCleanups, func(item worktreeCleanupItem) {
		cr := worktreeCleanupResult{Item: item}
		if qh.worktreeManager != nil {
			cr.Error = qh.worktreeManager.CleanupCommand(item.CommandID)
		}
		result.worktreeCleanups = append(result.worktreeCleanups, cr)
	}); err != nil {
		qh.log(LogLevelInfo, "worktree_cleanups_canceled: %v", err)
	}
}

// handleWorkerCommit commits a single worker's changes and manages the
// commit-failed marker. Returns true if the commit succeeded.
func (qh *QueueHandler) handleWorkerCommit(commandID, workerID, msg string, mr *worktreeMergeResult) bool {
	if err := qh.worktreeManager.CommitWorkerChanges(commandID, workerID, msg); err != nil {
		reason := classifyCommitError(err)
		qh.log(LogLevelWarn, "worktree_auto_commit command=%s worker=%s reason=%s error=%v",
			commandID, workerID, reason, err)
		mr.CommitFailures = append(mr.CommitFailures, commitFailure{
			WorkerID: workerID,
			Error:    err,
			Reason:   reason,
		})
		// Persist commit-failed marker so the publish gate blocks until cleared.
		if recErr := qh.worktreeManager.AddCommitFailedWorker(commandID, workerID); recErr != nil {
			qh.log(LogLevelWarn, "worktree_record_commit_failed command=%s worker=%s error=%v",
				commandID, workerID, recErr)
		}
		return false
	}
	// Clear any prior commit-failed marker on successful retry.
	if clrErr := qh.worktreeManager.RemoveCommitFailedWorker(commandID, workerID); clrErr != nil {
		qh.log(LogLevelWarn, "worktree_clear_commit_failed command=%s worker=%s error=%v",
			commandID, workerID, clrErr)
	}
	return true
}

const autoCommitFallbackMessage = "auto-commit: worker changes"

// workerCommitMessage returns the commit message for a worker's auto-commit.
// Uses the task purpose if available, truncated to 72 characters.
// Falls back to a generic message if purpose is empty.
func workerCommitMessage(workerPurposes map[string]string, workerID string) string {
	purpose := workerPurposes[workerID]
	if purpose == "" {
		return autoCommitFallbackMessage
	}
	if len(purpose) > 72 {
		purpose = purpose[:72]
	}
	return purpose
}
