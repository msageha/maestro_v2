package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

func testConfig() model.Config {
	return model.Config{
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{
				Count:        2,
				DefaultModel: "sonnet",
				Models:       map[string]string{"worker2": "opus"},
			},
		},
		Limits: model.LimitsConfig{
			MaxPendingTasksPerWorker: 10,
		},
	}
}

func setupMaestroDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	maestroDir := filepath.Join(base, ".maestro")

	dirs := []string{
		filepath.Join(maestroDir, "queue"),
		filepath.Join(maestroDir, "results"),
		filepath.Join(maestroDir, "state", "commands"),
		filepath.Join(maestroDir, "logs"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Create empty worker queue files (1-indexed to match setup convention)
	for i := 1; i <= 2; i++ {
		tq := model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks:         []model.Task{},
		}
		queueFile := filepath.Join(maestroDir, "queue", fmt.Sprintf("worker%d.yaml", i))
		if err := yamlutil.AtomicWrite(queueFile, tq); err != nil {
			t.Fatalf("write worker queue %d: %v", i, err)
		}
	}

	return maestroDir
}

func workerQueueFilename(index int) string {
	return fmt.Sprintf("worker%d.yaml", index)
}

func writePlannerQueue(t *testing.T, maestroDir string, commandID string, status model.Status) {
	t.Helper()
	leaseOwner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:         commandID,
				Content:    "test command",
				Priority:   100,
				Status:     status,
				LeaseOwner: &leaseOwner,
				CreatedAt:  "2025-01-01T00:00:00Z",
				UpdatedAt:  "2025-01-01T00:00:00Z",
			},
		},
	}
	path := filepath.Join(maestroDir, "queue", "planner.yaml")
	if err := yamlutil.AtomicWrite(path, cq); err != nil {
		t.Fatalf("write planner queue: %v", err)
	}
}

func writeTasksFile(t *testing.T, tasks []TaskInput) string {
	t.Helper()
	input := SubmitInput{Tasks: tasks}
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

func writePhasesFile(t *testing.T, phases []PhaseInput) string {
	t.Helper()
	input := SubmitInput{Phases: phases}
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

func TestSubmit_BasicTasks(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	commandID := "cmd_0000000001_aabbccdd"

	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	tasksFile := writeTasksFile(t, []TaskInput{
		{
			Name:               "task_a",
			Purpose:            "do task a",
			Content:            "implement feature a",
			AcceptanceCriteria: "feature a works",
			BloomLevel:         2,
			Required:           true,
		},
		{
			Name:               "task_b",
			Purpose:            "do task b",
			Content:            "implement feature b",
			AcceptanceCriteria: "feature b works",
			BloomLevel:         1,
			Required:           true,
		},
	})

	result, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		MaestroDir: maestroDir,
		Config:     cfg,
	})
	if err != nil {
		t.Fatalf("Submit returned error: %v", err)
	}

	if result.CommandID != commandID {
		t.Errorf("CommandID = %q, want %q", result.CommandID, commandID)
	}
	if len(result.Tasks) != 2 {
		t.Fatalf("len(Tasks) = %d, want 2", len(result.Tasks))
	}

	// Verify task names
	nameSet := make(map[string]bool)
	for _, tr := range result.Tasks {
		nameSet[tr.Name] = true
		if tr.TaskID == "" {
			t.Errorf("task %q has empty TaskID", tr.Name)
		}
		if tr.Worker == "" {
			t.Errorf("task %q has empty Worker", tr.Name)
		}
		if tr.Model == "" {
			t.Errorf("task %q has empty Model", tr.Name)
		}
		if !model.ValidateID(tr.TaskID) {
			t.Errorf("task %q has invalid TaskID format: %s", tr.Name, tr.TaskID)
		}
	}
	if !nameSet["task_a"] || !nameSet["task_b"] {
		t.Errorf("expected tasks task_a and task_b, got %v", nameSet)
	}

	// Verify state file was created with sealed status
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.PlanStatus != model.PlanStatusSealed {
		t.Errorf("PlanStatus = %q, want %q", state.PlanStatus, model.PlanStatusSealed)
	}
	if state.PlanVersion != 1 {
		t.Errorf("PlanVersion = %d, want 1", state.PlanVersion)
	}
	if state.ExpectedTaskCount != 2 {
		t.Errorf("ExpectedTaskCount = %d, want 2", state.ExpectedTaskCount)
	}
	if len(state.RequiredTaskIDs) != 2 {
		t.Errorf("len(RequiredTaskIDs) = %d, want 2", len(state.RequiredTaskIDs))
	}

	// Verify queue entries were written to at least one worker queue (1-indexed)
	totalQueueTasks := 0
	for i := 1; i <= 2; i++ {
		queueFile := filepath.Join(maestroDir, "queue", workerQueueFilename(i))
		data, err := os.ReadFile(queueFile)
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			continue
		}
		totalQueueTasks += len(tq.Tasks)
	}
	if totalQueueTasks != 2 {
		t.Errorf("total queue tasks = %d, want 2", totalQueueTasks)
	}
}

