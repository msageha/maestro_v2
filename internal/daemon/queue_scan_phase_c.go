package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// periodicScanPhaseC runs under scanMu.Lock. It reloads queues from disk,
// applies Phase B results with epoch fencing, flushes, and runs post-flush steps.
// Returns deferred notifications from reconciliation that must be executed outside the lock.
func (qh *QueueHandler) periodicScanPhaseC(pa phaseAResult, pb phaseBResult) []DeferredNotification {
	qh.scanMu.Lock()
	defer qh.scanMu.Unlock()

	// Restore counters accumulated during Phase A
	qh.scanCounters = pa.counters

	// --- Apply dispatch + busy check results (single load/flush) ---
	if len(pb.dispatches) > 0 || len(pb.busyChecks) > 0 {
		commandQueue, commandPath := qh.loadCommandQueue()
		taskQueues := qh.loadAllTaskQueues()
		notificationQueue, notificationPath := qh.loadNotificationQueue()
		commandsDirty := false
		notificationsDirty := false
		taskDirty := make(map[string]bool)

		for _, dr := range pb.dispatches {
			switch dr.Item.Kind {
			case "command":
				qh.applyCommandDispatchResult(dr, &commandQueue, &commandsDirty)
			case "task":
				qh.applyTaskDispatchResult(dr, taskQueues, taskDirty)
			case "notification":
				qh.applyNotificationDispatchResult(dr, &notificationQueue, &notificationsDirty)
			}
		}

		for _, bc := range pb.busyChecks {
			switch bc.Item.Kind {
			case "task":
				qh.applyTaskBusyCheckResult(bc, taskQueues, taskDirty)
			case "command":
				qh.applyCommandBusyCheckResult(bc, &commandQueue, &commandsDirty)
			}
		}

		// Single flush for both dispatch and busy check results
		qh.flushQueues(commandQueue, commandPath, commandsDirty,
			taskQueues, taskDirty,
			notificationQueue, notificationPath, notificationsDirty,
			model.PlannerSignalQueue{}, "", false)
	}

	// --- Apply worktree merge results: emit conflict signals, record merged phases ---
	if len(pb.worktreeMerges) > 0 {
		signalQueue, signalPath := qh.loadPlannerSignalQueue()
		signalsDirty := false
		signalIndex := buildSignalIndex(signalQueue.Signals)
		now := qh.clock.Now().UTC().Format(time.RFC3339)
		for _, mr := range pb.worktreeMerges {
			if mr.Error != nil {
				qh.log(LogLevelError, "worktree_merge_failed command=%s phase=%s error=%v",
					mr.Item.CommandID, mr.Item.PhaseID, mr.Error)
			}
			for _, conflict := range mr.Conflicts {
				msg := fmt.Sprintf("[maestro] kind:merge_conflict command_id:%s phase:%s worker:%s\nconflict_files: %s",
					mr.Item.CommandID, mr.Item.PhaseID, conflict.WorkerID,
					strings.Join(conflict.ConflictFiles, ", "))
				qh.upsertPlannerSignal(&signalQueue, &signalsDirty, model.PlannerSignal{
					Kind:      "merge_conflict",
					CommandID: mr.Item.CommandID,
					PhaseID:   mr.Item.PhaseID,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, signalIndex)
			}
			if mr.Error == nil && qh.worktreeManager != nil {
				if err := qh.worktreeManager.MarkPhaseMerged(mr.Item.CommandID, mr.Item.PhaseID); err != nil {
					qh.log(LogLevelWarn, "mark_phase_merged_failed command=%s phase=%s error=%v",
						mr.Item.CommandID, mr.Item.PhaseID, err)
				}
			}
		}
		if signalsDirty {
			p := signalPath
			if p == "" {
				p = filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
			}
			if err := yamlutil.AtomicWrite(p, signalQueue); err != nil {
				qh.log(LogLevelError, "write_planner_signals error=%v", err)
			}
		}
	}

	// --- Apply worktree publish results: emit signal on failure ---
	if len(pb.worktreePublishes) > 0 {
		signalQueue, signalPath := qh.loadPlannerSignalQueue()
		signalsDirty := false
		signalIndex := buildSignalIndex(signalQueue.Signals)
		now := qh.clock.Now().UTC().Format(time.RFC3339)
		for _, pr := range pb.worktreePublishes {
			if pr.Error != nil {
				qh.log(LogLevelError, "worktree_publish_failed command=%s error=%v",
					pr.Item.CommandID, pr.Error)
				msg := fmt.Sprintf("[maestro] kind:publish_failed command_id:%s\nerror: %v",
					pr.Item.CommandID, pr.Error)
				qh.upsertPlannerSignal(&signalQueue, &signalsDirty, model.PlannerSignal{
					Kind:      "publish_failed",
					CommandID: pr.Item.CommandID,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, signalIndex)
			} else {
				qh.log(LogLevelInfo, "worktree_published command=%s", pr.Item.CommandID)
			}
		}
		if signalsDirty {
			p := signalPath
			if p == "" {
				p = filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
			}
			if err := yamlutil.AtomicWrite(p, signalQueue); err != nil {
				qh.log(LogLevelError, "write_planner_signals error=%v", err)
			}
		}
	}

	// --- Apply worktree cleanup results: log only ---
	for _, cr := range pb.worktreeCleanups {
		if cr.Error != nil {
			qh.log(LogLevelWarn, "worktree_cleanup_failed command=%s reason=%s error=%v",
				cr.Item.CommandID, cr.Item.Reason, cr.Error)
		} else {
			qh.log(LogLevelInfo, "worktree_cleanup_complete command=%s reason=%s",
				cr.Item.CommandID, cr.Item.Reason)
		}
	}

	// --- Apply signal delivery results ---
	if len(pb.signals) > 0 {
		signalQueue, signalPath := qh.loadPlannerSignalQueue()
		signalsDirty := false
		qh.applySignalResults(pb.signals, &signalQueue, &signalsDirty)
		if signalsDirty {
			p := signalPath
			if p == "" {
				p = filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
			}
			if len(signalQueue.Signals) == 0 {
				_ = os.Remove(p)
			} else {
				if err := yamlutil.AtomicWrite(p, signalQueue); err != nil {
					qh.log(LogLevelError, "write_planner_signals error=%v", err)
				}
			}
		}
	}

	// Step 2.5: Result notification retry
	if qh.resultHandler != nil {
		n := qh.resultHandler.ScanAllResults()
		qh.scanCounters.NotificationRetries += n
		if n > 0 {
			qh.log(LogLevelInfo, "result_notify_scan notified=%d", n)
		}
	}

	// Step 3: Reconciliation (state mutations under scanMu, notifications deferred)
	var deferredNotifs []DeferredNotification
	if qh.reconciler != nil {
		repairs, notifs := qh.reconciler.Reconcile()
		deferredNotifs = notifs
		qh.scanCounters.ReconciliationRepairs += len(repairs)
		for _, repair := range repairs {
			qh.log(LogLevelInfo, "reconciliation pattern=%s command=%s task=%s detail=%s",
				repair.Pattern, repair.CommandID, repair.TaskID, repair.Detail)
		}
	}

	// Step 4: Metrics and dashboard
	if qh.metricsHandler != nil {
		commandQueue, _ := qh.loadCommandQueue()
		taskQueues := qh.loadAllTaskQueues()
		notificationQueue, _ := qh.loadNotificationQueue()
		scanDuration := qh.clock.Now().Sub(pa.scanStart)
		if err := qh.metricsHandler.UpdateMetrics(commandQueue, taskQueues, notificationQueue, pa.scanStart, scanDuration, &qh.scanCounters); err != nil {
			qh.log(LogLevelError, "update_metrics error=%v", err)
		}
		if err := qh.metricsHandler.UpdateDashboard(commandQueue, taskQueues, notificationQueue); err != nil {
			qh.log(LogLevelError, "update_dashboard error=%v", err)
		}
	}

	return deferredNotifs
}
