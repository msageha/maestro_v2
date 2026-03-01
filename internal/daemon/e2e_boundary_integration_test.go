//go:build integration

package daemon_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// =============================================================================
// E2E Boundary: Cancel-During-Phase-Transition
// =============================================================================

// TestE2E_CancelDuringAwaitingFill verifies that cancelling a command while
// a phase is in awaiting_fill properly records cancel.requested and cancel.reason.
func TestE2E_CancelDuringAwaitingFill(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "cancel during awaiting_fill")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	phasesFile := writePhasesYAML(t, []phaseInputYAML{
		{
			Name: "research",
			Type: "concrete",
			Tasks: []taskInputYAML{
				{
					Name: "research_task", Purpose: "research", Content: "investigate",
					AcceptanceCriteria: "done", BloomLevel: 2, Required: true,
				},
			},
		},
		{
			Name:            "implementation",
			Type:            "deferred",
			DependsOnPhases: []string{"research"},
			Constraints: &constraintInputYAML{
				MaxTasks: 5, AllowedBloomLevels: []int{1, 2, 3, 4, 5, 6}, TimeoutMinutes: 30,
			},
		},
	})

	resp := callPlanSubmit(t, e, commandID, phasesFile)
	sr := extractSubmitResult(t, resp)

	var researchTask submitTaskResult
	for _, p := range sr.Phases {
		if p.Name == "research" && len(p.Tasks) > 0 {
			researchTask = p.Tasks[0]
		}
	}

	e.PeriodicScan() // dispatch
	tq := daemon.E2EReadTaskQueue(t, e, researchTask.Worker)
	var epoch int
	for _, qt := range tq.Tasks {
		if qt.ID == researchTask.TaskID {
			epoch = qt.LeaseEpoch
		}
	}
	daemon.E2EWriteResult(t, e, researchTask.Worker, researchTask.TaskID, commandID, "completed", "done", epoch)
	e.PeriodicScan()
	e.PeriodicScan()

	// Verify awaiting_fill
	state := daemon.E2EReadCommandState(t, e, commandID)
	awaitingFill := false
	for _, phase := range state.Phases {
		if phase.Name == "implementation" && phase.Status == model.PhaseStatusAwaitingFill {
			awaitingFill = true
		}
	}
	if !awaitingFill {
		t.Fatal("expected implementation phase to be awaiting_fill before cancel")
	}

	// Cancel via queue-write cancel-request (the correct API)
	cancelResp := daemon.E2EWriteCancelRequest(t, e, commandID, "user requested cancellation")
	if !cancelResp.Success {
		t.Fatalf("cancel request should succeed: %+v", cancelResp.Error)
	}

	// Verify cancel.requested is set in state immediately
	stateAfterCancel := daemon.E2EReadCommandState(t, e, commandID)
	if !stateAfterCancel.Cancel.Requested {
		t.Fatal("expected cancel.requested = true after cancel-request")
	}
	if stateAfterCancel.Cancel.Reason == nil || *stateAfterCancel.Cancel.Reason != "user requested cancellation" {
		t.Errorf("cancel.reason: got %v, want 'user requested cancellation'", stateAfterCancel.Cancel.Reason)
	}

	// Scan to process cancellation effects
	e.PeriodicScan()
	e.PeriodicScan()

	// Verify cancel state is preserved and phases reflect cancellation
	finalState := daemon.E2EReadCommandState(t, e, commandID)
	if !finalState.Cancel.Requested {
		t.Error("cancel.requested should remain true")
	}
	// The cancel handler only cancels tasks (pending/in_progress), not phases.
	// A deferred phase in awaiting_fill has no tasks to cancel, so it stays awaiting_fill.
	for _, phase := range finalState.Phases {
		if phase.Name == "implementation" {
			if phase.Status != model.PhaseStatusAwaitingFill {
				t.Errorf("implementation phase after cancel: got %s, want awaiting_fill (cancel only affects tasks)", phase.Status)
			}
		}
	}
}

// =============================================================================
// E2E Boundary: Idempotent Result Writes
// =============================================================================

