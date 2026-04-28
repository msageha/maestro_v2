package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/model"
)

// errIntegrationNotPublishable is a sentinel error indicating that a publish
// was skipped because the integration status is not in a publishable state.
// This is expected during conflict recovery (e.g. partial_merge, conflict,
// resolving states) and should not be logged at ERROR level.
var errIntegrationNotPublishable = errors.New("integration status not publishable")

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
// It tracks per-kind success/failure counts and appends a recovery hint
// when partial dispatch is detected (some succeeded, some failed).
func (qh *QueueHandler) stepDispatchWork(ctx context.Context, pa *phaseAResult, result *phaseBResult) {
	if err := forEachUntilCanceled(ctx, pa.work.dispatches, func(item dispatchItem) {
		var err error
		switch item.Kind {
		case "command":
			qh.classifyAndLogCommand(item.Command)
			err = qh.dispatcher.DispatchCommand(ctx, item.Command)
		case "task":
			if qh.isTaskDispatchCancelled(item, pa) {
				err = fmt.Errorf("dispatch blocked: command %s cancel-requested", item.Task.CommandID)
			} else {
				qh.classifyAndLogTask(item.Task, item.WorkerID)
				err = qh.dispatcher.DispatchTask(ctx, item.Task, item.WorkerID)
			}
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

	qh.trackPartialDispatch(result)
}

// trackPartialDispatch examines dispatch results and, when some dispatches
// succeeded while others failed, logs a detailed warning per kind and appends
// a recovery hint so Phase C and downstream logging can reference the state.
//
// uncertain (ErrSubmitConfirmUncertain) outcomes are tracked separately from
// genuine failures: the upstream agent dispatch already landed the paste in
// the worker pane and the queue path immediately emits
// `dispatch_uncertain_assume_running` to keep the lease and let the worker
// proceed. Counting them as `failed` produced the 2026-04 retest7/8 noise
// where every successful task execution still surfaced a
// `phase_b_partial_dispatch ... failed=1` WARN. Splitting the buckets means
// the WARN only fires on actual failures and the uncertain count is
// surfaced as INFO so operators see both signals without being misled.
func (qh *QueueHandler) trackPartialDispatch(result *phaseBResult) {
	if len(result.dispatches) == 0 {
		return
	}

	type kindCounts struct {
		succeeded int
		failed    int
		uncertain int
	}
	counts := make(map[string]*kindCounts)
	totalSucceeded := 0
	totalFailed := 0
	totalUncertain := 0

	for _, dr := range result.dispatches {
		kc, ok := counts[dr.Item.Kind]
		if !ok {
			kc = &kindCounts{}
			counts[dr.Item.Kind] = kc
		}
		switch {
		case dr.Success:
			kc.succeeded++
			totalSucceeded++
		case errors.Is(dr.Error, agent.ErrSubmitConfirmUncertain):
			kc.uncertain++
			totalUncertain++
		default:
			kc.failed++
			totalFailed++
		}
	}

	total := totalSucceeded + totalFailed + totalUncertain

	// Uncertain-only batches: paste landed for those entries; the
	// queue-side dispatch_uncertain_assume_running already provides
	// per-entry visibility. Surface a single INFO summary so operator
	// dashboards show the count without escalating to WARN.
	if totalSucceeded > 0 && totalFailed == 0 && totalUncertain > 0 {
		qh.log(LogLevelInfo,
			"phase_b_dispatch_with_uncertain: total=%d succeeded=%d uncertain=%d "+
				"(uncertain entries kept lease via dispatch_uncertain_assume_running; not a delivery failure)",
			total, totalSucceeded, totalUncertain)
		return
	}

	// Only WARN-log + add recovery hint when at least one *genuine*
	// failure occurred AND something else happened (success or uncertain).
	// All-failure batches are surfaced by the per-entry dispatch_failed
	// log lines elsewhere.
	if totalFailed == 0 || (totalSucceeded == 0 && totalUncertain == 0) {
		return
	}

	qh.log(LogLevelWarn,
		"phase_b_partial_dispatch: total=%d succeeded=%d failed=%d uncertain=%d",
		total, totalSucceeded, totalFailed, totalUncertain)
	for kind, kc := range counts {
		if kc.failed > 0 {
			qh.log(LogLevelWarn,
				"phase_b_partial_dispatch_detail: kind=%s succeeded=%d failed=%d uncertain=%d",
				kind, kc.succeeded, kc.failed, kc.uncertain)
		}
	}

	hint := fmt.Sprintf("partial_dispatch: total=%d succeeded=%d failed=%d uncertain=%d",
		total, totalSucceeded, totalFailed, totalUncertain)
	for kind, kc := range counts {
		hint += fmt.Sprintf(" %s_ok=%d %s_fail=%d %s_uncertain=%d",
			kind, kc.succeeded, kind, kc.failed, kind, kc.uncertain)
	}
	hint += "; failed dispatches will be retried in the next scan cycle"
	result.recoveryHints = append(result.recoveryHints, hint)
}

// signalCascadeBreakThreshold sets the consecutive-failure count at which
// stepDeliverSignals stops attempting further deliveries during the current
// scan tick. Picked at 5 because: (a) it lets a small batch of unrelated
// transient errors still finish the tick (e.g. one stuck pane plus a few
// healthy ones), and (b) once we hit 5 in a row the cause is almost always
// tmux server-wide degradation (load-buffer / send-keys timing out across
// every planner pane), not per-signal. Without the gate, a long-running
// degradation produced thousands of identical "phase_b_signal_failed"
// log lines per scan and burned scan-cycle CPU on doomed deliveries — the
// 2026-04-27 retest log showed signal_delivery_failed accumulating into the
// 10k range before daemon shutdown. The remaining signals are retained in
// the queue (no Attempts increment, no NextAttemptAt update) so the next
// scan retries from the same point once tmux recovers.
const signalCascadeBreakThreshold = 5

// sustainedCascadeBreakThreshold sets the consecutive scan-tick count at
// which the cross-scan meta-circuit fires. Per-tick cascade-break already
// caps the log/CPU spend within a scan; this counter promotes "we have
// degraded for N ticks in a row" to an operator-facing ERROR so the
// long-tail tmux-server outage is not silently absorbed by the per-tick
// gate. Picked at 3 to align with one ~scanInterval × 3 window, long
// enough to rule out a single transient hiccup and short enough that the
// alert lands while a human can still act.
const sustainedCascadeBreakThreshold = 3

// signalCascadeTracker counts consecutive delivery failures and trips into a
// "broken" state once threshold is reached. broken stays sticky for the rest
// of the current scan tick. Threshold <= 0 disables the tracker.
type signalCascadeTracker struct {
	threshold   int
	consecutive int
	broken      bool
}

// recordFailure increments the consecutive failure counter and returns the
// post-increment broken state. Once broken, subsequent recordFailure calls
// remain broken until the tracker is discarded at the end of the scan tick.
func (s *signalCascadeTracker) recordFailure() (nowBroken bool) {
	s.consecutive++
	if s.threshold > 0 && s.consecutive >= s.threshold {
		s.broken = true
	}
	return s.broken
}

// recordSuccess resets the consecutive failure counter. Once broken, it stays
// broken for the rest of the scan; only intermittent failures with successes
// in between hold the tracker in the unbroken band.
func (s *signalCascadeTracker) recordSuccess() {
	if s.broken {
		return
	}
	s.consecutive = 0
}

// isBroken reports whether the tracker has tripped past the threshold.
func (s *signalCascadeTracker) isBroken() bool { return s.broken }

// stepDeliverSignals executes planner signal deliveries via tmux.
func (qh *QueueHandler) stepDeliverSignals(ctx context.Context, pa *phaseAResult, result *phaseBResult) {
	cascade := signalCascadeTracker{threshold: signalCascadeBreakThreshold}
	skipped := 0
	totalSignals := len(pa.work.signals)

	if err := forEachUntilCanceled(ctx, pa.work.signals, func(item signalDeliveryItem) {
		if cascade.isBroken() {
			skipped++
			return
		}
		err := qh.deliverPlannerSignal(ctx, item.CommandID, item.Message)
		result.signals = append(result.signals, signalDeliveryResult{
			Item:    item,
			Success: err == nil,
			Error:   err,
		})
		if err != nil {
			tripped := cascade.recordFailure()
			qh.log(LogLevelWarn, "phase_b_signal_failed command=%s error=%v", item.CommandID, err)
			result.recoveryHints = append(result.recoveryHints,
				fmt.Sprintf("signal_delivery_failed command=%s: signal will be retried next scan, but planner may have stale view until then", item.CommandID))
			if tripped {
				qh.log(LogLevelWarn,
					"phase_b_signal_cascade_break consecutive_failures=%d threshold=%d remaining=%d "+
						"(tmux delivery appears degraded; deferring remaining signals to next scan to avoid log/CPU storm)",
					cascade.consecutive, signalCascadeBreakThreshold, totalSignals-len(result.signals))
			}
			return
		}
		cascade.recordSuccess()
	}); err != nil {
		qh.log(LogLevelInfo, "phase_b_signals_canceled: %v", err)
	}

	if skipped > 0 {
		qh.log(LogLevelWarn,
			"phase_b_signal_cascade_skipped count=%d (deferred to next scan; entries retained with Attempts unchanged)",
			skipped)
		result.recoveryHints = append(result.recoveryHints,
			fmt.Sprintf("signal_cascade_break skipped=%d: signal queue paused this tick after %d consecutive failures; tmux likely needs operator attention",
				skipped, signalCascadeBreakThreshold))
	}

	qh.recordCascadeBreakOutcome(cascade.isBroken(), totalSignals)
}

// recordCascadeBreakOutcome promotes the per-tick cascade-break observation
// into the daemon-level meta-circuit. Called at the end of every
// stepDeliverSignals run.
//
//   - tripped=true: increment the consecutive-tick counter; on every tick from
//     sustainedCascadeBreakThreshold onward emit an ERROR-level
//     tmux_delivery_sustained_degradation line so the long-tail outage is
//     visible above the per-tick WARN noise.
//   - tripped=false (with at least one signal attempted): reset the counter.
//     If the previous value was non-zero we log a one-shot recovery line so
//     dashboards / log scanners see a clean transition out of the degraded
//     window.
//
// The "no signals this tick" case is a no-op — we cannot tell apart healthy
// quiet from suppressed-by-cascade quiet without a delivery attempt, so we
// hold the counter steady. The next tick that actually fires deliveries will
// re-evaluate.
func (qh *QueueHandler) recordCascadeBreakOutcome(tripped bool, totalSignals int) {
	if tripped {
		breaks := qh.consecutiveCascadeBreakScans.Add(1)
		if breaks >= sustainedCascadeBreakThreshold {
			qh.log(LogLevelError,
				"tmux_delivery_sustained_degradation consecutive_scan_breaks=%d threshold=%d "+
					"(signal cascade-break has tripped for %d consecutive scans; tmux server / planner pane likely needs operator intervention — `maestro down && maestro up` or kill-server)",
				breaks, sustainedCascadeBreakThreshold, breaks)
		}
		return
	}
	if totalSignals == 0 {
		return // no delivery attempted this tick — counter held steady
	}
	if prev := qh.consecutiveCascadeBreakScans.Swap(0); prev > 0 {
		qh.log(LogLevelInfo,
			"tmux_delivery_recovered prior_consecutive_scan_breaks=%d "+
				"(signal delivery succeeded this tick; clearing meta-circuit counter)",
			prev)
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
				if qh.handleWorkerCommit(item.CommandID, workerID, msg, item.WorkerExpectedPaths[workerID], &mr) {
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
			if err != nil || (cmdState.Integration.Status != model.IntegrationStatusMerged &&
				cmdState.Integration.Status != model.IntegrationStatusPublishFailed) {
				qh.log(LogLevelInfo, "worktree_publish_skip_stale command=%s status=%v err=%v",
					item.CommandID, func() string {
						if cmdState != nil {
							return string(cmdState.Integration.Status)
						}
						return "unknown"
					}(), err)
				pr.Error = errIntegrationNotPublishable
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
//
// 2026-04-28: each cleanup now respawns the worker panes attached to the
// command into the project root before `git worktree remove` runs. The
// worker pane's cwd is the worktree path, so deleting the worktree
// underneath a still-open claude-code process leaves any subsequent
// posix_spawn (typically Stop hook) failing with ENOENT for /bin/sh —
// node.js reports the chdir failure as if the binary itself were
// missing. The pane respawn is best-effort: a failure aborts the
// matching CleanupCommand call so the next scan retries rather than
// deleting a worktree that still has a pane pointed at it.
func (qh *QueueHandler) stepCleanupWorktrees(ctx context.Context, pa *phaseAResult, result *phaseBResult, additionalCleanups []worktreeCleanupItem) {
	allCleanups := append(pa.work.worktreeCleanups, additionalCleanups...)
	if err := forEachUntilCanceled(ctx, allCleanups, func(item worktreeCleanupItem) {
		cr := worktreeCleanupResult{Item: item}
		if qh.worktreeManager != nil {
			if respErr := qh.respawnWorkerPanesForCleanup(item.CommandID); respErr != nil {
				qh.log(LogLevelWarn,
					"worktree_cleanup_pane_respawn_failed command=%s error=%v "+
						"(skipping cleanup; next scan will retry once the pane is evictable)",
					item.CommandID, respErr)
				cr.Error = respErr
				result.worktreeCleanups = append(result.worktreeCleanups, cr)
				return
			}
			cr.Error = qh.worktreeManager.CleanupCommand(item.CommandID)
		}
		result.worktreeCleanups = append(result.worktreeCleanups, cr)
	}); err != nil {
		qh.log(LogLevelInfo, "worktree_cleanups_canceled: %v", err)
	}
}

// respawnWorkerPanesForCleanup evicts every worker pane attached to
// commandID from the worktree's directory ahead of the worktree being
// removed. Returns the first non-nil RespawnPaneToProjectRoot error so
// the caller can defer the cleanup; missing state, missing executor, or
// an empty worker list are silently treated as "nothing to evict".
func (qh *QueueHandler) respawnWorkerPanesForCleanup(commandID string) error {
	state, err := qh.worktreeManager.GetCommandState(commandID)
	if err != nil {
		// State already torn down or never created — proceed with
		// cleanup unblocked. The worktree dir itself may not exist,
		// in which case CleanupCommand is a no-op.
		qh.log(LogLevelDebug,
			"worktree_cleanup_state_load command=%s error=%v (skipping pane respawn, proceeding to cleanup)",
			commandID, err)
		return nil
	}
	if state == nil || len(state.Workers) == 0 {
		return nil
	}
	exec, err := qh.execProvider.GetExecutor()
	if err != nil {
		// Daemon shutting down or test harness without an executor.
		// Cleanup is still safe to attempt — without a live pane there
		// is no posix_spawn ENOENT path to worry about.
		qh.log(LogLevelDebug,
			"worktree_cleanup_executor_unavailable command=%s error=%v (proceeding without pane respawn)",
			commandID, err)
		return nil
	}
	var firstErr error
	for _, ws := range state.Workers {
		if err := exec.RespawnPaneToProjectRoot(ws.WorkerID); err != nil {
			qh.log(LogLevelWarn,
				"worktree_cleanup_pane_respawn worker=%s command=%s error=%v",
				ws.WorkerID, commandID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// handleWorkerCommit commits a single worker's changes and manages the
// commit-failed marker. Returns true if the commit succeeded.
//
// Workers owned by the resume-merge pipeline (Conflict/Resolving) return
// ErrWorkerOwnedByResumeMerge; this is treated as "skip, not a failure" so the
// worker is excluded from the merge batch without recording a commit_failed
// signal or feeding the commit_failed_workers publish gate.
type expectedPathCommitter interface {
	CommitWorkerChangesWithExpectedPaths(commandID, workerID, message string, expectedPaths []string) error
}

func (qh *QueueHandler) handleWorkerCommit(commandID, workerID, msg string, expectedPaths []string, mr *worktreeMergeResult) bool {
	var err error
	if c, ok := qh.worktreeManager.(expectedPathCommitter); ok {
		err = c.CommitWorkerChangesWithExpectedPaths(commandID, workerID, msg, expectedPaths)
	} else {
		err = qh.worktreeManager.CommitWorkerChanges(commandID, workerID, msg)
	}
	if err != nil {
		if errors.Is(err, worktree.ErrWorkerOwnedByResumeMerge) {
			qh.log(LogLevelDebug, "worktree_auto_commit_skipped command=%s worker=%s reason=resume_merge_owned",
				commandID, workerID)
			return false
		}
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
		// Cut at 72 bytes, then strip any partial UTF-8 sequence at the tail
		// so multi-byte runes (e.g. Japanese task purposes) are not split
		// mid-rune. strings.ToValidUTF8 with empty replacement removes invalid
		// trailing bytes; result is always <= 72 bytes.
		purpose = strings.ToValidUTF8(purpose[:72], "")
	}
	return purpose
}
