package daemon

// Notification delivery and dedup helpers extracted from result_handler.go
// (F-042 step 4 physical file split). Covers two responsibilities that share
// the (source_result_id, type) dedup contract:
//   - delivering Worker → Planner notifications via the agent executor.
//   - writing Planner / dead-letter notifications into queue/orchestrator.yaml
//     with idempotency guarantees.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/envelope"
	"github.com/msageha/maestro_v2/internal/model"
)

// notifyPlannerOfWorkerResultWithRetry attempts delivery to the planner with inline retries.
// On transient failure (e.g., planner busy during recovery), retries up to
// ResultNotifyInlineRetries times with a short delay between attempts, each bounded
// by ResultNotifyDeliveryTimeoutSec. This mirrors deliverPlannerSignal's inline retry
// pattern to avoid blocking for the full busy detection cycle on a single attempt.
func (rh *ResultHandler) notifyPlannerOfWorkerResultWithRetry(commandID, taskID, workerID, taskStatus string) error {
	maxRetries := rh.config.Retry.EffectiveResultNotifyInlineRetries()
	retryDelay := time.Duration(rh.config.Retry.EffectiveResultNotifyInlineRetryDelaySec()) * time.Second
	attemptTimeout := time.Duration(rh.config.Retry.EffectiveResultNotifyDeliveryTimeoutSec()) * time.Second

	parent := rh.parentContext()
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			rh.log(LogLevelInfo, "result_notify_inline_retry attempt=%d/%d task=%s command=%s error=%v",
				attempt+1, maxRetries+1, taskID, commandID, lastErr)
			select {
			case <-parent.Done():
				return fmt.Errorf("result notify aborted during retry: %w", parent.Err())
			case <-time.After(retryDelay):
			}
		}

		if err := parent.Err(); err != nil {
			return fmt.Errorf("result notify aborted: %w", err)
		}
		ctx, cancel := context.WithTimeout(parent, attemptTimeout)
		err := rh.notifyPlannerOfWorkerResult(ctx, commandID, taskID, workerID, taskStatus)
		cancel()

		if err == nil {
			if attempt > 0 {
				rh.log(LogLevelInfo, "result_notify_inline_retry_success task=%s command=%s total_attempts=%d",
					taskID, commandID, attempt+1)
			}
			return nil
		}
		lastErr = err
		// ErrSubmitConfirmUncertain is non-retryable by design (mirrors
		// deliverPlannerSignal's behaviour in queue_dispatch.go): the
		// underlying deliverer already burned its 8-attempt probe
		// budget (~6s) trying to confirm the paste landed in the
		// Planner pane and gave up. Re-pasting the same envelope risks
		// a duplicate task_result notification on top of the original
		// message, while continuing to wait simply multiplies the
		// scan-blocking window for no semantic gain. Surface the error
		// immediately so the per-result NotifyAttempts counter
		// advances and the dead-letter path engages instead of pinning
		// scan cycles to the same exhausted delivery for the full
		// inline-retry budget.
		if errors.Is(err, agent.ErrSubmitConfirmUncertain) {
			return err
		}
	}
	return lastErr
}

// notifyPlannerOfWorkerResult sends a task_result notification to Planner via agent_executor.
func (rh *ResultHandler) notifyPlannerOfWorkerResult(ctx context.Context, commandID, taskID, workerID, taskStatus string) error {
	exec, err := rh.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	message := envelope.BuildTaskResultNotificationWithMaestroDir(commandID, taskID, workerID, taskStatus, rh.maestroDir)

	result := exec.Execute(agent.ExecRequest{
		Context:   ctx,
		AgentID:   "planner",
		Message:   message,
		Mode:      agent.ModeDeliver,
		TaskID:    taskID,
		CommandID: commandID,
	})

	if result.Error != nil {
		return result.Error
	}
	return nil
}

// notifyOrchestratorOfCommandResult writes a notification to queue/orchestrator.yaml.
func (rh *ResultHandler) notifyOrchestratorOfCommandResult(resultID, commandID string, status model.Status) error {
	if err := rh.writeNotificationToOrchestratorQueue(resultID, commandID, status); err != nil {
		return fmt.Errorf("write orchestrator notification: %w", err)
	}
	return nil
}

// writeNotificationToOrchestratorQueue directly writes a notification to queue/orchestrator.yaml.
// Uses source_result_id idempotency to prevent duplicate notifications.
func (rh *ResultHandler) writeNotificationToOrchestratorQueue(resultID, commandID string, status model.Status) error {
	queuePath := notificationQueuePath(rh.maestroDir)

	// Serialize access to orchestrator queue file to prevent lost-update races
	// between fsnotify event path (no fileMu) and PeriodicScan path.
	rh.lockMap.Lock("queue:orchestrator")
	defer rh.lockMap.Unlock("queue:orchestrator")

	return updateYAMLFile(queuePath, func(nq *model.NotificationQueue) error {
		if nq.SchemaVersion == 0 {
			nq.SchemaVersion = 1
			nq.FileType = "queue_notification"
		}
		notifType := notificationTypeForStatus(status)
		now := rh.clock.Now().UTC().Format(time.RFC3339)
		content := fmt.Sprintf("command %s %s", commandID, status)

		switch rh.supersedeOrSkipNotification(nq.Notifications, resultID, notifType, content, now) {
		case notificationActionDedup:
			return errNoUpdate
		case notificationActionSuperseded:
			return nil
		}
		return appendNewNotification(nq, commandID, resultID, notifType, content, now)
	})
}

