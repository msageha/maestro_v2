package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
)

// stepWorktreeFastTrackCleanup — Fast-track cleanup for stalled phases.
//
// When all of a command's tasks have reached terminal state but at least one
// phase remains non-terminal (pending / awaiting_fill / filling / active) and
// the command has not been updated for longer than
// `worktree.stall_cleanup_after`, force-fail the stuck phases and queue a
// worktree cleanup item that bypasses the merge path. This prevents
// .maestro/worktrees/cmd_*/ directories from leaking when daemon or worker
// sessions are interrupted mid-phase, while reusing the existing
// `worktreeCleanupItem` Phase B pathway (no new cleanup channel introduced).
//
// Skipped when:
//   - worktree isolation is disabled
//   - the configured duration is 0 (explicit disable)
//   - the command is not in_progress / has no worktrees
//   - any task is still non-terminal
//   - all phases are already terminal (handled by stepWorktreePublish)
//   - the integration is already quarantined (manual operator state)
//   - the command's UpdatedAt is within the threshold
func (qh *QueueHandler) stepWorktreeFastTrackCleanup(s *scanState) {
	if qh.worktreeManager == nil || !qh.dependencyResolver.HasStateReader() {
		return
	}
	if !qh.config.Worktree.Enabled {
		return
	}
	threshold := qh.config.Worktree.EffectiveStallCleanupAfter()
	if threshold <= 0 {
		return
	}
	now := qh.clock.Now()

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		allTerm, _ := qh.checkCommandTasksTerminal(cmd.ID, s.tasks)
		if !allTerm {
			continue
		}
		// State-side gate: the queue tasks may all be at a terminal queue
		// status (e.g. completed) while the state file still tracks an
		// in-flight resolution path — most commonly paused_for_replan
		// after max-retries verify failure, where the Planner is composing
		// an add-retry-task call. Without this gate, the 10-minute stall
		// cleanup races the Planner: cleanup deletes the worktree, the
		// Planner's add-retry-task lands moments later, and the
		// dispatcher fails to resolve the worktree path. Skip cleanup
		// whenever any task in state is non-terminal.
		//
		// Phantom-task escape: the state-side gate could trap a command
		// forever when a retry/repair task lands in state.TaskStates as
		// `planned`/`pending` but never makes it into any worker queue.
		// Once the cleanup window has elapsed, detect TaskStates entries
		// whose IDs do not appear in *any* worker queue and force-fail
		// them — they are by definition undispatchable. Legitimate
		// paused_for_replan tasks remain in their owning worker queue
		// (typically as `completed` or `verify_pending`), so the queue-
		// presence check keeps them out of the phantom set.
		qh.tryClearPhantomTasks(cmd, s.tasks, threshold, now)
		if hasPending, err := qh.dependencyResolver.GetStateReader().HasNonTerminalTaskState(cmd.ID); err == nil && hasPending {
			continue
		}
		phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(cmd.ID)
		if err != nil {
			// Non-phased commands are handled by collectWorktreePublishAndCleanup;
			// other errors fail closed (do not force cleanup).
			continue
		}
		// Build a phase-by-id index so the dependency check below can look up
		// each `pending` phase's upstream phases without rescanning the slice.
		phaseByID := make(map[string]PhaseInfo, len(phases))
		for _, p := range phases {
			phaseByID[p.ID] = p
		}

		var stuck []PhaseInfo
		for _, p := range phases {
			if model.IsPhaseTerminal(p.Status) {
				continue
			}
			// awaiting_fill is a legitimate waiting state: the planner has
			// been notified and is expected to submit tasks.  It has its own
			// fill-deadline timeout (checkAwaitingFillTimeout) and must not
			// be killed here.
			if p.Status == model.PhaseStatusAwaitingFill {
				continue
			}
			// Pending phases are dependency-bound: a deferred phase
			// declared via plan submit starts at `pending` and only
			// transitions to `awaiting_fill` when the dependency resolver
			// observes every upstream phase as terminal. Killing a
			// pending phase whose declared deps are still active false-
			// fails legitimate multi-phase workflows. Only flag a pending
			// phase as stuck when every declared dependency is itself
			// terminal — at that point the dep resolver should have
			// advanced the phase, and the failure to do so is genuine.
			if p.Status == model.PhaseStatusPending {
				allDepsTerminal := true
				for _, depID := range p.DependsOn {
					dep, ok := phaseByID[depID]
					if !ok || !model.IsPhaseTerminal(dep.Status) {
						allDepsTerminal = false
						break
					}
				}
				if !allDepsTerminal {
					continue
				}
			}
			// Active / filling phases (and pending phases whose deps are
			// all terminal) fall through to the stuck list.
			stuck = append(stuck, p)
		}
		if len(stuck) == 0 {
			continue
		}

		// elapsed reflects "time since the last observable progress event
		// for this command", not just "time since cmd.UpdatedAt". Phase A's
		// dispatch / result-write paths refresh task.UpdatedAt reliably,
		// but cmd.UpdatedAt is not touched by every internal transition
		// (e.g., result_write Phase A holds the per-worker queue lock and
		// leaves cmd.UpdatedAt as last set at command dispatch). Without
		// this max() the fast-track threshold would force-fail an active
		// phase whose tasks were progressing.
		//
		// Use max(cmd.UpdatedAt, latest task.UpdatedAt for this command)
		// so the threshold honours real progress. cmd.CreatedAt is the
		// final fallback when the command never had a usable UpdatedAt.
		latestActivity := computeLatestCommandActivity(cmd, s.tasks, qh.timeCache)
		if latestActivity.IsZero() {
			continue
		}
		elapsed := now.Sub(latestActivity)
		if elapsed < threshold {
			continue
		}

		cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil {
			continue
		}
		// Quarantined integrations are normally operator territory, but
		// the auto-recoverable class (dirty_worktree from RunOnIntegration
		// verify pollution) must not stall fast-track cleanup. The phase
		// escalation path in isPhaseMergeRecorded already attempts
		// recovery; if that hasn't unblocked the command after
		// `threshold * 3`, we proceed with cleanup here so the operator
		// is not left with a perpetually-leaked worktree.
		if cmdState.Integration.Status == model.IntegrationStatusQuarantined {
			escalateAfter := threshold * 3
			if elapsed < escalateAfter {
				qh.log(LogLevelDebug,
					"fast_track_cleanup_quarantine_grace command=%s elapsed=%s grace_remaining=%s reason=%q",
					cmd.ID, elapsed.Round(time.Second), (escalateAfter - elapsed).Round(time.Second),
					cmdState.Integration.QuarantineReason)
				continue
			}
			qh.log(LogLevelWarn,
				"fast_track_cleanup_quarantine_force command=%s elapsed=%s reason=%q "+
					"(quarantine persisted past grace window; proceeding with cleanup)",
				cmd.ID, elapsed.Round(time.Second), cmdState.Integration.QuarantineReason)
		}

		// Before nuking phases and the integration branch, ask the
		// lineage-aware DeriveStatus what the live state thinks. If the
		// plan would derive Completed AND the integration branch has
		// actually been merged (the publish gate is one step away from
		// advancing it), the "stuck" phase is just waiting for the
		// resolver to flip it — preempting it with MarkIntegrationFailed
		// would deadlock the publish path.
		//
		// Escalation: if this skip persists beyond `escalateAfter` we stop
		// deferring to the publish gate (something downstream is itself
		// stuck) and force the regular fast-track path so the operator-
		// visible cleanup runs and the worktree can be reclaimed. The
		// grace multiplier intentionally keeps a wide buffer so a slow
		// publish pipeline does not race the escalation.
		if cmdState.Integration.Status == model.IntegrationStatusMerged && qh.derivePlanCompleted(cmd.ID) {
			escalateAfter := threshold * 3
			if elapsed < escalateAfter {
				qh.log(LogLevelInfo,
					"fast_track_cleanup_skipped_plan_completed command=%s elapsed=%s grace_remaining=%s "+
						"(DeriveStatus reports plan complete and integration merged; deferring to publish gate)",
					cmd.ID, elapsed.Round(time.Second), (escalateAfter - elapsed).Round(time.Second))
				continue
			}
			qh.log(LogLevelWarn,
				"fast_track_cleanup_escalated_plan_completed command=%s elapsed=%s threshold=%s escalate_after=%s "+
					"(publish gate has not advanced past plan_completed within grace window; forcing cleanup)",
				cmd.ID, elapsed.Round(time.Second), threshold, escalateAfter)
		}

		applied := qh.stepForceFailStuckPhases(cmd.ID, stuck, elapsed)
		if applied == 0 {
			continue
		}

		// Best-effort: align integration status so post-cleanup state is
		// consistent. Errors are logged but do not block the cleanup item.
		if err := qh.worktreeManager.MarkIntegrationFailed(cmd.ID); err != nil {
			qh.log(LogLevelWarn,
				"fast_track_cleanup_mark_integration_failed command=%s error=%v",
				cmd.ID, err)
		}

		s.work.worktreeCleanups = append(s.work.worktreeCleanups, worktreeCleanupItem{
			CommandID: cmd.ID,
			Reason:    "fast_track_stall",
		})
		qh.log(LogLevelWarn,
			"fast_track_cleanup_triggered command=%s stuck_phases=%d elapsed=%s threshold=%s",
			cmd.ID, applied, elapsed.Round(time.Second), threshold)
	}
}

