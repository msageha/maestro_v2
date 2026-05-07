package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// stepWorktreePhaseMerges — Step 0.7.1: Collect merge work items for Phase B.
//
// Originally gated on cmd.Status == StatusInProgress, which silently dropped
// the implicit-phase merge whenever plan_complete won the race against the
// next periodic scan: a single-task command would terminate before
// collectImplicitWorktreeMerge ever ran, the worker worktree was left at
// WorktreeStatusActive with dirty output, the publish gate observed
// integration_status=created and a plan-terminal command, and cleanup
// destroyed the worktree directory along with the worker's changes (Report 3
// of 2026-05-03). Terminal commands are now allowed back into the collector
// when the integration branch has not yet reached a terminal state — i.e.
// there is still pending merge work to do — so the implicit merge can land
// before cleanup runs.
func (qh *QueueHandler) stepWorktreePhaseMerges(s *scanState) {
	if qh.worktreeManager == nil || !qh.dependencyResolver.HasStateReader() {
		return
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		// Skip cancel_requested commands to avoid wasted merge/commit work
		// during the cancellation window.
		if qh.cancelHandler != nil && qh.cancelHandler.IsCommandCancelRequested(cmd) {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		if cmd.Status != model.StatusInProgress {
			// Terminal command: only proceed when the integration branch
			// is in a state the merge collector can still act on. Mirrors
			// stepWorktreePublish's terminal-command policy and prevents
			// the cleanup phase from running before pending implicit-phase
			// merges have a chance to land worker output on integration.
			cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
			if err != nil || cmdState == nil {
				continue
			}
			switch cmdState.Integration.Status {
			case model.IntegrationStatusCreated,
				model.IntegrationStatusMerged,
				model.IntegrationStatusPartialMerge,
				model.IntegrationStatusConflict,
				model.IntegrationStatusFailed:
				// Fall through — pending worker output may still need to land.
			default:
				continue
			}
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
		isNoOpCreated := cmdState.Integration.Status == model.IntegrationStatusCreated
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
		// Bypass the staleness threshold for the no-op (created) case.
		// A terminal command whose integration branch never received any
		// commits is definitively a no-op outcome — there is no race
		// window where a late commit could land, and cleaning up as soon
		// as the command terminates aligns with the design that a
		// finished command must not leave queue/dashboard noise behind.
		//
		// All workers must be past WorktreeStatusActive before the no-op
		// fast-path fires. An active worker indicates uncommitted output
		// that the implicit-phase incremental merge has not yet picked
		// up. Tearing down its worktree here would lose real work.
		// Falling back to the elapsed≥threshold gate gives the merge
		// collector at least one scan cycle to run.
		if isNoOpCreated {
			hasActive := false
			for _, ws := range cmdState.Workers {
				if ws.Status == model.WorktreeStatusActive {
					hasActive = true
					break
				}
			}
			if hasActive && elapsed < threshold {
				qh.log(LogLevelDebug,
					"orphan_worktree_cleanup_deferred command=%s integration_status=created reason=worker_active "+
						"(deferring no_op_terminal cleanup; uncommitted worker output may still merge)",
					cmd.ID)
				continue
			}
		}
		if !isNoOpCreated && elapsed < threshold {
			continue
		}

		alreadyCleaning[cmd.ID] = struct{}{}
		reason := "orphan_terminal"
		if isNoOpCreated {
			reason = "no_op_terminal"
		}
		s.work.worktreeCleanups = append(s.work.worktreeCleanups, worktreeCleanupItem{
			CommandID: cmd.ID,
			Reason:    reason,
		})
		// no_op_terminal bypasses the staleness threshold (line 222-237) so
		// the orphan cleans up as soon as the command terminates without
		// integration commits — there is no late-merge race window. Log the
		// bypass explicitly: previously the line printed `threshold=10m elapsed=1s`
		// which read as a violated guard, when in fact the guard is intentionally
		// skipped for the no-op case. Reported 2026-05-04.
		thresholdField := threshold.String()
		if isNoOpCreated {
			thresholdField = "bypass(no_op)"
		}
		qh.log(LogLevelInfo,
			"orphan_worktree_cleanup_triggered command=%s cmd_status=%s integration_status=%s elapsed=%s threshold=%s reason=%s",
			cmd.ID, cmd.Status, cmdState.Integration.Status, elapsed.Round(time.Second), thresholdField, reason)
	}
}

// stepFinalizeQuarantinedDeferredComplete force-terminates leftover
// deferred_complete intents on a quarantined integration.
//
// Background (Report 2026-05-06 round-3 P0):
//   - Planner calls `plan complete` → publish not finished → returns
//     `deferred_publish` and writes `intents/deferred_complete_<cmd>.yaml`.
//   - Publish then fails repeatedly → R8 transitions integration to
//     `quarantined`.
//   - The existing deferredPlanCompleter only fires on
//     `worktree_published`, so the deferred intent stays around forever
//     on the quarantine path → orchestrator notification wedges.
//
// This step walks `intents/` every scan tick and finalises the
// deferred_complete entries of commands whose integration is quarantined
// via deferredPlanCompleter. `Complete()` (specifically
// checkWorktreePublished) now routes quarantine through
// worktreePublishTerminalError, so the deferredPlanCompleter takes the
// fail-terminal → result write → orchestrator notify path normally.
//
// Side-effect safety:
//   - deferredPlanCompleter removes the intent file internally, so this
//     never double-fires.
//   - On failure (storage error, etc.) the intent file remains and the
//     next tick retries.
//   - No LLM agent involvement; the daemon resolves the wedge on its own.
func (qh *QueueHandler) stepFinalizeQuarantinedDeferredComplete(s *scanState) {
	if qh.deferredPlanCompleter == nil || qh.worktreeManager == nil {
		return
	}
	intentsDir := filepath.Join(qh.maestroDir, "intents")
	entries, err := os.ReadDir(intentsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "deferred_complete_") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(strings.TrimPrefix(name, "deferred_complete_"), ".yaml")
		if commandID == "" {
			continue
		}
		cmdState, err := qh.worktreeManager.GetCommandState(commandID)
		if err != nil || cmdState == nil {
			continue
		}
		if cmdState.Integration.Status != model.IntegrationStatusQuarantined {
			continue
		}
		// deferredPlanCompleter calls plan.CompleteDeferredPublish, which
		// detects the quarantine in Complete() and writes plan_status=failed.
		// On success the intent file is removed so this never re-fires.
		completed, err := qh.deferredPlanCompleter(commandID)
		if err != nil {
			qh.log(LogLevelWarn,
				"deferred_complete_quarantine_finalize_failed command=%s error=%v",
				commandID, err)
			continue
		}
		if completed {
			qh.log(LogLevelInfo,
				"deferred_complete_quarantine_finalized command=%s integration_status=%s reason=%s",
				commandID, cmdState.Integration.Status, cmdState.Integration.QuarantineReason)
		}
	}
	_ = s // s itself is not mutated here; Complete() updates state.
}
