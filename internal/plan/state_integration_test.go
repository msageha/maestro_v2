//go:build integration

package plan

import (
	"errors"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// TestCanComplete_AwaitingFillPhase verifies that CanComplete returns an
// ActionRequiredError when a phase is in awaiting_fill status.
func TestCanComplete_AwaitingFillPhase(t *testing.T) {
	state := &model.CommandState{
		CommandID:  "cmd-af-001",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 2,
			RequiredTaskIDs:   []string{"t1", "t2"},
			OptionalTaskIDs:   []string{},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
				"t2": model.StatusCompleted,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "phase-research",
					Name:    "research",
					Status:  model.PhaseStatusCompleted,
				},
				{
					PhaseID: "phase-impl",
					Name:    "implementation",
					Status:  model.PhaseStatusAwaitingFill,
				},
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("CanComplete should return error for awaiting_fill phase")
	}

	var actionErr *ActionRequiredError
	if !errors.As(err, &actionErr) {
		t.Fatalf("expected ActionRequiredError, got %T: %v", err, err)
	}

	if actionErr.Reason != "PHASE_AWAITING_FILL" {
		t.Errorf("Reason = %q, want PHASE_AWAITING_FILL", actionErr.Reason)
	}
	if actionErr.CommandID != "cmd-af-001" {
		t.Errorf("CommandID = %q, want cmd-af-001", actionErr.CommandID)
	}
	if actionErr.PhaseID != "phase-impl" {
		t.Errorf("PhaseID = %q, want phase-impl", actionErr.PhaseID)
	}
	if actionErr.PhaseName != "implementation" {
		t.Errorf("PhaseName = %q, want implementation", actionErr.PhaseName)
	}
	if actionErr.PhaseStatus != "awaiting_fill" {
		t.Errorf("PhaseStatus = %q, want awaiting_fill", actionErr.PhaseStatus)
	}
	if actionErr.ErrorCode() != "ACTION_REQUIRED" {
		t.Errorf("ErrorCode() = %q, want ACTION_REQUIRED", actionErr.ErrorCode())
	}
}

// TestCanComplete_AwaitingFillFormatStderr verifies that the FormatStderr output
// contains all key-value pairs that LLM agents can parse.
func TestCanComplete_AwaitingFillFormatStderr(t *testing.T) {
	state := &model.CommandState{
		CommandID:  "cmd-af-002",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{"t1"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "ph-impl",
					Name:    "implementation",
					Status:  model.PhaseStatusAwaitingFill,
				},
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("expected error")
	}

	var actionErr *ActionRequiredError
	if !errors.As(err, &actionErr) {
		t.Fatalf("expected ActionRequiredError, got %T", err)
	}

	stderr := actionErr.FormatStderr()

	expectedParts := []string{
		"error: action required (PHASE_AWAITING_FILL)",
		"command_id: cmd-af-002",
		"phase_id: ph-impl",
		"phase: implementation",
		"phase_status: awaiting_fill",
		"next_action: maestro plan submit --command-id cmd-af-002 --phase implementation --tasks-file plan.yaml",
	}

	for _, part := range expectedParts {
		if !strings.Contains(stderr, part) {
			t.Errorf("FormatStderr() missing %q\ngot:\n%s", part, stderr)
		}
	}
}

// TestCanComplete_AwaitingFillIsNotRetryable verifies that ActionRequiredError is
// distinct from retryableError.
func TestCanComplete_AwaitingFillIsNotRetryable(t *testing.T) {
	state := &model.CommandState{
		CommandID:  "cmd-af-003",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{"t1"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "ph-1",
					Name:    "phase-1",
					Status:  model.PhaseStatusAwaitingFill,
				},
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("expected error")
	}

	var retryErr *retryableError
	if errors.As(err, &retryErr) {
		t.Error("ActionRequiredError should NOT satisfy retryableError")
	}
}

// TestCanComplete_FillingStillRetryable verifies that filling phase still
// returns retryableError (regression check).
func TestCanComplete_FillingStillRetryable(t *testing.T) {
	state := &model.CommandState{
		CommandID:  "cmd-fill-001",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{"t1"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "ph-1",
					Name:    "phase-1",
					Status:  model.PhaseStatusFilling,
				},
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("expected error for filling phase")
	}

	var retryErr *retryableError
	if !errors.As(err, &retryErr) {
		t.Errorf("filling phase should return retryableError, got %T: %v", err, err)
	}
}

