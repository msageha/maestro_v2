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
		{PhaseStatusFailed, PhaseStatusCompleted},
		{PhaseStatusFailed, PhaseStatusPending},
		{PhaseStatusFailed, PhaseStatusFilling},
		{PhaseStatusFailed, PhaseStatusAwaitingFill},
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidatePhaseTransition(tt.from, tt.to); err == nil {
				t.Errorf("expected error for %q → %q", tt.from, tt.to)
			}
		})
	}
}

func TestIsWorktreeTerminal(t *testing.T) {
	tests := []struct {
		status   WorktreeStatus
		terminal bool
	}{
		{WorktreeStatusCreated, false},
		{WorktreeStatusActive, false},
		{WorktreeStatusCommitted, false},
		{WorktreeStatusIntegrated, false},
		{WorktreeStatusPublished, false},
		{WorktreeStatusConflict, false},
		{WorktreeStatusFailed, false},
		{WorktreeStatusCleanupDone, true},
		{WorktreeStatusCleanupFailed, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsWorktreeTerminal(tt.status); got != tt.terminal {
				t.Errorf("IsWorktreeTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
			}
		})
	}
}

func TestIsIntegrationTerminal(t *testing.T) {
	tests := []struct {
		status   IntegrationStatus
		terminal bool
	}{
		{IntegrationStatusCreated, false},
		{IntegrationStatusMerging, false},
		{IntegrationStatusMerged, false},
		{IntegrationStatusPublishing, false},
		{IntegrationStatusConflict, false},
		{IntegrationStatusPublished, true},
		{IntegrationStatusFailed, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsIntegrationTerminal(tt.status); got != tt.terminal {
				t.Errorf("IsIntegrationTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
			}
		})
	}
}

func TestIsContinuousTerminal(t *testing.T) {
	tests := []struct {
		status   ContinuousStatus
		terminal bool
	}{
		{ContinuousStatusIdle, false},
		{ContinuousStatusRunning, false},
		{ContinuousStatusPaused, false},
		{ContinuousStatusStopped, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsContinuousTerminal(tt.status); got != tt.terminal {
				t.Errorf("IsContinuousTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
			}
		})
	}
}

func TestValidateWorktreeTransition(t *testing.T) {
	valid := []struct {
		from, to WorktreeStatus
	}{
		{WorktreeStatusCreated, WorktreeStatusActive},
		{WorktreeStatusCreated, WorktreeStatusCommitted},
		{WorktreeStatusCreated, WorktreeStatusFailed},
		{WorktreeStatusActive, WorktreeStatusCommitted},
		{WorktreeStatusActive, WorktreeStatusConflict},
		{WorktreeStatusActive, WorktreeStatusFailed},
		{WorktreeStatusCommitted, WorktreeStatusIntegrated},
		{WorktreeStatusCommitted, WorktreeStatusFailed},
		{WorktreeStatusIntegrated, WorktreeStatusPublished},
		{WorktreeStatusIntegrated, WorktreeStatusActive},
		{WorktreeStatusIntegrated, WorktreeStatusFailed},
		{WorktreeStatusPublished, WorktreeStatusCleanupDone},
		{WorktreeStatusPublished, WorktreeStatusCleanupFailed},
		{WorktreeStatusConflict, WorktreeStatusActive},
		{WorktreeStatusConflict, WorktreeStatusFailed},
		{WorktreeStatusFailed, WorktreeStatusCleanupDone},
		{WorktreeStatusFailed, WorktreeStatusCleanupFailed},
	}
	for _, tt := range valid {
		t.Run(string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateWorktreeTransition(tt.from, tt.to); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}

	invalid := []struct {
		from, to WorktreeStatus
	}{
		{WorktreeStatusCleanupDone, WorktreeStatusActive},
		{WorktreeStatusCleanupFailed, WorktreeStatusActive},
		{WorktreeStatusCreated, WorktreeStatusIntegrated},
		{WorktreeStatusCreated, WorktreeStatusPublished},
		{WorktreeStatusActive, WorktreeStatusPublished},
		{WorktreeStatusPublished, WorktreeStatusActive},
		{WorktreeStatusCommitted, WorktreeStatusActive},
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateWorktreeTransition(tt.from, tt.to); err == nil {
				t.Errorf("expected error for %q → %q", tt.from, tt.to)
			}
		})
	}
}

