package lease

import (
	"fmt"
	"log"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// Manager handles lease lifecycle for queue entries.
type Manager struct {
	dispatchLeaseSec int
	maxInProgressMin int
	clock            core.Clock
	dl               *core.DaemonLogger
	logger           *log.Logger
	logLevel         core.LogLevel
}

// NewManager creates a new lease Manager.
func NewManager(cfg model.WatcherConfig, logger *log.Logger, logLevel core.LogLevel) *Manager {
	return NewManagerWithDeps(cfg, logger, logLevel, core.RealClock{})
}

// NewManagerWithDeps creates a Manager with explicit dependencies.
func NewManagerWithDeps(cfg model.WatcherConfig, logger *log.Logger, logLevel core.LogLevel, clock core.Clock) *Manager {
	dispatchLease := cfg.DispatchLeaseSec
	if dispatchLease <= 0 {
		dispatchLease = 300
	}
	maxInProgress := cfg.MaxInProgressMin
	if maxInProgress <= 0 {
		maxInProgress = 60
	}
	return &Manager{
		dispatchLeaseSec: dispatchLease,
		maxInProgressMin: maxInProgress,
		clock:            clock,
		dl:               core.NewDaemonLoggerFromLegacy("lease_manager", logger, logLevel),
		logger:           logger,
		logLevel:         logLevel,
	}
}

// Info represents lease metadata for a queue entry.
type Info struct {
	LeaseOwner     *string
	LeaseExpiresAt *string
	LeaseEpoch     int
}

// AcquireCommandLease transitions a command from pending to in_progress with lease.
func (lm *Manager) AcquireCommandLease(cmd *model.Command, owner string) error {
	if err := model.ValidateCommandTaskQueueTransition(cmd.Status, model.StatusInProgress); err != nil {
		return fmt.Errorf("cannot acquire lease: %w", err)
	}

	now := lm.clock.Now().UTC()
	expires := now.Add(time.Duration(lm.dispatchLeaseSec) * time.Second)

	cmd.Status = model.StatusInProgress
	ownerStr := owner
	cmd.LeaseOwner = &ownerStr
	expiresStr := expires.Format(time.RFC3339)
	cmd.LeaseExpiresAt = &expiresStr
	cmd.LeaseEpoch++
	cmd.UpdatedAt = now.Format(time.RFC3339)

	lm.log(core.LogLevelInfo, "lease_acquire type=command id=%s owner=%s epoch=%d expires=%s",
		cmd.ID, owner, cmd.LeaseEpoch, expiresStr)
	return nil
}

// AcquireTaskLease transitions a task from pending to in_progress with lease.
func (lm *Manager) AcquireTaskLease(task *model.Task, owner string) error {
	if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusInProgress); err != nil {
		return fmt.Errorf("cannot acquire lease: %w", err)
	}

	now := lm.clock.Now().UTC()
	expires := now.Add(time.Duration(lm.dispatchLeaseSec) * time.Second)

	task.Status = model.StatusInProgress
	ownerStr := owner
	task.LeaseOwner = &ownerStr
	expiresStr := expires.Format(time.RFC3339)
	task.LeaseExpiresAt = &expiresStr
	task.LeaseEpoch++
	nowStr := now.Format(time.RFC3339)
	if task.InProgressAt == nil {
		task.InProgressAt = &nowStr
	}
	task.UpdatedAt = nowStr

	lm.log(core.LogLevelInfo, "lease_acquire type=task id=%s owner=%s epoch=%d expires=%s",
		task.ID, owner, task.LeaseEpoch, expiresStr)
	return nil
}

// AcquireNotificationLease transitions a notification from pending to in_progress with lease.
func (lm *Manager) AcquireNotificationLease(ntf *model.Notification, owner string) error {
	if err := model.ValidateNotificationQueueTransition(ntf.Status, model.StatusInProgress); err != nil {
		return fmt.Errorf("cannot acquire lease: %w", err)
	}

	now := lm.clock.Now().UTC()
	expires := now.Add(time.Duration(lm.dispatchLeaseSec) * time.Second)

	ntf.Status = model.StatusInProgress
	ownerStr := owner
	ntf.LeaseOwner = &ownerStr
	expiresStr := expires.Format(time.RFC3339)
	ntf.LeaseExpiresAt = &expiresStr
	ntf.LeaseEpoch++
	ntf.UpdatedAt = now.Format(time.RFC3339)

	lm.log(core.LogLevelInfo, "lease_acquire type=notification id=%s owner=%s epoch=%d expires=%s",
		ntf.ID, owner, ntf.LeaseEpoch, expiresStr)
	return nil
}

