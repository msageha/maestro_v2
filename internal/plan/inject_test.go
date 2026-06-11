package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// addTaskTest is a thin wrapper around AddTask that fills in
// ExpectedPaths and DefinitionOfAbort with valid defaults when the caller
// leaves them blank. The schema requires both fields (REQUIREMENTS.md §S3-1)
// so production callers must supply them, but tests focusing on unrelated
// behavior should not be forced to repeat the boilerplate.
func addTaskTest(opts InjectOptions) (*InjectResult, error) {
	if opts.ExpectedPaths == nil {
		opts.ExpectedPaths = []string{"."}
	}
	if opts.DefinitionOfAbort == nil {
		doa := model.DefaultDefinitionOfAbort()
		opts.DefinitionOfAbort = &doa
	}
	return AddTask(opts)
}

// setupInjectFixture creates a maestro directory with a sealed state containing
// completed tasks suitable for injecting new tasks. Returns (maestroDir, commandID, completedTaskID).
func setupInjectFixture(t *testing.T) (string, string, string) {
	t.Helper()
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000030_aabbccdd"
	taskID1 := "task_0000000030_11111111"
	taskID2 := "task_0000000030_22222222"

	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusSealed,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 2,
			RequiredTaskIDs:   []string{taskID1, taskID2},
			OptionalTaskIDs:   []string{},
			TaskDependencies: map[string][]string{
				taskID1: {},
				taskID2: {taskID1},
			},
			TaskStates: map[string]model.Status{
				taskID1: model.StatusCompleted,
				taskID2: model.StatusCompleted,
			},
			CancelledReasons: make(map[string]string),
			AppliedResultIDs: make(map[string]string),
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

	return maestroDir, commandID, taskID1
}

// TestAddTask_DirectAPI_RejectsMissingSchemaFields exercises AddTask without
// the test wrapper that auto-fills expected_paths / definition_of_abort. This
// closes a coverage gap (review #8): production callers other than the test
// suite (e.g., a future non-CLI client speaking to plan layer in-process) must
// see REQUIREMENTS.md §S3-1 enforcement applied at the API boundary, not only
// at the CLI flag layer. Each subtest leaves exactly one field unset.
func TestAddTask_DirectAPI_RejectsMissingSchemaFields(t *testing.T) {
	maestroDir, commandID, completedTaskID := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	validDOA := model.DefaultDefinitionOfAbort()
	baseOpts := func() InjectOptions {
		return InjectOptions{
			CommandID:          commandID,
			Purpose:            "p with enough length to clear shell-damage minimum",
			Content:            "c with enough length to clear shell-damage minimum",
			AcceptanceCriteria: "ac with enough length to clear shell-damage minimum",
			BloomLevel:         3,
			Required:           true,
			BlockedBy:          []string{completedTaskID},
			MaestroDir:         maestroDir,
			Config:             cfg,
			LockMap:            lm,
			ExpectedPaths:      []string{"internal/example.go"},
			DefinitionOfAbort:  &validDOA,
		}
	}

	cases := []struct {
		name    string
		mutate  func(*InjectOptions)
		errFrag string
	}{
		{
			name:    "expected_paths_nil",
			mutate:  func(o *InjectOptions) { o.ExpectedPaths = nil },
			errFrag: "expected_paths",
		},
		{
			name:    "expected_paths_empty",
			mutate:  func(o *InjectOptions) { o.ExpectedPaths = []string{} },
			errFrag: "expected_paths",
		},
		{
			name:    "definition_of_abort_nil",
			mutate:  func(o *InjectOptions) { o.DefinitionOfAbort = nil },
			errFrag: "definition_of_abort",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := baseOpts()
			tc.mutate(&opts)
			// Call AddTask directly — no wrapper auto-fill — so the validation
			// gate in validateInjectedSchemaFields is the only thing between
			// us and a queue/state mutation.
			res, err := AddTask(opts)
			if err == nil {
				t.Fatalf("expected error for %s, got result=%+v", tc.name, res)
			}
			if !strings.Contains(err.Error(), tc.errFrag) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.errFrag)
			}
		})
	}
}

