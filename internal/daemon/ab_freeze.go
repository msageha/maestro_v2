package daemon

// A/B worker freeze (docs/design/ab_candidate_selection.md §3).
//
// While a candidate group is unresolved, the two workers executing its
// candidates must not receive any OTHER task: the post-selection intake
// merges the winner's candidate branch into the canonical worker's branch,
// and that merge is only race-free while the worker is idle. The freeze is
// computed per scan from the durable CandidateGroups state, so it survives
// daemon restarts and self-heals when groups resolve.

// computeABFreeze derives the frozen-worker map from the queue snapshot:
//
//	workerID → set of task IDs that MAY dispatch to that worker
//
// FAIL-CLOSED construction: every A/B candidate row freezes its worker by
// default (allowing only itself); a worker is released only when the
// owning command state is READABLE and the row's group is resolved /
// degraded. An unreadable state therefore keeps the freeze — dispatching a
// foreign task to a worker whose group might still be unresolved would
// break the intake's idle-worker precondition. Returns nil when no A/B
// candidate rows exist (fast path).
func (qh *QueueHandler) computeABFreeze(taskQueues map[string]*taskQueueEntry) map[string]map[string]bool {
	type abRow struct {
		workerID  string
		taskID    string
		commandID string
		groupID   string
	}
	var rows []abRow
	for queueFile, tq := range taskQueues {
		if tq == nil {
			continue
		}
		workerID := workerIDFromPath(queueFile)
		for i := range tq.Queue.Tasks {
			t := &tq.Queue.Tasks[i]
			if t.ABGroupID == "" {
				continue
			}
			rows = append(rows, abRow{workerID: workerID, taskID: t.ID, commandID: t.CommandID, groupID: t.ABGroupID})
		}
	}
	if len(rows) == 0 {
		return nil
	}

	// Resolve group statuses per command (one state read per command).
	resolvedGroups := map[string]bool{} // commandID+"/"+groupID → terminal group
	stateChecked := map[string]bool{}
	for _, r := range rows {
		if stateChecked[r.commandID] {
			continue
		}
		stateChecked[r.commandID] = true
		cs, err := qh.readCommandState(r.commandID)
		if err != nil {
			qh.log(LogLevelDebug, "ab_freeze_state_unreadable command=%s error=%v (keeping rows frozen)", r.commandID, err)
			continue // fail-closed: rows of this command stay frozen
		}
		for gid, g := range cs.CandidateGroups {
			if g != nil && !g.Status.IsUnresolved() {
				resolvedGroups[r.commandID+"/"+gid] = true
			}
		}
	}

	frozen := map[string]map[string]bool{}
	for _, r := range rows {
		if resolvedGroups[r.commandID+"/"+r.groupID] {
			continue // race finished — worker released
		}
		if frozen[r.workerID] == nil {
			frozen[r.workerID] = map[string]bool{}
		}
		frozen[r.workerID][r.taskID] = true
	}
	if len(frozen) == 0 {
		return nil
	}
	return frozen
}
