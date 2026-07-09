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
// Action: durably queue the fill_timeout planner signal FIRST (WAL), then set
// the phase to timed_out and cascade cancel downstream pending phases.
//
// Ordering rationale (D-F5): R6 only scans awaiting_fill phases, so once the
// timed_out transition is persisted the rule never re-fires for that phase. A
// crash between the state write and the first delivery therefore lost the
// escalation forever. Writing the durable signal BEFORE the transition closes
// that window: whatever the crash point, either the signal is already queued
// (Phase B delivers it with retry/backoff after restart) or the phase is
// still awaiting_fill and the next scan re-detects the expired deadline. A
// signal that was queued but whose transition never landed is removed by the
// scan loop's staleness filter (fill_timeout + phase not timed_out) before
// any delivery is attempted, so a premature crash cannot leak a false
// timeout message to the Planner.
type R6FillTimeout struct{}

// r6PhaseRef identifies one expired awaiting_fill phase between the WAL
// enqueue and the state transition.
type r6PhaseRef struct {
	id       string
	name     string
	deadline string
}

// Apply detects phases with an expired fill deadline, durably queues their
// fill_timeout planner signals, then transitions them to timed_out with
// cascading cancellations to dependent downstream phases. It follows the
// package's three-phase lock pattern (state read → queue write → state
// verify+write); see the type comment for the crash-window analysis.
func (R6FillTimeout) Apply(run *Run) Outcome {
	var repairs []Repair

	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.cachedReadDir(stateDir)
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

		// Phase 1 (state lock, read-only): collect phases whose fill
		// deadline has expired.
		var candidates []r6PhaseRef
		run.Deps.LockMap.WithLock(lockKey, func() {
			state, err := run.loadState(statePath)
			if err != nil {
				if !os.IsNotExist(err) {
					run.Log(core.LogLevelError, "R6 load_state_corrupted command=%s file=%s error=%v", commandID, entry.Name(), err)
				}
				return
			}
			for i := range state.Phases {
				phase := &state.Phases[i]
				if phase.Status != model.PhaseStatusAwaitingFill || phase.FillDeadlineAt == nil {
					continue
				}
				deadline, err := time.Parse(time.RFC3339, *phase.FillDeadlineAt)
				if err != nil {
					continue
				}
				if run.Deps.Clock.Now().UTC().Before(deadline) {
					continue
				}
				candidates = append(candidates, r6PhaseRef{id: phase.PhaseID, name: phase.Name, deadline: *phase.FillDeadlineAt})
			}
		})
		if len(candidates) == 0 {
			continue
		}

		// Phase 2 (queue lock): WAL — durably queue the fill_timeout signals
		// before any state mutation. upsertPlannerSignal dedups on
		// (Kind, CommandID, PhaseID), so a crash-retry loop cannot stack
		// duplicates. Message format matches the Phase A transition writer
		// so Planner-side handling is uniform.
		now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
		for _, ref := range candidates {
			upsertPlannerSignal(run, model.PlannerSignal{
				Kind:      "fill_timeout",
				CommandID: commandID,
				PhaseID:   ref.id,
				PhaseName: ref.name,
				Message: fmt.Sprintf("[maestro] kind:fill_timeout command_id:%s phase:%s\nfill deadline expired",
					commandID, ref.name),
				CreatedAt: now,
				UpdatedAt: now,
			})
			run.Log(core.LogLevelInfo, "R6 fill_timeout_signal_queued command=%s phase=%s (durable signal precedes the timed_out transition)",
				commandID, ref.name)
		}

		// Phase 3 (state lock): re-verify each candidate is still at
		// awaiting_fill (the Planner may have filled it between phases —
		// its signal then goes stale and the scan loop's staleness filter
		// removes it) and apply the transition + cascade cancels.
		var commandRepairs []Repair
		run.Deps.LockMap.WithLock(lockKey, func() {
			state, err := run.loadState(statePath)
			if err != nil {
				run.Log(core.LogLevelError, "R6 reload_state command=%s error=%v (signals already queued; next scan retries the transition)",
					commandID, err)
				return
			}

			modified := false
			timedOutPhases := make(map[string]bool)
			for _, ref := range candidates {
				idx, ok := state.PhaseIndex(ref.id)
				if !ok {
					continue
				}
				phase := &state.Phases[idx]
				if phase.Status != model.PhaseStatusAwaitingFill {
					run.Log(core.LogLevelInfo, "R6 skip_phase_moved_on command=%s phase=%s status=%s (Planner acted between snapshot and transition; stale signal is filtered before delivery)",
						commandID, phase.Name, phase.Status)
					continue
				}

				run.Log(core.LogLevelWarn, "R6 awaiting_fill_deadline_expired command=%s phase=%s deadline=%s",
					commandID, phase.Name, ref.deadline)
				phase.Status = model.PhaseStatusTimedOut
				modified = true
				timedOutPhases[phase.Name] = true
				commandRepairs = append(commandRepairs, Repair{
					Pattern:   PatternR6,
					CommandID: commandID,
					Detail:    fmt.Sprintf("phase %s timed_out (deadline %s)", phase.Name, ref.deadline),
				})
			}

			// Cascade cancel
			if len(timedOutPhases) > 0 {
				cascadeModified, cascadeRepairs, _ := cascadeCancelTimedOutPhases(state, timedOutPhases, run, commandID)
				if cascadeModified {
					modified = true
					commandRepairs = append(commandRepairs, cascadeRepairs...)
				}
			}

			if !modified {
				commandRepairs = nil
				return
			}
			nowStr := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
			state.LastReconciledAt = &nowStr
			state.UpdatedAt = nowStr
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				run.Log(core.LogLevelError, "R6 write_state command=%s error=%v (signals already queued; the staleness filter drops them while the phase stays awaiting_fill and the next scan retries)",
					commandID, err)
				commandRepairs = nil
				return
			}
		})

		repairs = append(repairs, commandRepairs...)
	}

	return Outcome{Repairs: repairs}
}

// cascadeCancelTimedOutPhases propagates cancellation from timedOutPhases to all
// transitively dependent pending/awaiting_fill phases. Returns true if any phase
// was cancelled, along with the accumulated repairs and full set of cancelled phase names.
func cascadeCancelTimedOutPhases(state *model.CommandState, timedOutPhases map[string]bool, run *Run, commandID string) (bool, []Repair, map[string]bool) {
	cancelledPhases := make(map[string]bool, len(timedOutPhases))
	for name := range timedOutPhases {
		cancelledPhases[name] = true
	}

	modified := false
	var repairs []Repair

	for {
		changed := false
		for i := range state.Phases {
			phase := &state.Phases[i]
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

					repairs = append(repairs, Repair{
						Pattern:   PatternR6,
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

	return modified, repairs, cancelledPhases
}
