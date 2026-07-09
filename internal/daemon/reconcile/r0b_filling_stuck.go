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
//
//	Phase 1: read state under lock → collect stuck phases and task IDs
//	Phase 2: remove tasks from worker queues (no state lock held)
//	Phase 3: re-acquire state lock → apply changes → write state
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

		// Phase 2 pre-check: re-read the state (lock-free atomic file read)
		// and drop phases that left `filling` since the Phase 1 snapshot.
		// R0b fires precisely when the Planner is slow, so "Planner
		// completed the fill between snapshot and now" is a realistic race:
		// batchRemoveTaskIDsFromQueues ignores task status and would strip
		// freshly dispatched in_progress tasks from the queues, while
		// Phase 3's filling re-check then skips the state restore — a
		// state/queue split-brain whose workers get task-not-found on
		// result_write. The residual window between this re-check and the
		// queue mutation is micro-seconds instead of the full Phase 1→2 gap,
		// and Phase 3 still re-verifies the state side under the lock.
		if state, err := run.loadState(statePath); err == nil {
			stillFilling := make(map[string]bool)
			for i := range state.Phases {
				if state.Phases[i].Status == model.PhaseStatusFilling {
					stillFilling[state.Phases[i].Name] = true
				}
			}
			filtered := stuckPhases[:0]
			allTaskIDsToRemove = allTaskIDsToRemove[:0]
			for _, sp := range stuckPhases {
				if !stillFilling[sp.phaseName] {
					run.Log(core.LogLevelInfo,
						"R0b skip_phase_left_filling command=%s phase=%s (fill completed between snapshot and queue removal)",
						commandID, sp.phaseName)
					continue
				}
				filtered = append(filtered, sp)
				allTaskIDsToRemove = append(allTaskIDsToRemove, sp.taskIDs...)
			}
			stuckPhases = filtered
			if len(stuckPhases) == 0 {
				continue
			}
		}

		// Phase 2: remove tasks from worker queues (no state lock held).
		// If this fails, skip state update to prevent split-brain.
		// keptActive entries (status advanced past pre-dispatch between the
		// re-check above and the removal) veto their whole phase in Phase 3:
		// a phase with a live dispatched task is no longer stuck-filling.
		keptActive := map[string]bool{}
		removedEntries := map[string]removedQueueEntry{}
		if len(allTaskIDsToRemove) > 0 {
			var batchErr error
			removedEntries, keptActive, batchErr = run.batchRemoveTaskIDsFromQueues(allTaskIDsToRemove)
			if batchErr != nil {
				// Partial failure: queues that were written before the error
				// lost their rows while the state keeps the task IDs — the
				// same split-brain the veto compensation guards against.
				// Re-insert everything captured so far (idempotent: rows
				// whose write actually failed still exist and are skipped).
				run.Log(core.LogLevelError,
					"R0b batch_remove_tasks command=%s error=%v, skipping state update (re-inserting %d removed rows)",
					commandID, batchErr, len(removedEntries))
				restoreAllRemovedEntries(run, removedEntries)
				continue
			}
		}

		// Phase 3: re-acquire state lock, re-read state, apply changes, write.
		// Every early exit that leaves the state referencing the removed
		// tasks must queue a compensation re-insert (restoreAll / restores),
		// otherwise the state/queue split-brain lets tryClearPhantomTasks
		// force-cancel a healthy fill two scans later.
		var modified bool
		var restores []removedQueueEntry
		restoreAll := false
		run.Deps.LockMap.WithLock(lockKey, func() {
			state, err := run.loadState(statePath)
			if err != nil {
				// State file deleted between Phase 2 and here (concurrent
				// cancel / cleanup finished the command): the removed rows
				// belong to a dead command, so re-inserting them would only
				// create orphan queue rows. Drop them instead.
				if os.IsNotExist(err) {
					run.Log(core.LogLevelInfo,
						"R0b state_gone_after_removal command=%s (command finished concurrently; dropping %d removed pre-dispatch rows)",
						commandID, len(removedEntries))
					return
				}
				// Transient read failure: state unchanged on disk but queue
				// rows are gone — restore them all so the next scan can
				// retry the whole transition.
				run.Log(core.LogLevelError,
					"R0b reload_state command=%s error=%v (re-inserting %d removed queue rows)",
					commandID, err, len(removedEntries))
				restoreAll = true
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
				// Re-verify: phase may no longer be in filling status (the
				// Planner completed the fill between the Phase 2 queue
				// removal and this re-read). The fill is live, so re-insert
				// the pre-dispatch rows Phase 2 deleted — same compensation
				// as the active-task veto below.
				if phase.Status != model.PhaseStatusFilling {
					restored := 0
					for _, taskID := range sp.taskIDs {
						if e, ok := removedEntries[taskID]; ok {
							restores = append(restores, e)
							restored++
						}
					}
					if restored > 0 {
						run.Log(core.LogLevelInfo,
							"R0b skip_phase_left_filling_after_removal command=%s phase=%s status=%s (re-inserting %d rows)",
							state.CommandID, sp.phaseName, phase.Status, restored)
					}
					continue
				}
				// Veto: a task of this phase advanced past pre-dispatch
				// during Phase 2 — the fill is live, leave the phase alone.
				// Pre-dispatch siblings of the SAME phase may already have
				// been deleted before the active row was encountered, so
				// queue the deleted rows for compensation re-insert (after
				// the state lock is released; queue→state order).
				phaseHasActive := false
				for _, taskID := range sp.taskIDs {
					if keptActive[taskID] {
						phaseHasActive = true
						break
					}
				}
				if phaseHasActive {
					for _, taskID := range sp.taskIDs {
						if e, ok := removedEntries[taskID]; ok {
							restores = append(restores, e)
						}
					}
					run.Log(core.LogLevelInfo,
						"R0b skip_phase_with_active_task command=%s phase=%s (queue entry advanced past pre-dispatch during removal; re-inserting %d sibling rows)",
						state.CommandID, sp.phaseName, len(restores))
					continue
				}

				for _, taskID := range sp.taskIDs {
					delete(state.TaskStates, taskID)
					delete(state.TaskStatusChangedAt, taskID)
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
					// State unchanged on disk (write failed) but queue rows
					// are gone — restore everything so the next scan retries.
					run.Log(core.LogLevelError,
						"R0b write_state command=%s error=%v (re-inserting %d removed queue rows)",
						state.CommandID, err, len(removedEntries))
					restoreAll = true
					return
				}
				repairs = append(repairs, localRepairs...)
			}

			modified = localModified
		})

		// Compensation (state lock released; queue→state lock order): re-insert
		// pre-dispatch rows that Phase 2 deleted but the state still references —
		// vetoed phases, phases that left filling, or whole-batch failure paths.
		// restoreQueueEntries is idempotent, but restoreAll supersedes the
		// per-phase list to avoid building both for the same rows.
		if restoreAll {
			restoreAllRemovedEntries(run, removedEntries)
		} else if len(restores) > 0 {
			run.restoreQueueEntries(restores)
		}

		if modified && run.Deps.ExecutorFactory != nil {
			notifications = append(notifications, DeferredNotification{
				Kind:      NotifyReFill,
				CommandID: commandID,
			})
		}
	}

	return Outcome{Repairs: repairs, Notifications: notifications}
}

// restoreAllRemovedEntries re-inserts every queue row captured by
// batchRemoveTaskIDsFromQueues. Used by the R0b failure paths (partial batch
// failure, state reload failure, state write failure) where the state still
// references all removed tasks. Idempotent via restoreQueueEntries.
func restoreAllRemovedEntries(run *Run, removedEntries map[string]removedQueueEntry) {
	if len(removedEntries) == 0 {
		return
	}
	all := make([]removedQueueEntry, 0, len(removedEntries))
	for _, e := range removedEntries {
		all = append(all, e)
	}
	run.restoreQueueEntries(all)
}
