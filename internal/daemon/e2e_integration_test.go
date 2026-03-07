//go:build integration

package daemon_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/bridge"
	"github.com/msageha/maestro_v2/internal/daemon"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// --- YAML input types (mirrors plan.SubmitInput/TaskInput/PhaseInput to avoid import cycle) ---

type taskInputYAML struct {
	Name               string   `yaml:"name"`
	Purpose            string   `yaml:"purpose"`
	Content            string   `yaml:"content"`
	AcceptanceCriteria string   `yaml:"acceptance_criteria"`
	Constraints        []string `yaml:"constraints,omitempty"`
	BlockedBy          []string `yaml:"blocked_by,omitempty"`
	BloomLevel         int      `yaml:"bloom_level"`
	Required           bool     `yaml:"required"`
	ToolsHint          []string `yaml:"tools_hint,omitempty"`
}

type constraintInputYAML struct {
	MaxTasks           int   `yaml:"max_tasks,omitempty"`
	AllowedBloomLevels []int `yaml:"allowed_bloom_levels,omitempty"`
	TimeoutMinutes     int   `yaml:"timeout_minutes,omitempty"`
}

type phaseInputYAML struct {
	Name            string                `yaml:"name"`
	Type            string                `yaml:"type"`
	DependsOnPhases []string              `yaml:"depends_on_phases,omitempty"`
	Tasks           []taskInputYAML       `yaml:"tasks,omitempty"`
	Constraints     *constraintInputYAML  `yaml:"constraints,omitempty"`
}

type submitInputYAML struct {
	Tasks  []taskInputYAML  `yaml:"tasks,omitempty"`
	Phases []phaseInputYAML `yaml:"phases,omitempty"`
}

// --- JSON response types (mirrors plan.SubmitResult) ---

type submitResult struct {
	Valid     bool                `json:"valid,omitempty"`
	CommandID string             `json:"command_id,omitempty"`
	Tasks     []submitTaskResult `json:"tasks,omitempty"`
	Phases    []submitPhaseResult `json:"phases,omitempty"`
}

type submitTaskResult struct {
	Name   string `json:"name"`
	TaskID string `json:"task_id"`
	Worker string `json:"worker"`
	Model  string `json:"model"`
}

type submitPhaseResult struct {
	Name    string             `json:"name"`
	PhaseID string             `json:"phase_id"`
	Type    string             `json:"type"`
	Status  string             `json:"status"`
	Tasks   []submitTaskResult `json:"tasks,omitempty"`
}

// --- E2E helpers ---

func newE2EDaemon(t *testing.T) *daemon.E2EDaemon {
	t.Helper()
	e := daemon.NewE2ETestDaemon(t)
	pe := &bridge.PlanExecutorImpl{
		MaestroDir: e.MaestroDir(),
		Config:     e.Config(),
		LockMap:    e.LockMap(),
	}
	e.D.SetPlanExecutor(pe)
	return e
}

func writeTasksYAML(t *testing.T, tasks []taskInputYAML) string {
	t.Helper()
	input := submitInputYAML{Tasks: tasks}
	data, err := yamlv3.Marshal(input)
	if err != nil {
		t.Fatalf("marshal tasks: %v", err)
	}
	path := filepath.Join(t.TempDir(), "tasks.yaml")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write tasks file: %v", err)
	}
	return path
}

func writePhasesYAML(t *testing.T, phases []phaseInputYAML) string {
	t.Helper()
	input := submitInputYAML{Phases: phases}
	data, err := yamlv3.Marshal(input)
	if err != nil {
		t.Fatalf("marshal phases: %v", err)
	}
	path := filepath.Join(t.TempDir(), "phases.yaml")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write phases file: %v", err)
	}
	return path
}

func callPlanSubmit(t *testing.T, e *daemon.E2EDaemon, commandID, tasksFile string) *uds.Response {
	t.Helper()
	data, _ := json.Marshal(map[string]string{
		"command_id": commandID,
		"tasks_file": tasksFile,
	})
	params, _ := json.Marshal(map[string]json.RawMessage{
		"operation": json.RawMessage(`"submit"`),
		"data":      data,
	})
	return e.HandlePlan(&uds.Request{
		ProtocolVersion: 1,
		Command:         "plan",
		Params:          params,
	})
}

