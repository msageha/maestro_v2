package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/reconcile"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil/mocks"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// =============================================================================
// Reconciler Boundary Tests — R0 Threshold
// =============================================================================

// TestReconciler_R0_ExactThreshold verifies that R0 DOES trigger when
// planning age is exactly at the threshold boundary (dispatch_lease_sec * 2).
func TestReconciler_R0_ExactThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	// dispatch_lease_sec=60, threshold=120s. Created exactly 120s ago.
	// time.Since(createdAt).Seconds() >= threshold should trigger.
	createdAt := time.Now().UTC().Add(-120 * time.Second).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0_exact", PlanStatus: model.PlanStatusPlanning,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0_exact.yaml"), state)

	repairs, _ := rec.Reconcile()
	r0 := filterRepairs(repairs, reconcile.PatternR0)
	if len(r0) != 1 {
		t.Fatalf("expected 1 R0 repair at exact threshold, got %d", len(r0))
	}
}

// TestReconciler_R0_JustBelowThreshold verifies that R0 does NOT trigger when
// planning age is just below the threshold.
func TestReconciler_R0_JustBelowThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	// Created 115 seconds ago (safely below 120s threshold; 119s is too close due to RFC3339 truncation)
	createdAt := time.Now().UTC().Add(-115 * time.Second).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0_below", PlanStatus: model.PlanStatusPlanning,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0_below.yaml"), state)

	repairs, _ := rec.Reconcile()
	r0 := filterRepairs(repairs, reconcile.PatternR0)
	if len(r0) != 0 {
		t.Fatalf("expected 0 R0 repairs just below threshold, got %d", len(r0))
	}
}

// TestReconciler_R0_WithWorkerTasks verifies that R0 also cleans up worker tasks.
func TestReconciler_R0_WithWorkerTasks(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	queueDir := filepath.Join(maestroDir, "queue")

	oldTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0_tasks", PlanStatus: model.PlanStatusPlanning,
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0_tasks.yaml"), state)

	// Create worker queues with tasks for this command + another command
	now := time.Now().UTC().Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1, FileType: "queue_task",
		Tasks: []model.Task{
			{ID: "task_r0_1", CommandID: "cmd_r0_tasks", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
			{ID: "task_other", CommandID: "cmd_other", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "worker1.yaml"), tq)

	repairs, _ := rec.Reconcile()
	r0 := filterRepairs(repairs, reconcile.PatternR0)
	if len(r0) != 1 {
		t.Fatalf("expected 1 R0 repair, got %d", len(r0))
	}

	// Verify only the target command's tasks were removed
	data, _ := os.ReadFile(filepath.Join(queueDir, "worker1.yaml"))
	var updatedTQ model.TaskQueue
	yamlv3.Unmarshal(data, &updatedTQ)
	if len(updatedTQ.Tasks) != 1 {
		t.Fatalf("expected 1 remaining task (other command), got %d", len(updatedTQ.Tasks))
	}
	if updatedTQ.Tasks[0].ID != "task_other" {
		t.Errorf("expected task_other to remain, got %s", updatedTQ.Tasks[0].ID)
	}
}

// =============================================================================
// Reconciler Boundary Tests — R0b Filling Stuck
// =============================================================================

// TestReconciler_R0b_NoTasks verifies R0b handles empty TaskIDs gracefully.
func TestReconciler_R0b_NoTasks(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	oldTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0b_empty", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "empty-phase", Status: model.PhaseStatusFilling, TaskIDs: nil},
		},
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0b_empty.yaml"), state)

	repairs, _ := rec.Reconcile()
	r0b := filterRepairs(repairs, reconcile.PatternR0b)
	if len(r0b) != 1 {
		t.Fatalf("expected 1 R0b repair with empty TaskIDs, got %d", len(r0b))
	}
}

// TestReconciler_R0b_MultiplePhases verifies R0b handles multiple stuck phases.
func TestReconciler_R0b_MultiplePhases(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	oldTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0b_multi", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase-1", Status: model.PhaseStatusFilling, TaskIDs: []string{"t1"}},
			{PhaseID: "p2", Name: "phase-2", Status: model.PhaseStatusCompleted}, // not filling
			{PhaseID: "p3", Name: "phase-3", Status: model.PhaseStatusFilling, TaskIDs: []string{"t2", "t3"}},
		},
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0b_multi.yaml"), state)

	repairs, _ := rec.Reconcile()
	r0b := filterRepairs(repairs, reconcile.PatternR0b)
	if len(r0b) != 2 {
		t.Fatalf("expected 2 R0b repairs (2 filling phases), got %d", len(r0b))
	}
}

