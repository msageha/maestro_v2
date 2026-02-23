package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

func newTestReconciler(maestroDir string) *Reconciler {
	cfg := model.Config{
		Watcher: model.WatcherConfig{DispatchLeaseSec: 60},
	}
	lockMap := lock.NewMutexMap()
	logger := log.New(&bytes.Buffer{}, "", 0)
	rh := NewResultHandler(maestroDir, cfg, lockMap, logger, LogLevelDebug)
	return NewReconciler(maestroDir, cfg, lockMap, logger, LogLevelDebug, rh, rh.executorFactory)
}

func TestReconciler_R0_PlanningStuck(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	// Create a state file stuck in "planning" for longer than threshold (dispatch_lease_sec*2 = 120s)
	oldTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_0000000001_aaaaaaaa",
		PlanStatus:    model.PlanStatusPlanning,
		CreatedAt:     oldTime,
		UpdatedAt:     oldTime,
	}
	statePath := filepath.Join(stateDir, "cmd_0000000001_aaaaaaaa.yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// Create corresponding planner queue entry
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:        "cmd_0000000001_aaaaaaaa",
				Content:   "test command",
				Status:    model.StatusPending,
				CreatedAt: oldTime,
				UpdatedAt: oldTime,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "planner.yaml"), cq)

	repairs := rec.Reconcile()

	// Should have one R0 repair
	r0 := filterRepairs(repairs, "R0")
	if len(r0) != 1 {
		t.Fatalf("expected 1 R0 repair, got %d", len(r0))
	}
	if r0[0].CommandID != "cmd_0000000001_aaaaaaaa" {
		t.Errorf("command_id: got %s", r0[0].CommandID)
	}

	// State file should be deleted
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("expected state file to be deleted")
	}

	// Planner queue entry should be removed entirely
	data, _ := os.ReadFile(filepath.Join(queueDir, "planner.yaml"))
	var updatedCQ model.CommandQueue
	yamlv3.Unmarshal(data, &updatedCQ)
	if len(updatedCQ.Commands) != 0 {
		t.Fatalf("expected 0 commands in planner queue (removed), got %d", len(updatedCQ.Commands))
	}
}

func TestReconciler_R0_PlanningRecent_NoRepair(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	// Create a state file in "planning" but recently created (within threshold)
	now := time.Now().UTC().Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_0000000001_aaaaaaaa",
		PlanStatus:    model.PlanStatusPlanning,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	statePath := filepath.Join(stateDir, "cmd_0000000001_aaaaaaaa.yaml")
	yamlutil.AtomicWrite(statePath, state)

	repairs := rec.Reconcile()

	r0 := filterRepairs(repairs, "R0")
	if len(r0) != 0 {
		t.Fatalf("expected no R0 repairs for recent planning, got %d", len(r0))
	}

	// State file should still exist
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Error("state file should not be deleted")
	}
}

func TestReconciler_R0b_FillingStuck(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	oldTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_0000000001_bbbbbbbb",
		PlanStatus:    model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID: "phase_001",
				Name:    "phase-1",
				Status:  model.PhaseStatusFilling,
				TaskIDs: []string{"task_0000000001_11111111", "task_0000000002_22222222"},
			},
		},
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	statePath := filepath.Join(stateDir, "cmd_0000000001_bbbbbbbb.yaml")
	yamlutil.AtomicWrite(statePath, state)

	// Create a worker queue with partial tasks
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:        "task_0000000001_11111111",
				CommandID: "cmd_0000000001_bbbbbbbb",
				Status:    model.StatusPending,
				CreatedAt: oldTime,
				UpdatedAt: oldTime,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "worker1.yaml"), tq)

	repairs := rec.Reconcile()

	r0b := filterRepairs(repairs, "R0b")
	if len(r0b) != 1 {
		t.Fatalf("expected 1 R0b repair, got %d", len(r0b))
	}
	if r0b[0].CommandID != "cmd_0000000001_bbbbbbbb" {
		t.Errorf("command_id: got %s", r0b[0].CommandID)
	}

	// State file should have phase reverted to awaiting_fill
	data, _ := os.ReadFile(statePath)
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)

	if updated.Phases[0].Status != model.PhaseStatusAwaitingFill {
		t.Errorf("phase status: got %s, want awaiting_fill", updated.Phases[0].Status)
	}
	if len(updated.Phases[0].TaskIDs) != 0 {
		t.Errorf("phase task_ids should be cleared, got %v", updated.Phases[0].TaskIDs)
	}
	if updated.LastReconciledAt == nil {
		t.Error("expected last_reconciled_at to be set")
	}

	// Worker queue should have task removed
	wqData, _ := os.ReadFile(filepath.Join(queueDir, "worker1.yaml"))
	var updatedTQ model.TaskQueue
	yamlv3.Unmarshal(wqData, &updatedTQ)
	if len(updatedTQ.Tasks) != 0 {
		t.Errorf("expected 0 tasks in worker queue, got %d", len(updatedTQ.Tasks))
	}
}

