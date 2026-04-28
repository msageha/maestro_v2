package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
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
		// Match the production template default (worktree.enabled=true)
		// so submit-side tests do not see __system_commit injected by
		// shouldInsertSystemCommit, which now fires whenever worktree mode
		// is off (2026-04-28: closed the dirty-main footgun for single-shot
		// runs). Tests that intentionally exercise worktree=false override
		// this field explicitly.
		Worktree: model.WorktreeConfig{Enabled: true},
	}
}

func setupMaestroDir(t *testing.T) string {
	t.Helper()
	return testutil.SetupDirWithQueues(t, 2)
}

// withDefaultRequiredFields fills in ExpectedPaths and DefinitionOfAbort with
// safe defaults for test fixtures that don't exercise those fields. Production
// validation rejects missing values, so tests that need valid tasks must declare
// them explicitly or use this helper.
func withDefaultRequiredFields(tasks []TaskInput) []TaskInput {
	for i := range tasks {
		// "." survives YAML round-trip with `omitempty` (empty slices are
		// stripped) and passes validateExpectedPaths (relative, no traversal,
		// non-empty).
		if tasks[i].ExpectedPaths == nil {
			tasks[i].ExpectedPaths = []string{"."}
		}
		if tasks[i].DefinitionOfAbort == nil {
			d := model.DefaultDefinitionOfAbort()
			tasks[i].DefinitionOfAbort = &d
		}
	}
	return tasks
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
	// Auto-fill ExpectedPaths/DefinitionOfAbort for fixtures that don't
	// exercise those fields. Validation-rejection tests for nil values do
	// not go through this helper, so they stay unaffected.
	tasks = withDefaultRequiredFields(tasks)
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
	// Auto-fill ExpectedPaths/DefinitionOfAbort for tasks inside phases.
	for i := range phases {
		phases[i].Tasks = withDefaultRequiredFields(phases[i].Tasks)
	}
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

func TestParseInput_RejectsUnknownField(t *testing.T) {
	_, err := parseInput([]byte(`
tasks:
  - name: task_a
    purpose: do task a
    content: implement feature a
    acceptance_criteria: feature a works
    bloom_level: 2
    required: true
    expected_paths: ["."]
    definition_of_abort:
      max_repair_count: 2
      max_wall_clock_sec: 300
    acceptence_criteria: typo should be rejected
`))
	if err == nil {
		t.Fatal("expected unknown field error")
	}
	if !strings.Contains(err.Error(), "field acceptence_criteria not found") {
		t.Fatalf("expected strict YAML unknown-field error, got: %v", err)
	}
	// 2026-04-28 follow-up: the typed wrapper is what makes the daemon's
	// plan handler route this error to ErrCodeValidation. Before this,
	// `field XXX not found` surfaced as `[INTERNAL_ERROR]` because the
	// plain fmt.Errorf wrap missed the type assertion in
	// daemonapi/plan.go. Pinning the *planValidationError shape here so
	// future refactors don't silently regress the operator-facing error
	// classification.
	var pve *planValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *planValidationError so daemon classifies as VALIDATION_ERROR, got %T: %v", err, err)
	}
}

// TestSubmit_DryRunRejectsUnknownWorkerID pins the 2026-04-28 retest2
// follow-up: `plan submit --dry-run` was returning {"valid": true} for
// a task tagged `worker_id: worker99` even when only 2 workers were
// configured. The format-only check inside validateTaskFieldsCore could
// not catch this — `worker99` matches the workerN regex but does not
// reference a real slot. The fix runs validateTaskWorkerPins before the
// dry-run early return so the operator sees the existence error from
// the same place the real submit reports it.
func TestSubmit_DryRunRejectsUnknownWorkerID(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig() // workers.count=2

	writePlannerQueue(t, maestroDir, "cmd_test_dry_pin", model.StatusInProgress)

	tasksFile := writeTasksFile(t, withDefaultRequiredFields([]TaskInput{
		{
			Name:               "feature_a",
			Purpose:            "do thing",
			Content:            "implement",
			AcceptanceCriteria: "passes",
			BloomLevel:         2,
			Required:           true,
			WorkerID:           "worker99", // beyond configured count
		},
	}))

	_, err := Submit(SubmitOptions{
		CommandID:  "cmd_test_dry_pin",
		TasksFile:  tasksFile,
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
		DryRun:     true,
	})
	if err == nil {
		t.Fatal("expected dry-run to reject worker_id beyond workers.count")
	}
	var verrs *ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("expected *ValidationErrors so daemon classifies as VALIDATION_ERROR, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "worker99") {
		t.Errorf("error %q does not name the offending worker_id", err.Error())
	}
}

