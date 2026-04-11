package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil/mocks"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// --- R3 planner queue tests ---

func TestR3PlannerQueue_NoResultFile(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR3PlannerQueue_NoTerminalResults(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusInProgress, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for non-terminal results, got %d", len(outcome.Repairs))
	}
}

func TestR3PlannerQueue_HappyPath_RepairsNonTerminalCommand(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	// Planner result is terminal
	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	// Queue still shows in_progress
	owner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd1", Status: model.StatusInProgress, LeaseOwner: &owner, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	// State file for UpdateLastReconciledAt
	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != PatternR3 {
		t.Errorf("pattern: got %s, want R3", outcome.Repairs[0].Pattern)
	}

	// Verify queue updated
	data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "planner.yaml"))
	var updated model.CommandQueue
	yamlv3.Unmarshal(data, &updated)
	if updated.Commands[0].Status != model.StatusCompleted {
		t.Errorf("queue status: got %s, want completed", updated.Commands[0].Status)
	}
	if updated.Commands[0].LeaseOwner != nil {
		t.Error("lease_owner should be nil after repair")
	}
	if updated.Commands[0].LeaseExpiresAt != nil {
		t.Error("lease_expires_at should be nil after repair")
	}
}

func TestR3PlannerQueue_Idempotent(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd1", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// First run
	run1 := newRun(&deps)
	outcome1 := R3PlannerQueue{}.Apply(run1)
	if len(outcome1.Repairs) != 1 {
		t.Fatalf("first run: expected 1 repair, got %d", len(outcome1.Repairs))
	}

	// Second run — queue already terminal
	run2 := newRun(&deps)
	outcome2 := R3PlannerQueue{}.Apply(run2)
	if len(outcome2.Repairs) != 0 {
		t.Errorf("second run: expected 0 repairs (idempotent), got %d", len(outcome2.Repairs))
	}
}

func TestR3PlannerQueue_AlreadyTerminalQueue_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd1", Status: model.StatusCompleted, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for already terminal queue, got %d", len(outcome.Repairs))
	}
}

func TestR3PlannerQueue_FailedResult(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusFailed, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd1", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}

	data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "planner.yaml"))
	var updated model.CommandQueue
	yamlv3.Unmarshal(data, &updated)
	if updated.Commands[0].Status != model.StatusFailed {
		t.Errorf("queue status: got %s, want failed", updated.Commands[0].Status)
	}
}

func TestR3PlannerQueue_MultipleCommands(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
			{ID: "res2", CommandID: "cmd2", Status: model.StatusFailed, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd1", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
			{ID: "cmd2", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
			{ID: "cmd3", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now}, // no result
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	state1 := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state1)
	state2 := model.CommandState{CommandID: "cmd2", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd2.yaml"), state2)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 2 {
		t.Errorf("expected 2 repairs, got %d", len(outcome.Repairs))
	}
}

func TestR3PlannerQueue_NoQueueFile_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	os.Remove(filepath.Join(maestroDir, "queue", "planner.yaml"))

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when queue file missing, got %d", len(outcome.Repairs))
	}
}

// --- R4 plan status tests ---

func TestR4PlanStatus_CanCompleteNil_Skipped(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R4PlanStatus{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when CanComplete is nil, got %d", len(outcome.Repairs))
	}
}

