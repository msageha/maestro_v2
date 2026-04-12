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
		// §2.1 extended states
		{StatusPlanned, false},
		{StatusReady, false},
		{StatusDispatched, false},
		{StatusRunning, false},
		{StatusVerifyPending, false},
		{StatusRepairPending, false},
		{StatusPausedForReplan, false},
		{StatusPausedForHuman, false},
		{StatusAborted, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsTerminal(tt.status); got != tt.terminal {
				t.Errorf("IsTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
			}
		})
	}
}

func TestIsActiveStatus(t *testing.T) {
	tests := []struct {
		status Status
		active bool
	}{
		{StatusPending, false},
		{StatusInProgress, false},
		{StatusCompleted, false},
		{StatusFailed, false},
		{StatusCancelled, false},
		{StatusDeadLetter, false},
		{StatusPlanned, false},
		{StatusReady, false},
		{StatusDispatched, true},
		{StatusRunning, true},
		{StatusVerifyPending, true},
		{StatusRepairPending, true},
		{StatusPausedForReplan, false},
		{StatusPausedForHuman, false},
		{StatusAborted, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsActiveStatus(tt.status); got != tt.active {
				t.Errorf("IsActiveStatus(%q) = %v, want %v", tt.status, got, tt.active)
			}
		})
	}
}

