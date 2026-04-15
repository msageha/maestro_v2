package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func setupCompleteTest(t *testing.T, commandID string, taskStates map[string]model.Status, requiredIDs []string) string {
	t.Helper()
	maestroDir := setupMaestroDir(t)

	// Write state file with sealed plan and given task states
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusSealed,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			AllowDynamicTasks:       false,
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: len(requiredIDs),
			RequiredTaskIDs:   requiredIDs,
			OptionalTaskIDs:   []string{},
			TaskDependencies:  make(map[string][]string),
			TaskStates:        taskStates,
			CancelledReasons:  make(map[string]string),
			AppliedResultIDs:  make(map[string]string),
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: make(map[string]string),
		},
		CreatedAt: "2025-01-01T00:00:00Z",
		UpdatedAt: "2025-01-01T00:00:00Z",
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// Write planner queue with in_progress command
	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	// Write worker results with matching task results
	var results []model.TaskResult
	for taskID, status := range taskStates {
		results = append(results, model.TaskResult{
			ID:        "res_0000000001_00000001",
			TaskID:    taskID,
			CommandID: commandID,
			Status:    status,
			Summary:   "task " + string(status),
			CreatedAt: "2025-01-01T00:00:00Z",
		})
	}
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results:       results,
	}
	resultPath := filepath.Join(maestroDir, "results", "worker0.yaml")
	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		t.Fatalf("write worker results: %v", err)
	}

	return maestroDir
}

func TestComplete_AllCompleted(t *testing.T) {
	commandID := "cmd_0000000010_aabbccdd"
	taskID1 := "task_0000000010_11111111"
	taskID2 := "task_0000000010_22222222"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
		taskID2: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1, taskID2}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()

	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "all tasks done",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}

	if result.CommandID != commandID {
		t.Errorf("CommandID = %q, want %q", result.CommandID, commandID)
	}
	if result.Status != string(model.PlanStatusCompleted) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusCompleted)
	}

	// Verify state file was updated
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("state.PlanStatus = %q, want %q", state.PlanStatus, model.PlanStatusCompleted)
	}

	// Verify planner queue was updated
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	plannerData, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(plannerData, &cq); err != nil {
		t.Fatalf("unmarshal planner queue: %v", err)
	}
	if len(cq.Commands) != 1 {
		t.Fatalf("len(Commands) = %d, want 1", len(cq.Commands))
	}
	if cq.Commands[0].Status != model.StatusCompleted {
		t.Errorf("queue command Status = %q, want %q", cq.Commands[0].Status, model.StatusCompleted)
	}
	if cq.Commands[0].LeaseOwner != nil {
		t.Errorf("queue command LeaseOwner = %v, want nil", cq.Commands[0].LeaseOwner)
	}

	// Verify command result was written to results/planner.yaml
	cmdResultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	cmdResultData, err := os.ReadFile(cmdResultPath)
	if err != nil {
		t.Fatalf("read command result: %v", err)
	}
	var crf model.CommandResultFile
	if err := yamlv3.Unmarshal(cmdResultData, &crf); err != nil {
		t.Fatalf("unmarshal command result: %v", err)
	}
	if len(crf.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1", len(crf.Results))
	}
	if crf.Results[0].Status != model.StatusCompleted {
		t.Errorf("command result Status = %q, want %q", crf.Results[0].Status, model.StatusCompleted)
	}
	if crf.Results[0].Summary != "all tasks done" {
		t.Errorf("command result Summary = %q, want %q", crf.Results[0].Summary, "all tasks done")
	}
}

func TestComplete_HasFailed(t *testing.T) {
	commandID := "cmd_0000000011_aabbccdd"
	taskID1 := "task_0000000011_11111111"
	taskID2 := "task_0000000011_22222222"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
		taskID2: model.StatusFailed,
	}
	requiredIDs := []string{taskID1, taskID2}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()

	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "one task failed",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}

	if result.Status != string(model.PlanStatusFailed) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusFailed)
	}

	// Verify state reflects failed plan status
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.PlanStatus != model.PlanStatusFailed {
		t.Errorf("state.PlanStatus = %q, want %q", state.PlanStatus, model.PlanStatusFailed)
	}

	// Verify planner queue reflects failed status
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	plannerData, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(plannerData, &cq); err != nil {
		t.Fatalf("unmarshal planner queue: %v", err)
	}
	if cq.Commands[0].Status != model.StatusFailed {
		t.Errorf("queue command Status = %q, want %q", cq.Commands[0].Status, model.StatusFailed)
	}
}

