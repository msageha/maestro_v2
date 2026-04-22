package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
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

	// Lock order: leaf lock under the state:* namespace; see daemon/doc.go.
	// Acquired in isolation — no state:{commandID} is held on this path.
	ch.lockMap.Lock("state:continuous")
	defer ch.lockMap.Unlock("state:continuous")

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

	queuePath := filepath.Join(ch.maestroDir, "queue", "orchestrator.yaml")

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