// TestParseInput_AcceptsWorkerID pins that the worker_id pinning field
// added 2026-04-28 actually decodes off the YAML wire. Before this, a
// Planner-supplied worker_id was rejected by SafeUnmarshalStrict as
// "unknown field" and the call surfaced as INTERNAL_ERROR — the exact
// regression flagged in the conflict-recovery E2E run.
func TestParseInput_AcceptsWorkerID(t *testing.T) {
	parsed, err := parseInput([]byte(`
tasks:
  - name: feature_a
    purpose: ship feature a
    content: implement A
    acceptance_criteria: A passes tests
    bloom_level: 2
    required: true
    worker_id: worker1
    expected_paths: ["."]
    definition_of_abort:
      max_repair_count: 2
      max_wall_clock_sec: 300
`))
	if err != nil {
		t.Fatalf("parseInput: %v", err)
	}
	if len(parsed.Tasks) != 1 || parsed.Tasks[0].WorkerID != "worker1" {
		t.Fatalf("worker_id not propagated: tasks=%+v", parsed.Tasks)
	}
}

// TestParseInput_OversizedInputIsValidationError pins that input size
// rejection is also surfaced as a validation error, not an internal one.
// Before the 2026-04-28 fix, this case used plain fmt.Errorf and the
// daemon mislabelled it as INTERNAL_ERROR — same misclassification as the
// strict-decode path.
func TestParseInput_OversizedInputIsValidationError(t *testing.T) {
	oversized := make([]byte, model.DefaultMaxYAMLFileBytes+1)
	for i := range oversized {
		oversized[i] = ' '
	}
	_, err := parseInput(oversized)
	if err == nil {
		t.Fatal("expected size-limit error")
	}
	var pve *planValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *planValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("error %q does not mention size limit", err.Error())
	}
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
		LockMap:    lock.NewMutexMap(),
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
		LockMap:    lock.NewMutexMap(),
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
		LockMap:    lock.NewMutexMap(),
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
		LockMap:    lock.NewMutexMap(),
	})
	if err == nil {
		t.Fatalf("second Submit returned nil error, want double submit error")
	}
	if !errors.Is(err, ErrDoubleSubmit) {
		t.Errorf("error = %v, want ErrDoubleSubmit in chain", err)
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
		LockMap:    lock.NewMutexMap(),
	})
	if err == nil {
		t.Fatalf("Submit returned nil error, want cancelled error")
	}
	if !errors.Is(err, ErrCommandCancelled) {
		t.Errorf("error = %v, want ErrCommandCancelled in chain", err)
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
		LockMap:    lock.NewMutexMap(),
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

// setupAwaitingFillFixture creates a CommandState with a sealed plan containing
// a concrete phase (completed) and a deferred phase in awaiting_fill status,
// along with the required maestro directory structure and worker queues.
func setupAwaitingFillFixture(t *testing.T) (string, *model.CommandState, string) {
	t.Helper()
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000010_aabbccdd"

	sm := NewStateManager(maestroDir, lock.NewMutexMap())

	now := "2025-01-01T00:00:00Z"
	state := &model.CommandState{
		SchemaVersion:    1,
		FileType:         "state_command",
		CommandID:        commandID,
		PlanVersion:      1,
		PlanStatus:       model.PlanStatusSealed,
		CompletionPolicy: defaultCompletionPolicy(),
		TaskTracking: model.TaskTracking{
			TaskDependencies: make(map[string][]string),
			TaskStates: map[string]model.Status{
				"task_0000000001_aaaaaaaa": model.StatusCompleted,
			},
			CancelledReasons:  make(map[string]string),
			AppliedResultIDs:  make(map[string]string),
			RequiredTaskIDs:   []string{"task_0000000001_aaaaaaaa"},
			ExpectedTaskCount: 1,
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: make(map[string]string),
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID:     "phase_0000000001_aaaaaaaa",
					Name:        "phase_build",
					Type:        "concrete",
					Status:      model.PhaseStatusCompleted,
					TaskIDs:     []string{"task_0000000001_aaaaaaaa"},
					ActivatedAt: &now,
					CompletedAt: &now,
				},
				{
					PhaseID:         "phase_0000000002_bbbbbbbb",
					Name:            "phase_review",
					Type:            "deferred",
					Status:          model.PhaseStatusAwaitingFill,
					DependsOnPhases: []string{"phase_build"},
					Constraints: &model.PhaseConstraints{
						MaxTasks:           5,
						TimeoutMinutes:     30,
						AllowedBloomLevels: []int{1, 2, 3},
					},
				},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := sm.SaveState(state); err != nil {
		t.Fatalf("save fixture state: %v", err)
	}

	return maestroDir, state, commandID
}

func TestSubmit_PhaseFill_Success(t *testing.T) {
	maestroDir, origState, commandID := setupAwaitingFillFixture(t)
	cfg := testConfig()

	tasksFile := writeTasksFile(t, []TaskInput{
		{
			Name:               "review_task_1",
			Purpose:            "review code",
			Content:            "review the implementation",
			AcceptanceCriteria: "code reviewed",
			BloomLevel:         2,
			Required:           true,
		},
		{
			Name:               "review_task_2",
			Purpose:            "review tests",
			Content:            "review the tests",
			AcceptanceCriteria: "tests reviewed",
			BloomLevel:         1,
			Required:           true,
		},
	})

	result, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		PhaseName:  "phase_review",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Submit phase fill returned error: %v", err)
	}

	if result.CommandID != commandID {
		t.Errorf("CommandID = %q, want %q", result.CommandID, commandID)
	}
	if len(result.Tasks) != 2 {
		t.Fatalf("len(Tasks) = %d, want 2", len(result.Tasks))
	}

	// Verify task names and IDs
	for _, tr := range result.Tasks {
		if tr.TaskID == "" {
			t.Errorf("task %q has empty TaskID", tr.Name)
		}
		if tr.Worker == "" {
			t.Errorf("task %q has empty Worker", tr.Name)
		}
		if !model.ValidateID(tr.TaskID) {
			t.Errorf("task %q has invalid TaskID format: %s", tr.Name, tr.TaskID)
		}
	}

	// Verify state: phase transitioned to active, PlanVersion incremented
	sm := NewStateManager(maestroDir, lock.NewMutexMap())
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if state.PlanVersion != origState.PlanVersion+1 {
		t.Errorf("PlanVersion = %d, want %d", state.PlanVersion, origState.PlanVersion+1)
	}

	// Find phase_review
	var reviewPhase *model.Phase
	for i := range state.Phases {
		if state.Phases[i].Name == "phase_review" {
			reviewPhase = &state.Phases[i]
			break
		}
	}
	if reviewPhase == nil {
		t.Fatal("phase_review not found in state")
	}
	if reviewPhase.Status != model.PhaseStatusActive {
		t.Errorf("phase_review.Status = %q, want %q", reviewPhase.Status, model.PhaseStatusActive)
	}
	if reviewPhase.ActivatedAt == nil {
		t.Error("phase_review.ActivatedAt is nil, want non-nil")
	}
	if len(reviewPhase.TaskIDs) != 2 {
		t.Errorf("len(phase_review.TaskIDs) = %d, want 2", len(reviewPhase.TaskIDs))
	}

	// Verify tasks were added to state
	if state.ExpectedTaskCount != 3 { // 1 original + 2 new
		t.Errorf("ExpectedTaskCount = %d, want 3", state.ExpectedTaskCount)
	}
	for _, tid := range reviewPhase.TaskIDs {
		if s, ok := state.TaskStates[tid]; !ok {
			t.Errorf("task %s not in TaskStates", tid)
		} else if s != model.StatusPlanned {
			// §2.1: tasks added via phase-fill enter the lifecycle at `planned`.
			t.Errorf("TaskStates[%s] = %q, want %q", tid, s, model.StatusPlanned)
		}
	}
}

func TestSubmit_PhaseFill_DryRun_NoMutation(t *testing.T) {
	maestroDir, origState, commandID := setupAwaitingFillFixture(t)
	cfg := testConfig()

	tasksFile := writeTasksFile(t, []TaskInput{
		{
			Name:               "review_dry",
			Purpose:            "dry run review",
			Content:            "validate only",
			AcceptanceCriteria: "valid input",
			BloomLevel:         2,
			Required:           true,
		},
	})

	result, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		PhaseName:  "phase_review",
		DryRun:     true,
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Submit phase fill dry-run returned error: %v", err)
	}
	if !result.Valid {
		t.Errorf("result.Valid = false, want true")
	}

	// Verify no mutation: state should be unchanged
	sm := NewStateManager(maestroDir, lock.NewMutexMap())
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.PlanVersion != origState.PlanVersion {
		t.Errorf("PlanVersion = %d, want %d (no mutation)", state.PlanVersion, origState.PlanVersion)
	}
	for i := range state.Phases {
		if state.Phases[i].Name == "phase_review" {
			if state.Phases[i].Status != model.PhaseStatusAwaitingFill {
				t.Errorf("phase_review.Status = %q, want %q (no mutation)", state.Phases[i].Status, model.PhaseStatusAwaitingFill)
			}
			break
		}
	}
}

