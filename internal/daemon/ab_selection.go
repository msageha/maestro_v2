package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	RunCandidateSelection(ctx context.Context, commandID, groupID string, candidates []worktree.ABSelectionInput, verifyCmds []string, crossPatterns []string, judgeModels []string) (*worktree.ABSelectionOutcome, error)
	IntakeWinner(commandID, workerID, candidateBranch, taskID string) error
}

// findABCandidateGroup loads the command state lock-free and returns the
// candidate group containing taskID. Returns nil when the task belongs to
// no group or the state is unreadable — callers treat that as "not an A/B
// candidate" because blocking normal machinery on a state read hiccup is
// worse than one redundant action.
func findABCandidateGroup(maestroDir, commandID, taskID string) *model.CandidateGroup {
	if commandID == "" || taskID == "" {
		return nil
	}
	data, err := os.ReadFile(commandStatePath(maestroDir, commandID)) //nolint:gosec // controlled state path
	if err != nil {
		return nil
	}
	var cs model.CommandState
	if err := yamlv3.Unmarshal(data, &cs); err != nil {
		return nil
	}
	_, g := cs.FindCandidateGroupByTask(taskID)
	return g
}

// abGroupWorkItem is collected in Phase A from the queue + state snapshot.
type abGroupWorkItem struct {
	CommandID string
	GroupID   string
	// QueueStatuses snapshots the candidates' queue row statuses. A task
	// ABSENT from the map has NO queue row (dead-lettered / archived / never
	// enqueued) — Phase B falls back to the durable state entry for it.
	QueueStatuses map[string]model.Status
	// RowWorkers maps task ID → worker queue holding its row.
	RowWorkers map[string]string
	// RowTagged records whether the row still carries the ab_group_id tag
	// (false = crashed fan-out left the canonical untagged).
	RowTagged map[string]bool
	// CanonicalWallClockSec carries the candidates' abort budget for the
	// race-timeout fallback (0 when unset).
	CanonicalWallClockSec int
}

// abRowInfo is the per-row slice of the queue snapshot indexed by task ID.
type abRowInfo struct {
	WorkerID  string
	Status    model.Status
	Tagged    bool
	WallClock int
}

// collectABGroupWork emits one work item per unresolved candidate group.
// Discovery is STATE-DRIVEN first (the durable CandidateGroups map is the
// SSOT — groups whose rows were deleted by dead-letter/archive or never
// written still get an item), then tag-driven for orphan rows whose group
// is missing from state (crashed legacy fan-out) so Phase B can repair them.
func (qh *QueueHandler) collectABGroupWork(taskQueues map[string]*taskQueueEntry, work *deferredWork) {
	rows := map[string]abRowInfo{}
	type agg struct {
		commandID string
		taskIDs   []string
	}
	taggedGroups := map[string]*agg{}
	for _, tq := range taskQueues {
		if tq == nil {
			continue
		}
		workerID := workerIDFromPath(tq.Path)
		for i := range tq.Queue.Tasks {
			t := &tq.Queue.Tasks[i]
			info := abRowInfo{WorkerID: workerID, Status: t.Status, Tagged: t.ABGroupID != ""}
			if t.DefinitionOfAbort != nil {
				info.WallClock = t.DefinitionOfAbort.MaxWallClockSec
			}
			rows[t.ID] = info
			if t.ABGroupID == "" {
				continue
			}
			a := taggedGroups[t.ABGroupID]
			if a == nil {
				a = &agg{commandID: t.CommandID}
				taggedGroups[t.ABGroupID] = a
			}
			a.taskIDs = append(a.taskIDs, t.ID)
		}
	}

	buildItem := func(commandID, groupID string, taskIDs []string) abGroupWorkItem {
		item := abGroupWorkItem{
			CommandID:     commandID,
			GroupID:       groupID,
			QueueStatuses: map[string]model.Status{},
			RowWorkers:    map[string]string{},
			RowTagged:     map[string]bool{},
		}
		for _, id := range taskIDs {
			r, ok := rows[id]
			if !ok {
				continue
			}
			item.QueueStatuses[id] = r.Status
			item.RowWorkers[id] = r.WorkerID
			item.RowTagged[id] = r.Tagged
			if r.WallClock > item.CanonicalWallClockSec {
				item.CanonicalWallClockSec = r.WallClock
			}
		}
		return item
	}

	emitted := map[string]bool{}
	for _, cs := range qh.readABCommandStates() {
		for gid, g := range cs.CandidateGroups {
			if g == nil || !g.Status.IsUnresolved() {
				continue
			}
			ids := make([]string, 0, len(g.Candidates))
			for _, c := range g.Candidates {
				ids = append(ids, c.TaskID)
			}
			work.abGroups = append(work.abGroups, buildItem(cs.CommandID, gid, ids))
			emitted[cs.CommandID+"/"+gid] = true
		}
	}
	for groupID, a := range taggedGroups {
		if emitted[a.commandID+"/"+groupID] {
			continue
		}
		work.abGroups = append(work.abGroups, buildItem(a.commandID, groupID, a.taskIDs))
	}
}

