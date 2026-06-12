package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/model"
)

// A/B candidate group orchestration (docs/design/ab_candidate_selection.md
// §5-§7). Phase A collects group work items from the queue snapshot; Phase B
// drives the group state machine:
//
//	racing ──(all candidates terminal)──▶ selecting ──▶ resolved
//	   │                                      │
//	   ├─(timeout, one completed)─ walkover ──┤
//	   └─(both failed / unrecoverable)──────▶ degraded
//
// Every failure path degrades to "canonical continues alone" — A/B never
// blocks command progress (design §1). This scan-driven step doubles as the
// design's R-AB reconciler: timeouts, stuck `selecting` groups and crash
// leftovers are re-examined on every scan.

// defaultABRaceTimeoutSec bounds the wait for the slower candidate when
// neither ab_test.timeout_sec nor the task's max_wall_clock_sec is set.
const defaultABRaceTimeoutSec = 1800

// abWorktreeOps is the optional capability surface the A/B step needs from
// the worktree manager. Asserted at runtime so existing QueueWorktreeManager
// stubs in tests remain valid (same pattern as PlanExecutorModelSelectorSettable).
type abWorktreeOps interface {
	CommitCandidateChanges(commandID, taskID string) error
	RunCandidateSelection(ctx context.Context, commandID, groupID string, candidates []worktree.ABSelectionInput, verifyCmds []string) (*worktree.ABSelectionOutcome, error)
	IntakeWinner(commandID, workerID, candidateBranch, taskID string) error
	RemoveCandidateWorktree(commandID, taskID string) error
}

// abGroupWorkItem is collected in Phase A from the queue snapshot.
type abGroupWorkItem struct {
	CommandID string
	GroupID   string
	// QueueStatuses snapshots the candidates' queue row statuses.
	QueueStatuses map[string]model.Status
	// CanonicalWallClockSec carries the canonical row's abort budget for the
	// race-timeout fallback (0 when unset).
	CanonicalWallClockSec int
}

// collectABGroupWork walks the queue snapshot and emits one work item per
// distinct candidate group that has at least one row in the queues.
func (qh *QueueHandler) collectABGroupWork(taskQueues map[string]*taskQueueEntry, work *deferredWork) {
	type agg struct {
		commandID string
		statuses  map[string]model.Status
		wallClock int
	}
	groups := map[string]*agg{}
	for _, tq := range taskQueues {
		if tq == nil {
			continue
		}
		for i := range tq.Queue.Tasks {
			t := &tq.Queue.Tasks[i]
			if t.ABGroupID == "" {
				continue
			}
			a := groups[t.ABGroupID]
			if a == nil {
				a = &agg{commandID: t.CommandID, statuses: map[string]model.Status{}}
				groups[t.ABGroupID] = a
			}
			a.statuses[t.ID] = t.Status
			if t.DefinitionOfAbort != nil && t.DefinitionOfAbort.MaxWallClockSec > a.wallClock {
				a.wallClock = t.DefinitionOfAbort.MaxWallClockSec
			}
		}
	}
	for groupID, a := range groups {
		work.abGroups = append(work.abGroups, abGroupWorkItem{
			CommandID:             a.commandID,
			GroupID:               groupID,
			QueueStatuses:         a.statuses,
			CanonicalWallClockSec: a.wallClock,
		})
	}
}

// stepABSelection drives every collected candidate group one transition
// forward. Runs in Phase B (no scan lock held; git + state file I/O).
func (qh *QueueHandler) stepABSelection(ctx context.Context, pa *phaseAResult) {
	if len(pa.work.abGroups) == 0 {
		return
	}
	wm, ok := qh.worktreeManager.(abWorktreeOps)
	if !ok || qh.worktreeManager == nil {
		return // worktree manager absent or stubbed without A/B capability
	}
	for _, item := range pa.work.abGroups {
		if ctx.Err() != nil {
			return
		}
		qh.processABGroup(ctx, wm, item)
	}
}