func callPlanSubmitPhase(t *testing.T, e *daemon.E2EDaemon, commandID, tasksFile, phaseName string) *uds.Response {
	t.Helper()
	data, _ := json.Marshal(map[string]string{
		"command_id": commandID,
		"tasks_file": tasksFile,
		"phase_name": phaseName,
	})
	params, _ := json.Marshal(map[string]json.RawMessage{
		"operation": json.RawMessage(`"submit"`),
		"data":      data,
	})
	return e.HandlePlan(&uds.Request{
		ProtocolVersion: 1,
		Command:         "plan",
		Params:          params,
	})
}

func callPlanComplete(t *testing.T, e *daemon.E2EDaemon, commandID, summary string) *uds.Response {
	t.Helper()
	data, _ := json.Marshal(map[string]string{
		"command_id": commandID,
		"summary":    summary,
	})
	params, _ := json.Marshal(map[string]json.RawMessage{
		"operation": json.RawMessage(`"complete"`),
		"data":      data,
	})
	return e.HandlePlan(&uds.Request{
		ProtocolVersion: 1,
		Command:         "plan",
		Params:          params,
	})
}

func readNotificationQueue(t *testing.T, e *daemon.E2EDaemon) model.NotificationQueue {
	t.Helper()
	path := filepath.Join(e.MaestroDir(), "queue", "orchestrator.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return model.NotificationQueue{}
	}
	var nq model.NotificationQueue
	yamlv3.Unmarshal(data, &nq)
	return nq
}

func readCommandResultFile(t *testing.T, e *daemon.E2EDaemon) model.CommandResultFile {
	t.Helper()
	path := filepath.Join(e.MaestroDir(), "results", "planner.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return model.CommandResultFile{}
	}
	var rf model.CommandResultFile
	yamlv3.Unmarshal(data, &rf)
	return rf
}

func extractSubmitResult(t *testing.T, resp *uds.Response) submitResult {
	t.Helper()
	if !resp.Success {
		t.Fatalf("plan submit failed: %+v", resp.Error)
	}
	var result submitResult
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("parse submit result: %v", err)
	}
	return result
}

func findTasksByWorker(result submitResult) map[string][]submitTaskResult {
	byWorker := make(map[string][]submitTaskResult)
	for _, t := range result.Tasks {
		byWorker[t.Worker] = append(byWorker[t.Worker], t)
	}
	for _, p := range result.Phases {
		for _, t := range p.Tasks {
			byWorker[t.Worker] = append(byWorker[t.Worker], t)
		}
	}
	return byWorker
}

// --- E2E Test Scenarios ---

