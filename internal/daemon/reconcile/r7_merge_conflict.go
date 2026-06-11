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

// resolvingStallTimeout is the minimum duration a worker must spend in
// resolving status before R7 resets it back to conflict. This handles cases
// where the conflict-resolution task fails but the Planner does not call
// resume-merge, leaving the worker stuck in resolving indefinitely.
// R7 only processes conflict workers (not resolving), so without this reset
// the command would be permanently stuck.
//
// Set to 20 minutes — longer than a typical task dispatch round-trip (~5 min)
// but shorter than the default task timeout (30 min), so the reset fires before
// the hard timeout would clean up the entire command.
const resolvingStallTimeout = 20 * time.Minute

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

		var r []Repair
		var n []DeferredNotification
		run.Deps.LockMap.WithLock("worktree:"+commandID, func() {
			state, err := run.loadWorktreeState(statePath)
			if err != nil {
				if !os.IsNotExist(err) {
					run.Log(core.LogLevelError, "R7 load_worktree_state command=%s error=%v", commandID, err)
				}
				return
			}

			if state.Integration.Status != model.IntegrationStatusConflict &&
				state.Integration.Status != model.IntegrationStatusPartialMerge {
				return
			}

			var commandRepairs []Repair
			var commandNotifications []DeferredNotification
			modified := false

			now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)

			// Pass 1: Reset stale resolving workers back to conflict.
			// When the conflict-resolution task fails without the Planner calling
			// resume-merge (e.g., due to worker policy violations), the worker
			// gets stuck in resolving state indefinitely. R7 normally only
			// processes conflict workers, so without this reset the command
			// cannot make progress. Resetting to conflict allows the next R7
			// cycle to re-dispatch a resolution task or escalate.
			for i := range state.Workers {
				ws := &state.Workers[i]
				if ws.Status != model.WorktreeStatusResolving {
					continue
				}
				updatedAt, err := time.Parse(time.RFC3339, ws.UpdatedAt)
				if err != nil {
					continue
				}
				if run.Deps.Clock.Now().Sub(updatedAt) < resolvingStallTimeout {
					continue
				}
				run.Log(core.LogLevelWarn, "R7 reset_stale_resolving command=%s worker=%s stale_since=%s",
					commandID, ws.WorkerID, ws.UpdatedAt)
				ws.Status = model.WorktreeStatusConflict
				ws.UpdatedAt = now
				modified = true
			}

			// Pass 2: Process conflict workers (dispatch resolution task or escalate).
			for i := range state.Workers {
				ws := &state.Workers[i]
				if ws.Status != model.WorktreeStatusConflict {
					continue
				}

				if ws.ConflictResolutionAttempts >= maxConflictResolutionAttempts {
					// One-shot guard: the conflict is unrecoverable and the
					// worker status stays `conflict` until an operator/Planner
					// recovery action changes it. Without this guard the
					// escalation notification + repair re-fired on every
					// reconcile scan (the branch sets no state), spamming the
					// Planner pane and inflating repair counts. Emit exactly
					// once per escalation, matching R8's StallSignaled pattern.
					// The guard is cleared by setWorkerStatus when the worker
					// re-enters conflict from a clean state (a fresh episode in
					// a later phase), so a genuinely new conflict re-escalates.
					if ws.ConflictEscalated {
						run.Log(core.LogLevelDebug,
							"R7 conflict_escalation_already_signaled command=%s worker=%s attempts=%d",
							commandID, ws.WorkerID, ws.ConflictResolutionAttempts)
						continue
					}
					run.Log(core.LogLevelWarn, "R7 conflict_escalation command=%s worker=%s attempts=%d",
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
					ws.ConflictEscalated = true
					modified = true
					continue
				}

				run.Log(core.LogLevelInfo, "R7 conflict_resolution command=%s worker=%s attempt=%d",
					commandID, ws.WorkerID, ws.ConflictResolutionAttempts+1)
				if err := model.ValidateWorktreeTransition(ws.Status, model.WorktreeStatusResolving); err != nil {
					run.Log(core.LogLevelError, "R7 invalid_worktree_transition command=%s worker=%s from=%s to=%s error=%v",
						commandID, ws.WorkerID, ws.Status, model.WorktreeStatusResolving, err)
					continue
				}
				ws.ConflictResolutionAttempts++
				ws.Status = model.WorktreeStatusResolving
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
				state.UpdatedAt = now
				if err := yamlutil.AtomicWrite(statePath, state); err != nil {
					run.Log(core.LogLevelError, "R7 write_worktree_state command=%s error=%v "+
						"(suppressing this scan's notifications/repairs — the unsaved "+
						"ConflictResolutionAttempts/ConflictEscalated would make the next scan "+
						"re-count and re-notify, duplicating resolution tasks)", commandID, err)
					// State (attempt counters, escalation flags, resolving
					// transitions) did not persist: the next scan will
					// re-derive the same decisions and re-emit. Sending the
					// notifications now would double-dispatch.
					r = nil
					n = nil
					return
				}
			}

			r = commandRepairs
			n = commandNotifications
		})

		repairs = append(repairs, r...)
		notifications = append(notifications, n...)
	}

	return Outcome{Repairs: repairs, Notifications: notifications}
}
