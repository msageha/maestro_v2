//go:build integration

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// readPlannerSignals loads .maestro/queue/planner_signals.yaml for test assertions.
func readPlannerSignals(t *testing.T, d *Daemon) model.PlannerSignalQueue {
	t.Helper()
	path := filepath.Join(d.maestroDir, "queue", "planner_signals.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return model.PlannerSignalQueue{}
	}
	var sq model.PlannerSignalQueue
	if err := yamlv3.Unmarshal(data, &sq); err != nil {
		t.Fatalf("parse planner_signals.yaml: %v", err)
	}
	return sq
}

// setupPhasedCommandState creates a phased command state with research (completed)
// and implementation (target status) phases. Returns the commandID.
func setupPhasedCommandState(t *testing.T, d *Daemon, commandID, researchPhaseID, implPhaseID string, implStatus model.PhaseStatus) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       commandID,
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: []string{"t1-research"},
		TaskStates:      map[string]model.Status{"t1-research": model.StatusCompleted},
		Phases: []model.Phase{
			{
				PhaseID: researchPhaseID,
				Name:    "research",
				Type:    "concrete",
				Status:  model.PhaseStatusCompleted,
				TaskIDs: []string{"t1-research"},
			},
			{
				PhaseID:         implPhaseID,
				Name:            "implementation",
				Type:            "deferred",
				Status:          implStatus,
				DependsOnPhases: []string{"research"},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	path := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write command state: %v", err)
	}
}

// Scenario S1: Phase transition to awaiting_fill creates a signal in the queue.
func TestSignal_AwaitingFillCreatesSignal(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_sig_0001_aabbcc01"
	researchPhaseID := "phase-research-001"
	implPhaseID := "phase-impl-001"

	// Setup: research completed, implementation pending (depends on research)
	setupPhasedCommandState(t, d, commandID, researchPhaseID, implPhaseID, model.PhaseStatusPending)

	// Setup: command in planner queue (in_progress)
	now := time.Now().UTC().Format(time.RFC3339)
	plannerOwner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:         commandID,
				Content:    "multi-phase command",
				Status:     model.StatusInProgress,
				LeaseOwner: &plannerOwner,
				LeaseEpoch: 1,
				Attempts:   1,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	// Mock executor that fails delivery (simulates busy planner)
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{
			Success: false,
			Error:   fmt.Errorf("busy_timeout"),
		}}, nil
	})

	// Run periodic scan — should detect pending→awaiting_fill transition and create signal
	d.handler.PeriodicScan()

	// Verify signal was created
	sq := readPlannerSignals(t, d)
	if len(sq.Signals) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(sq.Signals))
	}

	sig := sq.Signals[0]
	if sig.Kind != "awaiting_fill" {
		t.Errorf("signal kind = %q, want awaiting_fill", sig.Kind)
	}
	if sig.CommandID != commandID {
		t.Errorf("signal command_id = %q, want %s", sig.CommandID, commandID)
	}
	if sig.PhaseID != implPhaseID {
		t.Errorf("signal phase_id = %q, want %s", sig.PhaseID, implPhaseID)
	}
	if sig.Attempts != 1 {
		t.Errorf("signal attempts = %d, want 1 (first delivery attempted)", sig.Attempts)
	}
	if sig.LastError == nil || *sig.LastError == "" {
		t.Error("expected last_error to be set after failed delivery")
	}
}

