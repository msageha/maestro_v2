package daemon

import (
	"context"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/dispatch"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// Compile-time interface satisfaction checks.
var (
	_ QueueLeaseManager       = (*LeaseManager)(nil)
	_ QueueDispatcher         = (*Dispatcher)(nil)
	_ QueueDependencyResolver = (*DependencyResolver)(nil)
	_ QueueWorktreeManager    = (*WorktreeManager)(nil)
	_ QueueStore              = (*QueueStoreImpl)(nil)
	_ ExecutorGetter          = (*ExecutorProvider)(nil)
	_ ContinuousAdvancer      = (*ContinuousHandler)(nil)
	_ EventPublisher          = (*events.Bus)(nil)
)

// ExecutorGetter provides lazy executor access.
type ExecutorGetter interface {
	GetExecutor() (AgentExecutor, error)
}

// ContinuousAdvancer checks and advances continuous mode iteration.
type ContinuousAdvancer interface {
	CheckAndAdvance(commandID string, status model.Status) error
}

// EventPublisher publishes events to subscribers.
type EventPublisher interface {
	Publish(eventType events.EventType, data map[string]interface{})
}

// ---------------------------------------------------------------------------
// QueueLeaseManager: split into LeaseReader + LeaseWriter
// ---------------------------------------------------------------------------

// LeaseReader provides read-only lease state queries.
type LeaseReader interface {
	ExpireCommands(commands []model.Command) []int
	ExpireTasks(tasks []model.Task) []int
	ExpireNotifications(notifications []model.Notification) []int
	RenewableCommands(commands []model.Command, bufferSec int) []int
	IsLeaseExpired(leaseExpiresAt *string) bool
	IsLeaseNearExpiry(leaseExpiresAt *string, bufferSec int) bool
	GraceLeaseTTL(scanIntervalSec int) time.Duration
}

// LeaseWriter provides lease acquisition, release, and extension operations.
type LeaseWriter interface {
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
}

// QueueLeaseManager combines LeaseReader and LeaseWriter for backward compatibility.
type QueueLeaseManager interface {
	LeaseReader
	LeaseWriter
}

// ---------------------------------------------------------------------------
// QueueDispatcher: setter methods extracted to QueueDispatcherConfigurer
// ---------------------------------------------------------------------------

// QueueDispatcherConfigurer defines configuration/wiring operations for the dispatcher.
type QueueDispatcherConfigurer interface {
	SetEventBus(bus *events.Bus)
	SetQualityGate(qg dispatch.GateChecker)
	SetWorktreeManager(wm dispatch.WorktreeResolver)
}

// QueueDispatcherOperator defines runtime dispatch and sorting operations.
type QueueDispatcherOperator interface {
	DispatchCommand(cmd *model.Command) error
	DispatchTask(task *model.Task, workerID string) error
	DispatchNotification(ntf *model.Notification) error
	SortPendingCommands(commands []model.Command) []int
	SortPendingTasks(tasks []model.Task) []int
	SortPendingNotifications(notifications []model.Notification) []int
}

// QueueDispatcher combines QueueDispatcherConfigurer and QueueDispatcherOperator
// for backward compatibility. New code that only needs dispatch operations
// can accept QueueDispatcherOperator separately.
type QueueDispatcher interface {
	QueueDispatcherConfigurer
	QueueDispatcherOperator
}

// ---------------------------------------------------------------------------
// QueueDependencyResolver: setter extracted to QueueDependencyResolverConfigurer
// ---------------------------------------------------------------------------

// QueueDependencyResolverConfigurer defines configuration/wiring operations.
type QueueDependencyResolverConfigurer interface {
	SetEventBus(bus *events.Bus)
}

// QueueDependencyResolver defines the dependency resolution operations used by QueueHandler.
// Embeds QueueDependencyResolverConfigurer for backward compatibility.
type QueueDependencyResolver interface {
	QueueDependencyResolverConfigurer
	IsTaskBlocked(task *model.Task) (bool, error)
	IsSystemCommitReady(commandID, taskID string) (bool, bool, error)
	CheckDependencyFailure(task *model.Task) (string, model.Status, error)
	CheckPhaseTransitions(commandID string) ([]PhaseTransitionResult, error)
	GetPhaseStatus(commandID, phaseID string) (model.PhaseStatus, error)
	BuildAwaitingFillNotification(commandID string, phase PhaseInfo) string
	HasStateReader() bool
	GetStateReader() StateReader
	GetStateManager() StateManager
}

// ---------------------------------------------------------------------------
// QueueStore: split into QueueStoreReader + QueueStoreWriter
// ---------------------------------------------------------------------------

// QueueStoreReader provides read-only queue file loading operations.
type QueueStoreReader interface {
	LoadCommandQueue() (model.CommandQueue, string)
	LoadAllTaskQueues() map[string]*taskQueueEntry
	LoadNotificationQueue() (model.NotificationQueue, string)
	LoadPlannerSignalQueue() (model.PlannerSignalQueue, string)
}

// QueueStoreWriter provides queue file flush operations.
type QueueStoreWriter interface {
	FlushQueues(
		commandQueue model.CommandQueue, commandPath string, commandsDirty bool,
		taskQueues map[string]*taskQueueEntry, taskDirty map[string]bool,
		notificationQueue model.NotificationQueue, notificationPath string, notificationsDirty bool,
		signalQueue model.PlannerSignalQueue, signalPath string, signalsDirty bool,
	)
}

// QueueStore combines QueueStoreReader and QueueStoreWriter for queue file I/O.
type QueueStore interface {
	QueueStoreReader
	QueueStoreWriter
}

// ---------------------------------------------------------------------------
// QueueWorktreeManager: split into WorktreeGitOps + WorktreeStateManager
// ---------------------------------------------------------------------------

// WorktreeGitOps provides Git and filesystem operations on worktrees.
type WorktreeGitOps interface {
	CommitWorkerChanges(commandID, workerID, message string) error
	DiscardWorkerChanges(commandID, workerID string) error
	MergeToIntegration(ctx context.Context, commandID string, workerIDs []string, workerPurposes map[string]string) ([]model.MergeConflict, error)
	SyncFromIntegration(commandID string, workerIDs []string) error
	PublishToBase(commandID string, publishMessage string) error
	CleanupCommand(commandID string) error
	DispatchConflictResolution(commandID, phaseID, workerID, conflictGen string) error
	GC() error
}

// WorktreeStateManager provides state queries and updates for worktrees.
type WorktreeStateManager interface {
	GetCommandState(commandID string) (*model.WorktreeCommandState, error)
	HasWorktrees(commandID string) bool
	AutoCommit() bool
	AutoMerge() bool
	MarkIntegrationFailed(commandID string) error
	MarkIntegrationStallSignaled(commandID string) error
	MarkPhaseMerged(commandID, phaseID string) error
	AddCommitFailedWorker(commandID, workerID string) error
	RemoveCommitFailedWorker(commandID, workerID string) error
}

// QueueWorktreeManager combines WorktreeGitOps and WorktreeStateManager for backward compatibility.
type QueueWorktreeManager interface {
	WorktreeGitOps
	WorktreeStateManager
}
