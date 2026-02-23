package model

import "testing"

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		status   Status
		terminal bool
	}{
		{StatusPending, false},
		{StatusInProgress, false},
		{StatusCompleted, true},
		{StatusFailed, true},
		{StatusCancelled, true},
		{StatusDeadLetter, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsTerminal(tt.status); got != tt.terminal {
				t.Errorf("IsTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
			}
		})
	}
}

func TestIsPlanTerminal(t *testing.T) {
	tests := []struct {
		status   PlanStatus
		terminal bool
	}{
		{PlanStatusPlanning, false},
		{PlanStatusSealed, false},
		{PlanStatusCompleted, true},
		{PlanStatusFailed, true},
		{PlanStatusCancelled, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsPlanTerminal(tt.status); got != tt.terminal {
				t.Errorf("IsPlanTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
			}
		})
	}
}

func TestIsPhaseTerminal(t *testing.T) {
	tests := []struct {
		status   PhaseStatus
		terminal bool
	}{
		{PhaseStatusPending, false},
		{PhaseStatusAwaitingFill, false},
		{PhaseStatusFilling, false},
		{PhaseStatusActive, false},
		{PhaseStatusCompleted, true},
		{PhaseStatusFailed, true},
		{PhaseStatusCancelled, true},
		{PhaseStatusTimedOut, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsPhaseTerminal(tt.status); got != tt.terminal {
				t.Errorf("IsPhaseTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
			}
		})
	}
}

func TestValidateCommandTaskQueueTransition(t *testing.T) {
	valid := []struct {
		from, to Status
	}{
		{StatusPending, StatusInProgress},
		{StatusPending, StatusCancelled},
		{StatusPending, StatusDeadLetter},
		{StatusInProgress, StatusPending},
		{StatusInProgress, StatusCompleted},
		{StatusInProgress, StatusFailed},
		{StatusInProgress, StatusCancelled},
	}
	for _, tt := range valid {
		t.Run(string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateCommandTaskQueueTransition(tt.from, tt.to); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}

	invalid := []struct {
		from, to Status
	}{
		{StatusCompleted, StatusPending},
		{StatusCompleted, StatusInProgress},
		{StatusFailed, StatusPending},
		{StatusCancelled, StatusPending},
		{StatusDeadLetter, StatusPending},
		{StatusPending, StatusCompleted},
		{StatusPending, StatusFailed},
		{StatusInProgress, StatusDeadLetter}, // dead_letter only from pending
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateCommandTaskQueueTransition(tt.from, tt.to); err == nil {
				t.Errorf("expected error for %q → %q", tt.from, tt.to)
			}
		})
	}
}

func TestValidateNotificationQueueTransition(t *testing.T) {
	valid := []struct {
		from, to Status
	}{
		{StatusPending, StatusInProgress},
		{StatusPending, StatusDeadLetter},
		{StatusInProgress, StatusPending},
		{StatusInProgress, StatusCompleted},
	}
	for _, tt := range valid {
		t.Run(string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateNotificationQueueTransition(tt.from, tt.to); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}

	invalid := []struct {
		from, to Status
	}{
		{StatusPending, StatusCancelled},     // notifications cannot be cancelled
		{StatusPending, StatusFailed},        // notifications cannot fail
		{StatusInProgress, StatusFailed},     // notifications cannot fail
		{StatusInProgress, StatusCancelled},  // notifications cannot be cancelled
		{StatusInProgress, StatusDeadLetter}, // dead_letter only from pending
		{StatusCompleted, StatusPending},
		{StatusDeadLetter, StatusPending},
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateNotificationQueueTransition(tt.from, tt.to); err == nil {
				t.Errorf("expected error for %q → %q", tt.from, tt.to)
			}
		})
	}
}

func TestValidateTaskStateTransition(t *testing.T) {
	valid := []struct {
		from, to Status
	}{
		{StatusPending, StatusInProgress},
		{StatusPending, StatusCancelled},
		{StatusInProgress, StatusCompleted},
		{StatusInProgress, StatusFailed},
		{StatusInProgress, StatusCancelled},
	}
	for _, tt := range valid {
		t.Run(string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateTaskStateTransition(tt.from, tt.to); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}

	invalid := []struct {
		from, to Status
	}{
		{StatusCompleted, StatusPending},
		{StatusFailed, StatusPending},
		{StatusCancelled, StatusPending},
		{StatusPending, StatusCompleted},
		{StatusPending, StatusFailed},
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateTaskStateTransition(tt.from, tt.to); err == nil {
				t.Errorf("expected error for %q → %q", tt.from, tt.to)
			}
		})
	}
}

func TestValidatePlanTransition(t *testing.T) {
	valid := []struct {
		from, to PlanStatus
	}{
		{PlanStatusPlanning, PlanStatusSealed},
		{PlanStatusSealed, PlanStatusCompleted},
		{PlanStatusSealed, PlanStatusFailed},
		{PlanStatusSealed, PlanStatusCancelled},
	}
	for _, tt := range valid {
		t.Run(string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidatePlanTransition(tt.from, tt.to); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}

	invalid := []struct {
		from, to PlanStatus
	}{
		{PlanStatusCompleted, PlanStatusPlanning},
		{PlanStatusFailed, PlanStatusPlanning},
		{PlanStatusCancelled, PlanStatusPlanning},
		{PlanStatusPlanning, PlanStatusCompleted},
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidatePlanTransition(tt.from, tt.to); err == nil {
				t.Errorf("expected error for %q → %q", tt.from, tt.to)
			}
		})
	}
}

func TestValidatePhaseTransition(t *testing.T) {
	valid := []struct {
		from, to PhaseStatus
	}{
		{PhaseStatusPending, PhaseStatusAwaitingFill},
		{PhaseStatusPending, PhaseStatusCancelled},
		{PhaseStatusAwaitingFill, PhaseStatusFilling},
		{PhaseStatusAwaitingFill, PhaseStatusTimedOut},
		{PhaseStatusFilling, PhaseStatusActive},
		{PhaseStatusFilling, PhaseStatusAwaitingFill},
		{PhaseStatusActive, PhaseStatusCompleted},
		{PhaseStatusActive, PhaseStatusFailed},
		{PhaseStatusActive, PhaseStatusCancelled},
		{PhaseStatusFailed, PhaseStatusActive}, // add-retry-task reopen
	}
	for _, tt := range valid {
		t.Run(string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidatePhaseTransition(tt.from, tt.to); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}

	invalid := []struct {
		from, to PhaseStatus
	}{
		{PhaseStatusCompleted, PhaseStatusActive},
		{PhaseStatusCancelled, PhaseStatusActive},
		{PhaseStatusTimedOut, PhaseStatusActive},
		{PhaseStatusActive, PhaseStatusPending},
		{PhaseStatusFilling, PhaseStatusPending},
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidatePhaseTransition(tt.from, tt.to); err == nil {
				t.Errorf("expected error for %q → %q", tt.from, tt.to)
			}
		})
	}
}
