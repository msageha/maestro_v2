package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// R10PausedForReplanDeadletter escalates tasks that have been parked in
// paused_for_replan beyond the configured deadletter window (see
// VerifyDaemonConfig.PausedForReplanDeadletterSec; the default lives in
// model.DefaultPausedForReplanDeadletterSec, currently 1 hour, balancing
// Planner LLM deliberation time against the queue-occupancy cost of
// leaving a paused command parked for too long). Without this rule, a
// Planner that misclassifies the paused_for_replan signal as stale leaves
// the publish gate blocked indefinitely — at least one phase remains in
// a non-terminal state owned by an unresponsive Planner.
//
// Once the threshold elapses, R10 advances the task's TaskStates entry from
// paused_for_replan to failed and overwrites the queue task status to failed
// as well. The phase containing the task transitions to PhaseStatusFailed via
// the dependency resolver on the next scan, which lets the publish gate (and
// the operator-facing dashboard) reach a terminal decision instead of
// spinning forever.
//
// R10 is intentionally conservative:
//   - 0 threshold disables the rule entirely (operator opt-out);
//   - elapsed time is measured per task against the task's most recent worker
//     result (falling back to state.UpdatedAt when no result exists) — see
//     r10ResolveTaskStaleAnchor for why the command-level UpdatedAt anchor
//     could defer escalation indefinitely;
//   - the daemon emits a deadletter log so the operator sees the
//     escalation in the same channel as R7/R8/R9 deadletters.
type R10PausedForReplanDeadletter struct{}

// Apply scans every command state file, identifies paused_for_replan tasks
// whose per-task stale anchor is older than the configured threshold, and
// rewrites their TaskStates entry to failed. Sibling daemon plumbing (the
// dependency resolver, queue reconciler, and publish gate) reacts on the next
// scan.
func (R10PausedForReplanDeadletter) Apply(run *Run) Outcome {
	thresholdSec := run.Deps.Config.Verify.EffectivePausedForReplanDeadletterSec()
	if thresholdSec <= 0 {
		return Outcome{} // disabled
	}
	threshold := time.Duration(thresholdSec) * time.Second

	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.cachedReadDir(stateDir)
	if err != nil {
		return Outcome{}
	}

	resultTimestamps := r9LoadResultTimestamps(run)

	now := run.Deps.Clock.Now().UTC()
	var repairs []Repair

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(name, ".yaml")
		statePath := filepath.Join(stateDir, name)
		commandRepairs := r10ApplyForCommand(run, statePath, commandID, resultTimestamps[commandID], now, threshold)
		repairs = append(repairs, commandRepairs...)
	}

	return Outcome{Repairs: repairs}
}

// r10Candidate carries enough state from Phase 1 (read under state lock) to
// drive Phase 2 (queue updates under queue lock) and Phase 3 (state write
// under re-acquired state lock) without holding both locks at once.
type r10Candidate struct {
	taskID string
	age    time.Duration
}