func (qh *QueueHandler) processABGroup(ctx context.Context, wm abWorktreeOps, item abGroupWorkItem) {
	state, err := qh.readCommandState(item.CommandID)
	if err != nil {
		qh.log(LogLevelDebug, "ab_group_state_unreadable command=%s group=%s error=%v",
			item.CommandID, item.GroupID, err)
		return
	}
	g := state.CandidateGroups[item.GroupID]
	if g == nil || !g.Status.IsUnresolved() {
		return // resolved/degraded already, or unknown group
	}

	canonical := g.CandidateByTask(g.CanonicalTaskID)
	shadow := g.OtherCandidate(g.CanonicalTaskID)
	if canonical == nil || shadow == nil {
		qh.resolveABGroup(item.CommandID, item.GroupID, g.CanonicalTaskID, true,
			"malformed candidate group", map[string]string{"degraded": "malformed_group"}, wm)
		return
	}

	completed := func(taskID string) bool { return item.QueueStatuses[taskID] == model.StatusCompleted }
	terminal := func(taskID string) bool { return model.IsTerminal(item.QueueStatuses[taskID]) }

	switch {
	case terminal(canonical.TaskID) && terminal(shadow.TaskID):
		// Race finished — run (or re-run after a crash) the selection.
		qh.runABSelectionAndResolve(ctx, wm, item, state, g, completed)

	case qh.abRaceTimedOut(g, item):
		// Race budget exhausted with a non-terminal candidate left. A
		// RUNNING loser is never finalized destructively here — its worker
		// is executing inside the candidate worktree, and its own
		// DefinitionOfAbort / lease machinery bounds it; the all-terminal
		// branch above finalizes afterwards. Only a still-PENDING loser
		// (never dispatched) is safe to cancel so the race cannot hang on
		// a row no worker ever picks up.
		for _, c := range []*model.ABCandidate{canonical, shadow} {
			other := g.OtherCandidate(c.TaskID)
			if item.QueueStatuses[c.TaskID] == model.StatusPending && other != nil && completed(other.TaskID) {
				qh.cancelPendingABCandidate(item.CommandID, c)
			}
		}
		qh.log(LogLevelDebug, "ab_race_timeout_waiting command=%s group=%s statuses=%v",
			item.CommandID, item.GroupID, item.QueueStatuses)
	}
	// else: still racing within budget — wait for the next scan.
}

// cancelPendingABCandidate CAS-cancels a never-dispatched candidate row
// (queue: pending → cancelled) and mirrors the state entry, so a timed-out
// race with a completed opponent converges to the all-terminal walkover on
// the next scan. Running candidates are never touched.
func (qh *QueueHandler) cancelPendingABCandidate(commandID string, c *model.ABCandidate) {
	queueKey := "queue:" + c.WorkerID
	qh.lockMap.Lock(queueKey)
	cancelled := false
	if err := updateYAMLFile(taskQueuePath(qh.maestroDir, c.WorkerID), func(tq *model.TaskQueue) error {
		for i := range tq.Tasks {
			if tq.Tasks[i].ID == c.TaskID && tq.Tasks[i].Status == model.StatusPending {
				tq.Tasks[i].Status = model.StatusCancelled
				tq.Tasks[i].UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
				cancelled = true
				return nil
			}
		}
		return errNoUpdate
	}); err != nil && !errors.Is(err, errNoUpdate) {
		qh.lockMap.Unlock(queueKey)
		qh.log(LogLevelWarn, "ab_timeout_cancel_queue_failed command=%s task=%s error=%v", commandID, c.TaskID, err)
		return
	}
	qh.lockMap.Unlock(queueKey)
	if !cancelled {
		return // dispatched in the meantime — leave it to its own lifecycle
	}
	stateKey := "state:" + commandID
	qh.lockMap.Lock(stateKey)
	defer qh.lockMap.Unlock(stateKey)
	if err := updateYAMLFile(commandStatePath(qh.maestroDir, commandID), func(state *model.CommandState) error {
		if state.CancelledReasons == nil {
			state.CancelledReasons = map[string]string{}
		}
		state.TaskStates[c.TaskID] = model.StatusCancelled
		state.CancelledReasons[c.TaskID] = "superseded_by_ab_timeout"
		state.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
		return nil
	}); err != nil {
		qh.log(LogLevelWarn, "ab_timeout_cancel_state_failed command=%s task=%s error=%v", commandID, c.TaskID, err)
		return
	}
	qh.log(LogLevelInfo, "ab_timeout_cancelled_pending command=%s task=%s worker=%s", commandID, c.TaskID, c.WorkerID)
}