func TestAddTask_HappyPath(t *testing.T) {
	maestroDir, commandID, completedTaskID := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	result, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve merge conflict",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		BlockedBy:          []string{completedTaskID},
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	if result.TaskID == "" {
		t.Error("result.TaskID is empty")
	}
	if result.Worker == "" {
		t.Error("result.Worker is empty")
	}
	if result.Model == "" {
		t.Error("result.Model is empty")
	}

	// Verify state was persisted
	sm := NewStateManager(maestroDir, lm)
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// New task should be in RequiredTaskIDs
	found := false
	for _, id := range state.RequiredTaskIDs {
		if id == result.TaskID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("new task %s not in RequiredTaskIDs %v", result.TaskID, state.RequiredTaskIDs)
	}

	// ExpectedTaskCount should have increased
	if state.ExpectedTaskCount != 3 {
		t.Errorf("ExpectedTaskCount = %d, want 3", state.ExpectedTaskCount)
	}

	// §2.1: injected tasks enter the lifecycle at `planned` (the queue entry
	// remains `pending` because queue status tracks lease, not lifecycle).
	if state.TaskStates[result.TaskID] != model.StatusPlanned {
		t.Errorf("new task state = %s, want planned", state.TaskStates[result.TaskID])
	}

	// Dependencies should be set
	deps := state.TaskDependencies[result.TaskID]
	if len(deps) != 1 || deps[0] != completedTaskID {
		t.Errorf("dependencies = %v, want [%s]", deps, completedTaskID)
	}

	// PlanVersion should have incremented
	if state.PlanVersion != 2 {
		t.Errorf("PlanVersion = %d, want 2", state.PlanVersion)
	}

	// Queue entry written
	totalQueueTasks := 0
	for i := 1; i <= 2; i++ {
		queueFile := filepath.Join(maestroDir, "queue", fmt.Sprintf("worker%d.yaml", i))
		data, err := os.ReadFile(queueFile)
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if yamlv3.Unmarshal(data, &tq) != nil {
			continue
		}
		for _, task := range tq.Tasks {
			if task.ID == result.TaskID {
				totalQueueTasks++
				if task.Status != model.StatusPending {
					t.Errorf("queue task status = %s, want pending", task.Status)
				}
				if task.Purpose != "resolve merge conflict" {
					t.Errorf("queue task purpose = %q, want %q", task.Purpose, "resolve merge conflict")
				}
			}
		}
	}
	if totalQueueTasks != 1 {
		t.Errorf("queue entries for new task = %d, want 1", totalQueueTasks)
	}
}

func TestAddTask_Optional(t *testing.T) {
	maestroDir, commandID, _ := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	result, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "optional cleanup",
		Content:            "cleanup leftover files",
		AcceptanceCriteria: "no leftover files",
		BloomLevel:         2,
		Required:           false,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	sm := NewStateManager(maestroDir, lm)
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// New task should be in OptionalTaskIDs, not RequiredTaskIDs
	foundInOptional := false
	for _, id := range state.OptionalTaskIDs {
		if id == result.TaskID {
			foundInOptional = true
			break
		}
	}
	if !foundInOptional {
		t.Errorf("new task %s not in OptionalTaskIDs %v", result.TaskID, state.OptionalTaskIDs)
	}

	foundInRequired := false
	for _, id := range state.RequiredTaskIDs {
		if id == result.TaskID {
			foundInRequired = true
			break
		}
	}
	if foundInRequired {
		t.Errorf("optional task %s should not be in RequiredTaskIDs", result.TaskID)
	}
}

func TestAddTask_NilLockMap(t *testing.T) {
	_, err := addTaskTest(InjectOptions{
		CommandID:  "cmd_0000000030_aabbccdd",
		MaestroDir: t.TempDir(),
		LockMap:    nil,
	})
	if err == nil {
		t.Fatal("expected error for nil LockMap")
	}
}

// SaveState failure must roll back the queue entry WITHOUT re-acquiring the
// already-held queue lock: lock.MutexMap is non-reentrant, so passing
// opts.LockMap into rollbackRetryQueueEntries while AddTask still holds
// `queue:<worker>` (and `state:<command>`) self-deadlocked the daemon.
func TestAddTask_SaveStateFailureRollsBackWithoutDeadlock(t *testing.T) {
	maestroDir, commandID, completedTaskID := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// Make the state directory read-only so SaveState (AtomicWrite of the
	// command state) fails after the queue entry has been written.
	stateDir := filepath.Join(maestroDir, "state", "commands")
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	type outcome struct {
		result *InjectResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := addTaskTest(InjectOptions{
			CommandID:          commandID,
			Purpose:            "savestate failure rollback",
			Content:            "should be rolled back",
			AcceptanceCriteria: "n/a",
			BloomLevel:         3,
			Required:           true,
			BlockedBy:          []string{completedTaskID},
			MaestroDir:         maestroDir,
			Config:             cfg,
			LockMap:            lm,
		})
		done <- outcome{res, err}
	}()

	select {
	case out := <-done:
		if out.err == nil {
			t.Fatalf("expected SaveState failure, got success: %+v", out.result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("AddTask deadlocked in SaveState-failure rollback (queue lock re-acquired while already held)")
	}

	// The queue entry written before SaveState must have been rolled back.
	for i := 1; i <= 2; i++ {
		queueFile := filepath.Join(maestroDir, "queue", fmt.Sprintf("worker%d.yaml", i))
		data, err := os.ReadFile(queueFile)
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			t.Fatalf("parse queue %s: %v", queueFile, err)
		}
		for _, task := range tq.Tasks {
			if task.Purpose == "savestate failure rollback" {
				t.Errorf("queue entry for failed AddTask should have been rolled back (worker%d)", i)
			}
		}
	}
}

func TestAddTask_NotSealed(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000031_aabbccdd"
	cfg := testConfig()
	lm := lock.NewMutexMap()

	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusCompleted,
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

	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "test",
		Content:            "test",
		AcceptanceCriteria: "test",
		BloomLevel:         3,
		Required:           true,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected error for non-sealed plan")
	}
}

