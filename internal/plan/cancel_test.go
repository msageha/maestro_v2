package plan

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestSetCancelRequested(t *testing.T) {
	state := &model.CommandState{
		CommandID: "cmd-001",
	}

	if err := SetCancelRequested(state, "user-a", "no longer needed"); err != nil {
		t.Fatalf("SetCancelRequested returned error: %v", err)
	}

	if !state.Cancel.Requested {
		t.Errorf("Cancel.Requested = false, want true")
	}
	if state.Cancel.RequestedBy == nil || *state.Cancel.RequestedBy != "user-a" {
		t.Errorf("Cancel.RequestedBy = %v, want %q", state.Cancel.RequestedBy, "user-a")
	}
	if state.Cancel.Reason == nil || *state.Cancel.Reason != "no longer needed" {
		t.Errorf("Cancel.Reason = %v, want %q", state.Cancel.Reason, "no longer needed")
	}
	if state.Cancel.RequestedAt == nil {
		t.Errorf("Cancel.RequestedAt is nil, want non-nil timestamp")
	}
	if state.UpdatedAt == "" {
		t.Errorf("UpdatedAt is empty, want non-empty timestamp")
	}
}

func TestSetCancelRequested_Idempotent(t *testing.T) {
	state := &model.CommandState{
		CommandID: "cmd-002",
	}

	if err := SetCancelRequested(state, "user-a", "first reason"); err != nil {
		t.Fatalf("first SetCancelRequested returned error: %v", err)
	}

	origAt := state.Cancel.RequestedAt
	origBy := state.Cancel.RequestedBy
	origReason := state.Cancel.Reason
	origUpdated := state.UpdatedAt

	if err := SetCancelRequested(state, "user-b", "second reason"); err != nil {
		t.Fatalf("second SetCancelRequested returned error: %v", err)
	}

	if state.Cancel.RequestedAt != origAt {
		t.Errorf("RequestedAt changed on second call: got %v, want %v", state.Cancel.RequestedAt, origAt)
	}
	if state.Cancel.RequestedBy != origBy {
		t.Errorf("RequestedBy changed on second call: got %v, want %v", state.Cancel.RequestedBy, origBy)
	}
	if state.Cancel.Reason != origReason {
		t.Errorf("Reason changed on second call: got %v, want %v", state.Cancel.Reason, origReason)
	}
	if state.UpdatedAt != origUpdated {
		t.Errorf("UpdatedAt changed on second call: got %v, want %v", state.UpdatedAt, origUpdated)
	}
}

func TestIsCancelRequested(t *testing.T) {
	t.Run("not cancelled", func(t *testing.T) {
		state := &model.CommandState{}
		if IsCancelRequested(state) {
			t.Errorf("IsCancelRequested = true, want false for fresh state")
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		state := &model.CommandState{}
		_ = SetCancelRequested(state, "user", "reason")
		if !IsCancelRequested(state) {
			t.Errorf("IsCancelRequested = false, want true after SetCancelRequested")
		}
	})
}

func TestValidateNotCancelled(t *testing.T) {
	t.Run("not cancelled returns nil", func(t *testing.T) {
		state := &model.CommandState{CommandID: "cmd-100"}
		if err := ValidateNotCancelled(state); err != nil {
			t.Errorf("ValidateNotCancelled returned error for non-cancelled state: %v", err)
		}
	})

	t.Run("cancelled returns error", func(t *testing.T) {
		state := &model.CommandState{CommandID: "cmd-101"}
		_ = SetCancelRequested(state, "admin", "abort")
		err := ValidateNotCancelled(state)
		if err == nil {
			t.Fatalf("ValidateNotCancelled returned nil, want error for cancelled state")
		}
		if got := err.Error(); got == "" {
			t.Errorf("error message is empty")
		}
	})
}
