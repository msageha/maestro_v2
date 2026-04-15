package daemon

import (
	"fmt"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func (h *QueueWriteAPI) handleQueueWriteNotification(params QueueWriteParams) *uds.Response {
	if resp := validateRequired(params.CommandID, "command_id", "notification"); resp != nil {
		return resp
	}
	if resp := validateRequired(params.Content, "content", "notification"); resp != nil {
		return resp
	}
	if resp := validateRequired(params.SourceResultID, "source_result_id", "notification"); resp != nil {
		return resp
	}

	// notification_type validation
	if resp := validateRequired(params.NotificationType, "notification_type", "notification"); resp != nil {
		return resp
	}
	notifType := model.NotificationType(params.NotificationType)
	if !validNotificationTypes[notifType] {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("invalid notification_type: %q, must be command_completed|command_failed|command_cancelled", notifType))
	}

	if resp := validateContentSize(params.Content, h.config.Limits.MaxEntryContentBytes); resp != nil {
		return resp
	}

	return h.withQueueLocks([]string{"queue:orchestrator"}, func() *uds.Response {
		return h.executeNotificationWrite(params, notifType)
	})
}

// executeNotificationWrite performs the locked portion of notification queue write.
func (h *QueueWriteAPI) executeNotificationWrite(params QueueWriteParams, notifType model.NotificationType) *uds.Response {
	queuePath := notificationQueuePath(h.maestroDir)
	nq, data, err := loadNotificationQueueFile(queuePath)
	if err != nil {
		return internalErrorf("%v", err)
	}

	// Idempotency: dedup key is (source_result_id, type). When a re-delivery
	// arrives with the same source_result_id but a different notification type
	// (e.g. H3 reconcile forced a previously-completed result to cancelled),
	// supersede the existing entry in place: reset delivery state so the
	// orchestrator picks it up again. ID/CreatedAt/Priority are preserved.
	now := h.clock.Now().UTC().Format(time.RFC3339)
	for i := range nq.Notifications {
		if nq.Notifications[i].SourceResultID != params.SourceResultID {
			continue
		}
		if nq.Notifications[i].Type == notifType {
			h.logFn(LogLevelInfo, "queue_write type=notification duplicate source_result_id=%s existing_id=%s", params.SourceResultID, nq.Notifications[i].ID)
			return uds.SuccessResponse(map[string]string{"id": nq.Notifications[i].ID, "duplicate": "true"})
		}
		h.logFn(LogLevelInfo, "queue_write type=notification supersede source_result_id=%s old_type=%s new_type=%s existing_id=%s",
			params.SourceResultID, nq.Notifications[i].Type, notifType, nq.Notifications[i].ID)
		nq.Notifications[i].Type = notifType
		nq.Notifications[i].Content = params.Content
		nq.Notifications[i].Status = model.StatusPending
		nq.Notifications[i].Attempts = 0
		nq.Notifications[i].LastError = nil
		nq.Notifications[i].DeadLetteredAt = nil
		nq.Notifications[i].DeadLetterReason = nil
		nq.Notifications[i].LeaseOwner = nil
		nq.Notifications[i].LeaseExpiresAt = nil
		nq.Notifications[i].UpdatedAt = now
		if err := yamlutil.AtomicWrite(queuePath, nq); err != nil {
			return internalErrorf("write queue: %v", err)
		}
		h.notifySelfWrite(queuePath, "notification", nq)
		return uds.SuccessResponse(map[string]string{"id": nq.Notifications[i].ID, "superseded": "true"})
	}

	if resp := checkFileSizeLimit(h.config.Limits.MaxYAMLFileBytes, len(data), len(params.Content)+300); resp != nil {
		archived := archiveTerminalNotifications(&nq)
		if archived > 0 {
			newData, marshalErr := yamlv3.Marshal(nq)
			if marshalErr != nil {
				return internalErrorf("marshal queue after archive: %v", marshalErr)
			}
			if checkFileSizeLimit(h.config.Limits.MaxYAMLFileBytes, len(newData), len(params.Content)+300) != nil {
				return resp
			}
			h.logFn(LogLevelInfo, "queue_write archive_notifications archived=%d", archived)
		} else {
			return resp
		}
	}

	id, err := model.GenerateID(model.IDTypeNotification)
	if err != nil {
		return internalErrorf("generate ID: %v", err)
	}

	priority := params.Priority
	if priority == 0 {
		priority = model.DefaultPriority
	}

	nq.Notifications = append(nq.Notifications, model.Notification{
		ID:             id,
		CommandID:      params.CommandID,
		Type:           notifType,
		SourceResultID: params.SourceResultID,
		Content:        params.Content,
		Priority:       priority,
		Status:         model.StatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	})

	if err := yamlutil.AtomicWrite(queuePath, nq); err != nil {
		return internalErrorf("write queue: %v", err)
	}
	h.notifySelfWrite(queuePath, "notification", nq)

	h.logFn(LogLevelInfo, "queue_write type=notification id=%s command_id=%s source_result_id=%s", id, params.CommandID, params.SourceResultID)
	return uds.SuccessResponse(map[string]string{"id": id})
}
