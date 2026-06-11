package daemon

import (
	"fmt"
	"sort"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/paneactivity"
	"github.com/msageha/maestro_v2/internal/model"
)

// phaseTransitionPriority returns an integer priority for a phase status.
// Lower values indicate higher priority. The ordering ensures that terminal
// failure states are applied before success or awaiting states when multiple
// transitions target the same phase.
func phaseTransitionPriority(status model.PhaseStatus) int {
	switch status {
	case model.PhaseStatusFailed:
		return 0
	case model.PhaseStatusCancelled:
		return 1
	case model.PhaseStatusTimedOut:
		return 2
	case model.PhaseStatusCompleted:
		return 3
	case model.PhaseStatusAwaitingFill:
		return 4
	default:
		return 5
	}
}

// sortPhaseTransitions sorts transitions by priority (lowest number first)
// using a stable sort to preserve original order among equal priorities.
func sortPhaseTransitions(transitions []PhaseTransitionResult) {
	sort.SliceStable(transitions, func(i, j int) bool {
		return phaseTransitionPriority(transitions[i].NewStatus) < phaseTransitionPriority(transitions[j].NewStatus)
	})
}

// deduplicatePhaseTransitions removes duplicate transitions targeting the same
// phase ID, keeping only the first occurrence (highest priority after sorting).
func deduplicatePhaseTransitions(transitions []PhaseTransitionResult) []PhaseTransitionResult {
	seen := make(map[string]bool, len(transitions))
	out := make([]PhaseTransitionResult, 0, len(transitions))
	for _, tr := range transitions {
		if seen[tr.PhaseID] {
			continue
		}
		seen[tr.PhaseID] = true
		out = append(out, tr)
	}
	return out
}

// stepPhaseTransitions — Step 0.7: Detect and persist phase transitions.
func (qh *QueueHandler) stepPhaseTransitions(s *scanState) {
	if !qh.dependencyResolver.HasStateReader() {
		return
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		transitions, err := qh.dependencyResolver.CheckPhaseTransitions(cmd.ID)
		if err != nil {
			qh.log(LogLevelWarn, "phase_transition_check command=%s error=%v", cmd.ID, err)
			continue
		}

		sortPhaseTransitions(transitions)
		transitions = deduplicatePhaseTransitions(transitions)

		for _, tr := range transitions {
			// Gate PhaseStatusCompleted on worktree merge completion (G1).
			// The DependencyResolver declares a phase Completed as soon as
			// every required task finishes, but for worktree-isolated
			// commands the integration merge (and potential conflict
			// detection) happens in Phase B/C of the same or a later scan.
			// Emitting phase_diagnosis before the merge recording means the
			// Planner sees "phase done" and can dispatch verification /
			// next-phase work while a merge_conflict signal is still in
			// flight — producing duplicate conflict resolution cycles.
			// We therefore defer Completed until MarkPhaseMerged has
			// recorded the phase. Failed/Cancelled/TimedOut transitions
			// are NOT gated: a failed task should surface immediately
			// regardless of merge state.
			if tr.NewStatus == model.PhaseStatusCompleted && !qh.isPhaseMergeRecorded(cmd.ID, tr.PhaseID) {
				qh.log(LogLevelDebug,
					"phase_transition_deferred command=%s phase=%s reason=awaiting_worktree_merge",
					cmd.ID, tr.PhaseName)
				continue
			}

			qh.log(LogLevelInfo, "phase_transition command=%s phase=%s %s→%s reason=%s",
				cmd.ID, tr.PhaseName, tr.OldStatus, tr.NewStatus, tr.Reason)

			if err := qh.dependencyResolver.GetStateManager().ApplyPhaseTransition(cmd.ID, tr.PhaseID, tr.NewStatus); err != nil {
				qh.log(LogLevelError, "phase_transition_apply command=%s phase=%s error=%v",
					cmd.ID, tr.PhaseID, err)
				continue
			}

			// Persist Phase.CancelledReason alongside the status flip when the
			// resolver requested it. Used by:
			//  - cascade cancellations (set the marker so recovery can find it)
			//  - cascade recoveries (clear the marker so a future operator
			//    cancel does not collide with the prior cascade reference).
			// Failures here are logged at warn but do NOT roll back the
			// status transition: the recovery is observability-grade,
			// and a stale reason on a re-failed phase is bounded by the
			// next cascade overwriting it.
			if tr.ClearCancelledReason {
				if err := qh.dependencyResolver.GetStateManager().SetPhaseCancelledReason(cmd.ID, tr.PhaseID, nil); err != nil {
					qh.log(LogLevelWarn,
						"phase_cancelled_reason_clear_failed command=%s phase=%s error=%v",
						cmd.ID, tr.PhaseID, err)
				}
			} else if tr.CancelledReason != "" {
				reason := tr.CancelledReason
				if err := qh.dependencyResolver.GetStateManager().SetPhaseCancelledReason(cmd.ID, tr.PhaseID, &reason); err != nil {
					qh.log(LogLevelWarn,
						"phase_cancelled_reason_set_failed command=%s phase=%s reason=%s error=%v",
						cmd.ID, tr.PhaseID, reason, err)
				}
			}

			now := qh.clock.Now().UTC().Format(time.RFC3339)
			switch tr.NewStatus {
			case model.PhaseStatusCompleted:
				// A-3: Self-diagnosis on phase completion
				diagPrompt := qh.diagnosePhaseTasks(cmd.ID, tr.PhaseID, tr.PhaseName, s.tasks)
				if diagPrompt != "" {
					msg := fmt.Sprintf("[maestro] kind:phase_diagnosis command_id:%s phase:%s\n%s",
						cmd.ID, tr.PhaseName, diagPrompt)
					qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
						Kind:      "phase_diagnosis",
						CommandID: cmd.ID,
						PhaseID:   tr.PhaseID,
						PhaseName: tr.PhaseName,
						Message:   msg,
						CreatedAt: now,
						UpdatedAt: now,
					}, s.signalIndex)
					qh.log(LogLevelInfo, "phase_diagnosis_emitted command=%s phase=%s",
						cmd.ID, tr.PhaseName)
				}
			case model.PhaseStatusAwaitingFill:
				phase := PhaseInfo{ID: tr.PhaseID, Name: tr.PhaseName}
				msg := qh.dependencyResolver.BuildAwaitingFillNotification(cmd.ID, phase)
				qh.log(LogLevelInfo, "awaiting_fill_signal command=%s phase=%s",
					cmd.ID, tr.PhaseName)
				qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
					Kind:      "awaiting_fill",
					CommandID: cmd.ID,
					PhaseID:   tr.PhaseID,
					PhaseName: tr.PhaseName,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, s.signalIndex)
			case model.PhaseStatusTimedOut:
				msg := fmt.Sprintf("[maestro] kind:fill_timeout command_id:%s phase:%s\nfill deadline expired",
					cmd.ID, tr.PhaseName)
				qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
					Kind:      "fill_timeout",
					CommandID: cmd.ID,
					PhaseID:   tr.PhaseID,
					PhaseName: tr.PhaseName,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, s.signalIndex)
			}
		}
	}
}

