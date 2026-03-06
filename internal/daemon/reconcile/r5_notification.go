package reconcile

import (
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// R5Notification detects results/planner terminal + notified but no orchestrator notification.
// Action: re-issue notification via writeNotificationToOrchestratorQueue.
type R5Notification struct{}

func (R5Notification) Name() string { return "R5" }

func (R5Notification) Apply(run *Run) Outcome {
	var repairs []Repair

	if run.Deps.ResultHandler == nil {
		return Outcome{}
	}

	resultPath := filepath.Join(run.Deps.MaestroDir, "results", "planner.yaml")
	rf, err := run.LoadCommandResultFile(resultPath)
	if err != nil {
		return Outcome{}
	}

	nqPath := filepath.Join(run.Deps.MaestroDir, "queue", "orchestrator.yaml")
	nqData, err := os.ReadFile(nqPath)
	if err != nil && !os.IsNotExist(err) {
		return Outcome{}
	}
	var nq model.NotificationQueue
	if err == nil {
		if err := yamlv3.Unmarshal(nqData, &nq); err != nil {
			return Outcome{}
		}
	}

	existingSourceIDs := make(map[string]bool)
	for _, ntf := range nq.Notifications {
		if ntf.SourceResultID != "" {
			existingSourceIDs[ntf.SourceResultID] = true
		}
	}

	repairedCommands := make(map[string]bool)
	for _, result := range rf.Results {
		if !model.IsTerminal(result.Status) {
			continue
		}
		if !result.Notified {
			continue
		}

		if existingSourceIDs[result.ID] {
			continue
		}

		run.Log(core.LogLevelWarn, "R5 notified_result_no_orchestrator_notification command=%s result=%s",
			result.CommandID, result.ID)

		if err := run.Deps.ResultHandler.WriteNotificationToOrchestratorQueue(result.ID, result.CommandID, result.Status); err != nil {
			run.Log(core.LogLevelError, "R5 write_notification command=%s error=%v", result.CommandID, err)
			continue
		}

		repairedCommands[result.CommandID] = true
		repairs = append(repairs, Repair{
			Pattern:   "R5",
			CommandID: result.CommandID,
			Detail:    fmt.Sprintf("orchestrator notification re-issued for result %s", result.ID),
		})
	}

	for commandID := range repairedCommands {
		run.UpdateLastReconciledAt(commandID)
	}

	return Outcome{Repairs: repairs}
}
