package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestPhaseTransitionPriority(t *testing.T) {
	tests := []struct {
		status   model.PhaseStatus
		expected int
	}{
		{model.PhaseStatusFailed, 0},
		{model.PhaseStatusCancelled, 1},
		{model.PhaseStatusTimedOut, 2},
		{model.PhaseStatusCompleted, 3},
		{model.PhaseStatusAwaitingFill, 4},
		{model.PhaseStatus("unknown"), 5},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			got := phaseTransitionPriority(tt.status)
			if got != tt.expected {
				t.Errorf("phaseTransitionPriority(%q) = %d, want %d", tt.status, got, tt.expected)
			}
		})
	}
}

func TestSortPhaseTransitions(t *testing.T) {
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted, Reason: "all done"},
		{PhaseID: "p2", NewStatus: model.PhaseStatusFailed, Reason: "task failed"},
		{PhaseID: "p3", NewStatus: model.PhaseStatusAwaitingFill, Reason: "needs fill"},
		{PhaseID: "p4", NewStatus: model.PhaseStatusTimedOut, Reason: "deadline"},
		{PhaseID: "p5", NewStatus: model.PhaseStatusCancelled, Reason: "user cancel"},
	}

	sortPhaseTransitions(transitions)

	expectedOrder := []string{"p2", "p5", "p4", "p1", "p3"}
	for i, id := range expectedOrder {
		if transitions[i].PhaseID != id {
			t.Errorf("sorted[%d].PhaseID = %s, want %s", i, transitions[i].PhaseID, id)
		}
	}
}

func TestSortPhaseTransitions_StableOrder(t *testing.T) {
	// Two transitions with the same priority should preserve original order.
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted, Reason: "first"},
		{PhaseID: "p2", NewStatus: model.PhaseStatusCompleted, Reason: "second"},
		{PhaseID: "p3", NewStatus: model.PhaseStatusFailed, Reason: "third"},
	}

	sortPhaseTransitions(transitions)

	// Failed first, then two completed in original order.
	expected := []string{"p3", "p1", "p2"}
	for i, id := range expected {
		if transitions[i].PhaseID != id {
			t.Errorf("sorted[%d].PhaseID = %s, want %s", i, transitions[i].PhaseID, id)
		}
	}
}

func TestDeduplicatePhaseTransitions(t *testing.T) {
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusFailed, Reason: "task failed"},
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted, Reason: "all done"},
		{PhaseID: "p2", NewStatus: model.PhaseStatusTimedOut, Reason: "deadline"},
	}

	result := deduplicatePhaseTransitions(transitions)

	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2", len(result))
	}
	if result[0].PhaseID != "p1" || result[0].NewStatus != model.PhaseStatusFailed {
		t.Errorf("result[0] = {%s, %s}, want {p1, failed}", result[0].PhaseID, result[0].NewStatus)
	}
	if result[1].PhaseID != "p2" || result[1].NewStatus != model.PhaseStatusTimedOut {
		t.Errorf("result[1] = {%s, %s}, want {p2, timed_out}", result[1].PhaseID, result[1].NewStatus)
	}
}

func TestSortAndDeduplicatePhaseTransitions(t *testing.T) {
	// End-to-end: sort then dedup should keep highest priority per phase.
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted, Reason: "all done"},
		{PhaseID: "p1", NewStatus: model.PhaseStatusFailed, Reason: "task failed"},
		{PhaseID: "p2", NewStatus: model.PhaseStatusAwaitingFill, Reason: "needs fill"},
		{PhaseID: "p2", NewStatus: model.PhaseStatusTimedOut, Reason: "deadline"},
		{PhaseID: "p3", NewStatus: model.PhaseStatusCancelled, Reason: "user cancel"},
	}

	sortPhaseTransitions(transitions)
	result := deduplicatePhaseTransitions(transitions)

	if len(result) != 3 {
		t.Fatalf("len(result) = %d, want 3", len(result))
	}

	// p1 should be failed (priority 0), not completed (priority 3)
	if result[0].PhaseID != "p1" || result[0].NewStatus != model.PhaseStatusFailed {
		t.Errorf("result[0] = {%s, %s}, want {p1, failed}", result[0].PhaseID, result[0].NewStatus)
	}
	// p3 should be cancelled (priority 1)
	if result[1].PhaseID != "p3" || result[1].NewStatus != model.PhaseStatusCancelled {
		t.Errorf("result[1] = {%s, %s}, want {p3, cancelled}", result[1].PhaseID, result[1].NewStatus)
	}
	// p2 should be timed_out (priority 2), not awaiting_fill (priority 4)
	if result[2].PhaseID != "p2" || result[2].NewStatus != model.PhaseStatusTimedOut {
		t.Errorf("result[2] = {%s, %s}, want {p2, timed_out}", result[2].PhaseID, result[2].NewStatus)
	}
}

