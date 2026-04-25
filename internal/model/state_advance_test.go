package model

import "testing"

func TestAdvanceTaskState_DirectTransitions(t *testing.T) {
	tests := []struct {
		name    string
		current Status
		target  Status
	}{
		{"pending_to_in_progress", StatusPending, StatusInProgress},
		{"pending_to_cancelled", StatusPending, StatusCancelled},
		{"in_progress_to_verify_pending", StatusInProgress, StatusVerifyPending},
		{"in_progress_to_failed", StatusInProgress, StatusFailed},
		{"verify_pending_to_completed", StatusVerifyPending, StatusCompleted},
		{"verify_pending_to_repair_pending", StatusVerifyPending, StatusRepairPending},
		{"repair_pending_to_paused_for_replan", StatusRepairPending, StatusPausedForReplan},
		// Universal transitions
		{"in_progress_to_aborted", StatusInProgress, StatusAborted},
		{"in_progress_to_paused_for_human", StatusInProgress, StatusPausedForHuman},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			states := map[string]Status{"t1": tt.current}
			if err := AdvanceTaskState(states, "t1", tt.target); err != nil {
				t.Fatalf("AdvanceTaskState(%s → %s): %v", tt.current, tt.target, err)
			}
			if got := states["t1"]; got != tt.target {
				t.Errorf("after advance: got %s, want %s", got, tt.target)
			}
		})
	}
}

func TestAdvanceTaskState_MultiHopPaths(t *testing.T) {
	tests := []struct {
		name    string
		current Status
		target  Status
		final   Status // expected final state (== target unless noted)
	}{
		// in_progress is a composite of dispatched/running; advancing to
		// completed must traverse verify_pending per §2.1.
		{"in_progress_to_completed", StatusInProgress, StatusCompleted, StatusCompleted},
		{"in_progress_to_repair_pending", StatusInProgress, StatusRepairPending, StatusRepairPending},
		{"in_progress_to_paused_for_replan", StatusInProgress, StatusPausedForReplan, StatusPausedForReplan},
		// Extended state pipeline
		{"running_to_completed", StatusRunning, StatusCompleted, StatusCompleted},
		{"running_to_paused_for_replan", StatusRunning, StatusPausedForReplan, StatusPausedForReplan},
		{"verify_pending_to_paused_for_replan", StatusVerifyPending, StatusPausedForReplan, StatusPausedForReplan},
		// Pending → completed cannot happen directly; should advance through in_progress → verify_pending.
		{"pending_to_completed", StatusPending, StatusCompleted, StatusCompleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			states := map[string]Status{"t1": tt.current}
			if err := AdvanceTaskState(states, "t1", tt.target); err != nil {
				t.Fatalf("AdvanceTaskState(%s → %s): %v", tt.current, tt.target, err)
			}
			if got := states["t1"]; got != tt.final {
				t.Errorf("after advance: got %s, want %s", got, tt.final)
			}
		})
	}
}

func TestAdvanceTaskState_Idempotent(t *testing.T) {
	states := map[string]Status{"t1": StatusCompleted}
	if err := AdvanceTaskState(states, "t1", StatusCompleted); err != nil {
		t.Fatalf("idempotent advance to same state should succeed, got: %v", err)
	}
	if got := states["t1"]; got != StatusCompleted {
		t.Errorf("got %s, want %s", got, StatusCompleted)
	}
}

func TestAdvanceTaskState_RejectsTerminal(t *testing.T) {
	tests := []struct {
		name    string
		current Status
		target  Status
	}{
		{"completed_to_failed", StatusCompleted, StatusFailed},
		{"failed_to_completed", StatusFailed, StatusCompleted},
		{"cancelled_to_completed", StatusCancelled, StatusCompleted},
		{"aborted_to_paused_for_replan", StatusAborted, StatusPausedForReplan},
		{"dead_letter_to_in_progress", StatusDeadLetter, StatusInProgress},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			states := map[string]Status{"t1": tt.current}
			if err := AdvanceTaskState(states, "t1", tt.target); err == nil {
				t.Errorf("expected error for terminal %s → %s, got nil", tt.current, tt.target)
			}
		})
	}
}

func TestAdvanceTaskState_RejectsMissingTask(t *testing.T) {
	states := map[string]Status{"t1": StatusInProgress}
	if err := AdvanceTaskState(states, "missing", StatusVerifyPending); err == nil {
		t.Error("expected error for unknown task ID, got nil")
	}
}

func TestAdvanceTaskState_RejectsNilStates(t *testing.T) {
	if err := AdvanceTaskState(nil, "t1", StatusCompleted); err == nil {
		t.Error("expected error for nil states map, got nil")
	}
}

func TestAdvanceTaskState_PreservesOtherTasks(t *testing.T) {
	states := map[string]Status{
		"t1": StatusInProgress,
		"t2": StatusPending,
		"t3": StatusCompleted,
	}
	if err := AdvanceTaskState(states, "t1", StatusCompleted); err != nil {
		t.Fatalf("advance t1: %v", err)
	}
	if states["t2"] != StatusPending {
		t.Errorf("t2 mutated to %s", states["t2"])
	}
	if states["t3"] != StatusCompleted {
		t.Errorf("t3 mutated to %s", states["t3"])
	}
	if states["t1"] != StatusCompleted {
		t.Errorf("t1 = %s, want completed", states["t1"])
	}
}

func TestTerminateForAbort_FromNonTerminal(t *testing.T) {
	tests := []Status{
		StatusPending,
		StatusInProgress,
		StatusPlanned,
		StatusReady,
		StatusDispatched,
		StatusRunning,
		StatusVerifyPending,
		StatusRepairPending,
		StatusPausedForReplan,
		StatusPausedForHuman,
	}
	for _, from := range tests {
		t.Run(string(from), func(t *testing.T) {
			states := map[string]Status{"t1": from}
			if err := TerminateForAbort(states, "t1"); err != nil {
				t.Fatalf("abort from %s: %v", from, err)
			}
			if states["t1"] != StatusAborted {
				t.Errorf("got %s, want aborted", states["t1"])
			}
		})
	}
}

func TestTerminateForAbort_Idempotent(t *testing.T) {
	states := map[string]Status{"t1": StatusAborted}
	if err := TerminateForAbort(states, "t1"); err != nil {
		t.Fatalf("idempotent abort should succeed, got: %v", err)
	}
}

func TestTerminateForAbort_RejectsOtherTerminal(t *testing.T) {
	tests := []Status{StatusCompleted, StatusFailed, StatusCancelled, StatusDeadLetter}
	for _, from := range tests {
		t.Run(string(from), func(t *testing.T) {
			states := map[string]Status{"t1": from}
			if err := TerminateForAbort(states, "t1"); err == nil {
				t.Errorf("expected error aborting from %s, got nil", from)
			}
		})
	}
}