// phaseMergeDeferEscalation is the maximum time a Completed-transition may
// remain blocked on awaiting_worktree_merge before isPhaseMergeRecorded
// force-resolves it. The orchestrator's job is to keep work moving — if the
// merge has not been recorded after this long it is either a no-op merge
// whose recording was missed, or a transient infrastructure issue; either
// way, blocking the Planner from advancing is the worst outcome.
const phaseMergeDeferEscalation = 10 * time.Minute

// isPhaseMergeRecorded reports whether the phase's worktree merge has been
// recorded by Phase C (via MarkPhaseMerged). Used to gate PhaseStatusCompleted
// transitions so that phase_diagnosis is not emitted before a potential
// merge_conflict signal for the same phase.
//
// Returns true when:
//   - the command has no worktrees (no merge to wait for), OR
//   - MergedPhases already contains the phase, OR
//   - the worktree state file is unreadable (we conservatively unblock
//     instead of stalling on infrastructure flakes), OR
//   - the deferral has lasted longer than phaseMergeDeferEscalation AND
//     the integration is in a recoverable state — at that point we force
//     MarkPhaseMerged to unblock the Planner. For auto-recoverable
//     Quarantine reasons we also clear the quarantine so the next scan
//     can re-merge cleanly.
//
// Operator-required failure states (Conflict, PartialMerge, PublishFailed,
// or Quarantined with a non-recoverable reason) still defer here — those
// need explicit recovery via merge_conflict / publish_failed signals.
func (qh *QueueHandler) isPhaseMergeRecorded(commandID, phaseID string) bool {
	if qh.worktreeManager == nil {
		return true
	}
	if !qh.worktreeManager.HasWorktrees(commandID) {
		return true
	}
	cmdState, err := qh.worktreeManager.GetCommandState(commandID)
	if err != nil || cmdState == nil {
		qh.log(LogLevelDebug,
			"phase_transition_gate_state_unavailable command=%s phase=%s error=%v",
			commandID, phaseID, err)
		return true
	}
	deferKey := commandID + "::" + phaseID
	if cmdState.MergedPhases != nil {
		if _, merged := cmdState.MergedPhases[phaseID]; merged {
			qh.phaseMergeDeferStart.Delete(deferKey)
			return true
		}
	}

	// Track first observed deferral so we can escalate on stuck merges.
	now := qh.clock.Now()
	actual, _ := qh.phaseMergeDeferStart.LoadOrStore(deferKey, now)
	firstDefer, _ := actual.(time.Time)
	elapsed := now.Sub(firstDefer)
	if elapsed < phaseMergeDeferEscalation {
		return false
	}

	return qh.escalatePhaseMergeDefer(commandID, phaseID, deferKey, cmdState, elapsed, now)
}