func TestSubmit_PhaseFill_Preconditions(t *testing.T) {
	cfg := testConfig()

	validTasks := []TaskInput{
		{
			Name:               "fill_task",
			Purpose:            "fill task",
			Content:            "do work",
			AcceptanceCriteria: "done",
			BloomLevel:         2,
			Required:           true,
		},
	}

	tests := []struct {
		name       string
		setup      func(t *testing.T) (string, string)
		phaseName  string
		wantErrMsg string
	}{
		{
			name: "not_sealed",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				maestroDir := setupMaestroDir(t)
				cmdID := "cmd_0000000020_aabbccdd"
				sm := NewStateManager(maestroDir, lock.NewMutexMap())
				state := &model.CommandState{
					SchemaVersion: 1,
					FileType:      "state_command",
					CommandID:     cmdID,
					PlanStatus:    model.PlanStatusPlanning,
					TaskTracking: model.TaskTracking{
						TaskStates: make(map[string]model.Status),
					},
					PhaseTracking: model.PhaseTracking{
						Phases: []model.Phase{
							{PhaseID: "p1", Name: "phase_a", Type: "deferred", Status: model.PhaseStatusAwaitingFill},
						},
					},
				}
				if err := sm.SaveState(state); err != nil {
					t.Fatal(err)
				}
				return maestroDir, cmdID
			},
			phaseName:  "phase_a",
			wantErrMsg: "plan_status must be sealed",
		},
		{
			name: "cancelled",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				maestroDir := setupMaestroDir(t)
				cmdID := "cmd_0000000021_aabbccdd"
				sm := NewStateManager(maestroDir, lock.NewMutexMap())
				now := "2025-01-01T00:00:00Z"
				state := &model.CommandState{
					SchemaVersion: 1,
					FileType:      "state_command",
					CommandID:     cmdID,
					PlanStatus:    model.PlanStatusSealed,
					TaskTracking: model.TaskTracking{
						TaskStates: make(map[string]model.Status),
					},
					Cancel: model.CancelState{
						Requested:   true,
						RequestedAt: &now,
					},
					PhaseTracking: model.PhaseTracking{
						Phases: []model.Phase{
							{PhaseID: "p1", Name: "phase_a", Type: "deferred", Status: model.PhaseStatusAwaitingFill},
						},
					},
				}
				if err := sm.SaveState(state); err != nil {
					t.Fatal(err)
				}
				return maestroDir, cmdID
			},
			phaseName:  "phase_a",
			wantErrMsg: "cancelled",
		},
		{
			name: "phase_not_found",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				maestroDir := setupMaestroDir(t)
				cmdID := "cmd_0000000022_aabbccdd"
				sm := NewStateManager(maestroDir, lock.NewMutexMap())
				state := &model.CommandState{
					SchemaVersion: 1,
					FileType:      "state_command",
					CommandID:     cmdID,
					PlanStatus:    model.PlanStatusSealed,
					TaskTracking: model.TaskTracking{
						TaskStates: make(map[string]model.Status),
					},
					PhaseTracking: model.PhaseTracking{
						Phases: []model.Phase{
							{PhaseID: "p1", Name: "phase_a", Type: "deferred", Status: model.PhaseStatusAwaitingFill},
						},
					},
				}
				if err := sm.SaveState(state); err != nil {
					t.Fatal(err)
				}
				return maestroDir, cmdID
			},
			phaseName:  "nonexistent_phase",
			wantErrMsg: "not found",
		},
		{
			name: "phase_not_deferred",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				maestroDir := setupMaestroDir(t)
				cmdID := "cmd_0000000023_aabbccdd"
				sm := NewStateManager(maestroDir, lock.NewMutexMap())
				state := &model.CommandState{
					SchemaVersion: 1,
					FileType:      "state_command",
					CommandID:     cmdID,
					PlanStatus:    model.PlanStatusSealed,
					TaskTracking: model.TaskTracking{
						TaskStates: make(map[string]model.Status),
					},
					PhaseTracking: model.PhaseTracking{
						Phases: []model.Phase{
							{PhaseID: "p1", Name: "phase_a", Type: "concrete", Status: model.PhaseStatusActive},
						},
					},
				}
				if err := sm.SaveState(state); err != nil {
					t.Fatal(err)
				}
				return maestroDir, cmdID
			},
			phaseName:  "phase_a",
			wantErrMsg: "not deferred",
		},
		{
			name: "phase_not_awaiting_fill",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				maestroDir := setupMaestroDir(t)
				cmdID := "cmd_0000000024_aabbccdd"
				sm := NewStateManager(maestroDir, lock.NewMutexMap())
				state := &model.CommandState{
					SchemaVersion: 1,
					FileType:      "state_command",
					CommandID:     cmdID,
					PlanStatus:    model.PlanStatusSealed,
					TaskTracking: model.TaskTracking{
						TaskStates: make(map[string]model.Status),
					},
					PhaseTracking: model.PhaseTracking{
						Phases: []model.Phase{
							{PhaseID: "p1", Name: "phase_a", Type: "deferred", Status: model.PhaseStatusPending},
						},
					},
				}
				if err := sm.SaveState(state); err != nil {
					t.Fatal(err)
				}
				return maestroDir, cmdID
			},
			phaseName:  "phase_a",
			wantErrMsg: "awaiting_fill",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maestroDir, cmdID := tt.setup(t)
			tasksFile := writeTasksFile(t, validTasks)

			_, err := Submit(SubmitOptions{
				CommandID:  cmdID,
				TasksFile:  tasksFile,
				PhaseName:  tt.phaseName,
				MaestroDir: maestroDir,
				Config:     cfg,
				LockMap:    lock.NewMutexMap(),
			})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErrMsg)
			}
			if !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErrMsg)
			}
		})
	}
}

