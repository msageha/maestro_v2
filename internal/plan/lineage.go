package plan

import (
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// CascadeBlockedReasonPrefix is the canonical prefix the daemon writes into
// CommandState.CancelledReasons[taskID] when a downstream task is cancelled
// because one of its declared upstream dependencies entered a terminal
// failed/cancelled status. The full reason has the form
// "blocked_dependency_terminal:<dep_task_id>" so that EffectiveStatusForCompletion
// can recover the dep task ID without parsing free-form text. Mirrors the
// model.DependencyCascadeCancelPrefix convention used at the phase level.
const CascadeBlockedReasonPrefix = "blocked_dependency_terminal:"

// LatestDescendant walks forward through retry_lineage to find the latest
// descendant of taskID. The lineage map is keyed by retry/successor task
// ID with the predecessor as the value (newID -> oldID); LatestDescendant
// builds the reverse mapping internally and follows oldID -> newID until
// no further successor exists.
//
// When taskID has no successor (the common case), the input is returned
// unchanged. The walk is bounded by the size of retryLineage and uses a
// visited map to defend against any cycles a corrupted state file might
// encode.
func LatestDescendant(taskID string, retryLineage map[string]string) string {
	if len(retryLineage) == 0 {
		return taskID
	}
	successor := make(map[string]string, len(retryLineage))
	for newID, oldID := range retryLineage {
		successor[oldID] = newID
	}
	cur := taskID
	visited := map[string]bool{cur: true}
	for {
		next, ok := successor[cur]
		if !ok || visited[next] {
			return cur
		}
		visited[next] = true
		cur = next
	}
}

// EffectiveStatus returns the canonical status of taskID after walking
// forward through retry_lineage to the latest descendant. When a task
// has been superseded by a retry/repair successor — the predecessor's
// raw status (typically Cancelled with a "superseded_by_*" reason) is a
// structural artefact of the retry mechanism, not a real failure. The
// successor's status is the truth that DeriveStatus and the dependency
// resolver should consult.
//
// Returns the predecessor's raw status when no lineage entry maps from
// taskID, or when the latest descendant has no recorded TaskStates entry.
func EffectiveStatus(taskID string, taskStates map[string]model.Status, retryLineage map[string]string) model.Status {
	latest := LatestDescendant(taskID, retryLineage)
	if status, ok := taskStates[latest]; ok {
		return status
	}
	return taskStates[taskID]
}

// EffectiveStatusForCompletion is EffectiveStatus extended with cascade-
// cancellation unwinding. When a task's effective status is Cancelled and
// its CancelledReason indicates the cancellation was a downstream cascade
// (CascadeBlockedReasonPrefix) AND the upstream dependency lineage has
// itself completed, the task is treated as effectively Completed for plan-
// and phase-level completion decisions.
//
// Rationale: the daemon's verify-repair path injects a repair task without
// going through AddRetryTask, so cascadeRecover never re-dispatches the
// downstream tasks that were cascade-cancelled when the original failed.
// Once the repair lineage of the upstream completes, the plan is structurally
// healthy: every required predecessor has been delivered. Without this
// unwinding, DeriveStatus returns PlanStatusCancelled because the cascade-
// cancelled stragglers still appear failed/cancelled in TaskStates, and the
// command publish step skips, leaving main untouched even after every real
// task has succeeded. The unwinding restores the plan-completion semantic
// that "lineage satisfied = task satisfied".
//
// IMPORTANT: this view is intentionally *only* used by completion checks.
// Dispatch-time predicates (IsTaskBlocked, CheckDependencyFailure) keep
// using EffectiveStatus so a downstream worker is never woken up against a
// dependency whose actual output was never produced — a cascade-unwound
// "completed" is a structural marker, not real work output.
func EffectiveStatusForCompletion(taskID string, state *model.CommandState) model.Status {
	if state == nil {
		return ""
	}
	return effectiveStatusForCompletion(taskID, state, map[string]bool{})
}

func effectiveStatusForCompletion(taskID string, state *model.CommandState, visited map[string]bool) model.Status {
	if visited[taskID] {
		return state.TaskStates[taskID]
	}
	visited[taskID] = true

	raw := EffectiveStatus(taskID, state.TaskStates, state.RetryLineage)
	if raw != model.StatusCancelled {
		return raw
	}

	// Look up the cancellation reason on the *latest* descendant of the lineage —
	// the predecessor entry is purged when retry replaces it (purgeSupersededRetryEntries),
	// so the live reason hangs off the descendant.
	latest := LatestDescendant(taskID, state.RetryLineage)
	reason, ok := state.CancelledReasons[latest]
	if !ok || !strings.HasPrefix(reason, CascadeBlockedReasonPrefix) {
		return raw
	}
	depID := strings.TrimPrefix(reason, CascadeBlockedReasonPrefix)
	if depID == "" {
		return raw
	}

	depStatus := effectiveStatusForCompletion(depID, state, visited)
	if depStatus == model.StatusCompleted {
		return model.StatusCompleted
	}
	return raw
}