// escalatePhaseMergeDefer runs once the deferral has exceeded
// phaseMergeDeferEscalation. It either force-marks the phase merged
// (recoverable integration states), kicks off quarantine recovery, or keeps
// blocking for operator-required failures. Returns the recorded-state result
// the caller should propagate.
func (qh *QueueHandler) escalatePhaseMergeDefer(
	commandID, phaseID, deferKey string,
	cmdState *model.WorktreeCommandState,
	elapsed time.Duration, now time.Time,
) bool {
	switch {
	case isStatusSafeForMarkMerged(cmdState.Integration.Status):
		qh.log(LogLevelWarn,
			"phase_transition_merge_defer_escalated command=%s phase=%s elapsed=%s integration_status=%s "+
				"(merge gate stuck past escalation threshold; force-marking phase merged to unblock Planner)",
			commandID, phaseID, elapsed.Round(time.Second), cmdState.Integration.Status)
		if markErr := qh.worktreeManager.MarkPhaseMerged(commandID, phaseID); markErr != nil {
			qh.log(LogLevelWarn,
				"phase_transition_merge_defer_escalated_mark_failed command=%s phase=%s error=%v",
				commandID, phaseID, markErr)
		}
		qh.phaseMergeDeferStart.Delete(deferKey)
		return true
	case cmdState.Integration.Status == model.IntegrationStatusQuarantined &&
		isAutoRecoverableQuarantineReason(cmdState):
		// Auto-recover dirty_worktree-class quarantines: clean the
		// integration worktree and drop status back to Failed so the
		// merge retry loop can run on the next scan. We do NOT
		// MarkPhaseMerged yet — the actual merge has not happened —
		// but we reset the defer clock so escalation does not fire
		// repeatedly while recovery is in flight.
		qh.log(LogLevelWarn,
			"phase_transition_quarantine_recovery_started command=%s phase=%s elapsed=%s reason=%q "+
				"(transient-pollution quarantine; cleaning worktree and retrying merge)",
			commandID, phaseID, elapsed.Round(time.Second), cmdState.Integration.QuarantineReason)
		if recErr := qh.worktreeManager.RecoverDirtyWorktreeQuarantine(commandID); recErr != nil {
			qh.log(LogLevelWarn,
				"phase_transition_quarantine_recovery_failed command=%s phase=%s error=%v",
				commandID, phaseID, recErr)
			// Recovery failed — do not reset the timer. Next scan will
			// hit this branch again and try once more, or the operator
			// can intervene.
			return false
		}
		// Reset the defer timer so the next pass starts fresh after the
		// recovery completes.
		qh.phaseMergeDeferStart.Store(deferKey, now)
		return false
	default:
		// Operator-required failure: keep deferring so the existing
		// merge_conflict / publish_failed signal handling can drive the
		// Planner-side recovery. Do not log every scan — would spam.
		qh.log(LogLevelDebug,
			"phase_transition_merge_defer_blocked command=%s phase=%s elapsed=%s integration_status=%s "+
				"(operator-required state; awaiting external resolution)",
			commandID, phaseID, elapsed.Round(time.Second), cmdState.Integration.Status)
		return false
	}
}

// isAutoRecoverableQuarantineReason mirrors the worktree-package
// isAutoRecoverableQuarantine check at the QueueHandler layer (which only
// holds the WorktreeStateManager interface, not the concrete Manager).
// Kept narrow: we re-list the safe prefixes here rather than expand the
// interface surface with a `IsAutoRecoverable(state)` method.
func isAutoRecoverableQuarantineReason(state *model.WorktreeCommandState) bool {
	if state == nil || state.Integration.Status != model.IntegrationStatusQuarantined {
		return false
	}
	if state.Integration.QuarantineSource != model.QuarantineSourceMerge {
		return false
	}
	reason := state.Integration.QuarantineReason
	prefixes := []string{
		"dirty_worktree",
		"status_check_failed",
		"pre_merge_reset_failed",
		"dirty_worktree_after_clean",
	}
	for _, p := range prefixes {
		if len(reason) >= len(p) && reason[:len(p)] == p {
			return true
		}
	}
	return false
}