// r10ApplyForCommand escalates paused_for_replan tasks whose stale window
// has elapsed. It runs in three phases so the reconcile package's
// "state and queue locks MUST NOT be held simultaneously" rule (run.go) is
// honoured even at internal call boundaries:
//
//  1. Read-only Phase 1 under the state lock: identify candidates whose
//     per-task stale anchor is older than the threshold.
//  2. Per-candidate queue work under each queue lock (state lock released):
//     find the owning worker (transient I/O errors propagate so we do NOT
//     advance state for them) and overwrite the queue task to failed.
//  3. Phase 3 reacquires the state lock, re-verifies that each successful
//     candidate is still at paused_for_replan, and writes state to failed.
//
// The freshness check uses the task's most recent worker result timestamp
// (r10ResolveTaskStaleAnchor) with state.UpdatedAt as the fallback; if no
// parseable anchor exists the task is logged and skipped. Two-phase
// queue→state ordering means failures in Phase 2 leave the state at
// paused_for_replan so the next scan retries (r10MarkQueueTaskFailed is
// idempotent on already-failed queue rows).
func r10ApplyForCommand(run *Run, statePath, commandID string, resultsForCommand map[string]time.Time, now time.Time, threshold time.Duration) []Repair {
	// Phase 1: identify candidates under state lock (no queue locks here).
	var candidates []r10Candidate
	stateLoaded := false
	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			if !os.IsNotExist(err) {
				run.Log(core.LogLevelError, "R10 load_state command=%s error=%v", commandID, err)
			}
			return
		}
		stateLoaded = true
		if state.TaskStates == nil {
			return
		}
		for taskID, status := range state.TaskStates {
			if status != model.StatusPausedForReplan {
				continue
			}
			startedAt, anchorOk := r10ResolveTaskStaleAnchor(state, taskID, resultsForCommand)
			if !anchorOk {
				run.Log(core.LogLevelDebug,
					"R10 skip command=%s task=%s reason=stale_anchor_unparseable updated_at=%q "+
						"(candidate not evaluated; operator inspection may be required)",
					commandID, taskID, state.UpdatedAt)
				continue
			}
			age := now.Sub(startedAt)
			if age < threshold {
				continue
			}
			candidates = append(candidates, r10Candidate{taskID: taskID, age: age})
		}
	})
	if !stateLoaded || len(candidates) == 0 {
		return nil
	}

	// Phase 2: per-candidate queue work, state lock released. Each step
	// can fail independently — failed queue updates are dropped from the
	// escalation set so Phase 3 leaves their state at paused_for_replan
	// for the next scan to retry.
	type prepared struct {
		taskID     string
		age        time.Duration
		workerID   string
		prevStatus model.Status
	}
	var ready []prepared
	for _, c := range candidates {
		// Pre-write re-check (lock-free atomic state read): the Planner may
		// have re-activated the task (paused_for_replan → ready is a legal
		// markTaskReady transition) between the Phase 1 snapshot and now.
		// Writing queue=failed first and only then noticing the state change
		// in Phase 3 left a state=ready / queue=failed split: terminal queue
		// rows are never re-dispatched, so the revived task hung forever.
		// The residual window after this check is micro-seconds.
		//
		// A daemon crash between this queue write and the Phase 3 state
		// write is NOT the same hazard: the leftover queue row is terminal
		// (failed), so the dispatch loop (SortPendingIndices filters on
		// StatusPending) can never fire markTaskReady for it again — state
		// stays paused_for_replan, the next R10 scan re-selects the task,
		// r10MarkQueueTaskFailed treats the already-terminal row as
		// idempotent success, and Phase 3 completes the escalation. The
		// crash window self-heals; only the within-scan race needs the
		// compensation below. (Audit 2026-06-12, confirmed with 2nd review.)
		if st, err := run.loadState(statePath); err == nil {
			if st.TaskStates[c.taskID] != model.StatusPausedForReplan {
				run.Log(core.LogLevelInfo,
					"R10 skip_task_left_paused command=%s task=%s status=%s "+
						"(Planner acted between snapshot and queue write)",
					commandID, c.taskID, st.TaskStates[c.taskID])
				continue
			}
		}
		workerID, findErr := r10FindOwningWorker(run, commandID, c.taskID)
		if findErr != nil {
			run.Log(core.LogLevelWarn,
				"R10 find_owning_worker_failed command=%s task=%s error=%v "+
					"(state left at paused_for_replan so the next scan can retry)",
				commandID, c.taskID, findErr)
			continue
		}
		var prevStatus model.Status
		if workerID != "" {
			var qErr error
			prevStatus, qErr = r10MarkQueueTaskFailed(run, workerID, c.taskID)
			if qErr != nil {
				run.Log(core.LogLevelWarn,
					"R10 queue_update_failed command=%s task=%s worker=%s error=%v "+
						"(state left at paused_for_replan so the next scan can retry)",
					commandID, c.taskID, workerID, qErr)
				continue
			}
		}
		ready = append(ready, prepared{taskID: c.taskID, age: c.age, workerID: workerID, prevStatus: prevStatus})
	}
	if len(ready) == 0 {
		return nil
	}

	// Phase 3: reacquire state lock, re-verify, and write. Re-verification
	// guards against a parallel state mutation between Phase 1 and Phase 3
	// (e.g., the Planner finally acted, transitioning the task off
	// paused_for_replan). We never overwrite a non-paused_for_replan
	// status — better to skip than to clobber a legitimate transition.
	var commandRepairs []Repair
	var queueRestores []prepared
	requiredTaskEscalated := false
	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			run.Log(core.LogLevelError,
				"R10 reload_state command=%s error=%v "+
					"(queue tasks already failed; next scan will replay state escalation)",
				commandID, err)
			return
		}
		if state.TaskStates == nil {
			return
		}
		requiredSet := make(map[string]bool, len(state.RequiredTaskIDs))
		for _, tid := range state.RequiredTaskIDs {
			requiredSet[tid] = true
		}
		modified := false
		nowStr := now.Format(time.RFC3339)
		for _, p := range ready {
			if state.TaskStates[p.taskID] != model.StatusPausedForReplan {
				run.Log(core.LogLevelDebug,
					"R10 skip_terminal_write command=%s task=%s reason=status_changed_during_queue_update "+
						"(another path advanced the state; honouring it and restoring the queue entry)",
					commandID, p.taskID)
				if p.workerID != "" && p.prevStatus != "" {
					// Compensate after releasing the state lock (queue locks
					// must never be taken while holding state — canonical
					// order is queue → state).
					queueRestores = append(queueRestores, p)
				}
				continue
			}
			run.Log(core.LogLevelWarn,
				"R10 paused_for_replan_deadletter command=%s task=%s age=%s threshold=%s -> failed "+
					"(planner did not act within deadletter window — escalating to terminal failure)",
				commandID, p.taskID, p.age.Round(time.Second), threshold)
			state.TaskStates[p.taskID] = model.StatusFailed
			modified = true
			if requiredSet[p.taskID] {
				requiredTaskEscalated = true
			}
			commandRepairs = append(commandRepairs, Repair{
				Pattern:   PatternR10,
				CommandID: commandID,
				TaskID:    p.taskID,
				Detail:    fmt.Sprintf("paused_for_replan stalled for %s (>%s); escalated to failed (planner_unresponsive)", p.age.Round(time.Second), threshold),
			})
		}
		if modified {
			state.LastReconciledAt = &nowStr
			state.UpdatedAt = nowStr
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				run.Log(core.LogLevelError,
					"R10 write_state command=%s error=%v "+
						"(queue tasks already failed; next scan will replay state escalation)",
					commandID, err)
			}
		}
	})

	// Compensation (post Phase 3, state lock released): restore queue
	// entries whose state re-check showed the Planner revived the task.
	for _, p := range queueRestores {
		r10RestoreQueueTaskStatus(run, p.workerID, p.taskID, p.prevStatus)
	}

	// Phase 4: when at least one *required* task was escalated, publish a
	// synthetic planner result with status=failed so R3PlannerQueue picks
	// up the terminal-result/in_progress-queue mismatch and walks the
	// planner queue command to failed on the next scan, and R4PlanStatus
	// reconciles state.PlanStatus accordingly. Without this step, a
	// command whose Planner stops responding to paused_for_replan stays
	// in_progress on the planner queue until something else pokes it
	// (operator / Planner restart) — the user-observed "終わらない" pattern.
	//
	// Optional-only escalations skip this step because the command may
	// still complete via the remaining required tasks; CompletionPolicy
	// keeps optional failures from forcing a command-wide failure unless
	// the operator opted in.
	if requiredTaskEscalated {
		writeSyntheticPlannerFailedResult(run, PatternR10, commandID,
			"synthetic_failure: paused_for_replan deadletter — planner did not act within configured window (R10)")
	}

	return commandRepairs
}

