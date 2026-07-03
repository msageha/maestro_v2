package daemon

import (
	"time"

	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
)

// --- Deferred work types for three-phase PeriodicScan ---

// taskQueueEntry wraps a loaded task queue with its file path.
type taskQueueEntry struct {
	Queue model.TaskQueue
	Path  string
}

// dispatchItem captures a dispatch decision made in Phase A for execution in Phase B.
type dispatchItem struct {
	Kind         string              // "command", "task", "notification"
	Command      *model.Command      // snapshot for command dispatch
	Task         *model.Task         // snapshot for task dispatch
	Notification *model.Notification // snapshot for notification dispatch
	WorkerID     string              // worker ID (tasks only)
	Epoch        int                 // lease_epoch at time of decision
	ExpiresAt    string              // lease_expires_at snapshot for fencing
}

// busyCheckItem captures an expired-lease busy probe to execute in Phase B.
type busyCheckItem struct {
	Kind      string // "task", "command"
	EntryID   string
	AgentID   string
	Epoch     int
	QueueFile string // task queue file path
	UpdatedAt string // for max_in_progress_min check
	ExpiresAt string // fencing snapshot
}

// interruptItem captures a tmux interrupt to execute in Phase B.
type interruptItem struct {
	WorkerID  string
	TaskID    string
	CommandID string
	Epoch     int
}

// cancelMarkItem captures a deferred cancellation mutation to apply in Phase C
// after Phase B has interrupted the running worker (M3 + H4).
//
// Deferring the queue mutation past Phase B's interrupt resolves a race with
// result_write_handler: a worker that completes its real result before being
// interrupted can submit it via the normal path, and Phase C's apply step
// detects the now-terminal task and skips overwriting it with a synthetic
// cancelled marker.
type cancelMarkItem struct {
	QueueFile  string
	WorkerID   string
	TaskID     string
	CommandID  string
	LeaseEpoch int
}

// signalDeliveryItem captures a planner signal delivery for Phase B.
//
// WorkerID and ConflictGeneration mirror the signal's dedup identity
// (see signalDedupKey): per-worker signals (merge_conflict, commit_failed,
// conflict_resolution_*) share the same (CommandID, PhaseID, Kind) but differ
// by WorkerID, so the delivery item — and the Phase C result-match key — must
// carry them too. Without WorkerID, applySignalResults matched two per-worker
// results under one key, dropping/duplicating deliveries and misattributing
// retry/backoff state across workers.
type signalDeliveryItem struct {
	CommandID          string
	PhaseID            string
	Kind               string
	WorkerID           string
	ConflictGeneration string
	Message            string
}

// worktreeMergeItem captures a phase-boundary worktree merge for Phase B execution.
type worktreeMergeItem struct {
	CommandID      string
	PhaseID        string
	WorkerIDs      []string
	WorkerPurposes map[string]string // workerID -> task purpose (for commit messages)
}

// runOnIntegrationPreMergeItem captures a focused commit+merge of a
// RunOnIntegration task's dependency workers, fired in Phase B BEFORE
// dispatch so that integration reflects the dep state by the time the task
// runs. Without this, same-phase RunOnIntegration tasks read a stale
// integration tree and fail with "still missing dep changes".
//
// The item is built in Phase A (collectRunOnIntegrationPreMerges) when a
// RunOnIntegration task becomes ready (deps queue-completed) but its dep
// workers haven't been integrated yet. Phase B's stepRunOnIntegrationPreMerge
// commits each dep worker (if dirty) and merges to integration. The
// dispatch of the RunOnIntegration task itself is intentionally deferred
// to the next scan so the merge result is observable and IsTaskBlocked can
// recheck the gate.
type runOnIntegrationPreMergeItem struct {
	CommandID      string
	BlockedTaskID  string   // the RunOnIntegration task awaiting this merge (for logging)
	DepWorkerIDs   []string // unique dep worker IDs to commit + merge
	WorkerPurposes map[string]string
}

// commitFailure records a worker whose CommitWorkerChanges failed.
type commitFailure struct {
	WorkerID string
	Error    error
	// Reason is a structured classification computed at commit time using
	// errors.Is/As so downstream signal emission can populate
	// PlannerSignal.Reason without re-inspecting the error chain.
	Reason string
}

// worktreeMergeResult captures the outcome of a Phase B worktree merge.
type worktreeMergeResult struct {
	Item           worktreeMergeItem
	CommitFailures []commitFailure // workers whose commit failed (excluded from merge)
	Conflicts      []model.MergeConflict
	Error          error
}

// worktreePublishItem captures a publish-to-base operation for Phase B execution.
type worktreePublishItem struct {
	CommandID      string
	PublishMessage string // command content summary for commit message
}

// worktreePublishResult captures the outcome of a Phase B publish-to-base.
type worktreePublishResult struct {
	Item  worktreePublishItem
	Error error
}

// worktreeCleanupItem captures a worktree cleanup operation for Phase B execution.
type worktreeCleanupItem struct {
	CommandID string
	Reason    string // "success" or "failure"
}

// worktreeCleanupResult captures the outcome of a Phase B worktree cleanup.
type worktreeCleanupResult struct {
	Item  worktreeCleanupItem
	Error error
}

// deferredWork collects all slow I/O operations for Phase B execution.
type deferredWork struct {
	dispatches                []dispatchItem
	interrupts                []interruptItem
	cancelMarks               []cancelMarkItem
	busyChecks                []busyCheckItem
	signals                   []signalDeliveryItem
	clears                    []string // agent IDs to /clear
	worktreeMerges            []worktreeMergeItem
	runOnIntegrationPreMerges []runOnIntegrationPreMergeItem
	worktreePublishes         []worktreePublishItem
	worktreeCleanups          []worktreeCleanupItem
	abGroups                  []abGroupWorkItem   // A/B candidate groups observed in the queue snapshot
	cancelledCommandIDs       map[string]struct{} // in-memory set of cancel-requested commands for Phase B dispatch guard
}

// dispatchResult captures the outcome of a Phase B dispatch.
type dispatchResult struct {
	Item    dispatchItem
	Success bool
	Error   error
}

// busyCheckResult captures the outcome of a Phase B busy probe.
type busyCheckResult struct {
	Item      busyCheckItem
	Busy      bool
	Undecided bool // VerdictUndecided: neither extend nor release; defer to next scan
}

// signalDeliveryResult captures the outcome of a Phase B signal delivery.
type signalDeliveryResult struct {
	Item    signalDeliveryItem
	Success bool
	Error   error
}

// phaseAResult holds all data Phase A passes to Phase B and Phase C.
type phaseAResult struct {
	work      deferredWork
	scanStart time.Time
	counters  metrics.ScanCounters
}

// phaseBResult holds all results from Phase B for Phase C to apply.
type phaseBResult struct {
	dispatches        []dispatchResult
	busyChecks        []busyCheckResult
	signals           []signalDeliveryResult
	worktreeMerges    []worktreeMergeResult
	worktreePublishes []worktreePublishResult
	worktreeCleanups  []worktreeCleanupResult
	recoveryHints     []string // M3: recovery hints for partial failure diagnosis
}
