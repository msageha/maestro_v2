package reconcile

import (
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// R3PlannerQueue detects results/planner terminal + queue/planner in_progress mismatch.
// Action: update queue to terminal, clear lease. Update last_reconciled_at on state file.
type R3PlannerQueue struct{}

// Apply detects planner result/queue terminal-in_progress mismatches and corrects queue state.
func (R3PlannerQueue) Apply(run *Run) Outcome {
	resultPath := filepath.Join(run.Deps.MaestroDir, "results", "planner.yaml")
	rf, err := run.loadCommandResultFile(resultPath)
	if err != nil {
		return Outcome{}
	}

	terminalResults := make(map[string]terminalResultInfo)
	for _, result := range rf.Results {
		if model.IsTerminal(result.Status) {
			terminalResults[result.CommandID] = terminalResultInfo{Status: result.Status, CreatedAt: result.CreatedAt}
		}
	}
	if len(terminalResults) == 0 {
		return Outcome{}
	}

	queuePath := commandQueuePath(run.Deps.MaestroDir)
	repairs, repairedCommands, err := reconcileTerminalQueue(
		run, PatternR3, "planner", queuePath, terminalResults,
		unmarshalCommandQueue, setCommandQueueItems, commandQueueAccessor(),
	)
	if err != nil {
		run.Log(core.LogLevelError, "R3 reconcile_terminal_queue error=%v", err)
		return Outcome{}
	}

	for commandID := range repairedCommands {
		run.updateLastReconciledAt(commandID)
	}

	return Outcome{Repairs: repairs}
}