func TestR4PlanStatus_AlreadyTerminal_Skipped(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.CanComplete = func(*model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCompleted, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusCompleted, // Already terminal
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R4PlanStatus{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for already terminal state, got %d", len(outcome.Repairs))
	}
}

func TestR4PlanStatus_CanCompleteSuccess(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.CanComplete = func(*model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCompleted, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R4PlanStatus{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected no notifications on success, got %d", len(outcome.Notifications))
	}

	// Verify state updated
	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("plan_status: got %s, want completed", updated.PlanStatus)
	}
}

func TestR4PlanStatus_CanCompleteFails_QuarantineAndNotify(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.CanComplete = func(*model.CommandState) (model.PlanStatus, error) {
		return "", fmt.Errorf("tasks incomplete")
	}
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R4PlanStatus{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(outcome.Notifications))
	}
	if outcome.Notifications[0].Kind != NotifyReEvaluate {
		t.Errorf("notification kind: got %s", outcome.Notifications[0].Kind)
	}

	// Verify quarantine file created
	entries, _ := os.ReadDir(filepath.Join(maestroDir, "quarantine"))
	if len(entries) != 1 {
		t.Errorf("expected 1 quarantine file, got %d", len(entries))
	}

	// Verify result removed from planner.yaml
	data, _ := os.ReadFile(filepath.Join(maestroDir, "results", "planner.yaml"))
	var updatedRF model.CommandResultFile
	yamlv3.Unmarshal(data, &updatedRF)
	if len(updatedRF.Results) != 0 {
		t.Errorf("expected 0 results after quarantine, got %d", len(updatedRF.Results))
	}
}

func TestR4PlanStatus_StateNotFound_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.CanComplete = func(*model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCompleted, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd_no_state", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	run := newRun(&deps)
	outcome := R4PlanStatus{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when state file missing, got %d", len(outcome.Repairs))
	}
}

// --- R5 notification tests ---

func TestR5Notification_NilResultHandler(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	// ResultHandler is nil
	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when ResultHandler is nil, got %d", len(outcome.Repairs))
	}
}

func TestR5Notification_NotNotified_Ignored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.ResultHandler = &mockResultNotifier{}
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for non-notified result, got %d", len(outcome.Repairs))
	}
}

func TestR5Notification_WriteError(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{err: fmt.Errorf("write failed")}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now}, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), model.NotificationQueue{})

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	// Should not produce repairs on write error
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs on write error, got %d", len(outcome.Repairs))
	}
	if len(notifier.calls) != 1 {
		t.Errorf("expected 1 write attempt, got %d", len(notifier.calls))
	}
}

func TestR5Notification_HappyPath_ReissuesNotification(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	// Result is terminal, notified, but no orchestrator notification exists
	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now}, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	// Empty orchestrator queue — no matching source_result_id
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), model.NotificationQueue{})

	// State file for UpdateLastReconciledAt
	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != PatternR5 {
		t.Errorf("pattern: got %s, want R5", outcome.Repairs[0].Pattern)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notification call, got %d", len(notifier.calls))
	}
	if notifier.calls[0].resultID != "res1" {
		t.Errorf("resultID: got %s, want res1", notifier.calls[0].resultID)
	}
	if notifier.calls[0].commandID != "cmd1" {
		t.Errorf("commandID: got %s, want cmd1", notifier.calls[0].commandID)
	}
	if notifier.calls[0].status != model.StatusCompleted {
		t.Errorf("status: got %s, want completed", notifier.calls[0].status)
	}
}

func TestR5Notification_AlreadyInOrchestratorQueue_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now}, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	// Orchestrator queue already has matching notification
	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{ID: "ntf1", CommandID: "cmd1", Type: model.NotificationTypeCommandCompleted, SourceResultID: "res1", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), nq)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when notification already exists, got %d", len(outcome.Repairs))
	}
	if len(notifier.calls) != 0 {
		t.Errorf("expected no notification calls, got %d", len(notifier.calls))
	}
}

func TestR5Notification_Idempotent(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now}, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), model.NotificationQueue{})

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// First run — produces repair
	run1 := newRun(&deps)
	outcome1 := R5Notification{}.Apply(run1)
	if len(outcome1.Repairs) != 1 {
		t.Fatalf("first run: expected 1 repair, got %d", len(outcome1.Repairs))
	}

	// The mockResultNotifier doesn't actually write to orchestrator.yaml,
	// so calling again would still produce a repair (which is correct behavior —
	// R5 re-issues until the notification actually appears in the queue).
	// This verifies the mock was called twice.
	run2 := newRun(&deps)
	outcome2 := R5Notification{}.Apply(run2)
	if len(outcome2.Repairs) != 1 {
		t.Errorf("second run: expected 1 repair (mock doesn't persist), got %d", len(outcome2.Repairs))
	}
	if len(notifier.calls) != 2 {
		t.Errorf("expected 2 total notification calls, got %d", len(notifier.calls))
	}
}

