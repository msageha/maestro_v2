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

// maxConflictResolutionAttempts is the maximum number of conflict resolution
// attempts before escalating to the Planner.
const maxConflictResolutionAttempts = 2

// R7MergeConflict detects workers stuck in conflict status within worktree
// state and triggers conflict resolution or escalation.
//
// Action:
//   - ConflictResolutionAttempts < 2: transition worker to resolving, increment
//     attempts, and emit NotifyConflictResolution for Planner to generate a
//     __conflict_resolution task.
//   - ConflictResolutionAttempts >= 2: emit NotifyConflictEscalation for Planner
//     to handle the unresolvable conflict.
type R7MergeConflict struct{}

// Apply scans worktree state files for commands with IntegrationStatusConflict,
// finds workers in WorktreeStatusConflict, and triggers resolution or escalation.
func (R7MergeConflict) Apply(run *Run) Outcome {
	var repairs []Repair
	var notifications []DeferredNotification

	worktreeDir := filepath.Join(run.Deps.MaestroDir, "state", "worktrees")
	entries, err := run.cachedReadDir(worktreeDir)
	if err != nil {
		return Outcome{}
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		statePath := filepath.Join(worktreeDir, entry.Name())

		lockKey := "worktree:" + commandID
		r, n := func() ([]Repair, []DeferredNotification) {
			run.Deps.LockMap.Lock(lockKey)
			defer run.Deps.LockMap.Unlock(lockKey)

			state, err := run.loadWorktreeState(statePath)
			if err != nil {
				if !os.IsNotExist(err) {
					run.log(core.LogLevelError, "R7 load_worktree_state command=%s error=%v", commandID, err)
				}
				return nil, nil
			}

			if state.Integration.Status != model.IntegrationStatusConflict {
				return nil, nil
			}

			var commandRepairs []Repair
			var commandNotifications []DeferredNotification
			modified := false

			for i := range state.Workers {
				ws := &state.Workers[i]
				if ws.Status != model.WorktreeStatusConflict {
					continue
				}

				if ws.ConflictResolutionAttempts >= maxConflictResolutionAttempts {
					run.log(core.LogLevelWarn, "R7 conflict_escalation command=%s worker=%s attempts=%d",
						commandID, ws.WorkerID, ws.ConflictResolutionAttempts)
					commandNotifications = append(commandNotifications, DeferredNotification{
						Kind:      NotifyConflictEscalation,
						CommandID: commandID,
						WorkerID:  ws.WorkerID,
						Reason:    fmt.Sprintf("conflict resolution exceeded %d attempts", maxConflictResolutionAttempts),
					})
					commandRepairs = append(commandRepairs, Repair{
						Pattern:   PatternR7,
						CommandID: commandID,
						Detail:    fmt.Sprintf("worker %s conflict escalated (attempts=%d)", ws.WorkerID, ws.ConflictResolutionAttempts),
					})
					continue
				}

				run.log(core.LogLevelInfo, "R7 conflict_resolution command=%s worker=%s attempt=%d",
					commandID, ws.WorkerID, ws.ConflictResolutionAttempts+1)
				ws.ConflictResolutionAttempts++
				ws.Status = model.WorktreeStatusResolving
				now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
				ws.UpdatedAt = now
				modified = true

				commandNotifications = append(commandNotifications, DeferredNotification{
					Kind:      NotifyConflictResolution,
					CommandID: commandID,
					WorkerID:  ws.WorkerID,
				})
				commandRepairs = append(commandRepairs, Repair{
					Pattern:   PatternR7,
					CommandID: commandID,
					Detail:    fmt.Sprintf("worker %s conflict resolution dispatched (attempt=%d)", ws.WorkerID, ws.ConflictResolutionAttempts),
				})
			}

			if modified {
				now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
				state.UpdatedAt = now
				if err := yamlutil.AtomicWrite(statePath, state); err != nil {
					run.log(core.LogLevelError, "R7 write_worktree_state command=%s error=%v", commandID, err)
					return commandRepairs, nil
				}
			}

			return commandRepairs, commandNotifications
		}()

		repairs = append(repairs, r...)
		notifications = append(notifications, n...)
	}

	return Outcome{Repairs: repairs, Notifications: notifications}
}