func TestAddTask_InvalidBlockedBy(t *testing.T) {
	maestroDir, commandID, _ := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "test",
		Content:            "test",
		AcceptanceCriteria: "test",
		BloomLevel:         3,
		Required:           true,
		BlockedBy:          []string{"task_0000000099_nonexist"},
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected error for invalid blocked_by reference")
	}
}

func TestAddTask_InvalidBloomLevel(t *testing.T) {
	maestroDir, commandID, _ := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "test",
		Content:            "test",
		AcceptanceCriteria: "test",
		BloomLevel:         7,
		Required:           true,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected error for invalid bloom level")
	}
}

// TestAddTask_RejectsShellDamagedContent verifies that AddTask refuses
// payloads whose purpose / content / acceptance_criteria are so short they
// are almost certainly the result of shell backtick or `$()` expansion
// inside double quotes truncating the operator's intent. A multi-byte
// leftover (e.g. "x" from a failed command substitution) is blocked before
// it lands in the queue as a malformed task that would drive the Planner
// into a repair loop.
func TestAddTask_RejectsShellDamagedContent(t *testing.T) {
	maestroDir, commandID, _ := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	tests := []struct {
		name string
		opts InjectOptions
	}{
		{
			name: "shell-damaged purpose (1 byte)",
			opts: InjectOptions{
				CommandID: commandID, Purpose: "x",
				Content:            "content with sufficient length for the content-min",
				AcceptanceCriteria: "acceptance_ok", BloomLevel: 3,
				Required: true, MaestroDir: maestroDir, Config: cfg, LockMap: lm,
			},
		},
		{
			name: "shell-damaged content (2 bytes)",
			opts: InjectOptions{
				CommandID: commandID, Purpose: "purpose_ok",
				Content:            "ok",
				AcceptanceCriteria: "acceptance_ok", BloomLevel: 3,
				Required: true, MaestroDir: maestroDir, Config: cfg, LockMap: lm,
			},
		},
		{
			name: "shell-damaged acceptance_criteria (2 bytes)",
			opts: InjectOptions{
				CommandID: commandID, Purpose: "purpose_ok",
				Content:            "content with sufficient length",
				AcceptanceCriteria: "ok", BloomLevel: 3,
				Required: true, MaestroDir: maestroDir, Config: cfg, LockMap: lm,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := addTaskTest(tt.opts)
			if err == nil {
				t.Fatal("expected error for shell-damaged field")
			}
			if !strings.Contains(err.Error(), "too short") {
				t.Errorf("expected 'too short' in error, got: %v", err)
			}
			if !strings.Contains(err.Error(), "shell quoting") {
				t.Errorf("error should hint at shell quoting as the likely cause, got: %v", err)
			}
		})
	}
}