func TestComplete_NotSealed(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	commandID := "cmd_0000000012_aabbccdd"

	// Write state with planning status (not sealed)
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   0,
		PlanStatus:    model.PlanStatusPlanning,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			AllowDynamicTasks:       false,
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 0,
			RequiredTaskIDs:   []string{},
			OptionalTaskIDs:   []string{},
			TaskDependencies:  make(map[string][]string),
			TaskStates:        make(map[string]model.Status),
			CancelledReasons:  make(map[string]string),
			AppliedResultIDs:  make(map[string]string),
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: make(map[string]string),
		},
		CreatedAt: "2025-01-01T00:00:00Z",
		UpdatedAt: "2025-01-01T00:00:00Z",
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	_, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "should fail",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err == nil {
		t.Fatalf("Complete returned nil error, want error for non-sealed plan")
	}
	if !strings.Contains(err.Error(), "sealed") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "sealed")
	}
}

// --- WAL intent recovery tests ---

// writeManualIntent writes a complete intent file for a given command to simulate
// a crash after intent creation but before/during completion steps.
func writeManualIntent(t *testing.T, maestroDir string, intent *completeIntent) {
	t.Helper()
	dir := filepath.Join(maestroDir, "intents")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("create intents dir: %v", err)
	}
	if err := yamlutil.AtomicWrite(completeIntentPath(maestroDir, intent.CommandID), intent); err != nil {
		t.Fatalf("write intent: %v", err)
	}
}

func TestComplete_RecoveryReplay_NoStepsDone(t *testing.T) {
	// Simulate crash: intent exists but none of the 3 steps have been executed.
	commandID := "cmd_0000000040_aabbccdd"
	taskID1 := "task_0000000040_11111111"
	taskID2 := "task_0000000040_22222222"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
		taskID2: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1, taskID2}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// Write intent manually (simulating crash after intent write, before any step)
	writeManualIntent(t, maestroDir, &completeIntent{
		SchemaVersion: 1,
		FileType:      "intent_plan_complete",
		CommandID:     commandID,
		Summary:       "recovered summary",
		ResultStatus:  model.StatusCompleted,
		PlanStatus:    model.PlanStatusCompleted,
		TaskResults: []model.CommandResultTask{
			{TaskID: taskID1, Worker: "worker0", Status: model.StatusCompleted, Summary: "done"},
			{TaskID: taskID2, Worker: "worker0", Status: model.StatusCompleted, Summary: "done"},
		},
		CreatedAt: "2025-01-01T00:00:00Z",
	})

	// Call Complete — should discover intent and replay all steps
	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "caller summary (should be ignored since intent exists)",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lm,
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if result.Status != string(model.PlanStatusCompleted) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusCompleted)
	}

	// Verify Step 1: results/planner.yaml written
	cmdResultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	cmdResultData, err := os.ReadFile(cmdResultPath)
	if err != nil {
		t.Fatalf("read command result: %v", err)
	}
	var crf model.CommandResultFile
	if err := yamlv3.Unmarshal(cmdResultData, &crf); err != nil {
		t.Fatalf("unmarshal command result: %v", err)
	}
	if len(crf.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1", len(crf.Results))
	}
	if crf.Results[0].Summary != "recovered summary" {
		t.Errorf("result summary = %q, want %q", crf.Results[0].Summary, "recovered summary")
	}

	// Verify Step 2: queue/planner.yaml updated
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	plannerData, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(plannerData, &cq); err != nil {
		t.Fatalf("unmarshal planner queue: %v", err)
	}
	if cq.Commands[0].Status != model.StatusCompleted {
		t.Errorf("queue command status = %s, want completed", cq.Commands[0].Status)
	}

	// Verify Step 3: state updated
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("state.PlanStatus = %q, want %q", state.PlanStatus, model.PlanStatusCompleted)
	}

	// Verify intent file was removed after successful recovery
	intentPath := completeIntentPath(maestroDir, commandID)
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Errorf("intent file should be removed after recovery, but still exists")
	}
}