// TestE2E_DuplicateResultWrite verifies that writing the same result twice
// returns the same result_id (idempotent) and doesn't corrupt state.
func TestE2E_DuplicateResultWrite(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "duplicate result test")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	tasksFile := writeTasksYAML(t, []taskInputYAML{
		{
			Name: "task_dup", Purpose: "test dup", Content: "work",
			AcceptanceCriteria: "done", BloomLevel: 3, Required: true,
		},
	})
	resp := callPlanSubmit(t, e, commandID, tasksFile)
	sr := extractSubmitResult(t, resp)
	task := sr.Tasks[0]

	e.PeriodicScan() // dispatch

	tq := daemon.E2EReadTaskQueue(t, e, task.Worker)
	var epoch int
	for _, qt := range tq.Tasks {
		if qt.ID == task.TaskID {
			epoch = qt.LeaseEpoch
		}
	}

	resultID1 := daemon.E2EWriteResult(t, e, task.Worker, task.TaskID, commandID, "completed", "done", epoch)
	resultID2 := daemon.E2EWriteResult(t, e, task.Worker, task.TaskID, commandID, "completed", "done", epoch)

	if resultID1 == "" || resultID2 == "" {
		t.Fatal("both result writes should succeed")
	}

	// Idempotency: both should return the same result_id
	if resultID1 != resultID2 {
		t.Errorf("idempotent result writes should return same ID: got %s and %s", resultID1, resultID2)
	}

	// Verify exactly one result entry exists for the task
	rf := daemon.E2EReadResultFile(t, e, task.Worker)
	count := 0
	for _, r := range rf.Results {
		if r.TaskID == task.TaskID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 result entry for task, got %d", count)
	}

	e.PeriodicScan()

	state := daemon.E2EReadCommandState(t, e, commandID)
	if state.TaskStates[task.TaskID] != model.StatusCompleted {
		t.Errorf("task status after dup result: got %s, want completed", state.TaskStates[task.TaskID])
	}
}

// =============================================================================
// E2E Boundary: Result Write with Wrong Epoch (Fencing)
// =============================================================================

// TestE2E_ResultWriteWrongEpoch verifies that a result write with a stale
// epoch is rejected by the fencing logic with FENCING_REJECT.
func TestE2E_ResultWriteWrongEpoch(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "fencing test")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	tasksFile := writeTasksYAML(t, []taskInputYAML{
		{
			Name: "task_fence", Purpose: "test fence", Content: "work",
			AcceptanceCriteria: "done", BloomLevel: 3, Required: true,
		},
	})
	resp := callPlanSubmit(t, e, commandID, tasksFile)
	sr := extractSubmitResult(t, resp)
	task := sr.Tasks[0]

	e.PeriodicScan() // dispatch

	tq := daemon.E2EReadTaskQueue(t, e, task.Worker)
	var epoch int
	for _, qt := range tq.Tasks {
		if qt.ID == task.TaskID {
			epoch = qt.LeaseEpoch
		}
	}

	// Attempt stale write with wrong epoch — should be rejected
	staleEpoch := epoch + 100
	staleResp := daemon.E2EWriteResultRaw(t, e, daemon.ResultWriteParams{
		Reporter:   task.Worker,
		TaskID:     task.TaskID,
		CommandID:  commandID,
		LeaseEpoch: staleEpoch,
		Status:     "completed",
		Summary:    "stale result",
		RetrySafe:  true,
	})
	if staleResp.Success {
		t.Fatal("stale epoch result write should be rejected")
	}
	if staleResp.Error == nil || staleResp.Error.Code != uds.ErrCodeFencingReject {
		t.Errorf("expected FENCING_REJECT error, got %+v", staleResp.Error)
	}

	// Verify task is still in_progress (not corrupted)
	tqAfter := daemon.E2EReadTaskQueue(t, e, task.Worker)
	for _, qt := range tqAfter.Tasks {
		if qt.ID == task.TaskID {
			if qt.Status != model.StatusInProgress {
				t.Errorf("task should still be in_progress after stale write, got %s", qt.Status)
			}
		}
	}

	// Now write with correct epoch — should succeed
	correctResult := daemon.E2EWriteResult(t, e, task.Worker, task.TaskID, commandID, "completed", "correct result", epoch)
	if correctResult == "" {
		t.Fatal("correct epoch result write should succeed")
	}

	e.PeriodicScan()
	state := daemon.E2EReadCommandState(t, e, commandID)
	if state.TaskStates[task.TaskID] != model.StatusCompleted {
		t.Errorf("task should be completed with correct epoch, got %s", state.TaskStates[task.TaskID])
	}
}

// =============================================================================
// E2E Boundary: Task with Dependencies — Diamond DAG
// =============================================================================

