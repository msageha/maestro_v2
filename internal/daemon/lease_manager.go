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
	logger           *log.Logger
	logLevel         LogLevel
}

// NewLeaseManager creates a new LeaseManager.
func NewLeaseManager(cfg model.WatcherConfig, logger *log.Logger, logLevel LogLevel) *LeaseManager {
	dispatchLease := cfg.DispatchLeaseSec
	if dispatchLease <= 0 {
		dispatchLease = 300
	}
	maxInProgress := cfg.MaxInProgressMin
	if maxInProgress <= 0 {
		maxInProgress = 60
	}
	return &LeaseManager{
		dispatchLeaseSec: dispatchLease,
		maxInProgressMin: maxInProgress,
		logger:           logger,
		logLevel:         logLevel,
	}
}

// LeaseInfo represents lease metadata for a queue entry.
type LeaseInfo struct {
	LeaseOwner     *string
	LeaseExpiresAt *string
	LeaseEpoch     int
}

// AcquireCommandLease transitions a command from pending to in_progress with lease.
func (lm *LeaseManager) AcquireCommandLease(cmd *model.Command, owner string) error {
	if err := model.ValidateCommandTaskQueueTransition(cmd.Status, model.StatusInProgress); err != nil {
		return fmt.Errorf("cannot acquire lease: %w", err)
	}

	now := time.Now().UTC()
	expires := now.Add(time.Duration(lm.dispatchLeaseSec) * time.Second)

	cmd.Status = model.StatusInProgress
	ownerStr := owner
	cmd.LeaseOwner = &ownerStr
	expiresStr := expires.Format(time.RFC3339)
	cmd.LeaseExpiresAt = &expiresStr
	cmd.LeaseEpoch++
	cmd.UpdatedAt = now.Format(time.RFC3339)

	lm.log(LogLevelInfo, "lease_acquire type=command id=%s owner=%s epoch=%d expires=%s",
		cmd.ID, owner, cmd.LeaseEpoch, expiresStr)
	return nil
}

// AcquireTaskLease transitions a task from pending to in_progress with lease.
func (lm *LeaseManager) AcquireTaskLease(task *model.Task, owner string) error {
	if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusInProgress); err != nil {
		return fmt.Errorf("cannot acquire lease: %w", err)
	}

	now := time.Now().UTC()
	expires := now.Add(time.Duration(lm.dispatchLeaseSec) * time.Second)

	task.Status = model.StatusInProgress
	ownerStr := owner
	task.LeaseOwner = &ownerStr
	expiresStr := expires.Format(time.RFC3339)
	task.LeaseExpiresAt = &expiresStr
	task.LeaseEpoch++
	task.UpdatedAt = now.Format(time.RFC3339)

	lm.log(LogLevelInfo, "lease_acquire type=task id=%s owner=%s epoch=%d expires=%s",
		task.ID, owner, task.LeaseEpoch, expiresStr)
	return nil
}

// AcquireNotificationLease transitions a notification from pending to in_progress with lease.
func (lm *LeaseManager) AcquireNotificationLease(ntf *model.Notification, owner string) error {
	if err := model.ValidateNotificationQueueTransition(ntf.Status, model.StatusInProgress); err != nil {
		return fmt.Errorf("cannot acquire lease: %w", err)
	}

	now := time.Now().UTC()
	expires := now.Add(time.Duration(lm.dispatchLeaseSec) * time.Second)

	ntf.Status = model.StatusInProgress
	ownerStr := owner
	ntf.LeaseOwner = &ownerStr
	expiresStr := expires.Format(time.RFC3339)
	ntf.LeaseExpiresAt = &expiresStr
	ntf.LeaseEpoch++
	ntf.UpdatedAt = now.Format(time.RFC3339)

	lm.log(LogLevelInfo, "lease_acquire type=notification id=%s owner=%s epoch=%d expires=%s",
		ntf.ID, owner, ntf.LeaseEpoch, expiresStr)
	return nil
}

// ReleaseCommandLease transitions a command from in_progress back to pending with cleared lease.
func (lm *LeaseManager) ReleaseCommandLease(cmd *model.Command) error {
	if err := model.ValidateCommandTaskQueueTransition(cmd.Status, model.StatusPending); err != nil {
		return fmt.Errorf("cannot release lease: %w", err)
	}

	cmd.Status = model.StatusPending
	cmd.LeaseOwner = nil
	cmd.LeaseExpiresAt = nil
	cmd.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	lm.log(LogLevelInfo, "lease_release type=command id=%s epoch=%d", cmd.ID, cmd.LeaseEpoch)
	return nil
}