func TestAddTask_MissingRequiredFields(t *testing.T) {
	maestroDir, commandID, _ := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	tests := []struct {
		name string
		opts InjectOptions
	}{
		{
			name: "missing purpose",
			opts: InjectOptions{
				CommandID: commandID, Content: "c", AcceptanceCriteria: "ac", BloomLevel: 3,
				Required: true, MaestroDir: maestroDir, Config: cfg, LockMap: lm,
			},
		},
		{
			name: "missing content",
			opts: InjectOptions{
				CommandID: commandID, Purpose: "p", AcceptanceCriteria: "ac", BloomLevel: 3,
				Required: true, MaestroDir: maestroDir, Config: cfg, LockMap: lm,
			},
		},
		{
			name: "missing acceptance_criteria",
			opts: InjectOptions{
				CommandID: commandID, Purpose: "p", Content: "c", BloomLevel: 3,
				Required: true, MaestroDir: maestroDir, Config: cfg, LockMap: lm,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := addTaskTest(tt.opts)
			if err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

func TestAddTask_NoDependencies(t *testing.T) {
	maestroDir, commandID, _ := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	result, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "independent task",
		Content:            "do something",
		AcceptanceCriteria: "done",
		BloomLevel:         2,
		Required:           true,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	sm := NewStateManager(maestroDir, lm)
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// No dependencies should be set
	deps := state.TaskDependencies[result.TaskID]
	if len(deps) != 0 {
		t.Errorf("dependencies = %v, want empty", deps)
	}
}

func TestAddTask_OriginalTasksUnchanged(t *testing.T) {
	maestroDir, commandID, completedTaskID := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "new task",
		Content:            "new content",
		AcceptanceCriteria: "new ac",
		BloomLevel:         3,
		Required:           true,
		BlockedBy:          []string{completedTaskID},
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	sm := NewStateManager(maestroDir, lm)
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// Original tasks should still be completed
	if state.TaskStates[completedTaskID] != model.StatusCompleted {
		t.Errorf("original task %s state = %s, want completed", completedTaskID, state.TaskStates[completedTaskID])
	}

	// Original required tasks should still be present
	originalTaskID2 := "task_0000000030_22222222"
	if state.TaskStates[originalTaskID2] != model.StatusCompleted {
		t.Errorf("original task %s state = %s, want completed", originalTaskID2, state.TaskStates[originalTaskID2])
	}
}

func TestAddTask_IdempotencyKey_Dedup(t *testing.T) {
	maestroDir, commandID, completedTaskID := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	opts := InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve merge conflict",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		BlockedBy:          []string{completedTaskID},
		IdempotencyKey:     "conflict-resolution-abc123",
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	}

	// First call: creates a new task
	result1, err := addTaskTest(opts)
	if err != nil {
		t.Fatalf("first AddTask returned error: %v", err)
	}
	if result1.TaskID == "" {
		t.Fatal("first AddTask returned empty TaskID")
	}
	if result1.Deduplicated {
		t.Error("first AddTask should not be deduplicated")
	}

	// Second call with same idempotency key: should return existing task
	result2, err := addTaskTest(opts)
	if err != nil {
		t.Fatalf("second AddTask returned error: %v", err)
	}
	if result2.TaskID != result1.TaskID {
		t.Errorf("second AddTask returned different TaskID: got %s, want %s", result2.TaskID, result1.TaskID)
	}
	if !result2.Deduplicated {
		t.Error("second AddTask should be deduplicated")
	}
	if result2.Worker == "" {
		t.Error("second AddTask returned empty Worker")
	}

	// Verify state: only one task was created
	sm := NewStateManager(maestroDir, lm)
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// ExpectedTaskCount should be 3 (2 original + 1 new), not 4
	if state.ExpectedTaskCount != 3 {
		t.Errorf("ExpectedTaskCount = %d, want 3 (should not double-count)", state.ExpectedTaskCount)
	}

	// Idempotency key should be recorded
	if state.IdempotencyKeys == nil {
		t.Fatal("IdempotencyKeys map is nil")
	}
	if state.IdempotencyKeys["conflict-resolution-abc123"] != result1.TaskID {
		t.Errorf("IdempotencyKeys[key] = %s, want %s", state.IdempotencyKeys["conflict-resolution-abc123"], result1.TaskID)
	}
}

func TestAddTask_IdempotencyKey_DifferentKeys(t *testing.T) {
	maestroDir, commandID, completedTaskID := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	baseOpts := InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve merge conflict",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		BlockedBy:          []string{completedTaskID},
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	}

	// First call with key A
	opts1 := baseOpts
	opts1.IdempotencyKey = "key-A"
	result1, err := addTaskTest(opts1)
	if err != nil {
		t.Fatalf("first AddTask returned error: %v", err)
	}

	// Second call with key B: should create a new task
	opts2 := baseOpts
	opts2.IdempotencyKey = "key-B"
	result2, err := addTaskTest(opts2)
	if err != nil {
		t.Fatalf("second AddTask returned error: %v", err)
	}

	if result1.TaskID == result2.TaskID {
		t.Error("different idempotency keys should create different tasks")
	}
	if result2.Deduplicated {
		t.Error("second AddTask with different key should not be deduplicated")
	}
}

func TestAddTask_NoIdempotencyKey_NoDedupe(t *testing.T) {
	maestroDir, commandID, completedTaskID := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	opts := InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve merge conflict",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		BlockedBy:          []string{completedTaskID},
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	}

	// Two calls without idempotency key: both should create tasks
	result1, err := addTaskTest(opts)
	if err != nil {
		t.Fatalf("first AddTask returned error: %v", err)
	}
	result2, err := addTaskTest(opts)
	if err != nil {
		t.Fatalf("second AddTask returned error: %v", err)
	}

	if result1.TaskID == result2.TaskID {
		t.Error("calls without idempotency key should create separate tasks")
	}
}

// setupInjectFixtureWithPhases creates a fixture with multiple phases (some terminal).
// Returns (maestroDir, commandID, taskID in phase1, phase1 ID, phase2 ID).
func setupInjectFixtureWithPhases(t *testing.T) (string, string, string, string, string) {
	t.Helper()
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000050_aabbccdd"
	taskID1 := "task_0000000050_11111111"
	taskID2 := "task_0000000050_22222222"
	phase1ID := "phase_001"
	phase2ID := "phase_002"

	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusSealed,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 2,
			RequiredTaskIDs:   []string{taskID1, taskID2},
			OptionalTaskIDs:   []string{},
			TaskDependencies: map[string][]string{
				taskID1: {},
				taskID2: {taskID1},
			},
			TaskStates: map[string]model.Status{
				taskID1: model.StatusCompleted,
				taskID2: model.StatusCompleted,
			},
			CancelledReasons: make(map[string]string),
			AppliedResultIDs: make(map[string]string),
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID:     phase1ID,
					Name:        "phase1",
					Type:        "concrete",
					Status:      model.PhaseStatusCompleted,
					TaskIDs:     []string{taskID1},
					CompletedAt: strPtr("2025-01-01T00:10:00Z"),
				},
				{
					PhaseID:     phase2ID,
					Name:        "phase2",
					Type:        "concrete",
					Status:      model.PhaseStatusCompleted,
					TaskIDs:     []string{taskID2},
					CompletedAt: strPtr("2025-01-01T00:20:00Z"),
				},
			},
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

	return maestroDir, commandID, taskID1, phase1ID, phase2ID
}

