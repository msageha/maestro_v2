package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// R0bFillingStuck detects phases stuck in "filling" status.
// Action: revert to awaiting_fill, remove partially added tasks.
type R0bFillingStuck struct{}

// stuckPhaseInfo captures information about a stuck filling phase
// collected during the read phase, before queue removal.
type stuckPhaseInfo struct {
	phaseName string
	taskIDs   []string
	ageSec    float64
}

// Apply detects phases stuck in "filling" and reverts them to awaiting_fill.
//
// Uses a three-phase pattern to avoid split-brain when batchRemove fails:
//   Phase 1: read state under lock → collect stuck phases and task IDs
//   Phase 2: remove tasks from worker queues (no state lock held)
//   Phase 3: re-acquire state lock → apply changes → write state
//
// If queue removal fails in phase 2, state is left unchanged to prevent
// tasks lingering in worker queues while state shows awaiting_fill.
func (R0bFillingStuck) Apply(run *Run) Outcome {
	var repairs []Repair
	var notifications []DeferredNotification

	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.cachedReadDir(stateDir)
	if err != nil {
		return Outcome{}
	}

	threshold := run.stuckThresholdSec()

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		statePath := filepath.Join(stateDir, entry.Name())
		lockKey := "state:" + commandID

		// Phase 1: read state under lock, collect stuck phases but don't write.
		var stuckPhases []stuckPhaseInfo
		var allTaskIDsToRemove []string
		run.Deps.LockMap.WithLock(lockKey, func() {
			state, err := run.loadState(statePath)
			if err != nil {
				if !os.IsNotExist(err) {
					run.Log(core.LogLevelError, "R0b load_state_corrupted command=%s file=%s error=%v", commandID, entry.Name(), err)
				}
				return
			}

			for i := range state.Phases {
				phase := &state.Phases[i]
				if phase.Status != model.PhaseStatusFilling {
					continue
				}

				var fillingStarted time.Time
				if phase.FillingStartedAt != nil {
					fillingStarted, err = time.Parse(time.RFC3339, *phase.FillingStartedAt)
					if err != nil {
						continue
					}
				} else {
					fillingStarted, err = time.Parse(time.RFC3339, state.UpdatedAt)
					if err != nil {
						continue
					}
				}

				ageSec := run.Deps.Clock.Now().Sub(fillingStarted).Seconds()
				if ageSec < float64(threshold) {
					continue
				}

				run.Log(core.LogLevelWarn, "R0b filling_stuck command=%s phase=%s age_sec=%.0f",
					state.CommandID, phase.Name, ageSec)

				// Copy task IDs to avoid aliasing the state slice.
				taskIDs := make([]string, len(phase.TaskIDs))
				copy(taskIDs, phase.TaskIDs)

				stuckPhases = append(stuckPhases, stuckPhaseInfo{
					phaseName: phase.Name,
					taskIDs:   taskIDs,
					ageSec:    ageSec,
				})
				allTaskIDsToRemove = append(allTaskIDsToRemove, phase.TaskIDs...)
			}
		})

		if len(stuckPhases) == 0 {
			continue
		}

		// Phase 2: remove tasks from worker queues (no state lock held).
		// If this fails, skip state update to prevent split-brain.
		if len(allTaskIDsToRemove) > 0 {
			if err := run.batchRemoveTaskIDsFromQueues(allTaskIDsToRemove); err != nil {
				run.Log(core.LogLevelError, "R0b batch_remove_tasks command=%s error=%v, skipping state update", commandID, err)
				continue
			}
		}

		// Phase 3: re-acquire state lock, re-read state, apply changes, write.
		var modified bool
		run.Deps.LockMap.WithLock(lockKey, func() {
			state, err := run.loadState(statePath)
			if err != nil {
				run.Log(core.LogLevelError, "R0b reload_state command=%s error=%v", commandID, err)
				return
			}

			// Build lookup of stuck phases by name for matching.
			stuckByName := make(map[string]stuckPhaseInfo, len(stuckPhases))
			for _, sp := range stuckPhases {
				stuckByName[sp.phaseName] = sp
			}

			localModified := false
			var localRepairs []Repair
			for i := range state.Phases {
				phase := &state.Phases[i]
				sp, ok := stuckByName[phase.Name]
				if !ok {
					continue
				}
				// Re-verify: phase may no longer be in filling status.
				if phase.Status != model.PhaseStatusFilling {
					continue
				}

				for _, taskID := range sp.taskIDs {
					delete(state.TaskStates, taskID)
					delete(state.TaskDependencies, taskID)
					state.RequiredTaskIDs = removeFromSlice(state.RequiredTaskIDs, taskID)
					state.OptionalTaskIDs = removeFromSlice(state.OptionalTaskIDs, taskID)
				}
				state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)

				phase.TaskIDs = nil
				phase.Status = model.PhaseStatusAwaitingFill
				localModified = true

				localRepairs = append(localRepairs, Repair{
					Pattern:   PatternR0b,
					CommandID: state.CommandID,
					Detail:    fmt.Sprintf("phase %s filling stuck %.0fs, reverted to awaiting_fill", phase.Name, sp.ageSec),
				})
			}

			if localModified {
				now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
				state.LastReconciledAt = &now
				state.UpdatedAt = now
				if err := yamlutil.AtomicWrite(statePath, state); err != nil {
					run.Log(core.LogLevelError, "R0b write_state command=%s error=%v", state.CommandID, err)
					return
				}
				repairs = append(repairs, localRepairs...)
			}

			modified = localModified
		})

		if modified && run.Deps.ExecutorFactory != nil {
			notifications = append(notifications, DeferredNotification{
				Kind:      NotifyReFill,
				CommandID: commandID,
			})
		}
	}

	return Outcome{Repairs: repairs, Notifications: notifications}
}