// ReleaseTaskLease transitions a task from in_progress back to pending with cleared lease.
func (lm *LeaseManager) ReleaseTaskLease(task *model.Task) error {
	if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusPending); err != nil {
		return fmt.Errorf("cannot release lease: %w", err)
	}

	task.Status = model.StatusPending
	task.LeaseOwner = nil
	task.LeaseExpiresAt = nil
	task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	lm.log(LogLevelInfo, "lease_release type=task id=%s epoch=%d", task.ID, task.LeaseEpoch)
	return nil
}

// ReleaseNotificationLease transitions a notification back to pending with cleared lease.
func (lm *LeaseManager) ReleaseNotificationLease(ntf *model.Notification) error {
	if err := model.ValidateNotificationQueueTransition(ntf.Status, model.StatusPending); err != nil {
		return fmt.Errorf("cannot release lease: %w", err)
	}

	ntf.Status = model.StatusPending
	ntf.LeaseOwner = nil
	ntf.LeaseExpiresAt = nil
	ntf.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	lm.log(LogLevelInfo, "lease_release type=notification id=%s epoch=%d", ntf.ID, ntf.LeaseEpoch)
	return nil
}

// ExtendCommandLease extends the lease expiration for an in_progress command.
// NOTE: UpdatedAt is intentionally NOT updated here. It must retain the original
// dispatch timestamp from AcquireCommandLease so that max_in_progress_min checks
// in recoverExpiredCommandLeases work correctly.
func (lm *LeaseManager) ExtendCommandLease(cmd *model.Command) error {
	if cmd.Status != model.StatusInProgress {
		return fmt.Errorf("cannot extend lease: command %s is %s, not in_progress", cmd.ID, cmd.Status)
	}

	expires := time.Now().UTC().Add(time.Duration(lm.dispatchLeaseSec) * time.Second)
	expiresStr := expires.Format(time.RFC3339)
	cmd.LeaseExpiresAt = &expiresStr

	lm.log(LogLevelDebug, "lease_extend type=command id=%s epoch=%d new_expires=%s",
		cmd.ID, cmd.LeaseEpoch, expiresStr)
	return nil
}

// ExtendTaskLease extends the lease expiration for an in_progress task.
// NOTE: UpdatedAt is intentionally NOT updated here. It must retain the original
// dispatch timestamp from AcquireTaskLease so that max_in_progress_min checks
// in recoverExpiredTaskLeases work correctly.
func (lm *LeaseManager) ExtendTaskLease(task *model.Task) error {
	if task.Status != model.StatusInProgress {
		return fmt.Errorf("cannot extend lease: task %s is %s, not in_progress", task.ID, task.Status)
	}

	expires := time.Now().UTC().Add(time.Duration(lm.dispatchLeaseSec) * time.Second)
	expiresStr := expires.Format(time.RFC3339)
	task.LeaseExpiresAt = &expiresStr

	lm.log(LogLevelDebug, "lease_extend type=task id=%s epoch=%d new_expires=%s",
		task.ID, task.LeaseEpoch, expiresStr)
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
	return time.Now().UTC().After(expires)
}

// ExpireCommands returns commands whose leases have expired (periodic scan step 2).
func (lm *LeaseManager) ExpireCommands(commands []model.Command) []int {
	var expired []int
	for i := range commands {
		cmd := &commands[i]
		if cmd.Status == model.StatusInProgress && lm.IsLeaseExpired(cmd.LeaseExpiresAt) {
			expired = append(expired, i)
			lm.log(LogLevelWarn, "lease_expired type=command id=%s epoch=%d owner=%s",
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
			lm.log(LogLevelWarn, "lease_expired type=task id=%s epoch=%d owner=%s",
				task.ID, task.LeaseEpoch, ptrStr(task.LeaseOwner))
		}
	}
	return expired
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

// HasInFlightForOwner checks if an owner already has an in_progress entry.
// Enforces at-most-one-in-flight invariant per agent.
func (lm *LeaseManager) HasInFlightForOwner(tasks []model.Task, owner string) bool {
	for _, task := range tasks {
		if task.Status == model.StatusInProgress && task.LeaseOwner != nil && *task.LeaseOwner == owner {
			return true
		}
	}
	return false
}

func ptrStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func (lm *LeaseManager) log(level LogLevel, format string, args ...any) {
	if level < lm.logLevel {
		return
	}
	levelStr := "INFO"
	switch level {
	case LogLevelDebug:
		levelStr = "DEBUG"
	case LogLevelWarn:
		levelStr = "WARN"
	case LogLevelError:
		levelStr = "ERROR"
	}
	msg := fmt.Sprintf(format, args...)
	lm.logger.Printf("%s %s lease_manager: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}
