package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// ContinuousHandler manages continuous mode iteration tracking.
type ContinuousHandler struct {
	maestroDir string
	config     model.Config
	lockMap    *lock.MutexMap
	dl         *DaemonLogger
	logger     *log.Logger
	logLevel   LogLevel
	clock      Clock

	// stallMu guards stallNotifiedKey, the once-per-stall marker for
	// CheckStall. Delivered notifications are archived out of the queue
	// file, so queue-side dedup cannot prevent re-notification across
	// scans — this in-memory key does. A daemon restart re-notifies once,
	// which is acceptable for an advisory nudge.
	stallMu          sync.Mutex
	stallNotifiedKey string
}

// NewContinuousHandler creates a new ContinuousHandler.
func NewContinuousHandler(
	maestroDir string,
	cfg model.Config,
	lockMap *lock.MutexMap,
	logger *log.Logger,
	logLevel LogLevel,
) *ContinuousHandler {
	return &ContinuousHandler{
		maestroDir: maestroDir,
		config:     cfg,
		lockMap:    lockMap,
		dl:         NewDaemonLoggerFromLegacy("continuous", logger, logLevel),
		logger:     logger,
		logLevel:   logLevel,
		clock:      RealClock{},
	}
}

// CheckAndAdvance is called after a successful orchestrator notification of a command result.
// It increments the iteration counter and checks for stop/pause conditions.
func (ch *ContinuousHandler) CheckAndAdvance(commandID string, commandStatus model.Status) error {
	if !ch.config.Continuous.Enabled {
		return nil
	}

	// Reconciliation with state.plan_status (defense in depth — see also the
	// recovery guard in collectWorktreePublishAndCleanup). The CommandResult
	// passed in here can lag behind reality:
	//
	//   - A stale synthetic_failure result written before R3/R4 propagated
	//     a successful recovery would otherwise trigger pause_on_failure.
	//   - The Orchestrator-side status reported via NotifyOrchestrator may
	//     reflect what was true at write-time, but the daemon's state file
	//     is updated atomically by R4PlanStatus and is the canonical source.
	//
	// When state.plan_status disagrees with the passed-in commandStatus, we
	// trust the state file. A mismatch is logged at warn so the discrepancy
	// is observable in daemon.log without silently masking errors.
	if effectiveStatus, mismatch := ch.reconcileCommandStatusWithPlanStatus(commandID, commandStatus); mismatch {
		ch.log(LogLevelWarn,
			"continuous_status_reconciled command=%s passed_in=%s plan_status_derived=%s "+
				"(trusting state.plan_status — likely stale CommandResult or in-flight recovery)",
			commandID, commandStatus, effectiveStatus)
		commandStatus = effectiveStatus
	}

	// Lock order: state:continuous is held only while reading and persisting
	// continuous.yaml. Queue notifications are emitted after this lock is
	// released so the canonical queue:* -> state:* order is never inverted.
	ch.lockMap.Lock("state:continuous")
	stateLocked := true
	defer func() {
		if stateLocked {
			ch.lockMap.Unlock("state:continuous")
		}
	}()

	state, err := ch.loadContinuousState()
	if err != nil {
		return fmt.Errorf("load continuous state: %w", err)
	}

	// Only advance when running
	if state.Status != model.ContinuousStatusRunning {
		return nil
	}

	// Idempotency: skip if this command was already processed
	if state.LastCommandID != nil && *state.LastCommandID == commandID {
		return nil
	}

	// Compute new state values on a snapshot before applying mutations.
	// This ensures all-or-nothing: either all fields are updated consistently
	// and persisted, or the on-disk state remains unchanged.
	newIteration := state.CurrentIteration + 1
	newConsecutiveFailures := state.ConsecutiveFailures
	if commandStatus == model.StatusFailed {
		newConsecutiveFailures++
	} else {
		newConsecutiveFailures = 0
	}

	newStatus := state.Status
	var newPausedReason *string

	// Pre-generation gate: stop when consecutive failures reach the configured threshold.
	// MaxConsecutiveFailures == 0 means disabled. This is checked BEFORE the
	// pause_on_failure transition so that the hard stop fires independently of
	// pause_on_failure (otherwise pause would short-circuit Status from Running
	// to Paused and the gate guard would never fire on the threshold iteration).
	if ch.config.Continuous.MaxConsecutiveFailures > 0 &&
		newConsecutiveFailures >= ch.config.Continuous.MaxConsecutiveFailures {
		newStatus = model.ContinuousStatusStopped
		reason := "max_consecutive_failures_reached"
		newPausedReason = &reason
	}

	// Check pause_on_failure (only if still running — gate hard-stop takes precedence).
	if newStatus == model.ContinuousStatusRunning &&
		ch.config.Continuous.PauseOnFailure && commandStatus == model.StatusFailed {
		newStatus = model.ContinuousStatusPaused
		reason := "task_failure"
		newPausedReason = &reason
	}

	// Check max_iterations (only if still running — pause takes precedence).
	// MaxIterations == 0 means unlimited (no cap); only positive values enforce a stop.
	if newStatus == model.ContinuousStatusRunning && state.MaxIterations > 0 && newIteration >= state.MaxIterations {
		newStatus = model.ContinuousStatusStopped
		reason := "max_iterations_reached"
		newPausedReason = &reason
	}

	// Capture previous status to detect genuine transitions for notification emission.
	prevStatus := state.Status

	// Apply all mutations atomically
	now := ch.clock.Now().UTC().Format(time.RFC3339)
	state.CurrentIteration = newIteration
	state.LastCommandID = &commandID
	state.ConsecutiveFailures = newConsecutiveFailures
	state.Status = newStatus
	state.UpdatedAt = now
	if newPausedReason != nil {
		state.PausedReason = newPausedReason
	}

	if err := ch.saveContinuousState(state); err != nil {
		return fmt.Errorf("save continuous state: %w", err)
	}

	// Log status transitions after successful persist
	if newPausedReason != nil {
		switch newStatus {
		case model.ContinuousStatusStopped:
			ch.log(LogLevelInfo, "continuous_stop reason=%s iteration=%d consecutive_failures=%d",
				*newPausedReason, newIteration, newConsecutiveFailures)
		case model.ContinuousStatusPaused:
			ch.log(LogLevelInfo, "continuous_pause command=%s reason=%s iteration=%d",
				commandID, *newPausedReason, newIteration)
		}
	}

	// Emit an Orchestrator notification on Running -> Paused/Stopped transitions so the
	// Orchestrator can surface the pause/stop reason to the user and halt auto-generation.
	// Skipped when status did not actually change (e.g. already paused) to avoid duplicates.
	if prevStatus != newStatus {
		reason := ""
		if newPausedReason != nil {
			reason = *newPausedReason
		}
		ch.lockMap.Unlock("state:continuous")
		stateLocked = false
		switch newStatus {
		case model.ContinuousStatusPaused:
			if err := ch.writeContinuousTransitionNotification(
				commandID, newIteration, newStatus, reason,
			); err != nil {
				ch.log(LogLevelWarn, "continuous_notification_failed status=paused command=%s err=%v", commandID, err)
			}
		case model.ContinuousStatusStopped:
			if err := ch.writeContinuousTransitionNotification(
				commandID, newIteration, newStatus, reason,
			); err != nil {
				ch.log(LogLevelWarn, "continuous_notification_failed status=stopped command=%s err=%v", commandID, err)
			}
		}
	}

	return nil
}

