package plan

import (
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestGetModelForBloomLevel(t *testing.T) {
	tests := []struct {
		level int
		want  string
	}{
		{1, "sonnet"},
		{2, "sonnet"},
		{3, "sonnet"},
		{4, "opus"},
		{5, "opus"},
		{6, "opus"},
	}

	for _, tt := range tests {
		got := GetModelForBloomLevel(tt.level, false)
		if got != tt.want {
			t.Errorf("GetModelForBloomLevel(%d, false) = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestGetModelForBloomLevel_Boost(t *testing.T) {
	for level := 1; level <= 6; level++ {
		got := GetModelForBloomLevel(level, true)
		if got != "opus" {
			t.Errorf("GetModelForBloomLevel(%d, true) = %q, want %q", level, got, "opus")
		}
	}
}

func TestAssignWorkers_BasicSonnet(t *testing.T) {
	config := model.WorkerConfig{
		Count:        2,
		DefaultModel: "sonnet",
		Boost:        false,
	}
	limits := model.LimitsConfig{
		MaxPendingTasksPerWorker: 5,
	}
	workers := []WorkerState{
		{WorkerID: "worker_0", Model: "sonnet", PendingCount: 0},
		{WorkerID: "worker_1", Model: "opus", PendingCount: 0},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "task_a", BloomLevel: 2},
	}

	assignments, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(assignments))
	}
	if assignments[0].WorkerID != "worker_0" {
		t.Errorf("expected worker_0 (sonnet), got %q", assignments[0].WorkerID)
	}
	if assignments[0].Model != "sonnet" {
		t.Errorf("expected model sonnet, got %q", assignments[0].Model)
	}
	if assignments[0].TaskName != "task_a" {
		t.Errorf("expected task_a, got %q", assignments[0].TaskName)
	}
}

func TestAssignWorkers_BasicOpus(t *testing.T) {
	config := model.WorkerConfig{
		Count:        2,
		DefaultModel: "opus",
		Boost:        false,
	}
	limits := model.LimitsConfig{
		MaxPendingTasksPerWorker: 5,
	}
	workers := []WorkerState{
		{WorkerID: "worker_0", Model: "sonnet", PendingCount: 0},
		{WorkerID: "worker_1", Model: "opus", PendingCount: 0},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "task_b", BloomLevel: 5},
	}

	assignments, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(assignments))
	}
	if assignments[0].WorkerID != "worker_1" {
		t.Errorf("expected worker_1 (opus), got %q", assignments[0].WorkerID)
	}
	if assignments[0].Model != "opus" {
		t.Errorf("expected model opus, got %q", assignments[0].Model)
	}
}

func TestAssignWorkers_MinimumPending(t *testing.T) {
	config := model.WorkerConfig{
		Count:        3,
		DefaultModel: "sonnet",
		Boost:        false,
	}
	limits := model.LimitsConfig{
		MaxPendingTasksPerWorker: 10,
	}
	workers := []WorkerState{
		{WorkerID: "worker_0", Model: "sonnet", PendingCount: 5},
		{WorkerID: "worker_1", Model: "sonnet", PendingCount: 2},
		{WorkerID: "worker_2", Model: "sonnet", PendingCount: 8},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "task_c", BloomLevel: 1},
	}

	assignments, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(assignments))
	}
	if assignments[0].WorkerID != "worker_1" {
		t.Errorf("expected worker_1 (lowest pending=2), got %q", assignments[0].WorkerID)
	}
}

func TestAssignWorkers_Backpressure(t *testing.T) {
	config := model.WorkerConfig{
		Count:        2,
		DefaultModel: "sonnet",
		Boost:        false,
	}
	limits := model.LimitsConfig{
		MaxPendingTasksPerWorker: 3,
	}
	workers := []WorkerState{
		{WorkerID: "worker_0", Model: "sonnet", PendingCount: 3},
		{WorkerID: "worker_1", Model: "sonnet", PendingCount: 3},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "task_overflow", BloomLevel: 2},
	}

	_, err := AssignWorkers(config, limits, workers, tasks)
	if err == nil {
		t.Fatal("expected error when all workers at capacity, got nil")
	}
	if !strings.Contains(err.Error(), "no available worker") {
		t.Errorf("expected error containing 'no available worker', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "task_overflow") {
		t.Errorf("expected error mentioning task name 'task_overflow', got %q", err.Error())
	}
}

func TestAssignWorkers_MixedBloomLevels(t *testing.T) {
	config := model.WorkerConfig{
		Count:        2,
		DefaultModel: "sonnet",
		Boost:        false,
	}
	limits := model.LimitsConfig{
		MaxPendingTasksPerWorker: 10,
	}
	workers := []WorkerState{
		{WorkerID: "worker_0", Model: "sonnet", PendingCount: 0},
		{WorkerID: "worker_1", Model: "opus", PendingCount: 0},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "memorize", BloomLevel: 2},
		{Name: "evaluate", BloomLevel: 5},
		{Name: "understand", BloomLevel: 3},
		{Name: "create", BloomLevel: 6},
	}

	assignments, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 4 {
		t.Fatalf("expected 4 assignments, got %d", len(assignments))
	}

	for _, a := range assignments {
		switch a.TaskName {
		case "memorize", "understand":
			if a.Model != "sonnet" {
				t.Errorf("task %q (low bloom) should be sonnet, got %q", a.TaskName, a.Model)
			}
			if a.WorkerID != "worker_0" {
				t.Errorf("task %q should go to worker_0 (sonnet), got %q", a.TaskName, a.WorkerID)
			}
		case "evaluate", "create":
			if a.Model != "opus" {
				t.Errorf("task %q (high bloom) should be opus, got %q", a.TaskName, a.Model)
			}
			if a.WorkerID != "worker_1" {
				t.Errorf("task %q should go to worker_1 (opus), got %q", a.TaskName, a.WorkerID)
			}
		default:
			t.Errorf("unexpected task name: %q", a.TaskName)
		}
	}
}

func TestAssignWorkers_BoostMode(t *testing.T) {
	config := model.WorkerConfig{
		Count:        2,
		DefaultModel: "opus",
		Boost:        true,
	}
	limits := model.LimitsConfig{
		MaxPendingTasksPerWorker: 10,
	}
	workers := []WorkerState{
		{WorkerID: "worker_0", Model: "opus", PendingCount: 0},
		{WorkerID: "worker_1", Model: "opus", PendingCount: 0},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "remember", BloomLevel: 1},
		{Name: "apply", BloomLevel: 3},
		{Name: "synthesize", BloomLevel: 5},
	}

	assignments, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 3 {
		t.Fatalf("expected 3 assignments, got %d", len(assignments))
	}

	for _, a := range assignments {
		if a.Model != "opus" {
			t.Errorf("boost mode: task %q should use opus, got %q", a.TaskName, a.Model)
		}
	}
}
