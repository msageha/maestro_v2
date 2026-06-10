package plan

import (
	"errors"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

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

func TestModelFamily(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short sonnet", "sonnet", "sonnet"},
		{"short opus", "opus", "opus"},
		{"short haiku", "haiku", "haiku"},
		{"full sonnet 4-6", "claude-sonnet-4-6", "sonnet"},
		{"full opus 4-6", "claude-opus-4-6", "opus"},
		{"full opus 4-7", "claude-opus-4-7", "opus"},
		{"full haiku dated", "claude-haiku-4-5-20251001", "haiku"},
		{"unknown passthrough", "gemini-2.5-pro", "gemini-2.5-pro"},
		{"unknown claude passthrough", "claude-unknown-9-9", "claude-unknown-9-9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelFamily(tt.in); got != tt.want {
				t.Errorf("modelFamily(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestAssignWorkers_FullModelIDMatchesOpusFamily(t *testing.T) {
	// Worker configured with a full Claude ID should satisfy an "opus"
	// requirement derived from BloomLevel 4-6.
	config := model.WorkerConfig{
		Count:        2,
		DefaultModel: "sonnet",
		Models:       map[string]string{"worker2": "claude-opus-4-7"},
	}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet"},
		{WorkerID: "worker2", Model: "claude-opus-4-7"},
	}
	tasks := []TaskAssignmentRequest{{Name: "t1", BloomLevel: 5}}

	got, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(got))
	}
	if got[0].WorkerID != "worker2" {
		t.Errorf("expected worker2 (claude-opus-4-7), got %q", got[0].WorkerID)
	}
	if got[0].Model != "claude-opus-4-7" {
		t.Errorf("expected assignment to carry full ID, got %q", got[0].Model)
	}
}

func TestAssignWorkers_WithModelSelector_FullIDPickMatchesFamilyWorker(t *testing.T) {
	// Selector picks the family alias "opus"; worker is configured with the
	// full ID "claude-opus-4-7". The pick must be honored via family match.
	config := model.WorkerConfig{
		Count:        2,
		DefaultModel: "sonnet",
		Models:       map[string]string{"worker2": "claude-opus-4-7"},
	}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet"},
		{WorkerID: "worker2", Model: "claude-opus-4-7"},
	}
	tasks := []TaskAssignmentRequest{{Name: "t1", BloomLevel: 2}}

	got, err := AssignWorkers(config, limits, workers, tasks, WithModelSelector(stubModelSelector{choice: "opus"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker2" {
		t.Errorf("expected selector override to land on worker2, got %+v", got)
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

// TestAssignWorkers_FallbackAllOpus verifies that when all workers are
// configured for "opus" and a task maps to "sonnet" via BloomLevel, the
// assignment falls back to opus instead of returning an error. This
// mirrors the production deployment where operators run all-opus fleets
// and originally saw "bloom_level=2 requires sonnet" failures.
func TestAssignWorkers_FallbackAllOpus(t *testing.T) {
	config := model.WorkerConfig{
		Count:        2,
		DefaultModel: "opus",
	}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "opus"},
		{WorkerID: "worker2", Model: "opus"},
	}
	// Bloom level 2 statically maps to sonnet — but no sonnet worker exists.
	tasks := []TaskAssignmentRequest{{Name: "t1", BloomLevel: 2}}

	got, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("expected fallback to opus, got error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(got))
	}
	if got[0].Model != "opus" {
		t.Errorf("expected fallback model=opus, got %q", got[0].Model)
	}
}

// TestAssignWorkers_FallbackAllSonnet verifies the opposite direction:
// opus-requiring task with only sonnet workers falls back to sonnet.
func TestAssignWorkers_FallbackAllSonnet(t *testing.T) {
	config := model.WorkerConfig{
		Count:        1,
		DefaultModel: "sonnet",
	}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{{WorkerID: "worker1", Model: "sonnet"}}
	tasks := []TaskAssignmentRequest{{Name: "t_eval", BloomLevel: 5}}

	got, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("expected fallback to sonnet, got error: %v", err)
	}
	if len(got) != 1 || got[0].Model != "sonnet" {
		t.Errorf("expected sonnet fallback, got %+v", got)
	}
}

