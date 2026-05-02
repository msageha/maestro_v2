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
// Action: delete state file + remove queue entry. Planner will resubmit on next dispatch.
type R0PlanningStuck struct{}

// Apply detects planning commands stuck in "planning" status and removes them
// so the planner can resubmit on next dispatch.
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

		// Phase 2: Queue cleanup (no state lock held).
		var phase2Errs []error
		if err := run.removeCommandFromPlannerQueue(stuckCommandID); err != nil {
			run.Log(core.LogLevelError, "R0 phase2_planner_cleanup command=%s error=%v", stuckCommandID, err)
			phase2Errs = append(phase2Errs, err)
		}
		if err := run.removeTasksFromWorkerQueues(stuckCommandID); err != nil {
			run.Log(core.LogLevelError, "R0 phase2_worker_cleanup command=%s error=%v", stuckCommandID, err)
			phase2Errs = append(phase2Errs, err)
		}
		if len(phase2Errs) > 0 {
			run.Log(core.LogLevelWarn, "R0 cleanup_incomplete command=%s errors=%d; keeping planning state for retry",
				stuckCommandID, len(phase2Errs))
			continue
		}

		// Phase 3: Re-verify and delete state file (under state lock).
		// Between Phase 1 and Phase 3 the state file may have been deleted
		// or modified by another reconciler/handler, so re-check gracefully.
		repaired := true
		run.Deps.LockMap.WithLock(lockKey, func() {
			state, err := run.loadState(statePath)
			if err != nil {
				// State file was deleted or became unreadable between phases.
				run.Log(core.LogLevelInfo, "R0 state_gone_before_delete command=%s error=%v", stuckCommandID, err)
				repaired = false
				return
			}
			if state.PlanStatus != model.PlanStatusPlanning {
				repaired = false
				return
			}

			if err := os.Remove(statePath); err != nil {
				run.Log(core.LogLevelError, "R0 delete_state command=%s error=%v", stuckCommandID, err)
				repaired = false
			}
			// Also drop the .bak sibling so a future commandID reuse cannot
			// be revived by recoverStateDir's ORC-3 epoch floor clamp from a
			// stale prior generation.
			if err := os.Remove(statePath + ".bak"); err != nil && !os.IsNotExist(err) {
				run.Log(core.LogLevelWarn, "R0 delete_state_bak command=%s error=%v", stuckCommandID, err)
			}
		})

		if !repaired {
			continue
		}

		repairs = append(repairs, Repair{
			Pattern:   PatternR0,
			CommandID: stuckCommandID,
			Detail:    fmt.Sprintf("planning stuck %.0fs, state deleted + command removed from queue + worker tasks removed", stuckAgeSec),
		})
	}

	return Outcome{Repairs: repairs}
}
