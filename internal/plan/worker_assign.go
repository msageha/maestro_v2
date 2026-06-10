package plan

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/contract"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// WorkerAssignment represents the result of assigning a task to a specific worker.
type WorkerAssignment struct {
	TaskName string
	WorkerID string
	Model    string
}

// ModelSelector is an optional hook consulted by AssignWorkers for each task
// to potentially override the static BloomLevel→model mapping. The selector's
// choice is only honored when at least one configured worker runs that model;
// otherwise AssignWorkers falls back to GetModelForBloomLevel.
//
// Implementations must be safe for concurrent use.
//
// Aliased to contract.ModelSelector so plan and daemon/core share a single
// definition.
type ModelSelector = contract.ModelSelector

// assignConfig carries optional AssignWorkers behavior.
type assignConfig struct {
	selector ModelSelector
}

// AssignOption configures optional AssignWorkers behavior.
type AssignOption func(*assignConfig)

// WithModelSelector attaches an adaptive model selector to AssignWorkers.
// Passing a nil selector is a no-op (behaves like the static path).
func WithModelSelector(s ModelSelector) AssignOption {
	return func(c *assignConfig) { c.selector = s }
}

// WorkerState tracks a worker's current load for assignment decisions.
type WorkerState struct {
	WorkerID     string
	Model        string
	PendingCount int
}

// TaskAssignmentRequest describes a task that needs worker assignment.
// PinnedWorkerID, when set, forces AssignWorkers to bypass the bloom-based
// auto-assignment and place the task on the named worker — same shape as
// the inject path's TargetWorkerID but reachable from the initial-submit
// route, so Planner agents can fan tasks into fixed lanes (e.g. worker1 vs
// worker2 for separate features) on a fresh plan submit.
type TaskAssignmentRequest struct {
	Name           string
	BloomLevel     int
	PinnedWorkerID string
	// RequireClaudeRuntime restricts assignment to workers whose configured
	// model maps to the claude-code runtime. Set for run_on_main tasks: the
	// read-only guard on the main working directory is enforced by the
	// claude-code PreToolUse policy hook, which codex / gemini workers lack.
	RequireClaudeRuntime bool
}

// GetModelForBloomLevel returns the model family name appropriate for the
// given Bloom taxonomy level. BloomLevel 0 (unset/invalid) defaults to
// "sonnet" as a safe fallback. BloomLevel 1-3 maps to "sonnet", 4-6 maps to
// "opus".
//
// The returned value is a canonical family alias ("sonnet" / "opus"). Workers
// may be configured with either the same short alias or a full Claude model
// ID that belongs to the same family (e.g. "claude-opus-4-7" satisfies an
// "opus" requirement) — AssignWorkers compares using modelFamily so any full
// ID in the required family is eligible.
func GetModelForBloomLevel(bloomLevel int, boost bool) string {
	if boost {
		return "opus"
	}
	if bloomLevel >= 4 && bloomLevel <= 6 {
		return "opus"
	}
	return "sonnet"
}

// modelFamily returns the canonical family alias ("sonnet" / "opus" / "haiku")
// for a given model identifier. Short aliases pass through unchanged; full
// Claude model IDs such as "claude-opus-4-7" or "claude-haiku-4-5-20251001"
// are mapped to their family. Unknown names are returned unchanged so custom
// / third-party model names still compare equal to themselves.
func modelFamily(name string) string {
	switch name {
	case "", "sonnet", "opus", "haiku":
		return name
	}
	// Full Claude model IDs have the form "claude-<family>-<version>[-<suffix>]".
	if rest, ok := strings.CutPrefix(name, "claude-"); ok {
		family := rest
		if i := strings.IndexByte(rest, '-'); i >= 0 {
			family = rest[:i]
		}
		switch family {
		case "sonnet", "opus", "haiku":
			return family
		}
	}
	return name
}

// GetWorkerModel returns the model configured for the given worker, falling back to the default.
func GetWorkerModel(workerID string, config model.WorkerConfig) string {
	return config.ModelFor(workerID)
}

// isClaudeRuntimeModel reports whether the given worker model maps to the
// claude-code runtime. run_on_main tasks require claude-code because the
// read-only enforcement on the main working directory (the @run_on_main
// pane variable consumed by the PreToolUse policy hook) only exists for
// claude-code; codex / gemini workers run with sandbox bypass flags and
// would have no technical barrier against mutating main.
func isClaudeRuntimeModel(modelName string) bool {
	runtime, _ := model.ParseRuntimeFromModel(modelName)
	return runtime == model.RuntimeClaudeCode
}