// derivePlanCompleted returns true when the live command state — read
// directly from disk and walked through plan.DeriveStatus — would yield
// PlanStatusCompleted. False when the state file is missing/unreadable, or
// when DeriveStatus returns any other status (failed/cancelled/error). The
// helper is intentionally conservative: if anything goes wrong we behave
// as if the plan is NOT completed, so the calling cleanup path runs.
func (qh *QueueHandler) derivePlanCompleted(commandID string) bool {
	cs, ok := qh.loadCommandStateForDerivation(commandID)
	if !ok || cs == nil {
		return false
	}
	derived, err := plan.DeriveStatus(cs)
	if err != nil {
		return false
	}
	return derived == model.PlanStatusCompleted
}

// tryClearPhantomTasks force-fails state.TaskStates entries that have no
// corresponding queue presence after the configured stall window. Returns
// the number of entries cleared (0 when the window has not elapsed, no
// non-terminal entries exist, every entry has a queue counterpart, or the
// state read fails).
//
// Phantom criteria (intentionally conservative):
//
//   - state has the task ID at a non-terminal status, AND
//   - no worker queue file lists the task ID for this command, AND
//   - the cleanup window (cmd.UpdatedAt + threshold) has elapsed.
//
// The queue-presence check uses the scan-snapshot s.tasks (the same set
// the caller already loaded), so the read is consistent with the rest of
// the cleanup decision and the phantom check costs nothing extra.
//
// Failure mode: when UpdateTaskState returns an error, the entry is left
// non-terminal and logged. The next scan retries; this trades one stuck
// scan for safety (we never silently swallow a state-write error).
func (qh *QueueHandler) tryClearPhantomTasks(cmd *model.Command, tasks map[string]*taskQueueEntry, threshold time.Duration, now time.Time) int {
	latestActivity := computeLatestCommandActivity(cmd, tasks, qh.timeCache)
	if latestActivity.IsZero() {
		return 0
	}
	if elapsed := now.Sub(latestActivity); elapsed < threshold {
		return 0
	}

	nonTerminal, err := qh.dependencyResolver.GetStateReader().GetNonTerminalTaskStates(cmd.ID)
	if err != nil || len(nonTerminal) == 0 {
		return 0
	}

	// Build the set of queue task IDs for this command from the scan
	// snapshot. We deliberately use s.tasks rather than re-reading queue
	// files: the snapshot is already consistent with the gate decisions
	// above, and a re-read could observe a freshly added task that
	// happens to be in a transient registration window.
	queueIDs := make(map[string]bool)
	for _, tq := range tasks {
		for _, t := range tq.Queue.Tasks {
			if t.CommandID != cmd.ID {
				continue
			}
			queueIDs[t.ID] = true
		}
	}

	cleared := 0
	for taskID, status := range nonTerminal {
		if queueIDs[taskID] {
			continue
		}
		// Live re-check veto: nonTerminal states were read LIVE above while
		// queueIDs came from the scan snapshot. A task the Planner
		// registered after the snapshot was taken (e.g. a retry/resolution
		// task added right as the stall threshold elapsed) exists in both
		// live state and live queue but not in the snapshot — cancelling
		// it would split state from queue and the worker's result_write
		// would be rejected as task-not-found. Destruction requires the
		// task to be absent from the LIVE queues as well.
		suspectKey := cmd.ID + "/" + taskID
		if qh.liveQueueHasTask(cmd.ID, taskID) {
			delete(qh.phantomSuspects, suspectKey)
			qh.log(LogLevelInfo,
				"phantom_task_skip_live_queue command=%s task=%s (present in live queue; registered after scan snapshot)",
				cmd.ID, taskID)
			continue
		}
		// Two-scan confirmation: daemon-side retry registration
		// (RetryTaskAtomically, R9) writes state BEFORE the queue, so a
		// single probe can land inside that window and misread a
		// registering task as phantom. Require queue absence on two
		// consecutive scans — the registration window is milliseconds,
		// a scan interval apart it cannot persist.
		if qh.phantomSuspects == nil {
			qh.phantomSuspects = make(map[string]int)
		}
		qh.phantomSuspects[suspectKey]++
		if qh.phantomSuspects[suspectKey] < 2 {
			qh.log(LogLevelInfo,
				"phantom_task_suspected command=%s task=%s (awaiting confirmation on next scan)",
				cmd.ID, taskID)
			continue
		}
		delete(qh.phantomSuspects, suspectKey)
		// State has a non-terminal entry the queue knows nothing about —
		// terminate it so phase progression can resume. The transition target
		// depends on the source status because validTaskStateTransitions
		// (model/status.go) restricts which transitions are legal:
		//
		//   - planned / ready / dispatched / verify_pending / repair_pending /
		//     paused_for_replan: only `cancelled` (or paused_for_human/aborted)
		//     is reachable as a terminal status. cancelled is also semantically
		//     correct here — the task never executed (or its outcome was lost),
		//     so attributing a "failed" verdict overstates what happened.
		//   - in_progress / running: failed is reachable AND semantically right —
		//     execution actually started but its result is unrecoverable.
		//
		// Using `cancelled` for non-execution states avoids an "invalid
		// task state transition: planned → failed" loop that would
		// otherwise leave the phantom task wedged. CancelledReason
		// carries the audit string so log searches can still correlate
		// against this code path.
		nextStatus := phantomTerminalStatus(status)
		reason := "phantom_task_no_queue_entry: state retained non-terminal entry past stall_cleanup_after"
		if err := qh.dependencyResolver.GetStateManager().UpdateTaskState(
			cmd.ID, taskID, nextStatus, reason,
		); err != nil {
			qh.log(LogLevelWarn,
				"phantom_task_clear_failed command=%s task=%s old_status=%s next=%s error=%v",
				cmd.ID, taskID, status, nextStatus, err)
			continue
		}
		qh.log(LogLevelWarn,
			"phantom_task_terminated command=%s task=%s old_status=%s new_status=%s reason=no_queue_entry",
			cmd.ID, taskID, status, nextStatus)
		cleared++
	}
	return cleared
}