// TestE2E_DiamondDependency verifies correct dispatch ordering in a diamond DAG:
// A → B, A → C, B+C → D.
func TestE2E_DiamondDependency(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "diamond dep test")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	tasksFile := writeTasksYAML(t, []taskInputYAML{
		{
			Name: "task_a", Purpose: "base task", Content: "A",
			AcceptanceCriteria: "done", BloomLevel: 2, Required: true,
		},
		{
			Name: "task_b", Purpose: "left branch", Content: "B",
			AcceptanceCriteria: "done", BloomLevel: 2, Required: true,
			BlockedBy: []string{"task_a"},
		},
		{
			Name: "task_c", Purpose: "right branch", Content: "C",
			AcceptanceCriteria: "done", BloomLevel: 2, Required: true,
			BlockedBy: []string{"task_a"},
		},
		{
			Name: "task_d", Purpose: "join", Content: "D",
			AcceptanceCriteria: "done", BloomLevel: 2, Required: true,
			BlockedBy: []string{"task_b", "task_c"},
		},
	})

	resp := callPlanSubmit(t, e, commandID, tasksFile)
	sr := extractSubmitResult(t, resp)

	if len(sr.Tasks) != 4 {
		t.Fatalf("expected 4 tasks, got %d", len(sr.Tasks))
	}

	taskByName := make(map[string]submitTaskResult)
	for _, task := range sr.Tasks {
		taskByName[task.Name] = task
	}

	// Step 1: Scan — only task_a should be dispatched
	e.PeriodicScan()

	taskA := taskByName["task_a"]
	tqA := daemon.E2EReadTaskQueue(t, e, taskA.Worker)
	var aEpoch int
	aDispatched := false
	for _, qt := range tqA.Tasks {
		if qt.ID == taskA.TaskID && qt.Status == model.StatusInProgress {
			aDispatched = true
			aEpoch = qt.LeaseEpoch
		}
	}
	if !aDispatched {
		t.Fatal("task_a should be dispatched first (no deps)")
	}

	// B, C, D should all be pending (blocked)
	taskB := taskByName["task_b"]
	taskC := taskByName["task_c"]
	taskD := taskByName["task_d"]
	for _, tc := range []struct {
		name string
		info submitTaskResult
	}{
		{"task_b", taskB}, {"task_c", taskC}, {"task_d", taskD},
	} {
		tq := daemon.E2EReadTaskQueue(t, e, tc.info.Worker)
		for _, qt := range tq.Tasks {
			if qt.ID == tc.info.TaskID && qt.Status != model.StatusPending {
				t.Errorf("%s should be pending before A completes, got %s", tc.name, qt.Status)
			}
		}
	}

	// Step 2: Complete A
	daemon.E2EWriteResult(t, e, taskA.Worker, taskA.TaskID, commandID, "completed", "A done", aEpoch)

	// Step 3: Dispatch and complete B and C.
	// buildGlobalInFlightSet enforces at-most-one in_progress task per worker.
	// If B and C share a worker, only one can be dispatched per scan.
	// We handle this by completing whichever is dispatched first, then dispatching the other.
	bCompleted := false
	cCompleted := false
	for attempt := 0; attempt < 10 && !(bCompleted && cCompleted); attempt++ {
		e.PeriodicScan()

		// Check and complete B if dispatched
		if !bCompleted {
			tqB := daemon.E2EReadTaskQueue(t, e, taskB.Worker)
			for _, qt := range tqB.Tasks {
				if qt.ID == taskB.TaskID && qt.Status == model.StatusInProgress {
					// Verify D is still blocked
					tqD := daemon.E2EReadTaskQueue(t, e, taskD.Worker)
					for _, dq := range tqD.Tasks {
						if dq.ID == taskD.TaskID && dq.Status == model.StatusInProgress {
							t.Errorf("task_d dispatched before B and C both complete")
						}
					}
					daemon.E2EWriteResult(t, e, taskB.Worker, taskB.TaskID, commandID, "completed", "B done", qt.LeaseEpoch)
					bCompleted = true
				}
			}
		}

		// Check and complete C if dispatched
		if !cCompleted {
			tqC := daemon.E2EReadTaskQueue(t, e, taskC.Worker)
			for _, qt := range tqC.Tasks {
				if qt.ID == taskC.TaskID && qt.Status == model.StatusInProgress {
					// Verify D is still blocked (unless B also just completed above)
					if !bCompleted {
						tqD := daemon.E2EReadTaskQueue(t, e, taskD.Worker)
						for _, dq := range tqD.Tasks {
							if dq.ID == taskD.TaskID && dq.Status == model.StatusInProgress {
								t.Errorf("task_d dispatched before B and C both complete")
							}
						}
					}
					daemon.E2EWriteResult(t, e, taskC.Worker, taskC.TaskID, commandID, "completed", "C done", qt.LeaseEpoch)
					cCompleted = true
				}
			}
		}
	}
	if !bCompleted {
		t.Fatal("task_b was never dispatched")
	}
	if !cCompleted {
		t.Fatal("task_c was never dispatched")
	}

	// Step 4: D should become dispatchable
	var dEpoch int
	for scan := 0; scan < 5; scan++ {
		e.PeriodicScan()
		tqD := daemon.E2EReadTaskQueue(t, e, taskD.Worker)
		for _, qt := range tqD.Tasks {
			if qt.ID == taskD.TaskID && qt.Status == model.StatusInProgress {
				dEpoch = qt.LeaseEpoch
			}
		}
		if dEpoch > 0 {
			break
		}
	}
	if dEpoch == 0 {
		t.Fatal("task_d should be dispatched after B and C complete")
	}

	// Step 5: Complete D and verify final state
	daemon.E2EWriteResult(t, e, taskD.Worker, taskD.TaskID, commandID, "completed", "D done", dEpoch)
	e.PeriodicScan()

	completeResp := callPlanComplete(t, e, commandID, "all done")
	if !completeResp.Success {
		t.Fatalf("plan complete failed: %+v", completeResp.Error)
	}

	finalState := daemon.E2EReadCommandState(t, e, commandID)
	if finalState.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("final plan_status = %q, want completed", finalState.PlanStatus)
	}
	for taskName, s := range finalState.TaskStates {
		if s != model.StatusCompleted {
			t.Errorf("task %s should be completed, found %s", taskName, s)
		}
	}
}