func TestSubmit_DryRun(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	commandID := "cmd_0000000002_aabbccdd"

	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	tasksFile := writeTasksFile(t, []TaskInput{
		{
			Name:               "task_dry",
			Purpose:            "dry run task",
			Content:            "just validate",
			AcceptanceCriteria: "valid input",
			BloomLevel:         1,
			Required:           true,
		},
	})

	result, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		DryRun:     true,
		MaestroDir: maestroDir,
		Config:     cfg,
	})
	if err != nil {
		t.Fatalf("Submit dry-run returned error: %v", err)
	}

	if !result.Valid {
		t.Errorf("result.Valid = false, want true")
	}

	// No state file should be created
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if _, err := os.Stat(statePath); err == nil {
		t.Errorf("state file exists at %s, want no state file for dry-run", statePath)
	}
}

func TestSubmit_DoubleSubmit(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	commandID := "cmd_0000000003_aabbccdd"

	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	tasksFile := writeTasksFile(t, []TaskInput{
		{
			Name:               "task_first",
			Purpose:            "first submit",
			Content:            "initial implementation",
			AcceptanceCriteria: "works correctly",
			BloomLevel:         2,
			Required:           true,
		},
	})

	// First submit should succeed
	_, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		MaestroDir: maestroDir,
		Config:     cfg,
	})
	if err != nil {
		t.Fatalf("first Submit returned error: %v", err)
	}

	// Second submit with same command ID should fail
	tasksFile2 := writeTasksFile(t, []TaskInput{
		{
			Name:               "task_second",
			Purpose:            "second submit",
			Content:            "duplicate implementation",
			AcceptanceCriteria: "should fail",
			BloomLevel:         1,
			Required:           true,
		},
	})

	_, err = Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile2,
		MaestroDir: maestroDir,
		Config:     cfg,
	})
	if err == nil {
		t.Fatalf("second Submit returned nil error, want double submit error")
	}
	if !strings.Contains(err.Error(), "double submit") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "double submit")
	}
}

func TestSubmit_CancelledCommand(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	commandID := "cmd_0000000004_aabbccdd"

	// Write planner queue with cancelled status
	writePlannerQueue(t, maestroDir, commandID, model.StatusCancelled)

	tasksFile := writeTasksFile(t, []TaskInput{
		{
			Name:               "task_cancelled",
			Purpose:            "should not submit",
			Content:            "cancelled command",
			AcceptanceCriteria: "should fail",
			BloomLevel:         1,
			Required:           true,
		},
	})

	_, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		MaestroDir: maestroDir,
		Config:     cfg,
	})
	if err == nil {
		t.Fatalf("Submit returned nil error, want cancelled error")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "cancelled")
	}
}