func TestSubmit_PhaseFill_ConstraintViolation(t *testing.T) {
	maestroDir, origState, commandID := setupAwaitingFillFixture(t)
	cfg := testConfig()

	// The phase allows bloom levels 1,2,3 and max 5 tasks.
	// Submit a task with bloom level 5 (not allowed).
	tasksFile := writeTasksFile(t, []TaskInput{
		{
			Name:               "bad_bloom_task",
			Purpose:            "this should fail",
			Content:            "bloom level violation",
			AcceptanceCriteria: "never passes",
			BloomLevel:         5,
			Required:           true,
		},
	})

	_, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		PhaseName:  "phase_review",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err == nil {
		t.Fatal("expected constraint violation error, got nil")
	}
	if !strings.Contains(err.Error(), "bloom_level") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "bloom_level")
	}

	// Verify no mutation: state should be unchanged
	sm := NewStateManager(maestroDir, lock.NewMutexMap())
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.PlanVersion != origState.PlanVersion {
		t.Errorf("PlanVersion = %d, want %d (no mutation after constraint violation)", state.PlanVersion, origState.PlanVersion)
	}
	for i := range state.Phases {
		if state.Phases[i].Name == "phase_review" {
			if state.Phases[i].Status != model.PhaseStatusAwaitingFill {
				t.Errorf("phase_review.Status = %q, want %q (no mutation)", state.Phases[i].Status, model.PhaseStatusAwaitingFill)
			}
			if len(state.Phases[i].TaskIDs) != 0 {
				t.Errorf("len(phase_review.TaskIDs) = %d, want 0 (no mutation)", len(state.Phases[i].TaskIDs))
			}
			break
		}
	}
}