// Scenario S2: Successful signal delivery removes the signal from queue.
func TestSignal_SuccessfulDeliveryRemovesSignal(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_sig_0002_aabbcc02"
	implPhaseID := "phase-impl-002"

	// Setup: command state with implementation in awaiting_fill
	setupPhasedCommandState(t, d, commandID, "phase-research-002", implPhaseID, model.PhaseStatusAwaitingFill)

	// Setup: command in planner queue
	now := time.Now().UTC().Format(time.RFC3339)
	plannerOwner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:         commandID,
				Content:    "multi-phase command",
				Status:     model.StatusInProgress,
				LeaseOwner: &plannerOwner,
				LeaseEpoch: 1,
				Attempts:   1,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	// Pre-populate a signal that needs delivery (as if from a previous scan)
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "awaiting_fill",
				CommandID: commandID,
				PhaseID:   implPhaseID,
				PhaseName: "implementation",
				Message:   "awaiting_fill notification",
				Attempts:  0,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner_signals.yaml"), sq)

	// Mock executor: delivery succeeds
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	})

	// Run scan — signal should be delivered and removed
	d.handler.PeriodicScan()

	// Verify signal queue file removed (empty queue → file deleted)
	signalPath := filepath.Join(d.maestroDir, "queue", "planner_signals.yaml")
	if _, err := os.Stat(signalPath); !os.IsNotExist(err) {
		sq2 := readPlannerSignals(t, d)
		t.Fatalf("expected signal file removed after successful delivery, got %d signals", len(sq2.Signals))
	}
}

// Scenario S3: Failed delivery sets backoff and retains signal.
func TestSignal_FailedDeliveryBackoff(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_sig_0003_aabbcc03"
	implPhaseID := "phase-impl-003"

	// Setup: awaiting_fill phase
	setupPhasedCommandState(t, d, commandID, "phase-research-003", implPhaseID, model.PhaseStatusAwaitingFill)

	// Setup: command in planner queue
	now := time.Now().UTC().Format(time.RFC3339)
	plannerOwner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:         commandID,
				Content:    "multi-phase command",
				Status:     model.StatusInProgress,
				LeaseOwner: &plannerOwner,
				LeaseEpoch: 1,
				Attempts:   1,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	// Pre-populate signal
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "awaiting_fill",
				CommandID: commandID,
				PhaseID:   implPhaseID,
				PhaseName: "implementation",
				Message:   "awaiting_fill notification",
				Attempts:  0,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner_signals.yaml"), sq)

	// Mock executor: delivery fails
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{
			Success: false,
			Error:   fmt.Errorf("busy_timeout"),
		}}, nil
	})

	// Run scan
	d.handler.PeriodicScan()

	// Verify signal retained with backoff
	sq2 := readPlannerSignals(t, d)
	if len(sq2.Signals) != 1 {
		t.Fatalf("expected 1 signal retained, got %d", len(sq2.Signals))
	}

	sig := sq2.Signals[0]
	if sig.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", sig.Attempts)
	}
	if sig.NextAttemptAt == nil {
		t.Fatal("expected next_attempt_at to be set after failure")
	}
	if sig.LastError == nil {
		t.Fatal("expected last_error to be set")
	}

	// Verify next_attempt_at is in the future
	nextAt, err := time.Parse(time.RFC3339, *sig.NextAttemptAt)
	if err != nil {
		t.Fatalf("parse next_attempt_at: %v", err)
	}
	if !nextAt.After(time.Now().UTC()) {
		t.Errorf("next_attempt_at should be in the future, got %s", *sig.NextAttemptAt)
	}
}

