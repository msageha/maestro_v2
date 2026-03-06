package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// --- checkCommandTasksTerminal tests ---

func TestCheckCommandTasksTerminal_AllCompleted(t *testing.T) {
	qh := newMinimalQueueHandler(t)
	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	allTerminal, hasFailed := qh.checkCommandTasksTerminal("cmd1", tqs)
	if !allTerminal {
		t.Error("expected allTerminal=true")
	}
	if hasFailed {
		t.Error("expected hasFailed=false")
	}
}

func TestCheckCommandTasksTerminal_HasFailed(t *testing.T) {
	qh := newMinimalQueueHandler(t)
	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusFailed},
		},
	})

	allTerminal, hasFailed := qh.checkCommandTasksTerminal("cmd1", tqs)
	if !allTerminal {
		t.Error("expected allTerminal=true")
	}
	if !hasFailed {
		t.Error("expected hasFailed=true")
	}
}

func TestCheckCommandTasksTerminal_HasDeadLetter(t *testing.T) {
	qh := newMinimalQueueHandler(t)
	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusDeadLetter},
		},
	})

	allTerminal, hasFailed := qh.checkCommandTasksTerminal("cmd1", tqs)
	if !allTerminal {
		t.Error("expected allTerminal=true")
	}
	if !hasFailed {
		t.Error("expected hasFailed=true for dead_letter")
	}
}

func TestCheckCommandTasksTerminal_NotAllTerminal(t *testing.T) {
	qh := newMinimalQueueHandler(t)
	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusInProgress},
		},
	})

	allTerminal, _ := qh.checkCommandTasksTerminal("cmd1", tqs)
	if allTerminal {
		t.Error("expected allTerminal=false")
	}
}

func TestCheckCommandTasksTerminal_NoTasks(t *testing.T) {
	qh := newMinimalQueueHandler(t)
	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "other_cmd", Status: model.StatusCompleted},
		},
	})

	allTerminal, _ := qh.checkCommandTasksTerminal("cmd1", tqs)
	if allTerminal {
		t.Error("expected allTerminal=false when no tasks for command")
	}
}

func TestCheckCommandTasksTerminal_MixedCommands(t *testing.T) {
	qh := newMinimalQueueHandler(t)
	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd2", Status: model.StatusInProgress},
		},
		"worker2": {
			{ID: "t3", CommandID: "cmd1", Status: model.StatusCancelled},
		},
	})

	allTerminal, hasFailed := qh.checkCommandTasksTerminal("cmd1", tqs)
	if !allTerminal {
		t.Error("expected allTerminal=true for cmd1")
	}
	if hasFailed {
		t.Error("expected hasFailed=false for cmd1")
	}
}

func TestCheckCommandTasksTerminal_AcrossWorkers(t *testing.T) {
	qh := newMinimalQueueHandler(t)
	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
		"worker2": {
			{ID: "t2", CommandID: "cmd1", Status: model.StatusPending},
		},
	})

	allTerminal, _ := qh.checkCommandTasksTerminal("cmd1", tqs)
	if allTerminal {
		t.Error("expected allTerminal=false when worker2 task is pending")
	}
}

// --- collectWorktreePublishAndCleanup tests ---

func TestCollectWorktreePublish_MergedIntegration(t *testing.T) {
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnSuccess: true,
	})

	// Set up worktree state with "merged" integration
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)

	// Set up command state (no phases)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", tqs)
	if len(publishes) != 1 {
		t.Fatalf("expected 1 publish item, got %d", len(publishes))
	}
	if publishes[0].CommandID != "cmd1" {
		t.Errorf("publish CommandID = %q, want cmd1", publishes[0].CommandID)
	}
	// No cleanup yet (cleanup happens after publish success in Phase B)
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipAlreadyPublished(t *testing.T) {
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnSuccess: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusPublished)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items for already published, got %d", len(publishes))
	}
	// Should collect cleanup since it's published but not cleaned
	if len(cleanups) != 1 {
		t.Fatalf("expected 1 cleanup item, got %d", len(cleanups))
	}
	if cleanups[0].Reason != "success" {
		t.Errorf("cleanup reason = %q, want success", cleanups[0].Reason)
	}
}

func TestCollectWorktreePublish_SkipOnFailedTasks(t *testing.T) {
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnFailure: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusFailed,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusFailed},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when tasks failed, got %d", len(publishes))
	}
	if len(cleanups) != 1 {
		t.Fatalf("expected 1 cleanup item for failed command, got %d", len(cleanups))
	}
	if cleanups[0].Reason != "failure" {
		t.Errorf("cleanup reason = %q, want failure", cleanups[0].Reason)
	}
}