func TestIsPausedStatus(t *testing.T) {
	tests := []struct {
		status Status
		paused bool
	}{
		{StatusPending, false},
		{StatusInProgress, false},
		{StatusCompleted, false},
		{StatusFailed, false},
		{StatusCancelled, false},
		{StatusDeadLetter, false},
		{StatusPlanned, false},
		{StatusReady, false},
		{StatusDispatched, false},
		{StatusRunning, false},
		{StatusVerifyPending, false},
		{StatusRepairPending, false},
		{StatusPausedForReplan, true},
		{StatusPausedForHuman, true},
		{StatusAborted, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsPausedStatus(tt.status); got != tt.paused {
				t.Errorf("IsPausedStatus(%q) = %v, want %v", tt.status, got, tt.paused)
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
		// Existing transitions
		{StatusPending, StatusInProgress},
		{StatusPending, StatusCancelled},
		{StatusInProgress, StatusCompleted},
		{StatusInProgress, StatusFailed},
		{StatusInProgress, StatusCancelled},

		// §2.1 extended transitions
		{StatusPlanned, StatusReady},
		{StatusReady, StatusDispatched},
		{StatusReady, StatusCancelled},
		{StatusDispatched, StatusRunning},
		{StatusDispatched, StatusCancelled},
		{StatusRunning, StatusVerifyPending},
		{StatusRunning, StatusFailed},
		{StatusRunning, StatusCancelled},
		{StatusVerifyPending, StatusCompleted},
		{StatusVerifyPending, StatusRepairPending},
		{StatusRepairPending, StatusRunning},
		{StatusRepairPending, StatusPausedForReplan},
		{StatusPausedForReplan, StatusReady},
		{StatusPausedForHuman, StatusReady},

		// §2.1 wildcard: any non-terminal → paused_for_human
		{StatusPlanned, StatusPausedForHuman},
		{StatusReady, StatusPausedForHuman},
		{StatusDispatched, StatusPausedForHuman},
		{StatusRunning, StatusPausedForHuman},
		{StatusVerifyPending, StatusPausedForHuman},
		{StatusRepairPending, StatusPausedForHuman},
		{StatusPausedForReplan, StatusPausedForHuman},
		{StatusPending, StatusPausedForHuman},
		{StatusInProgress, StatusPausedForHuman},

		// §2.1 wildcard: any non-terminal → aborted
		{StatusPlanned, StatusAborted},
		{StatusReady, StatusAborted},
		{StatusDispatched, StatusAborted},
		{StatusRunning, StatusAborted},
		{StatusVerifyPending, StatusAborted},
		{StatusRepairPending, StatusAborted},
		{StatusPausedForReplan, StatusAborted},
		{StatusPausedForHuman, StatusAborted},
		{StatusPending, StatusAborted},
		{StatusInProgress, StatusAborted},
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
		// Existing invalid transitions
		{StatusCompleted, StatusPending},
		{StatusFailed, StatusPending},
		{StatusCancelled, StatusPending},
		{StatusPending, StatusCompleted},
		{StatusPending, StatusFailed},

		// Terminal states cannot transition
		{StatusAborted, StatusReady},
		{StatusAborted, StatusPausedForHuman},
		{StatusCompleted, StatusAborted},
		{StatusFailed, StatusAborted},
		{StatusCancelled, StatusAborted},
		{StatusDeadLetter, StatusAborted},

		// Invalid non-terminal transitions
		{StatusPlanned, StatusDispatched},   // must go through ready
		{StatusPlanned, StatusRunning},      // must go through ready → dispatched
		{StatusReady, StatusRunning},        // must go through dispatched
		{StatusReady, StatusCompleted},      // must go through dispatched → running → verify_pending
		{StatusDispatched, StatusCompleted}, // must go through running → verify_pending
		{StatusVerifyPending, StatusRunning},       // not allowed
		{StatusPausedForReplan, StatusRunning},      // must go through ready → dispatched
		{StatusPausedForHuman, StatusRunning},       // must go through ready → dispatched
		{StatusPausedForHuman, StatusCompleted},     // not allowed
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateTaskStateTransition(tt.from, tt.to); err == nil {
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
		{PhaseStatusPending, PhaseStatusFailed},        // fast-track stall cleanup
		{PhaseStatusAwaitingFill, PhaseStatusFilling},
		{PhaseStatusAwaitingFill, PhaseStatusTimedOut},
		{PhaseStatusAwaitingFill, PhaseStatusFailed},   // fast-track stall cleanup
		{PhaseStatusFilling, PhaseStatusActive},
		{PhaseStatusFilling, PhaseStatusAwaitingFill},
		{PhaseStatusFilling, PhaseStatusFailed},        // fast-track stall cleanup
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

func Test_isWorktreeTerminal(t *testing.T) {
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
		{WorktreeStatusCleanupFailed, false}, // can transition to cleanup_done
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := isWorktreeTerminal(tt.status); got != tt.terminal {
				t.Errorf("isWorktreeTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
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
		{IntegrationStatusPartialMerge, false},
		{IntegrationStatusPublished, true},
		{IntegrationStatusFailed, false}, // can transition to merging (retry)
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsIntegrationTerminal(tt.status); got != tt.terminal {
				t.Errorf("IsIntegrationTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
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
		{WorktreeStatusCreated, WorktreeStatusPublished},     // bulk publish
		{WorktreeStatusCreated, WorktreeStatusCleanupDone},   // cleanup
		{WorktreeStatusCreated, WorktreeStatusCleanupFailed}, // cleanup failure
		{WorktreeStatusActive, WorktreeStatusActive},      // multiple syncs
		{WorktreeStatusActive, WorktreeStatusCommitted},
		{WorktreeStatusActive, WorktreeStatusIntegrated}, // merge after sync
		{WorktreeStatusActive, WorktreeStatusConflict},
		{WorktreeStatusActive, WorktreeStatusFailed},
		{WorktreeStatusActive, WorktreeStatusPublished},     // bulk publish
		{WorktreeStatusActive, WorktreeStatusCleanupDone},   // cleanup
		{WorktreeStatusActive, WorktreeStatusCleanupFailed}, // cleanup failure
		{WorktreeStatusCommitted, WorktreeStatusActive},     // sync back
		{WorktreeStatusCommitted, WorktreeStatusCommitted},  // multiple commits
		{WorktreeStatusCommitted, WorktreeStatusIntegrated},
		{WorktreeStatusCommitted, WorktreeStatusFailed},
		{WorktreeStatusCommitted, WorktreeStatusPublished},     // bulk publish
		{WorktreeStatusCommitted, WorktreeStatusCleanupDone},   // cleanup
		{WorktreeStatusCommitted, WorktreeStatusCleanupFailed}, // cleanup failure
		{WorktreeStatusIntegrated, WorktreeStatusPublished},
		{WorktreeStatusIntegrated, WorktreeStatusActive},        // cross-phase sync
		{WorktreeStatusIntegrated, WorktreeStatusCommitted},     // cross-phase: new commit after integration without intermediate sync
		{WorktreeStatusIntegrated, WorktreeStatusFailed},
		{WorktreeStatusIntegrated, WorktreeStatusCleanupDone},   // cleanup
		{WorktreeStatusIntegrated, WorktreeStatusCleanupFailed}, // cleanup failure
		{WorktreeStatusPublished, WorktreeStatusCleanupDone},
		{WorktreeStatusPublished, WorktreeStatusCleanupFailed},
		{WorktreeStatusConflict, WorktreeStatusActive},
		{WorktreeStatusConflict, WorktreeStatusFailed},
		{WorktreeStatusConflict, WorktreeStatusPublished},     // bulk publish
		{WorktreeStatusConflict, WorktreeStatusCleanupDone},   // cleanup
		{WorktreeStatusConflict, WorktreeStatusCleanupFailed}, // cleanup failure
		{WorktreeStatusConflict, WorktreeStatusResolving},     // dispatch resolver
		{WorktreeStatusResolving, WorktreeStatusActive},       // resume-merge resets resolving workers to active
		{WorktreeStatusResolving, WorktreeStatusIntegrated},   // resolver commit success
		{WorktreeStatusResolving, WorktreeStatusConflict},     // resolver retryable failure
		{WorktreeStatusResolving, WorktreeStatusFailed},       // resolver permanent failure
		{WorktreeStatusResolving, WorktreeStatusCleanupDone},  // cleanup
		{WorktreeStatusResolving, WorktreeStatusCleanupFailed},
		{WorktreeStatusFailed, WorktreeStatusPublished},       // bulk publish
		{WorktreeStatusFailed, WorktreeStatusCleanupDone},
		{WorktreeStatusFailed, WorktreeStatusCleanupFailed},
		{WorktreeStatusCleanupFailed, WorktreeStatusCleanupDone}, // retry cleanup
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
		{WorktreeStatusCleanupDone, WorktreeStatusActive},    // terminal
		{WorktreeStatusCleanupDone, WorktreeStatusPublished}, // terminal
		{WorktreeStatusCleanupFailed, WorktreeStatusActive},  // only cleanup_done allowed
		{WorktreeStatusCreated, WorktreeStatusIntegrated},    // must go through committed first
		{WorktreeStatusPublished, WorktreeStatusActive},      // only cleanup transitions allowed
		{WorktreeStatusPublished, WorktreeStatusCommitted},   // only cleanup transitions allowed
		{WorktreeStatusActive, WorktreeStatusResolving},      // resolving only reachable from conflict
		{WorktreeStatusCommitted, WorktreeStatusResolving},   // resolving only reachable from conflict
		{WorktreeStatusResolving, WorktreeStatusCommitted},   // no direct return to committed
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
		{IntegrationStatusMerging, IntegrationStatusPartialMerge},
		{IntegrationStatusMerging, IntegrationStatusFailed},
		{IntegrationStatusMerged, IntegrationStatusMerging},    // re-merge for next phase
		{IntegrationStatusMerged, IntegrationStatusPublishing},
		{IntegrationStatusMerged, IntegrationStatusFailed},
		{IntegrationStatusPublishing, IntegrationStatusPublished},
		{IntegrationStatusPublishing, IntegrationStatusConflict},
		{IntegrationStatusPublishing, IntegrationStatusFailed},
		{IntegrationStatusPartialMerge, IntegrationStatusMerging}, // retry
		{IntegrationStatusPartialMerge, IntegrationStatusFailed},
		{IntegrationStatusConflict, IntegrationStatusMerging},
		{IntegrationStatusConflict, IntegrationStatusFailed},
		{IntegrationStatusFailed, IntegrationStatusMerging}, // retry after failure
		{IntegrationStatusFailed, IntegrationStatusFailed},  // repeated failures
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
		{IntegrationStatusPublished, IntegrationStatusMerging},   // terminal
		{IntegrationStatusPublished, IntegrationStatusFailed},    // terminal
		{IntegrationStatusCreated, IntegrationStatusMerged},      // must go through merging
		{IntegrationStatusCreated, IntegrationStatusPublished},   // must go through merging→merged→publishing
		{IntegrationStatusMerging, IntegrationStatusPublishing},     // must go through merged
		{IntegrationStatusPartialMerge, IntegrationStatusPublishing}, // partial_merge cannot publish directly
		{IntegrationStatusPartialMerge, IntegrationStatusCreated},    // invalid backward
		{IntegrationStatusFailed, IntegrationStatusPublishing},       // can only retry to merging
	}
	for _, tt := range invalid {
		t.Run("invalid_"+string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			if err := ValidateIntegrationTransition(tt.from, tt.to); err == nil {
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
