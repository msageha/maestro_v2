package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// stepWorktreeStallDetection — Step 0.7.3: Detect commands whose tasks and
// phases are all terminal but whose integration branch remains stuck in
// {created, merged} for longer than the configured stall timeout. Emits a
// worktree_stalled planner signal once per command (deduped via the
// integration state's StallSignaled flag). If signal persistence fails, the
// integration is transitioned to Failed so the command is not re-detected
// indefinitely.
func (qh *QueueHandler) stepWorktreeStallDetection(s *scanState) {
	if qh.worktreeManager == nil || !qh.dependencyResolver.HasStateReader() {
		return
	}
	if !qh.config.Worktree.Enabled {
		return
	}
	timeoutMin := qh.config.Worktree.EffectiveStallTimeoutMinutes()
	if timeoutMin <= 0 {
		return
	}

	now := qh.clock.Now()
	threshold := now.Add(-time.Duration(timeoutMin) * time.Minute)

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil {
			continue
		}
		// Stall detection applies only to integrations that are stuck in
		// "merge has not happened despite all tasks being terminal"
		// (IntegrationStatusCreated). The previous version also fired on
		// IntegrationStatusMerged — but `merged` is the natural state
		// between merge completion and publish, and the publish gate
		// (collectWorktreePublishAndCleanup) reliably advances it within
		// one or two scans. Operators reported 'worktree_stall_signal_emitted
		// reason=integration_stalled:merged' immediately followed by a
		// successful publish — pure noise. Removing the merged branch
		// keeps the stall machinery focused on the only state where it
		// produces actionable signal.
		if cmdState.Integration.Status != model.IntegrationStatusCreated {
			continue
		}
		if cmdState.Integration.StallSignaled {
			continue
		}
		if !qh.allPhasesAndTasksTerminal(cmd.ID, s.tasks) {
			continue
		}

		// Stall fast-path for commands with no declared phases (already
		// guarded above to IntegrationStatusCreated only): apply the same
		// timeoutMin-based check so flat-task plans are not falsely
		// flagged as stalled before the timeout elapses.
		phases, perr := qh.dependencyResolver.GetStateReader().GetCommandPhases(cmd.ID)
		noPhases := false
		if perr != nil {
			if errors.Is(perr, ErrStateNotFound) {
				noPhases = true
			}
		} else if len(phases) == 0 {
			noPhases = true
		}
		if noPhases {
			qh.emitWorktreeStallSignal(cmd, s, now, threshold, "integration_stalled_no_phases:created")
			continue
		}

		reason := fmt.Sprintf("integration_stalled:%s", cmdState.Integration.Status)
		qh.emitWorktreeStallSignal(cmd, s, now, threshold, reason)
	}
}

// emitWorktreeStallSignal handles the common logic for worktree stall detection:
// timestamp parsing from cmd.UpdatedAt/CreatedAt, threshold check, signal
// emission, and MarkIntegrationStallSignaled with fallback to MarkIntegrationFailed.
// All exit paths leave the caller free to advance to the next command — the
// function is a "fire-and-forget" stall handler.
func (qh *QueueHandler) emitWorktreeStallSignal(cmd *model.Command, s *scanState, now time.Time, threshold time.Time, reason string) {
	ref := cmd.UpdatedAt
	if ref == "" {
		ref = cmd.CreatedAt
	}
	refTime, err := qh.timeCache.ParseRFC3339(ref)
	if err != nil {
		return // skip command on parse error
	}
	if !refTime.Before(threshold) {
		return // not stalled yet
	}

	stalledSince := refTime.UTC().Format(time.RFC3339)
	nowStr := now.UTC().Format(time.RFC3339)
	msg := fmt.Sprintf("[maestro] kind:worktree_stalled command_id:%s\nreason: %s\nstalled_since: %s",
		cmd.ID, reason, stalledSince)

	qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
		Kind:      "worktree_stalled",
		CommandID: cmd.ID,
		Reason:    reason,
		Message:   msg,
		CreatedAt: nowStr,
		UpdatedAt: nowStr,
	}, s.signalIndex)

	markFn := qh.scanExecutor.worktreeStallMarkFn
	if markFn == nil {
		markFn = qh.worktreeManager.MarkIntegrationStallSignaled
	}
	if err := markFn(cmd.ID); err != nil {
		qh.log(LogLevelWarn, "worktree_stall_mark_failed command=%s error=%v", cmd.ID, err)
		if mfErr := qh.worktreeManager.MarkIntegrationFailed(cmd.ID); mfErr != nil {
			qh.log(LogLevelError, "worktree_stall_integration_failed_transition command=%s error=%v",
				cmd.ID, mfErr)
		} else {
			qh.log(LogLevelWarn, "worktree_stall_integration_marked_failed command=%s", cmd.ID)
		}
		return
	}
	qh.log(LogLevelWarn, "worktree_stall_signal_emitted command=%s reason=%s stalled_since=%s",
		cmd.ID, reason, stalledSince)
}

// allPhasesAndTasksTerminal returns true iff every task that belongs to the
// given command is terminal AND every phase known to the state reader is
// terminal. Used by stall detection.
func (qh *QueueHandler) allPhasesAndTasksTerminal(commandID string, taskQueues map[string]*taskQueueEntry) bool {
	allTerm, _ := qh.checkCommandTasksTerminal(commandID, taskQueues)
	if !allTerm {
		return false
	}
	phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(commandID)
	if err != nil {
		// Non-phased commands surface ErrStateNotFound here; we still want
		// stall detection in that case, so a not-found error is treated as
		// "no phases" rather than a hard failure. Other errors fail closed.
		if !errors.Is(err, ErrStateNotFound) {
			return false
		}
		return true
	}
	for _, p := range phases {
		if !model.IsPhaseTerminal(p.Status) {
			return false
		}
	}
	return true
}

