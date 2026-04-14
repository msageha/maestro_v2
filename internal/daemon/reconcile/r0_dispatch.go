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
func (R0Dispatch) Apply(run *Run) Outcome {
	var repairs []Repair

	queuePath := filepath.Join(run.Deps.MaestroDir, "queue", "planner.yaml")
	lockKey := "queue:planner"

	run.Deps.LockMap.Lock(lockKey)
	defer run.Deps.LockMap.Unlock(lockKey)

	leaseSec := run.Deps.Config.Watcher.DispatchLeaseSec
	if leaseSec <= 0 {
		leaseSec = defaultDispatchLeaseSec
	}
	threshold := time.Duration(leaseSec*2) * time.Second
	if threshold < minDispatchThreshold {
		threshold = minDispatchThreshold
	}

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

			var repaired *Repair
			run.Deps.LockMap.WithLock("state:"+cmd.ID, func() {
				statePath := filepath.Join(run.Deps.MaestroDir, "state", "commands", cmd.ID+".yaml")
				if _, err := os.Stat(statePath); err != nil {
					if !os.IsNotExist(err) {
						run.Log(core.LogLevelWarn, "R0-dispatch stat_error command=%s error=%v, skipping", cmd.ID, err)
						return
					}
				} else {
					run.Log(core.LogLevelDebug, "R0-dispatch state_exists_skip command=%s", cmd.ID)
					return
				}

				updatedAt, err := time.Parse(time.RFC3339, cmd.UpdatedAt)
				if err != nil {
					run.Log(core.LogLevelWarn, "R0-dispatch parse_updated_at command=%s error=%v", cmd.ID, err)
					return
				}

				age := run.Deps.Clock.Now().Sub(updatedAt)
				if age < threshold {
					return
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

				repaired = &Repair{
					Pattern:   PatternR0Dispatch,
					CommandID: cmd.ID,
					Detail:    fmt.Sprintf("dispatch stuck %.0fs, lease released, reverted to pending", age.Seconds()),
				}
			})

			if repaired != nil {
				dirty = true
				repairs = append(repairs, *repaired)
			}
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

