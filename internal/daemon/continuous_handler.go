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
	logger     *log.Logger
	logLevel   LogLevel
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
		logger:     logger,
		logLevel:   logLevel,
	}
}

// CheckAndAdvance is called after a successful orchestrator notification of a command result.
// It increments the iteration counter and checks for stop/pause conditions.
func (ch *ContinuousHandler) CheckAndAdvance(commandID string, commandStatus model.Status) error {
	if !ch.config.Continuous.Enabled {
		return nil
	}

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

	// Increment iteration
	state.CurrentIteration++
	state.LastCommandID = &commandID
	now := time.Now().UTC().Format(time.RFC3339)
	state.UpdatedAt = &now

	// Check pause_on_failure
	if ch.config.Continuous.PauseOnFailure && commandStatus == model.StatusFailed {
		state.Status = model.ContinuousStatusPaused
		reason := "task_failure"
		state.PausedReason = &reason
		ch.log(LogLevelInfo, "continuous_pause command=%s reason=%s iteration=%d", commandID, reason, state.CurrentIteration)
	}

	// Check max_iterations (only if still running — pause takes precedence)
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
	if level < ch.logLevel {
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
	ch.logger.Printf("%s %s continuous: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}
