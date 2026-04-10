package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/reviewer"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
)

// testPhaseDiagnoser wraps plan.DiagnosePhase + plan.FormatDiagnosisPrompt for tests.
func testPhaseDiagnoser(phase model.Phase, tasks []model.Task, results []model.TaskResult) string {
	diag := plan.DiagnosePhase(phase, tasks, results)
	if diag == nil {
		return ""
	}
	return plan.FormatDiagnosisPrompt(diag)
}

// --- A-1: ReviewDispatcher integration tests ---

func TestReviewDispatcher_InitializedWhenEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	for _, sub := range []string{"queue", "results", "logs", "state"} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}

	cfg := model.Config{
		Review: model.ReviewConfig{Enabled: true, Models: []string{"test-model"}},
	}

	buf := &bytes.Buffer{}
	d, err := newDaemon(maestroDir, cfg, buf, nopCloser{})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	defer d.cancel()

	d.initComponents()

	// Cleanup event bus goroutines to avoid goleak
	if d.eventBus != nil {
		d.eventBus.Close()
	}

	if d.reviewDispatcher == nil {
		t.Fatal("reviewDispatcher should be non-nil when Review.Enabled=true")
	}
	if d.usefulnessTracker == nil {
		t.Fatal("usefulnessTracker should be non-nil when Review.Enabled=true")
	}
	if d.reviewRequests == nil {
		t.Fatal("reviewRequests map should be initialized")
	}
}

func TestReviewDispatcher_SkippedWhenDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	for _, sub := range []string{"queue", "results", "logs", "state"} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}

	cfg := model.Config{
		Review: model.ReviewConfig{Enabled: false},
	}

	buf := &bytes.Buffer{}
	d, err := newDaemon(maestroDir, cfg, buf, nopCloser{})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	defer d.cancel()

	d.initComponents()

	// Cleanup event bus goroutines to avoid goleak
	if d.eventBus != nil {
		d.eventBus.Close()
	}

	if d.reviewDispatcher != nil {
		t.Fatal("reviewDispatcher should be nil when Review.Enabled=false")
	}
	if d.usefulnessTracker != nil {
		t.Fatal("usefulnessTracker should be nil when Review.Enabled=false")
	}
}

func TestReviewDispatcher_ShouldReview_BloomLevelFilter(t *testing.T) {
	minBloom := 3
	cfg := model.ReviewConfig{
		Enabled:       true,
		Models:        []string{"test-model"},
		MinBloomLevel: &minBloom,
	}
	rd := reviewer.NewReviewDispatcher(cfg)
	defer rd.Close()

	lowBloom := model.Task{ID: "t1", BloomLevel: 1}
	highBloom := model.Task{ID: "t2", BloomLevel: 4}

	if rd.ShouldReview(lowBloom) {
		t.Error("ShouldReview should return false for task below min bloom level")
	}
	if !rd.ShouldReview(highBloom) {
		t.Error("ShouldReview should return true for task at or above min bloom level")
	}
}

func TestReviewDispatcher_FailureDoesNotBlockMainFlow(t *testing.T) {
	cfg := model.ReviewConfig{Enabled: false}
	rd := reviewer.NewReviewDispatcher(cfg)
	defer rd.Close()

	task := model.Task{ID: "t1", BloomLevel: 5}

	// Dispatch on a disabled dispatcher should error but not panic
	err := rd.Dispatch(nil, task, "diff content")
	if err == nil {
		t.Error("expected error when dispatching on disabled dispatcher")
	}
}

// --- A-3: Phase diagnosis integration tests ---

func TestDiagnosePhaseTasks_EmitsSignalOnCompletion(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	// Create state directory
	if err := os.MkdirAll(filepath.Join(maestroDir, "state", "commands"), 0755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}

	qh := NewQueueHandler(maestroDir, model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{}, nil
	})
	qh.SetPhaseDiagnoser(testPhaseDiagnoser)

	// Set up state reader with a completed phase containing tasks
	reader := &phaseATestStateReader{
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:              "phase1",
					Name:            "build",
					Status:          model.PhaseStatusCompleted,
					RequiredTaskIDs: []string{"task1", "task2"},
				},
			},
		},
	}
	qh.SetStateReader(reader)

	// Build task queues with tasks that have repair history
	taskQueues := map[string]*taskQueueEntry{
		filepath.Join(maestroDir, "queue", "worker1.yaml"): {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{
						ID:               "task1",
						CommandID:        "cmd1",
						Purpose:          "compile module A",
						Status:           model.StatusCompleted,
						Attempts:         3,
						ExecutionRetries: 2,
					},
					{
						ID:        "task2",
						CommandID: "cmd1",
						Purpose:   "compile module B",
						Status:    model.StatusCompleted,
						Attempts:  1,
					},
				},
			},
		},
	}

	result := qh.diagnosePhaseTasks("cmd1", "phase1", "build", taskQueues)
	if result == "" {
		t.Fatal("expected non-empty diagnosis prompt for phase with repair hotspots")
	}

	// Should contain repair hotspot info since task1 has 2 retries
	if !strings.Contains(result, "Repair") {
		t.Errorf("diagnosis should mention Repair hotspots, got: %s", result)
	}
}