// r10ResolveTaskStaleAnchor extracts the timestamp R10 uses to measure "how
// long has this task been in paused_for_replan?" for a single task. The
// task's most recent worker result is preferred: it is written before the
// pause transition and never moves when unrelated tasks touch the state
// file, so escalation cannot be deferred indefinitely by sibling reconcile
// writes (the previous command-level state.UpdatedAt anchor reset on every
// write, letting an active sibling task postpone the deadletter forever).
// The result timestamp predates the actual pause by the verify/repair
// window, so escalation fires slightly early rather than late.
// state.UpdatedAt remains the fallback for tasks with no result on disk.
// Returns (zero, false) when no parseable anchor exists.
func r10ResolveTaskStaleAnchor(state *model.CommandState, taskID string, resultsForCommand map[string]time.Time) (time.Time, bool) {
	if t, ok := resultsForCommand[taskID]; ok {
		return t.UTC(), true
	}
	if state.UpdatedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, state.UpdatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// r10FindOwningWorker locates the worker queue file that holds taskID for
// commandID. Returns ("", nil) when the task definitively has no queue
// entry (file missing, task absent, or all worker queue files scanned
// successfully without a match); the caller advances state safely in that
// case.
//
// Returns ("", err) for transient I/O failures: the caller MUST treat
// these as "do not advance state, retry next scan" so a transient
// permission denied or unmarshal error does not silently leave the queue
// non-terminal while state is escalated to failed (publish gate
// invariant).
func r10FindOwningWorker(run *Run, commandID, taskID string) (string, error) {
	queueDir := queueDirPath(run.Deps.MaestroDir)
	entries, err := run.cachedReadDir(queueDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read queue dir %s: %w", queueDir, err)
	}
	for _, entry := range entries {
		workerID := extractWorkerID(entry.Name())
		if workerID == "" {
			continue
		}
		queuePath := filepath.Join(queueDir, entry.Name())
		var (
			found    bool
			queueErr error
		)
		run.Deps.LockMap.WithLock("queue:"+workerID, func() {
			data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is in the controlled queue directory
			if err != nil {
				if !os.IsNotExist(err) {
					queueErr = fmt.Errorf("read %s: %w", queuePath, err)
				}
				return
			}
			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(data, &tq); err != nil {
				queueErr = fmt.Errorf("unmarshal %s: %w", queuePath, err)
				return
			}
			for _, t := range tq.Tasks {
				if t.CommandID == commandID && t.ID == taskID {
					found = true
					return
				}
			}
		})
		if queueErr != nil {
			return "", queueErr
		}
		if found {
			return workerID, nil
		}
	}
	return "", nil
}

// r10MarkQueueTaskFailed sets the queue task's status to failed so command-
// level scans (publish gate, dashboard) see a consistent terminal status with
// the state file. Idempotent: a queue task already terminal is left alone
// (returns nil without rewriting). Returns an error only when the queue file
// could not be read, parsed, or atomically written; the caller uses the
// non-nil error as a signal to leave state at paused_for_replan so the next
// R10 scan can retry the whole transition.
// r10MarkQueueTaskFailed flips the queue entry to failed and returns the
// status it had before the overwrite (empty when the entry was missing,
// already terminal, or the write failed) so the caller can compensate —
// restore the previous status — when Phase 3 discovers the Planner revived
// the task in the meantime.
func r10MarkQueueTaskFailed(run *Run, workerID, taskID string) (model.Status, error) {
	queuePath := taskQueuePath(run.Deps.MaestroDir, workerID)
	var retErr error
	var prevStatus model.Status
	run.Deps.LockMap.WithLock("queue:"+workerID, func() {
		data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is in the controlled queue directory
		if err != nil {
			if os.IsNotExist(err) {
				// Queue file already gone — treat as success (nothing
				// to escalate; state can flip to failed safely).
				return
			}
			retErr = fmt.Errorf("read queue: %w", err)
			return
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			retErr = fmt.Errorf("unmarshal queue: %w", err)
			return
		}
		modified := false
		nowStr := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
		for i := range tq.Tasks {
			t := &tq.Tasks[i]
			if t.ID != taskID {
				continue
			}
			if model.IsTerminal(t.Status) {
				return // already terminal — idempotent success
			}
			prevStatus = t.Status
			t.Status = model.StatusFailed
			t.UpdatedAt = nowStr
			modified = true
		}
		if !modified {
			return // task not found in queue — treat as success
		}
		if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
			prevStatus = ""
			retErr = fmt.Errorf("write queue: %w", err)
		}
	})
	return prevStatus, retErr
}