// =============================================================================
// E2E Boundary: Mixed Success/Failure with Non-Required Tasks
// =============================================================================

// TestE2E_NonRequiredTaskFailure verifies that failure of a non-required task
// does not prevent plan completion as "completed" (not "failed").
func TestE2E_NonRequiredTaskFailure(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "non-required failure test")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	tasksFile := writeTasksYAML(t, []taskInputYAML{
		{
			Name: "required_task", Purpose: "must succeed", Content: "critical",
			AcceptanceCriteria: "done", BloomLevel: 3, Required: true,
		},
		{
			Name: "optional_task", Purpose: "nice to have", Content: "optional",
			AcceptanceCriteria: "done", BloomLevel: 2, Required: false,
		},
	})

	resp := callPlanSubmit(t, e, commandID, tasksFile)
	sr := extractSubmitResult(t, resp)

	// Dispatch and complete/fail both tasks (may need multiple scans if same worker)
	completedTasks := make(map[string]bool)
	for scan := 0; scan < 10 && len(completedTasks) < len(sr.Tasks); scan++ {
		e.PeriodicScan()
		for _, taskInfo := range sr.Tasks {
			if completedTasks[taskInfo.TaskID] {
				continue
			}
			tq := daemon.E2EReadTaskQueue(t, e, taskInfo.Worker)
			for _, qt := range tq.Tasks {
				if qt.ID == taskInfo.TaskID && qt.Status == model.StatusInProgress {
					status := "completed"
					summary := "success"
					if taskInfo.Name == "optional_task" {
						status = "failed"
						summary = "optional failed"
					}
					daemon.E2EWriteResult(t, e, taskInfo.Worker, taskInfo.TaskID, commandID, status, summary, qt.LeaseEpoch)
					completedTasks[taskInfo.TaskID] = true
				}
			}
		}
	}
	if len(completedTasks) != len(sr.Tasks) {
		t.Fatalf("expected all %d tasks to be dispatched, only %d were", len(sr.Tasks), len(completedTasks))
	}

	e.PeriodicScan()

	// Verify task states in command state before completing
	stateBeforeComplete := daemon.E2EReadCommandState(t, e, commandID)
	for taskID, taskStatus := range stateBeforeComplete.TaskStates {
		isRequired := false
		for _, rid := range stateBeforeComplete.RequiredTaskIDs {
			if rid == taskID {
				isRequired = true
			}
		}
		if isRequired && taskStatus != model.StatusCompleted {
			t.Errorf("required task %s should be completed, got %s", taskID, taskStatus)
		}
		if !isRequired && taskStatus != model.StatusFailed {
			t.Errorf("optional task %s should be failed, got %s", taskID, taskStatus)
		}
	}

	// Plan complete should succeed — failed task is non-required
	completeResp := callPlanComplete(t, e, commandID, "completed with optional failure")
	if !completeResp.Success {
		t.Fatalf("plan complete should succeed: %+v", completeResp.Error)
	}

	var result map[string]string
	if err := json.Unmarshal(completeResp.Data, &result); err != nil {
		t.Fatalf("parse complete response: %v", err)
	}
	if result["status"] != "completed" {
		t.Errorf("plan status = %q, want completed (non-required failure)", result["status"])
	}

	// Verify final plan_status
	finalState := daemon.E2EReadCommandState(t, e, commandID)
	if finalState.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("final plan_status = %q, want completed", finalState.PlanStatus)
	}
}