// =============================================================================
// Reconciler Boundary Tests — R4 CanComplete Various Statuses
// =============================================================================

// TestReconciler_R4_CanCompleteReturnsFailed verifies R4 correctly propagates
// failed status from canComplete.
func TestReconciler_R4_CanCompleteReturnsFailed(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetCanComplete(func(state *model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusFailed, nil
	})

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r4_f", CommandID: "cmd_r4_f", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r4_f", PlanStatus: model.PlanStatusSealed,
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r4_f.yaml"), state)

	repairs, _ := rec.Reconcile()
	r4 := filterRepairs(repairs, reconcile.PatternR4)
	if len(r4) != 1 {
		t.Fatalf("expected 1 R4 repair, got %d", len(r4))
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r4_f.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.PlanStatus != model.PlanStatusFailed {
		t.Errorf("plan_status: got %s, want failed", updated.PlanStatus)
	}
}

// TestReconciler_R4_CanCompleteReturnsCancelled verifies R4 correctly propagates
// cancelled status from canComplete.
func TestReconciler_R4_CanCompleteReturnsCancelled(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetCanComplete(func(state *model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCancelled, nil
	})

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r4_c", CommandID: "cmd_r4_c", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r4_c", PlanStatus: model.PlanStatusSealed,
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r4_c.yaml"), state)

	repairs, _ := rec.Reconcile()
	r4 := filterRepairs(repairs, reconcile.PatternR4)
	if len(r4) != 1 {
		t.Fatalf("expected 1 R4 repair, got %d", len(r4))
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r4_c.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.PlanStatus != model.PlanStatusCancelled {
		t.Errorf("plan_status: got %s, want cancelled", updated.PlanStatus)
	}
}

// TestReconciler_R4_AlreadyTerminal_NoRepair verifies R4 skips commands
// whose plan_status is already terminal.
func TestReconciler_R4_AlreadyTerminal_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetCanComplete(func(state *model.CommandState) (model.PlanStatus, error) {
		t.Fatal("canComplete should not be called for already-terminal states")
		return "", nil
	})

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r4_term", CommandID: "cmd_r4_term", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r4_term", PlanStatus: model.PlanStatusCompleted, // already terminal
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r4_term.yaml"), state)

	repairs, _ := rec.Reconcile()
	r4 := filterRepairs(repairs, reconcile.PatternR4)
	if len(r4) != 0 {
		t.Fatalf("expected 0 R4 repairs for terminal state, got %d", len(r4))
	}
}

// =============================================================================
// Reconciler Boundary Tests — R6 Complex Cascade
// =============================================================================

// TestReconciler_R6_MultiplePhasesTimedOut verifies R6 with multiple timed-out
// phases and their independent downstream cascades.
func TestReconciler_R6_MultiplePhasesTimedOut(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconcilerWithFactory(maestroDir, func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	})

	pastDeadline := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	// Two independent branches that both time out:
	// p1 (timed_out) → p3 (cancelled)
	// p2 (timed_out) → p4 (cancelled)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_multi", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "branch1", Type: "deferred", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			{PhaseID: "p2", Name: "branch2", Type: "deferred", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			{PhaseID: "p3", Name: "downstream1", Type: "deferred", Status: model.PhaseStatusPending, DependsOnPhases: []string{"branch1"}},
			{PhaseID: "p4", Name: "downstream2", Type: "deferred", Status: model.PhaseStatusPending, DependsOnPhases: []string{"branch2"}},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_multi.yaml"), state)

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, reconcile.PatternR6)
	// Should have 4: p1 timed_out, p2 timed_out, p3 cancelled, p4 cancelled
	if len(r6) != 4 {
		t.Fatalf("expected 4 R6 repairs, got %d: %+v", len(r6), r6)
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r6_multi.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)

	for _, phase := range updated.Phases {
		switch phase.Name {
		case "branch1", "branch2":
			if phase.Status != model.PhaseStatusTimedOut {
				t.Errorf("%s: got %s, want timed_out", phase.Name, phase.Status)
			}
		case "downstream1", "downstream2":
			if phase.Status != model.PhaseStatusCancelled {
				t.Errorf("%s: got %s, want cancelled", phase.Name, phase.Status)
			}
		}
	}
}