func TestReconciler_R1_ResultTerminal_QueueInProgress(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	// Create a result file with terminal result
	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_0000000001_aaaaaaaa",
				TaskID:    "task_0000000001_11111111",
				CommandID: "cmd_0000000001_cccccccc",
				Status:    model.StatusCompleted,
				CreatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "worker1.yaml"), rf)

	// Create a queue file where the task is still in_progress
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	owner := "daemon:1234"
	expiresAt := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             "task_0000000001_11111111",
				CommandID:      "cmd_0000000001_cccccccc",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiresAt,
				CreatedAt:      now,
				UpdatedAt:      now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "worker1.yaml"), tq)

	// Create corresponding state file for last_reconciled_at update
	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_0000000001_cccccccc",
		PlanStatus:    model.PlanStatusSealed,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	statePath := filepath.Join(stateDir, "cmd_0000000001_cccccccc.yaml")
	yamlutil.AtomicWrite(statePath, state)

	repairs := rec.Reconcile()

	r1 := filterRepairs(repairs, "R1")
	if len(r1) != 1 {
		t.Fatalf("expected 1 R1 repair, got %d", len(r1))
	}
	if r1[0].TaskID != "task_0000000001_11111111" {
		t.Errorf("task_id: got %s", r1[0].TaskID)
	}

	// Queue task should now be completed with lease cleared
	data, _ := os.ReadFile(filepath.Join(queueDir, "worker1.yaml"))
	var updatedTQ model.TaskQueue
	yamlv3.Unmarshal(data, &updatedTQ)

	if updatedTQ.Tasks[0].Status != model.StatusCompleted {
		t.Errorf("queue status: got %s, want completed", updatedTQ.Tasks[0].Status)
	}
	if updatedTQ.Tasks[0].LeaseOwner != nil {
		t.Error("lease_owner should be cleared")
	}
	if updatedTQ.Tasks[0].LeaseExpiresAt != nil {
		t.Error("lease_expires_at should be cleared")
	}

	// State file should have last_reconciled_at updated
	stateData, _ := os.ReadFile(statePath)
	var updatedState model.CommandState
	yamlv3.Unmarshal(stateData, &updatedState)
	if updatedState.LastReconciledAt == nil {
		t.Error("expected last_reconciled_at to be set on state file")
	}
}