// TestChooseFallbackFamily verifies the preference ordering and the
// no-workers case.
func TestChooseFallbackFamily(t *testing.T) {
	tests := []struct {
		name     string
		workers  map[string]string // workerID -> model
		required string
		want     string
	}{
		{
			name:     "sonnet required, only opus available",
			workers:  map[string]string{"w1": "opus"},
			required: "sonnet",
			want:     "opus",
		},
		{
			name:     "opus required, only sonnet available",
			workers:  map[string]string{"w1": "sonnet"},
			required: "opus",
			want:     "sonnet",
		},
		{
			name:     "sonnet required, opus preferred over haiku",
			workers:  map[string]string{"w1": "haiku", "w2": "opus"},
			required: "sonnet",
			want:     "opus",
		},
		{
			// chooseFallbackFamily prefers a different family to the required one.
			// If the only available model IS the required family (and has no workers),
			// the last-resort excludes it to avoid a circular result, returning "".
			// Callers must guard on "" and surface the original error.
			name:     "no non-required family available returns empty",
			workers:  map[string]string{"w1": "opus"},
			required: "opus",
			want:     "",
		},
		{
			name:     "empty stateMap returns empty",
			workers:  map[string]string{},
			required: "sonnet",
			want:     "",
		},
		{
			name:     "unknown required family uses generic order",
			workers:  map[string]string{"w1": "sonnet"},
			required: "gemini-pro",
			want:     "sonnet",
		},
		{
			// Non-Claude deployment: all workers use "codex". The Claude
			// preference list finds nothing, so the last resort returns the
			// actual provisioned model so tasks can still be assigned.
			name:     "codex-only fleet falls back to codex",
			workers:  map[string]string{"w1": "codex", "w2": "codex"},
			required: "sonnet",
			want:     "codex",
		},
		{
			// Gemini-only fleet: last-resort returns "gemini" when no Claude
			// family workers are available.
			name:     "gemini-only fleet falls back to gemini",
			workers:  map[string]string{"w1": "gemini", "w2": "gemini"},
			required: "opus",
			want:     "gemini",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := make(map[string]*WorkerState, len(tt.workers))
			for id, m := range tt.workers {
				sm[id] = &WorkerState{WorkerID: id, Model: m}
			}
			if got := chooseFallbackFamily(sm, tt.required, false); got != tt.want {
				t.Errorf("chooseFallbackFamily(%q) = %q, want %q", tt.required, got, tt.want)
			}
		})
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

// See internal/daemon/model_selector_test.go for AdaptiveModelSelector /
// BanditSelector coverage (warm-up, trace-requirement, fallback behavior).

// TestAssignWorkers_PinnedWorkerHonored verifies the PinnedWorkerID
// shortcut: when TaskAssignmentRequest names a configured worker, the
// bloom-derived auto-assignment is bypassed and the task lands on that
// worker regardless of load. This is the path Planner agents use when
// fanning parallel tasks into worker1 / worker2 lanes from a fresh plan
// submit.
func TestAssignWorkers_PinnedWorkerHonored(t *testing.T) {
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
	// Bloom level 2 normally maps to sonnet → worker1, so pinning to
	// worker2 (opus) is a real override.
	tasks := []TaskAssignmentRequest{
		{Name: "feature_a", BloomLevel: 2, PinnedWorkerID: "worker2"},
	}

	got, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("AssignWorkers: %v", err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker2" {
		t.Fatalf("assignment = %+v, want pinned to worker2", got)
	}
	if got[0].Model != "opus" {
		t.Errorf("model = %q, want opus (worker2's configured model)", got[0].Model)
	}
}

// TestAssignWorkers_PinnedWorkerUnknown rejects a typo in worker_id with a
// clear error rather than silently falling back to auto-assignment. The
// fallback would mask Planner bugs (e.g. "wroker1" → autopicked worker1
// anyway) and erode the determinism the pinning feature is meant to
// guarantee.
func TestAssignWorkers_PinnedWorkerUnknown(t *testing.T) {
	config := model.WorkerConfig{Count: 2, DefaultModel: "sonnet"}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet"},
		{WorkerID: "worker2", Model: "sonnet"},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "t1", BloomLevel: 2, PinnedWorkerID: "worker99"},
	}

	_, err := AssignWorkers(config, limits, workers, tasks)
	if err == nil {
		t.Fatal("expected error for unknown pinned worker")
	}
	if !errors.Is(err, ErrNoAvailableWorker) {
		t.Errorf("err = %v, want ErrNoAvailableWorker", err)
	}
	if !strings.Contains(err.Error(), "worker99") {
		t.Errorf("error %q does not name the offending worker", err.Error())
	}
}

// TestAssignWorkers_PinnedWorkerAtCapacity surfaces capacity pressure
// instead of silently picking another worker — pinning is a deterministic
// contract the Planner relies on; any auto-fallback would defeat it.
func TestAssignWorkers_PinnedWorkerAtCapacity(t *testing.T) {
	config := model.WorkerConfig{Count: 2, DefaultModel: "sonnet"}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 1}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", PendingCount: 1},
		{WorkerID: "worker2", Model: "sonnet"},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "t1", BloomLevel: 2, PinnedWorkerID: "worker1"},
	}

	_, err := AssignWorkers(config, limits, workers, tasks)
	if err == nil {
		t.Fatal("expected capacity error for pinned worker")
	}
	if !errors.Is(err, ErrNoAvailableWorker) {
		t.Errorf("err = %v, want ErrNoAvailableWorker", err)
	}
	if !strings.Contains(err.Error(), "capacity") {
		t.Errorf("error %q does not mention capacity", err.Error())
	}
}