// abStateCache caches per-file results of readABCommandStates keyed by
// path. AtomicWrite always replaces the file (rename → fresh inode +
// mtime), so mtime+size is a reliable change detector here.
type abStateCache struct {
	mu      sync.Mutex
	entries map[string]abStateCacheEntry
}

type abStateCacheEntry struct {
	modTimeNano int64
	size        int64
	// state is the parsed command state when the file contains candidate
	// groups, nil otherwise. READ-ONLY for consumers — collectABGroupWork
	// copies what it needs and never mutates.
	state *model.CommandState
}

// readABCommandStates returns parsed command states containing a
// candidate_groups section. Two cheap layers: an mtime+size cache skips
// unchanged files entirely, and a byte pre-filter skips YAML parsing for
// states that never ran an A/B race.
func (qh *QueueHandler) readABCommandStates() []*model.CommandState {
	dir := filepath.Join(qh.maestroDir, "state", "commands")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	cache := &qh.abStateCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = map[string]abStateCacheEntry{}
	}

	var out []*model.CommandState
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		seen[path] = true
		info, err := e.Info()
		if err != nil {
			continue
		}
		if c, ok := cache.entries[path]; ok &&
			c.modTimeNano == info.ModTime().UnixNano() && c.size == info.Size() {
			if c.state != nil {
				out = append(out, c.state)
			}
			continue
		}
		entry := abStateCacheEntry{modTimeNano: info.ModTime().UnixNano(), size: info.Size()}
		data, err := os.ReadFile(path) //nolint:gosec // controlled state dir
		if err == nil && bytes.Contains(data, []byte("candidate_groups")) {
			var cs model.CommandState
			if err := yamlv3.Unmarshal(data, &cs); err == nil && len(cs.CandidateGroups) > 0 {
				entry.state = &cs
			}
		}
		cache.entries[path] = entry
		if entry.state != nil {
			out = append(out, entry.state)
		}
	}
	// Prune deleted files so the cache cannot grow unboundedly.
	for path := range cache.entries {
		if !seen[path] {
			delete(cache.entries, path)
		}
	}
	return out
}