func TestReconciler_R2_ResultTerminal_StateNonTerminal(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	// Create a result file with terminal result
	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_0000000001_aaaaaaaa",
				TaskID:    "task_0000000001_11111111",
				CommandID: "cmd_0000000001_dddddddd",
				Status:    model.StatusCompleted,
				CreatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "worker1.yaml"), rf)

	// Create a state file where the task is still in_progress
	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_0000000001_dddddddd",
		PlanStatus:    model.PlanStatusSealed,
		TaskStates: map[string]model.Status{
			"task_0000000001_11111111": model.StatusInProgress,
		},
		AppliedResultIDs: map[string]string{},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	statePath := filepath.Join(stateDir, "cmd_0000000001_dddddddd.yaml")
	yamlutil.AtomicWrite(statePath, state)

	repairs := rec.Reconcile()

	r2 := filterRepairs(repairs, "R2")
	if len(r2) != 1 {
		t.Fatalf("expected 1 R2 repair, got %d", len(r2))
	}
	if r2[0].TaskID != "task_0000000001_11111111" {
		t.Errorf("task_id: got %s", r2[0].TaskID)
	}

	// State should now have task as completed
	data, _ := os.ReadFile(statePath)
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)

	if updated.TaskStates["task_0000000001_11111111"] != model.StatusCompleted {
		t.Errorf("task_state: got %s, want completed", updated.TaskStates["task_0000000001_11111111"])
	}
	if updated.AppliedResultIDs["task_0000000001_11111111"] != "res_0000000001_aaaaaaaa" {
		t.Errorf("applied_result_id: got %s", updated.AppliedResultIDs["task_0000000001_11111111"])
	}
	if updated.LastReconciledAt == nil {
		t.Error("expected last_reconciled_at to be set")
	}
}

func TestReconciler_R2_AlreadyTerminal_NoRepair(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_0000000001_aaaaaaaa",
				TaskID:    "task_0000000001_11111111",
				CommandID: "cmd_0000000001_eeeeeeee",
				Status:    model.StatusCompleted,
				CreatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "worker1.yaml"), rf)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_0000000001_eeeeeeee",
		PlanStatus:    model.PlanStatusSealed,
		TaskStates: map[string]model.Status{
			"task_0000000001_11111111": model.StatusCompleted, // Already terminal
		},
		AppliedResultIDs: map[string]string{
			"task_0000000001_11111111": "res_0000000001_aaaaaaaa",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_0000000001_eeeeeeee.yaml"), state)

	repairs := rec.Reconcile()

	r2 := filterRepairs(repairs, "R2")
	if len(r2) != 0 {
		t.Fatalf("expected no R2 repairs for already terminal state, got %d", len(r2))
	}
}

