package daemon

import (
	"fmt"
	"log"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// LeaseManager handles lease lifecycle for queue entries.
type LeaseManager struct {
	dispatchLeaseSec int
	maxInProgressMin int
	clock            Clock
	dl               *DaemonLogger
	logger           *log.Logger
	logLevel         LogLevel
}

// LeaseManagerOption configures a LeaseManager.
type LeaseManagerOption func(*LeaseManager)

// WithLeaseManagerClock sets a custom Clock for the LeaseManager.
func WithLeaseManagerClock(c Clock) LeaseManagerOption {
	return func(lm *LeaseManager) { lm.clock = c }
}

// NewLeaseManager creates a new LeaseManager.
func NewLeaseManager(cfg model.WatcherConfig, logger *log.Logger, logLevel LogLevel, opts ...LeaseManagerOption) *LeaseManager {
	dispatchLease := cfg.DispatchLeaseSec
	if dispatchLease <= 0 {
		dispatchLease = 300
	}
	maxInProgress := cfg.EffectiveMaxInProgressMin()
	lm := &LeaseManager{
		dispatchLeaseSec: dispatchLease,
		maxInProgressMin: maxInProgress,
		clock:            RealClock{},
		dl:               NewDaemonLoggerFromLegacy("lease_manager", logger, logLevel),
		logger:           logger,
		logLevel:         logLevel,
	}
	for _, opt := range opts {
		opt(lm)
	}
	return lm
}

// LeaseInfo represents lease metadata for a queue entry.
type LeaseInfo struct {
	LeaseOwner     *string
	LeaseExpiresAt *string
	LeaseEpoch     int
}

// leaseRef provides unified pointer-based access to lease-related fields
// across Command, Task, and Notification types, eliminating duplication
// in acquire/release/extend operations.
type leaseRef struct {
	id             string
	entityType     string
	status         *model.Status
	leaseOwner     **string
	leaseExpiresAt **string
	leaseEpoch     *int
	updatedAt      *string
	validate       func(model.Status, model.Status) error
	postAcquire    func(nowStr string) // type-specific hook (e.g., Task.InProgressAt)
	postRelease    func()              // type-specific hook (e.g., Task.InProgressAt)
}

func commandLeaseRef(cmd *model.Command) leaseRef {
	return leaseRef{
		id: cmd.ID, entityType: "command",
		status: &cmd.Status, leaseOwner: &cmd.LeaseOwner,
		leaseExpiresAt: &cmd.LeaseExpiresAt, leaseEpoch: &cmd.LeaseEpoch,
		updatedAt: &cmd.UpdatedAt,
		validate:  model.ValidateCommandTaskQueueTransition,
	}
}

func taskLeaseRef(task *model.Task) leaseRef {
	return leaseRef{
		id: task.ID, entityType: "task",
		status: &task.Status, leaseOwner: &task.LeaseOwner,
		leaseExpiresAt: &task.LeaseExpiresAt, leaseEpoch: &task.LeaseEpoch,
		updatedAt: &task.UpdatedAt,
		validate:  model.ValidateCommandTaskQueueTransition,
		postAcquire: func(nowStr string) {
			if task.InProgressAt == nil {
				task.InProgressAt = &nowStr
			}
		},
		postRelease: func() {
			// H5: clear InProgressAt so the next AcquireTaskLease records a fresh
			// dispatch timestamp. Otherwise the original (stale) value would block
			// max_in_progress_min checks from ever firing on the re-dispatched task.
			task.InProgressAt = nil
		},
	}
}

func notificationLeaseRef(ntf *model.Notification) leaseRef {
	return leaseRef{
		id: ntf.ID, entityType: "notification",
		status: &ntf.Status, leaseOwner: &ntf.LeaseOwner,
		leaseExpiresAt: &ntf.LeaseExpiresAt, leaseEpoch: &ntf.LeaseEpoch,
		updatedAt: &ntf.UpdatedAt,
		validate:  model.ValidateNotificationQueueTransition,
	}
}

// acquireLease transitions a queue entry from pending to in_progress with lease.
func (lm *LeaseManager) acquireLease(ref leaseRef, owner string) error {
	if err := ref.validate(*ref.status, model.StatusInProgress); err != nil {
		return fmt.Errorf("cannot acquire lease: %w", err)
	}

	now := lm.clock.Now().UTC()
	expires := now.Add(time.Duration(lm.dispatchLeaseSec) * time.Second)

	*ref.status = model.StatusInProgress
	ownerStr := owner
	*ref.leaseOwner = &ownerStr
	expiresStr := expires.Format(time.RFC3339)
	*ref.leaseExpiresAt = &expiresStr
	(*ref.leaseEpoch)++
	nowStr := now.Format(time.RFC3339)
	*ref.updatedAt = nowStr

	if ref.postAcquire != nil {
		ref.postAcquire(nowStr)
	}

	lm.log(LogLevelInfo, "lease_acquire type=%s id=%s owner=%s epoch=%d expires=%s",
		ref.entityType, ref.id, owner, *ref.leaseEpoch, expiresStr)
	return nil
}

// releaseLease transitions a queue entry from in_progress back to pending.
func (lm *LeaseManager) releaseLease(ref leaseRef) error {
	if err := ref.validate(*ref.status, model.StatusPending); err != nil {
		return fmt.Errorf("cannot release lease: %w", err)
	}

	*ref.status = model.StatusPending
	*ref.leaseOwner = nil
	*ref.leaseExpiresAt = nil
	*ref.updatedAt = lm.clock.Now().UTC().Format(time.RFC3339)

	if ref.postRelease != nil {
		ref.postRelease()
	}

	lm.log(LogLevelInfo, "lease_release type=%s id=%s epoch=%d", ref.entityType, ref.id, *ref.leaseEpoch)
	return nil
}

// extendLeaseExpiry sets a new expiration time on an in_progress entry.
// UpdatedAt is intentionally NOT updated — it must retain the original
// dispatch timestamp so that max_in_progress_min checks work correctly.
func (lm *LeaseManager) extendLeaseExpiry(ref leaseRef, ttl time.Duration) error {
	if *ref.status != model.StatusInProgress {
		return fmt.Errorf("cannot extend lease: %s %s is %s, not in_progress", ref.entityType, ref.id, *ref.status)
	}
	expires := lm.clock.Now().UTC().Add(ttl)
	expiresStr := expires.Format(time.RFC3339)
	*ref.leaseExpiresAt = &expiresStr
	return nil
}

// AcquireCommandLease transitions a command from pending to in_progress with lease.
func (lm *LeaseManager) AcquireCommandLease(cmd *model.Command, owner string) error {
	return lm.acquireLease(commandLeaseRef(cmd), owner)
}

// AcquireTaskLease transitions a task from pending to in_progress with lease.
func (lm *LeaseManager) AcquireTaskLease(task *model.Task, owner string) error {
	return lm.acquireLease(taskLeaseRef(task), owner)
}

// AcquireNotificationLease transitions a notification from pending to in_progress with lease.
func (lm *LeaseManager) AcquireNotificationLease(ntf *model.Notification, owner string) error {
	return lm.acquireLease(notificationLeaseRef(ntf), owner)
}

// ReleaseCommandLease transitions a command from in_progress back to pending with cleared lease.
func (lm *LeaseManager) ReleaseCommandLease(cmd *model.Command) error {
	return lm.releaseLease(commandLeaseRef(cmd))
}

// ReleaseTaskLease transitions a task from in_progress back to pending with cleared lease.
func (lm *LeaseManager) ReleaseTaskLease(task *model.Task) error {
	return lm.releaseLease(taskLeaseRef(task))
}

// ReleaseNotificationLease transitions a notification back to pending with cleared lease.
func (lm *LeaseManager) ReleaseNotificationLease(ntf *model.Notification) error {
	return lm.releaseLease(notificationLeaseRef(ntf))
}

// ExtendCommandLease extends the lease expiration for an in_progress command.
func (lm *LeaseManager) ExtendCommandLease(cmd *model.Command) error {
	if err := lm.extendLeaseExpiry(commandLeaseRef(cmd), time.Duration(lm.dispatchLeaseSec)*time.Second); err != nil {
		return err
	}
	lm.log(LogLevelDebug, "lease_extend type=command id=%s epoch=%d new_expires=%s",
		cmd.ID, cmd.LeaseEpoch, *cmd.LeaseExpiresAt)
	return nil
}

// ExtendTaskLease extends the lease expiration for an in_progress task.
func (lm *LeaseManager) ExtendTaskLease(task *model.Task) error {
	if err := lm.extendLeaseExpiry(taskLeaseRef(task), time.Duration(lm.dispatchLeaseSec)*time.Second); err != nil {
		return err
	}
	lm.log(LogLevelDebug, "lease_extend type=task id=%s epoch=%d new_expires=%s",
		task.ID, task.LeaseEpoch, *task.LeaseExpiresAt)
	return nil
}

// GraceLeaseTTL returns a shorter TTL for undecided busy probes.
// Formula: max(2 * scan_interval, dispatch_lease / 3), capped at dispatch_lease / 2.
// Floor: scan_interval + 10s to ensure the lease survives until the next scan.
func (lm *LeaseManager) GraceLeaseTTL(scanIntervalSec int) time.Duration {
	if scanIntervalSec <= 0 {
		scanIntervalSec = 10
	}
	twoScans := 2 * scanIntervalSec
	thirdLease := lm.dispatchLeaseSec / 3
	grace := twoScans
	if thirdLease > grace {
		grace = thirdLease
	}
	halfLease := lm.dispatchLeaseSec / 2
	if grace > halfLease {
		grace = halfLease
	}
	// Ensure grace survives at least until the next scan cycle
	minGrace := scanIntervalSec + 10
	if grace < minGrace {
		grace = minGrace
	}
	if grace < 1 {
		grace = 1
	}
	return time.Duration(grace) * time.Second
}

// ExtendCommandLeaseGrace extends the lease with a shorter grace TTL for undecided probes.
func (lm *LeaseManager) ExtendCommandLeaseGrace(cmd *model.Command, graceTTL time.Duration) error {
	if err := lm.extendLeaseExpiry(commandLeaseRef(cmd), graceTTL); err != nil {
		return err
	}
	lm.log(LogLevelDebug, "lease_grace_extend type=command id=%s epoch=%d grace_ttl=%s new_expires=%s",
		cmd.ID, cmd.LeaseEpoch, graceTTL, *cmd.LeaseExpiresAt)
	return nil
}

// ExtendTaskLeaseGrace extends the lease with a shorter grace TTL for undecided probes.
func (lm *LeaseManager) ExtendTaskLeaseGrace(task *model.Task, graceTTL time.Duration) error {
	if err := lm.extendLeaseExpiry(taskLeaseRef(task), graceTTL); err != nil {
		return err
	}
	lm.log(LogLevelDebug, "lease_grace_extend type=task id=%s epoch=%d grace_ttl=%s new_expires=%s",
		task.ID, task.LeaseEpoch, graceTTL, *task.LeaseExpiresAt)
	return nil
}

// IsLeaseExpired checks if a lease has expired. Returns true if expired.
func (lm *LeaseManager) IsLeaseExpired(leaseExpiresAt *string) bool {
	if leaseExpiresAt == nil {
		return true
	}
	expires, err := time.Parse(time.RFC3339, *leaseExpiresAt)
	if err != nil {
		return true
	}
	return lm.clock.Now().UTC().After(expires)
}

// IsLeaseNearExpiry checks if a lease will expire within the given buffer duration.
// Returns true if the lease expires within bufferSec seconds from now.
func (lm *LeaseManager) IsLeaseNearExpiry(leaseExpiresAt *string, bufferSec int) bool {
	if leaseExpiresAt == nil {
		return true
	}
	expires, err := time.Parse(time.RFC3339, *leaseExpiresAt)
	if err != nil {
		return true
	}
	return lm.clock.Now().UTC().Add(time.Duration(bufferSec) * time.Second).After(expires)
}

// ExpireCommands returns commands whose leases have expired (periodic scan step 2).
func (lm *LeaseManager) ExpireCommands(commands []model.Command) []int {
	var expired []int
	for i := range commands {
		cmd := &commands[i]
		if cmd.Status == model.StatusInProgress && lm.IsLeaseExpired(cmd.LeaseExpiresAt) {
			expired = append(expired, i)
			lm.log(LogLevelDebug, "lease_expired type=command id=%s epoch=%d owner=%s",
				cmd.ID, cmd.LeaseEpoch, ptrStr(cmd.LeaseOwner))
		}
	}
	return expired
}

// ExpireTasks returns tasks whose leases have expired (periodic scan step 2).
func (lm *LeaseManager) ExpireTasks(tasks []model.Task) []int {
	var expired []int
	for i := range tasks {
		task := &tasks[i]
		if task.Status == model.StatusInProgress && lm.IsLeaseExpired(task.LeaseExpiresAt) {
			expired = append(expired, i)
			lm.log(LogLevelInfo, "lease_expired type=task id=%s epoch=%d owner=%s",
				task.ID, task.LeaseEpoch, ptrStr(task.LeaseOwner))
		}
	}
	return expired
}

// RenewableCommands returns commands whose leases are approaching expiry but not yet expired.
// Used for preemptive renewal to avoid the expire→auto-extend cycle.
func (lm *LeaseManager) RenewableCommands(commands []model.Command, bufferSec int) []int {
	var renewable []int
	for i := range commands {
		cmd := &commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		if lm.IsLeaseExpired(cmd.LeaseExpiresAt) {
			continue // already expired; handled by ExpireCommands
		}
		if lm.IsLeaseNearExpiry(cmd.LeaseExpiresAt, bufferSec) {
			renewable = append(renewable, i)
			lm.log(LogLevelDebug, "lease_near_expiry type=command id=%s epoch=%d",
				cmd.ID, cmd.LeaseEpoch)
		}
	}
	return renewable
}

// ExpireNotifications returns notifications whose leases have expired.
func (lm *LeaseManager) ExpireNotifications(notifications []model.Notification) []int {
	var expired []int
	for i := range notifications {
		ntf := &notifications[i]
		if ntf.Status == model.StatusInProgress && lm.IsLeaseExpired(ntf.LeaseExpiresAt) {
			expired = append(expired, i)
			lm.log(LogLevelWarn, "lease_expired type=notification id=%s epoch=%d owner=%s",
				ntf.ID, ntf.LeaseEpoch, ptrStr(ntf.LeaseOwner))
		}
	}
	return expired
}

func ptrStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func (lm *LeaseManager) log(level LogLevel, format string, args ...any) {
	lm.dl.Logf(level, format, args...)
}
