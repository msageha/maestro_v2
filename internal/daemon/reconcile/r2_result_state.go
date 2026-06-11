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

// R2ResultState detects results/worker terminal + state/ non-terminal mismatch.
// Action: update task_states + applied_result_ids in state file.
type R2ResultState struct{}

// Apply detects worker result/state mismatches and updates task states in the command state file.
func (R2ResultState) Apply(run *Run) Outcome {
	var repairs []Repair

	resultsDir := filepath.Join(run.Deps.MaestroDir, "results")
	entries, err := run.cachedReadDir(resultsDir)
	if err != nil {
		return Outcome{}
	}

	type resultEntry struct {
		TaskID   string
		ResultID string
		Status   model.Status
		Worker   string
		// NeedsVerifyStamp marks a RunOnIntegration/RunOnMain completed
		// entry whose VerifyOutcomeAppliedAt is still nil. When R2 applies
		// its terminal status to the state (the §S1-1 crash-recovery
		// bypass — the verify pipeline never took ownership because the
		// daemon crashed between result write and the verify_pending
		// transition), the notify gate's Layer 1 would otherwise defer the
		// Planner notification forever: nothing re-drives verify for a
		// task whose state went straight to completed, and the gate does
		// not count attempts, so the entry never dead-letters either.
		NeedsVerifyStamp bool
	}
	commandResults := make(map[string][]resultEntry)

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		resultPath := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(resultPath) //nolint:gosec // resultPath is constructed from a controlled application results directory
		if err != nil {
			run.Log(core.LogLevelWarn, "R2 read_result_file file=%s error=%v", name, err)
			continue
		}
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			run.Log(core.LogLevelWarn, "R2 parse_result_file file=%s error=%v", name, err)
			continue
		}

		worker := strings.TrimSuffix(name, ".yaml")
		for _, result := range rf.Results {
			if model.IsTerminal(result.Status) {
				commandResults[result.CommandID] = append(commandResults[result.CommandID], resultEntry{
					TaskID:   result.TaskID,
					ResultID: result.ID,
					Status:   result.Status,
					Worker:   worker,
					NeedsVerifyStamp: (result.RunOnIntegration || result.RunOnMain) &&
						result.Status == model.StatusCompleted &&
						(result.VerifyOutcomeAppliedAt == nil || *result.VerifyOutcomeAppliedAt == ""),
				})
			}
		}
	}

	for commandID, results := range commandResults {
		statePath := filepath.Join(run.Deps.MaestroDir, "state", "commands", commandID+".yaml")

		var commandRepairs []Repair
		// stampEntries depend on the R2 state write succeeding; stampOnly
		// entries reflect state that is already terminal on disk and are
		// stamped regardless of this pass's save outcome.
		var stampEntries, stampOnly []resultEntry
		run.Deps.LockMap.WithLock("state:"+commandID, func() {
			state, err := run.loadState(statePath)
			if err != nil {
				return
			}

			if state.TaskStates == nil {
				state.TaskStates = make(map[string]model.Status)
			}
			if state.AppliedResultIDs == nil {
				state.AppliedResultIDs = make(map[string]string)
			}

			modified := false
			for _, re := range results {
				currentStatus, exists := state.TaskStates[re.TaskID]
				if !exists {
					run.Log(core.LogLevelWarn, "R2 skip_unknown_task command=%s task=%s result_status=%s (task not registered in state)",
						commandID, re.TaskID, re.Status)
					continue
				}
				if model.IsTerminal(currentStatus) {
					// Stamp retry: applyVerifyOutcome may have advanced the
					// state to completed but failed its best-effort
					// markVerifyOutcomeAppliedOnResult write. The notify
					// gate's verify-pipeline layer checks ONLY the result
					// entry's stamp (it never falls back to state), so a
					// missing stamp on an already-terminal task would defer
					// the Planner notification forever. Only stamp when the
					// state's applied result matches this entry (or none is
					// recorded) to avoid blessing a stale entry.
					if re.NeedsVerifyStamp && currentStatus == model.StatusCompleted {
						if applied, ok := state.AppliedResultIDs[re.TaskID]; !ok || applied == re.ResultID {
							stampOnly = append(stampOnly, re)
						}
					}
					continue
				}
				// Verification pipeline ownership guard.
				//
				// verify_pending is the state machine slot for "worker
				// completed; quality gate is in flight". The result file
				// shows the worker's reported status (typically completed
				// or failed), so naively comparing
				// "result terminal vs state non-terminal" would misclassify
				// verify_pending as a stuck task and overwrite it with the
				// worker's reported terminal status — racing the async
				// verify runner and silently dropping the verify result.
				// The same failure mode applies to repair_pending: that
				// slot is owned by the post-verify repair task path.
				//
				// R9_VerifyStall is the dedicated reconciler for stalled
				// verify_pending entries; it transitions to repair_pending
				// once the configured stall threshold elapses, so verify
				// pipeline state never gets stuck even with R2 stepping
				// back here.
				if currentStatus == model.StatusVerifyPending || currentStatus == model.StatusRepairPending {
					run.Log(core.LogLevelDebug,
						"R2 skip_verify_pipeline command=%s task=%s result_status=%s state_status=%s "+
							"(verification/repair pipeline owns this slot — applyVerifyOutcome / R9_VerifyStall handle it)",
						commandID, re.TaskID, re.Status, currentStatus)
					continue
				}
				// paused_for_replan is a Planner-handoff slot (Circuit
				// Breaker / max_repair exhaustion). The worker may still emit
				// a terminal result file for the original attempt — typically
				// the failed result that *triggered* the replan in the first
				// place — but accepting it here would silently flip the task
				// back to completed/failed and erase the Planner's "must
				// replan" hint. The repair-pipeline guard above protects
				// only the pre-replan slots; this clause covers the
				// post-replan slot so Planner replan can run to completion.
				if currentStatus == model.StatusPausedForReplan {
					run.Log(core.LogLevelDebug,
						"R2 skip_paused_for_replan command=%s task=%s result_status=%s state_status=%s "+
							"(awaiting Planner replan — original-attempt result must not overwrite this slot)",
						commandID, re.TaskID, re.Status, currentStatus)
					continue
				}

				run.Log(core.LogLevelWarn, "R2 result_terminal_state_nonterminal command=%s task=%s result_status=%s state_status=%s",
					commandID, re.TaskID, re.Status, currentStatus)

				state.TaskStates[re.TaskID] = re.Status
				state.AppliedResultIDs[re.TaskID] = re.ResultID
				modified = true
				if re.NeedsVerifyStamp {
					stampEntries = append(stampEntries, re)
				}

				commandRepairs = append(commandRepairs, Repair{
					Pattern:   PatternR2,
					CommandID: commandID,
					TaskID:    re.TaskID,
					Detail:    fmt.Sprintf("state task_states updated from %s to %s", currentStatus, re.Status),
				})
			}

			if modified {
				now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
				state.LastReconciledAt = &now
				state.UpdatedAt = now
				if err := yamlutil.AtomicWrite(statePath, state); err != nil {
					run.Log(core.LogLevelError, "R2 write_state command=%s error=%v", commandID, err)
					commandRepairs = nil
					stampEntries = nil
				}
			}
		})
		repairs = append(repairs, commandRepairs...)

		// Release the notify gate for entries whose state R2 just completed
		// without the verify pipeline (crash-recovery supersede), and for
		// already-terminal entries whose earlier stamp write failed.
		for _, re := range append(stampEntries, stampOnly...) {
			r2StampVerifyOutcomeApplied(run, re.Worker, re.TaskID, commandID)
		}
	}

	return Outcome{Repairs: repairs}
}