// stepABSelection drives every collected candidate group one transition
// forward. Runs in Phase B (no scan lock held; git + state file I/O).
func (qh *QueueHandler) stepABSelection(ctx context.Context, pa *phaseAResult) {
	if len(pa.work.abGroups) == 0 {
		return
	}
	wm, ok := qh.worktreeManager.(abWorktreeOps)
	if !ok {
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
	if g == nil {
		// Tagged rows without a durable group: legacy fan-out (queues
		// written before state) crashed mid-way. Repair so the rows stop
		// freezing their workers and any candidate-branch work is recovered.
		qh.repairOrphanABRows(wm, item, state)
		return
	}
	if !g.Status.IsUnresolved() {
		return // resolved/degraded already
	}

	canonical := g.CandidateByTask(g.CanonicalTaskID)
	shadow := g.OtherCandidate(g.CanonicalTaskID)
	if canonical == nil || shadow == nil {
		qh.resolveABGroup(item.CommandID, item.GroupID, g.CanonicalTaskID, true,
			"malformed candidate group", map[string]string{"degraded": "malformed_group"})
		return
	}

	// Fan-out incomplete (state-first write order): the group exists but the
	// canonical row never received its tag — the canonical is living a
	// normal NON-candidate life in the worker worktree, so the candidate
	// machinery (commit/intake from candidate branches) cannot apply to it.
	// Degrade: the canonical keeps its own status/lifecycle untouched, the
	// shadow is superseded.
	if tagged, present := item.RowTagged[canonical.TaskID]; present && !tagged {
		qh.resolveABGroup(item.CommandID, item.GroupID, "", true,
			"fan-out incomplete: canonical row untagged",
			map[string]string{"degraded": "fanout_incomplete"})
		return
	}
	// Shadow row never enqueued (crash between the state and shadow-queue
	// writes): nobody will ever pick it up — cancel it in state so the race
	// converges to a canonical walkover instead of hanging until timeout.
	if _, present := item.QueueStatuses[shadow.TaskID]; !present &&
		state.TaskStates[shadow.TaskID] == model.StatusPending {
		qh.cancelABCandidateStateOnly(item.CommandID, shadow.TaskID, "ab_fanout_incomplete")
		return // re-collected with the cancelled state on the next scan
	}

	// Effective status: the queue row when one exists, else the durable
	// state entry. Rows can vanish while the group is unresolved (dead-letter
	// deletes exhausted pending rows and mirrors `failed` into state); the
	// group must still converge (#1 of the 2026-06-12 A/B audit).
	eff := func(taskID string) model.Status {
		if s, present := item.QueueStatuses[taskID]; present {
			return s
		}
		return state.TaskStates[taskID]
	}
	completed := func(taskID string) bool { return eff(taskID) == model.StatusCompleted }
	terminal := func(taskID string) bool { return model.IsTerminal(eff(taskID)) }

	switch {
	case terminal(canonical.TaskID) && terminal(shadow.TaskID):
		// Race finished — run (or re-run after a crash) the selection.
		qh.runABSelectionAndResolve(ctx, wm, item, state, g, completed)

	case qh.abRaceTimedOut(g, item):
		// Race budget exhausted with a non-terminal candidate left. A
		// RUNNING loser is never finalized destructively here — its worker
		// is executing inside the candidate worktree, and its own
		// DefinitionOfAbort / lease machinery bounds it; the all-terminal
		// branch above finalizes afterwards. Cancellation is limited to
		// candidates no worker is executing: a still-PENDING row (never
		// dispatched) or a rowless candidate whose state entry never went
		// terminal (lost row).
		for _, c := range []*model.ABCandidate{canonical, shadow} {
			other := g.OtherCandidate(c.TaskID)
			if other == nil || !completed(other.TaskID) {
				continue
			}
			s, present := item.QueueStatuses[c.TaskID]
			switch {
			case present && s == model.StatusPending:
				qh.cancelPendingABCandidate(item.CommandID, c)
			case !present && !model.IsTerminal(state.TaskStates[c.TaskID]):
				qh.cancelABCandidateStateOnly(item.CommandID, c.TaskID, "superseded_by_ab_timeout")
			}
		}
		qh.log(LogLevelDebug, "ab_race_timeout_waiting command=%s group=%s statuses=%v",
			item.CommandID, item.GroupID, item.QueueStatuses)
	}
	// else: still racing within budget — wait for the next scan.
}

// cancelABCandidateStateOnly CAS-cancels a candidate that has NO queue row
// (never enqueued, or the row was deleted while undispatched) directly in
// command state so the group can converge.
func (qh *QueueHandler) cancelABCandidateStateOnly(commandID, taskID, reason string) {
	stateKey := "state:" + commandID
	qh.lockMap.Lock(stateKey)
	defer qh.lockMap.Unlock(stateKey)
	if err := updateYAMLFile(commandStatePath(qh.maestroDir, commandID), func(state *model.CommandState) error {
		if model.IsTerminal(state.TaskStates[taskID]) {
			return errNoUpdate
		}
		if state.CancelledReasons == nil {
			state.CancelledReasons = map[string]string{}
		}
		state.SetTaskState(taskID, model.StatusCancelled, qh.clock.Now().UTC().Format(time.RFC3339))
		state.CancelledReasons[taskID] = reason
		state.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
		return nil
	}); err != nil {
		if !errors.Is(err, errNoUpdate) {
			qh.log(LogLevelWarn, "ab_state_cancel_failed command=%s task=%s error=%v", commandID, taskID, err)
		}
		return
	}
	qh.log(LogLevelInfo, "ab_candidate_state_cancelled command=%s task=%s reason=%s", commandID, taskID, reason)
}

// repairOrphanABRows handles tagged queue rows whose group is absent from a
// READABLE command state (legacy queue-first fan-out crashed between the
// queue and state writes). Without repair such rows freeze their workers
// forever (fail-closed) and route work to candidate branches nobody intakes.
func (qh *QueueHandler) repairOrphanABRows(wm abWorktreeOps, item abGroupWorkItem, state *model.CommandState) {
	for taskID, workerID := range item.RowWorkers {
		if !item.RowTagged[taskID] {
			continue
		}
		status := item.QueueStatuses[taskID]
		_, knownToState := state.TaskStates[taskID]
		switch {
		case status == model.StatusInProgress:
			// A worker is executing in the candidate worktree; lease/DOA
			// bounds it. Repair once it goes terminal.
			continue
		case knownToState && status == model.StatusCompleted:
			// Work happened inside the candidate worktree — recover it into
			// the worker branch before releasing the freeze.
			if err := wm.CommitCandidateChanges(item.CommandID, taskID); err != nil {
				qh.log(LogLevelWarn, "ab_orphan_commit_failed command=%s task=%s error=%v (retry next scan)",
					item.CommandID, taskID, err)
				continue
			}
			if err := wm.IntakeWinner(item.CommandID, workerID,
				model.ABCandidateBranch(item.CommandID, taskID), taskID); err != nil {
				// The work cannot be merged: a bare tag strip would let a
				// completed-but-unintegrated task flow onward (silent work
				// loss). Re-execute through the standard repair machinery
				// instead; only then release the freeze. The candidate
				// branch stays for audit until command cleanup.
				qh.log(LogLevelError, "ab_orphan_intake_failed command=%s task=%s error=%v (re-executing via repair task)",
					item.CommandID, taskID, err)
				if !qh.enqueueOrphanRepair(item.CommandID, workerID, taskID, err) {
					continue // keep the tag (fail-closed freeze); retry next scan
				}
			}
			qh.stripABTag(workerID, taskID, false)
		case knownToState:
			// pending / failed / cancelled: no candidate work worth
			// recovering — strip the tag so a pending row rejoins the
			// normal pipeline and a terminal row rests as a plain row.
			qh.stripABTag(workerID, taskID, false)
		default:
			// Unknown to state: a shadow remnant. It must never dispatch
			// (it would duplicate the canonical's work) and must stop
			// freezing its worker.
			qh.stripABTag(workerID, taskID, true)
		}
	}
}

// enqueueOrphanRepair re-executes an orphan candidate's logical task through
// the standard repair machinery (idempotent via the wired RetryLineage
// successor: a repeated call detects the previous repair and succeeds
// without re-issuing). Returns false when no repair could be ensured.
func (qh *QueueHandler) enqueueOrphanRepair(commandID, workerID, taskID string, cause error) bool {
	if cs, err := qh.readCommandState(commandID); err == nil {
		for _, pred := range cs.RetryLineage {
			if pred == taskID {
				return true // a successor is already wired
			}
		}
	}
	row := qh.findQueueTask(workerID, taskID)
	if row == nil {
		qh.log(LogLevelWarn, "ab_orphan_repair_no_row command=%s task=%s (retry next scan)", commandID, taskID)
		return false
	}
	retryHandler := NewTaskRetryHandler(qh.maestroDir, qh.config, qh.lockMap, qh.logger, qh.logLevel)
	repairTask, err := retryHandler.CreateVerifyRepairTask(row,
		fmt.Sprintf("orphan A/B candidate work could not be merged into the worker branch: %v. Re-implement the task on the current state.", cause))
	if err != nil {
		qh.log(LogLevelWarn, "ab_orphan_repair_create_failed command=%s task=%s error=%v", commandID, taskID, err)
		return false
	}
	repairTask.ABGroupID = ""
	if err := retryHandler.RetryTaskAtomically(repairTask, taskID, commandID, workerID); err != nil {
		qh.log(LogLevelWarn, "ab_orphan_repair_enqueue_failed command=%s task=%s error=%v", commandID, taskID, err)
		return false
	}
	qh.log(LogLevelInfo, "ab_orphan_repair_enqueued command=%s task=%s repair=%s", commandID, taskID, repairTask.ID)
	return true
}

// stripABTag CAS-clears a queue row's ab_group_id (releasing the fail-closed
// freeze); when cancelPending is set a still-pending row is also cancelled.
func (qh *QueueHandler) stripABTag(workerID, taskID string, cancelPending bool) {
	queueKey := "queue:" + workerID
	qh.lockMap.Lock(queueKey)
	defer qh.lockMap.Unlock(queueKey)
	if err := updateYAMLFile(taskQueuePath(qh.maestroDir, workerID), func(tq *model.TaskQueue) error {
		for i := range tq.Tasks {
			t := &tq.Tasks[i]
			if t.ID != taskID || t.ABGroupID == "" {
				continue
			}
			t.ABGroupID = ""
			if cancelPending && t.Status == model.StatusPending {
				t.Status = model.StatusCancelled
			}
			t.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
			return nil
		}
		return errNoUpdate
	}); err != nil {
		if !errors.Is(err, errNoUpdate) {
			qh.log(LogLevelWarn, "ab_tag_strip_failed worker=%s task=%s error=%v", workerID, taskID, err)
		}
		return
	}
	qh.log(LogLevelInfo, "ab_orphan_row_repaired worker=%s task=%s cancel_pending=%v", workerID, taskID, cancelPending)
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
		state.SetTaskState(c.TaskID, model.StatusCancelled, qh.clock.Now().UTC().Format(time.RFC3339))
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
			map[string]string{"degraded": "both_failed"})
		return
	}

	// Repair-priority (crash between repair enqueue and resolve): a wired
	// NON-candidate successor of the canonical proves a repair task already
	// exists, so the winner must not be intaken anymore — finishing it now
	// would double-execute the logical task. While the group is unresolved
	// such a successor can only come from degradeABGroupWithRepair (daemon
	// and Planner retries are gated for candidates), so this check is sound
	// and must run BEFORE the pending-winner replay.
	for succ, pred := range state.RetryLineage {
		if pred == g.CanonicalTaskID && g.CandidateByTask(succ) == nil {
			qh.degradeABGroupWithRepair(item, g,
				map[string]string{"degraded": "repair_already_enqueued"},
				fmt.Errorf("repair successor %s already wired", succ))
			return
		}
	}

	// Durable winner from a crashed previous attempt: re-finalize EXACTLY
	// that decision. Re-running the selection could flake to a different
	// winner and merge a SECOND candidate branch into another worker. The
	// replay is still bounded by the selecting timeout: a permanently
	// failing winner commit escapes to the repair degrade instead of
	// wedging on the recorded decision forever.
	if g.PendingWinnerTaskID != "" {
		if winner := g.CandidateByTask(g.PendingWinnerTaskID); winner != nil {
			if qh.abSelectionTimedOut(g) {
				if err := wm.CommitCandidateChanges(item.CommandID, winner.TaskID); err != nil {
					qh.log(LogLevelWarn, "ab_pending_winner_unrecoverable command=%s group=%s winner=%s error=%v",
						item.CommandID, item.GroupID, winner.TaskID, err)
					qh.degradeABGroupWithRepair(item, g,
						map[string]string{"degraded": "pending_winner_timeout"},
						fmt.Errorf("pending winner unrecoverable past the selection budget: %w", err))
					return
				}
			}
			qh.finalizeABWinner(wm, item, g, winner, g.OtherCandidate(winner.TaskID),
				map[string]string{"recovered": "pending_winner"})
			return
		}
	}

	// Mark selecting FIRST (CAS: racing → selecting): the selection-timeout
	// clock then bounds EVERY later step — including a permanently failing
	// candidate commit — so the degrade escape below stays reachable.
	if g.Status == model.ABGroupRacing {
		if err := qh.markABGroupSelecting(item.CommandID, item.GroupID); err != nil {
			qh.log(LogLevelWarn, "ab_group_mark_selecting_failed command=%s group=%s error=%v",
				item.CommandID, item.GroupID, err)
			return
		}
	} else if qh.abSelectionTimedOut(g) {
		// Selection never converged within the budget. Walk over to the
		// first finisher (canonical preferred) when its work is still
		// recoverable; otherwise re-execute the logical task through the
		// repair machinery, which needs no candidate artifacts.
		w := finished[0]
		if err := wm.CommitCandidateChanges(item.CommandID, w.TaskID); err == nil {
			qh.finalizeABWinner(wm, item, g, w, g.OtherCandidate(w.TaskID),
				map[string]string{"walkover": "selection_timeout"})
			return
		} else { //nolint:revive // keep err scoped to the walkover attempt
			qh.log(LogLevelWarn, "ab_selection_timeout_walkover_unrecoverable command=%s group=%s error=%v",
				item.CommandID, item.GroupID, err)
			qh.degradeABGroupWithRepair(item, g,
				map[string]string{"degraded": "selection_timeout"},
				fmt.Errorf("selection timed out and the finisher's work is unrecoverable: %w", err))
			return
		}
	}

	// Commit completed candidates' worktrees (idempotent; bounded by the
	// selecting timeout above when permanently failing).
	for _, c := range finished {
		if err := wm.CommitCandidateChanges(item.CommandID, c.TaskID); err != nil {
			qh.log(LogLevelWarn, "ab_candidate_commit_failed command=%s task=%s error=%v (retry next scan)",
				item.CommandID, c.TaskID, err)
			return
		}
	}

	// Selection runs for ONE finisher too (walkover verification, design
	// §5): a healthy verifier must pass the sole finisher before intake.
	verifyCmds := qh.loadABVerifyCommands(item.CommandID)
	// Both candidates share the LOGICAL task's context (expected_paths for
	// the Stage 2 deviation metric; purpose/acceptance for the Stage 3
	// judges): use whichever queue row survives so a one-sided row loss
	// cannot skew the signals.
	sharedRow := qh.abSurvivingRow(canonical, shadow)
	inputs := make([]worktree.ABSelectionInput, 0, len(finished)) // canonical FIRST: ties go to it
	for _, c := range finished {
		in := worktree.ABSelectionInput{TaskID: c.TaskID, Branch: c.Branch}
		if sharedRow != nil {
			in.ExpectedPaths = sharedRow.ExpectedPaths
			in.TaskPurpose = sharedRow.Purpose
			in.AcceptanceCriteria = sharedRow.AcceptanceCriteria
		}
		inputs = append(inputs, in)
	}
	outcome, err := wm.RunCandidateSelection(ctx, item.CommandID, item.GroupID, inputs, verifyCmds,
		qh.config.ABTest.EffectiveCrossTestPatterns(), qh.config.ABTest.EffectiveJudgeModels())
	if err != nil {
		switch {
		case errors.Is(err, worktree.ErrSelectionBusy):
			qh.log(LogLevelDebug, "ab_selection_deferred command=%s group=%s (integration busy)",
				item.CommandID, item.GroupID)
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			// Shutdown — resume on the next scan, never a verdict.
			qh.log(LogLevelDebug, "ab_selection_interrupted command=%s group=%s (retry next scan)",
				item.CommandID, item.GroupID)
		default:
			qh.log(LogLevelWarn, "ab_selection_failed command=%s group=%s error=%v (walkover to first finisher)",
				item.CommandID, item.GroupID, err)
			qh.finalizeABWinner(wm, item, g, finished[0], g.OtherCandidate(finished[0].TaskID),
				map[string]string{"walkover": "selection_error", "selection_error": err.Error()})
		}
		return // busy / interrupted: selecting persists; selection-timeout bounds the wait
	}

	evidence := outcome.Evidence
	if evidence == nil {
		evidence = map[string]string{}
	}
	if outcome.SoleCandidateFailed {
		// Healthy verifier, sole finisher demonstrably bad: re-execute the
		// logical task instead of shipping verified-bad work.
		evidence["degraded"] = "sole_finisher_failed_verification"
		qh.degradeABGroupWithRepair(item, g, evidence,
			errors.New("sole finisher failed verification against a healthy baseline"))
		return
	}
	winner := finished[0]
	if outcome.Degraded {
		// Mechanical signal unavailable (verifier broken / no verifier /
		// all candidates conflicted). First finisher (canonical preferred)
		// wins by deterministic default; signal the Planner that the
		// verifier needs attention.
		evidence["degraded_selection"] = outcome.Reason
		qh.log(LogLevelWarn, "ab_selection_no_signal command=%s group=%s reason=%q (default win)",
			item.CommandID, item.GroupID, outcome.Reason)
	} else if w := g.CandidateByTask(outcome.WinnerTaskID); w != nil {
		winner = w
	}
	qh.finalizeABWinner(wm, item, g, winner, g.OtherCandidate(winner.TaskID), evidence)
}

