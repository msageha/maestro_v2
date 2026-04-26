package daemon

import (
	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
)

func (qh *QueueHandler) resetScanTimeCache() {
	qh.timeCache.Reset()
}

func (qh *QueueHandler) newScanState(counters *metrics.ScanCounters) scanState {
	scanStart := qh.clock.Now()
	*counters = metrics.ScanCounters{}

	commandQueue, commandPath, err := qh.queueStore.LoadCommandQueue()
	if err != nil {
		qh.log(LogLevelError, "load_command_queue_failed error=%v", err)
	}
	taskQueues, err := qh.queueStore.LoadAllTaskQueues()
	if err != nil {
		qh.log(LogLevelError, "load_task_queues_failed error=%v", err)
	}
	notificationQueue, notificationPath, err := qh.queueStore.LoadNotificationQueue()
	if err != nil {
		qh.log(LogLevelError, "load_notification_queue_failed error=%v", err)
	}
	signalQueue, signalPath, err := qh.queueStore.LoadPlannerSignalQueue()
	if err != nil {
		qh.log(LogLevelError, "load_signal_queue_failed error=%v", err)
	}

	return scanState{
		commands:      fileState[model.CommandQueue]{Data: commandQueue, Path: commandPath},
		tasks:         taskQueues,
		taskDirty:     make(map[string]bool),
		notifications: fileState[model.NotificationQueue]{Data: notificationQueue, Path: notificationPath},
		signals:       fileState[model.PlannerSignalQueue]{Data: signalQueue, Path: signalPath},
		signalIndex:   buildSignalIndex(signalQueue.Signals),
		scanStart:     scanStart,
	}
}

func (qh *QueueHandler) flushScanState(s scanState) {
	qh.queueStore.FlushQueues(s.commands.Data, s.commands.Path, s.commands.Dirty,
		s.tasks, s.taskDirty,
		s.notifications.Data, s.notifications.Path, s.notifications.Dirty,
		s.signals.Data, s.signals.Path, s.signals.Dirty)
}

func (qh *QueueHandler) executeDeferredReconcileNotifications(notifications []DeferredNotification) {
	if qh.reconciler == nil || len(notifications) == 0 {
		return
	}
	failed := qh.reconciler.ExecuteDeferredNotifications(notifications)
	if len(failed) == 0 {
		return
	}
	for _, n := range failed {
		qh.log(LogLevelWarn, "reconciler_notification_failed kind=%s command_id=%s", n.Kind, n.CommandID)
	}
	qh.log(LogLevelWarn, "reconciler_notifications_failed count=%d", len(failed))
}

func (qh *QueueHandler) runPeriodicWorktreeGC() {
	if qh.worktreeManager == nil {
		return
	}
	if err := qh.worktreeManager.GC(); err != nil {
		qh.log(LogLevelWarn, "worktree_gc error=%v", err)
	}
}