// runABSelectionAndResolve handles the all-terminal case: commit candidate
// work, run the selection engine, and finalize.
func (qh *QueueHandler) runABSelectionAndResolve(ctx context.Context, wm abWorktreeOps, item abGroupWorkItem, state *model.CommandState, g *model.CandidateGroup, completed func(string) bool) {
	canonical := g.CandidateByTask(g.CanonicalTaskID)
	shadow := g.OtherCandidate(g.CanonicalTaskID)

	var finished []*model.ABCandidate
	for _, c := range []*model.ABCandidate{canonical, shadow} {
		if completed(c.TaskID) {
			finished = append(finished, c)
		}
	}
	if len(finished) == 0 {
		// Both candidates failed / dead-lettered: the canonical continues on
		// the normal repair path; the shadow is terminally superseded.
		qh.resolveABGroup(item.CommandID, item.GroupID, "", true,
			"both candidates failed; canonical continues on the normal repair path",
			map[string]string{"degraded": "both_failed"}, wm)
		return
	}

	// Commit completed candidates' worktrees (idempotent).
	for _, c := range finished {
		if err := wm.CommitCandidateChanges(item.CommandID, c.TaskID); err != nil {
			qh.log(LogLevelWarn, "ab_candidate_commit_failed command=%s task=%s error=%v (retry next scan)",
				item.CommandID, c.TaskID, err)
			return
		}
	}

	if len(finished) == 1 {
		winner := finished[0]
		loser := canonical
		if winner == canonical {
			loser = shadow
		}
		qh.finalizeABWinner(ctx, wm, item, g, winner, loser,
			map[string]string{"walkover": "single_completed"})
		return
	}

	// Mark selecting (CAS: racing → selecting) so the selection-timeout
	// clock starts and observers see the phase change.
	if g.Status == model.ABGroupRacing {
		if err := qh.markABGroupSelecting(item.CommandID, item.GroupID); err != nil {
			qh.log(LogLevelWarn, "ab_group_mark_selecting_failed command=%s group=%s error=%v",
				item.CommandID, item.GroupID, err)
			return
		}
	} else if qh.abSelectionTimedOut(g) {
		qh.log(LogLevelWarn, "ab_selection_timeout command=%s group=%s (canonical walkover)",
			item.CommandID, item.GroupID)
		qh.finalizeABWinner(ctx, wm, item, g, canonical, shadow,
			map[string]string{"walkover": "selection_timeout"})
		return
	}

	verifyCmds := qh.loadABVerifyCommands(item.CommandID)
	inputs := []worktree.ABSelectionInput{ // canonical FIRST: ties go to it
		{TaskID: canonical.TaskID, Branch: canonical.Branch},
		{TaskID: shadow.TaskID, Branch: shadow.Branch},
	}
	outcome, err := wm.RunCandidateSelection(ctx, item.CommandID, item.GroupID, inputs, verifyCmds)
	if err != nil {
		if errors.Is(err, worktree.ErrSelectionBusy) {
			qh.log(LogLevelDebug, "ab_selection_deferred command=%s group=%s (integration busy)",
				item.CommandID, item.GroupID)
			return // selecting status persists; selection-timeout bounds the wait
		}
		qh.log(LogLevelWarn, "ab_selection_failed command=%s group=%s error=%v (canonical walkover)",
			item.CommandID, item.GroupID, err)
		qh.finalizeABWinner(ctx, wm, item, g, canonical, shadow,
			map[string]string{"walkover": "selection_error", "selection_error": err.Error()})
		return
	}

	evidence := outcome.Evidence
	if evidence == nil {
		evidence = map[string]string{}
	}
	winner, loser := canonical, shadow
	if outcome.Degraded {
		// Mechanical signal unavailable (verifier broken / no verifier /
		// all candidates conflicted). Canonical wins by deterministic
		// default; signal the Planner that the verifier needs attention.
		evidence["degraded_selection"] = outcome.Reason
		qh.log(LogLevelWarn, "ab_selection_no_signal command=%s group=%s reason=%q (canonical default win)",
			item.CommandID, item.GroupID, outcome.Reason)
	} else if outcome.WinnerTaskID == shadow.TaskID {
		winner, loser = shadow, canonical
	}
	qh.finalizeABWinner(ctx, wm, item, g, winner, loser, evidence)
}

