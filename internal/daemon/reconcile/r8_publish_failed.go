package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// R8PublishFailed detects commands whose integration status has reached
// IntegrationStatusQuarantined due to repeated publish failures and emits
// a NotifyPublishQuarantined escalation to the Planner.
//
// The quarantine transition itself is performed by the worktree Manager
// (recordPublishFailure) when PublishFailureCount reaches the threshold.
// This pattern observes the resulting state and ensures the Planner is
// informed exactly once (guarded by StallSignaled).
//
// Action:
//   - IntegrationStatusQuarantined with non-empty QuarantineReason containing
//     "publish": set StallSignaled=true to prevent re-emission, emit
//     NotifyPublishQuarantined, and record a Repair.
type R8PublishFailed struct{}

// Apply scans worktree state files for commands quarantined due to publish
// failures and triggers Planner escalation.
func (R8PublishFailed) Apply(run *Run) Outcome {
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

			// Guard: emit notification only once per quarantine event.
			if state.Integration.StallSignaled {
				return
			}

			run.Log(core.LogLevelWarn, "R8 publish_quarantined command=%s failure_count=%d reason=%s",
				commandID, state.Integration.PublishFailureCount, state.Integration.QuarantineReason)

			state.Integration.StallSignaled = true
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				run.Log(core.LogLevelError, "R8 write_worktree_state command=%s error=%v", commandID, err)
				return
			}

			n = append(n, DeferredNotification{
				Kind:      NotifyPublishQuarantined,
				CommandID: commandID,
				Reason:    state.Integration.QuarantineReason,
			})
			r = append(r, Repair{
				Pattern:   PatternR8,
				CommandID: commandID,
				Detail:    fmt.Sprintf("publish quarantined: %s", state.Integration.QuarantineReason),
			})
		})

		repairs = append(repairs, r...)
		notifications = append(notifications, n...)
	}

	return Outcome{Repairs: repairs, Notifications: notifications}
}