// stepAwaitingFillWatchdog — Step 0.7.5: re-prompt a Planner that has
// stopped making progress on an awaiting_fill phase.
//
// Phase transitions to awaiting_fill emit a one-shot `awaiting_fill`
// planner signal in stepPhaseTransitions. If the Planner LLM stops
// making progress (e.g. dry-run succeeds but the real submit never
// follows), there is otherwise no further re-prompt until R6 fires the
// hard fill_deadline_at timeout — which defaults to 3 hours. The
// watchdog closes that gap by detecting a per-phase elapsed window since
// `awaiting_fill_since` and re-emitting a stall-prompt signal.
//
// Re-fire policy: the original implementation fired once per
// awaiting_fill window. That was insufficient for the "Planner thinks,
// stalls, never submits" failure mode — a single nudge often did not
// recover. The watchdog now re-fires at every `threshold` interval as
// long as the phase remains in awaiting_fill, both via the persisted
// `AwaitingFillStallNotifiedAt` (re-evaluated as "last fire time" rather
// than a one-shot gate) and an in-memory tracker that survives state
// write hiccups.
//
// Skipped when:
//   - the watchdog is disabled (notify_minutes = 0)
//   - StateReader is not wired
//   - the command is not in_progress / has been cancel-requested
//   - the phase is not at awaiting_fill
//   - the phase has no AwaitingFillSince (legacy state pre-watchdog)
//   - elapsed since AwaitingFillSince < threshold
//   - elapsed since last fire < threshold
func (qh *QueueHandler) stepAwaitingFillWatchdog(s *scanState) {
	if !qh.dependencyResolver.HasStateReader() {
		return
	}
	thresholdMin := qh.config.Maestro.EffectiveAwaitingFillStallNotifyMinutes()
	if thresholdMin <= 0 {
		return
	}
	threshold := time.Duration(thresholdMin) * time.Minute
	now := qh.clock.Now()
	nowStr := now.UTC().Format(time.RFC3339)

	stateReader := qh.dependencyResolver.GetStateReader()
	stateMgr := qh.dependencyResolver.GetStateManager()

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		// Cancellation in flight: the Planner pane may legitimately go
		// quiet while cancellation propagates; firing a stall prompt now
		// would race the cancel signal that is about to land. Defer.
		if qh.cancelHandler != nil && qh.cancelHandler.IsCommandCancelRequested(cmd) {
			continue
		}

		phases, err := stateReader.GetCommandPhases(cmd.ID)
		if err != nil {
			continue
		}
		for _, p := range phases {
			fireKey := cmd.ID + "::" + p.ID
			if p.Status != model.PhaseStatusAwaitingFill {
				// Phase exited awaiting_fill — clear the in-memory
				// re-fire tracker so a future re-entry starts fresh.
				qh.awaitingFillStallLastFire.Delete(fireKey)
				qh.awaitingFillStallFireCount.Delete(fireKey)
				continue
			}
			if p.AwaitingFillSince == nil {
				continue
			}
			since, err := qh.timeCache.ParseRFC3339(*p.AwaitingFillSince)
			if err != nil {
				qh.log(LogLevelDebug,
					"awaiting_fill_watchdog_parse_failed command=%s phase=%s value=%q error=%v",
					cmd.ID, p.Name, *p.AwaitingFillSince, err)
				continue
			}
			elapsed := now.Sub(since)
			if elapsed < threshold {
				continue
			}

			// Re-fire gate: only emit when at least `threshold` has
			// elapsed since the previous fire. The persisted state
			// (AwaitingFillStallNotifiedAt) and the in-memory tracker
			// are consulted in turn so we never lose the previous
			// timestamp on a state-write hiccup.
			lastFire := qh.lastAwaitingFillStallFire(fireKey, p.AwaitingFillStallNotifiedAt)
			isFirstFire := lastFire.IsZero()
			if !isFirstFire {
				if now.Sub(lastFire) < threshold {
					continue
				}
			}

			// Suppress fire when the Planner pane is observably alive
			// (rendering a spinner / activity verb). The canonical
			// false-positive shape is a Planner that ran
			// `plan submit --dry-run` to validate the snapshot, then
			// spent another ~30 s composing the real submit — the
			// watchdog used to flag this as a stall and re-fire WARNs
			// even though the LLM was demonstrably working (Report
			// 2026-05-06 P2). VerdictActive means the cross-scan
			// activity hint matched, so we know the Planner is
			// live; demote to DEBUG and skip the fire so operators
			// reading daemon.log don't see false alarms.
			//
			// Wall-clock backstop: a Planner pane that renders spinner
			// glyphs while the LLM has actually crashed / hung will sit
			// at VerdictActive forever, and bumpAwaitingFillStallFireCount
			// would never advance — escalation would never fire (Report
			// 2026-05-06 P-2 from review). Compare elapsed against the
			// budget the fire-count path uses (5 fires × threshold) and
			// force an escalation if the wall clock has exceeded it.
			// This keeps a wedged-but-spinning Planner from holding the
			// command in awaiting_fill indefinitely.
			if qh.paneActivity != nil {
				switch qh.observePaneVerdictForAgent("planner") {
				case paneactivity.VerdictActive:
					backstop := awaitingFillStallActiveBackstop
					if elapsed >= backstop {
						qh.log(LogLevelWarn,
							"awaiting_fill_watchdog_planner_active_backstop_escalate command=%s phase=%s elapsed=%s backstop=%s "+
								"(Planner pane VerdictActive but wall-clock budget exceeded — wedged-but-spinning detection; escalating)",
							cmd.ID, p.Name, elapsed.Round(time.Second), backstop)
						qh.escalateAwaitingFillStall(s, cmd, p, awaitingFillStallEscalateAfter, elapsed, threshold)
						continue
					}
					qh.log(LogLevelDebug,
						"awaiting_fill_watchdog_skipped_planner_active command=%s phase=%s elapsed=%s threshold=%s backstop_in=%s "+
							"(Planner pane is observably composing the next submit)",
						cmd.ID, p.Name, elapsed.Round(time.Second), threshold, (backstop - elapsed).Round(time.Second))
					continue
				}
			}

			msg := fmt.Sprintf(
				"[maestro] kind:awaiting_fill_stall command_id:%s phase:%s\n"+
					"%s elapsed since the original awaiting_fill signal without a plan_submit follow-up.\n"+
					"next_action: review the phase context (audit/test results, prior tasks) and call "+
					"`maestro plan submit --command-id %s --phase %s --tasks-file <path>` "+
					"with the concrete task list. If the phase is genuinely blocked, call "+
					"`maestro plan request-cancel --command-id %s --reason \"<why>\"` instead so "+
					"the Orchestrator is notified.",
				cmd.ID, p.Name, elapsed.Round(time.Second), cmd.ID, p.Name, cmd.ID,
			)
			qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
				Kind:      "awaiting_fill_stall",
				CommandID: cmd.ID,
				PhaseID:   p.ID,
				PhaseName: p.Name,
				Message:   msg,
				Reason:    fmt.Sprintf("awaiting_fill_elapsed=%s threshold=%s refire", elapsed.Round(time.Second), threshold),
				CreatedAt: nowStr,
				UpdatedAt: nowStr,
			}, s.signalIndex)

			// Persist the fire timestamp so subsequent scans observe it
			// even after a daemon restart. The in-memory tracker is the
			// authoritative copy within this process and survives state-
			// write failures.
			qh.awaitingFillStallLastFire.Store(fireKey, now)
			if stateMgr != nil {
				if err := stateMgr.MarkAwaitingFillStallNotified(cmd.ID, p.ID, nowStr); err != nil {
					qh.log(LogLevelDebug,
						"awaiting_fill_watchdog_mark_notified_failed command=%s phase=%s error=%v",
						cmd.ID, p.Name, err)
				}
			}
			// Severity policy: the first fire is informational — Planner
			// dry-run / API thinking can legitimately push beyond the
			// threshold without anything being broken (reported 2026-05-04
			// as alarm noise on a healthy run). Re-fires after another
			// `threshold` window without progress are escalated to WARN
			// because that genuinely indicates a stuck Planner.
			level := LogLevelWarn
			if isFirstFire {
				level = LogLevelInfo
			}

			// Increment per-(command, phase) fire count and escalate
			// when the planner has shown no progress across
			// awaitingFillStallEscalateAfter consecutive watchdog
			// re-fires. Without this, a wedged Planner pane (queued
			// messages stuck, input collision, runtime crash) keeps
			// receiving stall prompts forever and the iteration never
			// exits. Report 2026-05-05 P0 — Planner sat in
			// awaiting_fill for 15+ minutes, watchdog fired 3 times,
			// nothing converged.
			fireCount := qh.bumpAwaitingFillStallFireCount(fireKey)
			qh.log(level,
				"awaiting_fill_watchdog_fired command=%s phase=%s elapsed=%s threshold=%s first_fire=%t fires=%d",
				cmd.ID, p.Name, elapsed.Round(time.Second), threshold, isFirstFire, fireCount)
			if fireCount >= awaitingFillStallEscalateAfter {
				qh.escalateAwaitingFillStall(s, cmd, p, fireCount, elapsed, threshold)
			}
		}
	}
}