// phantomTerminalStatus picks the terminal status the phantom-task cleanup
// should drive a non-terminal TaskStates entry to, given that entry's current
// status. It honours validTaskStateTransitions: only in_progress / running can
// transition directly to failed; every other non-terminal state must go to
// cancelled (or one of the paused/aborted variants we do not use here).
func phantomTerminalStatus(current model.Status) model.Status {
	switch current {
	case model.StatusInProgress, model.StatusRunning:
		return model.StatusFailed
	default:
		return model.StatusCancelled
	}
}

// computeLatestCommandActivity returns the most recent observable progress
// timestamp for the command. It is the maximum of cmd.UpdatedAt (falling
// back to cmd.CreatedAt when UpdatedAt is empty) and every queue task's
// UpdatedAt that belongs to this command.
//
// cmd.UpdatedAt is not refreshed by every internal transition, so a
// phase that runs through retry + verify could see fast-track cleanup
// fire against the threshold even though every task had reported
// progress within the last few seconds. Considering task UpdatedAt
// timestamps closes that gap without requiring every Phase A path to
// remember to update cmd.UpdatedAt.
//
// Returns the zero time when no parseable timestamp is available;
// callers must treat that as "skip the gate" because we cannot reason
// about elapsed time without a reference point.
func computeLatestCommandActivity(cmd *model.Command, tasks map[string]*taskQueueEntry, cache *timeParseCache) time.Time {
	var latest time.Time
	consider := func(ts string) {
		if ts == "" {
			return
		}
		t, err := cache.ParseRFC3339(ts)
		if err != nil {
			return
		}
		if t.After(latest) {
			latest = t
		}
	}
	consider(cmd.UpdatedAt)
	if latest.IsZero() {
		consider(cmd.CreatedAt)
	}
	for _, tq := range tasks {
		if tq == nil {
			continue
		}
		for i := range tq.Queue.Tasks {
			t := &tq.Queue.Tasks[i]
			if t.CommandID != cmd.ID {
				continue
			}
			consider(t.UpdatedAt)
		}
	}
	return latest
}