func TestR5Notification_NonTerminalResult_Ignored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusInProgress, NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now}, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), model.NotificationQueue{})

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for non-terminal result, got %d", len(outcome.Repairs))
	}
}

func TestR5Notification_NoOrchestratorQueue_StillRepairs(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now}, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	// No orchestrator.yaml file

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Errorf("expected 1 repair when orchestrator queue missing, got %d", len(outcome.Repairs))
	}
}

func TestR5Notification_MultipleResults(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now}, CreatedAt: now},
			{ID: "res2", CommandID: "cmd2", Status: model.StatusFailed, NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now}, CreatedAt: now},
			{ID: "res3", CommandID: "cmd3", Status: model.StatusCompleted, CreatedAt: now}, // not notified
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), model.NotificationQueue{})

	state1 := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state1)
	state2 := model.CommandState{CommandID: "cmd2", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd2.yaml"), state2)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	// Only res1 and res2 should be repaired (res3 is not notified)
	if len(outcome.Repairs) != 2 {
		t.Errorf("expected 2 repairs, got %d", len(outcome.Repairs))
	}
	if len(notifier.calls) != 2 {
		t.Errorf("expected 2 notification calls, got %d", len(notifier.calls))
	}
}

// TestR5Notification_TypeMismatch_ReissuesForSupersede covers the H3-driven case
// where a result's terminal status was promoted (e.g. completed → cancelled, or
// → failed) after a notification with the previous type was already enqueued.
// The dedup key is (source_result_id, type), so R5 must re-issue the notification
// for the new type instead of dropping it.
func TestR5Notification_TypeMismatch_ReissuesForSupersede(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	// Result has been promoted to failed (e.g. by H3 cancelled-on-failure path).
	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusFailed, NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now}, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	// Existing notification still has the previous type (command_completed).
	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{ID: "ntf1", CommandID: "cmd1", Type: model.NotificationTypeCommandCompleted, SourceResultID: "res1", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), nq)

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair when type mismatches existing notification, got %d", len(outcome.Repairs))
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notification call, got %d", len(notifier.calls))
	}
	if notifier.calls[0].status != model.StatusFailed {
		t.Errorf("expected notifier called with failed status, got %s", notifier.calls[0].status)
	}
	if notifier.calls[0].resultID != "res1" {
		t.Errorf("expected resultID res1, got %s", notifier.calls[0].resultID)
	}
}

// --- R6 fill timeout tests ---

func TestR6FillTimeout_NoPhases_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR6FillTimeout_NoDeadline_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusAwaitingFill},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs without deadline, got %d", len(outcome.Repairs))
	}
}

func TestR6FillTimeout_ActivePhase_Ignored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	pastDeadline := now.Add(-1 * time.Hour).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusActive, FillDeadlineAt: &pastDeadline},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for active phase, got %d", len(outcome.Repairs))
	}
}

func TestR6FillTimeout_MultipleTimedOutPhases(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	}

	pastDeadline := now.Add(-1 * time.Hour).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 2 {
		t.Fatalf("expected 2 repairs, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(outcome.Notifications))
	}
}

func TestR6FillTimeout_NoExecutorFactory_NoNotification(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	pastDeadline := now.Add(-1 * time.Hour).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected no notifications without executor factory, got %d", len(outcome.Notifications))
	}
}

// --- R1 RetryEnqueueFailed tests ---

func TestR1ResultQueue_RetryEnqueueFailed_AlreadyInQueue(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	// State file with RetryEnqueueFailed entry
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		RetryEnqueueFailed: map[string]string{
			"retry_task1": "worker1",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// Queue already has the retry task
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{ID: "retry_task1", CommandID: "cmd1", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq)

	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)

	// Should produce 1 repair (cleared entry)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Detail != "retry_enqueue_failed cleared (task already in queue)" {
		t.Errorf("unexpected detail: %s", outcome.Repairs[0].Detail)
	}

	// Verify state file: RetryEnqueueFailed entry should be removed
	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if len(updated.RetryEnqueueFailed) != 0 {
		t.Errorf("expected RetryEnqueueFailed to be empty, got %v", updated.RetryEnqueueFailed)
	}
	if updated.LastReconciledAt == nil {
		t.Error("expected LastReconciledAt to be set")
	}
}

