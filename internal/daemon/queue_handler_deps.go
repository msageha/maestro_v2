package daemon

import (
	"time"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// Compile-time interface satisfaction checks.
var (
	_ QueueLeaseManager       = (*LeaseManager)(nil)
	_ QueueDispatcher         = (*Dispatcher)(nil)
	_ QueueDependencyResolver = (*DependencyResolver)(nil)
	_ QueueWorktreeManager    = (*WorktreeManager)(nil)
)

// QueueLeaseManager defines the lease operations used by QueueHandler.
type QueueLeaseManager interface {
	AcquireCommandLease(cmd *model.Command, owner string) error
	AcquireTaskLease(task *model.Task, owner string) error
	AcquireNotificationLease(ntf *model.Notification, owner string) error
	ReleaseCommandLease(cmd *model.Command) error
	ReleaseTaskLease(task *model.Task) error
	ReleaseNotificationLease(ntf *model.Notification) error
	ExtendCommandLease(cmd *model.Command) error
	ExtendTaskLease(task *model.Task) error
	ExtendCommandLeaseGrace(cmd *model.Command, graceTTL time.Duration) error
	ExtendTaskLeaseGrace(task *model.Task, graceTTL time.Duration) error
	ExpireCommands(commands []model.Command) []int
	ExpireTasks(tasks []model.Task) []int
	ExpireNotifications(notifications []model.Notification) []int
	RenewableCommands(commands []model.Command, bufferSec int) []int
	IsLeaseExpired(leaseExpiresAt *string) bool
	IsLeaseNearExpiry(leaseExpiresAt *string, bufferSec int) bool
	GraceLeaseTTL(scanIntervalSec int) time.Duration
}

// QueueDispatcher defines the dispatch operations used by QueueHandler.
type QueueDispatcher interface {
	DispatchCommand(cmd *model.Command) error
	DispatchTask(task *model.Task, workerID string) error
	DispatchNotification(ntf *model.Notification) error
	SortPendingCommands(commands []model.Command) []int
	SortPendingTasks(tasks []model.Task) []int
	SortPendingNotifications(notifications []model.Notification) []int
	SetEventBus(bus *events.Bus)
	SetQualityGate(qg *QualityGateDaemon)
	SetWorktreeManager(wm *WorktreeManager)
}

// QueueDependencyResolver defines the dependency resolution operations used by QueueHandler.
type QueueDependencyResolver interface {
	IsTaskBlocked(task *model.Task) (bool, error)
	IsSystemCommitReady(commandID, taskID string) (bool, bool, error)
	CheckDependencyFailure(task *model.Task) (string, model.Status, error)
	CheckPhaseTransitions(commandID string) ([]PhaseTransitionResult, error)
	GetPhaseStatus(commandID, phaseID string) (model.PhaseStatus, error)
	BuildAwaitingFillNotification(commandID string, phase PhaseInfo) string
	SetEventBus(bus *events.Bus)
	HasStateReader() bool
	GetStateReader() StateReader
}

// QueueWorktreeManager defines the worktree operations used by QueueHandler.
type QueueWorktreeManager interface {
	GC() error
	GetCommandState(commandID string) (*model.WorktreeCommandState, error)
	HasWorktrees(commandID string) bool
	MarkIntegrationFailed(commandID string) error
	MarkIntegrationStallSignaled(commandID string) error
	MarkPhaseMerged(commandID, phaseID string) error
	CommitWorkerChanges(commandID, workerID, message string) error
	DiscardWorkerChanges(commandID, workerID string) error
	AddCommitFailedWorker(commandID, workerID string) error
	RemoveCommitFailedWorker(commandID, workerID string) error
	MergeToIntegration(commandID string, workerIDs []string, workerPurposes map[string]string) ([]model.MergeConflict, error)
	SyncFromIntegration(commandID string, workerIDs []string) error
	PublishToBase(commandID string, publishMessage string) error
	CleanupCommand(commandID string) error
	DispatchConflictResolution(commandID, phaseID, workerID, conflictGen string) error
	AutoCommit() bool
	AutoMerge() bool
}