func TestComplete_RecoveryReplay_AfterStep1(t *testing.T) {
	// Simulate crash after Step 1 (result written) but before Step 2 and 3.
	commandID := "cmd_0000000041_aabbccdd"
	taskID1 := "task_0000000041_11111111"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// Manually execute Step 1 (write command result to results/planner.yaml)
	lm.Lock("result:planner")
	err := writeCommandResultLocked(maestroDir, commandID, model.StatusCompleted, "step1 done", []model.CommandResultTask{
		{TaskID: taskID1, Worker: "worker0", Status: model.StatusCompleted, Summary: "done"},
	})
	lm.Unlock("result:planner")
	if err != nil {
		t.Fatalf("pre-write command result: %v", err)
	}

	// Write intent (simulating crash after Step 1 was done)
	writeManualIntent(t, maestroDir, &completeIntent{
		SchemaVersion: 1,
		FileType:      "intent_plan_complete",
		CommandID:     commandID,
		Summary:       "step1 done",
		ResultStatus:  model.StatusCompleted,
		PlanStatus:    model.PlanStatusCompleted,
		TaskResults: []model.CommandResultTask{
			{TaskID: taskID1, Worker: "worker0", Status: model.StatusCompleted, Summary: "done"},
		},
		CreatedAt: "2025-01-01T00:00:00Z",
	})

	// Call Complete — should replay, Step 1 is idempotent (no duplicate)
	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "caller summary",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lm,
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if result.Status != string(model.PlanStatusCompleted) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusCompleted)
	}

	// Verify no duplicate result entries (idempotency)
	cmdResultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	cmdResultData, err := os.ReadFile(cmdResultPath)
	if err != nil {
		t.Fatalf("read command result: %v", err)
	}
	var crf model.CommandResultFile
	if err := yamlv3.Unmarshal(cmdResultData, &crf); err != nil {
		t.Fatalf("unmarshal command result: %v", err)
	}
	if len(crf.Results) != 1 {
		t.Errorf("len(Results) = %d, want 1 (no duplicate)", len(crf.Results))
	}

	// Verify Step 2 and 3 were completed
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	plannerData, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(plannerData, &cq); err != nil {
		t.Fatalf("unmarshal planner queue: %v", err)
	}
	if cq.Commands[0].Status != model.StatusCompleted {
		t.Errorf("queue command status = %s, want completed", cq.Commands[0].Status)
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("state.PlanStatus = %q, want %q", state.PlanStatus, model.PlanStatusCompleted)
	}

	// Verify intent removed
	intentPath := completeIntentPath(maestroDir, commandID)
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Error("intent file should be removed after recovery")
	}
}

func TestComplete_IntentCorrupt_Removed(t *testing.T) {
	// A corrupt intent file should be removed and the normal flow proceeds.
	commandID := "cmd_0000000042_aabbccdd"
	taskID1 := "task_0000000042_11111111"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// Write corrupt intent file (invalid YAML / wrong schema)
	intentDir := filepath.Join(maestroDir, "intents")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatalf("create intents dir: %v", err)
	}
	intentPath := completeIntentPath(maestroDir, commandID)
	if err := os.WriteFile(intentPath, []byte("{{{{not valid yaml!!!!"), 0644); err != nil {
		t.Fatalf("write corrupt intent: %v", err)
	}

	// Complete should remove the corrupt intent and proceed normally
	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "normal flow after corrupt intent",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lm,
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if result.Status != string(model.PlanStatusCompleted) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusCompleted)
	}

	// Verify corrupt intent was removed
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Error("corrupt intent file should be removed")
	}

	// Verify normal completion artifacts exist
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("state.PlanStatus = %q, want %q", state.PlanStatus, model.PlanStatusCompleted)
	}
}

