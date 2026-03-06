package qualitygate

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/quality"
)

// Event represents an event that triggers quality gate evaluation.
type Event interface {
	EventType() string
	Timestamp() time.Time
}

// TaskStartEvent is emitted when a task starts execution.
type TaskStartEvent struct {
	TaskID    string
	CommandID string
	AgentID   string
	StartedAt time.Time
}

func (e TaskStartEvent) EventType() string { return "task_start" }
func (e TaskStartEvent) Timestamp() time.Time { return e.StartedAt }

// TaskCompleteEvent is emitted when a task completes (success or failure).
type TaskCompleteEvent struct {
	TaskID      string
	CommandID   string
	AgentID     string
	Status      model.Status
	CompletedAt time.Time
}

func (e TaskCompleteEvent) EventType() string { return "task_complete" }
func (e TaskCompleteEvent) Timestamp() time.Time { return e.CompletedAt }

// PhaseTransitionEvent is emitted when a phase changes status.
type PhaseTransitionEvent struct {
	PhaseID        string
	CommandID      string
	OldStatus      model.PhaseStatus
	NewStatus      model.PhaseStatus
	TransitionedAt time.Time
}

func (e PhaseTransitionEvent) EventType() string { return "phase_transition" }
func (e PhaseTransitionEvent) Timestamp() time.Time { return e.TransitionedAt }

// Metrics tracks quality gate evaluation metrics.
type Metrics struct {
	mu                    sync.RWMutex
	evaluationCount       int64
	successCount          int64
	failureCount          int64
	totalEvaluationTimeMs int64
}

func (m *Metrics) RecordEvaluation(success bool, durationMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evaluationCount++
	m.totalEvaluationTimeMs += durationMs
	if success {
		m.successCount++
	} else {
		m.failureCount++
	}
}

func (m *Metrics) GetStats() (evalCount, successCount, failureCount, avgTimeMs int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	evalCount = m.evaluationCount
	successCount = m.successCount
	failureCount = m.failureCount
	if m.evaluationCount > 0 {
		avgTimeMs = m.totalEvaluationTimeMs / m.evaluationCount
	}
	return
}

