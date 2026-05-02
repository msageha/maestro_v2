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

		for _, result := range rf.Results {
			if model.IsTerminal(result.Status) {
				commandResults[result.CommandID] = append(commandResults[result.CommandID], resultEntry{
					TaskID:   result.TaskID,
					ResultID: result.ID,
					Status:   result.Status,
				})
			}
		}
	}

	for commandID, results := range commandResults {
		statePath := filepath.Join(run.Deps.MaestroDir, "state", "commands", commandID+".yaml")

		var commandRepairs []Repair
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
				}
			}
		})
		repairs = append(repairs, commandRepairs...)
	}

	return Outcome{Repairs: repairs}
}