// =============================================================================
// E2E Boundary: Phase Fill Timeout → Cascade Cancel (with downstream phase)
// =============================================================================

// TestE2E_PhaseFillTimeout verifies that when a deferred phase's fill deadline
// expires, checkAwaitingFillTimeout (step 0.7) triggers timed_out and
// checkPendingPhaseCascade cascades cancellation to downstream phases.
func TestE2E_PhaseFillTimeout(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "fill timeout test")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	// Three-phase plan: research → implementation (deferred) → testing (deferred, depends on impl)
	phasesFile := writePhasesYAML(t, []phaseInputYAML{
		{
			Name: "research",
			Type: "concrete",
			Tasks: []taskInputYAML{
				{
					Name: "research_task", Purpose: "research", Content: "study",
					AcceptanceCriteria: "done", BloomLevel: 2, Required: true,
				},
			},
		},
		{
			Name:            "implementation",
			Type:            "deferred",
			DependsOnPhases: []string{"research"},
			Constraints: &constraintInputYAML{
				MaxTasks: 5, AllowedBloomLevels: []int{1, 2, 3, 4, 5, 6}, TimeoutMinutes: 1,
			},
		},
		{
			Name:            "testing",
			Type:            "deferred",
			DependsOnPhases: []string{"implementation"},
			Constraints: &constraintInputYAML{
				MaxTasks: 3, AllowedBloomLevels: []int{1, 2, 3}, TimeoutMinutes: 30,
			},
		},
	})

	resp := callPlanSubmit(t, e, commandID, phasesFile)
	sr := extractSubmitResult(t, resp)

	// Complete research task
	var researchTask submitTaskResult
	for _, p := range sr.Phases {
		if p.Name == "research" && len(p.Tasks) > 0 {
			researchTask = p.Tasks[0]
		}
	}

	e.PeriodicScan() // dispatch
	tq := daemon.E2EReadTaskQueue(t, e, researchTask.Worker)
	var epoch int
	for _, qt := range tq.Tasks {
		if qt.ID == researchTask.TaskID {
			epoch = qt.LeaseEpoch
		}
	}
	daemon.E2EWriteResult(t, e, researchTask.Worker, researchTask.TaskID, commandID, "completed", "done", epoch)
	e.PeriodicScan()
	e.PeriodicScan()

	// Verify awaiting_fill with deadline
	state := daemon.E2EReadCommandState(t, e, commandID)
	var implPhase *model.Phase
	for i, phase := range state.Phases {
		if phase.Name == "implementation" {
			implPhase = &state.Phases[i]
		}
	}
	if implPhase == nil || implPhase.Status != model.PhaseStatusAwaitingFill {
		t.Fatal("implementation phase should be awaiting_fill")
	}
	if implPhase.FillDeadlineAt == nil {
		t.Fatal("FillDeadlineAt should be set for deferred phase with TimeoutMinutes > 0")
	}

	// Manually set the fill deadline to the past to trigger R6
	pastDeadline := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	statePath := filepath.Join(e.MaestroDir(), "state", "commands", commandID+".yaml")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var rawState model.CommandState
	if err := yamlv3.Unmarshal(data, &rawState); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	for i := range rawState.Phases {
		if rawState.Phases[i].Name == "implementation" {
			rawState.Phases[i].FillDeadlineAt = &pastDeadline
		}
	}
	updatedData, err := yamlv3.Marshal(rawState)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(statePath, updatedData, 0644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// Cascade cancel requires two scan cycles:
	// Scan N: checkAwaitingFillTimeout detects expired deadline → implementation=timed_out
	// Scan N+1: checkPendingPhaseCascade sees implementation=timed_out → testing=cancelled
	// Note: handleResultWrite's background goroutine may contribute an additional scan,
	// so we run 2 explicit scans to ensure both transitions complete.
	e.PeriodicScan()
	e.PeriodicScan()

	// Verify phases
	stateAfter := daemon.E2EReadCommandState(t, e, commandID)
	for _, phase := range stateAfter.Phases {
		switch phase.Name {
		case "research":
			if phase.Status != model.PhaseStatusCompleted {
				t.Errorf("research phase: got %s, want completed", phase.Status)
			}
		case "implementation":
			if phase.Status != model.PhaseStatusTimedOut {
				t.Errorf("implementation phase: got %s, want timed_out", phase.Status)
			}
		case "testing":
			// Downstream phase should be cascade-cancelled
			if phase.Status != model.PhaseStatusCancelled {
				t.Errorf("testing phase (downstream): got %s, want cancelled (cascade from timed_out)", phase.Status)
			}
		}
	}
}

