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

// periodicScanPhaseB executes all slow tmux I/O operations without holding any lock.
// Order: interrupts → busy checks → dispatches → signals (per Codex review).
// SRE-002: accepts context for cancellation support during slow I/O.
func (qh *QueueHandler) periodicScanPhaseB(ctx context.Context, pa phaseAResult) phaseBResult {
	result := phaseBResult{
		busyChecks:        make([]busyCheckResult, 0, len(pa.work.busyChecks)),
		dispatches:        make([]dispatchResult, 0, len(pa.work.dispatches)),
		signals:           make([]signalDeliveryResult, 0, len(pa.work.signals)),
		recoveryHints:     make([]string, 0, len(pa.work.signals)),
		worktreeMerges:    make([]worktreeMergeResult, 0, len(pa.work.worktreeMerges)),
		worktreePublishes: make([]worktreePublishResult, 0, len(pa.work.worktreePublishes)),
		worktreeCleanups:  make([]worktreeCleanupResult, 0, len(pa.work.worktreeCleanups)+len(pa.work.worktreePublishes)),
	}

	// 1. Execute interrupts first (before dispatches to avoid killing new tasks).
	// H4: discard the worker's uncommitted worktree changes ONLY after the
	// tmux interrupt has been delivered, so the worker process is no longer
	// holding files in its working tree when the reset runs.
	forEachUntilCanceled(ctx, pa.work.interrupts, func(item interruptItem) {
		if err := qh.cancelHandler.interruptAgent(item.WorkerID, item.TaskID, item.CommandID, item.Epoch); err != nil {
			qh.log(LogLevelWarn, "phase_b_interrupt worker=%s task=%s error=%v", item.WorkerID, item.TaskID, err)
		}
		if qh.worktreeManager != nil && item.WorkerID != "" {
			if err := qh.worktreeManager.DiscardWorkerChanges(item.CommandID, item.WorkerID); err != nil {
				qh.log(LogLevelWarn, "phase_b_worktree_discard worker=%s task=%s error=%v",
					item.WorkerID, item.TaskID, err)
			}
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
		if err != nil {
			qh.log(LogLevelWarn, "phase_b_signal_failed command=%s error=%v", item.CommandID, err)
			result.recoveryHints = append(result.recoveryHints,
				fmt.Sprintf("signal_delivery_failed command=%s: signal will be retried next scan, but planner may have stale view until then", item.CommandID))
		}
	})

	// Log partial failure summary: dispatches succeeded but related signals failed
	if len(result.dispatches) > 0 || len(result.signals) > 0 {
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

	// 5. Execute agent clears (fire-and-forget)
	forEachUntilCanceled(ctx, pa.work.clears, func(agentID string) {
		qh.clearAgent(ctx, agentID)
	})

	// 6. Execute worktree merges (slow git I/O, outside scanMu.Lock)
	forEachUntilCanceled(ctx, pa.work.worktreeMerges, func(item worktreeMergeItem) {
		mr := worktreeMergeResult{Item: item}

		// First commit worker changes, tracking failures
		committedWorkerIDs := item.WorkerIDs
		if qh.worktreeManager != nil && qh.worktreeManager.AutoCommit() {
			var succeeded []string
			for _, workerID := range item.WorkerIDs {
				msg := workerCommitMessage(item.WorkerPurposes, workerID)
				if err := qh.worktreeManager.CommitWorkerChanges(item.CommandID, workerID, msg); err != nil {
					reason := classifyCommitError(err)
					qh.log(LogLevelWarn, "worktree_auto_commit command=%s worker=%s reason=%s error=%v",
						item.CommandID, workerID, reason, err)
					mr.CommitFailures = append(mr.CommitFailures, commitFailure{
						WorkerID: workerID,
						Error:    err,
						Reason:   reason,
					})
					// Persist commit-failed marker so the publish gate blocks until cleared.
					if recErr := qh.worktreeManager.AddCommitFailedWorker(item.CommandID, workerID); recErr != nil {
						qh.log(LogLevelWarn, "worktree_record_commit_failed command=%s worker=%s error=%v",
							item.CommandID, workerID, recErr)
					}
				} else {
					succeeded = append(succeeded, workerID)
					// Clear any prior commit-failed marker on successful retry.
					if clrErr := qh.worktreeManager.RemoveCommitFailedWorker(item.CommandID, workerID); clrErr != nil {
						qh.log(LogLevelWarn, "worktree_clear_commit_failed command=%s worker=%s error=%v",
							item.CommandID, workerID, clrErr)
					}
				}
			}
			committedWorkerIDs = succeeded
		}

		// Then merge to integration (only workers that committed successfully)
		if qh.worktreeManager != nil && qh.worktreeManager.AutoMerge() && len(committedWorkerIDs) > 0 {
			conflicts, err := qh.worktreeManager.MergeToIntegration(item.CommandID, committedWorkerIDs, item.WorkerPurposes)
			mr.Conflicts = conflicts
			mr.Error = err

			if len(conflicts) == 0 && err == nil {
				// Sync only the workers that successfully committed; workers
				// excluded due to commit failure must not be sync targets
				// because their dirty worktrees would block the merge anyway
				// and we do not want to advance their state.
				if syncErr := qh.worktreeManager.SyncFromIntegration(item.CommandID, committedWorkerIDs); syncErr != nil {
					qh.log(LogLevelWarn, "worktree_sync_failed command=%s error=%v", item.CommandID, syncErr)
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