func TestSubmit_PhasedSubmit(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	commandID := "cmd_0000000005_aabbccdd"

	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	phasesFile := writePhasesFile(t, []PhaseInput{
		{
			Name: "phase_build",
			Type: "concrete",
			Tasks: []TaskInput{
				{
					Name:               "compile",
					Purpose:            "compile the project",
					Content:            "run go build",
					AcceptanceCriteria: "builds successfully",
					BloomLevel:         2,
					Required:           true,
				},
			},
		},
		{
			Name:            "phase_review",
			Type:            "deferred",
			DependsOnPhases: []string{"phase_build"},
			Constraints: &ConstraintInput{
				MaxTasks:           5,
				TimeoutMinutes:     30,
				AllowedBloomLevels: []int{1, 2, 3},
			},
		},
	})

	result, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  phasesFile,
		MaestroDir: maestroDir,
		Config:     cfg,
	})
	if err != nil {
		t.Fatalf("Submit phased returned error: %v", err)
	}

	if result.CommandID != commandID {
		t.Errorf("CommandID = %q, want %q", result.CommandID, commandID)
	}
	if len(result.Phases) != 2 {
		t.Fatalf("len(Phases) = %d, want 2", len(result.Phases))
	}

	// Verify concrete phase
	buildPhase := result.Phases[0]
	if buildPhase.Name != "phase_build" {
		t.Errorf("Phases[0].Name = %q, want %q", buildPhase.Name, "phase_build")
	}
	if buildPhase.Type != "concrete" {
		t.Errorf("Phases[0].Type = %q, want %q", buildPhase.Type, "concrete")
	}
	if buildPhase.Status != string(model.PhaseStatusActive) {
		t.Errorf("Phases[0].Status = %q, want %q", buildPhase.Status, model.PhaseStatusActive)
	}
	if len(buildPhase.Tasks) != 1 {
		t.Fatalf("Phases[0].Tasks count = %d, want 1", len(buildPhase.Tasks))
	}
	if buildPhase.Tasks[0].Name != "compile" {
		t.Errorf("Phases[0].Tasks[0].Name = %q, want %q", buildPhase.Tasks[0].Name, "compile")
	}
	if !model.ValidateID(buildPhase.PhaseID) {
		t.Errorf("Phases[0].PhaseID has invalid format: %s", buildPhase.PhaseID)
	}

	// Verify deferred phase
	reviewPhase := result.Phases[1]
	if reviewPhase.Name != "phase_review" {
		t.Errorf("Phases[1].Name = %q, want %q", reviewPhase.Name, "phase_review")
	}
	if reviewPhase.Type != "deferred" {
		t.Errorf("Phases[1].Type = %q, want %q", reviewPhase.Type, "deferred")
	}
	if reviewPhase.Status != string(model.PhaseStatusPending) {
		t.Errorf("Phases[1].Status = %q, want %q", reviewPhase.Status, model.PhaseStatusPending)
	}
	if len(reviewPhase.Tasks) != 0 {
		t.Errorf("Phases[1].Tasks count = %d, want 0 (deferred)", len(reviewPhase.Tasks))
	}

	// Verify state file
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.PlanStatus != model.PlanStatusSealed {
		t.Errorf("PlanStatus = %q, want %q", state.PlanStatus, model.PlanStatusSealed)
	}
	if len(state.Phases) != 2 {
		t.Fatalf("len(state.Phases) = %d, want 2", len(state.Phases))
	}
	if state.Phases[0].Status != model.PhaseStatusActive {
		t.Errorf("state.Phases[0].Status = %q, want %q", state.Phases[0].Status, model.PhaseStatusActive)
	}
	if state.Phases[1].Status != model.PhaseStatusPending {
		t.Errorf("state.Phases[1].Status = %q, want %q", state.Phases[1].Status, model.PhaseStatusPending)
	}
	if state.Phases[1].Constraints == nil {
		t.Fatalf("state.Phases[1].Constraints is nil, want non-nil")
	}
	if state.Phases[1].Constraints.MaxTasks != 5 {
		t.Errorf("state.Phases[1].Constraints.MaxTasks = %d, want 5", state.Phases[1].Constraints.MaxTasks)
	}
}