func TestR1ResultQueue_RetryEnqueueFailed_MaxAttemptsExceeded(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	// State with retry count >= maxRetryEnqueueAttempts (3)
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		RetryEnqueueFailed: map[string]string{
			"retry_task1": formatRetryEnqueueValue("worker1", 3),
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// Empty queue (task not present)
	tq := model.TaskQueue{SchemaVersion: 1, FileType: "queue_task"}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq)

	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Detail != fmt.Sprintf("retry_enqueue_failed max attempts (%d) exceeded, marked failed", 3) {
		t.Errorf("unexpected detail: %s", outcome.Repairs[0].Detail)
	}

	// Verify state: entry removed, task marked failed
	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if len(updated.RetryEnqueueFailed) != 0 {
		t.Errorf("expected RetryEnqueueFailed empty, got %v", updated.RetryEnqueueFailed)
	}
	if updated.TaskStates["retry_task1"] != model.StatusFailed {
		t.Errorf("expected task status failed, got %s", updated.TaskStates["retry_task1"])
	}
}

func TestR1ResultQueue_RetryEnqueueFailed_OriginalTaskNotFound(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	// State with retry entry (count 0, below max)
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		RetryEnqueueFailed: map[string]string{
			"retry_task1": "worker1",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// Queue has same-command task but non-terminal (pending), so r1FindOriginalTask returns nil
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{ID: "task_pending", CommandID: "cmd1", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq)

	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Detail != "retry_enqueue_failed original task not found, marked failed" {
		t.Errorf("unexpected detail: %s", outcome.Repairs[0].Detail)
	}

	// Verify state: entry removed, task marked failed
	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if len(updated.RetryEnqueueFailed) != 0 {
		t.Errorf("expected RetryEnqueueFailed empty, got %v", updated.RetryEnqueueFailed)
	}
	if updated.TaskStates["retry_task1"] != model.StatusFailed {
		t.Errorf("expected task status failed, got %s", updated.TaskStates["retry_task1"])
	}
}

func TestR1ResultQueue_RetryEnqueueFailed_ReenqueueSuccess(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	fixedTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	deps := newTestDeps(t, maestroDir)
	setClock(&deps, fixedTime)
	now := fixedTime.Format(time.RFC3339)

	// State with retry entry
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		RetryEnqueueFailed: map[string]string{
			"retry_task1": "worker1",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// Queue with a terminal (completed) original task for cmd1, but NOT retry_task1
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:        "original_task1",
				CommandID: "cmd1",
				Status:    model.StatusCompleted,
				Content:   "do something",
				Purpose:   "test purpose",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq)

	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Detail != "retry_enqueue_failed re-enqueued to worker1" {
		t.Errorf("unexpected detail: %s", outcome.Repairs[0].Detail)
	}

	// Verify state: entry removed
	stateData, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"))
	var updatedState model.CommandState
	yamlv3.Unmarshal(stateData, &updatedState)
	if len(updatedState.RetryEnqueueFailed) != 0 {
		t.Errorf("expected RetryEnqueueFailed empty, got %v", updatedState.RetryEnqueueFailed)
	}

	// Verify queue: retry task added with correct fields
	queueData, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "worker1.yaml"))
	var updatedQueue model.TaskQueue
	yamlv3.Unmarshal(queueData, &updatedQueue)
	if len(updatedQueue.Tasks) != 2 {
		t.Fatalf("expected 2 tasks in queue, got %d", len(updatedQueue.Tasks))
	}
	retryTask := updatedQueue.Tasks[1]
	if retryTask.ID != "retry_task1" {
		t.Errorf("expected retry task ID retry_task1, got %s", retryTask.ID)
	}
	if retryTask.Status != model.StatusPending {
		t.Errorf("expected pending status, got %s", retryTask.Status)
	}
	if retryTask.ExecutionRetries != 1 {
		t.Errorf("expected ExecutionRetries=1, got %d", retryTask.ExecutionRetries)
	}
	if retryTask.OriginalTaskID != "original_task1" {
		t.Errorf("expected OriginalTaskID=original_task1, got %s", retryTask.OriginalTaskID)
	}
	if retryTask.LeaseOwner != nil {
		t.Error("expected LeaseOwner nil")
	}
	if retryTask.Attempts != 0 {
		t.Errorf("expected Attempts=0, got %d", retryTask.Attempts)
	}
}