func TestCollectWorktreePublish_NoCleanupOnFailureDisabled(t *testing.T) {
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnFailure: false, // disabled
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusFailed,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusFailed},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items when CleanupOnFailure=false, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipNotReady(t *testing.T) {
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	// Integration still in "created" — not ready for publish
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items for created status, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipConflict(t *testing.T) {
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusConflict)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items for conflict status, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipNonTerminalPhases(t *testing.T) {
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	// Command has phases, one still active
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, []model.Phase{
		{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted},
		{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusActive},
	})

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when phases not terminal, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_AllPhasesTerminal(t *testing.T) {
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, []model.Phase{
		{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted},
		{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusCompleted},
	})

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, _ := qh.collectWorktreePublishAndCleanup("cmd1", tqs)
	if len(publishes) != 1 {
		t.Fatalf("expected 1 publish item when all phases terminal, got %d", len(publishes))
	}
}

func TestCollectWorktreePublish_SkipNonTerminalTasks(t *testing.T) {
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusInProgress,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusInProgress},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when tasks not terminal, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_PhaseErrorFailsClosed(t *testing.T) {
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	// No command state file → stateReader.GetCommandPhases will return error

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when phase check errors, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items when phase check errors, got %d", len(cleanups))
	}
}

// --- Test helpers ---

// scanPhaseStateReader implements StateReader for scan phase tests.
type scanPhaseStateReader struct {
	maestroDir string
}

func (r *scanPhaseStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return "", err
	}
	s, ok := state.TaskStates[taskID]
	if !ok {
		return "", ErrStateNotFound
	}
	return s, nil
}

func (r *scanPhaseStateReader) GetCommandPhases(commandID string) ([]PhaseInfo, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return nil, err
	}
	var phases []PhaseInfo
	for _, p := range state.Phases {
		phases = append(phases, PhaseInfo{
			ID:     p.PhaseID,
			Name:   p.Name,
			Status: p.Status,
		})
	}
	return phases, nil
}

func (r *scanPhaseStateReader) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	return nil, nil
}

func (r *scanPhaseStateReader) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	return nil
}

func (r *scanPhaseStateReader) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	return nil
}

func (r *scanPhaseStateReader) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	return false, false, nil
}

func (r *scanPhaseStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	return false, nil
}

func (r *scanPhaseStateReader) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	return &model.CircuitBreakerState{}, nil
}

func (r *scanPhaseStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	return nil
}

func (r *scanPhaseStateReader) loadState(commandID string) (*model.CommandState, error) {
	path := filepath.Join(r.maestroDir, "state", "commands", commandID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrStateNotFound
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func setupScanPhaseTestDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	for _, sub := range []string{
		"queue", "results", "logs", "state/commands", "state/worktrees",
	} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}
	return maestroDir
}

func newMinimalQueueHandler(t *testing.T) *QueueHandler {
	t.Helper()
	maestroDir := setupScanPhaseTestDir(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
	}
	return NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
}

func newScanPhaseTestQueueHandler(t *testing.T, maestroDir string, wtConfig model.WorktreeConfig) *QueueHandler {
	t.Helper()
	cfg := model.Config{
		Agents:   model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher:  model.WatcherConfig{DispatchLeaseSec: 300},
		Worktree: wtConfig,
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	// Wire state reader
	reader := &scanPhaseStateReader{maestroDir: maestroDir}
	qh.SetStateReader(reader)

	// Wire worktree manager with minimal config (only needs to read state files)
	projectRoot := filepath.Dir(maestroDir)
	wm := NewWorktreeManager(maestroDir, wtConfig, log.New(&bytes.Buffer{}, "", 0), LogLevelError)
	// Override projectRoot for tests
	wm.SetProjectRoot(projectRoot)
	qh.SetWorktreeManager(wm)

	return qh
}

func makeTaskQueues(workerTasks map[string][]model.Task) map[string]*taskQueueEntry {
	tqs := make(map[string]*taskQueueEntry)
	for workerID, tasks := range workerTasks {
		path := "/fake/" + workerID + ".yaml"
		tqs[path] = &taskQueueEntry{
			Queue: model.TaskQueue{
				SchemaVersion: 1,
				FileType:      "queue_task",
				Tasks:         tasks,
			},
			Path: path,
		}
	}
	return tqs
}

func writeWorktreeState(t *testing.T, maestroDir, commandID string, integrationStatus model.IntegrationStatus) {
	t.Helper()
	state := model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID: commandID,
			Branch:    "maestro/" + commandID + "/integration",
			BaseSHA:   "abc123",
			Status:    integrationStatus,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		Workers: []model.WorktreeState{
			{
				CommandID: commandID,
				WorkerID:  "worker1",
				Path:      "/fake/worktree/worker1",
				Branch:    "maestro/" + commandID + "/worker1",
				BaseSHA:   "abc123",
				Status:    model.WorktreeStatusActive,
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	path := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write worktree state: %v", err)
	}
}

func writeCommandState(t *testing.T, maestroDir, commandID string, taskStates map[string]model.Status, phases []model.Phase) {
	t.Helper()
	var requiredIDs []string
	for id := range taskStates {
		requiredIDs = append(requiredIDs, id)
	}
	state := model.CommandState{
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       commandID,
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: requiredIDs,
		TaskStates:      taskStates,
		Phases:          phases,
		CreatedAt:       "2026-01-01T00:00:00Z",
		UpdatedAt:       "2026-01-01T00:00:00Z",
	}
	path := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write command state: %v", err)
	}
}