// TestShouldInsertSystemCommit pins the (post-2026-04-28) invariant that
// __system_commit insertion is driven solely by worktree mode being
// disabled. Continuous mode is independent: a single-shot run with
// worktree.enabled=false still needs the commit task, otherwise the
// Worker's in-place edits to main stay dirty after `completed`. The
// previous predicate gated insertion on continuous=true, which is the
// footgun we are fixing here.
func TestShouldInsertSystemCommit(t *testing.T) {
	tests := []struct {
		name       string
		continuous bool
		worktree   bool
		want       bool
	}{
		{"continuous off, worktree off", false, false, true},
		{"continuous on,  worktree off", true, false, true},
		{"continuous off, worktree on", false, true, false},
		{"continuous on,  worktree on", true, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := model.Config{
				Continuous: model.ContinuousConfig{Enabled: tc.continuous},
				Worktree:   model.WorktreeConfig{Enabled: tc.worktree},
			}
			if got := shouldInsertSystemCommit(cfg); got != tc.want {
				t.Errorf("shouldInsertSystemCommit(continuous=%v worktree=%v) = %v, want %v",
					tc.continuous, tc.worktree, got, tc.want)
			}
		})
	}
}

// TestSubmit_NoSystemCommit_InWorktreeMode verifies that even when continuous
// mode is enabled, no __system_commit task is inserted into the plan when
// worktree isolation is enabled. This ensures that the responsibility for
// committing changes lives in exactly one place (the Daemon's worktree
// merge/publish path), not in a Worker-side task.
func TestSubmit_NoSystemCommit_InWorktreeMode(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	cfg.Continuous = model.ContinuousConfig{Enabled: true}
	cfg.Worktree = model.WorktreeConfig{Enabled: true}
	commandID := "cmd_0000000099_aabbccdd"

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
	})

	result, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Submit returned error: %v", err)
	}

	for _, tr := range result.Tasks {
		if tr.Name == "__system_commit" {
			t.Errorf("Submit inserted __system_commit task in worktree mode (should be skipped); tasks=%+v", result.Tasks)
		}
	}
	if len(result.Tasks) != 1 {
		t.Errorf("len(Tasks) = %d, want 1 (only the user task, no __system_commit)", len(result.Tasks))
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.SystemCommitTaskID != nil {
		t.Errorf("SystemCommitTaskID = %v, want nil in worktree mode", *state.SystemCommitTaskID)
	}
}