func TestReconciler_AllPatterns_Combined(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	oldTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)

	// R0 scenario: stuck planning
	state0 := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0_stuck", PlanStatus: model.PlanStatusPlanning,
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0_stuck.yaml"), state0)

	// R0b scenario: stuck filling
	state0b := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0b_stuck", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase-1", Status: model.PhaseStatusFilling, TaskIDs: []string{"task_partial"}},
		},
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0b_stuck.yaml"), state0b)

	// R1 scenario: result terminal + queue in_progress
	rf1 := model.TaskResultFile{
		SchemaVersion: 1, FileType: "result_task",
		Results: []model.TaskResult{
			{ID: "res_r1", TaskID: "task_r1", CommandID: "cmd_r1", Status: model.StatusFailed, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "worker1.yaml"), rf1)
	tq1 := model.TaskQueue{
		SchemaVersion: 1, FileType: "queue_task",
		Tasks: []model.Task{
			{ID: "task_r1", CommandID: "cmd_r1", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "worker1.yaml"), tq1)

	// R2 scenario: result terminal + state non-terminal
	rf2 := model.TaskResultFile{
		SchemaVersion: 1, FileType: "result_task",
		Results: []model.TaskResult{
			{ID: "res_r2", TaskID: "task_r2", CommandID: "cmd_r2", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "worker2.yaml"), rf2)
	state2 := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r2", PlanStatus: model.PlanStatusSealed,
		TaskStates: map[string]model.Status{"task_r2": model.StatusInProgress},
		AppliedResultIDs: map[string]string{},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r2.yaml"), state2)

	repairs := rec.Reconcile()

	r0 := filterRepairs(repairs, "R0")
	r0b := filterRepairs(repairs, "R0b")
	r1 := filterRepairs(repairs, "R1")
	r2 := filterRepairs(repairs, "R2")

	if len(r0) != 1 {
		t.Errorf("R0: expected 1 repair, got %d", len(r0))
	}
	if len(r0b) != 1 {
		t.Errorf("R0b: expected 1 repair, got %d", len(r0b))
	}
	if len(r1) != 1 {
		t.Errorf("R1: expected 1 repair, got %d", len(r1))
	}
	if len(r2) != 1 {
		t.Errorf("R2: expected 1 repair, got %d", len(r2))
	}
}

// --- R3 Tests ---

func TestReconciler_R3_PlannerResultTerminal_QueueInProgress(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	// Create planner result file with terminal result
	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1,
		FileType:      "result_command",
		Results: []model.CommandResult{
			{
				ID:        "res_0000000001_r3r3r3r3",
				CommandID: "cmd_0000000001_r3r3r3r3",
				Status:    model.StatusCompleted,
				CreatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	// Create planner queue with in_progress entry
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	owner := "planner"
	expiresAt := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:             "cmd_0000000001_r3r3r3r3",
				Content:        "test",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiresAt,
				CreatedAt:      now,
				UpdatedAt:      now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "planner.yaml"), cq)

	// Create state file for last_reconciled_at
	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_0000000001_r3r3r3r3", PlanStatus: model.PlanStatusSealed,
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_0000000001_r3r3r3r3.yaml"), state)

	repairs := rec.Reconcile()

	r3 := filterRepairs(repairs, "R3")
	if len(r3) != 1 {
		t.Fatalf("expected 1 R3 repair, got %d", len(r3))
	}
	if r3[0].CommandID != "cmd_0000000001_r3r3r3r3" {
		t.Errorf("command_id: got %s", r3[0].CommandID)
	}

	// Queue should be updated to completed with lease cleared
	data, _ := os.ReadFile(filepath.Join(queueDir, "planner.yaml"))
	var updatedCQ model.CommandQueue
	yamlv3.Unmarshal(data, &updatedCQ)
	if updatedCQ.Commands[0].Status != model.StatusCompleted {
		t.Errorf("queue status: got %s, want completed", updatedCQ.Commands[0].Status)
	}
	if updatedCQ.Commands[0].LeaseOwner != nil {
		t.Error("lease_owner should be cleared")
	}
}

func TestReconciler_R3_QueueAlreadyTerminal_NoRepair(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r3_ok", CommandID: "cmd_r3_ok", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	cq := model.CommandQueue{
		SchemaVersion: 1, FileType: "queue_command",
		Commands: []model.Command{
			{ID: "cmd_r3_ok", Status: model.StatusCompleted, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "planner.yaml"), cq)

	repairs := rec.Reconcile()
	r3 := filterRepairs(repairs, "R3")
	if len(r3) != 0 {
		t.Fatalf("expected no R3 repairs, got %d", len(r3))
	}
}

// --- R4 Tests ---

func TestReconciler_R4_PlannerResultTerminal_StateSealed_CanComplete(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	// Wire canComplete that succeeds
	rec.SetCanComplete(func(state *model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCompleted, nil
	})

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r4_ok", CommandID: "cmd_r4_ok", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r4_ok", PlanStatus: model.PlanStatusSealed,
		CreatedAt: now, UpdatedAt: now,
	}
	statePath := filepath.Join(stateDir, "cmd_r4_ok.yaml")
	yamlutil.AtomicWrite(statePath, state)

	repairs := rec.Reconcile()
	r4 := filterRepairs(repairs, "R4")
	if len(r4) != 1 {
		t.Fatalf("expected 1 R4 repair, got %d", len(r4))
	}

	// State should be updated to completed
	data, _ := os.ReadFile(statePath)
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("plan_status: got %s, want completed", updated.PlanStatus)
	}
	if updated.LastReconciledAt == nil {
		t.Error("expected last_reconciled_at to be set")
	}
}