// TestComplete_H3_ConflictRecovery_StateFailedIntentCompleted simulates the
// H3 race: a previous Complete attempt wrote results/planner.yaml and the
// queue entry as "completed" but crashed before updating state. After the
// crash, dead-letter (or another actor) transitioned state.PlanStatus to
// "failed". On recovery, the replay must reconcile result/queue to match the
// actual state ("failed"), not silently leave the prior "completed" artifacts
// in place.
func TestComplete_H3_ConflictRecovery_StateFailedIntentCompleted(t *testing.T) {
	commandID := "cmd_0000000043_aabbccdd"
	taskID1 := "task_0000000043_11111111"

	// State: tasks all completed (so CanComplete would derive completed),
	// but plan_status has been forced to failed (e.g. by dead-letter).
	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// Force state to failed (simulating dead-letter post-crash transition).
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st model.CommandState
	if err := yamlv3.Unmarshal(stateData, &st); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	st.PlanStatus = model.PlanStatusFailed
	if err := yamlutil.AtomicWrite(statePath, &st); err != nil {
		t.Fatalf("write failed state: %v", err)
	}

	// Pre-write results/planner.yaml as completed (Step 1 from prior crashed run).
	lm.Lock("result:planner")
	if err := writeCommandResultLocked(maestroDir, commandID, model.StatusCompleted, "prior completed", []model.CommandResultTask{
		{TaskID: taskID1, Worker: "worker0", Status: model.StatusCompleted, Summary: "done"},
	}); err != nil {
		lm.Unlock("result:planner")
		t.Fatalf("pre-write result: %v", err)
	}
	lm.Unlock("result:planner")

	// Pre-update planner queue as completed (Step 2 from prior crashed run).
	if err := updateCommandQueueEntryLocked(maestroDir, commandID, model.StatusCompleted); err != nil {
		t.Fatalf("pre-update queue: %v", err)
	}

	// Write the original "completed" intent.
	writeManualIntent(t, maestroDir, &completeIntent{
		SchemaVersion: 1,
		FileType:      "intent_plan_complete",
		CommandID:     commandID,
		Summary:       "prior completed",
		ResultStatus:  model.StatusCompleted,
		PlanStatus:    model.PlanStatusCompleted,
		TaskResults: []model.CommandResultTask{
			{TaskID: taskID1, Worker: "worker0", Status: model.StatusCompleted, Summary: "done"},
		},
		CreatedAt: "2025-01-01T00:00:00Z",
	})

	// Recovery via Complete().
	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "caller summary (ignored on recovery)",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lm,
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	// Returned status reflects the actual state, not the intent.
	if result.Status != string(model.PlanStatusFailed) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusFailed)
	}

	// results/planner.yaml: existing entry should be reconciled to failed,
	// preserving its ID (so notification dedup keys remain stable).
	cmdResultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	cmdResultData, err := os.ReadFile(cmdResultPath)
	if err != nil {
		t.Fatalf("read command result: %v", err)
	}
	var crf model.CommandResultFile
	if err := yamlv3.Unmarshal(cmdResultData, &crf); err != nil {
		t.Fatalf("unmarshal command result: %v", err)
	}
	if len(crf.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1 (in-place reconcile)", len(crf.Results))
	}
	if crf.Results[0].Status != model.StatusFailed {
		t.Errorf("result.Status = %q, want failed (reconciled)", crf.Results[0].Status)
	}
	if crf.Results[0].Notified {
		t.Errorf("result.Notified = true, want false (reset for re-notification)")
	}

	// queue/planner.yaml: entry should be force-updated from completed → failed.
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	plannerData, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(plannerData, &cq); err != nil {
		t.Fatalf("unmarshal planner queue: %v", err)
	}
	if cq.Commands[0].Status != model.StatusFailed {
		t.Errorf("queue command status = %s, want failed (reconciled)", cq.Commands[0].Status)
	}

	// state.PlanStatus must remain failed (not overwritten back to completed).
	stateData2, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("re-read state: %v", err)
	}
	var st2 model.CommandState
	if err := yamlv3.Unmarshal(stateData2, &st2); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if st2.PlanStatus != model.PlanStatusFailed {
		t.Errorf("state.PlanStatus = %q, want failed (unchanged)", st2.PlanStatus)
	}

	// Intent file removed after recovery.
	intentPath := completeIntentPath(maestroDir, commandID)
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Error("intent file should be removed after recovery")
	}

	// Sanity: avoid unused-import lint when strings package is dropped.
	_ = strings.TrimSpace
}

// --- Worktree publish guard tests ---

// writeWorktreeState writes a WorktreeCommandState file for the given command.
func writeWorktreeState(t *testing.T, maestroDir, commandID string, integrationStatus model.IntegrationStatus) {
	t.Helper()
	dir := filepath.Join(maestroDir, "state", "worktrees")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("create worktrees dir: %v", err)
	}
	wcs := model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID: commandID,
			Branch:    "integration/" + commandID,
			Status:    integrationStatus,
			CreatedAt: "2025-01-01T00:00:00Z",
			UpdatedAt: "2025-01-01T00:00:00Z",
		},
		CreatedAt: "2025-01-01T00:00:00Z",
		UpdatedAt: "2025-01-01T00:00:00Z",
	}
	path := filepath.Join(dir, commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, wcs); err != nil {
		t.Fatalf("write worktree state: %v", err)
	}
}