func TestDiagnosePhaseTasks_EmptyForCleanPhase(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	qh := NewQueueHandler(maestroDir, model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.SetPhaseDiagnoser(testPhaseDiagnoser)

	reader := &phaseATestStateReader{
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:              "phase1",
					Name:            "build",
					Status:          model.PhaseStatusCompleted,
					RequiredTaskIDs: []string{"task1"},
				},
			},
		},
	}
	qh.SetStateReader(reader)

	// Single clean task with no retries
	taskQueues := map[string]*taskQueueEntry{
		filepath.Join(maestroDir, "queue", "worker1.yaml"): {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{
						ID:        "task1",
						CommandID: "cmd1",
						Purpose:   "compile",
						Status:    model.StatusCompleted,
						Attempts:  1,
					},
				},
			},
		},
	}

	result := qh.diagnosePhaseTasks("cmd1", "phase1", "build", taskQueues)

	// Clean phase should produce a "問題なし" prompt
	if result == "" {
		t.Fatal("expected non-empty diagnosis even for clean phase")
	}
	if !strings.Contains(result, "問題なし") {
		t.Errorf("expected clean diagnosis to contain '問題なし', got: %s", result)
	}
}

func TestDiagnosePhaseTasks_NoStateReader(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	qh := NewQueueHandler(maestroDir, model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	// No state reader set
	result := qh.diagnosePhaseTasks("cmd1", "phase1", "build", nil)
	if result != "" {
		t.Errorf("expected empty diagnosis without state reader, got: %s", result)
	}
}

// --- A-1 + A-3: Shutdown integration ---

func TestReviewDispatcher_CloseOnShutdown(t *testing.T) {
	cfg := model.ReviewConfig{
		Enabled: true,
		Models:  []string{"test-model"},
	}
	rd := reviewer.NewReviewDispatcher(cfg)

	// Close should not panic and should close the results channel
	rd.Close()

	// Results channel should be closed
	_, ok := <-rd.Results()
	if ok {
		t.Error("Results channel should be closed after Close()")
	}
}

func TestExtractTaskIDFromRequestID(t *testing.T) {
	tests := []struct {
		requestID string
		want      string
	}{
		{"review-task_123_abc-1234567890", "task_123_abc"},
		{"review-task_1775774427_4e4d5a08-1718000000000000000", "task_1775774427_4e4d5a08"},
		{"review-simple-999", "simple"},
		{"review-nodash", "nodash"}, // no trailing timestamp segment
	}
	for _, tt := range tests {
		got := extractTaskIDFromRequestID(tt.requestID)
		if got != tt.want {
			t.Errorf("extractTaskIDFromRequestID(%q) = %q, want %q", tt.requestID, got, tt.want)
		}
	}
}

// --- Helpers ---

// nopCloser satisfies io.Closer.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// phaseATestStateReader is a minimal StateReader for phase diagnosis tests.
type phaseATestStateReader struct {
	phases map[string][]PhaseInfo // key: commandID
}

func (m *phaseATestStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	return model.StatusCompleted, nil
}

func (m *phaseATestStateReader) GetCommandPhases(commandID string) ([]PhaseInfo, error) {
	p, ok := m.phases[commandID]
	if !ok {
		return nil, fmt.Errorf("command %s not found", commandID)
	}
	return p, nil
}

func (m *phaseATestStateReader) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	return nil, nil
}

func (m *phaseATestStateReader) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	return nil
}

func (m *phaseATestStateReader) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	return nil
}

func (m *phaseATestStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	return false, nil
}

func (m *phaseATestStateReader) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	return &model.CircuitBreakerState{}, nil
}

func (m *phaseATestStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	return nil
}

func (m *phaseATestStateReader) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	return false, false, nil
}