// finalizeABWinner performs intake (winner candidate branch → its worker
// branch), loser cleanup, and the state resolution. An intake conflict
// degrades the group with canonical re-execution (design §6, PR1).
func (qh *QueueHandler) finalizeABWinner(_ context.Context, wm abWorktreeOps, item abGroupWorkItem, g *model.CandidateGroup, winner, loser *model.ABCandidate, evidence map[string]string) {
	// Winner work must be committed even on walkover paths (idempotent).
	if err := wm.CommitCandidateChanges(item.CommandID, winner.TaskID); err != nil {
		qh.log(LogLevelWarn, "ab_winner_commit_failed command=%s task=%s error=%v (retry next scan)",
			item.CommandID, winner.TaskID, err)
		return
	}
	if err := wm.IntakeWinner(item.CommandID, winner.WorkerID, winner.Branch, winner.TaskID); err != nil {
		qh.log(LogLevelError,
			"ab_intake_conflict command=%s group=%s winner=%s error=%v (degrading: re-execution via repair task)",
			item.CommandID, item.GroupID, winner.TaskID, err)
		evidence["degraded"] = "intake_conflict"
		evidence["intake_error"] = err.Error()
		qh.degradeABGroupWithRepair(item, g, evidence, err)
		return
	}

	// Loser + winner candidate worktrees/branches are no longer needed
	// (winner content now lives on the worker branch). Removal failures are
	// retried by GC/cleanup.
	if err := wm.RemoveCandidateWorktree(item.CommandID, loser.TaskID); err != nil {
		qh.log(LogLevelWarn, "ab_loser_cleanup_failed command=%s task=%s error=%v", item.CommandID, loser.TaskID, err)
	}
	if err := wm.RemoveCandidateWorktree(item.CommandID, winner.TaskID); err != nil {
		qh.log(LogLevelWarn, "ab_winner_cleanup_failed command=%s task=%s error=%v", item.CommandID, winner.TaskID, err)
	}

	qh.resolveABGroup(item.CommandID, item.GroupID, winner.TaskID, false, "", evidence, wm)
	qh.log(LogLevelInfo, "ab_group_resolved command=%s group=%s winner=%s loser=%s",
		item.CommandID, item.GroupID, winner.TaskID, loser.TaskID)
	_ = g
}

// degradeABGroupWithRepair handles an intake conflict: the logical task is
// re-executed through the standard repair machinery (a fresh non-A/B retry
// task supersedes the canonical), so the Planner hears about the retry's
// outcome instead of a stale "completed" from a candidate whose work was
// never integrated. On any failure the group stays unresolved and the next
// scan retries the whole finalize (including the intake itself).
func (qh *QueueHandler) degradeABGroupWithRepair(item abGroupWorkItem, g *model.CandidateGroup, evidence map[string]string, intakeErr error) {
	canonical := g.CandidateByTask(g.CanonicalTaskID)
	canonRow := qh.findQueueTask(canonical.WorkerID, canonical.TaskID)
	if canonRow == nil {
		qh.log(LogLevelWarn, "ab_degrade_repair_no_canonical_row command=%s task=%s (retry next scan)",
			item.CommandID, canonical.TaskID)
		return
	}
	retryHandler := NewTaskRetryHandler(qh.maestroDir, qh.config, qh.lockMap, qh.logger, qh.logLevel)
	repairTask, err := retryHandler.CreateVerifyRepairTask(canonRow,
		fmt.Sprintf("A/B winner intake conflicted with the worker branch: %v. Re-implement the task on the current state.", intakeErr))
	if err != nil {
		qh.log(LogLevelWarn, "ab_degrade_repair_create_failed command=%s task=%s error=%v (retry next scan)",
			item.CommandID, canonical.TaskID, err)
		return
	}
	repairTask.ABGroupID = "" // the re-execution is a normal single-candidate task
	if err := retryHandler.RetryTaskAtomically(repairTask, canonical.TaskID, item.CommandID, canonical.WorkerID); err != nil {
		qh.log(LogLevelWarn, "ab_degrade_repair_enqueue_failed command=%s task=%s error=%v (retry next scan)",
			item.CommandID, canonical.TaskID, err)
		return
	}
	qh.resolveABGroup(item.CommandID, item.GroupID, "", true,
		"winner intake conflicted; logical task re-enqueued as "+repairTask.ID,
		evidence, nil, "superseded_by_retry:"+repairTask.ID)
	qh.log(LogLevelInfo, "ab_degraded_with_repair command=%s group=%s repair=%s",
		item.CommandID, item.GroupID, repairTask.ID)
}