// finalizeABWinner records the decision durably, performs intake (winner
// candidate branch → its worker branch) and writes the state resolution.
// Candidate worktrees/branches are intentionally NOT removed here — they
// stay for audit until command cleanup (design §7), which also removes the
// crash window between artifact removal and resolution. An intake conflict
// degrades the group with a repair re-execution (design §6, PR1).
func (qh *QueueHandler) finalizeABWinner(wm abWorktreeOps, item abGroupWorkItem, g *model.CandidateGroup, winner, loser *model.ABCandidate, evidence map[string]string) {
	// Durable decision BEFORE any worker-branch mutation: a crash after the
	// intake then re-finalizes this exact winner instead of re-deciding.
	if err := qh.markABPendingWinner(item.CommandID, item.GroupID, winner.TaskID); err != nil {
		qh.log(LogLevelWarn, "ab_pending_winner_mark_failed command=%s group=%s winner=%s error=%v (retry next scan)",
			item.CommandID, item.GroupID, winner.TaskID, err)
		return
	}
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

	qh.resolveABGroup(item.CommandID, item.GroupID, winner.TaskID, false, "", evidence)
	loserID := "<none>"
	if loser != nil {
		loserID = loser.TaskID
	}
	qh.log(LogLevelInfo, "ab_group_resolved command=%s group=%s winner=%s loser=%s",
		item.CommandID, item.GroupID, winner.TaskID, loserID)
}