// r2StampVerifyOutcomeApplied stamps VerifyOutcomeAppliedAt on the worker's
// result entry after R2 applied its terminal status to the state without the
// verify pipeline (the daemon crashed between Phase A's result write and
// Phase B's verify_pending transition, so applyVerifyOutcome will never run
// for this entry). Without the stamp, the notify gate's verify-pipeline
// layer defers the Planner notification on every scan, silently and forever.
// Best-effort: a failed stamp is retried by the next reconcile pass.
func r2StampVerifyOutcomeApplied(run *Run, workerID, taskID, commandID string) {
	resultPath := filepath.Join(run.Deps.MaestroDir, "results", workerID+".yaml")
	run.Deps.LockMap.WithLock("result:"+workerID, func() {
		data, err := os.ReadFile(resultPath) //nolint:gosec // controlled application results directory
		if err != nil {
			run.Log(core.LogLevelWarn, "R2 verify_stamp_read_failed worker=%s task=%s error=%v", workerID, taskID, err)
			return
		}
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			run.Log(core.LogLevelWarn, "R2 verify_stamp_parse_failed worker=%s task=%s error=%v", workerID, taskID, err)
			return
		}
		now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
		mutated := false
		for i := range rf.Results {
			r := &rf.Results[i]
			if r.TaskID != taskID || r.CommandID != commandID {
				continue
			}
			if r.VerifyOutcomeAppliedAt != nil && *r.VerifyOutcomeAppliedAt != "" {
				continue
			}
			stamp := now
			r.VerifyOutcomeAppliedAt = &stamp
			mutated = true
		}
		if !mutated {
			return
		}
		if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
			run.Log(core.LogLevelWarn, "R2 verify_stamp_write_failed worker=%s task=%s error=%v", workerID, taskID, err)
			return
		}
		run.Log(core.LogLevelInfo,
			"R2 verify_outcome_stamped worker=%s task=%s command=%s "+
				"(crash-recovery: state completed without verify pipeline; notify gate released)",
			workerID, taskID, commandID)
	})
}