// awaitingFillStallEscalateAfter is the number of consecutive watchdog
// fires for the same (command, phase) without progress before the
// scanner escalates to command_failed. Three fires at default 5-minute
// threshold = ~15 minutes of unresponsive Planner before escalation.
//
// Tuned down from 5 (=25min) on 2026-05-06: the previous value
// occasionally lost the race against circuit_breaker.progress_timeout
// (default 30 min). The watchdog skips when the Planner pane shows
// activity (cross-scan hash delta), so a 5-minute cooldown after each
// fire combined with intermittent skips meant the 5th fire could land
// past the 30-minute progress timeout — the command was then
// auto-cancelled by the breaker rather than going through the
// watchdog's command_failed path with a structured stall reason.
// Three fires keeps a comfortable margin under any reasonable
// progress_timeout while still tolerating one or two legitimately slow
// LLM thinking cycles before declaring stall.
const awaitingFillStallEscalateAfter = 3

// awaitingFillStallActiveBackstop is the wall-clock threshold for forcing
// an escalate when the Planner pane keeps reporting VerdictActive (i.e.
// it appears to be composing, but the LLM itself may be wedged).
//
// This is a separate channel from the fire-count path
// (`awaitingFillStallEscalateAfter` * threshold ≒ 15 min). Fires are
// skipped while the verdict is Active, so without this backstop a
// wedged-but-spinning Planner never accumulates fires and never escalates
// (Report 2026-05-06 P-2 review).
//
// Picking the value:
//   - Too short: legitimate Opus 4.7 xhigh-effort thinking (~15-20 min
//     for a multi-phase plan) false-fires (Report 2026-05-06 round-2 P1).
//   - Too long: circuit_breaker.progress_timeout (default 30 min) eats
//     the case first and auto-cancels without a structured reason
//     (Report 2026-05-06 P0, original).
//
// 25 min permits ~20 min of legitimate thinking while still escalating
// 5 min before circuit_breaker, with a structured reason. The fire-count
// path (~15 min) stays focused on wedges that linger in VerdictIdle;
// active spinning wedges are caught by this backstop instead.
const awaitingFillStallActiveBackstop = 25 * time.Minute