// TestSubmit_PhasedSubmit_NoSystemCommit_InWorktreeMode is the phased-input
// counterpart of TestSubmit_NoSystemCommit_InWorktreeMode.
func TestSubmit_PhasedSubmit_NoSystemCommit_InWorktreeMode(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	cfg.Continuous = model.ContinuousConfig{Enabled: true}
	cfg.Worktree = model.WorktreeConfig{Enabled: true}
	commandID := "cmd_0000000100_aabbccdd"

	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	phasesFile := writePhasesFile(t, []PhaseInput{
		{
			Name: "phase_impl",
			Type: "concrete",
			Tasks: []TaskInput{
				{
					Name:               "task_a",
					Purpose:            "do task a",
					Content:            "implement feature a",
					AcceptanceCriteria: "feature a works",
					BloomLevel:         2,
					Required:           true,
				},
			},
		},
	})

	result, err := Submit(SubmitOptions{
		CommandID:  commandID,
		TasksFile:  phasesFile,
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Submit returned error: %v", err)
	}
	for _, tr := range result.Tasks {
		if tr.Name == "__system_commit" {
			t.Errorf("Phased Submit inserted __system_commit task in worktree mode; tasks=%+v", result.Tasks)
		}
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.SystemCommitTaskID != nil {
		t.Errorf("SystemCommitTaskID = %v, want nil in worktree mode (phased)", *state.SystemCommitTaskID)
	}
}

// --- Bug M: empty tasks in phase-fill mode must surface as a structured
// validation error (not a generic ErrCodeInternal), and stale awaiting_fill
// signals against a phase that has already moved on must produce the
// state-aware "must be awaiting_fill" diagnostic rather than the generic
// "either tasks or phases must be specified" error. ---

func TestSubmit_PhaseFill_EmptyTasks_StateAwareDiagnostic(t *testing.T) {
	// Reproduces Bug M: a Planner with a stale awaiting_fill signal re-submits
	// after the phase has already transitioned to "filling". The submission
	// arrives with the phase-fill PhaseName set and (in some race variants)
	// empty tasks. Before the fix, Submit's empty-tasks guard fired BEFORE
	// the phase-fill router, returning a bare fmt.Errorf that surfaced as
	// ErrCodeInternal in the daemon log. After the fix, the phase-fill router
	// runs first and submitPhaseFill's state-aware check produces a
	// planValidationError with "must be awaiting_fill" wording.
	cfg := testConfig()
	maestroDir := setupMaestroDir(t)
	cmdID := "cmd_0000000099_bugmtest"
	sm := NewStateManager(maestroDir, lock.NewMutexMap())
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     cmdID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: make(map[string]model.Status),
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				// Phase has already moved past awaiting_fill: the prior submit
				// has won the race and transitioned it to filling.
				{PhaseID: "p1", Name: "phase_a", Type: "deferred", Status: model.PhaseStatusFilling},
			},
		},
	}
	if err := sm.SaveState(state); err != nil {
		t.Fatal(err)
	}

	// Empty tasks list — the duplicate signal can carry no payload.
	tasksFile := writeTasksFile(t, []TaskInput{})

	_, err := Submit(SubmitOptions{
		CommandID:  cmdID,
		TasksFile:  tasksFile,
		PhaseName:  "phase_a",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err == nil {
		t.Fatal("expected error for stale phase-fill submission, got nil")
	}

	// Must be a planValidationError so the daemon maps it to ErrCodeValidation,
	// not ErrCodeInternal.
	var ve *planValidationError
	if !errors.As(err, &ve) {
		var verrs *ValidationErrors
		if !errors.As(err, &verrs) {
			t.Fatalf("error must be planValidationError or *ValidationErrors for ErrCodeValidation routing, got %T: %v", err, err)
		}
	}

	// Must surface the state-aware diagnostic, not the generic empty-tasks message.
	if !strings.Contains(err.Error(), "awaiting_fill") {
		t.Errorf("expected state-aware diagnostic mentioning awaiting_fill, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "either tasks or phases must be specified") {
		t.Errorf("Bug M regression: empty-tasks guard fired before phase-fill router: %q", err.Error())
	}
}

// --- C-A3 / TOCTOU: Concurrent double-submit prevention ---

func TestSubmit_ConcurrentDoubleSubmit(t *testing.T) {
	// Launch multiple goroutines submitting with the same command ID.
	// Exactly one should succeed; all others should fail with ErrDoubleSubmit.
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	commandID := "cmd_0000000060_aabbccdd"
	lm := lock.NewMutexMap()

	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	const goroutines = 5
	results := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			tasksFile := writeTasksFile(t, []TaskInput{
				{
					Name:               fmt.Sprintf("task_concurrent_%d", idx),
					Purpose:            "concurrent test",
					Content:            "concurrent implementation",
					AcceptanceCriteria: "works correctly",
					BloomLevel:         2,
					Required:           true,
				},
			})
			_, err := Submit(SubmitOptions{
				CommandID:  commandID,
				TasksFile:  tasksFile,
				MaestroDir: maestroDir,
				Config:     cfg,
				LockMap:    lm,
			})
			results <- err
		}(i)
	}

	var successes, doubleSubmits int
	for i := 0; i < goroutines; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, ErrDoubleSubmit) {
			doubleSubmits++
		} else {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1", successes)
	}
	if doubleSubmits != goroutines-1 {
		t.Errorf("doubleSubmits = %d, want %d", doubleSubmits, goroutines-1)
	}
}

