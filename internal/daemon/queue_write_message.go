package daemon

import (
	"fmt"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// handleQueueWriteMessage enqueues a free-form user message for the
// Orchestrator (queue_write type=message). The message rides the existing
// orchestrator notification queue (type=user_message) so it inherits the
// same lease / retry / dead-letter machinery as command notifications, but:
//   - it has no backing result file — the content is delivered in the
//     envelope itself (BuildOrchestratorUserMessageEnvelope);
//   - it deliberately bypasses checkContinuousGate: a paused/stopped
//     continuous run is exactly when the user needs to reach the
//     Orchestrator from outside the tmux pane.
func (h *QueueWriteAPI) handleQueueWriteMessage(params QueueWriteParams) *uds.Response {
	// The Orchestrator is the only user-message consumer. Reject other
	// targets at the daemon too (the CLI already validates) so non-CLI
	// callers get an immediate error instead of a silently rerouted write.
	if params.Target != "orchestrator" {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("type=message requires target=orchestrator (the only user-message consumer); got target=%q", params.Target))
	}
	if resp := validateRequired(params.Content, "content", "message"); resp != nil {
		return resp
	}
	if resp := validateContentSize(params.Content, h.config.Limits.MaxEntryContentBytes); resp != nil {
		return resp
	}

	return h.withQueueLocks([]string{"queue:orchestrator"}, func() *uds.Response {
		return h.executeMessageWrite(params)
	})
}

// executeMessageWrite performs the locked portion of user-message queue write.
func (h *QueueWriteAPI) executeMessageWrite(params QueueWriteParams) *uds.Response {
	queuePath := notificationQueuePath(h.maestroDir)
	nq, data, err := loadNotificationQueueFile(queuePath)
	if err != nil {
		return internalErrorf("%v", err)
	}

	if resp := ensureCapacityWithArchive(
		h.config.Limits.MaxYAMLFileBytes, data, len(params.Content)+300,
		func() int { return archiveTerminalNotifications(&nq) },
		func() ([]byte, error) { return yamlv3.Marshal(nq) },
		func(f string, a ...any) { h.logFn(LogLevelInfo, f, a...) },
		"notifications",
	); resp != nil {
		return resp
	}

	id, err := model.GenerateID(model.IDTypeNotification)
	if err != nil {
		return internalErrorf("generate ID: %v", err)
	}

	now := formatNowUTC(h.clock)
	nq.Notifications = append(nq.Notifications, model.Notification{
		ID:   id,
		Type: model.NotificationTypeUserMessage,
		// Every user message is a distinct delivery: synthesise a unique
		// SourceResultID from the entry ID so the (source_result_id, type)
		// dedup/supersede logic used elsewhere in the queue never collides.
		SourceResultID: "user:" + id,
		Content:        params.Content,
		Priority:       resolvePriority(params.Priority),
		Status:         model.StatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	})

	if resp := h.writeAndNotify(queuePath, "notification", nq); resp != nil {
		return resp
	}

	h.logFn(LogLevelInfo, "queue_write type=message id=%s", id)
	return uds.SuccessResponse(map[string]string{"id": id})
}