// TestAddTask_RejectsExceedingMaxTasks pins the policy that an
// ordinary add-task injection must be rejected when the target phase
// has reached its declared max_tasks cap. Report 2026-05-03 issue-3
// observed cycle2 silently inflating from max_tasks=2 to 5 tasks
// because the daemon previously logged an advisory warning instead of
// failing the call. The hard reject is the load-bearing constraint;
// the Planner can recover by replacing an existing task via
// add-retry-task or by re-targeting another phase.
func TestAddTask_RejectsExceedingMaxTasks(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000060_maxtasks"
	taskID1 := "task_0000000060_11111111"
	phaseID := "phase_with_cap"

	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusSealed,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{taskID1},
			OptionalTaskIDs:   []string{},
			TaskDependencies:  map[string][]string{taskID1: {}},
			TaskStates: map[string]model.Status{
				taskID1: model.StatusCompleted,
			},
			CancelledReasons: make(map[string]string),
			AppliedResultIDs: make(map[string]string),
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: phaseID,
					Name:    "capped",
					Type:    "concrete",
					Status:  model.PhaseStatusActive,
					TaskIDs: []string{taskID1},
					Constraints: &model.PhaseConstraints{
						MaxTasks: 1, // cap already saturated by taskID1
					},
				},
			},
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

	cfg := testConfig()
	lm := lock.NewMutexMap()

	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		TargetPhase:        phaseID,
		Purpose:            "p with enough length to clear shell-damage minimum",
		Content:            "c with enough length to clear shell-damage minimum",
		AcceptanceCriteria: "ac with enough length to clear shell-damage minimum",
		BloomLevel:         3,
		Required:           true,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected add-task to be rejected on a phase already at max_tasks, got nil")
	}
	var pve *planValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *planValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(pve.Msg, "max_tasks") {
		t.Errorf("error message = %q, want to mention 'max_tasks'", pve.Msg)
	}

	// State must be unchanged: the rejection must not leave a half-applied
	// task in the phase.
	updated, _ := plan_loadStateForInjectTest(t, maestroDir, commandID)
	if updated == nil {
		t.Fatal("expected reloadable command state after injection")
	}
	if len(updated.Phases) != 1 || len(updated.Phases[0].TaskIDs) != 1 {
		t.Errorf("rejected injection must not mutate phase tasks; got %+v", updated.Phases)
	}
}

// TestAddTask_RecoveryExemptFromMaxTasks pins that recovery
// injections (RunOnMain post-publish verification, RunOnIntegration
// publish_conflict resolution) bypass the max_tasks cap. Recovery
// flows must remain available even when the Planner already filled
// the phase to its declared parallelism budget.
func TestAddTask_RecoveryExemptFromMaxTasks(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000061_maxtasks_recovery"
	taskID1 := "task_0000000061_11111111"
	phaseID := "phase_with_cap"

	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusSealed,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{taskID1},
			OptionalTaskIDs:   []string{},
			TaskDependencies:  map[string][]string{taskID1: {}},
			TaskStates: map[string]model.Status{
				taskID1: model.StatusCompleted,
			},
			CancelledReasons: make(map[string]string),
			AppliedResultIDs: make(map[string]string),
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: phaseID,
					Name:    "capped",
					Type:    "concrete",
					Status:  model.PhaseStatusActive,
					TaskIDs: []string{taskID1},
					Constraints: &model.PhaseConstraints{
						MaxTasks: 1, // cap already saturated
					},
				},
			},
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

	// Materialise a worktree state file so validateInjectRequest does not
	// reject the RunOnIntegration path on the "integration cleaned up
	// after publish" guard. The contents are minimal — only the file's
	// existence matters for that gate.
	wtPath := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		t.Fatalf("mkdir worktree state dir: %v", err)
	}
	if err := os.WriteFile(wtPath, []byte("schema_version: 1\nfile_type: state_worktree\ncommand_id: "+commandID+"\n"), 0o644); err != nil {
		t.Fatalf("write worktree state stub: %v", err)
	}

	cfg := testConfig()
	lm := lock.NewMutexMap()

	result, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		TargetPhase:        phaseID,
		Purpose:            "publish_conflict resolution task — recovery path",
		Content:            "resolve the conflict via add-task on the integration worktree",
		AcceptanceCriteria: "merge succeeds and integration is publishable",
		BloomLevel:         3,
		Required:           true,
		RunOnIntegration:   true, // recovery path → exempt from max_tasks
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("expected RunOnIntegration recovery injection to bypass max_tasks; got: %v", err)
	}
	if result == nil || result.TaskID == "" {
		t.Fatalf("expected a new task to be added, got %+v", result)
	}
	updated, _ := plan_loadStateForInjectTest(t, maestroDir, commandID)
	if updated == nil {
		t.Fatal("expected reloadable command state after injection")
	}
	if len(updated.Phases) != 1 || len(updated.Phases[0].TaskIDs) != 2 {
		t.Errorf("expected phase to hold 2 tasks (recovery exempt from cap=1), got %+v", updated.Phases)
	}
}

