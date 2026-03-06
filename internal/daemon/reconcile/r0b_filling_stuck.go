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

func (R0bFillingStuck) Name() string { return "R0b" }

func (R0bFillingStuck) Apply(run *Run) Outcome {
	var repairs []Repair
	var notifications []DeferredNotification

	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.CachedReadDir(stateDir)
	if err != nil {
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

		var taskIDsToRemove []string
		modified := func() bool {
			run.Deps.LockMap.Lock(lockKey)
			defer run.Deps.LockMap.Unlock(lockKey)

			state, err := run.LoadState(statePath)
			if err != nil {
				if !os.IsNotExist(err) {
					run.Log(core.LogLevelError, "R0b load_state_corrupted command=%s file=%s error=%v", commandID, entry.Name(), err)
				}
				return false
			}

			localModified := false
			var localRepairs []Repair
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

				taskIDsToRemove = append(taskIDsToRemove, phase.TaskIDs...)

				for _, taskID := range phase.TaskIDs {
					delete(state.TaskStates, taskID)
					delete(state.TaskDependencies, taskID)
					state.RequiredTaskIDs = RemoveFromSlice(state.RequiredTaskIDs, taskID)
					state.OptionalTaskIDs = RemoveFromSlice(state.OptionalTaskIDs, taskID)
				}
				state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)

				phase.TaskIDs = nil
				phase.Status = model.PhaseStatusAwaitingFill
				localModified = true

				localRepairs = append(localRepairs, Repair{
					Pattern:   "R0b",
					CommandID: state.CommandID,
					Detail:    fmt.Sprintf("phase %s filling stuck %.0fs, reverted to awaiting_fill", phase.Name, ageSec),
				})
			}

			if localModified {
				now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
				state.LastReconciledAt = &now
				state.UpdatedAt = now
				if err := yamlutil.AtomicWrite(statePath, state); err != nil {
					run.Log(core.LogLevelError, "R0b write_state command=%s error=%v", state.CommandID, err)
					return false
				}
				repairs = append(repairs, localRepairs...)
			}

			return localModified
		}()

		if len(taskIDsToRemove) > 0 {
			if err := run.BatchRemoveTaskIDsFromQueues(taskIDsToRemove); err != nil {
				run.Log(core.LogLevelError, "R0b batch_remove_tasks command=%s error=%v", commandID, err)
			}
		}

		if modified && run.Deps.ExecutorFactory != nil {
			notifications = append(notifications, DeferredNotification{
				Kind:      "re_fill",
				CommandID: commandID,
			})
		}
	}

	return Outcome{Repairs: repairs, Notifications: notifications}
}