// TestE2E_NonPhasedFlow tests the full lifecycle:
// command → planner dispatch → plan submit → task dispatch → result write → plan complete → orchestrator notification
func TestE2E_NonPhasedFlow(t *testing.T) {
	e := newE2EDaemon(t)

	// Step 1: Write command to planner queue
	cmdID := daemon.E2EWriteCommand(t, e, "implement feature X")
	if cmdID == "" {
		t.Fatal("expected command ID from writeCommand")
	}

	// Step 2: Dispatch command to planner
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	if len(cq.Commands) == 0 || cq.Commands[0].Status != model.StatusInProgress {
		t.Fatalf("expected command in_progress after dispatch, got %+v", cq)
	}
	commandID := cq.Commands[0].ID

	// Step 3: Plan submit via handlePlan (real plan.Submit via bridge)
	tasksFile := writeTasksYAML(t, []taskInputYAML{
		{
			Name:               "task_a",
			Purpose:            "implement module A",
			Content:            "write code for module A",
			AcceptanceCriteria: "module A works",
			BloomLevel:         3,
			Required:           true,
		},
		{
			Name:               "task_b",
			Purpose:            "implement module B",
			Content:            "write code for module B",
			AcceptanceCriteria: "module B works",
			BloomLevel:         3,
			Required:           true,
		},
	})

	resp := callPlanSubmit(t, e, commandID, tasksFile)
	sr := extractSubmitResult(t, resp)

	if sr.CommandID != commandID {
		t.Errorf("submit command_id = %q, want %q", sr.CommandID, commandID)
	}
	if len(sr.Tasks) != 2 {
		t.Fatalf("expected 2 tasks in submit result, got %d", len(sr.Tasks))
	}

	// Verify state was created
	state := daemon.E2EReadCommandState(t, e, commandID)
	if state.PlanStatus != model.PlanStatusSealed {
		t.Errorf("plan_status = %q, want sealed", state.PlanStatus)
	}
	if state.ExpectedTaskCount != 2 {
		t.Errorf("expected_task_count = %d, want 2", state.ExpectedTaskCount)
	}

	// Step 4: Dispatch tasks to workers
	e.PeriodicScan()

	byWorker := findTasksByWorker(sr)
	dispatched := false
	for workerID := range byWorker {
		tq := daemon.E2EReadTaskQueue(t, e, workerID)
		for _, task := range tq.Tasks {
			if task.Status == model.StatusInProgress {
				dispatched = true
			}
		}
	}
	if !dispatched {
		t.Fatal("expected at least one task dispatched after scan")
	}

	// Step 5: Write results for both tasks
	for workerID, tasks := range byWorker {
		for _, taskInfo := range tasks {
			tq := daemon.E2EReadTaskQueue(t, e, workerID)
			for _, qt := range tq.Tasks {
				if qt.ID == taskInfo.TaskID {
					resultID := daemon.E2EWriteResult(t, e, workerID, taskInfo.TaskID, commandID, "completed", "done: "+taskInfo.Name, qt.LeaseEpoch)
					if resultID == "" {
						t.Fatalf("expected result ID for task %s", taskInfo.TaskID)
					}
				}
			}
		}
	}

	// Step 6: Scan for state updates
	e.PeriodicScan()

	// Step 7: Plan complete
	completeResp := callPlanComplete(t, e, commandID, "all tasks completed successfully")
	if !completeResp.Success {
		t.Fatalf("plan complete failed: %+v", completeResp.Error)
	}

	var completeResult map[string]string
	json.Unmarshal(completeResp.Data, &completeResult)
	if completeResult["status"] != "completed" {
		t.Errorf("complete status = %q, want completed", completeResult["status"])
	}

	// Step 8: Scan to process notification
	e.PeriodicScan()

	// --- Verification ---

	// V1: State should be completed
	finalState := daemon.E2EReadCommandState(t, e, commandID)
	if finalState.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("final plan_status = %q, want completed", finalState.PlanStatus)
	}
	for taskID, status := range finalState.TaskStates {
		if status != model.StatusCompleted {
			t.Errorf("task %s status = %q, want completed", taskID, status)
		}
	}

	// V2: Command result should exist in results/planner.yaml
	cmdResultFile := readCommandResultFile(t, e)
	found := false
	for _, r := range cmdResultFile.Results {
		if r.CommandID == commandID && r.Status == model.StatusCompleted {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected completed CommandResult in results/planner.yaml")
	}

	// V3: Command queue entry should be completed
	finalCQ := daemon.E2EReadCommandQueue(t, e)
	for _, cmd := range finalCQ.Commands {
		if cmd.ID == commandID {
			if cmd.Status != model.StatusCompleted {
				t.Errorf("command queue status = %q, want completed", cmd.Status)
			}
		}
	}

	// V4: Orchestrator notification should exist
	nq := readNotificationQueue(t, e)
	notifFound := false
	for _, ntf := range nq.Notifications {
		if ntf.CommandID == commandID && ntf.Type == "command_completed" {
			notifFound = true
			break
		}
	}
	if !notifFound {
		t.Error("expected command_completed notification in orchestrator queue")
	}
}

