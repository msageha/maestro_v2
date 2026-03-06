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

// R0Dispatch detects commands stuck in dispatch phase (no state file created).
// Detection: status=in_progress + no state file + age > dispatch_lease_sec
// Action: Release lease and revert to pending for retry
type R0Dispatch struct{}

func (R0Dispatch) Name() string { return "R0-dispatch" }

func (R0Dispatch) Apply(run *Run) Outcome {
	var repairs []Repair

	queuePath := filepath.Join(run.Deps.MaestroDir, "queue", "planner.yaml")
	lockKey := "queue:planner"

	run.Deps.LockMap.Lock(lockKey)
	defer run.Deps.LockMap.Unlock(lockKey)

	data, err := os.ReadFile(queuePath)
	if err != nil {
		if !os.IsNotExist(err) {
			run.Log(core.LogLevelWarn, "R0-dispatch read_queue error=%v", err)
		}
		return Outcome{}
	}

	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		run.Log(core.LogLevelWarn, "R0-dispatch parse_queue error=%v", err)
		return Outcome{}
	}

	leaseSec := run.Deps.Config.Watcher.DispatchLeaseSec
	if leaseSec <= 0 {
		leaseSec = 300
	}
	threshold := time.Duration(leaseSec*2) * time.Second
	if threshold < 10*time.Minute {
		threshold = 10 * time.Minute
	}

	dirty := false
	for i := range cq.Commands {
		cmd := &cq.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}

		repaired := func(cmd *model.Command) *Repair {
			stateLockKey := "state:" + cmd.ID
			run.Deps.LockMap.Lock(stateLockKey)
			defer run.Deps.LockMap.Unlock(stateLockKey)

			statePath := filepath.Join(run.Deps.MaestroDir, "state", "commands", cmd.ID+".yaml")
			if _, err := os.Stat(statePath); err != nil {
				if !os.IsNotExist(err) {
					run.Log(core.LogLevelWarn, "R0-dispatch stat_error command=%s error=%v, skipping", cmd.ID, err)
					return nil
				}
			} else {
				run.Log(core.LogLevelDebug, "R0-dispatch state_exists_skip command=%s", cmd.ID)
				return nil
			}

			updatedAt, err := time.Parse(time.RFC3339, cmd.UpdatedAt)
			if err != nil {
				run.Log(core.LogLevelWarn, "R0-dispatch parse_updated_at command=%s error=%v", cmd.ID, err)
				return nil
			}

			age := run.Deps.Clock.Now().Sub(updatedAt)
			if age < threshold {
				return nil
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

			return &Repair{
				Pattern:   "R0-dispatch",
				CommandID: cmd.ID,
				Detail:    fmt.Sprintf("dispatch stuck %.0fs, lease released, reverted to pending", age.Seconds()),
			}
		}(cmd)

		if repaired != nil {
			dirty = true
			repairs = append(repairs, *repaired)
		}
	}

	if dirty {
		if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
			run.Log(core.LogLevelError, "R0-dispatch write_queue error=%v", err)
		}
	}

	return Outcome{Repairs: repairs}
}
