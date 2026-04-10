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
	SetQualityGate(qg *QualityGateDaemon)
	SetWorktreeManager(wm *WorktreeManager)
}

// QueueDispatcher defines the dispatch operations used by QueueHandler.
// Embeds QueueDispatcherConfigurer for backward compatibility; new code
// that only needs dispatch operations can accept QueueDispatcherConfigurer
// or QueueDispatcher separately.
type QueueDispatcher interface {
	QueueDispatcherConfigurer
	DispatchCommand(cmd *model.Command) error
	DispatchTask(task *model.Task, workerID string) error
	DispatchNotification(ntf *model.Notification) error
	SortPendingCommands(commands []model.Command) []int
	SortPendingTasks(tasks []model.Task) []int
	SortPendingNotifications(notifications []model.Notification) []int
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
// QueueWorktreeManager: split into WorktreeReader + WorktreeWriter
// ---------------------------------------------------------------------------

// WorktreeReader provides read-only worktree state queries.
type WorktreeReader interface {
	GC() error
	GetCommandState(commandID string) (*model.WorktreeCommandState, error)
	HasWorktrees(commandID string) bool
	AutoCommit() bool
	AutoMerge() bool
}

// WorktreeWriter provides worktree mutation operations.
type WorktreeWriter interface {
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
}

// QueueWorktreeManager combines WorktreeReader and WorktreeWriter for backward compatibility.
type QueueWorktreeManager interface {
	WorktreeReader
	WorktreeWriter
}