// bumpAwaitingFillStallFireCount increments and returns the per-(cmd,
// phase) watchdog fire counter. Race-free via sync.Map atomic update.
func (qh *QueueHandler) bumpAwaitingFillStallFireCount(fireKey string) int {
	for {
		var prev int
		if v, ok := qh.awaitingFillStallFireCount.Load(fireKey); ok {
			if n, ok := v.(int); ok {
				prev = n
			}
		}
		next := prev + 1
		if qh.awaitingFillStallFireCount.CompareAndSwap(fireKey, prev, next) ||
			(prev == 0 && qh.swapAwaitingFillStallFireCount(fireKey, next)) {
			return next
		}
		// CAS lost — retry; concurrent watchdog fires for the same key
		// are extraordinarily rare (single scanner goroutine) but the
		// loop guards against future parallel scanners.
	}
}

// swapAwaitingFillStallFireCount handles the "no prior entry" case for
// CompareAndSwap by attempting LoadOrStore; returns true if THIS call
// stored the value. Necessary because sync.Map.CompareAndSwap requires
// an existing prior value.
func (qh *QueueHandler) swapAwaitingFillStallFireCount(fireKey string, val int) bool {
	_, loaded := qh.awaitingFillStallFireCount.LoadOrStore(fireKey, val)
	return !loaded
}

