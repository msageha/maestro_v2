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

// R1ResultQueue detects results/ terminal + queue/ in_progress mismatch.
// Action: update queue to terminal, clear lease. Update last_reconciled_at on state file.
type R1ResultQueue struct{}

func (R1ResultQueue) Name() string { return "R1" }

func (R1ResultQueue) Apply(run *Run) Outcome {
	var repairs []Repair
	repairedCommands := make(map[string]bool)

	resultsDir := filepath.Join(run.Deps.MaestroDir, "results")
	entries, err := run.CachedReadDir(resultsDir)
	if err != nil {
		return Outcome{}
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := strings.TrimSuffix(name, ".yaml")
		resultPath := filepath.Join(resultsDir, name)
		queuePath := filepath.Join(run.Deps.MaestroDir, "queue", name)

		resultData, err := os.ReadFile(resultPath)
		if err != nil {
			continue
		}
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(resultData, &rf); err != nil {
			continue
		}

		terminalResults := make(map[string]model.Status)
		for _, result := range rf.Results {
			if model.IsTerminal(result.Status) {
				terminalResults[result.TaskID] = result.Status
			}
		}
		if len(terminalResults) == 0 {
			continue
		}

		func() {
			run.Deps.LockMap.Lock("queue:" + workerID)
			defer run.Deps.LockMap.Unlock("queue:" + workerID)

			queueData, err := os.ReadFile(queuePath)
			if err != nil {
				return
			}
			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(queueData, &tq); err != nil {
				return
			}

			queueModified := false
			var workerRepairs []Repair
			workerRepairedCommands := make(map[string]bool)
			for i := range tq.Tasks {
				task := &tq.Tasks[i]
				if task.Status != model.StatusInProgress {
					continue
				}

				resultStatus, found := terminalResults[task.ID]
				if !found {
					continue
				}

				run.Log(core.LogLevelWarn, "R1 result_terminal_queue_inprogress worker=%s task=%s result_status=%s",
					workerID, task.ID, resultStatus)

				task.Status = resultStatus
				task.LeaseOwner = nil
				task.LeaseExpiresAt = nil
				task.UpdatedAt = run.Deps.Clock.Now().UTC().Format(time.RFC3339)
				queueModified = true
				workerRepairedCommands[task.CommandID] = true

				workerRepairs = append(workerRepairs, Repair{
					Pattern:   "R1",
					CommandID: task.CommandID,
					TaskID:    task.ID,
					Detail:    fmt.Sprintf("queue %s updated from in_progress to %s", workerID, resultStatus),
				})
			}

			if queueModified {
				if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
					run.Log(core.LogLevelError, "R1 write_queue worker=%s error=%v", workerID, err)
					return
				}
				repairs = append(repairs, workerRepairs...)
				for cmdID := range workerRepairedCommands {
					repairedCommands[cmdID] = true
				}
			}
		}()
	}

	for commandID := range repairedCommands {
		run.UpdateLastReconciledAt(commandID)
	}

	return Outcome{Repairs: repairs}
}
