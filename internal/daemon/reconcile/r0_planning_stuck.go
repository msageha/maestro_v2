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

func (R0PlanningStuck) Name() string { return "R0" }

func (R0PlanningStuck) Apply(run *Run) Outcome {
	var repairs []Repair

	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.CachedReadDir(stateDir)
	if err != nil {
		if !os.IsNotExist(err) {
			run.Log(core.LogLevelWarn, "R0 read_state_dir error=%v", err)
		}
		return Outcome{}
	}

	threshold := run.StuckThresholdSec()

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
		needsRepair := func() bool {
			run.Deps.LockMap.Lock(lockKey)
			defer run.Deps.LockMap.Unlock(lockKey)

			state, err := run.LoadState(statePath)
			if err != nil {
				run.Log(core.LogLevelWarn, "R0 load_state file=%s error=%v", entry.Name(), err)
				return false
			}

			if state.PlanStatus != model.PlanStatusPlanning {
				return false
			}

			createdAt, err := time.Parse(time.RFC3339, state.CreatedAt)
			if err != nil {
				run.Log(core.LogLevelWarn, "R0 parse_created_at command=%s error=%v", state.CommandID, err)
				return false
			}

			ageSec := run.Deps.Clock.Now().Sub(createdAt).Seconds()
			if ageSec < float64(threshold) {
				return false
			}

			run.Log(core.LogLevelWarn, "R0 planning_stuck command=%s age_sec=%.0f threshold=%d",
				state.CommandID, ageSec, threshold)

			stuckCommandID = state.CommandID
			stuckAgeSec = ageSec
			return true
		}()

		if !needsRepair {
			continue
		}

		// Phase 2: Queue cleanup (no state lock held).
		var phase2Errs []error
		if err := run.RemoveCommandFromPlannerQueue(stuckCommandID); err != nil {
			run.Log(core.LogLevelError, "R0 phase2_planner_cleanup command=%s error=%v", stuckCommandID, err)
			phase2Errs = append(phase2Errs, err)
		}
		if err := run.RemoveTasksFromWorkerQueues(stuckCommandID); err != nil {
			run.Log(core.LogLevelError, "R0 phase2_worker_cleanup command=%s error=%v", stuckCommandID, err)
			phase2Errs = append(phase2Errs, err)
		}
		if len(phase2Errs) > 0 {
			run.Log(core.LogLevelWarn, "R0 cleanup_incomplete command=%s errors=%d; keeping planning state for retry",
				stuckCommandID, len(phase2Errs))
			continue
		}

		// Phase 3: Re-verify and delete state file (under state lock).
		repaired := func() bool {
			run.Deps.LockMap.Lock(lockKey)
			defer run.Deps.LockMap.Unlock(lockKey)

			state, err := run.LoadState(statePath)
			if err != nil {
				return true
			}
			if state.PlanStatus != model.PlanStatusPlanning {
				return false
			}

			if err := os.Remove(statePath); err != nil {
				run.Log(core.LogLevelError, "R0 delete_state command=%s error=%v", stuckCommandID, err)
				return false
			}
			return true
		}()

		if !repaired {
			continue
		}

		repairs = append(repairs, Repair{
			Pattern:   "R0",
			CommandID: stuckCommandID,
			Detail:    fmt.Sprintf("planning stuck %.0fs, state deleted + command removed from queue + worker tasks removed", stuckAgeSec),
		})
	}

	return Outcome{Repairs: repairs}
}