// stepForceFailStuckPhases transitions each stuck phase to failed, logging each
// transition. Returns the number of phases successfully transitioned.
func (qh *QueueHandler) stepForceFailStuckPhases(commandID string, stuck []PhaseInfo, elapsed time.Duration) int {
	applied := 0
	for _, p := range stuck {
		if err := qh.dependencyResolver.GetStateManager().ApplyPhaseTransition(
			commandID, p.ID, model.PhaseStatusFailed,
		); err != nil {
			qh.log(LogLevelWarn,
				"fast_track_cleanup_phase_apply_failed command=%s phase=%s old_status=%s error=%v",
				commandID, p.ID, p.Status, err)
			continue
		}
		qh.log(LogLevelWarn,
			"fast_track_cleanup_phase_failed command=%s phase=%s old_status=%s elapsed=%s",
			commandID, p.ID, p.Status, elapsed.Round(time.Second))
		applied++
	}
	return applied
}

// liveQueueHasTask re-reads the worker queue files from disk (under their
// queue locks) and reports whether taskID for commandID exists in any of
// them. Used as a destruction veto by tryClearPhantomTasks. Read or parse
// failures count as "might exist": when queue contents cannot be confirmed,
// the safe answer for a destructive caller is to skip.
func (qh *QueueHandler) liveQueueHasTask(commandID, taskID string) bool {
	queueDir := queueDirPath(qh.maestroDir)
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return !os.IsNotExist(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		workerID := strings.TrimSuffix(name, ".yaml")
		qh.lockMap.Lock("queue:" + workerID)
		data, readErr := os.ReadFile(filepath.Join(queueDir, name)) //nolint:gosec // controlled queue directory
		qh.lockMap.Unlock("queue:" + workerID)
		if readErr != nil {
			return true
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			return true
		}
		for i := range tq.Tasks {
			if tq.Tasks[i].CommandID == commandID && tq.Tasks[i].ID == taskID {
				return true
			}
		}
	}
	return false
}