// Scenario S4: Signal skipped when next_attempt_at is in the future.
func TestSignal_BackoffSkipsDelivery(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_sig_0004_aabbcc04"
	implPhaseID := "phase-impl-004"

	// Setup: awaiting_fill phase
	setupPhasedCommandState(t, d, commandID, "phase-research-004", implPhaseID, model.PhaseStatusAwaitingFill)

	// Setup: command in planner queue
	now := time.Now().UTC().Format(time.RFC3339)
	plannerOwner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:         commandID,
				Content:    "multi-phase command",
				Status:     model.StatusInProgress,
				LeaseOwner: &plannerOwner,
				LeaseEpoch: 1,
				Attempts:   1,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	// Pre-populate signal with future next_attempt_at
	futureTime := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	errStr := "previous error"
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:          "awaiting_fill",
				CommandID:     commandID,
				PhaseID:       implPhaseID,
				PhaseName:     "implementation",
				Message:       "awaiting_fill notification",
				Attempts:      2,
				CreatedAt:     now,
				UpdatedAt:     now,
				NextAttemptAt: &futureTime,
				LastError:     &errStr,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner_signals.yaml"), sq)

	// Track if executor is called
	called := false
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		called = true
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	})

	// Run scan — signal should be skipped (backoff not elapsed)
	d.handler.PeriodicScan()

	// Verify signal still retained with same attempts count
	sq2 := readPlannerSignals(t, d)
	if len(sq2.Signals) != 1 {
		t.Fatalf("expected 1 signal retained, got %d", len(sq2.Signals))
	}
	if sq2.Signals[0].Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (unchanged)", sq2.Signals[0].Attempts)
	}

	// Note: executor may be called for normal command dispatch (not signal delivery).
	// The key assertion is that signal attempts didn't increment.
	_ = called
}

// Scenario S5: Stale signal removed when phase leaves awaiting_fill.
func TestSignal_StaleSignalRemovedOnPhaseAdvance(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_sig_0005_aabbcc05"
	implPhaseID := "phase-impl-005"

	// Setup: phase has moved past awaiting_fill to filling
	setupPhasedCommandState(t, d, commandID, "phase-research-005", implPhaseID, model.PhaseStatusFilling)

	// Setup: command in planner queue
	now := time.Now().UTC().Format(time.RFC3339)
	plannerOwner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:         commandID,
				Content:    "multi-phase command",
				Status:     model.StatusInProgress,
				LeaseOwner: &plannerOwner,
				LeaseEpoch: 1,
				Attempts:   1,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	// Pre-populate a stale awaiting_fill signal
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "awaiting_fill",
				CommandID: commandID,
				PhaseID:   implPhaseID,
				PhaseName: "implementation",
				Message:   "awaiting_fill notification",
				Attempts:  1,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner_signals.yaml"), sq)

	// Run scan — stale signal should be removed
	d.handler.PeriodicScan()

	// Verify signal file removed
	signalPath := filepath.Join(d.maestroDir, "queue", "planner_signals.yaml")
	if _, err := os.Stat(signalPath); !os.IsNotExist(err) {
		sq2 := readPlannerSignals(t, d)
		t.Fatalf("expected signal file removed (stale signal), got %d signals", len(sq2.Signals))
	}
}

// Scenario S6: Orphaned signal removed when command state not found.
func TestSignal_OrphanedSignalRemovedOnMissingState(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_sig_0006_nonexistent"

	// No command state file exists for this command ID

	// Setup: command in planner queue (to avoid empty scan)
	now := time.Now().UTC().Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	// Pre-populate orphaned signal
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "awaiting_fill",
				CommandID: commandID,
				PhaseID:   "phase-ghost",
				PhaseName: "ghost",
				Message:   "orphaned signal",
				Attempts:  0,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner_signals.yaml"), sq)

	// Run scan — orphaned signal should be removed
	d.handler.PeriodicScan()

	// Verify signal file removed
	signalPath := filepath.Join(d.maestroDir, "queue", "planner_signals.yaml")
	if _, err := os.Stat(signalPath); !os.IsNotExist(err) {
		sq2 := readPlannerSignals(t, d)
		t.Fatalf("expected orphaned signal removed, got %d signals", len(sq2.Signals))
	}
}