func TestReconciler_R4_CanCompleteFails_Quarantine(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	// Wire canComplete that fails
	rec.SetCanComplete(func(state *model.CommandState) (model.PlanStatus, error) {
		return "", fmt.Errorf("tasks not terminal")
	})

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r4_fail", CommandID: "cmd_r4_fail", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r4_fail", PlanStatus: model.PlanStatusSealed,
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r4_fail.yaml"), state)

	// Create quarantine dir
	os.MkdirAll(filepath.Join(maestroDir, "quarantine"), 0755)

	repairs := rec.Reconcile()
	r4 := filterRepairs(repairs, "R4")
	if len(r4) != 1 {
		t.Fatalf("expected 1 R4 repair, got %d", len(r4))
	}
	if r4[0].Detail == "" || !strings.Contains(r4[0].Detail, "quarantined") {
		t.Errorf("detail should mention quarantine, got: %s", r4[0].Detail)
	}

	// Result should be removed from planner.yaml
	data, _ := os.ReadFile(filepath.Join(resultsDir, "planner.yaml"))
	var updatedRF model.CommandResultFile
	yamlv3.Unmarshal(data, &updatedRF)
	if len(updatedRF.Results) != 0 {
		t.Errorf("expected 0 results after quarantine, got %d", len(updatedRF.Results))
	}

	// Quarantine file should exist
	qEntries, _ := os.ReadDir(filepath.Join(maestroDir, "quarantine"))
	if len(qEntries) != 1 {
		t.Errorf("expected 1 quarantine file, got %d", len(qEntries))
	}
}

func TestReconciler_R4_PlanStatusPlanning_Skip(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetCanComplete(func(state *model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCompleted, nil
	})

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r4_p", CommandID: "cmd_r4_planning", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	// State in "planning" — R0's domain, not R4
	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r4_planning", PlanStatus: model.PlanStatusPlanning,
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r4_planning.yaml"), state)

	repairs := rec.Reconcile()
	r4 := filterRepairs(repairs, "R4")
	if len(r4) != 0 {
		t.Fatalf("expected no R4 repairs for planning state, got %d", len(r4))
	}
}

// --- R5 Tests ---

func TestReconciler_R5_NotifiedResult_NoOrchestratorNotification(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	// Create planner result with notified=true
	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{
				ID:        "res_r5_missing",
				CommandID: "cmd_r5_test",
				Status:    model.StatusCompleted,
				Notified:  true,
				NotifiedAt: &now,
				CreatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	// Empty orchestrator queue (no matching notification)
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	nq := model.NotificationQueue{SchemaVersion: 1, FileType: "queue_notification"}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "orchestrator.yaml"), nq)

	// State file for last_reconciled_at
	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r5_test", PlanStatus: model.PlanStatusCompleted,
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r5_test.yaml"), state)

	repairs := rec.Reconcile()
	r5 := filterRepairs(repairs, "R5")
	if len(r5) != 1 {
		t.Fatalf("expected 1 R5 repair, got %d", len(r5))
	}

	// Orchestrator queue should now have a notification
	data, _ := os.ReadFile(filepath.Join(queueDir, "orchestrator.yaml"))
	var updatedNQ model.NotificationQueue
	yamlv3.Unmarshal(data, &updatedNQ)
	if len(updatedNQ.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(updatedNQ.Notifications))
	}
	if updatedNQ.Notifications[0].SourceResultID != "res_r5_missing" {
		t.Errorf("source_result_id: got %s", updatedNQ.Notifications[0].SourceResultID)
	}
}

