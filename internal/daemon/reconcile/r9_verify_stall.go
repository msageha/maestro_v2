package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// R9VerifyStall recovers tasks that are stuck in verify_pending. The §S1-1
// verification runner is responsible for advancing such tasks to completed or
// repair_pending, but if the runner crashes mid-execution (or the daemon is
// restarted between writing the result and finishing verification) a task can
// be left dangling. R9 detects tasks whose verify_pending state has been held
// longer than VerifyDaemonConfig.StallThresholdSec and forces them to
// repair_pending so the planner / retry pipeline can construct a fresh attempt.
//
// Stall age is measured against the most recent worker result for the task —
// when result_write_phase_b records a verify_pending transition the task's
// matching TaskResult.CreatedAt is the closest available timestamp for "when
// verification started". Falling back to the state file's UpdatedAt would over-
// count edits unrelated to this task.
type R9VerifyStall struct{}

// Apply scans every command state file, finds verify_pending tasks whose
// matching worker result is older than the configured threshold, and rewrites
// their TaskStates entry to repair_pending.
func (R9VerifyStall) Apply(run *Run) Outcome {
	thresholdSec := run.Deps.Config.Verify.EffectiveStallThresholdSec()
	if thresholdSec <= 0 {
		return Outcome{} // disabled
	}
	threshold := time.Duration(thresholdSec) * time.Second

	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.cachedReadDir(stateDir)
	if err != nil {
		return Outcome{}
	}

	resultTimestamps := r9LoadResultTimestamps(run)

	var repairs []Repair
	now := run.Deps.Clock.Now().UTC()

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(name, ".yaml")
		statePath := filepath.Join(stateDir, name)

		commandRepairs := r9ApplyForCommand(run, statePath, commandID, resultTimestamps[commandID], now, threshold)
		repairs = append(repairs, commandRepairs...)
	}

	return Outcome{Repairs: repairs}
}

// r9ApplyForCommand applies R9 logic to a single command state file under its
// state lock. Returns the repairs performed (or nil when no transitions were
// needed / the write failed).
func r9ApplyForCommand(run *Run, statePath, commandID string, resultsForCommand map[string]time.Time, now time.Time, threshold time.Duration) []Repair {
	var commandRepairs []Repair

	run.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			if !os.IsNotExist(err) {
				run.Log(core.LogLevelError, "R9 load_state command=%s error=%v", commandID, err)
			}
			return
		}
		if state.TaskStates == nil {
			return
		}

		modified := false
		for taskID, status := range state.TaskStates {
			if status != model.StatusVerifyPending {
				continue
			}
			startedAt, ok := resultsForCommand[taskID]
			if !ok {
				// No result on disk for this task — fall back to the
				// state file's UpdatedAt to avoid permanent stuck states
				// when results are archived.
				if state.UpdatedAt == "" {
					continue
				}
				if t, perr := time.Parse(time.RFC3339, state.UpdatedAt); perr == nil {
					startedAt = t
				} else {
					continue
				}
			}
			age := now.Sub(startedAt)
			if age < threshold {
				continue
			}

			run.Log(core.LogLevelWarn,
				"R9 verify_pending_stall command=%s task=%s age=%s threshold=%s -> repair_pending",
				commandID, taskID, age.Round(time.Second), threshold)

			state.TaskStates[taskID] = model.StatusRepairPending
			modified = true
			commandRepairs = append(commandRepairs, Repair{
				Pattern:   PatternR9,
				CommandID: commandID,
				TaskID:    taskID,
				Detail:    fmt.Sprintf("verify_pending stalled for %s (>%s); transitioned to repair_pending (verify_runner_stall)", age.Round(time.Second), threshold),
			})
		}

		if modified {
			nowStr := now.Format(time.RFC3339)
			state.LastReconciledAt = &nowStr
			state.UpdatedAt = nowStr
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				run.Log(core.LogLevelError, "R9 write_state command=%s error=%v", commandID, err)
				commandRepairs = nil
			}
		}
	})

	return commandRepairs
}

// r9LoadResultTimestamps walks results/worker*.yaml and returns the most
// recent CreatedAt for each (commandID, taskID) pair. The latest result is
// preferred because verify_pending is set immediately after a worker writes a
// result; older results from earlier attempts must not gate stall detection.
func r9LoadResultTimestamps(run *Run) map[string]map[string]time.Time {
	out := make(map[string]map[string]time.Time)

	resultsDir := filepath.Join(run.Deps.MaestroDir, "results")
	entries, err := run.cachedReadDir(resultsDir)
	if err != nil {
		return out
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		path := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(path) //nolint:gosec // path is in a controlled application directory
		if err != nil {
			continue
		}
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			continue
		}
		for _, res := range rf.Results {
			if res.CreatedAt == "" {
				continue
			}
			t, perr := time.Parse(time.RFC3339, res.CreatedAt)
			if perr != nil {
				continue
			}
			byCommand, ok := out[res.CommandID]
			if !ok {
				byCommand = make(map[string]time.Time)
				out[res.CommandID] = byCommand
			}
			if existing, exists := byCommand[res.TaskID]; !exists || t.After(existing) {
				byCommand[res.TaskID] = t
			}
		}
	}

	return out
}
