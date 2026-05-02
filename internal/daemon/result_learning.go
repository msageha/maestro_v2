package daemon

// Phase C learning helpers. These functions are tightly coupled to
// ResultHandler because they read PhaseCManager / banditModelSelector /
// config off the handler, but they form a coherent "feed completed task
// results into the learning components" responsibility that is easier
// to evolve in isolation when adding new C-x signals.

import (
	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/model"
)

// recordTaskResultLearning feeds a completed worker task result into the
// Phase C learning components:
//   - FingerprintDB (C-5): failed results contribute a failure fingerprint
//     and category derived from the result Summary.
//   - Bandit model selector (C-2): completed/failed/dead-lettered results
//     reward the worker's model arm.
//   - Search tree & Thompson sampler (C-4): backpropagate the task outcome
//     and reward the widen/deepen decision recorded at dispatch.
//
// Each component path is independent and nil-safe; disabling one does not
// skip the others. Two nil-guard styles coexist intentionally:
//   - FingerprintDB access is a *field read* so the call site must check
//     `m != nil` explicitly before dereferencing.
//   - ObserveTaskOutcome / RecordTaskCompletionNovelty / PlanRetryMutations
//     are *method calls* on *PhaseCManager that tolerate a nil receiver,
//     so the call site omits the guard and lets the callee short-circuit.
//
// Keep this split when adding new signals — do not collapse to a single
// `if m != nil { ... }` wrapper, as that hides the callee's nil contract.
func (rh *ResultHandler) recordTaskResultLearning(r *model.TaskResult, workerID string) {
	if r == nil {
		return
	}
	m := rh.getPhaseC()

	// Each C-xxx component runs independently. Splitting them keeps
	// recordTaskResultLearning a high-level orchestrator that is easy to
	// extend (e.g. adding C-6 / C-7) without re-flowing this function.
	rh.recordFingerprintCapture(r, workerID, m)
	rh.recordBanditReward(r, workerID)
	rh.recordSearchTreeOutcome(r, m)
	rh.recordEvolutionSignal(r, m)
}

// recordFingerprintCapture is the C-5 path of recordTaskResultLearning. It
// computes a stable error fingerprint from the failure summary and stores
// it in the FingerprintDB so future failures can be deduplicated.
func (rh *ResultHandler) recordFingerprintCapture(r *model.TaskResult, workerID string, m *PhaseCManager) {
	if m == nil || m.FingerprintDB == nil {
		return
	}
	// TaskResult encodes failure detail in Summary (no dedicated LastError
	// field); success results typically have benign summaries we ignore.
	switch r.Status {
	case model.StatusFailed, model.StatusDeadLetter:
		fp, category := learnings.ComputeErrorFingerprint(r.Summary)
		if fp == "" {
			return
		}
		// Strategy is empty at capture time; the planner/retry logic may
		// attach one later via Store(fp, category, strategy).
		m.FingerprintDB.Store(fp, category, "")
		rh.log(LogLevelInfo, "fingerprint_store worker=%s task=%s command=%s category=%s fp=%s",
			workerID, r.TaskID, r.CommandID, category, fp)
	}
}

// recordBanditReward is the C-2 path of recordTaskResultLearning. Reward
// policy: Completed=1.0, Failed/DeadLetter=0.0, anything else ignored.
// Mirrors REQUIREMENTS §5-7 — see internal/daemon/model_selector.go.
func (rh *ResultHandler) recordBanditReward(r *model.TaskResult, workerID string) {
	sel := rh.getModelSelector()
	if sel == nil {
		return
	}
	modelName := rh.workerModelName(workerID)
	if modelName == "" {
		return
	}
	switch r.Status {
	case model.StatusCompleted:
		sel.RecordResult(modelName, 1.0)
	case model.StatusFailed, model.StatusDeadLetter:
		sel.RecordResult(modelName, 0.0)
	}
}

// recordSearchTreeOutcome is the C-4 path of recordTaskResultLearning. It
// backpropagates the task outcome into the search tree / Thompson sampler.
// nil-safe on the manager and its components. Status mapping matches
// recordBanditReward.
func (rh *ResultHandler) recordSearchTreeOutcome(r *model.TaskResult, m *PhaseCManager) {
	switch r.Status {
	case model.StatusCompleted:
		m.ObserveTaskOutcome(r.TaskID, 1.0, true)
	case model.StatusFailed, model.StatusDeadLetter:
		m.ObserveTaskOutcome(r.TaskID, 0.0, false)
	}
}

// recordEvolutionSignal is the C-1 path of recordTaskResultLearning.
//   - Completed: check novelty of the summary against prior completions.
//     A non-novel signal means the daemon has converged on a repeated
//     solution, which is surfaced as an observability log so operators can
//     notice when exploration has stalled.
//   - Failed/DeadLetter: increment per-command failure count and, once the
//     threshold is crossed, run PlanMutations to emit a retry strategy
//     plan (observability only — the existing retry path in plan/retry.go
//     performs the actual retries; this log lets operators see which
//     mutation strategies the evolution engine would pick).
func (rh *ResultHandler) recordEvolutionSignal(r *model.TaskResult, m *PhaseCManager) {
	switch r.Status {
	case model.StatusCompleted:
		if novel, evaluated := m.RecordTaskCompletionNovelty(r.CommandID, r.Summary); evaluated {
			rh.log(LogLevelInfo, "evolution_novelty command=%s task=%s novel=%t",
				r.CommandID, r.TaskID, novel)
		}
	case model.StatusFailed, model.StatusDeadLetter:
		// parentCount is bounded below by 2 so the cross-strategy slot is
		// eligible once the threshold is crossed; the engine itself drops
		// cross when parentCount < 2.
		if slots, planned := m.PlanRetryMutations(r.CommandID, 2); planned {
			rh.log(LogLevelInfo, "evolution_retry_plan command=%s task=%s slots=%d",
				r.CommandID, r.TaskID, len(slots))
		}
	}
}

// workerModelName resolves the model name configured for the given worker.
// Returns empty when the worker is unknown and no default is configured.
// Duplicates plan.GetWorkerModel's logic to keep daemon decoupled from plan.
func (rh *ResultHandler) workerModelName(workerID string) string {
	cfg := rh.config.Agents.Workers
	if cfg.Boost {
		return "opus"
	}
	if m, ok := cfg.Models[workerID]; ok && m != "" {
		return m
	}
	if cfg.DefaultModel != "" {
		return cfg.DefaultModel
	}
	return "sonnet"
}