func TestValidateIntegrationTransition(t *testing.T) {
	valid := []struct {
		from, to IntegrationStatus
	}{
		{IntegrationStatusCreated, IntegrationStatusMerging},
		{IntegrationStatusCreated, IntegrationStatusFailed},
		{IntegrationStatusMerging, IntegrationStatusMerged},
		{IntegrationStatusMerging, IntegrationStatusConflict},
		{IntegrationStatusMerging, IntegrationStatusFailed},
		{IntegrationStatusMerged, IntegrationStatusPublishing},
		{IntegrationStatusMerged, IntegrationStatusFailed},
		{IntegrationStatusPublishing, IntegrationStatusPublished},
		{IntegrationStatusPublishing, IntegrationStatusConflict},
		{IntegrationStatusPublishing, IntegrationStatusFailed},
		{IntegrationStatusConflict, IntegrationStatusMerging},
		{IntegrationStatusConflict, IntegrationStatusFailed},
	}
	for _, tt := range valid {
		t.Run(string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateIntegrationTransition(tt.from, tt.to); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}

	invalid := []struct {
		from, to IntegrationStatus
	}{
		{IntegrationStatusPublished, IntegrationStatusMerging},
		{IntegrationStatusFailed, IntegrationStatusMerging},
		{IntegrationStatusCreated, IntegrationStatusMerged},
		{IntegrationStatusCreated, IntegrationStatusPublished},
		{IntegrationStatusMerging, IntegrationStatusPublishing},
		{IntegrationStatusMerged, IntegrationStatusMerging},
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateIntegrationTransition(tt.from, tt.to); err == nil {
				t.Errorf("expected error for %q → %q", tt.from, tt.to)
			}
		})
	}
}

func TestValidateContinuousTransition(t *testing.T) {
	valid := []struct {
		from, to ContinuousStatus
	}{
		{ContinuousStatusIdle, ContinuousStatusRunning},
		{ContinuousStatusRunning, ContinuousStatusPaused},
		{ContinuousStatusRunning, ContinuousStatusStopped},
		{ContinuousStatusPaused, ContinuousStatusRunning},
		{ContinuousStatusPaused, ContinuousStatusStopped},
	}
	for _, tt := range valid {
		t.Run(string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateContinuousTransition(tt.from, tt.to); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}

	invalid := []struct {
		from, to ContinuousStatus
	}{
		{ContinuousStatusStopped, ContinuousStatusRunning},
		{ContinuousStatusStopped, ContinuousStatusPaused},
		{ContinuousStatusIdle, ContinuousStatusPaused},
		{ContinuousStatusIdle, ContinuousStatusStopped},
		{ContinuousStatusRunning, ContinuousStatusIdle},
		{ContinuousStatusPaused, ContinuousStatusIdle},
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateContinuousTransition(tt.from, tt.to); err == nil {
				t.Errorf("expected error for %q → %q", tt.from, tt.to)
			}
		})
	}
}

func TestValidateNotificationType(t *testing.T) {
	valid := []NotificationType{
		NotificationTypeCommandCompleted,
		NotificationTypeCommandFailed,
		NotificationTypeCommandCancelled,
	}
	for _, nt := range valid {
		t.Run(string(nt), func(t *testing.T) {
			if err := ValidateNotificationType(nt); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}

	invalid := []NotificationType{
		"",
		"invalid_type",
		"task_completed",
		"phase_complete",
	}
	for _, nt := range invalid {
		name := string(nt)
		if name == "" {
			name = "empty"
		}
		t.Run("invalid_"+name, func(t *testing.T) {
			if err := ValidateNotificationType(nt); err == nil {
				t.Errorf("expected error for %q", nt)
			}
		})
	}
}
