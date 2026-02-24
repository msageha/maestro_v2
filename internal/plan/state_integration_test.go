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
		CommandID:         "cmd-af-001",
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1", "t2"},
		OptionalTaskIDs:   []string{},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCompleted,
		},
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
		CommandID:         "cmd-af-002",
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		Phases: []model.Phase{
			{
				PhaseID: "ph-impl",
				Name:    "implementation",
				Status:  model.PhaseStatusAwaitingFill,
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
// distinct from RetryableError.
func TestCanComplete_AwaitingFillIsNotRetryable(t *testing.T) {
	state := &model.CommandState{
		CommandID:         "cmd-af-003",
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		Phases: []model.Phase{
			{
				PhaseID: "ph-1",
				Name:    "phase-1",
				Status:  model.PhaseStatusAwaitingFill,
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("expected error")
	}

	var retryErr *RetryableError
	if errors.As(err, &retryErr) {
		t.Error("ActionRequiredError should NOT satisfy RetryableError")
	}
}

// TestCanComplete_FillingStillRetryable verifies that filling phase still
// returns RetryableError (regression check).
func TestCanComplete_FillingStillRetryable(t *testing.T) {
	state := &model.CommandState{
		CommandID:         "cmd-fill-001",
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		Phases: []model.Phase{
			{
				PhaseID: "ph-1",
				Name:    "phase-1",
				Status:  model.PhaseStatusFilling,
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("expected error for filling phase")
	}

	var retryErr *RetryableError
	if !errors.As(err, &retryErr) {
		t.Errorf("filling phase should return RetryableError, got %T: %v", err, err)
	}
}

// TestCanComplete_CompletedPhasesNoError verifies that all-completed phases
// do not trigger ActionRequiredError.
func TestCanComplete_CompletedPhasesNoError(t *testing.T) {
	state := &model.CommandState{
		CommandID:         "cmd-ok-001",
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1", "t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCompleted,
		},
		Phases: []model.Phase{
			{PhaseID: "ph-1", Name: "research", Status: model.PhaseStatusCompleted},
			{PhaseID: "ph-2", Name: "implementation", Status: model.PhaseStatusCompleted},
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