// findQueueTask reads a worker queue row by ID (lock-free atomic read).
func (qh *QueueHandler) findQueueTask(workerID, taskID string) *model.Task {
	data, err := os.ReadFile(taskQueuePath(qh.maestroDir, workerID)) //nolint:gosec // controlled queue dir
	if err != nil {
		return nil
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		return nil
	}
	for i := range tq.Tasks {
		if tq.Tasks[i].ID == taskID {
			return &tq.Tasks[i]
		}
	}
	return nil
}

// resolveABGroup writes the terminal group state under the state lock.
// canonicalSupersededReason (degraded only): when non-empty the canonical
// is cancelled with that reason (e.g. "superseded_by_retry:<id>" after a
// repair enqueue); when empty the canonical's own terminal status stands
// (both-failed case — its result entry already says failed, so the notify
// stays truthful).
func (qh *QueueHandler) resolveABGroup(commandID, groupID, winnerTaskID string, degraded bool, reason string, evidence map[string]string, wm abWorktreeOps, canonicalSupersededReason ...string) {
	statePath := commandStatePath(qh.maestroDir, commandID)
	lockKey := "state:" + commandID
	qh.lockMap.Lock(lockKey)
	defer qh.lockMap.Unlock(lockKey)

	var loserTasks []string
	if err := updateYAMLFile(statePath, func(state *model.CommandState) error {
		g := state.CandidateGroups[groupID]
		if g == nil || !g.Status.IsUnresolved() {
			return errNoUpdate // idempotent: already resolved
		}
		now := qh.clock.Now().UTC().Format(time.RFC3339)
		if g.SelectionEvidence == nil {
			g.SelectionEvidence = map[string]string{}
		}
		for k, v := range evidence {
			g.SelectionEvidence[k] = v
		}
		if state.CancelledReasons == nil {
			state.CancelledReasons = map[string]string{}
		}

		if degraded {
			// Candidate work stays auditable on candidate branches until
			// command cleanup. The canonical is only superseded when a
			// repair successor was enqueued (reason supplied); otherwise its
			// own terminal status stands so result-entry and state agree.
			g.Status = model.ABGroupDegraded
			g.WinnerTaskID = ""
			g.SelectionEvidence["degraded_reason"] = reason
			for _, c := range g.Candidates {
				if c.TaskID == g.CanonicalTaskID {
					if len(canonicalSupersededReason) > 0 && canonicalSupersededReason[0] != "" {
						state.TaskStates[c.TaskID] = model.StatusCancelled
						state.CancelledReasons[c.TaskID] = canonicalSupersededReason[0]
					}
				} else {
					state.TaskStates[c.TaskID] = model.StatusCancelled
					state.CancelledReasons[c.TaskID] = "superseded_by_ab_degraded"
					loserTasks = append(loserTasks, c.TaskID)
				}
			}
		} else {
			g.Status = model.ABGroupResolved
			g.WinnerTaskID = winnerTaskID
			if winnerTaskID != g.CanonicalTaskID {
				// Shadow won: supersede canonical → shadow with the same
				// wiring retries use (membership / phase / lineage / deps).
				// WireRetryTaskIntoState resets the successor to planned
				// (retry semantics); the A/B winner has already COMPLETED,
				// so restore its real status or the notify gate and phase
				// completion would wait forever for a re-execution that
				// never comes.
				model.WireRetryTaskIntoState(state, winnerTaskID, g.CanonicalTaskID, now)
				state.TaskStates[winnerTaskID] = model.StatusCompleted
				state.TaskStates[g.CanonicalTaskID] = model.StatusCancelled
				state.CancelledReasons[g.CanonicalTaskID] = "superseded_by_ab_winner:" + winnerTaskID
			}
			for _, c := range g.Candidates {
				if c.TaskID != winnerTaskID && state.TaskStates[c.TaskID] != model.StatusCancelled {
					state.TaskStates[c.TaskID] = model.StatusCancelled
					state.CancelledReasons[c.TaskID] = "superseded_by_ab_loser"
				}
			}
		}
		g.UpdatedAt = now
		state.UpdatedAt = now
		return nil
	}); err != nil && !errors.Is(err, errNoUpdate) {
		qh.log(LogLevelError, "ab_group_resolve_write_failed command=%s group=%s error=%v (retry next scan)",
			commandID, groupID, err)
		return
	}
	// Degraded groups keep candidate branches for audit until command
	// cleanup; loser worktree directories can go now (wm may be nil when
	// the caller already handled artifacts).
	if wm != nil {
		for _, taskID := range loserTasks {
			if err := wm.RemoveCandidateWorktree(commandID, taskID); err != nil {
				qh.log(LogLevelDebug, "ab_degraded_cleanup_failed command=%s task=%s error=%v", commandID, taskID, err)
			}
		}
	}
}