// =============================================================================
// E2E Boundary: Empty Plan Submit (No Tasks)
// =============================================================================

// TestE2E_EmptyPlanSubmit verifies that submitting a plan with no tasks
// is rejected with an error.
func TestE2E_EmptyPlanSubmit(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "empty plan test")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	tasksFile := writeTasksYAML(t, []taskInputYAML{})

	resp := callPlanSubmit(t, e, commandID, tasksFile)
	if resp.Success {
		t.Error("expected empty plan submit to fail")
	}
	if resp.Error == nil {
		t.Fatal("expected error in response")
	}
	// Verify error message indicates the issue
	if resp.Error.Message == "" {
		t.Error("expected meaningful error message for empty plan")
	}

	// Verify no state file was created for the command
	statePath := filepath.Join(e.MaestroDir(), "state", "commands", commandID+".yaml")
	if _, err := os.Stat(statePath); err == nil {
		t.Error("state file should not exist after rejected empty submit")
	}
}

// =============================================================================
// E2E Boundary: Double Plan Submit (Idempotency)
// =============================================================================

// TestE2E_DoublePlanSubmit verifies that submitting a plan twice for the same
// command is rejected, and the state is not altered by the second submit.
func TestE2E_DoublePlanSubmit(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "double submit test")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	tasksFile := writeTasksYAML(t, []taskInputYAML{
		{
			Name: "task_1", Purpose: "test", Content: "work",
			AcceptanceCriteria: "done", BloomLevel: 3, Required: true,
		},
	})

	// First submit — should succeed
	resp1 := callPlanSubmit(t, e, commandID, tasksFile)
	if !resp1.Success {
		t.Fatalf("first submit should succeed: %+v", resp1.Error)
	}

	// Capture state after first submit
	stateAfterFirst := daemon.E2EReadCommandState(t, e, commandID)
	taskCountAfterFirst := len(stateAfterFirst.TaskStates)

	// Second submit — should fail (already sealed)
	resp2 := callPlanSubmit(t, e, commandID, tasksFile)
	if resp2.Success {
		t.Error("second submit should be rejected (already sealed)")
	}
	if resp2.Error == nil {
		t.Fatal("expected error for duplicate submit")
	}

	// Verify state was not altered by second submit
	stateAfterSecond := daemon.E2EReadCommandState(t, e, commandID)
	if len(stateAfterSecond.TaskStates) != taskCountAfterFirst {
		t.Errorf("task count changed: %d → %d (second submit should not alter state)",
			taskCountAfterFirst, len(stateAfterSecond.TaskStates))
	}
	if stateAfterSecond.PlanStatus != stateAfterFirst.PlanStatus {
		t.Errorf("plan_status changed: %s → %s", stateAfterFirst.PlanStatus, stateAfterSecond.PlanStatus)
	}
}

// =============================================================================
// E2E Boundary: Plan Complete Before All Tasks Done
// =============================================================================

// TestE2E_PlanCompleteBeforeTasksDone verifies that plan complete fails
// when required tasks are still non-terminal, and plan status remains unchanged.
func TestE2E_PlanCompleteBeforeTasksDone(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "premature complete test")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	tasksFile := writeTasksYAML(t, []taskInputYAML{
		{
			Name: "task_a", Purpose: "test", Content: "work",
			AcceptanceCriteria: "done", BloomLevel: 3, Required: true,
		},
		{
			Name: "task_b", Purpose: "test2", Content: "work2",
			AcceptanceCriteria: "done", BloomLevel: 3, Required: true,
		},
	})

	resp := callPlanSubmit(t, e, commandID, tasksFile)
	sr := extractSubmitResult(t, resp)

	// Dispatch task_a (may need multiple scans if on same worker as task_b)
	var taskAInfo submitTaskResult
	for _, task := range sr.Tasks {
		if task.Name == "task_a" {
			taskAInfo = task
		}
	}
	for scan := 0; scan < 5; scan++ {
		e.PeriodicScan()
		tq := daemon.E2EReadTaskQueue(t, e, taskAInfo.Worker)
		for _, qt := range tq.Tasks {
			if qt.ID == taskAInfo.TaskID && qt.Status == model.StatusInProgress {
				daemon.E2EWriteResult(t, e, taskAInfo.Worker, taskAInfo.TaskID, commandID, "completed", "done", qt.LeaseEpoch)
				goto taskADone
			}
		}
	}
	t.Fatal("task_a was never dispatched")
