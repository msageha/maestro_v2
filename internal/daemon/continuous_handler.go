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

	state.CurrentIteration++
	state.LastCommandID = &commandID
	now := ch.clock.Now().UTC().Format(time.RFC3339)
	state.UpdatedAt = now

	// Track consecutive failures for the pre-generation gate. Reset on any non-failed
	// command so transient failures recover automatically.
	if commandStatus == model.StatusFailed {
		state.ConsecutiveFailures++
	} else {
		state.ConsecutiveFailures = 0
	}

	// Pre-generation gate: stop when consecutive failures reach the configured threshold.
	// MaxConsecutiveFailures == 0 means disabled. This is checked BEFORE the
	// pause_on_failure transition so that the hard stop fires independently of
	// pause_on_failure (otherwise pause would short-circuit Status from Running
	// to Paused and the gate guard would never fire on the threshold iteration).
	if ch.config.Continuous.MaxConsecutiveFailures > 0 &&
		state.ConsecutiveFailures >= ch.config.Continuous.MaxConsecutiveFailures {
		state.Status = model.ContinuousStatusStopped
		reason := "max_consecutive_failures_reached"
		state.PausedReason = &reason
		ch.log(LogLevelInfo, "continuous_stop reason=%s iteration=%d consecutive_failures=%d max=%d",
			reason, state.CurrentIteration, state.ConsecutiveFailures, ch.config.Continuous.MaxConsecutiveFailures)
	}

	// Check pause_on_failure (only if still running — gate hard-stop takes precedence).
	if state.Status == model.ContinuousStatusRunning &&
		ch.config.Continuous.PauseOnFailure && commandStatus == model.StatusFailed {
		state.Status = model.ContinuousStatusPaused
		reason := "task_failure"
		state.PausedReason = &reason
		ch.log(LogLevelInfo, "continuous_pause command=%s reason=%s iteration=%d", commandID, reason, state.CurrentIteration)
	}

	// Check max_iterations (only if still running — pause takes precedence).
	// MaxIterations == 0 means unlimited (no cap); only positive values enforce a stop.
	if state.Status == model.ContinuousStatusRunning && state.MaxIterations > 0 && state.CurrentIteration >= state.MaxIterations {
		state.Status = model.ContinuousStatusStopped
		reason := "max_iterations_reached"
		state.PausedReason = &reason
		ch.log(LogLevelInfo, "continuous_stop reason=%s iteration=%d max=%d", reason, state.CurrentIteration, state.MaxIterations)
	}

	return ch.saveContinuousState(state)
}

func (ch *ContinuousHandler) loadContinuousState() (*model.Continuous, error) {
	statePath := filepath.Join(ch.maestroDir, "state", "continuous.yaml")
	data, err := os.ReadFile(statePath)
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
	if err := os.MkdirAll(filepath.Dir(statePath), 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	return yamlutil.AtomicWrite(statePath, state)
}

func (ch *ContinuousHandler) log(level LogLevel, format string, args ...any) {
	ch.dl.Logf(level, format, args...)
}
