package reconcile

import (
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// notificationTypeForStatus mirrors ResultHandler.writeNotificationToOrchestratorQueue.
// Kept in sync so R5's dedup key matches the type the orchestrator-side writer would
// assign for a given terminal status.
func notificationTypeForStatus(status model.Status) model.NotificationType {
	switch status {
	case model.StatusFailed:
		return model.NotificationTypeCommandFailed
	case model.StatusCancelled:
		return model.NotificationTypeCommandCancelled
	default:
		return model.NotificationTypeCommandCompleted
	}
}

// R5Notification detects results/planner terminal + notified but no orchestrator notification.
// Action: re-issue notification via writeNotificationToOrchestratorQueue.
type R5Notification struct{}

// Apply detects notified planner results without a corresponding orchestrator notification
// and re-issues the notification via WriteNotificationToOrchestratorQueue.
func (R5Notification) Apply(run *Run) Outcome {
	if run.Deps.ResultHandler == nil {
		return Outcome{}
	}

	resultPath := filepath.Join(run.Deps.MaestroDir, "results", "planner.yaml")
	rf, err := run.loadCommandResultFile(resultPath)
	if err != nil {
		return Outcome{}
	}

	nqPath := filepath.Join(run.Deps.MaestroDir, "queue", "orchestrator.yaml")
	nqData, err := os.ReadFile(nqPath) //nolint:gosec // nqPath is constructed from a controlled application queue directory
	if err != nil && !os.IsNotExist(err) {
		return Outcome{}
	}
	var nq model.NotificationQueue
	if err == nil {
		if err := yamlv3.Unmarshal(nqData, &nq); err != nil {
			return Outcome{}
		}
	}

	// Dedup key is (source_result_id, type) to mirror the upsert logic in
	// ResultHandler.writeNotificationToOrchestratorQueue. H3 reconcile may
	// promote a result to a different terminal status (e.g. completed →
	// cancelled), in which case the existing notification has the previous
	// type and we must re-issue so the orchestrator-side handler supersedes
	// it. Keying on source_result_id alone would drop that re-delivery.
	type dedupKey struct {
		SourceResultID string
		Type           model.NotificationType
	}
	existingKeys := make(map[dedupKey]bool)
	for _, ntf := range nq.Notifications {
		if ntf.SourceResultID == "" {
			continue
		}
		existingKeys[dedupKey{SourceResultID: ntf.SourceResultID, Type: ntf.Type}] = true
	}

	repairs := make([]Repair, 0, len(rf.Results))
	repairedCommands := make(map[string]bool)
	for _, result := range rf.Results {
		if !model.IsTerminal(result.Status) {
			continue
		}
		if !result.Notified {
			continue
		}

		notifType := notificationTypeForStatus(result.Status)
		if existingKeys[dedupKey{SourceResultID: result.ID, Type: notifType}] {
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
			Pattern:   PatternR5,
			CommandID: result.CommandID,
			Detail:    fmt.Sprintf("orchestrator notification re-issued for result %s", result.ID),
		})
	}

	for commandID := range repairedCommands {
		run.updateLastReconciledAt(commandID)
	}

	return Outcome{Repairs: repairs}
}