// TestReconciler_R6_DiamondDependency verifies R6 cascade with diamond dependency:
// p1 → p2, p1 → p3, p2+p3 → p4
func TestReconciler_R6_DiamondDependency(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconcilerWithFactory(maestroDir, func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	})

	pastDeadline := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_diamond", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "root", Type: "deferred", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			{PhaseID: "p2", Name: "left", Type: "deferred", Status: model.PhaseStatusPending, DependsOnPhases: []string{"root"}},
			{PhaseID: "p3", Name: "right", Type: "deferred", Status: model.PhaseStatusPending, DependsOnPhases: []string{"root"}},
			{PhaseID: "p4", Name: "merge", Type: "deferred", Status: model.PhaseStatusPending, DependsOnPhases: []string{"left", "right"}},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_diamond.yaml"), state)

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, reconcile.PatternR6)
	// root timed_out, left cancelled, right cancelled, merge cancelled
	if len(r6) != 4 {
		t.Fatalf("expected 4 R6 repairs (diamond), got %d: %+v", len(r6), r6)
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r6_diamond.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)

	if updated.Phases[0].Status != model.PhaseStatusTimedOut {
		t.Errorf("root: got %s, want timed_out", updated.Phases[0].Status)
	}
	for i := 1; i <= 3; i++ {
		if updated.Phases[i].Status != model.PhaseStatusCancelled {
			t.Errorf("phase %s: got %s, want cancelled", updated.Phases[i].Name, updated.Phases[i].Status)
		}
	}
}

// TestReconciler_R6_ActivePhaseNotCancelled verifies that active phases
// are NOT cancelled by R6 cascade (only pending/awaiting_fill).
func TestReconciler_R6_ActivePhaseNotCancelled(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconcilerWithFactory(maestroDir, func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	})

	pastDeadline := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_active", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "timeout", Type: "deferred", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			{PhaseID: "p2", Name: "active_phase", Type: "concrete", Status: model.PhaseStatusActive, DependsOnPhases: []string{"timeout"}},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_active.yaml"), state)

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, reconcile.PatternR6)
	// Only p1 timed_out; p2 is active so NOT cancelled
	if len(r6) != 1 {
		t.Fatalf("expected 1 R6 repair (only timeout), got %d: %+v", len(r6), r6)
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r6_active.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.Phases[1].Status != model.PhaseStatusActive {
		t.Errorf("active_phase: got %s, want active (not affected by cascade)", updated.Phases[1].Status)
	}
}

// TestReconciler_R6_NoDeadline_NoTimeout verifies that awaiting_fill phases
// without fill_deadline_at are NOT timed out.
func TestReconciler_R6_NoDeadline_NoTimeout(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_nodeadline", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "no_deadline", Type: "deferred", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: nil},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_nodeadline.yaml"), state)

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, reconcile.PatternR6)
	if len(r6) != 0 {
		t.Fatalf("expected 0 R6 repairs (no deadline), got %d", len(r6))
	}
}

// =============================================================================
// Dependency Failure Propagation — Complex DAG
// =============================================================================