// ReleaseCommandLease transitions a command from in_progress back to pending with cleared lease.
func (lm *Manager) ReleaseCommandLease(cmd *model.Command) error {
	if err := model.ValidateCommandTaskQueueTransition(cmd.Status, model.StatusPending); err != nil {
		return fmt.Errorf("cannot release lease: %w", err)
	}

	cmd.Status = model.StatusPending
	cmd.LeaseOwner = nil
	cmd.LeaseExpiresAt = nil
	cmd.UpdatedAt = lm.clock.Now().UTC().Format(time.RFC3339)

	lm.log(core.LogLevelInfo, "lease_release type=command id=%s epoch=%d", cmd.ID, cmd.LeaseEpoch)
	return nil
}

// ReleaseTaskLease transitions a task from in_progress back to pending with cleared lease.
func (lm *Manager) ReleaseTaskLease(task *model.Task) error {
	if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusPending); err != nil {
		return fmt.Errorf("cannot release lease: %w", err)
	}

	task.Status = model.StatusPending
	task.LeaseOwner = nil
	task.LeaseExpiresAt = nil
	task.UpdatedAt = lm.clock.Now().UTC().Format(time.RFC3339)

	lm.log(core.LogLevelInfo, "lease_release type=task id=%s epoch=%d", task.ID, task.LeaseEpoch)
	return nil
}

// ReleaseNotificationLease transitions a notification back to pending with cleared lease.
func (lm *Manager) ReleaseNotificationLease(ntf *model.Notification) error {
	if err := model.ValidateNotificationQueueTransition(ntf.Status, model.StatusPending); err != nil {
		return fmt.Errorf("cannot release lease: %w", err)
	}

	ntf.Status = model.StatusPending
	ntf.LeaseOwner = nil
	ntf.LeaseExpiresAt = nil
	ntf.UpdatedAt = lm.clock.Now().UTC().Format(time.RFC3339)

	lm.log(core.LogLevelInfo, "lease_release type=notification id=%s epoch=%d", ntf.ID, ntf.LeaseEpoch)
	return nil
}

// ExtendCommandLease extends the lease expiration for an in_progress command.
// NOTE: UpdatedAt is intentionally NOT updated here. It must retain the original
// dispatch timestamp from AcquireCommandLease so that max_in_progress_min checks
// in recoverExpiredCommandLeases work correctly.
func (lm *Manager) ExtendCommandLease(cmd *model.Command) error {
	if cmd.Status != model.StatusInProgress {
		return fmt.Errorf("cannot extend lease: command %s is %s, not in_progress", cmd.ID, cmd.Status)
	}

	expires := lm.clock.Now().UTC().Add(time.Duration(lm.dispatchLeaseSec) * time.Second)
	expiresStr := expires.Format(time.RFC3339)
	cmd.LeaseExpiresAt = &expiresStr

	lm.log(core.LogLevelDebug, "lease_extend type=command id=%s epoch=%d new_expires=%s",
		cmd.ID, cmd.LeaseEpoch, expiresStr)
	return nil
}

// ExtendTaskLease extends the lease expiration for an in_progress task.
// NOTE: UpdatedAt is intentionally NOT updated here. It must retain the original
// dispatch timestamp from AcquireTaskLease so that max_in_progress_min checks
// in recoverExpiredTaskLeases work correctly.
func (lm *Manager) ExtendTaskLease(task *model.Task) error {
	if task.Status != model.StatusInProgress {
		return fmt.Errorf("cannot extend lease: task %s is %s, not in_progress", task.ID, task.Status)
	}

	expires := lm.clock.Now().UTC().Add(time.Duration(lm.dispatchLeaseSec) * time.Second)
	expiresStr := expires.Format(time.RFC3339)
	task.LeaseExpiresAt = &expiresStr

	lm.log(core.LogLevelDebug, "lease_extend type=task id=%s epoch=%d new_expires=%s",
		task.ID, task.LeaseEpoch, expiresStr)
	return nil
}

