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

// R6FillTimeout detects awaiting_fill + fill_deadline_at expired.
// Action: set phase to timed_out, cascade cancel downstream pending phases, defer Planner notification.
type R6FillTimeout struct{}

func (R6FillTimeout) Name() string { return "R6" }

func (R6FillTimeout) Apply(run *Run) Outcome {
	var repairs []Repair
	var notifications []DeferredNotification

	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.CachedReadDir(stateDir)
	if err != nil {
		return Outcome{}
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		statePath := filepath.Join(stateDir, entry.Name())

		lockKey := "state:" + commandID
		writeOK, commandRepairs, timedOutPhases := func() (bool, []Repair, map[string]bool) {
			run.Deps.LockMap.Lock(lockKey)
			defer run.Deps.LockMap.Unlock(lockKey)

			state, err := run.LoadState(statePath)
			if err != nil {
				if !os.IsNotExist(err) {
					run.Log(core.LogLevelError, "R6 load_state_corrupted command=%s file=%s error=%v", commandID, entry.Name(), err)
				}
				return false, nil, nil
			}

			if len(state.Phases) == 0 {
				return false, nil, nil
			}

			modified := false
			timedOutPhases := make(map[string]bool)
			var commandRepairs []Repair

			for i := range state.Phases {
				phase := &state.Phases[i]
				if phase.Status != model.PhaseStatusAwaitingFill {
					continue
				}
				if phase.FillDeadlineAt == nil {
					continue
				}

				deadline, err := time.Parse(time.RFC3339, *phase.FillDeadlineAt)
				if err != nil {
					continue
				}

				if run.Deps.Clock.Now().UTC().Before(deadline) {
					continue
				}

				run.Log(core.LogLevelWarn, "R6 awaiting_fill_deadline_expired command=%s phase=%s deadline=%s",
					commandID, phase.Name, *phase.FillDeadlineAt)

				phase.Status = model.PhaseStatusTimedOut
				modified = true
				timedOutPhases[phase.Name] = true

				commandRepairs = append(commandRepairs, Repair{
					Pattern:   "R6",
					CommandID: commandID,
					Detail:    fmt.Sprintf("phase %s timed_out (deadline %s)", phase.Name, *phase.FillDeadlineAt),
				})
			}

			// Cascade cancel with cycle detection (M-09)
			if len(timedOutPhases) > 0 {
				cancelledPhases := make(map[string]bool)
				for name := range timedOutPhases {
					cancelledPhases[name] = true
				}

				// Cap iterations at len(Phases) to prevent infinite loops from cyclic dependencies
				maxIter := len(state.Phases)
				for iter := 0; iter < maxIter; iter++ {
					changed := false
					for i := range state.Phases {
						phase := &state.Phases[i]
						if cancelledPhases[phase.Name] {
							continue // already cancelled, skip
						}
						if phase.Status != model.PhaseStatusPending && phase.Status != model.PhaseStatusAwaitingFill {
							continue
						}
						for _, dep := range phase.DependsOnPhases {
							if cancelledPhases[dep] {
								run.Log(core.LogLevelWarn, "R6 cascade_cancel command=%s phase=%s (depends on %s)",
									commandID, phase.Name, dep)
								phase.Status = model.PhaseStatusCancelled
								modified = true
								changed = true
								cancelledPhases[phase.Name] = true

								commandRepairs = append(commandRepairs, Repair{
									Pattern:   "R6",
									CommandID: commandID,
									Detail:    fmt.Sprintf("phase %s cancelled (cascade from %s)", phase.Name, dep),
								})
								break
							}
						}
					}
					if !changed {
						break
					}
				}
			}

			if modified {
				now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
				state.LastReconciledAt = &now
				state.UpdatedAt = now
				if err := yamlutil.AtomicWrite(statePath, state); err != nil {
					run.Log(core.LogLevelError, "R6 write_state command=%s error=%v", commandID, err)
					return false, commandRepairs, timedOutPhases
				}
				return true, commandRepairs, timedOutPhases
			}

			return false, nil, nil
		}()

		if writeOK {
			repairs = append(repairs, commandRepairs...)
			if run.Deps.ExecutorFactory != nil {
				notifications = append(notifications, DeferredNotification{
					Kind:           "fill_timeout",
					CommandID:      commandID,
					TimedOutPhases: timedOutPhases,
				})
			}
		}
	}

	return Outcome{Repairs: repairs, Notifications: notifications}
}
