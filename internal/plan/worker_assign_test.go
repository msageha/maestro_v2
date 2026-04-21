package plan

import (
	"errors"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/bandit"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

// newTestBanditSelector creates a bandit.Selector for testing.
func newTestBanditSelector(t *testing.T, cfg model.BanditConfig) BanditSelector {
	t.Helper()
	sel, err := bandit.NewSelector(cfg.EffectiveExplorationCoeff())
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	return sel
}

// stubModelSelector is a test double for ModelSelector that returns a fixed
// value from SelectModel, letting tests force a particular bandit choice.
type stubModelSelector struct {
	choice string
}

func (s stubModelSelector) SelectModel(_ int, _ string) string { return s.choice }

func TestAssignWorkers_WithModelSelector_HonorsPick(t *testing.T) {
	config := model.WorkerConfig{
		Count:        2,
		DefaultModel: "sonnet",
		Models:       map[string]string{"worker2": "opus"},
	}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet"},
		{WorkerID: "worker2", Model: "opus"},
	}
	// Bloom level 2 statically maps to sonnet; selector overrides to opus.
	tasks := []TaskAssignmentRequest{{Name: "t1", BloomLevel: 2}}

	got, err := AssignWorkers(config, limits, workers, tasks, WithModelSelector(stubModelSelector{choice: "opus"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(got))
	}
	if got[0].Model != "opus" {
		t.Errorf("expected selector override to opus, got model=%q", got[0].Model)
	}
	if got[0].WorkerID != "worker2" {
		t.Errorf("expected worker2 (opus), got %q", got[0].WorkerID)
	}
}

func TestAssignWorkers_WithModelSelector_IgnoresInfeasiblePick(t *testing.T) {
	// Selector picks "haiku" but no worker runs haiku → fall back to static.
	config := model.WorkerConfig{
		Count:        1,
		DefaultModel: "sonnet",
	}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet"},
	}
	tasks := []TaskAssignmentRequest{{Name: "t1", BloomLevel: 2}}

	got, err := AssignWorkers(config, limits, workers, tasks, WithModelSelector(stubModelSelector{choice: "haiku"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Model != "sonnet" {
		t.Errorf("expected static fallback to sonnet, got %+v", got)
	}
}

func TestAssignWorkers_NilSelectorOption_IsNoop(t *testing.T) {
	config := model.WorkerConfig{Count: 1, DefaultModel: "sonnet"}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{{WorkerID: "worker1", Model: "sonnet"}}
	tasks := []TaskAssignmentRequest{{Name: "t1", BloomLevel: 2}}

	// Passing nil ModelSelector must be safe and equivalent to no option.
	got, err := AssignWorkers(config, limits, workers, tasks, WithModelSelector(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Model != "sonnet" {
		t.Errorf("expected sonnet assignment, got %+v", got)
	}
}

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
	if !errors.Is(err, ErrNoAvailableWorker) {
		t.Errorf("expected ErrNoAvailableWorker in error chain, got %v", err)
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

// --- AdaptiveModelSelector tests ---

func TestAdaptiveModelSelector_Disabled(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled: ptr.Bool(false),
	}
	sel := NewAdaptiveModelSelector(cfg, nil)

	// Should fall back to static mapping regardless of data
	got := sel.SelectModel(2, "implement")
	if got != "sonnet" {
		t.Errorf("disabled selector: SelectModel(2) = %q, want %q", got, "sonnet")
	}
	got = sel.SelectModel(5, "implement")
	if got != "opus" {
		t.Errorf("disabled selector: SelectModel(5) = %q, want %q", got, "opus")
	}
}

func TestAdaptiveModelSelector_InsufficientData_TraceRequirement(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(1.41),
		MinSamplesBeforeUse:  ptr.Int(2),
		TraceDataRequirement: ptr.Int(10), // require 10 total pulls
	}
	sel := NewAdaptiveModelSelector(cfg, newTestBanditSelector(t, cfg))

	// Seed a few observations — not enough to meet TraceDataRequirement(10)
	for i := 0; i < 3; i++ {
		sel.RecordResult("sonnet", 1.0)
		sel.RecordResult("opus", 0.8)
	}
	// total pulls = 6, TraceDataRequirement = 10 → fallback
	got := sel.SelectModel(2, "implement")
	if got != "sonnet" {
		t.Errorf("insufficient trace data: SelectModel(2) = %q, want %q (static fallback)", got, "sonnet")
	}
}

func TestAdaptiveModelSelector_InsufficientData_MinSamples(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(1.41),
		MinSamplesBeforeUse:  ptr.Int(5),  // each arm needs 5
		TraceDataRequirement: ptr.Int(3),  // total trace easily met
	}
	sel := NewAdaptiveModelSelector(cfg, newTestBanditSelector(t, cfg))

	// Give enough total but haiku has 0 pulls
	for i := 0; i < 5; i++ {
		sel.RecordResult("sonnet", 1.0)
		sel.RecordResult("opus", 0.8)
	}
	// total=10 >= TraceDataRequirement(3), but haiku has 0 < MinSamples(5) → fallback
	got := sel.SelectModel(2, "implement")
	if got != "sonnet" {
		t.Errorf("min samples not met: SelectModel(2) = %q, want %q (static fallback)", got, "sonnet")
	}
}

func TestAdaptiveModelSelector_SufficientData_BanditSelection(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(0.0), // zero exploration → pure exploitation
		MinSamplesBeforeUse:  ptr.Int(2),
		TraceDataRequirement: ptr.Int(6),
	}
	sel := NewAdaptiveModelSelector(cfg, newTestBanditSelector(t, cfg))

	// Seed data: sonnet gets high reward, opus medium, haiku low
	for i := 0; i < 5; i++ {
		sel.RecordResult("sonnet", 1.0)
		sel.RecordResult("opus", 0.5)
		sel.RecordResult("haiku", 0.1)
	}
	// total=15 >= 6, each arm has 5 >= 2 → bandit should select

	got := sel.SelectModel(2, "implement")
	// With explorationCoeff=0, pure exploitation → highest avg reward = sonnet
	if got != "sonnet" {
		t.Errorf("bandit selection: SelectModel(2) = %q, want %q (highest reward)", got, "sonnet")
	}
}

func TestAdaptiveModelSelector_RecordResult(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:          ptr.Bool(true),
		ExplorationCoeff: ptr.Float64(1.41),
	}
	sel := NewAdaptiveModelSelector(cfg, newTestBanditSelector(t, cfg))

	sel.RecordResult("sonnet", 1.0)
	sel.RecordResult("sonnet", 0.6)

	// Access underlying *bandit.Selector via type assertion for detailed stats
	bs := sel.bandit.(*bandit.Selector)
	stats := bs.GetStats()
	arm, ok := stats["sonnet"]
	if !ok {
		t.Fatal("expected sonnet arm in stats")
	}
	if arm.PullCount != 2 {
		t.Errorf("PullCount = %d, want 2", arm.PullCount)
	}
	// AvgReward should be (1.0+0.6)/2 = 0.8
	if arm.AvgReward < 0.79 || arm.AvgReward > 0.81 {
		t.Errorf("AvgReward = %f, want ~0.8", arm.AvgReward)
	}
}

func TestAdaptiveModelSelector_RecordResult_Disabled(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled: ptr.Bool(false),
	}
	sel := NewAdaptiveModelSelector(cfg, nil)

	// Should not panic
	sel.RecordResult("sonnet", 1.0)
	if sel.bandit != nil {
		t.Error("bandit should be nil when disabled")
	}
}

func TestAdaptiveModelSelector_FallbackOnError(t *testing.T) {
	cfg := model.BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(1.41),
		MinSamplesBeforeUse:  ptr.Int(0),
		TraceDataRequirement: ptr.Int(0),
	}
	sel := NewAdaptiveModelSelector(cfg, newTestBanditSelector(t, cfg))

	// Remove all arms to force SelectArm error
	sel.bandit.Reset()

	got := sel.SelectModel(5, "implement")
	if got != "opus" {
		t.Errorf("error fallback: SelectModel(5) = %q, want %q (static fallback)", got, "opus")
	}
}
