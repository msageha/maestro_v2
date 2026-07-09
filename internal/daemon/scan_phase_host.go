package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/paneactivity"
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
		paneVerdicts:  make(map[string]paneactivity.Verdict),
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
	// ExecuteDeferredNotifications performs per-kind failure recovery itself
	// (one-shot guard rollback for R7/R8, durable fill_timeout signal for
	// R6), so a failed delivery here re-emits on a following scan instead of
	// being silently dropped.
	failed := qh.reconciler.ExecuteDeferredNotifications(notifications)
	if len(failed) == 0 {
		return
	}
	for _, n := range failed {
		qh.log(LogLevelWarn, "reconciler_notification_failed kind=%s command_id=%s (recovery scheduled; re-emits on a later scan)", n.Kind, n.CommandID)
	}
	qh.log(LogLevelWarn, "reconciler_notifications_failed count=%d", len(failed))
}

// runPostScanResultNotifications drives the per-result notification
// retry sweep (formerly Phase C step 2.5). Phase C used to call
// resultHandler.ScanAllResults under scanMu.Lock, which meant a slow
// Planner pane could hold the lock for the full inline-retry budget
// (default 3 attempts × delivery_timeout + 2 × retry_delay) and starve
// queue_write / plan_complete / verify_write UDS handlers waiting on
// scanMu.RLock — the symptom users observed as 30-second CLI timeouts
// despite the daemon eventually reporting success.
//
// Running this AFTER periodicScanPhaseC releases scanMu means:
//   - the notification path holds only its own per-result lockMap key
//     ("result:<worker>"), not scanMu, so UDS writes can proceed in
//     parallel even when the Planner is unresponsive;
//   - state mutations from Phase C are already flushed to disk, so the
//     notification scan sees the freshest results;
//   - the surrounding scanRunMu still serializes successive scan cycles,
//     so notifications and the next Phase A do not interleave.
func (qh *QueueHandler) runPostScanResultNotifications(se *ScanPhaseExecutor) {
	if qh.resultHandler == nil {
		return
	}
	n := qh.resultHandler.ScanAllResults()
	se.scanCounters.NotificationRetries += n
	if n > 0 {
		qh.log(LogLevelInfo, "result_notify_scan notified=%d", n)
	}
}

func (qh *QueueHandler) runPeriodicWorktreeGC() {
	if qh.worktreeManager == nil {
		return
	}
	if err := qh.worktreeManager.GC(); err != nil {
		qh.log(LogLevelWarn, "worktree_gc error=%v", err)
	}
}