// escalateAwaitingFillStall transitions a wedged awaiting_fill phase to
// failed and emits a command_failed Orchestrator notification so the
// operator (and the Continuous-mode iteration counter) can route around
// the stuck command instead of holding the queue indefinitely.
//
// Best-effort: any individual failure (state write, notification append)
// is logged but not propagated, since this is itself a recovery path —
// subsequent scans will re-evaluate.
func (qh *QueueHandler) escalateAwaitingFillStall(s *scanState, cmd *model.Command, phase model.PhaseInfo, fireCount int, elapsed, threshold time.Duration) {
	now := qh.clock.Now()
	nowStr := now.UTC().Format(time.RFC3339)

	qh.log(LogLevelError,
		"awaiting_fill_watchdog_escalate_command_failed command=%s phase=%s fires=%d elapsed=%s threshold=%s "+
			"(planner unresponsive across %d consecutive watchdog re-fires; marking plan_status=failed and notifying Orchestrator)",
		cmd.ID, phase.Name, fireCount, elapsed.Round(time.Second), threshold, fireCount)

	// Transition the wedged phase to failed. R4PlanStatus picks this up
	// on the next reconciler pass and propagates plan_status=failed, so
	// phase_complete / publish gates short-circuit instead of waiting on
	// the wedged phase. Best-effort: a write failure is logged but the
	// notification still goes out so the operator can act.
	stateMgr := qh.dependencyResolver.GetStateManager()
	if stateMgr != nil {
		if err := stateMgr.ApplyPhaseTransition(cmd.ID, phase.ID, model.PhaseStatusFailed); err != nil {
			qh.log(LogLevelWarn,
				"awaiting_fill_watchdog_escalate_phase_transition_failed command=%s phase=%s error=%v "+
					"(continuing — Orchestrator will still see the synthetic notification)",
				cmd.ID, phase.Name, err)
		} else {
			reason := fmt.Sprintf("awaiting_fill_stall_escalated fires=%d elapsed=%s",
				fireCount, elapsed.Round(time.Second))
			if err := stateMgr.SetPhaseCancelledReason(cmd.ID, phase.ID, &reason); err != nil {
				qh.log(LogLevelDebug,
					"awaiting_fill_watchdog_escalate_phase_reason_failed command=%s phase=%s error=%v",
					cmd.ID, phase.Name, err)
			}
		}
	}

	// Append a command_failed notification to the orchestrator queue so
	// the Orchestrator pane is told the command is dead. Mirrors the
	// dead_letter notification format so existing Orchestrator handling
	// works unchanged.
	notifID, err := model.GenerateID(model.IDTypeNotification)
	if err != nil {
		qh.log(LogLevelError,
			"awaiting_fill_watchdog_escalate_notification_id_failed command=%s error=%v",
			cmd.ID, err)
		return
	}
	s.notifications.Data.Notifications = append(s.notifications.Data.Notifications, model.Notification{
		ID:        notifID,
		CommandID: cmd.ID,
		Type:      model.NotificationTypeCommandFailed,
		SourceResultID: fmt.Sprintf("res_awaiting_fill_stall_%s_%s",
			cmd.ID, phase.ID),
		Content: fmt.Sprintf("command %s failed: planner did not produce a plan submission for phase %q across %d watchdog fires (~%s elapsed); auto-escalated to command_failed",
			cmd.ID, phase.Name, fireCount, elapsed.Round(time.Second)),
		Priority:  100,
		Status:    model.StatusPending,
		CreatedAt: nowStr,
		UpdatedAt: nowStr,
	})
	s.notifications.Dirty = true

	// Reset the fire counter so a future re-entry starts fresh.
	qh.awaitingFillStallFireCount.Delete(cmd.ID + "::" + phase.ID)
}

// lastAwaitingFillStallFire returns the most recent fire timestamp for
// (command, phase). The in-memory tracker beats the persisted state
// because it is process-local and authoritative within this daemon
// instance; the persisted timestamp is the cross-restart fallback.
func (qh *QueueHandler) lastAwaitingFillStallFire(fireKey string, persisted *string) time.Time {
	if v, ok := qh.awaitingFillStallLastFire.Load(fireKey); ok {
		if t, ok := v.(time.Time); ok {
			return t
		}
	}
	if persisted == nil {
		return time.Time{}
	}
	t, err := qh.timeCache.ParseRFC3339(*persisted)
	if err != nil {
		return time.Time{}
	}
	return t
}

// stepPlannerSignals — Step 0.8: Evaluate backoff/staleness, defer delivery.
func (qh *QueueHandler) stepPlannerSignals(s *scanState) {
	if len(s.signals.Data.Signals) > 0 {
		qh.stepPlannerSignalsDeferred(&s.signals.Data, &s.signals.Dirty, &s.work, s.commands.Data)
	}
}

// stepPreemptiveRenewal — Renew command leases before checking expired leases.
func (qh *QueueHandler) stepPreemptiveRenewal(s *scanState) {
	qh.preemptiveCommandRenewal(&s.commands.Data, &s.commands.Dirty, s.tasks)
}

