package model

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefinitionOfAbort_YAMLRoundTrip(t *testing.T) {
	t.Parallel()
	doa := DefinitionOfAbort{
		MaxRepairCount:            5,
		MaxWallClockSec:           3600,
		ExplicitFailureConditions: []string{"compilation error", "test timeout"},
	}

	data, err := yaml.Marshal(&doa)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded DefinitionOfAbort
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.MaxRepairCount != 5 {
		t.Errorf("max_repair_count: got %d, want 5", decoded.MaxRepairCount)
	}
	if decoded.MaxWallClockSec != 3600 {
		t.Errorf("max_wall_clock_sec: got %d, want 3600", decoded.MaxWallClockSec)
	}
	if len(decoded.ExplicitFailureConditions) != 2 {
		t.Fatalf("explicit_failure_conditions: got %d items, want 2", len(decoded.ExplicitFailureConditions))
	}
	if decoded.ExplicitFailureConditions[0] != "compilation error" {
		t.Errorf("explicit_failure_conditions[0]: got %q", decoded.ExplicitFailureConditions[0])
	}
}

func TestDefaultDefinitionOfAbort(t *testing.T) {
	t.Parallel()
	doa := DefaultDefinitionOfAbort()

	if doa.MaxRepairCount != 3 {
		t.Errorf("MaxRepairCount: got %d, want 3", doa.MaxRepairCount)
	}
	if doa.MaxWallClockSec != 1800 {
		t.Errorf("MaxWallClockSec: got %d, want 1800", doa.MaxWallClockSec)
	}
	if doa.ExplicitFailureConditions != nil {
		t.Errorf("ExplicitFailureConditions: got %v, want nil", doa.ExplicitFailureConditions)
	}
}

func TestTask_NewFields_YAMLRoundTrip(t *testing.T) {
	t.Parallel()
	task := Task{
		ID:                 "task_001",
		CommandID:          "cmd_001",
		Purpose:            "test new fields",
		Content:            "test content",
		AcceptanceCriteria: "legacy criteria",
		DefinitionOfDone:   []string{"tests pass", "no lint errors"},
		ExpectedPaths:      []string{"internal/model/queue.go", "internal/model/queue_test.go"},
		DefinitionOfAbort: &DefinitionOfAbort{
			MaxRepairCount:            2,
			MaxWallClockSec:           900,
			ExplicitFailureConditions: []string{"panic"},
		},
		Priority:   100,
		Status:     StatusPending,
		LeaseEpoch: 0,
		CreatedAt:  "2026-04-10T10:00:00+09:00",
		UpdatedAt:  "2026-04-10T10:00:00+09:00",
	}

	data, err := yaml.Marshal(&task)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded Task
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// ExpectedPaths
	if len(decoded.ExpectedPaths) != 2 {
		t.Fatalf("expected_paths: got %d items, want 2", len(decoded.ExpectedPaths))
	}
	if decoded.ExpectedPaths[0] != "internal/model/queue.go" {
		t.Errorf("expected_paths[0]: got %q", decoded.ExpectedPaths[0])
	}

	// DefinitionOfDone
	if len(decoded.DefinitionOfDone) != 2 {
		t.Fatalf("definition_of_done: got %d items, want 2", len(decoded.DefinitionOfDone))
	}
	if decoded.DefinitionOfDone[0] != "tests pass" {
		t.Errorf("definition_of_done[0]: got %q", decoded.DefinitionOfDone[0])
	}

	// DefinitionOfAbort
	if decoded.DefinitionOfAbort == nil {
		t.Fatal("definition_of_abort: expected non-nil")
	}
	if decoded.DefinitionOfAbort.MaxRepairCount != 2 {
		t.Errorf("definition_of_abort.max_repair_count: got %d, want 2", decoded.DefinitionOfAbort.MaxRepairCount)
	}
	if decoded.DefinitionOfAbort.MaxWallClockSec != 900 {
		t.Errorf("definition_of_abort.max_wall_clock_sec: got %d, want 900", decoded.DefinitionOfAbort.MaxWallClockSec)
	}
	if len(decoded.DefinitionOfAbort.ExplicitFailureConditions) != 1 {
		t.Fatalf("explicit_failure_conditions: got %d, want 1", len(decoded.DefinitionOfAbort.ExplicitFailureConditions))
	}

	// AcceptanceCriteria preserved
	if decoded.AcceptanceCriteria != "legacy criteria" {
		t.Errorf("acceptance_criteria: got %q", decoded.AcceptanceCriteria)
	}
}