// AssignWorkers distributes tasks across available workers using least-loaded selection per model.
// Optional AssignOption values can override static model selection via WithModelSelector.
func AssignWorkers(
	config model.WorkerConfig,
	limits model.LimitsConfig,
	currentWorkerStates []WorkerState,
	tasks []TaskAssignmentRequest,
	opts ...AssignOption,
) ([]WorkerAssignment, error) {
	if len(tasks) == 0 {
		return []WorkerAssignment{}, nil
	}

	var ac assignConfig
	for _, o := range opts {
		if o != nil {
			o(&ac)
		}
	}

	// Build worker state map (copy to track incremental assignments)
	stateMap := make(map[string]*WorkerState, len(currentWorkerStates))
	for i := range currentWorkerStates {
		ws := currentWorkerStates[i]
		stateMap[ws.WorkerID] = &ws
	}

	maxPending := limits.MaxPendingTasksPerWorker
	if maxPending <= 0 {
		maxPending = 10
	}

	assignments := make([]WorkerAssignment, 0, len(tasks))
	for _, task := range tasks {
		// Pinned-worker fast path: when a TaskInput supplies an explicit
		// worker_id (e.g. `plan submit` YAML pinning a task to worker1
		// for a feature lane), skip the bloom-based selector and use the
		// configured worker model directly. Mirrors the inject.go
		// TargetWorkerID branch so behaviour is identical regardless of
		// which CLI/API entry point produced the assignment.
		if task.PinnedWorkerID != "" {
			ws, ok := stateMap[task.PinnedWorkerID]
			if !ok {
				return nil, fmt.Errorf("%w for task %q: pinned worker %q not configured (workers.count=%d)",
					ErrNoAvailableWorker, task.Name, task.PinnedWorkerID, config.Count)
			}
			if task.RequireClaudeRuntime && !isClaudeRuntimeModel(ws.Model) {
				return nil, fmt.Errorf("%w for task %q: pinned worker %q runs model %q (non-claude runtime); run_on_main tasks require a claude-code worker because only claude-code enforces the read-only main guard",
					ErrNoAvailableWorker, task.Name, task.PinnedWorkerID, ws.Model)
			}
			if ws.PendingCount >= maxPending {
				return nil, fmt.Errorf("%w for task %q: pinned worker %q at capacity (max_pending_tasks_per_worker=%d)",
					ErrNoAvailableWorker, task.Name, task.PinnedWorkerID, maxPending)
			}
			assignments = append(assignments, WorkerAssignment{
				TaskName: task.Name,
				WorkerID: ws.WorkerID,
				Model:    ws.Model,
			})
			ws.PendingCount++
			continue
		}

		requiredModel := GetModelForBloomLevel(task.BloomLevel, config.Boost)
		// Honor a bandit / adaptive selector when it picks a model that at
		// least one worker is configured for; otherwise keep the static
		// bloom-derived model to preserve feasibility. Bandit arms are built
		// from worker-configured models and may include codex/gemini; a
		// RequireClaudeRuntime task must ignore such picks — honoring one
		// would make the eligibility loop below skip every worker (claude
		// workers fail the family match, non-claude workers fail the runtime
		// check) and fail the assignment even with idle claude workers.
		if ac.selector != nil {
			if pick := ac.selector.SelectModel(task.BloomLevel, task.Name); pick != "" && pick != requiredModel {
				if workerExistsForModel(stateMap, pick) &&
					(!task.RequireClaudeRuntime || isClaudeRuntimeModel(pick)) {
					requiredModel = pick
				}
			}
		}

		// If no workers are configured for the bloom-derived family, fall
		// back to whichever family the operator actually provisioned. This
		// prevents deployments with a single model family (e.g. all-opus)
		// from failing on tasks whose Bloom level normally maps to a
		// different family. Overqualified assignments (opus handling a
		// sonnet-level task) are benign; underqualified assignments (sonnet
		// handling an opus-level task) trigger an observability warning so
		// operators can notice capability mismatches.
		if !workerExistsForModel(stateMap, requiredModel) {
			if fallback := chooseFallbackFamily(stateMap, requiredModel, task.RequireClaudeRuntime); fallback != "" {
				slog.Warn("worker_model_fallback",
					"task", task.Name,
					"bloom_level", task.BloomLevel,
					"required", requiredModel,
					"fallback", fallback,
					"reason", "no worker configured for required family")
				requiredModel = fallback
			}
		}

		// Find eligible workers with matching model family and minimum pending.
		// Workers configured with a full Claude model ID (e.g.
		// "claude-opus-4-7") satisfy a required family alias ("opus") via
		// modelFamily normalization.
		requiredFamily := modelFamily(requiredModel)
		var bestWorker *WorkerState
		for _, ws := range stateMap {
			if modelFamily(ws.Model) != requiredFamily {
				continue
			}
			if task.RequireClaudeRuntime && !isClaudeRuntimeModel(ws.Model) {
				continue
			}
			if ws.PendingCount >= maxPending {
				continue
			}
			if bestWorker == nil ||
				ws.PendingCount < bestWorker.PendingCount ||
				(ws.PendingCount == bestWorker.PendingCount && ws.WorkerID < bestWorker.WorkerID) {
				bestWorker = ws
			}
		}

		if bestWorker == nil {
			if task.RequireClaudeRuntime {
				hasClaudeWorker := false
				for _, ws := range stateMap {
					if isClaudeRuntimeModel(ws.Model) {
						hasClaudeWorker = true
						break
					}
				}
				if !hasClaudeWorker {
					return nil, fmt.Errorf("%w for task %q: no claude-code workers configured; run_on_main tasks require a claude-code worker because only claude-code enforces the read-only main guard",
						ErrNoAvailableWorker, task.Name)
				}
			}
			// Distinguish "no workers with matching family" from "matching workers at capacity"
			hasMatchingModel := false
			for _, ws := range stateMap {
				if modelFamily(ws.Model) == requiredFamily {
					hasMatchingModel = true
					break
				}
			}
			if !hasMatchingModel {
				return nil, fmt.Errorf("%w for task %q (model=%s): no workers configured for model %q (bloom_level=%d requires %s; use bloom_level 1-3 for sonnet or enable boost mode for opus)",
					ErrNoAvailableWorker, task.Name, requiredModel, requiredModel, task.BloomLevel, requiredModel)
			}
			return nil, fmt.Errorf("%w for task %q (model=%s): all %s workers at capacity (max_pending_tasks_per_worker=%d)",
				ErrNoAvailableWorker, task.Name, requiredModel, requiredModel, maxPending)
		}

		assignments = append(assignments, WorkerAssignment{
			TaskName: task.Name,
			WorkerID: bestWorker.WorkerID,
			Model:    bestWorker.Model,
		})
		bestWorker.PendingCount++
	}

	return assignments, nil
}