// plan_loadStateForInjectTest is a thin reader used by the advisory-cap test
// to confirm the persisted shape after add-task. Kept inline to avoid
// wiring a new export from plan; deliberate naming prefix prevents test
// name collision elsewhere.
func plan_loadStateForInjectTest(t *testing.T, maestroDir, commandID string) (*model.CommandState, error) {
	t.Helper()
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}
	var st model.CommandState
	if err := yamlv3.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func TestAddTask_TargetPhase_PlacedCorrectly(t *testing.T) {
	maestroDir, commandID, _, _, phase2ID := setupInjectFixtureWithPhases(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	result, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve conflict in phase2",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		TargetPhase:        phase2ID,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	sm := NewStateManager(maestroDir, lm)
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// Task should be placed in phase2, not phase 0
	foundInPhase2 := false
	for _, tid := range state.Phases[1].TaskIDs {
		if tid == result.TaskID {
			foundInPhase2 = true
			break
		}
	}
	if !foundInPhase2 {
		t.Errorf("task %s not in phase2 (target); phase2 TaskIDs: %v", result.TaskID, state.Phases[1].TaskIDs)
	}

	// Task should NOT be in phase1
	for _, tid := range state.Phases[0].TaskIDs {
		if tid == result.TaskID {
			t.Errorf("task %s unexpectedly in phase1; should only be in target phase2", result.TaskID)
		}
	}
}

func TestAddTask_TargetPhase_NotFound(t *testing.T) {
	maestroDir, commandID, _, _, _ := setupInjectFixtureWithPhases(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve conflict",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		TargetPhase:        "phase_nonexistent",
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent target_phase")
	}
}

func TestAddTask_NoTargetPhase_AllTerminal_ReturnsError(t *testing.T) {
	maestroDir, commandID, _, _, _ := setupInjectFixtureWithPhases(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// Both phases are terminal (completed). Without TargetPhase, should return error.
	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "independent task",
		Content:            "do something",
		AcceptanceCriteria: "done",
		BloomLevel:         2,
		Required:           true,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected error when all phases are terminal and no target_phase is set")
	}

	var pve *planValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *planValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(pve.Msg, "all phases are terminal") {
		t.Errorf("error message = %q, want to contain 'all phases are terminal'", pve.Msg)
	}
}

// TestAddTask_RunOnMain_PostPublishRejected pins the policy that a
// RunOnMain (or RunOnIntegration) injection must be rejected once every
// phase is terminal on a sealed plan: the deferred completion handler
// removes the integration worktree as soon as publish succeeds, so a
// post-publish injection has nowhere to dispatch and would dead-letter
// on integration_branch_check_failed before flipping plan_status to
// failed (Report 2026-05-03 issue-2). The autonomous orchestration
// contract treats post-publish recovery as "submit a fresh command",
// not "extend a published plan".
func TestAddTask_RunOnMain_PostPublishRejected(t *testing.T) {
	maestroDir, commandID, _, _, _ := setupInjectFixtureWithPhases(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "post-publish final verification",
		Content:            "run go test ./... on main branch",
		AcceptanceCriteria: "all tests pass on main",
		BloomLevel:         3,
		Required:           false,
		RunOnMain:          true,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected RunOnMain injection on a fully-terminal sealed plan to be rejected, got nil")
	}
	var pve *planValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *planValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(pve.Msg, "integration worktree has been cleaned up") {
		t.Errorf("error message = %q, want to mention 'integration worktree has been cleaned up'", pve.Msg)
	}
}

// TestAddTask_RunOnIntegration_WorktreeDisabled pins the policy that a
// RunOnIntegration injection cannot proceed when worktree mode is
// disabled. Without a worktree there is no integration branch to
// dispatch against, so the injection must fail at the API boundary
// rather than flowing through to a delayed dead-letter.
func TestAddTask_RunOnIntegration_WorktreeDisabled(t *testing.T) {
	maestroDir, commandID, _, _, _ := setupInjectFixtureWithPhases(t)
	cfg := testConfig()
	cfg.Worktree.Enabled = false
	lm := lock.NewMutexMap()

	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "publish_conflict resolution",
		Content:            "resolve conflict on integration branch",
		AcceptanceCriteria: "merge succeeds",
		BloomLevel:         3,
		Required:           true,
		RunOnIntegration:   true,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected RunOnIntegration injection without worktree mode to be rejected, got nil")
	}
	var pve *planValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *planValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(pve.Msg, "worktree mode is disabled") {
		t.Errorf("error message = %q, want to mention 'worktree mode is disabled'", pve.Msg)
	}
}