// notificationAction is the 3-state outcome of the dedup/supersede helper
// used by writeNotificationToOrchestratorQueue. F-042 step 3 helper.
type notificationAction int

const (
	notificationActionAppend     notificationAction = iota // no existing entry; caller appends
	notificationActionSuperseded                           // existing entry mutated in place; write needed
	notificationActionDedup                                // exact duplicate; caller MUST signal errNoUpdate
)

// notificationTypeForStatus maps a terminal task / command status to the
// orchestrator notification type. Defaults to CommandCompleted; unknown
// terminal states fall through, preserving prior behaviour. F-042 step 3
// helper extracted from writeNotificationToOrchestratorQueue.
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

// indexNotificationBySourceResult returns the index of the first
// notification whose SourceResultID matches resultID, or -1 if none.
// F-042 step 3 helper.
func indexNotificationBySourceResult(notifs []model.Notification, resultID string) int {
	for i := range notifs {
		if notifs[i].SourceResultID == resultID {
			return i
		}
	}
	return -1
}

// supersedeOrSkipNotification implements the (source_result_id, type) dedup
// key for orchestrator notifications and returns the 3-state action the
// caller must take.
//
// Behaviour:
//   - same resultID and same type → debug log, return notificationActionDedup
//     (caller maps to errNoUpdate so updateYAMLFile skips the write).
//   - same resultID but different type (H3 reconcile changed the terminal
//     status) → mutate the entry in place, reset delivery state, return
//     notificationActionSuperseded. ID / CreatedAt / Priority are
//     preserved so downstream observers see the same notification identity.
//   - no match → return notificationActionAppend; caller appends a fresh entry.
//
// F-042 step 3 helper extracted from writeNotificationToOrchestratorQueue.
func (rh *ResultHandler) supersedeOrSkipNotification(
	notifs []model.Notification,
	resultID string,
	notifType model.NotificationType,
	content, now string,
) notificationAction {
	i := indexNotificationBySourceResult(notifs, resultID)
	if i < 0 {
		return notificationActionAppend
	}
	if notifs[i].Type == notifType {
		rh.log(LogLevelDebug, "orchestrator_notification_duplicate source_result_id=%s", resultID)
		return notificationActionDedup
	}
	rh.log(LogLevelInfo, "orchestrator_notification_supersede source_result_id=%s old_type=%s new_type=%s existing_id=%s",
		resultID, notifs[i].Type, notifType, notifs[i].ID)
	notifs[i].Type = notifType
	notifs[i].Content = content
	notifs[i].Status = model.StatusPending
	notifs[i].Attempts = 0
	notifs[i].LastError = nil
	notifs[i].DeadLetteredAt = nil
	notifs[i].DeadLetterReason = nil
	notifs[i].LeaseOwner = nil
	notifs[i].LeaseExpiresAt = nil
	notifs[i].UpdatedAt = now
	return notificationActionSuperseded
}

// appendNewNotification creates a fresh model.Notification with a generated
// ID and appends it to nq. F-042 step 3 helper.
func appendNewNotification(
	nq *model.NotificationQueue,
	commandID, resultID string,
	notifType model.NotificationType,
	content, now string,
) error {
	id, err := model.GenerateID(model.IDTypeNotification)
	if err != nil {
		return fmt.Errorf("generate notification ID: %w", err)
	}
	nq.Notifications = append(nq.Notifications, model.Notification{
		ID:             id,
		CommandID:      commandID,
		Type:           notifType,
		SourceResultID: resultID,
		Content:        content,
		Priority:       defaultNotificationPriority,
		Status:         model.StatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	return nil
}

// WriteNotificationToOrchestratorQueue is the exported wrapper for writeNotificationToOrchestratorQueue.
// Used by the reconcile package via the ResultNotifier interface.
func (rh *ResultHandler) WriteNotificationToOrchestratorQueue(resultID, commandID string, status model.Status) error {
	return rh.writeNotificationToOrchestratorQueue(resultID, commandID, status)
}

// emitResultDeadLetterNotification writes a one-shot notification into
// queue/orchestrator.yaml telling the Orchestrator that a result (task or
// command) has been dead-lettered because notification retries were exhausted.
// Dedup key: a synthetic SourceResultID derived from resultID to survive
// repeated sweep passes without emitting duplicates.
func (rh *ResultHandler) emitResultDeadLetterNotification(resultID, commandID, content string) error {
	queuePath := notificationQueuePath(rh.maestroDir)
	sourceID := "result_dl_" + resultID

	rh.lockMap.Lock("queue:orchestrator")
	defer rh.lockMap.Unlock("queue:orchestrator")

	return updateYAMLFile(queuePath, func(nq *model.NotificationQueue) error {
		if nq.SchemaVersion == 0 {
			nq.SchemaVersion = 1
			nq.FileType = "queue_notification"
		}

		for i := range nq.Notifications {
			if nq.Notifications[i].SourceResultID == sourceID &&
				nq.Notifications[i].Type == model.NotificationTypeCommandFailed {
				return errNoUpdate
			}
		}

		id, err := model.GenerateID(model.IDTypeNotification)
		if err != nil {
			return fmt.Errorf("generate notification ID: %w", err)
		}
		now := rh.clock.Now().UTC().Format(time.RFC3339)
		nq.Notifications = append(nq.Notifications, model.Notification{
			ID:             id,
			CommandID:      commandID,
			Type:           model.NotificationTypeCommandFailed,
			SourceResultID: sourceID,
			Content:        content,
			Priority:       defaultNotificationPriority,
			Status:         model.StatusPending,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
		return nil
	})
}