// markABGroupSelecting CAS-transitions a group racing → selecting.
func (qh *QueueHandler) markABGroupSelecting(commandID, groupID string) error {
	statePath := commandStatePath(qh.maestroDir, commandID)
	lockKey := "state:" + commandID
	qh.lockMap.Lock(lockKey)
	defer qh.lockMap.Unlock(lockKey)
	err := updateYAMLFile(statePath, func(state *model.CommandState) error {
		g := state.CandidateGroups[groupID]
		if g == nil || g.Status != model.ABGroupRacing {
			return errNoUpdate
		}
		now := qh.clock.Now().UTC().Format(time.RFC3339)
		g.Status = model.ABGroupSelecting
		g.UpdatedAt = now
		state.UpdatedAt = now
		return nil
	})
	if errors.Is(err, errNoUpdate) {
		return nil
	}
	return err
}

// abRaceTimedOut reports whether the racing window has been exhausted.
func (qh *QueueHandler) abRaceTimedOut(g *model.CandidateGroup, item abGroupWorkItem) bool {
	timeout := qh.config.ABTest.EffectiveTimeoutSec()
	if timeout <= 0 {
		timeout = item.CanonicalWallClockSec
	}
	if timeout <= 0 {
		timeout = defaultABRaceTimeoutSec
	}
	started, err := time.Parse(time.RFC3339, g.CreatedAt)
	if err != nil {
		return false // unparseable timestamp: never force a timeout on bad data
	}
	return qh.clock.Now().UTC().Sub(started) > time.Duration(timeout)*time.Second
}

// abSelectionTimedOut bounds how long a group may sit in `selecting`
// (integration busy, repeated transient failures).
func (qh *QueueHandler) abSelectionTimedOut(g *model.CandidateGroup) bool {
	limit := time.Duration(qh.config.ABTest.EffectiveSelectionTimeoutSec()) * time.Second
	entered, err := time.Parse(time.RFC3339, g.UpdatedAt)
	if err != nil {
		return false
	}
	return qh.clock.Now().UTC().Sub(entered) > limit
}

// loadABVerifyCommands returns the command-scoped verify.yaml command list.
func (qh *QueueHandler) loadABVerifyCommands(commandID string) []string {
	path := filepath.Join(qh.maestroDir, "state", "verify", commandID+".yaml")
	cfg, err := model.LoadOrDefaultVerifyConfigForProject(filepath.Dir(qh.maestroDir), path)
	if err != nil || cfg == nil {
		qh.log(LogLevelDebug, "ab_verify_config_unavailable command=%s error=%v", commandID, err)
		return nil
	}
	return cfg.AllCommands()
}

// readCommandState is a lock-free atomic read of the command state file.
func (qh *QueueHandler) readCommandState(commandID string) (*model.CommandState, error) {
	data, err := os.ReadFile(commandStatePath(qh.maestroDir, commandID)) //nolint:gosec // controlled state path
	if err != nil {
		return nil, err
	}
	var cs model.CommandState
	if err := yamlv3.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("parse command state: %w", err)
	}
	return &cs, nil
}