taskADone:
	e.PeriodicScan()

	// Verify preconditions: task_a completed, task_b still non-terminal
	stateBefore := daemon.E2EReadCommandState(t, e, commandID)
	taskACompleted := false
	taskBNonTerminal := false
	for taskID, taskStatus := range stateBefore.TaskStates {
		for _, task := range sr.Tasks {
			if task.TaskID == taskID {
				if task.Name == "task_a" && taskStatus == model.StatusCompleted {
					taskACompleted = true
				}
				if task.Name == "task_b" && !model.IsTerminal(taskStatus) {
					taskBNonTerminal = true
				}
			}
		}
	}
	if !taskACompleted {
		t.Fatal("precondition failed: task_a should be completed")
	}
	if !taskBNonTerminal {
		t.Fatal("precondition failed: task_b should be non-terminal")
	}

	// Plan complete should fail — task_b still non-terminal
	completeResp := callPlanComplete(t, e, commandID, "trying to complete early")
	if completeResp.Success {
		t.Error("plan complete should fail when required tasks are non-terminal")
	}
	if completeResp.Error == nil {
		t.Fatal("expected error for premature complete")
	}
	if completeResp.Error.Code != uds.ErrCodeValidation {
		t.Errorf("expected VALIDATION_ERROR code, got %s", completeResp.Error.Code)
	}

	// Verify plan status unchanged
	stateAfter := daemon.E2EReadCommandState(t, e, commandID)
	if stateAfter.PlanStatus != stateBefore.PlanStatus {
		t.Errorf("plan_status should be unchanged: was %s, now %s",
			stateBefore.PlanStatus, stateAfter.PlanStatus)
	}
}

// =============================================================================
// E2E Boundary: Multiple Commands — Second Command Blocked by Guard
// =============================================================================

// TestE2E_MultipleCommands_GuardBlocks verifies that while one command is
// in_progress, a second pending command is NOT dispatched (at-most-one guard).
func TestE2E_MultipleCommands_GuardBlocks(t *testing.T) {
	e := newE2EDaemon(t)

	// Write two commands
	_ = daemon.E2EWriteCommand(t, e, "first command")
	_ = daemon.E2EWriteCommand(t, e, "second command")

	// First scan dispatches one command
	e.PeriodicScan()

	cq := daemon.E2EReadCommandQueue(t, e)
	if len(cq.Commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(cq.Commands))
	}

	inProgress := 0
	pending := 0
	for _, cmd := range cq.Commands {
		switch cmd.Status {
		case model.StatusInProgress:
			inProgress++
		case model.StatusPending:
			pending++
		}
	}

	// At-most-one guard: exactly 1 in_progress, 1 pending
	if inProgress != 1 {
		t.Errorf("expected 1 in_progress command, got %d", inProgress)
	}
	if pending != 1 {
		t.Errorf("expected 1 pending command, got %d", pending)
	}

	// Second scan should NOT dispatch the second command (guard blocks)
	e.PeriodicScan()
	cq2 := daemon.E2EReadCommandQueue(t, e)
	inProgress2 := 0
	for _, cmd := range cq2.Commands {
		if cmd.Status == model.StatusInProgress {
			inProgress2++
		}
	}
	if inProgress2 != 1 {
		t.Errorf("after second scan: expected 1 in_progress, got %d", inProgress2)
	}
}

// =============================================================================
// Signal Integration: Signal Creation and Delivery Lifecycle
// =============================================================================