// BuildWorkerStates reads queue files to build the current load state for each configured worker.
func BuildWorkerStates(maestroDir string, config model.WorkerConfig) ([]WorkerState, error) {
	if config.Count <= 0 {
		return nil, fmt.Errorf("invalid worker count %d: must be greater than 0", config.Count)
	}

	var states []WorkerState

	for i := 1; i <= config.Count; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		if err := validate.ID(workerID); err != nil {
			return nil, fmt.Errorf("invalid worker ID %q: %w", workerID, err)
		}
		workerModel := GetWorkerModel(workerID, config)

		pendingCount := 0
		queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(workerID))
		data, err := os.ReadFile(queueFile) //nolint:gosec // queueFile is constructed from a controlled application queue directory
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("read queue file %s: %w", queueFile, err)
			}
			// os.ErrNotExist → treat as empty queue (pendingCount=0)
		} else {
			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(data, &tq); err != nil {
				return nil, fmt.Errorf("parse queue file %s: %w", queueFile, err)
			}
			for _, task := range tq.Tasks {
				// Count pending + in_progress together for load balancing.
				// Excluding in_progress would let a long-running task occupy
				// a worker but read as PendingCount=0 to the next submit
				// batch, which the tie-break would then always resolve to
				// worker1 — leaving the other workers idle (Report
				// 2026-05-06 P3).
				if task.Status == model.StatusPending || task.Status == model.StatusInProgress {
					pendingCount++
				}
			}
		}

		states = append(states, WorkerState{
			WorkerID:     workerID,
			Model:        workerModel,
			PendingCount: pendingCount,
		})
	}

	return states, nil
}