// Scenario S7: Signal deduplication — same (kind, command_id, phase_id) not duplicated.
func TestSignal_Deduplication(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_sig_0007_aabbcc07"
	implPhaseID := "phase-impl-007"

	// Setup: pending→awaiting_fill transition will happen
	setupPhasedCommandState(t, d, commandID, "phase-research-007", implPhaseID, model.PhaseStatusPending)

	// Setup: command in planner queue
	now := time.Now().UTC().Format(time.RFC3339)
	plannerOwner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:         commandID,
				Content:    "multi-phase command",
				Status:     model.StatusInProgress,
				LeaseOwner: &plannerOwner,
				LeaseEpoch: 1,
				Attempts:   1,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	// Pre-populate existing signal for the same phase
	futureTime := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:          "awaiting_fill",
				CommandID:     commandID,
				PhaseID:       implPhaseID,
				PhaseName:     "implementation",
				Message:       "existing signal",
				Attempts:      3,
				CreatedAt:     now,
				UpdatedAt:     now,
				NextAttemptAt: &futureTime,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner_signals.yaml"), sq)

	// Mock executor: succeeds for normal dispatch, fails for signal
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	})

	// Run scan — transition should upsert, but dedup should prevent duplicate
	d.handler.PeriodicScan()

	sq2 := readPlannerSignals(t, d)
	// Count awaiting_fill signals for this command+phase
	count := 0
	for _, sig := range sq2.Signals {
		if sig.Kind == "awaiting_fill" && sig.CommandID == commandID && sig.PhaseID == implPhaseID {
			count++
		}
	}
	if count > 1 {
		t.Errorf("expected at most 1 signal for same (kind, command, phase), got %d", count)
	}
}

// Scenario S8: computeSignalBackoff produces expected values.
func TestSignal_ComputeBackoff(t *testing.T) {
	d := newIntegrationDaemon(t)

	tests := []struct {
		attempts int
		wantSec  int
	}{
		{1, 5},   // 5 * 2^0 = 5
		{2, 10},  // 5 * 2^1 = 10
		{3, 20},  // 5 * 2^2 = 20
		{4, 40},  // 5 * 2^3 = 40
		{5, 60},  // 5 * 2^4 = 80, capped at scan_interval=60
		{10, 60}, // capped
	}

	for _, tt := range tests {
		got := d.handler.computeSignalBackoff(tt.attempts)
		want := time.Duration(tt.wantSec) * time.Second
		if got != want {
			t.Errorf("computeSignalBackoff(%d) = %v, want %v", tt.attempts, got, want)
		}
	}
}

// Scenario S9: fill_timeout signal created for timed_out phase transition.
func TestSignal_FillTimeoutCreatesSignal(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_sig_0009_aabbcc09"
	implPhaseID := "phase-impl-009"

	// Setup: awaiting_fill phase with past deadline → will transition to timed_out
	pastDeadline := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	state := model.CommandState{
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       commandID,
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: []string{"t1-research"},
		TaskStates:      map[string]model.Status{"t1-research": model.StatusCompleted},
		Phases: []model.Phase{
			{
				PhaseID: "phase-research-009",
				Name:    "research",
				Type:    "concrete",
				Status:  model.PhaseStatusCompleted,
				TaskIDs: []string{"t1-research"},
			},
			{
				PhaseID:         implPhaseID,
				Name:            "implementation",
				Type:            "deferred",
				Status:          model.PhaseStatusAwaitingFill,
				DependsOnPhases: []string{"research"},
				FillDeadlineAt:  &pastDeadline,
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml"), state)

	// Setup: command in planner queue
	plannerOwner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:         commandID,
				Content:    "multi-phase command",
				Status:     model.StatusInProgress,
				LeaseOwner: &plannerOwner,
				LeaseEpoch: 1,
				Attempts:   1,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	// Mock executor: delivery fails (to keep signal in queue for inspection)
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{
			Success: false,
			Error:   fmt.Errorf("busy_timeout"),
		}}, nil
	})

	// Run scan
	d.handler.PeriodicScan()

	// Verify fill_timeout signal was created
	sq := readPlannerSignals(t, d)
	found := false
	for _, sig := range sq.Signals {
		if sig.Kind == "fill_timeout" && sig.CommandID == commandID && sig.PhaseID == implPhaseID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected fill_timeout signal for command %s phase %s, signals: %+v",
			commandID, implPhaseID, sq.Signals)
	}
}