// TestParseInput_SpecialEscapeCharacters verifies that parseInput handles YAML
// containing invalid escape sequences in double-quoted strings (e.g. \! and \:)
// which Planner agents may produce via heredoc.
func TestParseInput_SpecialEscapeCharacters(t *testing.T) {
	// bs produces a literal backslash without Go escape ambiguity.
	const bs = "\x5c"

	tests := []struct {
		name        string
		yaml        string
		wantContent string
	}{
		{
			name: "backslash-bang in double-quoted content",
			yaml: "tasks:\n" +
				"  - name: t1\n" +
				"    purpose: test\n" +
				"    content: " + `"run ` + bs + `!cmd here"` + "\n" +
				"    acceptance_criteria: done\n" +
				"    bloom_level: 2\n" +
				"    required: true\n",
			wantContent: "run " + bs + "!cmd here",
		},
		{
			name: "backslash-colon in double-quoted content",
			yaml: "tasks:\n" +
				"  - name: t1\n" +
				"    purpose: test\n" +
				"    content: " + `"check ` + bs + `:value"` + "\n" +
				"    acceptance_criteria: done\n" +
				"    bloom_level: 2\n" +
				"    required: true\n",
			wantContent: "check " + bs + ":value",
		},
		{
			name: "multiple special chars in double-quoted content",
			yaml: "tasks:\n" +
				"  - name: t1\n" +
				"    purpose: test\n" +
				"    content: " + `"` + bs + `! and ` + bs + `: and ` + bs + `# mixed"` + "\n" +
				"    acceptance_criteria: done\n" +
				"    bloom_level: 2\n" +
				"    required: true\n",
			wantContent: bs + "! and " + bs + ": and " + bs + "# mixed",
		},
		{
			name: "literal block scalar with backslash-bang preserved",
			yaml: "tasks:\n" +
				"  - name: t1\n" +
				"    purpose: test\n" +
				"    content: |\n" +
				"      run " + bs + "!cmd here\n" +
				"    acceptance_criteria: done\n" +
				"    bloom_level: 2\n" +
				"    required: true\n",
			wantContent: "run " + bs + "!cmd here\n",
		},
		{
			name: "valid escapes in double-quoted content preserved",
			yaml: "tasks:\n" +
				"  - name: t1\n" +
				"    purpose: test\n" +
				"    content: " + `"line1` + bs + `nline2"` + "\n" +
				"    acceptance_criteria: done\n" +
				"    bloom_level: 2\n" +
				"    required: true\n",
			wantContent: "line1\nline2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, err := parseInput([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("parseInput failed: %v\nyaml: %q", err, tt.yaml)
			}
			if len(input.Tasks) != 1 {
				t.Fatalf("expected 1 task, got %d", len(input.Tasks))
			}
			if input.Tasks[0].Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", input.Tasks[0].Content, tt.wantContent)
			}
		})
	}
}
