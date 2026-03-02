package daemon

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/quality"
)

// QualityGateEvent represents an event that triggers quality gate evaluation.
type QualityGateEvent interface {
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

func (e TaskStartEvent) EventType() string  { return "task_start" }
func (e TaskStartEvent) Timestamp() time.Time { return e.StartedAt }

// TaskCompleteEvent is emitted when a task completes (success or failure).
type TaskCompleteEvent struct {
	TaskID      string
	CommandID   string
	AgentID     string
	Status      model.Status
	CompletedAt time.Time
}

func (e TaskCompleteEvent) EventType() string  { return "task_complete" }
func (e TaskCompleteEvent) Timestamp() time.Time { return e.CompletedAt }

// PhaseTransitionEvent is emitted when a phase changes status.
type PhaseTransitionEvent struct {
	PhaseID       string
	CommandID     string
	OldStatus     model.PhaseStatus
	NewStatus     model.PhaseStatus
	TransitionedAt time.Time
}

func (e PhaseTransitionEvent) EventType() string  { return "phase_transition" }
func (e PhaseTransitionEvent) Timestamp() time.Time { return e.TransitionedAt }

// QualityGateMetrics tracks quality gate evaluation metrics.
type QualityGateMetrics struct {
	mu                    sync.RWMutex
	evaluationCount       int64
	successCount          int64
	failureCount          int64
	totalEvaluationTimeMs int64
}

// RecordEvaluation records a quality gate evaluation.
func (m *QualityGateMetrics) RecordEvaluation(success bool, durationMs int64) {
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

// GetStats returns current metrics statistics.
func (m *QualityGateMetrics) GetStats() (evalCount, successCount, failureCount, avgTimeMs int64) {
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

// QualityGateDaemon manages quality gate evaluations independently from the core daemon loop.
// It runs in a separate goroutine and processes events asynchronously.
type QualityGateDaemon struct {
	maestroDir string
	config     model.Config
	lockMap    *lock.MutexMap
	logger     *log.Logger
	logLevel   LogLevel

	eventChan         chan QualityGateEvent
	metrics           *QualityGateMetrics
	stopped           atomic.Bool // Prevents EmitEvent after Stop to avoid panic
	engine            *quality.Engine
	evaluationTimeout time.Duration
	gates             map[string][]*GateDefinition // For test compatibility

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// GateDefinition is a simplified version for test compatibility
type GateDefinition struct {
	ID       string
	Name     string
	Type     string
	Enabled  bool
	Priority int
	Rules    []RuleDefinition
	Action   ActionDefinition
}

// RuleDefinition is a simplified version for test compatibility
type RuleDefinition struct {
	ID        string
	Condition RuleCondition
	Severity  string
}

// RuleCondition is a simplified version for test compatibility
type RuleCondition struct {
	Type   string
	Field  string
	Script func(context.Context, map[string]interface{}) (bool, error)
}

// ActionDefinition is a simplified version for test compatibility
type ActionDefinition struct {
	OnPass string
	OnFail string
}

// NewQualityGateDaemon creates a new QualityGateDaemon instance.
func NewQualityGateDaemon(
	maestroDir string,
	cfg model.Config,
	lockMap *lock.MutexMap,
	logger *log.Logger,
	logLevel LogLevel,
) *QualityGateDaemon {
	ctx, cancel := context.WithCancel(context.Background())
	return &QualityGateDaemon{
		maestroDir:        maestroDir,
		config:            cfg,
		lockMap:           lockMap,
		logger:            logger,
		logLevel:          logLevel,
		eventChan:         make(chan QualityGateEvent, 100), // Buffered to prevent blocking
		metrics:           &QualityGateMetrics{},
		engine:            quality.NewEngine(),
		evaluationTimeout: 100 * time.Millisecond,
		gates:             make(map[string][]*GateDefinition), // For test compatibility
		ctx:               ctx,
		cancel:            cancel,
	}
}

// Start begins the quality gate daemon's event processing loop.
// It runs in a separate goroutine and does not block.
func (qg *QualityGateDaemon) Start() error {
	qg.log(LogLevelInfo, "quality_gate_daemon starting")

	// Load gate definitions on startup
	if err := qg.loadGateDefinitions(); err != nil {
		// Log error but don't fail - we can run without gates
		qg.log(LogLevelWarn, "quality_gate_daemon failed to load definitions: %v", err)
	}

	qg.wg.Add(1)
	go qg.eventLoop()

	qg.log(LogLevelInfo, "quality_gate_daemon started")
	return nil
}

// Stop gracefully shuts down the quality gate daemon.
// It waits for the event processing loop to finish.
func (qg *QualityGateDaemon) Stop() error {
	qg.log(LogLevelInfo, "quality_gate_daemon stopping")

	// Set stopped flag first to prevent new EmitEvent calls
	qg.stopped.Store(true)

	// Signal shutdown
	qg.cancel()

	// Close event channel to unblock eventLoop
	close(qg.eventChan)

	// Wait for event loop to finish
	qg.wg.Wait()

	qg.log(LogLevelInfo, "quality_gate_daemon stopped")
	return nil
}

// EmitEvent sends an event to the quality gate daemon for processing.
// This is a non-blocking operation; events are queued in a buffered channel.
// Safe to call from multiple goroutines, but drops events after Stop().
func (qg *QualityGateDaemon) EmitEvent(event QualityGateEvent) {
	// Check if already stopped to avoid sending to closed channel
	if qg.stopped.Load() {
		qg.log(LogLevelDebug, "quality_gate_event_dropped type=%s reason=stopped", event.EventType())
		return
	}

	select {
	case qg.eventChan <- event:
		qg.log(LogLevelDebug, "quality_gate_event_queued type=%s", event.EventType())
	case <-qg.ctx.Done():
		qg.log(LogLevelDebug, "quality_gate_event_dropped type=%s reason=shutdown", event.EventType())
	default:
		qg.log(LogLevelWarn, "quality_gate_event_dropped type=%s reason=channel_full", event.EventType())
	}
}

// eventLoop is the main processing loop for quality gate events.
// It runs in a separate goroutine and processes events asynchronously.
func (qg *QualityGateDaemon) eventLoop() {
	defer qg.wg.Done()

	qg.log(LogLevelDebug, "quality_gate_event_loop started")

	for {
		select {
		case <-qg.ctx.Done():
			qg.log(LogLevelDebug, "quality_gate_event_loop shutdown_signal")
			return
		case event, ok := <-qg.eventChan:
			if !ok {
				qg.log(LogLevelDebug, "quality_gate_event_loop channel_closed")
				return
			}
			qg.processEvent(event)
		}
	}
}

// processEvent handles a single quality gate event.
func (qg *QualityGateDaemon) processEvent(event QualityGateEvent) {
	qg.log(LogLevelDebug, "quality_gate_processing_event type=%s", event.EventType())

	// Record processing start time for metrics
	startTime := time.Now()

	// Process based on event type
	var err error
	switch e := event.(type) {
	case TaskStartEvent:
		err = qg.handleTaskStart(e)
	case TaskCompleteEvent:
		err = qg.handleTaskComplete(e)
	case PhaseTransitionEvent:
		err = qg.handlePhaseTransition(e)
	default:
		qg.log(LogLevelWarn, "quality_gate_unknown_event_type type=%s", event.EventType())
		return
	}

	// Record metrics
	duration := time.Since(startTime).Milliseconds()
	qg.metrics.RecordEvaluation(err == nil, duration)

	if err != nil {
		qg.log(LogLevelError, "quality_gate_processing_error type=%s error=%v", event.EventType(), err)
	} else {
		qg.log(LogLevelDebug, "quality_gate_processed_event type=%s duration_ms=%d", event.EventType(), duration)
	}
}

// handleTaskStart processes task start events.
func (qg *QualityGateDaemon) handleTaskStart(event TaskStartEvent) error {
	qg.log(LogLevelDebug, "quality_gate_task_start task_id=%s command_id=%s agent_id=%s",
		event.TaskID, event.CommandID, event.AgentID)

	// Evaluate quality gates for task start
	return qg.evaluateGate("task_start", map[string]interface{}{
		"task_id":    event.TaskID,
		"command_id": event.CommandID,
		"agent_id":   event.AgentID,
	})
}

// handleTaskComplete processes task completion events.
func (qg *QualityGateDaemon) handleTaskComplete(event TaskCompleteEvent) error {
	qg.log(LogLevelDebug, "quality_gate_task_complete task_id=%s command_id=%s status=%s",
		event.TaskID, event.CommandID, event.Status)

	// Evaluate quality gates for task completion
	return qg.evaluateGate("task_complete", map[string]interface{}{
		"task_id":    event.TaskID,
		"command_id": event.CommandID,
		"agent_id":   event.AgentID,
		"status":     event.Status,
	})
}

// handlePhaseTransition processes phase transition events.
func (qg *QualityGateDaemon) handlePhaseTransition(event PhaseTransitionEvent) error {
	qg.log(LogLevelDebug, "quality_gate_phase_transition phase_id=%s command_id=%s old=%s new=%s",
		event.PhaseID, event.CommandID, event.OldStatus, event.NewStatus)

	// Evaluate quality gates for phase transitions
	return qg.evaluateGate("phase_transition", map[string]interface{}{
		"phase_id":   event.PhaseID,
		"command_id": event.CommandID,
		"old_status": event.OldStatus,
		"new_status": event.NewStatus,
	})
}

// evaluateGate evaluates quality gates for the specified type and context
func (qg *QualityGateDaemon) evaluateGate(gateType string, evalContext map[string]interface{}) error {
	qg.log(LogLevelDebug, "quality_gate_evaluate gate_type=%s context=%v", gateType, evalContext)

	// Map string gate type to quality.GateType
	var qualityGateType quality.GateType
	switch gateType {
	case "task_start":
		qualityGateType = quality.GateTypePreTask
	case "task_complete":
		qualityGateType = quality.GateTypePostTask
	case "phase_transition":
		qualityGateType = quality.GateTypePhaseTransition
	case "command_validation":
		qualityGateType = quality.GateTypeCommandValidation
	default:
		return fmt.Errorf("unknown gate type: %s", gateType)
	}

	// Create evaluation context with timeout
	ctx, cancel := context.WithTimeout(qg.ctx, qg.evaluationTimeout)
	defer cancel()

	// Evaluate using the engine
	result, err := qg.engine.Evaluate(ctx, qualityGateType, evalContext)
	if err != nil {
		return fmt.Errorf("evaluation failed: %w", err)
	}

	// Log the result
	if result.Passed {
		qg.log(LogLevelDebug, "quality_gate_passed gate_type=%s duration_ms=%d cache_hit=%v",
			gateType, result.Duration.Milliseconds(), result.CacheHit)
	} else {
		qg.log(LogLevelWarn, "quality_gate_failed gate_type=%s failed_gates=%v action=%s",
			gateType, result.FailedGates, result.Action)

		// If action is block, return error
		if result.Action == quality.ActionBlock {
			return fmt.Errorf("gate evaluation blocked: %v", result.FailedGates)
		}
	}

	return nil
}

// evaluateGateWithResult evaluates gates and returns the full result (for testing)
func (qg *QualityGateDaemon) evaluateGateWithResult(gateType string, evalContext map[string]interface{}) (*quality.EvaluationResult, error) {
	// Map string gate type to quality.GateType
	var qualityGateType quality.GateType
	switch gateType {
	case "pre_task", "task_start":
		qualityGateType = quality.GateTypePreTask
	case "post_task", "task_complete":
		qualityGateType = quality.GateTypePostTask
	case "phase_transition":
		qualityGateType = quality.GateTypePhaseTransition
	case "command_validation":
		qualityGateType = quality.GateTypeCommandValidation
	default:
		return nil, fmt.Errorf("unknown gate type: %s", gateType)
	}

	// Create evaluation context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), qg.evaluationTimeout)
	defer cancel()

	// Evaluate using the engine
	return qg.engine.Evaluate(ctx, qualityGateType, evalContext)
}

// loadGateDefinitions loads gate definitions from the maestro directory
func (qg *QualityGateDaemon) loadGateDefinitions() error {
	loader := quality.NewLoader(qg.maestroDir)
	config, err := loader.LoadConfiguration()
	if err != nil {
		return fmt.Errorf("failed to load gate definitions: %w", err)
	}

	// Load into the engine
	if err := qg.engine.LoadConfiguration(config); err != nil {
		return fmt.Errorf("failed to load configuration into engine: %w", err)
	}

	qg.log(LogLevelInfo, "quality_gate_definitions_loaded count=%d", len(config.Gates))
	return nil
}

// GetMetrics returns the current quality gate metrics.
func (qg *QualityGateDaemon) GetMetrics() *QualityGateMetrics {
	return qg.metrics
}

// log writes a log message if the level is enabled.
func (qg *QualityGateDaemon) log(level LogLevel, format string, args ...interface{}) {
	if level < qg.logLevel {
		return
	}
	prefix := ""
	switch level {
	case LogLevelDebug:
		prefix = "[DEBUG] "
	case LogLevelInfo:
		prefix = "[INFO] "
	case LogLevelWarn:
		prefix = "[WARN] "
	case LogLevelError:
		prefix = "[ERROR] "
	}
	qg.logger.Printf(prefix+format, args...)
}