// SnapshotWorkerStates creates an independent copy of worker states,
// providing a consistent point-in-time snapshot safe to pass across
// function boundaries without risk of concurrent mutation.
func SnapshotWorkerStates(states []WorkerState) []WorkerState {
	snapshot := make([]WorkerState, len(states))
	copy(snapshot, states)
	return snapshot
}

func workerIDToQueueFile(workerID string) string {
	// worker1 → worker1.yaml
	return workerID + ".yaml"
}

// workerExistsForModel reports whether at least one worker in stateMap is
// configured for the given model name. Comparison is performed at the
// modelFamily level so full Claude model IDs (e.g. "claude-opus-4-7") match
// a request for the short family alias ("opus") and vice versa.
func workerExistsForModel(stateMap map[string]*WorkerState, modelName string) bool {
	family := modelFamily(modelName)
	for _, ws := range stateMap {
		if modelFamily(ws.Model) == family {
			return true
		}
	}
	return false
}

// familyFallbackOrder defines, for each required family, the preferred
// substitution order when no worker for that family is configured. The
// sequence prefers equally- or over-qualified substitutes over
// under-qualified ones so that capability is preserved when possible:
//
//   - sonnet → opus (upgrade — fine) → haiku (downgrade — warned)
//   - opus   → sonnet (downgrade — warned) → haiku (larger downgrade)
//   - haiku  → sonnet (upgrade) → opus (larger upgrade)
//
// Keys are the canonical family aliases returned by modelFamily().
var familyFallbackOrder = map[string][]string{
	"sonnet": {"opus", "haiku"},
	"opus":   {"sonnet", "haiku"},
	"haiku":  {"sonnet", "opus"},
}

// chooseFallbackFamily returns the preferred fallback family name when no
// worker exists for the required family. Returns "" if none of the
// preferred fallbacks is available in stateMap. Callers should treat the
// empty-string result as "assignment infeasible" and surface an error —
// this function only proposes a substitute; it does not guarantee one.
//
// For non-Claude deployments (e.g. a codex-only or gemini-only worker fleet),
// the Bloom-level mapping always produces a Claude family name ("sonnet"/"opus"),
// which never matches the provisioned workers. In that case the function falls
// back to any model actually present in the fleet so that tasks can still be
// assigned — a capability mismatch warning is emitted by the caller.
//
// requireClaude restricts the last-resort scan to claude-runtime models.
// Without it, a RequireClaudeRuntime task in a mixed fleet could receive a
// codex/gemini family (map iteration order dependent), which the eligibility
// loop then rejects wholesale and the assignment fails with a misleading
// "at capacity" error even though a claude worker is available.
func chooseFallbackFamily(stateMap map[string]*WorkerState, required string, requireClaude bool) string {
	family := modelFamily(required)
	prefs, ok := familyFallbackOrder[family]
	if !ok {
		// Custom / third-party model: try every known family as a last resort.
		prefs = []string{"opus", "sonnet", "haiku"}
	}
	for _, candidate := range prefs {
		if workerExistsForModel(stateMap, candidate) {
			return candidate
		}
	}
	// Last resort: return any model actually provisioned in the fleet that
	// is NOT the required family. This handles non-Claude deployments
	// (e.g. all workers configured with model="codex" or model="gemini")
	// where none of the Claude family fallbacks exist but tasks still need
	// to be assigned. Excluding the required family avoids a circular
	// result where we "fallback" to a family that already has no workers.
	for _, ws := range stateMap {
		if requireClaude && !isClaudeRuntimeModel(ws.Model) {
			continue
		}
		if ws.Model != "" && modelFamily(ws.Model) != family {
			return ws.Model
		}
	}
	return ""
}

// AdaptiveModelSelector / BanditSelector / defaultModelArms are intentionally
// not declared in this package. The production model-selection path lives in
// internal/daemon (banditModelSelector) so that result_handler can record
// rewards without taking a dependency on plan. AssignWorkers consumes the
// daemon's selector via the small ModelSelector interface above; a return
// value of "" means "no adaptive pick — keep the static GetModelForBloomLevel
// mapping". See internal/daemon/model_selector.go for the production
// implementation and internal/daemon/model_selector_test.go for warm-up /
// fallback behavior.
