// Package metrics provides metrics collection and reporting for the Maestro daemon.
// It handles reading/writing state/metrics.yaml and computing queue-depth snapshots.
package metrics

import "github.com/msageha/maestro_v2/internal/model"

// ScanCounters tracks cumulative counters during a single PeriodicScan cycle.
type ScanCounters struct {
	CommandsDispatched    int
	TasksDispatched       int
	TasksCompleted        int
	TasksFailed           int
	TasksCancelled        int
	DeadLetters           int
	ReconciliationRepairs int
	NotificationRetries   int
	SignalDeliveries           int
	SignalRetries              int
	SignalDeadLetters          int
	SignalInlineRetrySuccesses int
	LeaseRenewals         int
	LeaseExtensions       int
	LeaseReleases         int
}

// Gauges holds snapshot (non-incremental) values computed at scan time
// and overwritten on every UpdateMetrics call.
type Gauges struct {
	WorktreeCommandsStalled int
	BakFilesCount           int
}

// TaskQueueSnapshot is a read-only view of a worker's task queue,
// decoupled from the daemon's internal taskQueueEntry type.
type TaskQueueSnapshot struct {
	WorkerID string
	Tasks    []model.Task
}
