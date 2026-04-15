package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

const (
	// defaultDispatchLeaseSec is the fallback lease duration (in seconds) when
	// Config.Watcher.DispatchLeaseSec is unset or non-positive.
	defaultDispatchLeaseSec = 300

	// minDispatchThreshold is the minimum age threshold before a dispatch-stuck
	// command is reverted to pending.
	minDispatchThreshold = 10 * time.Minute
)

// R0Dispatch detects commands stuck in dispatch phase (no state file created).
// Detection: status=in_progress + no state file + age > dispatch_lease_sec
// Action: Release lease and revert to pending for retry
type R0Dispatch struct{}

// Apply detects commands stuck in dispatch phase (no state file) and reverts them to pending.
//
// Uses a three-phase pattern to avoid holding queue and state locks simultaneously
// (see run.go lock ordering convention).
func (R0Dispatch) Apply(run *Run) Outcome {
	var repairs []Repair

	queuePath := filepath.Join(run.Deps.MaestroDir, "queue", "planner.yaml")
	lockKey := "queue:planner"

	leaseSec := run.Deps.Config.Watcher.DispatchLeaseSec
	if leaseSec <= 0 {
		leaseSec = defaultDispatchLeaseSec
	}
	threshold := time.Duration(leaseSec*2) * time.Second
	if threshold < minDispatchThreshold {
		threshold = minDispatchThreshold
	}

	// Phase 1: snapshot in_progress command IDs under queue lock.
	type cmdSnapshot struct {
		ID       string
		Attempts int
	}
	var candidates []cmdSnapshot
	run.Deps.LockMap.WithLock(lockKey, func() {
		data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
		if err != nil {
			return
		}
		var cq model.CommandQueue
		if err := yamlv3.Unmarshal(data, &cq); err != nil {
			return
		}
		if cq.SchemaVersion == 0 && len(cq.Commands) == 0 {
			return
		}
		for _, cmd := range cq.Commands {
			if cmd.Status == model.StatusInProgress {
				candidates = append(candidates, cmdSnapshot{
					ID: cmd.ID, Attempts: cmd.Attempts,
				})
			}
		}
	})

	if len(candidates) == 0 {
		return Outcome{}
	}

	// Phase 2: check state files under individual state locks (no queue lock held).
	stuckIDs := make(map[string]bool)
	for _, c := range candidates {
		cmdID := c.ID
		run.Deps.LockMap.WithLock("state:"+cmdID, func() {
			statePath := filepath.Join(run.Deps.MaestroDir, "state", "commands", cmdID+".yaml")
			if _, err := os.Stat(statePath); err != nil {
				if !os.IsNotExist(err) {
					run.Log(core.LogLevelWarn, "R0-dispatch stat_error command=%s error=%v, skipping", cmdID, err)
					return
				}
			} else {
				run.Log(core.LogLevelDebug, "R0-dispatch state_exists_skip command=%s", cmdID)
				return
			}
			stuckIDs[cmdID] = true
		})
	}

	if len(stuckIDs) == 0 {
		return Outcome{}
	}

	// Phase 3: apply repairs under queue lock, re-verifying each command.
	run.Deps.LockMap.Lock(lockKey)
	defer run.Deps.LockMap.Unlock(lockKey)

	if err := yamlutil.ReadModifyWrite(queuePath, func(cq *model.CommandQueue) error {
		if cq.SchemaVersion == 0 && len(cq.Commands) == 0 {
			return yamlutil.ErrNoUpdate
		}

		dirty := false
		for i := range cq.Commands {
			cmd := &cq.Commands[i]
			if cmd.Status != model.StatusInProgress {
				continue
			}
			if !stuckIDs[cmd.ID] {
				continue
			}

			// Re-verify: state file may have appeared between phase 2 and 3.
			statePath := filepath.Join(run.Deps.MaestroDir, "state", "commands", cmd.ID+".yaml")
			if _, err := os.Stat(statePath); err == nil {
				continue
			}

			updatedAt, err := time.Parse(time.RFC3339, cmd.UpdatedAt)
			if err != nil {
				run.Log(core.LogLevelWarn, "R0-dispatch parse_updated_at command=%s error=%v", cmd.ID, err)
				continue
			}

			age := run.Deps.Clock.Now().Sub(updatedAt)
			if age < threshold {
				continue
			}

			run.Log(core.LogLevelWarn, "R0-dispatch dispatch_deadlock command=%s age_sec=%.0f attempts=%d no_state_file",
				cmd.ID, age.Seconds(), cmd.Attempts)

			cmd.Status = model.StatusPending
			cmd.LeaseOwner = nil
			cmd.LeaseExpiresAt = nil

			errMsg := fmt.Sprintf("dispatch stuck %.0fs without state submission, reverted for retry",
				age.Seconds())
			cmd.LastError = &errMsg
			cmd.UpdatedAt = run.Deps.Clock.Now().UTC().Format(time.RFC3339)

			repairs = append(repairs, Repair{
				Pattern:   PatternR0Dispatch,
				CommandID: cmd.ID,
				Detail:    fmt.Sprintf("dispatch stuck %.0fs, lease released, reverted to pending", age.Seconds()),
			})
			dirty = true
		}

		if !dirty {
			return yamlutil.ErrNoUpdate
		}
		return nil
	}); err != nil {
		run.Log(core.LogLevelError, "R0-dispatch update_queue error=%v", err)
	}

	return Outcome{Repairs: repairs}
}