func TestTask_NewFields_Omitempty(t *testing.T) {
	t.Parallel()
	task := Task{
		ID:                 "task_002",
		CommandID:          "cmd_001",
		AcceptanceCriteria: "criteria",
		Status:             StatusPending,
		CreatedAt:          "2026-04-10T10:00:00+09:00",
		UpdatedAt:          "2026-04-10T10:00:00+09:00",
	}

	data, err := yaml.Marshal(&task)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded Task
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// expected_paths is no longer omitempty: nil marshals as "expected_paths: []",
	// which round-trips to an empty (non-nil) slice.
	if decoded.ExpectedPaths == nil {
		t.Errorf("expected_paths: got nil, want empty slice")
	}
	if len(decoded.ExpectedPaths) != 0 {
		t.Errorf("expected_paths: got %v, want empty", decoded.ExpectedPaths)
	}
	// definition_of_abort: nil pointer marshals as "definition_of_abort: null",
	// which round-trips back to nil.
	if decoded.DefinitionOfAbort != nil {
		t.Errorf("definition_of_abort: got %v, want nil", decoded.DefinitionOfAbort)
	}
	if decoded.DefinitionOfDone != nil {
		t.Errorf("definition_of_done: got %v, want nil", decoded.DefinitionOfDone)
	}
}

func TestTask_YAMLFromString(t *testing.T) {
	t.Parallel()
	yamlData := []byte(`
id: task_003
command_id: cmd_002
purpose: yaml string test
content: test
acceptance_criteria: old criteria
definition_of_done:
  - unit tests pass
  - integration tests pass
expected_paths:
  - src/main.go
  - src/handler.go
definition_of_abort:
  max_repair_count: 4
  max_wall_clock_sec: 1200
  explicit_failure_conditions:
    - build failure
    - dependency missing
status: pending
created_at: "2026-04-10T10:00:00+09:00"
updated_at: "2026-04-10T10:00:00+09:00"
`)

	var task Task
	if err := yaml.Unmarshal(yamlData, &task); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(task.DefinitionOfDone) != 2 {
		t.Fatalf("definition_of_done: got %d, want 2", len(task.DefinitionOfDone))
	}
	if len(task.ExpectedPaths) != 2 {
		t.Fatalf("expected_paths: got %d, want 2", len(task.ExpectedPaths))
	}
	if task.DefinitionOfAbort == nil {
		t.Fatal("definition_of_abort: expected non-nil")
	}
	if task.DefinitionOfAbort.MaxRepairCount != 4 {
		t.Errorf("max_repair_count: got %d, want 4", task.DefinitionOfAbort.MaxRepairCount)
	}
	if len(task.DefinitionOfAbort.ExplicitFailureConditions) != 2 {
		t.Errorf("explicit_failure_conditions: got %d, want 2", len(task.DefinitionOfAbort.ExplicitFailureConditions))
	}
}

func TestTask_GetDoneConditions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		task     Task
		expected []string
	}{
		{
			name: "DefinitionOfDone only",
			task: Task{
				DefinitionOfDone: []string{"cond1", "cond2"},
			},
			expected: []string{"cond1", "cond2"},
		},
		{
			name: "AcceptanceCriteria only",
			task: Task{
				AcceptanceCriteria: "all tests pass",
			},
			expected: []string{"all tests pass"},
		},
		{
			name: "both set - DefinitionOfDone takes priority",
			task: Task{
				AcceptanceCriteria: "legacy",
				DefinitionOfDone:   []string{"new1", "new2"},
			},
			expected: []string{"new1", "new2"},
		},
		{
			name:     "neither set",
			task:     Task{},
			expected: nil,
		},
		{
			name: "empty DefinitionOfDone falls back to AcceptanceCriteria",
			task: Task{
				AcceptanceCriteria: "fallback",
				DefinitionOfDone:   []string{},
			},
			expected: []string{"fallback"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.task.GetDoneConditions()
			if tt.expected == nil {
				if got != nil {
					t.Errorf("GetDoneConditions() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.expected) {
				t.Fatalf("GetDoneConditions() length = %d, want %d", len(got), len(tt.expected))
			}
			for i, v := range tt.expected {
				if got[i] != v {
					t.Errorf("GetDoneConditions()[%d] = %q, want %q", i, got[i], v)
				}
			}
		})
	}
}

// TestTask_GetDoneConditions_NilReceiver verifies that calling GetDoneConditions
// on a nil *Task pointer panics (nil dereference). This documents the expected
// behavior so that callers know they must nil-check before calling the method.
func TestTask_GetDoneConditions_NilReceiver(t *testing.T) {
	t.Parallel()
	var task *Task
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil receiver, but did not panic")
		}
	}()
	_ = task.GetDoneConditions()
}
