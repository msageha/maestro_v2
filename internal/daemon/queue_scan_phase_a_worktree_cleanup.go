package daemon

import (
	"time"

	"github.com/msageha/maestro_v2/internal/model"
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
		phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(cmd.ID)
		if err != nil {
			// Non-phased commands are handled by collectWorktreePublishAndCleanup;
			// other errors fail closed (do not force cleanup).
			continue
		}
		var stuck []PhaseInfo
		for _, p := range phases {
			if !model.IsPhaseTerminal(p.Status) &&
				p.Status != model.PhaseStatusAwaitingFill {
				// awaiting_fill is a legitimate waiting state: the planner has
				// been notified and is expected to submit tasks.  It has its own
				// fill-deadline timeout (checkAwaitingFillTimeout) and must not
				// be killed here.  Only truly stalled phases (pending, filling,
				// active with no running tasks) should be force-failed.
				stuck = append(stuck, p)
			}
		}
		if len(stuck) == 0 {
			continue
		}

		ref := cmd.UpdatedAt
		if ref == "" {
			ref = cmd.CreatedAt
		}
		refTime, err := qh.timeCache.ParseRFC3339(ref)
		if err != nil {
			continue
		}
		elapsed := now.Sub(refTime)
		if elapsed < threshold {
			continue
		}

		cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil {
			continue
		}
		if cmdState.Integration.Status == model.IntegrationStatusQuarantined {
			continue
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