// stepDispatchOrRecovery — Steps 1 & 2: Dispatch or recovery (mutually exclusive).
func (qh *QueueHandler) stepDispatchOrRecovery(s *scanState) {
	expiredExists := qh.hasExpiredLeases(s.tasks, &s.commands.Data, &s.notifications.Data)

	if expiredExists {
		// Step 2: Collect busy check items for expired leases
		for queueFile, tq := range s.tasks {
			agentID := workerIDFromPath(queueFile)
			d := s.taskDirty[queueFile]
			items := qh.collectExpiredTaskBusyChecks(tq, agentID, queueFile, &d, s.paneVerdicts)
			s.taskDirty[queueFile] = d
			s.work.busyChecks = append(s.work.busyChecks, items...)
		}
		qh.autoExtendExpiredCommandLeases(&s.commands.Data, &s.commands.Dirty, s.tasks)
		qh.recoverExpiredNotificationLeases(&s.notifications.Data, &s.notifications.Dirty)
		qh.log(LogLevelDebug, "expired_leases_detected busy_checks=%d skipping_dispatch", len(s.work.busyChecks))
	} else {
		// Step 1: Collect dispatch items
		qh.collectPendingCommandDispatches(&s.commands.Data, &s.commands.Dirty, &s.work)

		globalInFlight := qh.buildGlobalInFlightSet(s.tasks)
		inFlightPaths := collectInFlightPaths(s.tasks, qh.leaseManager.IsLeaseExpired)
		for queueFile, tq := range s.tasks {
			workerID := workerIDFromPath(queueFile)
			if workerID == "" {
				qh.log(LogLevelWarn, "skip_dispatch cannot derive worker from %s", queueFile)
				continue
			}
			dirty := qh.collectPendingTaskDispatches(tq, workerID, globalInFlight, inFlightPaths, s.tasks, &s.work)
			if dirty {
				s.taskDirty[queueFile] = true
			}
		}
		qh.collectPendingNotificationDispatches(&s.notifications.Data, &s.notifications.Dirty, &s.work)
	}
}

// stepDependencyFailures — Step 1.5: Check pending/in-progress tasks for dependency failures.
func (qh *QueueHandler) stepDependencyFailures(s *scanState) {
	for queueFile, tq := range s.tasks {
		dirty := qh.checkPendingDependencyFailuresDeferred(tq, workerIDFromPath(queueFile))
		dirty2, interrupts2 := qh.checkInProgressDependencyFailuresDeferred(tq, workerIDFromPath(queueFile))
		if dirty || dirty2 {
			s.taskDirty[queueFile] = true
		}
		s.work.interrupts = append(s.work.interrupts, interrupts2...)
	}
}

// diagnosePhaseTasks collects tasks belonging to a completed phase and produces
// a diagnosis prompt using the injected phaseDiagnoser. Returns the formatted
// prompt string, or "" if diagnosis yields no actionable information or if
// no diagnoser is configured.
func (qh *QueueHandler) diagnosePhaseTasks(commandID, phaseID, _ string, taskQueues map[string]*taskQueueEntry) string {
	if qh.scanExecutor.phaseDiagnoser == nil || !qh.dependencyResolver.HasStateReader() {
		return ""
	}

	phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(commandID)
	if err != nil {
		qh.log(LogLevelWarn, "phase_diagnosis_skip command=%s phase=%s reason=get_phases_error: %v",
			commandID, phaseID, err)
		return ""
	}

	// Find the phase and its task IDs
	var phaseTaskIDs []string
	var phase model.Phase
	for _, p := range phases {
		if p.ID == phaseID {
			phaseTaskIDs = p.RequiredTaskIDs
			phase = model.Phase{
				PhaseID: p.ID,
				Name:    p.Name,
				Status:  p.Status,
			}
			break
		}
	}

	if len(phaseTaskIDs) == 0 {
		return ""
	}

	// Build a set of task IDs for fast lookup
	taskIDSet := make(map[string]bool, len(phaseTaskIDs))
	for _, id := range phaseTaskIDs {
		taskIDSet[id] = true
	}

	// Collect matching tasks from all task queues
	var tasks []model.Task
	for _, tq := range taskQueues {
		for i := range tq.Queue.Tasks {
			if taskIDSet[tq.Queue.Tasks[i].ID] {
				tasks = append(tasks, tq.Queue.Tasks[i])
			}
		}
	}

	return qh.scanExecutor.phaseDiagnoser(phase, tasks, nil)
}