// TestE2E_PhasedFlow tests the multi-phase lifecycle with awaiting_fill:
// command → submit phases → research dispatch → result → phase transition →
// awaiting_fill signal → plan complete (ACTION_REQUIRED) → phase fill →
// implementation dispatch → result → complete → notification
func TestE2E_PhasedFlow(t *testing.T) {
	e := newE2EDaemon(t)

	// Step 1: Write command
	_ = daemon.E2EWriteCommand(t, e, "multi-phase project")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	// Step 2: Submit phased plan (research=concrete, implementation=deferred)
	phasesFile := writePhasesYAML(t, []phaseInputYAML{
		{
			Name: "research",
			Type: "concrete",
			Tasks: []taskInputYAML{
				{
					Name:               "research_task",
					Purpose:            "research the problem",
					Content:            "investigate requirements",
					AcceptanceCriteria: "research complete",
					BloomLevel:         2,
					Required:           true,
				},
			},
		},
		{
			Name:            "implementation",
			Type:            "deferred",
			DependsOnPhases: []string{"research"},
			Constraints: &constraintInputYAML{
				MaxTasks:           5,
				AllowedBloomLevels: []int{1, 2, 3, 4, 5, 6},
				TimeoutMinutes:     30,
			},
		},
	})

	resp := callPlanSubmit(t, e, commandID, phasesFile)
	sr := extractSubmitResult(t, resp)

	// Verify phased submit
	if len(sr.Phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(sr.Phases))
	}

	// Find research task info
	var researchTask submitTaskResult
	for _, p := range sr.Phases {
		if p.Name == "research" {
			if len(p.Tasks) != 1 {
				t.Fatalf("expected 1 research task, got %d", len(p.Tasks))
			}
			researchTask = p.Tasks[0]
		}
	}
	if researchTask.TaskID == "" {
		t.Fatal("research task not found in submit result")
	}

	// Verify state: research=active, implementation=pending
	state := daemon.E2EReadCommandState(t, e, commandID)
	for _, phase := range state.Phases {
		switch phase.Name {
		case "research":
			if phase.Status != model.PhaseStatusActive {
				t.Errorf("research phase status = %q, want active", phase.Status)
			}
		case "implementation":
			if phase.Status != model.PhaseStatusPending {
				t.Errorf("implementation phase status = %q, want pending", phase.Status)
			}
		}
	}

	// Step 3: Dispatch research task
	e.PeriodicScan()

	// Step 4: Complete research task
	tq := daemon.E2EReadTaskQueue(t, e, researchTask.Worker)
	var leaseEpoch int
	for _, qt := range tq.Tasks {
		if qt.ID == researchTask.TaskID {
			leaseEpoch = qt.LeaseEpoch
		}
	}
	daemon.E2EWriteResult(t, e, researchTask.Worker, researchTask.TaskID, commandID, "completed", "research done", leaseEpoch)

	// Step 5: Scan to process result and trigger phase transitions
	e.PeriodicScan()
	e.PeriodicScan()

	// Verify implementation phase is now awaiting_fill
	state = daemon.E2EReadCommandState(t, e, commandID)
	var implAwaitingFill bool
	for _, phase := range state.Phases {
		if phase.Name == "implementation" && phase.Status == model.PhaseStatusAwaitingFill {
			implAwaitingFill = true
		}
	}
	if !implAwaitingFill {
		for _, phase := range state.Phases {
			t.Logf("phase %q status=%s", phase.Name, phase.Status)
		}
		t.Fatal("expected implementation phase to be awaiting_fill")
	}

	// Step 6: Signal queue verification
	// The mock executor delivers signals successfully within the same PeriodicScan cycle,
	// so the signal is created (step 0.7), delivered (step 0.8), and removed from the queue.
	// Assert that no stale signals remain for this command (confirms delivery succeeded).
	// Signal creation and delivery mechanics are tested separately in signal_integration_test.go.
	sq := daemon.E2EReadPlannerSignals(t, e)
	for _, sig := range sq.Signals {
		if sig.CommandID == commandID {
			t.Errorf("expected no pending signals for command %s after successful delivery, found kind=%s attempts=%d",
				commandID, sig.Kind, sig.Attempts)
		}
	}

	// Step 7: Plan complete should return ACTION_REQUIRED
	completeResp := callPlanComplete(t, e, commandID, "trying to complete")
	if completeResp.Success {
		t.Fatal("plan complete should fail with ACTION_REQUIRED for awaiting_fill phase")
	}
	if completeResp.Error.Code != uds.ErrCodeActionRequired {
		t.Errorf("error code = %q, want ACTION_REQUIRED", completeResp.Error.Code)
	}

	// Step 8: Phase fill — submit implementation tasks
	implTasksFile := writeTasksYAML(t, []taskInputYAML{
		{
			Name:               "impl_task",
			Purpose:            "implement the feature",
			Content:            "write the implementation",
			AcceptanceCriteria: "feature works",
			BloomLevel:         3,
			Required:           true,
		},
	})

	fillResp := callPlanSubmitPhase(t, e, commandID, implTasksFile, "implementation")
	fillResult := extractSubmitResult(t, fillResp)
	if len(fillResult.Tasks) != 1 {
		t.Fatalf("expected 1 impl task, got %d", len(fillResult.Tasks))
	}
	implTask := fillResult.Tasks[0]

	// Verify implementation phase is now active
	state = daemon.E2EReadCommandState(t, e, commandID)
	for _, phase := range state.Phases {
		if phase.Name == "implementation" {
			if phase.Status != model.PhaseStatusActive {
				t.Errorf("implementation phase status = %q, want active after fill", phase.Status)
			}
		}
	}

	// Step 9: Dispatch implementation task
	e.PeriodicScan()

	// Step 10: Complete implementation task
	tq = daemon.E2EReadTaskQueue(t, e, implTask.Worker)
	leaseEpoch = 0
	for _, qt := range tq.Tasks {
		if qt.ID == implTask.TaskID {
			leaseEpoch = qt.LeaseEpoch
		}
	}
	daemon.E2EWriteResult(t, e, implTask.Worker, implTask.TaskID, commandID, "completed", "implementation done", leaseEpoch)

	// Step 11: Scan for phase completion
	e.PeriodicScan()
	e.PeriodicScan()

	// Step 12: Plan complete should succeed now
	completeResp = callPlanComplete(t, e, commandID, "all phases completed")
	if !completeResp.Success {
		t.Fatalf("plan complete failed: %+v", completeResp.Error)
	}

	// Step 13: Scan for notification
	e.PeriodicScan()

	// --- Verification ---

	// V1: State completed
	finalState := daemon.E2EReadCommandState(t, e, commandID)
	if finalState.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("final plan_status = %q, want completed", finalState.PlanStatus)
	}

	// V2: All phases terminal
	for _, phase := range finalState.Phases {
		if !model.IsPhaseTerminal(phase.Status) {
			t.Errorf("phase %q status = %q, want terminal", phase.Name, phase.Status)
		}
	}

	// V3: Orchestrator notification exists
	nq := readNotificationQueue(t, e)
	notifFound := false
	for _, ntf := range nq.Notifications {
		if ntf.CommandID == commandID && ntf.Type == "command_completed" {
			notifFound = true
		}
	}
	if !notifFound {
		t.Error("expected command_completed notification in orchestrator queue")
	}

	// V4: Signal queue should be empty (stale signal removed)
	sqFinal := daemon.E2EReadPlannerSignals(t, e)
	for _, sig := range sqFinal.Signals {
		if sig.CommandID == commandID {
			t.Errorf("expected no signals for command %s, found kind=%s", commandID, sig.Kind)
		}
	}
}