func TestReconciler_R5_NotificationExists_NoRepair(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{
				ID: "res_r5_exists", CommandID: "cmd_r5_ok", Status: model.StatusCompleted,
				Notified: true, NotifiedAt: &now, CreatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	nq := model.NotificationQueue{
		SchemaVersion: 1, FileType: "queue_notification",
		Notifications: []model.Notification{
			{ID: "ntf_001", CommandID: "cmd_r5_ok", SourceResultID: "res_r5_exists",
				Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "orchestrator.yaml"), nq)

	repairs := rec.Reconcile()
	r5 := filterRepairs(repairs, "R5")
	if len(r5) != 0 {
		t.Fatalf("expected no R5 repairs, got %d", len(r5))
	}
}

// --- R6 Tests ---

func TestReconciler_R6_AwaitingFill_DeadlineExpired(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	// Set a mock executor so notification doesn't fail
	rec.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return &mockExecutorR6{}, nil
	})

	pastDeadline := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_test", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID: "p1", Name: "research", Type: "concrete",
				Status: model.PhaseStatusCompleted,
			},
			{
				PhaseID: "p2", Name: "implementation", Type: "deferred",
				Status: model.PhaseStatusAwaitingFill,
				DependsOnPhases: []string{"research"},
				FillDeadlineAt:  &pastDeadline,
			},
			{
				PhaseID: "p3", Name: "testing", Type: "deferred",
				Status: model.PhaseStatusPending,
				DependsOnPhases: []string{"implementation"},
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	statePath := filepath.Join(stateDir, "cmd_r6_test.yaml")
	yamlutil.AtomicWrite(statePath, state)

	repairs := rec.Reconcile()
	r6 := filterRepairs(repairs, "R6")
	// Should have: implementation → timed_out, testing → cancelled (cascade)
	if len(r6) != 2 {
		t.Fatalf("expected 2 R6 repairs, got %d: %+v", len(r6), r6)
	}

	// Verify state
	data, _ := os.ReadFile(statePath)
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)

	if updated.Phases[1].Status != model.PhaseStatusTimedOut {
		t.Errorf("implementation phase: got %s, want timed_out", updated.Phases[1].Status)
	}
	if updated.Phases[2].Status != model.PhaseStatusCancelled {
		t.Errorf("testing phase: got %s, want cancelled", updated.Phases[2].Status)
	}
	if updated.LastReconciledAt == nil {
		t.Error("expected last_reconciled_at to be set")
	}
}

func TestReconciler_R6_DeadlineNotExpired_NoRepair(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	futureDeadline := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_ok", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID: "p1", Name: "impl", Type: "deferred",
				Status:         model.PhaseStatusAwaitingFill,
				FillDeadlineAt: &futureDeadline,
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_ok.yaml"), state)

	repairs := rec.Reconcile()
	r6 := filterRepairs(repairs, "R6")
	if len(r6) != 0 {
		t.Fatalf("expected no R6 repairs, got %d", len(r6))
	}
}

// mockExecutorR6 is a no-op executor for R6 Planner notification tests.
type mockExecutorR6 struct{}

func (m *mockExecutorR6) Execute(req agent.ExecRequest) agent.ExecResult {
	return agent.ExecResult{Success: true}
}
func (m *mockExecutorR6) Close() error { return nil }

func filterRepairs(repairs []ReconcileRepair, pattern string) []ReconcileRepair {
	var filtered []ReconcileRepair
	for _, r := range repairs {
		if r.Pattern == pattern {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

func TestReconciler_R6_TransitiveCascade(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return &mockExecutorR6{}, nil
	})

	pastDeadline := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	// Phase chain: p1 (timed_out) → p2 (pending, depends on p1) → p3 (pending, depends on p2)
	// p3 should also be cancelled transitively.
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_trans", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID: "p1", Name: "phase1", Type: "deferred",
				Status:         model.PhaseStatusAwaitingFill,
				FillDeadlineAt: &pastDeadline,
			},
			{
				PhaseID: "p2", Name: "phase2", Type: "deferred",
				Status:          model.PhaseStatusPending,
				DependsOnPhases: []string{"phase1"},
			},
			{
				PhaseID: "p3", Name: "phase3", Type: "deferred",
				Status:          model.PhaseStatusPending,
				DependsOnPhases: []string{"phase2"},
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_trans.yaml"), state)

	repairs := rec.Reconcile()
	r6 := filterRepairs(repairs, "R6")
	// Should have 3: p1 timed_out, p2 cancelled, p3 cancelled (transitive)
	if len(r6) != 3 {
		t.Fatalf("expected 3 R6 repairs (transitive cascade), got %d: %+v", len(r6), r6)
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r6_trans.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)

	if updated.Phases[0].Status != model.PhaseStatusTimedOut {
		t.Errorf("phase1: got %s, want timed_out", updated.Phases[0].Status)
	}
	if updated.Phases[1].Status != model.PhaseStatusCancelled {
		t.Errorf("phase2: got %s, want cancelled", updated.Phases[1].Status)
	}
	if updated.Phases[2].Status != model.PhaseStatusCancelled {
		t.Errorf("phase3: got %s, want cancelled (transitive)", updated.Phases[2].Status)
	}
}