// TestCanComplete_ActivePhaseAllTasksTerminalRetryable verifies the
// transient race window: when a phase is still Active but every task
// inside it is terminal, the daemon's queue_scan_phase_a is moments
// away from flipping the phase to Completed (gated on merge_recorded).
// CanComplete must surface a retryableError so Complete() can write a
// deferred_complete intent instead of bouncing the Planner with a
// non-retryable validation error.
func TestCanComplete_ActivePhaseAllTasksTerminalRetryable(t *testing.T) {
	state := &model.CommandState{
		CommandID:  "cmd-active-race-001",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{"t1"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "ph-verify",
					Name:    "verification",
					Status:  model.PhaseStatusActive,
					TaskIDs: []string{"t1"},
				},
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("expected error for active phase with all tasks terminal")
	}

	var retryErr *retryableError
	if !errors.As(err, &retryErr) {
		t.Errorf("expected retryableError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "verification") {
		t.Errorf("error should reference phase name: %v", err)
	}
}

// TestCanComplete_ActivePhaseSomeTasksPendingNotRetryable verifies the
// non-race case: when a phase is Active and at least one of its tasks
// is non-terminal, CanComplete must return planValidationError (NOT
// retryable). Otherwise the deferred_complete pattern would mask a
// real "Planner called complete too early" mistake.
func TestCanComplete_ActivePhaseSomeTasksPendingNotRetryable(t *testing.T) {
	state := &model.CommandState{
		CommandID:  "cmd-active-pending-001",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 2,
			RequiredTaskIDs:   []string{"t1", "t2"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
				"t2": model.StatusInProgress,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "ph-impl",
					Name:    "implementation",
					Status:  model.PhaseStatusActive,
					TaskIDs: []string{"t1", "t2"},
				},
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("expected error for active phase with non-terminal task")
	}

	var retryErr *retryableError
	if errors.As(err, &retryErr) {
		t.Errorf("active phase with non-terminal task must NOT be retryable, got: %v", err)
	}
	var pve *planValidationError
	if !errors.As(err, &pve) {
		t.Errorf("expected planValidationError, got %T: %v", err, err)
	}
}

// TestCanComplete_ActivePhaseFailedTaskNotRetryable verifies that the
// deferred_publish path is only valid when Phase B will actually merge the
// phase, which requires every task at completed (or cancelled, the
// supersede-by-retry marker). A failed task means the phase is destined
// for PhaseStatusFailed via the dependency resolver, so without this guard
// the deferred-publish path would write an intent file that never gets
// cleared and hand the Planner a misleading "publish 待ち" status.
func TestCanComplete_ActivePhaseFailedTaskNotRetryable(t *testing.T) {
	state := &model.CommandState{
		CommandID:  "cmd-active-failed-001",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 2,
			RequiredTaskIDs:   []string{"t1", "t2"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
				"t2": model.StatusFailed, // genuine failure → phase must fail, not defer
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "ph-impl",
					Name:    "implementation",
					Status:  model.PhaseStatusActive,
					TaskIDs: []string{"t1", "t2"},
				},
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("expected error for active phase with failed task")
	}
	var retryErr *retryableError
	if errors.As(err, &retryErr) {
		t.Errorf("failed task must NOT yield retryableError (would dangle deferred_complete intent): %v", err)
	}
	var pve *planValidationError
	if !errors.As(err, &pve) {
		t.Errorf("expected planValidationError, got %T: %v", err, err)
	}
}

// TestCanComplete_ActivePhaseCompletedAndCancelledRetryable covers the
// retry-supersede case: a task at StatusCancelled because its retry
// completed should still allow the deferred-publish race window. Phase B
// will merge the retry's contribution, the deferred completer rides on
// that merge, and the cancelled predecessor is just a lineage marker.
func TestCanComplete_ActivePhaseCompletedAndCancelledRetryable(t *testing.T) {
	state := &model.CommandState{
		CommandID:  "cmd-active-supersede-001",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			// ExpectedTaskCount and RequiredTaskIDs reflect the state after
			// retry replacement: the original t1 was cancelled and t1_retry
			// took its slot in RequiredTaskIDs (see task_retry_handler.go's
			// replaceTaskMembership). The cancelled predecessor stays in
			// TaskStates for lineage but no longer counts toward required.
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{"t1_retry"},
			OptionalTaskIDs:   []string{},
			TaskStates: map[string]model.Status{
				"t1":       model.StatusCancelled, // superseded by retry
				"t1_retry": model.StatusCompleted, // retry succeeded
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "ph-impl",
					Name:    "implementation",
					Status:  model.PhaseStatusActive, // merge_recorded gate not flipped yet
					TaskIDs: []string{"t1", "t1_retry"},
				},
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("expected retryable error for active phase with cancelled+completed tasks")
	}
	var retryErr *retryableError
	if !errors.As(err, &retryErr) {
		t.Errorf("expected retryableError so Complete() writes deferred_complete intent, got %T: %v", err, err)
	}
}

// TestCanComplete_CompletedPhasesNoError verifies that all-completed phases
// do not trigger ActionRequiredError.
func TestCanComplete_CompletedPhasesNoError(t *testing.T) {
	state := &model.CommandState{
		CommandID:  "cmd-ok-001",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 2,
			RequiredTaskIDs:   []string{"t1", "t2"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
				"t2": model.StatusCompleted,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{PhaseID: "ph-1", Name: "research", Status: model.PhaseStatusCompleted},
				{PhaseID: "ph-2", Name: "implementation", Status: model.PhaseStatusCompleted},
			},
		},
	}

	status, err := CanComplete(state)
	if err != nil {
		t.Fatalf("CanComplete returned error for all-completed phases: %v", err)
	}
	if status != model.PlanStatusCompleted {
		t.Errorf("status = %q, want completed", status)
	}
}