// stepResolvingWorkerStallDetection — Step 0.7.3b: Detect workers stuck in the
// `resolving` worktree status longer than the configured stall timeout.
//
// The deferred-merge path in worktree.tryMergeWorker leaves a worker at
// `resolving` until the dispatched __conflict_resolution task reports
// completion via result_write_handler → AutoRecoverAfterResolution. If that
// notification never arrives (e.g. an extended tmux signal_delivery_failed
// cascade prevents notify_worker from being delivered), the worker is silently
// pinned at `resolving` and the integration branch never advances. This step
// emits a `worker_resolving_stalled` planner signal once per (command, worker)
// pair so an operator can intervene with `plan resume_merge` or by clearing
// the worker manually. The signal index dedups subsequent emissions for the
// same key, matching the dedup model used by merge_conflict / commit_failed.
func (qh *QueueHandler) stepResolvingWorkerStallDetection(s *scanState) {
	if qh.worktreeManager == nil || !qh.config.Worktree.Enabled {
		return
	}
	timeoutMin := qh.config.Worktree.EffectiveStallTimeoutMinutes()
	if timeoutMin <= 0 {
		return
	}

	now := qh.clock.Now()
	threshold := now.Add(-time.Duration(timeoutMin) * time.Minute)

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil || cmdState == nil {
			continue
		}
		for _, ws := range cmdState.Workers {
			if ws.Status != model.WorktreeStatusResolving {
				continue
			}
			ref := ws.UpdatedAt
			if ref == "" {
				ref = ws.CreatedAt
			}
			refTime, err := qh.timeCache.ParseRFC3339(ref)
			if err != nil {
				continue
			}
			if !refTime.Before(threshold) {
				continue
			}

			stalledSince := refTime.UTC().Format(time.RFC3339)
			nowStr := now.UTC().Format(time.RFC3339)
			msg := fmt.Sprintf(
				"[maestro] kind:worker_resolving_stalled command_id:%s worker_id:%s\n"+
					"reason: resolving status held >= %d minutes (resolution task may not have reported)\n"+
					"stalled_since: %s\n"+
					"next_action: inspect worker pane / queue, then run `maestro plan resume-merge --command-id %s` once edits are present",
				cmd.ID, ws.WorkerID, timeoutMin, stalledSince, cmd.ID,
			)
			qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
				Kind:      "worker_resolving_stalled",
				CommandID: cmd.ID,
				WorkerID:  ws.WorkerID,
				Reason:    "resolving_status_held_too_long",
				Message:   msg,
				CreatedAt: nowStr,
				UpdatedAt: nowStr,
			}, s.signalIndex)
			qh.log(LogLevelWarn,
				"worker_resolving_stalled command=%s worker=%s stalled_since=%s threshold_min=%d",
				cmd.ID, ws.WorkerID, stalledSince, timeoutMin)
		}
	}
}

// stepCheckWorktreeConfigViolations — Step 0.7.4: When AutoCommit/AutoMerge are
// disabled, warn the operator (via WARN log + planner signal) for any in-progress
// command whose integration branch has stayed unmerged longer than the configured
// fallback timeout. The daemon does NOT force a merge; this step only surfaces
// the situation so the operator can act. Independent of stepWorktreeStallDetection.
func (qh *QueueHandler) stepCheckWorktreeConfigViolations(s *scanState) {
	if qh.worktreeManager == nil {
		return
	}
	// Both flags enabled → normal mode, nothing to warn about.
	if qh.worktreeManager.AutoCommit() && qh.worktreeManager.AutoMerge() {
		return
	}
	timeoutMin := qh.config.Worktree.EffectiveFallbackMergeTimeoutMinutes()
	if timeoutMin <= 0 {
		return
	}
	timeout := time.Duration(timeoutMin) * time.Minute

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil || cmdState == nil {
			continue
		}
		switch cmdState.Integration.Status {
		case model.IntegrationStatusMerged,
			model.IntegrationStatusPublishing,
			model.IntegrationStatusPublished:
			continue
		}
		createdAt, err := qh.timeCache.ParseRFC3339(cmdState.Integration.CreatedAt)
		if err != nil {
			continue
		}
		elapsed := qh.clock.Now().Sub(createdAt)
		if elapsed < timeout {
			continue
		}

		elapsedMin := int(elapsed.Minutes())
		autoCommit := qh.worktreeManager.AutoCommit()
		autoMerge := qh.worktreeManager.AutoMerge()
		qh.log(LogLevelWarn,
			"worktree_config_violation command=%s auto_commit=%v auto_merge=%v elapsed_min=%d timeout_min=%d",
			cmd.ID, autoCommit, autoMerge, elapsedMin, timeoutMin)

		now := qh.clock.Now().UTC().Format(time.RFC3339)
		msg := fmt.Sprintf(
			"[maestro] kind:worktree_config_violation command_id:%s\n"+
				"auto_commit=%v auto_merge=%v\n"+
				"integration branch unmerged for %d minutes (timeout: %d)\n"+
				"operator action required: review worktree config or merge manually",
			cmd.ID, autoCommit, autoMerge, elapsedMin, timeoutMin)
		qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
			Kind:      "worktree_config_violation",
			CommandID: cmd.ID,
			Message:   msg,
			CreatedAt: now,
			UpdatedAt: now,
		}, s.signalIndex)
	}
}
