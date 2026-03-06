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

func (R2ResultState) Name() string { return "R2" }

func (R2ResultState) Apply(run *Run) Outcome {
	var repairs []Repair

	resultsDir := filepath.Join(run.Deps.MaestroDir, "results")
	entries, err := run.CachedReadDir(resultsDir)
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
		data, err := os.ReadFile(resultPath)
		if err != nil {
			continue
		}
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
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

		lockKey := "state:" + commandID
		commandRepairs := func() []Repair {
			run.Deps.LockMap.Lock(lockKey)
			defer run.Deps.LockMap.Unlock(lockKey)

			state, err := run.LoadState(statePath)
			if err != nil {
				return nil
			}

			if state.TaskStates == nil {
				state.TaskStates = make(map[string]model.Status)
			}
			if state.AppliedResultIDs == nil {
				state.AppliedResultIDs = make(map[string]string)
			}

			modified := false
			var reps []Repair
			for _, re := range results {
				currentStatus, exists := state.TaskStates[re.TaskID]
				if exists && model.IsTerminal(currentStatus) {
					continue
				}

				run.Log(core.LogLevelWarn, "R2 result_terminal_state_nonterminal command=%s task=%s result_status=%s state_status=%s",
					commandID, re.TaskID, re.Status, currentStatus)

				state.TaskStates[re.TaskID] = re.Status
				state.AppliedResultIDs[re.TaskID] = re.ResultID
				modified = true

				reps = append(reps, Repair{
					Pattern:   "R2",
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
					return nil
				}
			}
			return reps
		}()
		repairs = append(repairs, commandRepairs...)
	}

	return Outcome{Repairs: repairs}
}
