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

// R8PublishFailed detects commands whose integration status has reached
// IntegrationStatusQuarantined due to repeated publish failures and escalates
// to the Planner through the durable planner-signal queue.
//
// The quarantine transition itself is performed by the worktree Manager
// (recordPublishFailure) when PublishFailureCount reaches the threshold.
// This pattern observes the resulting state and ensures the Planner is
// informed at least once (guarded by StallSignaled).
//
// Action:
//   - IntegrationStatusQuarantined with non-empty QuarantineReason containing
//     "publish": durably queue a publish_quarantined planner signal FIRST
//     (WAL), then set StallSignaled=true and record a Repair. Delivery
//     happens through the planner-signal queue (retry/backoff, removed on
//     successful delivery), so neither a delivery failure nor a crash between
//     the guard write and delivery can silence the escalation (D-F1).
//     Duplicate deliveries are possible in crash-retry windows and are
//     tolerated downstream.
type R8PublishFailed struct{}

// Apply scans worktree state files for commands quarantined due to publish
// failures and triggers Planner escalation, following the package's
// three-phase lock pattern (worktree read → queue write → worktree
// verify+write).
func (R8PublishFailed) Apply(run *Run) Outcome {
	var repairs []Repair

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

		// Phase 1 (worktree lock, read-only): detect an unsignalled publish
		// quarantine.
		candidate := false
		quarantineReason := ""
		run.Deps.LockMap.WithLock(lockKey, func() {
			state, err := run.loadWorktreeState(statePath)
			if err != nil {
				if !os.IsNotExist(err) {
					run.Log(core.LogLevelError, "R8 load_worktree_state command=%s error=%v", commandID, err)
				}
				return
			}
			if state.Integration.Status != model.IntegrationStatusQuarantined {
				return
			}
			// Only handle publish-related quarantines.
			if !strings.Contains(state.Integration.QuarantineReason, "publish") {
				return
			}
			// Guard: emit at most once per quarantine event.
			if state.Integration.StallSignaled {
				return
			}
			candidate = true
			quarantineReason = state.Integration.QuarantineReason
		})
		if !candidate {
			continue
		}

		// Phase 2 (queue lock): WAL — durably queue the escalation signal
		// BEFORE the guard write.
		now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
		upsertPlannerSignal(run, model.PlannerSignal{
			Kind:      "publish_quarantined",
			CommandID: commandID,
			Message: fmt.Sprintf("[maestro] kind:publish_quarantined command_id:%s\npublish failures reached quarantine threshold — operator intervention required: %s",
				commandID, quarantineReason),
			CreatedAt: now,
			UpdatedAt: now,
		})
		run.Log(core.LogLevelInfo,
			"R8 publish_quarantined_signal_queued command=%s (durable signal precedes the one-shot guard)", commandID)

		// Phase 3 (worktree lock): re-verify and persist the guard.
		signalled := false
		run.Deps.LockMap.WithLock(lockKey, func() {
			state, err := run.loadWorktreeState(statePath)
			if err != nil {
				run.Log(core.LogLevelError, "R8 reload_worktree_state command=%s error=%v (signal already queued; next scan retries the guard)",
					commandID, err)
				return
			}
			if state.Integration.Status != model.IntegrationStatusQuarantined ||
				!strings.Contains(state.Integration.QuarantineReason, "publish") ||
				state.Integration.StallSignaled {
				return // moved on between phases; Phase 4 compensates the signal
			}

			run.Log(core.LogLevelWarn, "R8 publish_quarantined command=%s failure_count=%d reason=%s",
				commandID, state.Integration.PublishFailureCount, state.Integration.QuarantineReason)

			state.Integration.StallSignaled = true
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				run.Log(core.LogLevelError, "R8 write_worktree_state command=%s error=%v (signal already queued; next scan retries the guard)",
					commandID, err)
				return
			}
			signalled = true
			repairs = append(repairs, Repair{
				Pattern:   PatternR8,
				CommandID: commandID,
				Detail:    fmt.Sprintf("publish quarantined: %s", state.Integration.QuarantineReason),
			})
		})

		// Phase 4: compensate the signal when the quarantine resolved
		// between phases — the kind is command-scoped so the staleness
		// filter cannot clean it up. On a guard-write FAILURE the signal is
		// deliberately kept: the escalation fact still holds and the retry
		// dedups against it. The state re-read happens under the worktree
		// lock and the removal under the queue lock afterwards (locks are
		// never held simultaneously).
		if !signalled {
			stillPending := false
			run.Deps.LockMap.WithLock(lockKey, func() {
				state, err := run.loadWorktreeState(statePath)
				if err != nil {
					stillPending = true // cannot verify; keep the signal
					return
				}
				stillPending = state.Integration.Status == model.IntegrationStatusQuarantined &&
					strings.Contains(state.Integration.QuarantineReason, "publish") &&
					!state.Integration.StallSignaled
			})
			if !stillPending {
				removePlannerSignal(run, "publish_quarantined", commandID, "")
			}
		}
	}

	return Outcome{Repairs: repairs}
}