// writeContinuousTransitionNotification appends (or dedups) a notification into
// queue/orchestrator.yaml describing a Running -> Paused/Stopped transition.
// Dedup key: (SourceResultID, Type) where SourceResultID is synthesised from the
// iteration + commandID so that re-entrancy (e.g. two fsnotify wakeups) does not
// enqueue the same transition twice.
func (ch *ContinuousHandler) writeContinuousTransitionNotification(
	commandID string,
	iteration int,
	newStatus model.ContinuousStatus,
	reason string,
) error {
	var notifType model.NotificationType
	switch newStatus {
	case model.ContinuousStatusPaused:
		notifType = model.NotificationTypeContinuousPaused
	case model.ContinuousStatusStopped:
		notifType = model.NotificationTypeContinuousStopped
	default:
		return nil
	}

	// Synthetic source_result_id — one per (iteration, commandID, status) tuple.
	sourceID := fmt.Sprintf("continuous:%s:%d:%s", newStatus, iteration, commandID)

	queuePath := notificationQueuePath(ch.maestroDir)

	ch.lockMap.Lock("queue:orchestrator")
	defer ch.lockMap.Unlock("queue:orchestrator")

	return updateYAMLFile(queuePath, func(nq *model.NotificationQueue) error {
		if nq.SchemaVersion == 0 {
			nq.SchemaVersion = 1
			nq.FileType = "queue_notification"
		}

		// Idempotency: skip if an entry with the same (SourceResultID, Type) exists.
		for i := range nq.Notifications {
			if nq.Notifications[i].SourceResultID == sourceID &&
				nq.Notifications[i].Type == notifType {
				return errNoUpdate
			}
		}

		id, err := model.GenerateID(model.IDTypeNotification)
		if err != nil {
			return fmt.Errorf("generate notification ID: %w", err)
		}

		now := ch.clock.Now().UTC().Format(time.RFC3339)
		content := fmt.Sprintf("continuous %s iteration=%d reason=%s last_command=%s",
			newStatus, iteration, reason, commandID)

		nq.Notifications = append(nq.Notifications, model.Notification{
			ID:             id,
			CommandID:      commandID,
			Type:           notifType,
			SourceResultID: sourceID,
			Content:        content,
			Priority:       defaultNotificationPriority,
			Status:         model.StatusPending,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
		return nil
	})
}

// CheckStall detects a silent continuous-mode stall: status=running, the last
// iteration's result was already processed (LastCommandID set), no
// non-terminal command remains in the planner queue, and more than
// continuous.stall_notification_sec has elapsed since the state last
// advanced. The Orchestrator owns next-iteration generation (the daemon never
// auto-submits), so a missed command_completed notification — compaction,
// crashed pane, lost paste — stalls the loop while every component looks
// nominally healthy. This watchdog re-surfaces the stall as a
// continuous_stalled notification. Called from the daemon ticker loop;
// notified once per stalled iteration via stallNotifiedKey.
func (ch *ContinuousHandler) CheckStall() {
	if !ch.config.Continuous.Enabled {
		return
	}
	thresholdSec := ch.config.Continuous.EffectiveStallNotificationSec()
	if thresholdSec <= 0 {
		return
	}

	ch.lockMap.Lock("state:continuous")
	state, err := ch.loadContinuousState()
	ch.lockMap.Unlock("state:continuous")
	if err != nil {
		ch.log(LogLevelDebug, "continuous_stall_check load_state error=%v", err)
		return
	}
	if state.Status != model.ContinuousStatusRunning || state.LastCommandID == nil {
		return
	}
	advancedAt, err := time.Parse(time.RFC3339, state.UpdatedAt)
	if err != nil {
		ch.log(LogLevelDebug, "continuous_stall_check parse_updated_at error=%v", err)
		return
	}
	stalledFor := ch.clock.Now().UTC().Sub(advancedAt)
	if stalledFor < time.Duration(thresholdSec)*time.Second {
		return
	}

	active, err := ch.hasNonTerminalCommands()
	if err != nil {
		ch.log(LogLevelDebug, "continuous_stall_check load_queue error=%v", err)
		return
	}
	if active {
		return
	}

	key := fmt.Sprintf("%d:%s:%s", state.CurrentIteration, *state.LastCommandID, state.UpdatedAt)
	ch.stallMu.Lock()
	alreadyNotified := ch.stallNotifiedKey == key
	ch.stallMu.Unlock()
	if alreadyNotified {
		return
	}

	if err := ch.writeContinuousStallNotification(state, stalledFor); err != nil {
		ch.log(LogLevelWarn, "continuous_stall_notification_failed iteration=%d err=%v",
			state.CurrentIteration, err)
		return
	}
	ch.stallMu.Lock()
	ch.stallNotifiedKey = key
	ch.stallMu.Unlock()
	ch.log(LogLevelInfo, "continuous_stall_notified iteration=%d last_command=%s stalled_for=%s",
		state.CurrentIteration, *state.LastCommandID, stalledFor.Truncate(time.Second))
}

// hasNonTerminalCommands reports whether the planner command queue holds any
// command that has not reached a terminal status — i.e. work is still queued
// or in flight and the continuous loop is not stalled.
func (ch *ContinuousHandler) hasNonTerminalCommands() (bool, error) {
	ch.lockMap.Lock("queue:planner")
	defer ch.lockMap.Unlock("queue:planner")
	data, err := os.ReadFile(commandQueuePath(ch.maestroDir)) //nolint:gosec // controlled application queue path
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return false, err
	}
	for i := range cq.Commands {
		if !model.IsTerminal(cq.Commands[i].Status) {
			return true, nil
		}
	}
	return false, nil
}

// writeContinuousStallNotification appends (or dedups) a continuous_stalled
// notification into queue/orchestrator.yaml. The queue-side dedup only covers
// the window where a previous stall notification is still pending; long-term
// once-per-stall control lives in stallNotifiedKey (see CheckStall).
func (ch *ContinuousHandler) writeContinuousStallNotification(state *model.Continuous, stalledFor time.Duration) error {
	sourceID := fmt.Sprintf("continuous:stalled:%d:%s", state.CurrentIteration, *state.LastCommandID)
	queuePath := notificationQueuePath(ch.maestroDir)

	ch.lockMap.Lock("queue:orchestrator")
	defer ch.lockMap.Unlock("queue:orchestrator")

	return updateYAMLFile(queuePath, func(nq *model.NotificationQueue) error {
		if nq.SchemaVersion == 0 {
			nq.SchemaVersion = 1
			nq.FileType = "queue_notification"
		}
		for i := range nq.Notifications {
			if nq.Notifications[i].SourceResultID == sourceID &&
				nq.Notifications[i].Type == model.NotificationTypeContinuousStalled {
				return errNoUpdate
			}
		}
		id, err := model.GenerateID(model.IDTypeNotification)
		if err != nil {
			return fmt.Errorf("generate notification ID: %w", err)
		}
		now := ch.clock.Now().UTC().Format(time.RFC3339)
		content := fmt.Sprintf(
			"continuous stalled iteration=%d last_command=%s stalled_for=%s "+
				"(running but no next command submitted — run the Decide step for the next iteration, or pause/stop continuous mode)",
			state.CurrentIteration, *state.LastCommandID, stalledFor.Truncate(time.Second))
		nq.Notifications = append(nq.Notifications, model.Notification{
			ID:             id,
			CommandID:      *state.LastCommandID,
			Type:           model.NotificationTypeContinuousStalled,
			SourceResultID: sourceID,
			Content:        content,
			Priority:       defaultNotificationPriority,
			Status:         model.StatusPending,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
		return nil
	})
}

// reconcileCommandStatusWithPlanStatus reads state/commands/<commandID>.yaml
// and returns (effectiveStatus, mismatch). The effective status is derived
// from the command's authoritative plan_status:
//
//   - PlanStatusCompleted → StatusCompleted
//   - PlanStatusFailed    → StatusFailed
//   - PlanStatusCancelled → StatusCancelled
//   - other / unknown / error → returns the passed-in commandStatus
//
// mismatch is true only when the derived status differs from the passed-in
// status. State-file read errors fail open: we trust the input rather than
// refuse to advance, so a transient read failure cannot indefinitely stall
// continuous mode.
func (ch *ContinuousHandler) reconcileCommandStatusWithPlanStatus(
	commandID string,
	passedIn model.Status,
) (model.Status, bool) {
	statePath := commandStatePath(ch.maestroDir, commandID)
	data, err := os.ReadFile(statePath) //nolint:gosec // controlled application state path
	if err != nil {
		// Missing state file is normal for non-phased commands or pre-Phase-A
		// arrivals — fall back to the passed-in status without flagging.
		return passedIn, false
	}
	if len(data) == 0 {
		return passedIn, false
	}
	var cs model.CommandState
	if err := yamlv3.Unmarshal(data, &cs); err != nil {
		ch.log(LogLevelDebug,
			"continuous_plan_status_parse_failed command=%s error=%v (using passed-in status)",
			commandID, err)
		return passedIn, false
	}
	var derived model.Status
	switch cs.PlanStatus {
	case model.PlanStatusCompleted:
		derived = model.StatusCompleted
	case model.PlanStatusFailed:
		derived = model.StatusFailed
	case model.PlanStatusCancelled:
		derived = model.StatusCancelled
	default:
		// Non-terminal plan_status (planning/sealed/etc) — R4PlanStatus
		// has not caught up yet. Derive directly from the live state so
		// continuous mode is not paused by a stale CommandResult while
		// the plan is, in fact, on its way to completed (the verify-
		// repair lineage path: predecessor cancelled, successor in
		// flight, R4 still queued behind the scan that observed the
		// failed phase). Lineage-aware DeriveStatus prevents a
		// superseded-but-completed retry from being mistaken for a
		// real failure. On derive errors we trust the input — fail
		// open rather than indefinitely stall continuous mode.
		live, deriveErr := plan.DeriveStatus(&cs)
		if deriveErr != nil {
			ch.log(LogLevelDebug,
				"continuous_status_derive_failed command=%s error=%v (using passed-in status)",
				commandID, deriveErr)
			return passedIn, false
		}
		switch live {
		case model.PlanStatusCompleted:
			derived = model.StatusCompleted
		case model.PlanStatusFailed:
			derived = model.StatusFailed
		case model.PlanStatusCancelled:
			derived = model.StatusCancelled
		default:
			return passedIn, false
		}
	}
	return derived, derived != passedIn
}

func (ch *ContinuousHandler) loadContinuousState() (*model.Continuous, error) {
	statePath := filepath.Join(ch.maestroDir, "state", "continuous.yaml")
	data, err := os.ReadFile(statePath) //nolint:gosec // statePath is constructed from a controlled application state directory
	if err != nil {
		if os.IsNotExist(err) {
			return &model.Continuous{
				SchemaVersion: 1,
				FileType:      "state_continuous",
				Status:        model.ContinuousStatusStopped,
			}, nil
		}
		return nil, err
	}

	var state model.Continuous
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (ch *ContinuousHandler) saveContinuousState(state *model.Continuous) error {
	statePath := filepath.Join(ch.maestroDir, "state", "continuous.yaml")
	if err := os.MkdirAll(filepath.Dir(statePath), 0755); err != nil { //nolint:gosec // 0755 is appropriate for a state directory
		return fmt.Errorf("create state dir: %w", err)
	}
	return yamlutil.AtomicWrite(statePath, state)
}

func (ch *ContinuousHandler) log(level LogLevel, format string, args ...any) {
	ch.dl.Logf(level, format, args...)
}