// markABPendingWinner CAS-records the selection decision in the durable
// group. Returns an error when the group is already resolved or a DIFFERENT
// pending winner is recorded (the next scan finalizes that one instead).
func (qh *QueueHandler) markABPendingWinner(commandID, groupID, taskID string) error {
	lockKey := "state:" + commandID
	qh.lockMap.Lock(lockKey)
	defer qh.lockMap.Unlock(lockKey)
	err := updateYAMLFile(commandStatePath(qh.maestroDir, commandID), func(state *model.CommandState) error {
		g := state.CandidateGroups[groupID]
		switch {
		case g == nil || !g.Status.IsUnresolved():
			return fmt.Errorf("group %s missing or already resolved", groupID)
		case g.PendingWinnerTaskID == taskID:
			return errNoUpdate // already recorded — proceed
		case g.PendingWinnerTaskID != "":
			return fmt.Errorf("conflicting pending winner %s already recorded", g.PendingWinnerTaskID)
		}
		now := qh.clock.Now().UTC().Format(time.RFC3339)
		g.PendingWinnerTaskID = taskID
		g.UpdatedAt = now
		state.UpdatedAt = now
		return nil
	})
	if errors.Is(err, errNoUpdate) {
		return nil
	}
	return err
}

// degradeABGroupWithRepair degrades a group whose winner cannot be intaken
// (intake conflict, unrecoverable timeout, sole finisher failed verify): the
// logical task is re-executed through the standard repair machinery (a fresh
// non-A/B retry task supersedes the canonical), so the Planner hears about
// the retry's outcome instead of a stale "completed" from a candidate whose
// work was never integrated. On transient failure the group stays unresolved
// and the next scan retries.
func (qh *QueueHandler) degradeABGroupWithRepair(item abGroupWorkItem, g *model.CandidateGroup, evidence map[string]string, cause error) {
	canonical := g.CandidateByTask(g.CanonicalTaskID)

	// Idempotency: a previous attempt may have enqueued the repair and then
	// crashed (or failed) before resolving the group. RetryTaskAtomically
	// wires RetryLineage[repair]=canonical durably at enqueue time, so a
	// non-candidate successor of the canonical proves a repair exists —
	// re-issuing would double-execute the logical task. Only finish the
	// resolution in that case.
	if cs, err := qh.readCommandState(item.CommandID); err == nil {
		for succ, pred := range cs.RetryLineage {
			if pred == canonical.TaskID && g.CandidateByTask(succ) == nil {
				qh.resolveABGroup(item.CommandID, item.GroupID, "", true,
					"repair already enqueued ("+succ+")",
					evidence, "superseded_by_retry:"+succ)
				return
			}
		}
	}

	canonRow := qh.findQueueTask(canonical.WorkerID, canonical.TaskID)
	if canonRow == nil {
		// No row to derive a repair from (dead-lettered / archived). The
		// canonical's own terminal state stands — resolve degraded so the
		// command converges; evidence records why no repair was issued.
		evidence["repair_skipped"] = "canonical row missing"
		qh.resolveABGroup(item.CommandID, item.GroupID, "", true,
			fmt.Sprintf("degraded without repair (no canonical row): %v", cause), evidence)
		return
	}
	retryHandler := NewTaskRetryHandler(qh.maestroDir, qh.config, qh.lockMap, qh.logger, qh.logLevel)
	repairTask, err := retryHandler.CreateVerifyRepairTask(canonRow,
		fmt.Sprintf("A/B selection degraded: %v. Re-implement the task on the current state.", cause))
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
		"winner unusable; logical task re-enqueued as "+repairTask.ID,
		evidence, "superseded_by_retry:"+repairTask.ID)
	qh.log(LogLevelInfo, "ab_degraded_with_repair command=%s group=%s repair=%s",
		item.CommandID, item.GroupID, repairTask.ID)
}

