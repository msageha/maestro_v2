package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// R0PlanningStuck detects plan_status: "planning" that has been stuck.
// Action: delete state file + remove queue entry + write a synthetic failed
// planner result so the orchestrator (and thus the user) is told the command
// was torn down instead of it silently vanishing.
type R0PlanningStuck struct{}

// Apply detects planning commands stuck in "planning" status and removes them.
//
// Recovery follows the three-phase queue→state pattern (run.go lock ordering):
// Phase 2 removes only pre-dispatch queue rows and captures everything it
// deletes; every Phase 3 veto (planner progressed, transient errors) re-inserts
// the captured rows so a lost race never leaks queue entries. A confirmed-busy
// Planner defers the teardown up to max_in_progress_min, mirroring R0Dispatch's
// busy probe, so a slow-but-alive first planning round is not destroyed.
func (R0PlanningStuck) Apply(run *Run) Outcome {
	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.cachedReadDir(stateDir)
	if err != nil {
		if !os.IsNotExist(err) {
			run.Log(core.LogLevelWarn, "R0 read_state_dir error=%v", err)
		}
		return Outcome{}
	}

	repairs := make([]Repair, 0, len(entries))
	threshold := run.stuckThresholdSec()

	// Planner-liveness probe (lazy; the Planner is a singleton so probe at
	// most once per Apply). Same rationale as R0Dispatch: a Planner that is
	// alive and still working toward its first plan submit keeps
	// plan_status=planning the whole time, so age alone cannot distinguish
	// "slow but progressing" from "wedged". A confirmed-busy pane defers the
	// teardown up to the busyDeferCap hard cap; idle / undecided / probe
	// unavailable never defer.
	plannerBusyProbed := false
	plannerBusy := false
	busyDeferCap := time.Duration(run.Deps.Config.Watcher.EffectiveMaxInProgressMin()) * time.Minute

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		statePath := filepath.Join(stateDir, entry.Name())
		lockKey := "state:" + commandID

		var stuckCommandID string
		var stuckAgeSec float64

		// Phase 1: Check if state is stuck (under state lock only).
		var needsRepair bool
		run.Deps.LockMap.WithLock(lockKey, func() {
			state, err := run.loadState(statePath)
			if err != nil {
				run.Log(core.LogLevelWarn, "R0 load_state file=%s error=%v", entry.Name(), err)
				return
			}

			if state.PlanStatus != model.PlanStatusPlanning {
				return
			}

			createdAt, err := time.Parse(time.RFC3339, state.CreatedAt)
			if err != nil {
				run.Log(core.LogLevelWarn, "R0 parse_created_at command=%s error=%v", state.CommandID, err)
				return
			}

			ageSec := run.Deps.Clock.Now().Sub(createdAt).Seconds()
			if ageSec < float64(threshold) {
				return
			}

			run.Log(core.LogLevelWarn, "R0 planning_stuck command=%s age_sec=%.0f threshold=%d",
				state.CommandID, ageSec, threshold)

			stuckCommandID = state.CommandID
			stuckAgeSec = ageSec
			needsRepair = true
		})

		if !needsRepair {
			continue
		}

		// Busy-probe deferral, bounded by busyDeferCap so a wedged Planner
		// cannot suspend recovery indefinitely.
		if busyDeferCap > 0 && stuckAgeSec < busyDeferCap.Seconds() {
			if !plannerBusyProbed {
				plannerBusy = plannerPaneActivelyProcessing(run)
				plannerBusyProbed = true
			}
			if plannerBusy {
				run.Log(core.LogLevelInfo,
					"R0 defer_planner_active command=%s age_sec=%.0f cap=%s "+
						"(planner pane busy and within hard cap; deferring planning-stuck teardown)",
					stuckCommandID, stuckAgeSec, busyDeferCap)
				continue
			}
		}

		// Phase 2: Queue cleanup (no state lock held). Only pre-dispatch rows
		// are removed; everything removed is captured for compensation.
		removedCmd, cmdErr := run.removeCommandFromPlannerQueue(stuckCommandID)
		if cmdErr != nil {
			run.Log(core.LogLevelError, "R0 phase2_planner_cleanup command=%s error=%v; keeping planning state for retry",
				stuckCommandID, cmdErr)
			continue
		}
		removedTasks, keptActive, taskErr := run.removeCommandTasksFromWorkerQueues(stuckCommandID)
		if taskErr != nil {
			run.Log(core.LogLevelWarn,
				"R0 cleanup_incomplete command=%s error=%v; restoring removed rows and keeping planning state for retry",
				stuckCommandID, taskErr)
			r0RestoreQueueState(run, removedCmd, removedTasks)
			continue
		}
		if len(keptActive) > 0 {
			// A row advanced past pre-dispatch: the command has live worker
			// activity, so tearing it down would strand a dispatched task.
			run.Log(core.LogLevelWarn,
				"R0 veto_active_tasks command=%s active=%d (dispatched rows exist despite plan_status=planning; restoring removed rows and skipping teardown)",
				stuckCommandID, len(keptActive))
			r0RestoreQueueState(run, removedCmd, removedTasks)
			continue
		}

		// Phase 3: Re-verify and delete state file (under state lock).
		// Between Phase 1 and Phase 3 the state file may have been deleted
		// or modified by another reconciler/handler, so re-check gracefully.
		repaired := true
		restoreQueue := false
		run.Deps.LockMap.WithLock(lockKey, func() {
			state, err := run.loadState(statePath)
			if err != nil {
				if os.IsNotExist(err) {
					// Command finished/cancelled concurrently: the removed rows
					// belong to a dead command, so re-inserting them would only
					// create orphan queue rows. Drop them instead.
					run.Log(core.LogLevelInfo, "R0 state_gone_before_delete command=%s (dropping %d removed pre-dispatch rows)",
						stuckCommandID, len(removedTasks))
					repaired = false
					return
				}
				// Transient read failure: state unchanged on disk but queue
				// rows are gone — restore them so the next scan can retry.
				run.Log(core.LogLevelError, "R0 reload_state command=%s error=%v (restoring removed queue rows)",
					stuckCommandID, err)
				repaired = false
				restoreQueue = true
				return
			}
			if state.PlanStatus != model.PlanStatusPlanning {
				// Planner progressed between phases; the command is live, so
				// re-insert everything Phase 2 deleted.
				run.Log(core.LogLevelInfo,
					"R0 skip_planner_progressed command=%s plan_status=%s (restoring removed queue rows)",
					stuckCommandID, state.PlanStatus)
				repaired = false
				restoreQueue = true
				return
			}

			if err := os.Remove(statePath); err != nil {
				run.Log(core.LogLevelError, "R0 delete_state command=%s error=%v (restoring removed queue rows)",
					stuckCommandID, err)
				repaired = false
				restoreQueue = true
				return
			}
			// Also drop the .bak sibling so a future commandID reuse cannot
			// be revived by recoverStateDir's ORC-3 epoch floor clamp from a
			// stale prior generation.
			if err := os.Remove(statePath + ".bak"); err != nil && !os.IsNotExist(err) {
				run.Log(core.LogLevelWarn, "R0 delete_state_bak command=%s error=%v", stuckCommandID, err)
			}
		})

		if restoreQueue {
			r0RestoreQueueState(run, removedCmd, removedTasks)
		}
		if !repaired {
			continue
		}

		// Surface the teardown to the orchestrator: without a terminal planner
		// result the command silently disappears and the user only observes
		// "終わらない". The synthetic failed result rides the standard
		// result_notification pipeline (lease/retry/dead-letter) to the
		// orchestrator queue.
		writeSyntheticPlannerFailedResult(run, PatternR0, stuckCommandID,
			fmt.Sprintf("synthetic_failure: planning stuck %.0fs — daemon removed the command and its queue entries (R0)", stuckAgeSec))

		repairs = append(repairs, Repair{
			Pattern:   PatternR0,
			CommandID: stuckCommandID,
			Detail:    fmt.Sprintf("planning stuck %.0fs, state deleted + command removed from queue + pre-dispatch worker tasks removed", stuckAgeSec),
		})
	}

	return Outcome{Repairs: repairs}
}

// r0RestoreQueueState re-inserts everything a vetoed R0 repair removed in
// Phase 2: the planner queue command row and the pre-dispatch worker rows.
// Both restores are idempotent.
func r0RestoreQueueState(run *Run, removedCmd *model.Command, removedTasks map[string]removedQueueEntry) {
	restoreAllRemovedEntries(run, removedTasks)
	if removedCmd != nil {
		run.restoreCommandToPlannerQueue(*removedCmd)
	}
}