func TestComplete_WorktreeEnabled_PartialMerge_Rejected(t *testing.T) {
	commandID := "cmd_0000000050_aabbccdd"
	taskID1 := "task_0000000050_11111111"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()
	cfg.Worktree.Enabled = true

	writeWorktreeState(t, maestroDir, commandID, model.IntegrationStatusPartialMerge)

	_, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "should be rejected",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err == nil {
		t.Fatal("Complete returned nil error, want error for partial_merge worktree status")
	}
	if !strings.Contains(err.Error(), "partial_merge") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "partial_merge")
	}
	if !strings.Contains(err.Error(), "published") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "published")
	}
}

func TestComplete_WorktreeEnabled_Published_Allowed(t *testing.T) {
	commandID := "cmd_0000000051_aabbccdd"
	taskID1 := "task_0000000051_11111111"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()
	cfg.Worktree.Enabled = true

	writeWorktreeState(t, maestroDir, commandID, model.IntegrationStatusPublished)

	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "should succeed",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if result.Status != string(model.PlanStatusCompleted) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusCompleted)
	}
}

func TestComplete_WorktreeDisabled_Allowed(t *testing.T) {
	commandID := "cmd_0000000052_aabbccdd"
	taskID1 := "task_0000000052_11111111"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()
	cfg.Worktree.Enabled = false

	// Write worktree state with partial_merge — should be ignored because worktree is disabled
	writeWorktreeState(t, maestroDir, commandID, model.IntegrationStatusPartialMerge)

	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "should succeed despite partial_merge",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if result.Status != string(model.PlanStatusCompleted) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusCompleted)
	}
}

func TestComplete_WorktreeEnabled_NoStateFile_Allowed(t *testing.T) {
	commandID := "cmd_0000000053_aabbccdd"
	taskID1 := "task_0000000053_11111111"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()
	cfg.Worktree.Enabled = true

	// No worktree state file written — should be allowed

	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "should succeed without worktree state",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if result.Status != string(model.PlanStatusCompleted) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusCompleted)
	}
}

// writeWorktreeStateWithFailedWorkers is a helper that writes a worktree state file
// with the given integration status and commit_failed_workers list.
func writeWorktreeStateWithFailedWorkers(t *testing.T, maestroDir, commandID string, integrationStatus model.IntegrationStatus, failedWorkers []string) {
	t.Helper()
	dir := filepath.Join(maestroDir, "state", "worktrees")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("create worktrees dir: %v", err)
	}
	wcs := model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID: commandID,
			Branch:    "integration/" + commandID,
			Status:    integrationStatus,
			CreatedAt: "2025-01-01T00:00:00Z",
			UpdatedAt: "2025-01-01T00:00:00Z",
		},
		CommitFailedWorkers: failedWorkers,
		CreatedAt:           "2025-01-01T00:00:00Z",
		UpdatedAt:           "2025-01-01T00:00:00Z",
	}
	path := filepath.Join(dir, commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, wcs); err != nil {
		t.Fatalf("write worktree state: %v", err)
	}
}

func TestComplete_WorktreeEnabled_CommitFailedWorkers_Rejected(t *testing.T) {
	commandID := "cmd_0000000054_aabbccdd"
	taskID1 := "task_0000000054_11111111"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()
	cfg.Worktree.Enabled = true

	// Published but has commit_failed_workers — should be rejected
	writeWorktreeStateWithFailedWorkers(t, maestroDir, commandID, model.IntegrationStatusPublished, []string{"worker2", "worker3"})

	_, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "should be rejected",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err == nil {
		t.Fatal("Complete returned nil error, want error for commit_failed_workers")
	}
	if !strings.Contains(err.Error(), "commit failures") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "commit failures")
	}
	if !strings.Contains(err.Error(), "worker2") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "worker2")
	}
	if !strings.Contains(err.Error(), "2 worker(s)") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "2 worker(s)")
	}
}

func TestComplete_WorktreeEnabled_Published_NoFailedWorkers_Allowed(t *testing.T) {
	commandID := "cmd_0000000055_aabbccdd"
	taskID1 := "task_0000000055_11111111"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()
	cfg.Worktree.Enabled = true

	// Published with empty commit_failed_workers — should succeed
	writeWorktreeStateWithFailedWorkers(t, maestroDir, commandID, model.IntegrationStatusPublished, nil)

	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "should succeed",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if result.Status != string(model.PlanStatusCompleted) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusCompleted)
	}
}
