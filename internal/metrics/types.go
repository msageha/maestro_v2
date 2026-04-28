// Package metrics provides metrics collection and reporting for the Maestro daemon.
// It handles reading/writing state/metrics.yaml and computing queue-depth snapshots.
//
// The package name shadows the standard library's runtime/metrics; the
// daemon never imports runtime/metrics directly, and this domain-level
// "metrics" name is used pervasively across the queue and dashboard code.
// Renaming is a bigger churn than the warning warrants.
package metrics //nolint:revive // intentional name; runtime/metrics is unused here

import "github.com/msageha/maestro_v2/internal/model"

// ScanCounters tracks cumulative counters during a single PeriodicScan cycle.
type ScanCounters struct {
	CommandsDispatched int
	TasksDispatched    int
	// TasksDispatchedUncertain counts task dispatches whose submit
	// confirmation probe exhausted (ErrSubmitConfirmUncertain) and were
	// "assumed delivered" — the lease is retained and the task remains
	// in_progress so the worker's eventual result_write completes the task,
	// while a re-dispatch is suppressed to avoid duplicating the still-in-
	// flight prompt. Operators monitoring this metric: a non-zero value is
	// not necessarily a bug (the deliverer was over-cautious and the worker
	// almost certainly got the prompt) but a sustained increase indicates
	// the submit probe is failing to detect the runtime's UI markers and
	// worth investigating in the worker pane logs.
	TasksDispatchedUncertain   int
	TasksCompleted             int
	TasksFailed                int
	TasksCancelled             int
	DeadLetters                int
	ReconciliationRepairs      int
	NotificationRetries        int
	SignalDeliveries           int
	SignalRetries              int
	SignalDeadLetters          int
	SignalInlineRetrySuccesses int
	LeaseRenewals              int
	LeaseExtensions            int
	LeaseReleases              int
}

// Merge adds all counter values from other into c.
// This is used to combine counters accumulated across different scan phases
// without losing increments from any phase.
func (c *ScanCounters) Merge(other ScanCounters) {
	c.CommandsDispatched += other.CommandsDispatched
	c.TasksDispatched += other.TasksDispatched
	c.TasksDispatchedUncertain += other.TasksDispatchedUncertain
	c.TasksCompleted += other.TasksCompleted
	c.TasksFailed += other.TasksFailed
	c.TasksCancelled += other.TasksCancelled
	c.DeadLetters += other.DeadLetters
	c.ReconciliationRepairs += other.ReconciliationRepairs
	c.NotificationRetries += other.NotificationRetries
	c.SignalDeliveries += other.SignalDeliveries
	c.SignalRetries += other.SignalRetries
	c.SignalDeadLetters += other.SignalDeadLetters
	c.SignalInlineRetrySuccesses += other.SignalInlineRetrySuccesses
	c.LeaseRenewals += other.LeaseRenewals
	c.LeaseExtensions += other.LeaseExtensions
	c.LeaseReleases += other.LeaseReleases
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