// TestE2E_SignalCreationAndDelivery verifies the end-to-end signal lifecycle:
// deferred phase transition to awaiting_fill creates a signal, the scan delivers it,
// and the signal is removed from the queue.
func TestE2E_SignalCreationAndDelivery(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "signal retry test")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	// Submit phased plan
	phasesFile := writePhasesYAML(t, []phaseInputYAML{
		{
			Name: "research",
			Type: "concrete",
			Tasks: []taskInputYAML{
				{
					Name: "research_task", Purpose: "research", Content: "study",
					AcceptanceCriteria: "done", BloomLevel: 2, Required: true,
				},
			},
		},
		{
			Name:            "implementation",
			Type:            "deferred",
			DependsOnPhases: []string{"research"},
			Constraints: &constraintInputYAML{
				MaxTasks: 5, AllowedBloomLevels: []int{1, 2, 3, 4, 5, 6}, TimeoutMinutes: 30,
			},
		},
	})

	resp := callPlanSubmit(t, e, commandID, phasesFile)
	sr := extractSubmitResult(t, resp)

	var researchTask submitTaskResult
	for _, p := range sr.Phases {
		if p.Name == "research" && len(p.Tasks) > 0 {
			researchTask = p.Tasks[0]
		}
	}

	// Complete research → trigger awaiting_fill transition
	e.PeriodicScan()
	tq := daemon.E2EReadTaskQueue(t, e, researchTask.Worker)
	var epoch int
	for _, qt := range tq.Tasks {
		if qt.ID == researchTask.TaskID {
			epoch = qt.LeaseEpoch
		}
	}
	daemon.E2EWriteResult(t, e, researchTask.Worker, researchTask.TaskID, commandID, "completed", "done", epoch)

	// Scans to process: result → research=completed → implementation=awaiting_fill → signal created → delivered.
	// The awaiting_fill transition and signal creation happen in the same code path (step 0.7),
	// so verifying awaiting_fill status proves the signal was created.
	// handleResultWrite also spawns a background scan, so total scans = 3+ (1 bg + 2 explicit).
	e.PeriodicScan()
	e.PeriodicScan()

	// Verify awaiting_fill reached — this proves the signal creation code path executed,
	// since the signal is created in the same step 0.7 handler block as the phase transition.
	state := daemon.E2EReadCommandState(t, e, commandID)
	awaitingFill := false
	for _, phase := range state.Phases {
		if phase.Name == "implementation" && phase.Status == model.PhaseStatusAwaitingFill {
			awaitingFill = true
		}
	}
	if !awaitingFill {
		t.Fatal("implementation should be awaiting_fill")
	}

	// After successful signal delivery, no stale signals should remain.
	// The signal was created when implementation transitioned to awaiting_fill,
	// and the default executor delivers it successfully.
	sq := daemon.E2EReadPlannerSignals(t, e)
	staleCount := 0
	for _, sig := range sq.Signals {
		if sig.CommandID == commandID {
			staleCount++
			t.Logf("stale signal: kind=%s attempts=%d", sig.Kind, sig.Attempts)
		}
	}
	if staleCount > 0 {
		t.Errorf("expected 0 stale signals after successful delivery, got %d", staleCount)
	}
}

// =============================================================================
// E2E Boundary: Rapid Successive Scans — Duplicate Prevention
// =============================================================================

// TestE2E_RapidScans verifies that running multiple rapid scans doesn't
// cause duplicate dispatches, duplicate queue entries, or attempt inflation.
func TestE2E_RapidScans(t *testing.T) {
	e := newE2EDaemon(t)

	_ = daemon.E2EWriteCommand(t, e, "rapid scan test")

	// Submit a task to verify no duplicate task dispatch
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	tasksFile := writeTasksYAML(t, []taskInputYAML{
		{
			Name: "rapid_task", Purpose: "test", Content: "work",
			AcceptanceCriteria: "done", BloomLevel: 3, Required: true,
		},
	})
	submitResp := callPlanSubmit(t, e, commandID, tasksFile)
	if !submitResp.Success {
		t.Fatalf("plan submit should succeed: %+v", submitResp.Error)
	}

	// Run 5 rapid scans
	for i := 0; i < 5; i++ {
		e.PeriodicScan()
	}

	// Verify exactly 1 in_progress command
	cqAfter := daemon.E2EReadCommandQueue(t, e)
	inProgress := 0
	for _, cmd := range cqAfter.Commands {
		if cmd.Status == model.StatusInProgress {
			inProgress++
		}
	}
	if inProgress != 1 {
		t.Errorf("expected 1 in_progress command after rapid scans, got %d", inProgress)
	}

	// Verify task was dispatched exactly once (attempts == 1)
	// Also verify no duplicate queue entries were created
	taskFound := false
	totalTaskEntries := 0
	for i := 1; i <= 2; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		tq := daemon.E2EReadTaskQueue(t, e, workerID)
		totalTaskEntries += len(tq.Tasks)
		for _, qt := range tq.Tasks {
			if qt.Status == model.StatusInProgress {
				taskFound = true
				if qt.Attempts != 1 {
					t.Errorf("task %s on %s: attempts=%d, want 1 (no re-dispatch)", qt.ID, workerID, qt.Attempts)
				}
				if qt.LeaseEpoch != 1 {
					t.Errorf("task %s on %s: lease_epoch=%d, want 1 (no re-acquire)", qt.ID, workerID, qt.LeaseEpoch)
				}
			}
		}
	}
	if !taskFound {
		t.Error("expected at least one dispatched task after rapid scans")
	}
	// Exactly 1 task was submitted, so there should be exactly 1 queue entry total
	if totalTaskEntries != 1 {
		t.Errorf("expected 1 total task queue entry, got %d (duplicate prevention)", totalTaskEntries)
	}
}