func TestAddTask_TargetPhase_AllTerminal(t *testing.T) {
	maestroDir, commandID, _, phase1ID, _ := setupInjectFixtureWithPhases(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// With TargetPhase set, even if all phases are terminal, task goes to the specified phase
	result, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve conflict in completed phase",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		TargetPhase:        phase1ID,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	sm := NewStateManager(maestroDir, lm)
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// Task should be placed in phase1 despite it being completed
	foundInPhase1 := false
	for _, tid := range state.Phases[0].TaskIDs {
		if tid == result.TaskID {
			foundInPhase1 = true
			break
		}
	}
	if !foundInPhase1 {
		t.Errorf("task %s not in phase1 (target); phase1 TaskIDs: %v", result.TaskID, state.Phases[0].TaskIDs)
	}
}

func TestAddTask_TargetWorkerID_FallbackToWorkerPhase(t *testing.T) {
	// Scenario: conflict resolution task is injected with TargetWorkerID="worker1"
	// but no TargetPhase and no BlockedBy. The worker has an existing task in
	// phase2 (parallel-edit). The new task should land in phase2, not phase1 (setup).
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000060_aabbccdd"
	taskID1 := "task_0000000060_11111111"
	taskID2 := "task_0000000060_22222222"
	phase1ID := "phase_001"
	phase2ID := "phase_002"

	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusSealed,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 2,
			RequiredTaskIDs:   []string{taskID1, taskID2},
			OptionalTaskIDs:   []string{},
			TaskDependencies: map[string][]string{
				taskID1: {},
				taskID2: {taskID1},
			},
			TaskStates: map[string]model.Status{
				taskID1: model.StatusCompleted,
				taskID2: model.StatusInProgress,
			},
			CancelledReasons: make(map[string]string),
			AppliedResultIDs: make(map[string]string),
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: phase1ID,
					Name:    "setup",
					Type:    "concrete",
					Status:  model.PhaseStatusCompleted,
					TaskIDs: []string{taskID1},
				},
				{
					PhaseID: phase2ID,
					Name:    "parallel-edit",
					Type:    "concrete",
					Status:  model.PhaseStatusActive,
					TaskIDs: []string{taskID2},
				},
			},
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

	// Write worker1 queue with taskID2 assigned to it (belongs to phase2)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:        taskID2,
				CommandID: commandID,
				Purpose:   "edit files",
				Content:   "edit some files",
				Status:    model.StatusInProgress,
				CreatedAt: "2025-01-01T00:00:00Z",
				UpdatedAt: "2025-01-01T00:00:00Z",
			},
		},
	}
	queueFile := filepath.Join(maestroDir, "queue", "worker1.yaml")
	if err := yamlutil.AtomicWrite(queueFile, tq); err != nil {
		t.Fatalf("write worker1 queue: %v", err)
	}

	cfg := testConfig()
	lm := lock.NewMutexMap()

	result, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve conflict for worker1",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		TargetWorkerID:     "worker1",
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	sm := NewStateManager(maestroDir, lm)
	reloaded, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// Task should be placed in phase2 (parallel-edit), not phase1 (setup)
	foundInPhase2 := false
	for _, tid := range reloaded.Phases[1].TaskIDs {
		if tid == result.TaskID {
			foundInPhase2 = true
			break
		}
	}
	if !foundInPhase2 {
		t.Errorf("task %s not in phase2 (parallel-edit); phase2 TaskIDs: %v", result.TaskID, reloaded.Phases[1].TaskIDs)
	}

	// Task should NOT be in phase1 (setup)
	for _, tid := range reloaded.Phases[0].TaskIDs {
		if tid == result.TaskID {
			t.Errorf("task %s unexpectedly in phase1 (setup); should be in phase2 (parallel-edit)", result.TaskID)
		}
	}
}