// TestE2E_FailureFlow tests the failure lifecycle:
// command → submit → task dispatch → one task fails → plan complete (failed) → notification
func TestE2E_FailureFlow(t *testing.T) {
	e := newE2EDaemon(t)

	// Step 1: Write command + dispatch
	_ = daemon.E2EWriteCommand(t, e, "feature with failure")
	e.PeriodicScan()
	cq := daemon.E2EReadCommandQueue(t, e)
	commandID := cq.Commands[0].ID

	// Step 2: Submit two tasks
	tasksFile := writeTasksYAML(t, []taskInputYAML{
		{
			Name:               "good_task",
			Purpose:            "this will succeed",
			Content:            "easy work",
			AcceptanceCriteria: "passes",
			BloomLevel:         2,
			Required:           true,
		},
		{
			Name:               "bad_task",
			Purpose:            "this will fail",
			Content:            "impossible work",
			AcceptanceCriteria: "impossible criteria",
			BloomLevel:         3,
			Required:           true,
		},
	})

	resp := callPlanSubmit(t, e, commandID, tasksFile)
	sr := extractSubmitResult(t, resp)

	// Step 3: Dispatch
	e.PeriodicScan()

	// Step 4: Write results — one completed, one failed
	byWorker := findTasksByWorker(sr)
	for workerID, tasks := range byWorker {
		tq := daemon.E2EReadTaskQueue(t, e, workerID)
		for _, taskInfo := range tasks {
			for _, qt := range tq.Tasks {
				if qt.ID == taskInfo.TaskID {
					status := "completed"
					summary := "success"
					if taskInfo.Name == "bad_task" {
						status = "failed"
						summary = "task failed: impossible"
					}
					daemon.E2EWriteResult(t, e, workerID, taskInfo.TaskID, commandID, status, summary, qt.LeaseEpoch)
				}
			}
		}
	}

	// Step 5: Scan for state updates
	e.PeriodicScan()

	// Step 6: Plan complete — should derive failed status
	completeResp := callPlanComplete(t, e, commandID, "completed with failure")
	if !completeResp.Success {
		t.Fatalf("plan complete should succeed (derive failed status): %+v", completeResp.Error)
	}

	var completeResult map[string]string
	json.Unmarshal(completeResp.Data, &completeResult)
	if completeResult["status"] != "failed" {
		t.Errorf("complete status = %q, want failed", completeResult["status"])
	}

	// Step 7: Scan for notification
	e.PeriodicScan()

	// --- Verification ---

	// V1: State should be failed
	finalState := daemon.E2EReadCommandState(t, e, commandID)
	if finalState.PlanStatus != model.PlanStatusFailed {
		t.Errorf("final plan_status = %q, want failed", finalState.PlanStatus)
	}

	// V2: Orchestrator notification should be command_failed
	nq := readNotificationQueue(t, e)
	notifFound := false
	for _, ntf := range nq.Notifications {
		if ntf.CommandID == commandID && ntf.Type == "command_failed" {
			notifFound = true
		}
	}
	if !notifFound {
		t.Error("expected command_failed notification in orchestrator queue")
	}
}