// abSurvivingRow resolves the logical task's queue row from whichever
// candidate row still exists (canonical preferred). Nil when both rows are
// gone — Stage 2 then skips the deviation metric and the judges get no
// task context.
func (qh *QueueHandler) abSurvivingRow(candidates ...*model.ABCandidate) *model.Task {
	for _, c := range candidates {
		if c == nil {
			continue
		}
		if row := qh.findQueueTask(c.WorkerID, c.TaskID); row != nil {
			return row
		}
	}
	return nil
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
func (qh *QueueHandler) resolveABGroup(commandID, groupID, winnerTaskID string, degraded bool, reason string, evidence map[string]string, canonicalSupersededReason ...string) {
	statePath := commandStatePath(qh.maestroDir, commandID)
	lockKey := "state:" + commandID

	// Side-effect snapshot, applied AFTER the state lock is released:
	// bandit rewards (resolved only — degraded has no win/lose label) and
	// the verifier-weak signal (its own queue lock must not nest inside
	// the state lock).
	var rewards []abBanditOutcome
	applied := false

	qh.lockMap.Lock(lockKey)
	err := updateYAMLFile(statePath, func(state *model.CommandState) error {
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
			// Candidate work stays auditable on candidate worktrees/branches
			// until command cleanup. The canonical is only superseded when a
			// repair successor was enqueued (reason supplied); otherwise its
			// own terminal status stands so result-entry and state agree.
			g.Status = model.ABGroupDegraded
			g.WinnerTaskID = ""
			g.SelectionEvidence["degraded_reason"] = reason
			for _, c := range g.Candidates {
				if c.TaskID == g.CanonicalTaskID {
					if len(canonicalSupersededReason) > 0 && canonicalSupersededReason[0] != "" {
						state.SetTaskState(c.TaskID, model.StatusCancelled, now)
						state.CancelledReasons[c.TaskID] = canonicalSupersededReason[0]
					}
				} else {
					state.SetTaskState(c.TaskID, model.StatusCancelled, now)
					state.CancelledReasons[c.TaskID] = "superseded_by_ab_degraded"
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
				state.SetTaskState(winnerTaskID, model.StatusCompleted, now)
				state.SetTaskState(g.CanonicalTaskID, model.StatusCancelled, now)
				state.CancelledReasons[g.CanonicalTaskID] = "superseded_by_ab_winner:" + winnerTaskID
			}
			for _, c := range g.Candidates {
				if c.TaskID != winnerTaskID && state.TaskStates[c.TaskID] != model.StatusCancelled {
					state.SetTaskState(c.TaskID, model.StatusCancelled, now)
					state.CancelledReasons[c.TaskID] = "superseded_by_ab_loser"
				}
			}
		}
		g.PendingWinnerTaskID = "" // decision is final; WinnerTaskID is the record
		g.UpdatedAt = now
		state.UpdatedAt = now
		applied = true
		if !degraded {
			for _, c := range g.Candidates {
				rewards = append(rewards, abBanditOutcome{
					Model: c.Model, BloomLevel: c.BloomLevel, Win: c.TaskID == winnerTaskID,
				})
			}
		}
		return nil
	})
	qh.lockMap.Unlock(lockKey)
	if err != nil && !errors.Is(err, errNoUpdate) {
		qh.log(LogLevelError, "ab_group_resolve_write_failed command=%s group=%s error=%v (retry next scan)",
			commandID, groupID, err)
		return
	}
	if !applied {
		return // idempotent re-entry: side effects already happened once
	}
	// Candidate worktrees/branches are kept for audit until command cleanup
	// (cleanupCommandCore removes them); nothing to remove here.

	// Design §8: the race outcome is the authoritative pairwise win/lose
	// label for the contextual bandit (the candidates' normal per-result
	// rewards were skipped by the learning gate). Walkovers count — a
	// candidate that failed or never finished IS a loss signal.
	if sel := qh.resultHandler.getModelSelector(); sel != nil {
		for _, r := range rewards {
			reward := 0.0
			if r.Win {
				reward = 1.0
			}
			sel.RecordResult(r.Model, r.BloomLevel, reward)
			qh.log(LogLevelInfo, "ab_bandit_outcome command=%s group=%s model=%s bloom=%d win=%v",
				commandID, groupID, r.Model, r.BloomLevel, r.Win)
		}
	}

	// Weak mechanical signal → tell the Planner how to fix it (design §8:
	// verifier_weak). Once per group via the signal dedup key.
	if weak := abWeakVerifierReason(evidence); weak != "" {
		qh.emitABVerifierWeakSignal(commandID, groupID, weak)
	}
}

// abBanditOutcome is one candidate's win/lose label captured at resolution.
type abBanditOutcome struct {
	Model      string
	BloomLevel int
	Win        bool
}

// abWeakVerifierReason classifies evidence that the selection ran without a
// meaningful mechanical signal ("" = signal was fine).
func abWeakVerifierReason(evidence map[string]string) string {
	switch {
	case evidence["stage0"] == "no_verifier":
		return "no_verifier"
	case evidence["stage0"] == "verifier_broken":
		return "verifier_broken"
	case evidence["degraded_selection"] != "":
		return "degraded_selection"
	}
	return ""
}

// emitABVerifierWeakSignal enqueues a Planner signal asking for a real
// verify snapshot. Deduplicated per group by Kind+CommandID+PhaseID.
func (qh *QueueHandler) emitABVerifierWeakSignal(commandID, groupID, reason string) {
	now := qh.clock.Now().UTC().Format(time.RFC3339)
	sig := model.PlannerSignal{
		Kind:      "ab_verifier_weak",
		CommandID: commandID,
		PhaseID:   "__ab_" + groupID,
		Message: fmt.Sprintf("[maestro] kind:ab_verifier_weak command_id:%s group:%s\nreason: %s\n"+
			"next_action: A/B 選抜の機械シグナルが弱い状態で確定しました。次の command では `maestro verify write` で build/test を含む command-scoped verify snapshot を書いてください。",
			commandID, groupID, reason),
		Reason:    reason,
		CreatedAt: now,
		UpdatedAt: now,
	}
	qh.lockMap.Lock("queue:planner_signals")
	defer qh.lockMap.Unlock("queue:planner_signals")
	if err := updateYAMLFile(signalQueuePath(qh.maestroDir), func(sq *model.PlannerSignalQueue) error {
		index := buildSignalIndex(sq.Signals)
		if _, exists := index[signalDedupKey(sig)]; exists {
			return errNoUpdate
		}
		if sq.SchemaVersion == 0 {
			sq.SchemaVersion = 1
			sq.FileType = "planner_signal_queue"
		}
		sq.Signals = append(sq.Signals, sig)
		return nil
	}); err != nil {
		if !errors.Is(err, errNoUpdate) {
			qh.log(LogLevelWarn, "ab_verifier_weak_signal_failed command=%s group=%s error=%v", commandID, groupID, err)
		}
		return
	}
	qh.log(LogLevelInfo, "ab_verifier_weak_signal_queued command=%s group=%s reason=%s", commandID, groupID, reason)
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
