package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// r4Backoff tracks backoff state for a single command's CanComplete failures.
type r4Backoff struct {
	failures      int // consecutive CanComplete failure count
	skipRemaining int // remaining cycles to skip before next retry
}

// BackoffTracker holds exponential backoff state for CanComplete failures,
// keyed by commandID. It is designed to be owned externally (e.g. by
// Reconciler) and injected into R4PlanStatus so that backoff state survives
// Engine reconstruction.
type BackoffTracker struct {
	entries map[string]*r4Backoff
}

// NewBackoffTracker creates an empty BackoffTracker.
func NewBackoffTracker() *BackoffTracker {
	return &BackoffTracker{entries: make(map[string]*r4Backoff)}
}

// tick decrements all active backoff counters once per reconciliation cycle.
func (bt *BackoffTracker) tick() {
	for _, bo := range bt.entries {
		if bo.skipRemaining > 0 {
			bo.skipRemaining--
		}
	}
}

// isInBackoff returns true if the command should be skipped this cycle.
func (bt *BackoffTracker) isInBackoff(commandID string) bool {
	bo, ok := bt.entries[commandID]
	return ok && bo.skipRemaining > 0
}

// recordFailure increments the failure count and sets the backoff skip counter.
func (bt *BackoffTracker) recordFailure(commandID string) {
	bo, ok := bt.entries[commandID]
	if !ok {
		bo = &r4Backoff{}
		bt.entries[commandID] = bo
	}
	bo.failures++
	bo.skipRemaining = r4BackoffCycles(bo.failures)
}

// clearBackoff removes backoff state for a command (on success or state resolution).
func (bt *BackoffTracker) clearBackoff(commandID string) {
	delete(bt.entries, commandID)
}

// R4PlanStatus detects results/planner terminal + state non-terminal plan_status.
// Action: re-evaluate via plan.CanComplete. If OK, update plan_status. If NG, quarantine result.
//
// When CanComplete fails, the command enters exponential backoff (1, 2, 4, 8 cycles max)
// to avoid repeatedly invoking a failing evaluator every scan cycle.
//
// The BackoffTracker is held externally so that backoff state survives Engine
// reconstruction. Use NewR4PlanStatus to create an instance with a shared tracker.
type R4PlanStatus struct {
	backoffs *BackoffTracker
}

// NewR4PlanStatus creates an R4PlanStatus that uses the provided BackoffTracker.
// If tracker is nil, a new empty tracker is created (backward-compatible default).
func NewR4PlanStatus(tracker *BackoffTracker) *R4PlanStatus {
	if tracker == nil {
		tracker = NewBackoffTracker()
	}
	return &R4PlanStatus{backoffs: tracker}
}

// r4BackoffCycles returns the number of cycles to skip after the nth consecutive failure.
// Exponential: 1, 2, 4, 8 (capped at 8).
func r4BackoffCycles(failures int) int {
	if failures <= 0 {
		return 0
	}
	cycles := 1 << (failures - 1) // 2^(failures-1)
	if cycles > 8 {
		return 8
	}
	return cycles
}

// Tick advances the exponential-backoff counters by one reconciliation cycle.
// The engine calls this exactly once per Reconcile() (before the bounded
// fixpoint loop), NOT inside Apply: Apply runs up to maxReconcilePasses times
// per scan, so ticking there decremented every command's skipRemaining 2–3x
// per scan and shortened the intended backoff window.
func (r *R4PlanStatus) Tick() {
	r.backoffs.tick()
}

// Apply detects non-terminal plan_status despite terminal planner result and updates or quarantines.
func (r *R4PlanStatus) Apply(run *Run) Outcome {
	var repairs []Repair
	var notifications []DeferredNotification

	resultPath := filepath.Join(run.Deps.MaestroDir, "results", "planner.yaml")
	rf, err := run.loadCommandResultFile(resultPath)
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

		// Skip commands in exponential backoff after CanComplete failures
		if r.backoffs.isInBackoff(commandID) {
			run.Log(core.LogLevelDebug, "R4 backoff_skip command=%s", commandID)
			continue
		}

		statePath := filepath.Join(run.Deps.MaestroDir, "state", "commands", commandID+".yaml")

		lockKey := "state:" + commandID
		outcome := func() r4Outcome {
			run.Deps.LockMap.Lock(lockKey)
			defer run.Deps.LockMap.Unlock(lockKey)

			state, err := run.loadState(statePath)
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
				r.backoffs.recordFailure(commandID)
				// Retryable failures (phase mid-fill, A/B selection in
				// progress, merge_recorded gate) resolve on a following scan
				// without operator action. Quarantining on those deleted the
				// planner result permanently while recovery depended on a
				// single re_evaluate notification delivery (D-F2). Backoff is
				// still recorded so a long-lived transient does not hammer
				// the evaluator every scan. Classification asks the evaluator's
				// own error (plan.IsRetryable) — an evaluator that does not
				// wrap plan's retryableError falls back to the pre-existing
				// quarantine path.
				if plan.IsRetryable(canCompleteErr) {
					run.Log(core.LogLevelInfo,
						"R4 can_complete_retryable command=%s error=%v (transient fill/selection/merge-gate state; deferring to next scan instead of quarantining)",
						commandID, canCompleteErr)
					return r4Outcome{}
				}
				run.Log(core.LogLevelWarn, "R4 can_complete_failed command=%s error=%v → quarantine result + notify planner",
					commandID, canCompleteErr)
				return r4Outcome{
					quarantine: true,
					repair: &Repair{
						Pattern:   PatternR4,
						CommandID: commandID,
						Detail:    fmt.Sprintf("can_complete failed (%v), result quarantined, planner notification deferred", canCompleteErr),
					},
					notification: &DeferredNotification{
						Kind:      NotifyReEvaluate,
						CommandID: commandID,
						Reason:    canCompleteErr.Error(),
					},
				}
			}

			r.backoffs.clearBackoff(commandID)
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
					Pattern:   PatternR4,
					CommandID: commandID,
					Detail:    fmt.Sprintf("plan_status updated to %s via can_complete", derivedStatus),
				},
			}
		}()

		if outcome.quarantine {
			if err := run.quarantineCommandResult(resultPath, result); err != nil {
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
