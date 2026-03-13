package reconcile

import (
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/model"
)

// R3PlannerQueue detects results/planner terminal + queue/planner in_progress mismatch.
// Action: update queue to terminal, clear lease. Update last_reconciled_at on state file.
type R3PlannerQueue struct{}

func (R3PlannerQueue) Apply(run *Run) Outcome {
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
	repairs, repairedCommands := reconcileTerminalQueue(
		run, PatternR3, "planner", queuePath, terminalResults,
		unmarshalCommandQueue, setCommandQueueItems, commandQueueAccessor(),
	)

	for commandID := range repairedCommands {
		run.UpdateLastReconciledAt(commandID)
	}

	return Outcome{Repairs: repairs}
}