// GraceLeaseTTL returns a shorter TTL for undecided busy probes.
// Formula: max(2 * scan_interval, dispatch_lease / 3), capped at dispatch_lease / 2.
// Floor: scan_interval + 10s to ensure the lease survives until the next scan.
func (lm *Manager) GraceLeaseTTL(scanIntervalSec int) time.Duration {
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
func (lm *Manager) ExtendCommandLeaseGrace(cmd *model.Command, graceTTL time.Duration) error {
	if cmd.Status != model.StatusInProgress {
		return fmt.Errorf("cannot extend lease: command %s is %s, not in_progress", cmd.ID, cmd.Status)
	}
	expires := lm.clock.Now().UTC().Add(graceTTL)
	expiresStr := expires.Format(time.RFC3339)
	cmd.LeaseExpiresAt = &expiresStr
	lm.log(core.LogLevelDebug, "lease_grace_extend type=command id=%s epoch=%d grace_ttl=%s new_expires=%s",
		cmd.ID, cmd.LeaseEpoch, graceTTL, expiresStr)
	return nil
}

// ExtendTaskLeaseGrace extends the lease with a shorter grace TTL for undecided probes.
func (lm *Manager) ExtendTaskLeaseGrace(task *model.Task, graceTTL time.Duration) error {
	if task.Status != model.StatusInProgress {
		return fmt.Errorf("cannot extend lease: task %s is %s, not in_progress", task.ID, task.Status)
	}
	expires := lm.clock.Now().UTC().Add(graceTTL)
	expiresStr := expires.Format(time.RFC3339)
	task.LeaseExpiresAt = &expiresStr
	lm.log(core.LogLevelDebug, "lease_grace_extend type=task id=%s epoch=%d grace_ttl=%s new_expires=%s",
		task.ID, task.LeaseEpoch, graceTTL, expiresStr)
	return nil
}

// IsLeaseExpired checks if a lease has expired. Returns true if expired.
func (lm *Manager) IsLeaseExpired(leaseExpiresAt *string) bool {
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
func (lm *Manager) IsLeaseNearExpiry(leaseExpiresAt *string, bufferSec int) bool {
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
func (lm *Manager) ExpireCommands(commands []model.Command) []int {
	var expired []int
	for i := range commands {
		cmd := &commands[i]
		if cmd.Status == model.StatusInProgress && lm.IsLeaseExpired(cmd.LeaseExpiresAt) {
			expired = append(expired, i)
			lm.log(core.LogLevelDebug, "lease_expired type=command id=%s epoch=%d owner=%s",
				cmd.ID, cmd.LeaseEpoch, PtrStr(cmd.LeaseOwner))
		}
	}
	return expired
}

// ExpireTasks returns tasks whose leases have expired (periodic scan step 2).
func (lm *Manager) ExpireTasks(tasks []model.Task) []int {
	var expired []int
	for i := range tasks {
		task := &tasks[i]
		if task.Status == model.StatusInProgress && lm.IsLeaseExpired(task.LeaseExpiresAt) {
			expired = append(expired, i)
			lm.log(core.LogLevelInfo, "lease_expired type=task id=%s epoch=%d owner=%s",
				task.ID, task.LeaseEpoch, PtrStr(task.LeaseOwner))
		}
	}
	return expired
}

// RenewableCommands returns commands whose leases are approaching expiry but not yet expired.
// Used for preemptive renewal to avoid the expire→auto-extend cycle.
func (lm *Manager) RenewableCommands(commands []model.Command, bufferSec int) []int {
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
			lm.log(core.LogLevelDebug, "lease_near_expiry type=command id=%s epoch=%d",
				cmd.ID, cmd.LeaseEpoch)
		}
	}
	return renewable
}

// ExpireNotifications returns notifications whose leases have expired.
func (lm *Manager) ExpireNotifications(notifications []model.Notification) []int {
	var expired []int
	for i := range notifications {
		ntf := &notifications[i]
		if ntf.Status == model.StatusInProgress && lm.IsLeaseExpired(ntf.LeaseExpiresAt) {
			expired = append(expired, i)
			lm.log(core.LogLevelWarn, "lease_expired type=notification id=%s epoch=%d owner=%s",
				ntf.ID, ntf.LeaseEpoch, PtrStr(ntf.LeaseOwner))
		}
	}
	return expired
}

// HasInFlightForOwner checks if an owner already has an in_progress entry.
// Enforces at-most-one-in-flight invariant per agent.
func (lm *Manager) HasInFlightForOwner(tasks []model.Task, owner string) bool {
	for _, task := range tasks {
		if task.Status == model.StatusInProgress && task.LeaseOwner != nil && *task.LeaseOwner == owner {
			return true
		}
	}
	return false
}

// PtrStr returns the string value or "<nil>" for nil pointers.
func PtrStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func (lm *Manager) log(level core.LogLevel, format string, args ...any) {
	lm.dl.Logf(level, format, args...)
}