// r10RestoreQueueTaskStatus undoes r10MarkQueueTaskFailed when Phase 3
// found the state no longer at paused_for_replan (the Planner revived the
// task between the queue write and the state re-check). Only restores when
// the entry is still at the failed status R10 wrote — a CAS so concurrent
// legitimate writes are never clobbered. Without this compensation the
// task was left state=ready / queue=failed: terminal queue rows are never
// re-dispatched, so the revived task hung forever.
func r10RestoreQueueTaskStatus(run *Run, workerID, taskID string, prevStatus model.Status) {
	queuePath := taskQueuePath(run.Deps.MaestroDir, workerID)
	run.Deps.LockMap.WithLock("queue:"+workerID, func() {
		data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is in the controlled queue directory
		if err != nil {
			run.Log(core.LogLevelWarn, "R10 queue_restore_read_failed worker=%s task=%s error=%v", workerID, taskID, err)
			return
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			run.Log(core.LogLevelWarn, "R10 queue_restore_parse_failed worker=%s task=%s error=%v", workerID, taskID, err)
			return
		}
		for i := range tq.Tasks {
			t := &tq.Tasks[i]
			if t.ID != taskID {
				continue
			}
			if t.Status != model.StatusFailed {
				return // someone else wrote since — honour it
			}
			t.Status = prevStatus
			t.UpdatedAt = run.Deps.Clock.Now().UTC().Format(time.RFC3339)
			if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
				run.Log(core.LogLevelWarn, "R10 queue_restore_write_failed worker=%s task=%s error=%v", workerID, taskID, err)
				return
			}
			run.Log(core.LogLevelInfo,
				"R10 queue_status_restored worker=%s task=%s status=%s (state left paused_for_replan during queue update)",
				workerID, taskID, prevStatus)
			return
		}
	})
}