// TestDependencyFailure_TransitivePending verifies that when taskA fails,
// taskB (depends on taskA) and taskC (depends on taskB) are both cancelled.
func TestDependencyFailure_TransitivePending(t *testing.T) {
	t.Parallel()
	d := newBoundaryTestDaemon(t)
	commandID := "cmd_dep_trans"
	taskA := "task_dep_a"
	taskB := "task_dep_b"
	taskC := "task_dep_c"

	state := model.CommandState{
		SchemaVersion:   1, FileType: "state_command",
		CommandID:       commandID, PlanStatus: model.PlanStatusSealed,
		RequiredTaskIDs: []string{taskA, taskB, taskC},
		TaskStates:      map[string]model.Status{taskA: model.StatusFailed, taskB: model.StatusPending, taskC: model.StatusPending},
		TaskDependencies: map[string][]string{
			taskB: {taskA},
			taskC: {taskB},
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml"), state)

	tq := model.TaskQueue{
		SchemaVersion: 1, FileType: "queue_task",
		Tasks: []model.Task{
			{ID: taskB, CommandID: commandID, Status: model.StatusPending, BlockedBy: []string{taskA}, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
			{ID: taskC, CommandID: commandID, Status: model.StatusPending, BlockedBy: []string{taskB}, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	// First scan: taskB cancelled (depends on failed taskA)
	d.handler.PeriodicScan()

	tqAfter1 := readTaskQueue(t, d, "worker1")
	taskBStatus := model.StatusPending
	taskCStatus := model.StatusPending
	for _, task := range tqAfter1.Tasks {
		switch task.ID {
		case taskB:
			taskBStatus = task.Status
		case taskC:
			taskCStatus = task.Status
		}
	}
	if taskBStatus != model.StatusCancelled {
		t.Errorf("taskB: got %s, want cancelled", taskBStatus)
	}

	// Second scan: taskC cancelled (depends on now-cancelled taskB)
	d.handler.PeriodicScan()

	tqAfter2 := readTaskQueue(t, d, "worker1")
	for _, task := range tqAfter2.Tasks {
		if task.ID == taskC {
			taskCStatus = task.Status
		}
	}
	if taskCStatus != model.StatusCancelled {
		t.Errorf("taskC: got %s, want cancelled (transitive propagation)", taskCStatus)
	}
}

// TestDependencyFailure_InProgressTaskInterrupted verifies that an in-progress
// task with a failed dependency gets cancelled and an interrupt is deferred.
func TestDependencyFailure_InProgressTaskInterrupted(t *testing.T) {
	t.Parallel()
	d := newBoundaryTestDaemon(t)
	commandID := "cmd_dep_inprog"
	taskA := "task_depip_a"
	taskB := "task_depip_b"

	state := model.CommandState{
		SchemaVersion:   1, FileType: "state_command",
		CommandID:       commandID, PlanStatus: model.PlanStatusSealed,
		RequiredTaskIDs: []string{taskA, taskB},
		TaskStates:      map[string]model.Status{taskA: model.StatusFailed, taskB: model.StatusInProgress},
		TaskDependencies: map[string][]string{
			taskB: {taskA},
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml"), state)

	owner := "worker1"
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1, FileType: "queue_task",
		Tasks: []model.Task{
			{
				ID: taskB, CommandID: commandID, Status: model.StatusInProgress,
				LeaseOwner: &owner, LeaseExpiresAt: &future, LeaseEpoch: 1,
				BlockedBy: []string{taskA},
				CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	d.handler.PeriodicScan()

	tqAfter := readTaskQueue(t, d, "worker1")
	if len(tqAfter.Tasks) == 0 {
		t.Fatal("expected task in queue")
	}
	if tqAfter.Tasks[0].Status != model.StatusCancelled {
		t.Errorf("taskB: got %s, want cancelled (dependency failure)", tqAfter.Tasks[0].Status)
	}
	if tqAfter.Tasks[0].LeaseOwner != nil {
		t.Error("lease should be cleared after cancellation")
	}
}

// =============================================================================
// Signal Processing — Boundary Tests
// =============================================================================

// TestSignalProcessing_BackoffRespected verifies that signals with
// NextAttemptAt in the future are not delivered.
func TestSignalProcessing_BackoffRespected(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	futureAttempt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind: "awaiting_fill", CommandID: "cmd_sig1", PhaseID: "p1", PhaseName: "phase1",
				Message: "test signal", Attempts: 2, NextAttemptAt: &futureAttempt,
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	sigPath := filepath.Join(maestroDir, "queue", "planner_signals.yaml")
	yamlutil.AtomicWrite(sigPath, sq)

	qh.PeriodicScan()

	// Signal should still be in queue (not delivered due to backoff)
	data, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatal("signal file should still exist")
	}
	var updated model.PlannerSignalQueue
	yamlv3.Unmarshal(data, &updated)
	if len(updated.Signals) != 1 {
		t.Fatalf("expected 1 signal retained (backoff), got %d", len(updated.Signals))
	}
	if updated.Signals[0].Attempts != 2 {
		t.Errorf("attempts should be unchanged (2), got %d", updated.Signals[0].Attempts)
	}
}

// TestSignalBackoff_Exponential verifies the exponential backoff computation.
func TestSignalBackoff_Exponential(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)
	qh.config.Watcher.ScanIntervalSec = 60

	tests := []struct {
		attempts int
		baseSec  int // base value before jitter
	}{
		{0, 2},  // clamped to 1, baseSec=2 (≤3) → 2*(1<<0) = 2
		{1, 2},  // baseSec=2 (≤3) → 2*(1<<0) = 2
		{2, 4},  // baseSec=2 (≤3) → 2*(1<<1) = 4
		{3, 8},  // baseSec=2 (≤3) → 2*(1<<2) = 8
		{4, 40}, // baseSec=5 (>3) → 5*(1<<3) = 40
		{5, 60}, // baseSec=5 (>3) → 5*(1<<4) = 80, capped at ScanIntervalSec=60
		{10, 60},
	}

	for _, tt := range tests {
		d := qh.computeSignalBackoff(tt.attempts)
		base := time.Duration(tt.baseSec) * time.Second
		lo := time.Duration(float64(base) * 0.75)
		hi := time.Duration(float64(base) * 1.25)
		if d < lo || d > hi {
			t.Errorf("attempts=%d: got %v, want in [%v, %v]", tt.attempts, d, lo, hi)
		}
	}
}
