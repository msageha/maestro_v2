package daemon

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// maxGateEvaluations is the maximum number of gate evaluation entries kept in memory.
// Why: 1000 entries accommodate typical concurrent command loads while bounding
// memory usage. When exceeded, the oldest half is evicted (LRU-style).
const maxGateEvaluations = 1000

// QualityGateEvaluator encapsulates pre-task quality gate evaluation logic,
// separating it from the Dispatcher's core dispatch responsibilities.
type QualityGateEvaluator struct {
	config      model.Config
	clock       Clock
	dl          *DaemonLogger
	getGateFn   func() *QualityGateDaemon
	evaluations map[string]*model.QualityGateEvaluation
	evalMutex   sync.RWMutex
}

// NewQualityGateEvaluator creates a QualityGateEvaluator.
func NewQualityGateEvaluator(cfg model.Config, clock Clock, dl *DaemonLogger, getGateFn func() *QualityGateDaemon) *QualityGateEvaluator {
	return &QualityGateEvaluator{
		config:      cfg,
		clock:       clock,
		dl:          dl,
		getGateFn:   getGateFn,
		evaluations: make(map[string]*model.QualityGateEvaluation),
	}
}

// ShouldEvaluate determines if quality gates should be evaluated.
func (e *QualityGateEvaluator) ShouldEvaluate() bool {
	if !e.config.QualityGates.Enabled {
		return false
	}
	if e.config.QualityGates.SkipGates {
		e.dl.Logf(LogLevelInfo, "quality_gates_skipped reason=emergency_mode")
		return false
	}
	if e.getGateFn() == nil {
		e.dl.Logf(LogLevelDebug, "quality_gates_skipped reason=daemon_not_available")
		return false
	}
	return true
}

// EvaluatePreTask evaluates quality gates before task execution and returns the result.
//
// L-7: There is a TOCTOU window between this pre-task gate check and actual task dispatch.
// A gate condition could change between evaluation and dispatch. This is acceptable because:
//   - Phase C's epoch fencing is the authoritative guard against stale dispatches.
//   - The pre-task gate check is a best-effort early filter (advisory), not a guarantee.
//   - Making the check and dispatch fully atomic would require holding scanMu across the
//     entire dispatch path, which would serialize all dispatches and hurt throughput.
//
// The three-phase design (Phase A: scan, Phase B: dispatch, Phase C: commit with fencing)
// ensures correctness even when pre-task checks are slightly stale.
func (e *QualityGateEvaluator) EvaluatePreTask(task *model.Task, workerID string) (*model.QualityGateEvaluation, error) {
	qg := e.getGateFn()
	if qg == nil {
		return nil, nil
	}

	// NOTE: We do NOT emit TaskStartEvent here. The EventBus publish in
	// DispatchTask (after successful dispatch) triggers the subscription
	// in subscribeQualityGateEvents, which forwards the event to
	// QualityGateDaemon. Emitting here would cause duplicate delivery.

	context := map[string]interface{}{
		"task_id":    task.ID,
		"command_id": task.CommandID,
		"agent_id":   workerID,
		"priority":   task.Priority,
		"attempts":   task.Attempts,
	}

	result, err := qg.evaluateGateWithResult("pre_task", context)

	evaluation := &model.QualityGateEvaluation{
		Passed:      result != nil && result.Passed,
		EvaluatedAt: e.clock.Now().Format(time.RFC3339),
	}

	if result != nil {
		evaluation.Action = string(result.Action)
		if len(result.FailedGates) > 0 {
			evaluation.FailedGates = make([]string, len(result.FailedGates))
			copy(evaluation.FailedGates, result.FailedGates)
		}
	}

	if err != nil {
		return evaluation, fmt.Errorf("evaluation failed: %w", err)
	}
	if result == nil {
		return evaluation, fmt.Errorf("evaluation returned nil result")
	}
	if !result.Passed {
		return evaluation, fmt.Errorf("quality gate check failed: %v", result.FailedGates)
	}
	return evaluation, nil
}

// StoreEvaluation stores the gate evaluation for a task.
// Evicts oldest entries when the map exceeds maxGateEvaluations.
func (e *QualityGateEvaluator) StoreEvaluation(taskID string, evaluation *model.QualityGateEvaluation) {
	if evaluation == nil {
		return
	}

	e.evalMutex.Lock()
	defer e.evalMutex.Unlock()
	e.evaluations[taskID] = evaluation

	if len(e.evaluations) > maxGateEvaluations {
		e.evictOldEvaluationsLocked()
	}
}

// evictOldEvaluationsLocked removes the oldest gate evaluation entries to bring
// the map back to maxGateEvaluations/2. Caller must hold evalMutex.
func (e *QualityGateEvaluator) evictOldEvaluationsLocked() {
	type entry struct {
		taskID      string
		evaluatedAt time.Time
	}
	entries := make([]entry, 0, len(e.evaluations))
	for id, eval := range e.evaluations {
		t, err := time.Parse(time.RFC3339, eval.EvaluatedAt)
		if err != nil {
			t = time.Time{}
		}
		entries = append(entries, entry{taskID: id, evaluatedAt: t})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].evaluatedAt.Before(entries[j].evaluatedAt)
	})

	target := maxGateEvaluations / 2
	for i := 0; i < len(entries) && len(e.evaluations) > target; i++ {
		delete(e.evaluations, entries[i].taskID)
	}
}

// SkippedEvaluation returns an evaluation indicating gates were skipped with the given reason.
func (e *QualityGateEvaluator) SkippedEvaluation(reason string) *model.QualityGateEvaluation {
	return &model.QualityGateEvaluation{
		Passed:        true,
		SkippedReason: reason,
		EvaluatedAt:   e.clock.Now().Format(time.RFC3339),
	}
}