func TestDeduplicatePhaseTransitions_Empty(t *testing.T) {
	result := deduplicatePhaseTransitions(nil)
	if len(result) != 0 {
		t.Errorf("len(result) = %d, want 0", len(result))
	}
}

func TestSortPhaseTransitions_Empty(t *testing.T) {
	// Should not panic on empty slice.
	sortPhaseTransitions(nil)
	sortPhaseTransitions([]PhaseTransitionResult{})
}

func TestDeduplicatePhaseTransitions_NoDuplicates(t *testing.T) {
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusFailed},
		{PhaseID: "p2", NewStatus: model.PhaseStatusCompleted},
		{PhaseID: "p3", NewStatus: model.PhaseStatusTimedOut},
	}

	result := deduplicatePhaseTransitions(transitions)

	if len(result) != 3 {
		t.Fatalf("len(result) = %d, want 3", len(result))
	}
	for i, tr := range transitions {
		if result[i].PhaseID != tr.PhaseID {
			t.Errorf("result[%d].PhaseID = %s, want %s", i, result[i].PhaseID, tr.PhaseID)
		}
	}
}

// TestDeduplicatePhaseTransitions_SingleElement verifies single element passes through.
func TestDeduplicatePhaseTransitions_SingleElement(t *testing.T) {
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted, Reason: "done"},
	}
	result := deduplicatePhaseTransitions(transitions)
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if result[0].PhaseID != "p1" {
		t.Errorf("result[0].PhaseID = %s, want p1", result[0].PhaseID)
	}
}

// TestPhaseTransitionPriority_AllStatusesCovered ensures all known statuses are ordered.
func TestPhaseTransitionPriority_AllStatusesCovered(t *testing.T) {
	// Failed < Cancelled < TimedOut < Completed < AwaitingFill < default
	statuses := []model.PhaseStatus{
		model.PhaseStatusFailed,
		model.PhaseStatusCancelled,
		model.PhaseStatusTimedOut,
		model.PhaseStatusCompleted,
		model.PhaseStatusAwaitingFill,
	}
	for i := 1; i < len(statuses); i++ {
		prev := phaseTransitionPriority(statuses[i-1])
		curr := phaseTransitionPriority(statuses[i])
		if prev >= curr {
			t.Errorf("priority(%s)=%d should be < priority(%s)=%d",
				statuses[i-1], prev, statuses[i], curr)
		}
	}
}

// TestSortPhaseTransitions_SingleElement verifies no panic on single element.
func TestSortPhaseTransitions_SingleElement(t *testing.T) {
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted},
	}
	sortPhaseTransitions(transitions)
	if transitions[0].PhaseID != "p1" {
		t.Errorf("PhaseID = %s, want p1", transitions[0].PhaseID)
	}
}

// TestStepPhaseTransitions_SkipsNonInProgress verifies that stepPhaseTransitions
// skips commands that are not in_progress.
func TestStepPhaseTransitions_SkipsNonInProgress(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{ID: "p1", Name: "phase1", Status: model.PhaseStatusPending, DependsOn: nil},
	})
	qh.SetStateReader(reader)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusPending},
					{ID: "cmd2", Status: model.StatusCompleted},
					{ID: "cmd3", Status: model.StatusFailed},
				},
			},
		},
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: buildSignalIndex(nil),
	}

	qh.stepPhaseTransitions(&s)

	// No transitions should be applied for non-in_progress commands
	transitions := reader.getTransitions()
	if len(transitions) != 0 {
		t.Errorf("expected 0 transitions for non-in_progress commands, got %d", len(transitions))
	}
}

// TestStepPhaseTransitions_NoStateReader verifies that stepPhaseTransitions
// is a no-op when no state reader is configured.
func TestStepPhaseTransitions_NoStateReader(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)
	// No state reader set

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
				},
			},
		},
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: buildSignalIndex(nil),
	}

	// Must not panic
	qh.stepPhaseTransitions(&s)

	if len(s.signals.Data.Signals) != 0 {
		t.Errorf("expected 0 signals with no state reader, got %d", len(s.signals.Data.Signals))
	}
}

// TestStepPlannerSignals_EmptySignals verifies that stepPlannerSignals is a
// no-op when no signals exist.
func TestStepPlannerSignals_EmptySignals(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)

	s := scanState{
		signals: fileState[model.PlannerSignalQueue]{
			Data: model.PlannerSignalQueue{Signals: nil},
		},
	}

	// Must not panic or modify state
	qh.stepPlannerSignals(&s)

	if s.signals.Dirty {
		t.Error("expected signals not dirty when empty")
	}
}