func TestAddTask_TargetWorkerID_NoQueueMatch_FallsBackToFirstNonTerminal(t *testing.T) {
	// When TargetWorkerID is set but the worker has no tasks in the queue for
	// this command, the fallback should behave like the original logic (first
	// non-terminal phase).
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000061_aabbccdd"
	taskID1 := "task_0000000061_11111111"
	phase1ID := "phase_001"
	phase2ID := "phase_002"

	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusSealed,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{taskID1},
			OptionalTaskIDs:   []string{},
			TaskDependencies:  map[string][]string{taskID1: {}},
			TaskStates:        map[string]model.Status{taskID1: model.StatusCompleted},
			CancelledReasons:  make(map[string]string),
			AppliedResultIDs:  make(map[string]string),
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: phase1ID,
					Name:    "setup",
					Type:    "concrete",
					Status:  model.PhaseStatusCompleted,
					TaskIDs: []string{taskID1},
				},
				{
					PhaseID: phase2ID,
					Name:    "parallel-edit",
					Type:    "concrete",
					Status:  model.PhaseStatusActive,
					TaskIDs: []string{},
				},
			},
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

	cfg := testConfig()
	lm := lock.NewMutexMap()

	result, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve conflict for worker2",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		TargetWorkerID:     "worker2",
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	sm := NewStateManager(maestroDir, lm)
	reloaded, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// phase2 is the first non-terminal phase (active), so fallback should place here
	foundInPhase2 := false
	for _, tid := range reloaded.Phases[1].TaskIDs {
		if tid == result.TaskID {
			foundInPhase2 = true
			break
		}
	}
	if !foundInPhase2 {
		t.Errorf("task %s not in phase2 (first non-terminal); phase2 TaskIDs: %v", result.TaskID, reloaded.Phases[1].TaskIDs)
	}
}

func TestAddTask_TargetWorkerID_AllTerminal_ReturnsError(t *testing.T) {
	// When TargetWorkerID is set but the worker has no tasks in the queue,
	// and all phases are terminal, should return error instead of falling back to phase 0.
	maestroDir, commandID, _, _, _ := setupInjectFixtureWithPhases(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve conflict for worker2",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		TargetWorkerID:     "worker2",
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected error when all phases are terminal and TargetWorkerID has no queue match")
	}

	var pve *planValidationError
	if !errors.As(err, &pve) {
		t.Fatalf("expected *planValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(pve.Msg, "all phases are terminal") {
		t.Errorf("error message = %q, want to contain 'all phases are terminal'", pve.Msg)
	}
}

func TestAddTask_CompletedPhase_ReopensOnInject(t *testing.T) {
	maestroDir, commandID, _, phase1ID, _ := setupInjectFixtureWithPhases(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// Verify precondition: phase1 is completed with CompletedAt set
	sm := NewStateManager(maestroDir, lm)
	stateBefore, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if stateBefore.Phases[0].Status != model.PhaseStatusCompleted {
		t.Fatalf("precondition: phase1 status = %s, want completed", stateBefore.Phases[0].Status)
	}
	if stateBefore.Phases[0].CompletedAt == nil {
		t.Fatal("precondition: phase1 CompletedAt should be set")
	}

	// Inject task into the completed phase
	result, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve conflict in completed phase",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		TargetPhase:        phase1ID,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	stateAfter, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	phase := stateAfter.Phases[0]

	// Phase status should be reopened to active
	if phase.Status != model.PhaseStatusActive {
		t.Errorf("phase status = %s, want active", phase.Status)
	}

	// CompletedAt should be cleared
	if phase.CompletedAt != nil {
		t.Errorf("phase CompletedAt = %v, want nil", *phase.CompletedAt)
	}

	// ReopenedAt should be set
	if phase.ReopenedAt == nil {
		t.Error("phase ReopenedAt should be set after reopen")
	}

	// Task should be in the phase
	foundInPhase := false
	for _, tid := range phase.TaskIDs {
		if tid == result.TaskID {
			foundInPhase = true
			break
		}
	}
	if !foundInPhase {
		t.Errorf("task %s not in phase1; TaskIDs: %v", result.TaskID, phase.TaskIDs)
	}

	// The other completed phase should remain unchanged
	phase2 := stateAfter.Phases[1]
	if phase2.Status != model.PhaseStatusCompleted {
		t.Errorf("phase2 status = %s, want completed (should be unaffected)", phase2.Status)
	}
	if phase2.CompletedAt == nil {
		t.Error("phase2 CompletedAt should remain set")
	}
}

func TestAddTask_BlockedBy_CompletedPhase_ReopensOnInject(t *testing.T) {
	maestroDir, commandID, taskID1, phase1ID, _ := setupInjectFixtureWithPhases(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// Inject task with BlockedBy referencing a task in completed phase1
	_, err := addTaskTest(InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve conflict via blocked_by",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		BlockedBy:          []string{taskID1},
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	sm := NewStateManager(maestroDir, lm)
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// phase1 (containing taskID1) should be reopened
	phase := state.Phases[0]
	if phase.PhaseID != phase1ID {
		t.Fatalf("unexpected phase at index 0: %s", phase.PhaseID)
	}
	if phase.Status != model.PhaseStatusActive {
		t.Errorf("phase1 status = %s, want active", phase.Status)
	}
	if phase.CompletedAt != nil {
		t.Errorf("phase1 CompletedAt = %v, want nil", *phase.CompletedAt)
	}
	if phase.ReopenedAt == nil {
		t.Error("phase1 ReopenedAt should be set")
	}
}

func strPtr(s string) *string {
	return &s
}
