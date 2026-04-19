package daemon

import (
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// stepWorktreePhaseMerges — Step 0.7.1: Collect merge work items for Phase B.
func (qh *QueueHandler) stepWorktreePhaseMerges(s *scanState) {
	if qh.worktreeManager == nil || !qh.dependencyResolver.HasStateReader() {
		return
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		// Skip cancel_requested commands to avoid wasted merge/commit work
		// during the cancellation window.
		if qh.cancelHandler != nil && qh.cancelHandler.IsCommandCancelRequested(cmd) {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		mergeItems := qh.collectWorktreePhaseMerges(cmd.ID, s.tasks)
		s.work.worktreeMerges = append(s.work.worktreeMerges, mergeItems...)
	}
}

// stepWorktreePublish — Step 0.7.2: Detect command completion for worktree publishing.
//
// Originally gated on cmd.Status == in_progress, which leaked
// .maestro/worktrees/<cmd>/ directories whenever a command terminated (failed,
// cancelled, or completed via a path that did not transit publish) before the
// integration branch was merged or cleaned. The guard is now relaxed: terminal
// commands are also considered as long as their integration branch is still in
// a state collectWorktreePublishAndCleanup can act on (created → publish/no-op,
// merged → publish, published → cleanup-on-success). Other states (publishing,
// quarantined, …) are skipped to avoid stomping on in-flight Phase B work or
// operator-managed quarantines. Pending commands are still skipped — they have
// no worktrees yet.
func (qh *QueueHandler) stepWorktreePublish(s *scanState) {
	if qh.worktreeManager == nil || !qh.dependencyResolver.HasStateReader() {
		return
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status == model.StatusPending {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		if cmd.Status != model.StatusInProgress {
			// Terminal command — only proceed if the integration branch is in
			// a state the publish/cleanup pathway can still act on.
			cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
			if err != nil || cmdState == nil {
				continue
			}
			switch cmdState.Integration.Status {
			case model.IntegrationStatusCreated,
				model.IntegrationStatusMerged,
				model.IntegrationStatusPublished:
				// fallthrough to collect
			default:
				continue
			}
		}
		publishes, cleanups := qh.collectWorktreePublishAndCleanup(cmd.ID, cmd.Content, s.tasks)
		s.work.worktreePublishes = append(s.work.worktreePublishes, publishes...)
		s.work.worktreeCleanups = append(s.work.worktreeCleanups, cleanups...)
	}
}

// stepWorktreeOrphanCleanup — Cleanup for non-phased terminal commands whose
// worktrees would otherwise leak.
//
// stepWorktreePublish reuses collectWorktreePublishAndCleanup, which only
// emits a cleanup item when integration.status is published (cleanup-on-success)
// or when there is a failed task with cleanup-on-failure enabled. That leaves
// a gap: a non-phased command can terminate (failed/cancelled) with the
// integration branch still in {created, failed, conflict, partial_merge} —
// nothing in publish/fast-track collects a cleanup, and the worktree directory
// + state yaml linger until the 168h GC reaper.
//
// This step targets exactly that gap. It is intentionally narrow:
//
//   - cmd terminal (any IsTerminal status)
//   - HasWorktrees
//   - integration ∈ {created, failed, conflict, partial_merge}
//     (merged/published handled by stepWorktreePublish, publishing is
//     in-flight Phase B, quarantined requires manual operator action)
//   - elapsed since cmd.UpdatedAt ≥ worktree.stall_cleanup_after
//
// Idempotency vs stepWorktreePublish: the integration-status sets are
// disjoint, so the same command never produces a cleanup item from both
// steps in the same scan. Across scans, once Phase B runs the cleanup the
// worktree state file disappears and HasWorktrees returns false on the next
// scan, so this step naturally stops firing.
//
// Skipped when:
//   - worktree isolation is disabled
//   - the configured duration is 0
//   - the command is not terminal
//   - the command has no worktrees
//   - integration status is not in the orphan set above
//   - the command's UpdatedAt is within the threshold
func (qh *QueueHandler) stepWorktreeOrphanCleanup(s *scanState) {
	if qh.worktreeManager == nil {
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

	// Build a set of commands that earlier steps in this scan already
	// enqueued a cleanup for, so we never emit two cleanup items for the
	// same command in one scan (Phase B does not dedupe).
	alreadyCleaning := make(map[string]struct{}, len(s.work.worktreeCleanups))
	for _, c := range s.work.worktreeCleanups {
		alreadyCleaning[c.CommandID] = struct{}{}
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if !model.IsTerminal(cmd.Status) {
			continue
		}
		if _, dup := alreadyCleaning[cmd.ID]; dup {
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
		case model.IntegrationStatusCreated,
			model.IntegrationStatusFailed,
			model.IntegrationStatusConflict,
			model.IntegrationStatusPartialMerge:
			// orphan candidate
		default:
			continue
		}

		// Use the latest of cmd.UpdatedAt and integration.UpdatedAt as the
		// staleness reference. Integration states like failed/conflict/
		// partial_merge can be written long after the command itself
		// terminated (e.g. a fresh publish conflict on a long-completed
		// command), so keying off cmd.UpdatedAt alone would clean up
		// immediately with no grace period. Taking the max gives the
		// orphan at least `threshold` of quiet time before cleanup.
		ref := cmd.UpdatedAt
		if ref == "" {
			ref = cmd.CreatedAt
		}
		refTime, err := qh.timeCache.ParseRFC3339(ref)
		if err != nil {
			continue
		}
		if intTime, ierr := qh.timeCache.ParseRFC3339(cmdState.Integration.UpdatedAt); ierr == nil && intTime.After(refTime) {
			refTime = intTime
		}
		elapsed := now.Sub(refTime)
		if elapsed < threshold {
			continue
		}

		alreadyCleaning[cmd.ID] = struct{}{}
		s.work.worktreeCleanups = append(s.work.worktreeCleanups, worktreeCleanupItem{
			CommandID: cmd.ID,
			Reason:    "orphan_terminal",
		})
		qh.log(LogLevelWarn,
			"orphan_worktree_cleanup_triggered command=%s cmd_status=%s integration_status=%s elapsed=%s threshold=%s",
			cmd.ID, cmd.Status, cmdState.Integration.Status, elapsed.Round(time.Second), threshold)
	}
}