func TestR1ResultQueue_RetryEnqueueFailed_ReenqueueFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping: running as root")
	}
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	// State with retry entry at count 1 (below max)
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		RetryEnqueueFailed: map[string]string{
			"retry_task1": formatRetryEnqueueValue("worker1", 1),
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// Queue with a terminal original task for cmd1 (so r1FindOriginalTask succeeds)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:        "original_task1",
				CommandID: "cmd1",
				Status:    model.StatusCompleted,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq)

	// Make queue directory read-only so AtomicWrite (CreateTemp) fails
	queueDir := filepath.Join(maestroDir, "queue")
	os.Chmod(queueDir, 0555)
	defer os.Chmod(queueDir, 0755)

	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)

	// Re-enqueue fails → no repair emitted (only logged), but state is updated
	// The code increments retry count and continues; no Repair is appended for failures
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected 0 repairs (re-enqueue failure doesn't emit repair), got %d", len(outcome.Repairs))
	}

	// Restore permissions to read state file
	os.Chmod(queueDir, 0755)

	// Verify state: entry kept with incremented count
	stateData, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"))
	var updatedState model.CommandState
	yamlv3.Unmarshal(stateData, &updatedState)
	if len(updatedState.RetryEnqueueFailed) != 1 {
		t.Fatalf("expected 1 RetryEnqueueFailed entry, got %d", len(updatedState.RetryEnqueueFailed))
	}
	value := updatedState.RetryEnqueueFailed["retry_task1"]
	wantValue := formatRetryEnqueueValue("worker1", 2)
	if value != wantValue {
		t.Errorf("expected RetryEnqueueFailed value %q, got %q", wantValue, value)
	}
}

// --- Run helper method tests ---

func TestLoadState_CorruptedYAML(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)

	path := filepath.Join(maestroDir, "state", "commands", "corrupt.yaml")
	os.WriteFile(path, []byte("plan_status: [unterminated"), 0644)

	_, err := run.loadState(path)
	if err == nil {
		t.Error("expected error for corrupted YAML")
	}
}

func TestLoadState_NonExistent(t *testing.T) {
	t.Parallel()
	deps := Deps{}
	run := newRun(&deps)
	_, err := run.loadState("/nonexistent/path.yaml")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestLoadCommandResultFile_NonExistent(t *testing.T) {
	t.Parallel()
	deps := Deps{}
	run := newRun(&deps)
	rf, err := run.loadCommandResultFile("/nonexistent/path.yaml")
	if err != nil {
		t.Fatalf("expected no error for non-existent file, got %v", err)
	}
	if len(rf.Results) != 0 {
		t.Errorf("expected empty results, got %d", len(rf.Results))
	}
}

func TestRemoveCommandFromPlannerQueue_NoQueueFile(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)

	// No planner.yaml exists — should remove queue dir to test missing file
	os.Remove(filepath.Join(maestroDir, "queue", "planner.yaml"))

	err := run.removeCommandFromPlannerQueue("cmd_nonexistent")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestRemoveCommandFromPlannerQueue_CommandNotFound(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	cq := model.CommandQueue{
		Commands: []model.Command{
			{ID: "cmd_other", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	run := newRun(&deps)
	err := run.removeCommandFromPlannerQueue("cmd_nonexistent")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}

	// Original command should still be there
	data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "planner.yaml"))
	var updated model.CommandQueue
	yamlv3.Unmarshal(data, &updated)
	if len(updated.Commands) != 1 {
		t.Errorf("expected 1 command, got %d", len(updated.Commands))
	}
}
