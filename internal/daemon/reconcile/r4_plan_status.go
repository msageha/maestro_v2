package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// R4PlanStatus detects results/planner terminal + state non-terminal plan_status.
// Action: re-evaluate via plan.CanComplete. If OK, update plan_status. If NG, quarantine result.
type R4PlanStatus struct{}

func (R4PlanStatus) Name() string { return "R4" }

func (R4PlanStatus) Apply(run *Run) Outcome {
	var repairs []Repair
	var notifications []DeferredNotification

	resultPath := filepath.Join(run.Deps.MaestroDir, "results", "planner.yaml")
	rf, err := run.LoadCommandResultFile(resultPath)
	if err != nil {
		return Outcome{}
	}

	type r4Outcome struct {
		repair       *Repair
		notification *DeferredNotification
		quarantine   bool
	}

	for _, result := range rf.Results {
		if !model.IsTerminal(result.Status) {
			continue
		}

		commandID := result.CommandID
		statePath := filepath.Join(run.Deps.MaestroDir, "state", "commands", commandID+".yaml")

		lockKey := "state:" + commandID
		outcome := func() r4Outcome {
			run.Deps.LockMap.Lock(lockKey)
			defer run.Deps.LockMap.Unlock(lockKey)

			state, err := run.LoadState(statePath)
			if err != nil {
				if !os.IsNotExist(err) {
					run.Log(core.LogLevelError, "R4 load_state_corrupted command=%s error=%v", commandID, err)
				}
				return r4Outcome{}
			}

			if model.IsPlanTerminal(state.PlanStatus) {
				return r4Outcome{}
			}

			if state.PlanStatus == model.PlanStatusPlanning {
				return r4Outcome{}
			}

			run.Log(core.LogLevelWarn, "R4 result_terminal_state_nonterminal command=%s result_status=%s plan_status=%s",
				commandID, result.Status, state.PlanStatus)

			if run.Deps.CanComplete == nil {
				run.Log(core.LogLevelWarn, "R4 skipped command=%s (canComplete not wired)", commandID)
				return r4Outcome{}
			}

			derivedStatus, canCompleteErr := run.Deps.CanComplete(state)
			if canCompleteErr != nil {
				run.Log(core.LogLevelWarn, "R4 can_complete_failed command=%s error=%v → quarantine result + notify planner",
					commandID, canCompleteErr)
				return r4Outcome{
					quarantine: true,
					repair: &Repair{
						Pattern:   "R4",
						CommandID: commandID,
						Detail:    fmt.Sprintf("can_complete failed (%v), result quarantined, planner notification deferred", canCompleteErr),
					},
					notification: &DeferredNotification{
						Kind:      "re_evaluate",
						CommandID: commandID,
						Reason:    canCompleteErr.Error(),
					},
				}
			}

			state.PlanStatus = derivedStatus
			now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
			state.LastReconciledAt = &now
			state.UpdatedAt = now
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				run.Log(core.LogLevelError, "R4 write_state command=%s error=%v", commandID, err)
				return r4Outcome{}
			}

			return r4Outcome{
				repair: &Repair{
					Pattern:   "R4",
					CommandID: commandID,
					Detail:    fmt.Sprintf("plan_status updated to %s via can_complete", derivedStatus),
				},
			}
		}()

		if outcome.quarantine {
			if err := run.QuarantineCommandResult(resultPath, result); err != nil {
				run.Log(core.LogLevelError, "R4 quarantine command=%s error=%v", commandID, err)
			}
		}
		if outcome.repair != nil {
			repairs = append(repairs, *outcome.repair)
		}
		if outcome.notification != nil {
			notifications = append(notifications, *outcome.notification)
		}
	}

	return Outcome{Repairs: repairs, Notifications: notifications}
}
