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
	//
	// This value is the "pane completely silent before we give up" timer,
	// NOT a per-task SLA: the queue-scan path inspects each worker pane
	// every scan and extends the lease when it observes activity
	// (cross-scan hash delta or busy_patterns). Long-running tasks that
	// produce output get their lease auto-extended; only a frozen pane
	// hits this default.
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

	queuePath := commandQueuePath(run.Deps.MaestroDir)
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

	// Planner-liveness probe (run once outside the queue lock; the Planner is a
	// singleton). R0 keys off cmd.UpdatedAt, which is frozen at dispatch time —
	// lease auto-extension bumps only LeaseExpiresAt (see lease/manager.go
	// extendLeaseExpiry), and the command state file is not created until the
	// Planner's first `plan submit`. A Planner that is alive and still working
	// toward that first submit (slow analysis, large command) would otherwise be
	// reverted at the threshold even while the queue scan keeps extending its
	// lease, re-delivering the command to the Planner pane (a confusing
	// duplicate; in the worst case a duplicate plan submit that ErrDoubleSubmit
	// only partially guards).
	//
	// When the probe CONFIRMS busy we DEFER the revert — but only up to a hard
	// cap (max_in_progress_min). A confirmed-busy Planner that has produced no
	// state file for that long is wedged (busy on unrelated work or a non-submit
	// loop), and the command-lease max-timeout path cannot rescue a no-state
	// command (commandHasActivePlannerWork treats GetCommandPhases'
	// ErrStateNotFound as "active" and extends), so R0 must still be the
	// backstop. Idle / undecided / probe-unavailable never defer, preserving the
	// fast threshold-age recovery for a genuinely stuck or dead dispatch.
	plannerBusy := plannerPaneActivelyProcessing(run)
	busyDeferCap := time.Duration(run.Deps.Config.Watcher.EffectiveMaxInProgressMin()) * time.Minute

	// Diagnostic-only probe: does the Planner pane show an agent-runtime API
	// error banner (e.g. a safety-classifier rejection) instead of having
	// processed the dispatched command? This never changes the revert
	// decision below — it only annotates the dispatch_deadlock log so an
	// operator does not have to manually inspect tmux to learn that the
	// pane's silence is explained by the runtime rejecting the message, not
	// by a wedged or dead agent. Computed once per Apply call for the same
	// reason plannerBusy is: the Planner is a singleton.
	plannerAPIError := plannerPaneShowsAPIError(run)

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

			// Defer to a confirmed-busy Planner, bounded by busyDeferCap so a
			// wedged Planner cannot suspend recovery indefinitely.
			if plannerBusy && busyDeferCap > 0 && age < busyDeferCap {
				run.Log(core.LogLevelInfo,
					"R0-dispatch defer_planner_active command=%s age_sec=%.0f cap=%s "+
						"(planner pane busy and within hard cap; deferring dispatch-stuck revert so a slow first plan-submit is not re-dispatched)",
					cmd.ID, age.Seconds(), busyDeferCap)
				continue
			}

			if plannerAPIError {
				run.Log(core.LogLevelWarn,
					"R0-dispatch planner_api_error_detected command=%s "+
						"(planner pane shows an agent-runtime API Error banner instead of processing the "+
						"dispatched message — likely a safety-classifier rejection or a runtime API failure, "+
						"not a wedged/dead pane; a plain retry will probably repeat the same rejection. "+
						"Inspect the pane manually, e.g. `tmux capture-pane`, before relying on further retries.)",
					cmd.ID)
			}
			run.Log(core.LogLevelWarn, "R0-dispatch dispatch_deadlock command=%s age_sec=%.0f attempts=%d no_state_file planner_busy=%t",
				cmd.ID, age.Seconds(), cmd.Attempts, plannerBusy)

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

// plannerPaneActivelyProcessing returns true only when a busy probe of the
// Planner pane CONFIRMS the agent is actively processing. R0 uses it to avoid
// reverting a command whose Planner is alive and working but has not yet
// produced a state file. Any non-confirmed outcome — idle, undecided, probe
// error, or no executor factory wired (tests) — returns false so R0 keeps its
// recovery behaviour for a genuinely stuck or dead dispatch. The probe is
// read-only (capture-pane only; no message is sent) and runs outside the queue
// lock.
func plannerPaneActivelyProcessing(run *Run) bool {
	if run.Deps.ExecutorFactory == nil {
		return false
	}
	exec, err := run.Deps.ExecutorFactory(run.Deps.MaestroDir, run.Deps.Config.Watcher, run.Deps.Config.Logging.Level)
	if err != nil || exec == nil {
		return false
	}
	defer func() {
		if cerr := exec.Close(); cerr != nil {
			run.Log(core.LogLevelDebug, "R0-dispatch planner_busy_probe close_executor error=%v", cerr)
		}
	}()
	result := exec.Execute(model.ExecRequest{
		AgentID: "planner",
		Mode:    model.ModeIsBusy,
	})
	// ModeIsBusy: Success==true means confirmed busy. Idle (Success==false)
	// and undecided (Error set, e.g. ErrBusyUndecided) both yield false so the
	// revert proceeds.
	return result.Error == nil && result.Success
}

// plannerPaneShowsAPIError returns true only when a read-only probe of the
// Planner pane finds a visible agent-runtime API error banner (e.g. Claude
// Code's "API Error: ...safeguards flagged..." rejection banner). Purely
// diagnostic: it never changes R0's revert-to-pending decision, it only lets
// the dispatch_deadlock log name the real cause — that the agent rejected or
// failed on the message rather than the pane being wedged or dead — so an
// operator does not have to attach to tmux to learn that a plain retry will
// most likely repeat the same rejection. Any non-confirmed outcome (no
// banner, probe error, no executor factory wired) returns false. The probe
// is read-only (capture-pane only; no message is sent) and runs outside the
// queue lock, mirroring plannerPaneActivelyProcessing.
func plannerPaneShowsAPIError(run *Run) bool {
	if run.Deps.ExecutorFactory == nil {
		return false
	}
	exec, err := run.Deps.ExecutorFactory(run.Deps.MaestroDir, run.Deps.Config.Watcher, run.Deps.Config.Logging.Level)
	if err != nil || exec == nil {
		return false
	}
	defer func() {
		if cerr := exec.Close(); cerr != nil {
			run.Log(core.LogLevelDebug, "R0-dispatch planner_api_error_probe close_executor error=%v", cerr)
		}
	}()
	result := exec.Execute(model.ExecRequest{
		AgentID: "planner",
		Mode:    model.ModeCheckAgentError,
	})
	return result.Error == nil && result.Success
}