// Daemon manages quality gate evaluations independently from the core daemon loop.
type Daemon struct {
	maestroDir string
	config     model.Config
	lockMap    *lock.MutexMap
	dl         *core.DaemonLogger
	logger     *log.Logger
	logLevel   core.LogLevel
	clock      core.Clock

	eventChan         chan Event
	metrics           *Metrics
	stopped           atomic.Bool
	stopOnce          sync.Once
	engine            *quality.Engine
	evaluationTimeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewDaemon creates a new quality gate Daemon instance.
func NewDaemon(
	maestroDir string,
	cfg model.Config,
	lockMap *lock.MutexMap,
	logger *log.Logger,
	logLevel core.LogLevel,
	parentCtx context.Context,
) *Daemon {
	ctx, cancel := context.WithCancel(parentCtx)
	return &Daemon{
		maestroDir:        maestroDir,
		config:            cfg,
		lockMap:           lockMap,
		dl:                core.NewDaemonLoggerFromLegacy("quality_gate", logger, logLevel),
		logger:            logger,
		logLevel:          logLevel,
		clock:             core.RealClock{},
		eventChan:         make(chan Event, 100),
		metrics:           &Metrics{},
		engine:            quality.NewEngine(),
		evaluationTimeout: 100 * time.Millisecond,
		ctx:               ctx,
		cancel:            cancel,
	}
}

func (qg *Daemon) Start() error {
	qg.log(core.LogLevelInfo, "quality_gate_daemon starting")

	if err := qg.loadGateDefinitions(); err != nil {
		qg.log(core.LogLevelWarn, "quality_gate_daemon failed to load definitions: %v", err)
	}

	qg.wg.Add(1)
	go qg.eventLoop()

	qg.log(core.LogLevelInfo, "quality_gate_daemon started")
	return nil
}

func (qg *Daemon) Stop() error {
	qg.stopOnce.Do(func() {
		qg.log(core.LogLevelInfo, "quality_gate_daemon stopping")
		qg.stopped.Store(true)
		qg.cancel()
		qg.wg.Wait()
		qg.log(core.LogLevelInfo, "quality_gate_daemon stopped")
	})
	return nil
}

// EmitEvent sends an event to the quality gate daemon for processing.
// Implements core.QualityGateEvaluator.
func (qg *Daemon) EmitEvent(event interface{}) {
	typedEvent, ok := event.(Event)
	if !ok {
		qg.log(core.LogLevelWarn, "quality_gate_event_invalid not a qualitygate.Event")
		return
	}

	if qg.stopped.Load() {
		qg.log(core.LogLevelDebug, "quality_gate_event_dropped type=%s reason=stopped", typedEvent.EventType())
		return
	}

	select {
	case qg.eventChan <- typedEvent:
		qg.log(core.LogLevelDebug, "quality_gate_event_queued type=%s", typedEvent.EventType())
	case <-qg.ctx.Done():
		qg.log(core.LogLevelDebug, "quality_gate_event_dropped type=%s reason=shutdown", typedEvent.EventType())
	default:
		qg.log(core.LogLevelWarn, "quality_gate_event_dropped type=%s reason=channel_full", typedEvent.EventType())
	}
}

// EvaluateGateWithResult evaluates gates and returns the result.
// Implements core.QualityGateEvaluator.
func (qg *Daemon) EvaluateGateWithResult(gateType string, evalContext map[string]interface{}) (core.QualityGateResult, error) {
	qualityGateType, err := mapGateType(gateType)
	if err != nil {
		return core.QualityGateResult{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), qg.evaluationTimeout)
	defer cancel()

	result, err := qg.engine.Evaluate(ctx, qualityGateType, evalContext)
	if err != nil {
		return core.QualityGateResult{}, err
	}

	return core.QualityGateResult{
		Passed:      result.Passed,
		Action:      string(result.Action),
		FailedGates: result.FailedGates,
		CacheHit:    result.CacheHit,
		TimedOut:    result.TimedOut,
		DurationMs:  result.Duration.Milliseconds(),
	}, nil
}

func (qg *Daemon) eventLoop() {
	defer qg.wg.Done()

	qg.log(core.LogLevelDebug, "quality_gate_event_loop started")

	for {
		select {
		case <-qg.ctx.Done():
			qg.log(core.LogLevelDebug, "quality_gate_event_loop shutdown_signal")
			discarded := 0
			for {
				select {
				case _, ok := <-qg.eventChan:
					if !ok {
						goto drained
					}
					discarded++
				default:
					goto drained
				}
			}
		drained:
			if discarded > 0 {
				qg.log(core.LogLevelWarn, "quality_gate_event_loop discarded %d buffered events on shutdown", discarded)
			}
			return
		case event, ok := <-qg.eventChan:
			if !ok {
				qg.log(core.LogLevelDebug, "quality_gate_event_loop channel_closed")
				return
			}
			qg.processEvent(event)
		}
	}
}

func (qg *Daemon) processEvent(event Event) {
	qg.log(core.LogLevelDebug, "quality_gate_processing_event type=%s", event.EventType())

	startTime := qg.clock.Now()

	var err error
	switch e := event.(type) {
	case TaskStartEvent:
		err = qg.handleTaskStart(e)
	case TaskCompleteEvent:
		err = qg.handleTaskComplete(e)
	case PhaseTransitionEvent:
		err = qg.handlePhaseTransition(e)
	default:
		qg.log(core.LogLevelWarn, "quality_gate_unknown_event_type type=%s", event.EventType())
		return
	}

	duration := time.Since(startTime).Milliseconds()
	qg.metrics.RecordEvaluation(err == nil, duration)

	if err != nil {
		qg.log(core.LogLevelError, "quality_gate_processing_error type=%s error=%v", event.EventType(), err)
	} else {
		qg.log(core.LogLevelDebug, "quality_gate_processed_event type=%s duration_ms=%d", event.EventType(), duration)
	}
}

func (qg *Daemon) handleTaskStart(event TaskStartEvent) error {
	qg.log(core.LogLevelDebug, "quality_gate_task_start task_id=%s command_id=%s agent_id=%s",
		event.TaskID, event.CommandID, event.AgentID)
	return qg.evaluateGate("task_start", map[string]interface{}{
		"task_id":    event.TaskID,
		"command_id": event.CommandID,
		"agent_id":   event.AgentID,
	})
}

func (qg *Daemon) handleTaskComplete(event TaskCompleteEvent) error {
	qg.log(core.LogLevelDebug, "quality_gate_task_complete task_id=%s command_id=%s status=%s",
		event.TaskID, event.CommandID, event.Status)
	return qg.evaluateGate("task_complete", map[string]interface{}{
		"task_id":    event.TaskID,
		"command_id": event.CommandID,
		"agent_id":   event.AgentID,
		"status":     event.Status,
	})
}

func (qg *Daemon) handlePhaseTransition(event PhaseTransitionEvent) error {
	qg.log(core.LogLevelDebug, "quality_gate_phase_transition phase_id=%s command_id=%s old=%s new=%s",
		event.PhaseID, event.CommandID, event.OldStatus, event.NewStatus)
	return qg.evaluateGate("phase_transition", map[string]interface{}{
		"phase_id":   event.PhaseID,
		"command_id": event.CommandID,
		"old_status": event.OldStatus,
		"new_status": event.NewStatus,
	})
}

func mapGateType(gateType string) (quality.GateType, error) {
	switch gateType {
	case "pre_task", "task_start":
		return quality.GateTypePreTask, nil
	case "post_task", "task_complete":
		return quality.GateTypePostTask, nil
	case "phase_transition":
		return quality.GateTypePhaseTransition, nil
	case "command_validation":
		return quality.GateTypeCommandValidation, nil
	default:
		return "", fmt.Errorf("unknown gate type: %s", gateType)
	}
}

func (qg *Daemon) evaluateGate(gateType string, evalContext map[string]interface{}) error {
	qg.log(core.LogLevelDebug, "quality_gate_evaluate gate_type=%s context=%v", gateType, evalContext)

	qualityGateType, err := mapGateType(gateType)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(qg.ctx, qg.evaluationTimeout)
	defer cancel()

	result, err := qg.engine.Evaluate(ctx, qualityGateType, evalContext)
	if err != nil {
		return fmt.Errorf("evaluation failed: %w", err)
	}

	if result.Passed {
		qg.log(core.LogLevelDebug, "quality_gate_passed gate_type=%s duration_ms=%d cache_hit=%v",
			gateType, result.Duration.Milliseconds(), result.CacheHit)
	} else {
		qg.log(core.LogLevelWarn, "quality_gate_failed gate_type=%s failed_gates=%v action=%s",
			gateType, result.FailedGates, result.Action)

		if result.Action == quality.ActionBlock {
			return fmt.Errorf("gate evaluation blocked: %v", result.FailedGates)
		}
	}

	return nil
}

// LoadGateDefinitions loads gate definitions from the maestro directory.
func (qg *Daemon) LoadGateDefinitions() error {
	return qg.loadGateDefinitions()
}

func (qg *Daemon) loadGateDefinitions() error {
	loader := quality.NewLoader(qg.maestroDir)
	config, err := loader.LoadConfiguration()
	if err != nil {
		return fmt.Errorf("failed to load gate definitions: %w", err)
	}

	if err := qg.engine.LoadConfiguration(config); err != nil {
		return fmt.Errorf("failed to load configuration into engine: %w", err)
	}

	qg.log(core.LogLevelInfo, "quality_gate_definitions_loaded count=%d", len(config.Gates))
	return nil
}

// GetMetrics returns the current quality gate metrics.
func (qg *Daemon) GetMetrics() *Metrics {
	return qg.metrics
}

func (qg *Daemon) log(level core.LogLevel, format string, args ...interface{}) {
	qg.dl.Logf(level, format, args...)
}
