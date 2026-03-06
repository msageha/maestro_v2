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

// R3PlannerQueue detects results/planner terminal + queue/planner in_progress mismatch.
// Action: update queue to terminal, clear lease. Update last_reconciled_at on state file.
type R3PlannerQueue struct{}

func (R3PlannerQueue) Name() string { return "R3" }

func (R3PlannerQueue) Apply(run *Run) Outcome {
	var repairs []Repair
	repairedCommands := make(map[string]bool)

	resultPath := filepath.Join(run.Deps.MaestroDir, "results", "planner.yaml")
	rf, err := run.LoadCommandResultFile(resultPath)
	if err != nil {
		return Outcome{}
	}

	terminalResults := make(map[string]model.Status)
	for _, result := range rf.Results {
		if model.IsTerminal(result.Status) {
			terminalResults[result.CommandID] = result.Status
		}
	}
	if len(terminalResults) == 0 {
		return Outcome{}
	}

	queuePath := filepath.Join(run.Deps.MaestroDir, "queue", "planner.yaml")
	func() {
		run.Deps.LockMap.Lock("queue:planner")
		defer run.Deps.LockMap.Unlock("queue:planner")

		queueData, err := os.ReadFile(queuePath)
		if err != nil {
			return
		}
		var cq model.CommandQueue
		if err := yamlv3.Unmarshal(queueData, &cq); err != nil {
			return
		}

		queueModified := false
		var localRepairs []Repair
		localRepairedCommands := make(map[string]bool)
		for i := range cq.Commands {
			cmd := &cq.Commands[i]
			if model.IsTerminal(cmd.Status) {
				continue
			}

			resultStatus, found := terminalResults[cmd.ID]
			if !found {
				continue
			}

			run.Log(core.LogLevelWarn, "R3 result_terminal_queue_nonterminal command=%s queue_status=%s result_status=%s",
				cmd.ID, cmd.Status, resultStatus)

			cmd.Status = resultStatus
			cmd.LeaseOwner = nil
			cmd.LeaseExpiresAt = nil
			cmd.UpdatedAt = run.Deps.Clock.Now().UTC().Format(time.RFC3339)
			queueModified = true
			localRepairedCommands[cmd.ID] = true

			localRepairs = append(localRepairs, Repair{
				Pattern:   "R3",
				CommandID: cmd.ID,
				Detail:    fmt.Sprintf("queue planner updated from in_progress to %s", resultStatus),
			})
		}

		if queueModified {
			if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
				run.Log(core.LogLevelError, "R3 write_queue error=%v", err)
				return
			}
			repairs = append(repairs, localRepairs...)
			for cmdID := range localRepairedCommands {
				repairedCommands[cmdID] = true
			}
		}
	}()

	for commandID := range repairedCommands {
		run.UpdateLastReconciledAt(commandID)
	}

	return Outcome{Repairs: repairs}
}
