package daemon

import (
	"bytes"
	"context"
	errorspkg "errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	worktreepkg "github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	"github.com/msageha/maestro_v2/internal/testutil"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// --- checkCommandTasksTerminal tests ---

func TestCheckCommandTasksTerminal_AllCompleted(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
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
	t.Parallel()
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

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
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
	t.Parallel()
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

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
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
	t.Parallel()
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

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items when CleanupOnFailure=false, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipNotReady(t *testing.T) {
	t.Parallel()
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

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items for created status, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

// TestCollectWorktreePublish_BlockedByCommitFailedWorkers verifies that publish
// is blocked when worktree state still records workers whose auto-commit failed,
// even if integration status reached Merged via the workers that did commit.
func TestCollectWorktreePublish_BlockedByCommitFailedWorkers(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnSuccess: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)

	// Re-load and inject CommitFailedWorkers, then re-write.
	statePath := filepath.Join(maestroDir, "state", "worktrees", "cmd1.yaml")
	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("load worktree state: %v", err)
	}
	state.CommitFailedWorkers = []string{"worker2"}
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("rewrite worktree state: %v", err)
	}

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

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when CommitFailedWorkers is non-empty, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipConflict(t *testing.T) {
	t.Parallel()
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

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items for conflict status, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipNonTerminalPhases(t *testing.T) {
	t.Parallel()
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

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when phases not terminal, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_AllPhasesTerminal(t *testing.T) {
	t.Parallel()
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

	publishes, _ := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 1 {
		t.Fatalf("expected 1 publish item when all phases terminal, got %d", len(publishes))
	}
}

func TestCollectWorktreePublish_SkipNonTerminalTasks(t *testing.T) {
	t.Parallel()
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

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when tasks not terminal, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_PhaseErrorFailsClosed(t *testing.T) {
	t.Parallel()
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

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
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
	return testutil.SetupDir(t)
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
	wm := NewWorktreeManager(maestroDir, wtConfig, log.New(&bytes.Buffer{}, "", 0), LogLevelError)
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

// --- collectWorktreePhaseMerges phase 0 件 fallback tests ---

// noPhasesFallbackFixture seeds: worktree state (Integration=integrationStatus,
// 1 worker), command state with phases=nil, and a task queue with the given
// task statuses. Returns the qh + task queue map ready for the helper call.
func noPhasesFallbackFixture(
	t *testing.T,
	integrationStatus model.IntegrationStatus,
	taskStates map[string]model.Status,
	mergedPhases map[string]string,
) (*QueueHandler, map[string]*taskQueueEntry) {
	t.Helper()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})
	writeWorktreeState(t, maestroDir, "cmd1", integrationStatus)
	if mergedPhases != nil {
		statePath := filepath.Join(maestroDir, "state", "worktrees", "cmd1.yaml")
		state, err := qh.worktreeManager.GetCommandState("cmd1")
		if err != nil {
			t.Fatalf("get worktree state: %v", err)
		}
		state.MergedPhases = mergedPhases
		if err := yamlutil.AtomicWrite(statePath, state); err != nil {
			t.Fatalf("rewrite worktree state: %v", err)
		}
	}
	writeCommandState(t, maestroDir, "cmd1", taskStates, nil)

	tasks := make([]model.Task, 0, len(taskStates))
	for id, st := range taskStates {
		tasks = append(tasks, model.Task{ID: id, CommandID: "cmd1", Status: st})
	}
	tqs := makeTaskQueues(map[string][]model.Task{"worker1": tasks})
	return qh, tqs
}

func TestCollectWorktreePhaseMerges_NoPhasesFallback(t *testing.T) {
	t.Parallel()
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusCreated, map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, nil)

	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 1 {
		t.Fatalf("expected 1 implicit merge item, got %d", len(items))
	}
	if items[0].PhaseID != "__implicit_phase" {
		t.Errorf("PhaseID = %q, want __implicit_phase", items[0].PhaseID)
	}
	if items[0].CommandID != "cmd1" {
		t.Errorf("CommandID = %q, want cmd1", items[0].CommandID)
	}
	if len(items[0].WorkerIDs) != 1 || items[0].WorkerIDs[0] != "worker1" {
		t.Errorf("WorkerIDs = %v, want [worker1]", items[0].WorkerIDs)
	}
}

func TestCollectWorktreePhaseMerges_NoPhasesSkipsOnFailure(t *testing.T) {
	t.Parallel()
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusCreated, map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusFailed,
	}, nil)
	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if items != nil {
		t.Errorf("expected nil items when a task failed, got %v", items)
	}
}

func TestCollectWorktreePhaseMerges_NoPhasesSkipsWhenNotAllTerminal(t *testing.T) {
	t.Parallel()
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusCreated, map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusInProgress,
	}, nil)
	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if items != nil {
		t.Errorf("expected nil items when a task is non-terminal, got %v", items)
	}
}

func TestCollectWorktreePhaseMerges_NoPhasesSkipsWhenAlreadyMerged(t *testing.T) {
	t.Parallel()
	// Integration already merged → fallback should not produce a new item.
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusMerged, map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)
	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if items != nil {
		t.Errorf("expected nil items when integration already merged, got %v", items)
	}
}

// TestStepWorktreeStallDetection_NoPhasesFastPath verifies the case 5
// fast-path: phase 0 件 + Integration.Status==created → 即時 stall シグナル発火
// (timeoutMin を待たない).
func TestStepWorktreeStallDetection_NoPhasesFastPath(t *testing.T) {
	t.Parallel()
	// recent updated_at: regular timeout path is NOT triggered.
	recent := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	qh, s := stallTestSetup(t, recent, model.IntegrationStatusCreated, false)

	// Overwrite command state with no phases.
	writeCommandState(t, qh.maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	qh.stepWorktreeStallDetection(s)

	if len(s.signals.Data.Signals) != 1 {
		t.Fatalf("expected 1 fast-path stall signal, got %d", len(s.signals.Data.Signals))
	}
	sig := s.signals.Data.Signals[0]
	if sig.Reason != "integration_stalled_no_phases:created" {
		t.Errorf("reason = %q, want integration_stalled_no_phases:created", sig.Reason)
	}
	if sig.Kind != "worktree_stalled" || sig.CommandID != "cmd1" {
		t.Errorf("unexpected signal: %+v", sig)
	}
	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !state.Integration.StallSignaled {
		t.Errorf("StallSignaled flag was not persisted")
	}
}

// --- classifyCommitError unit tests ---

func TestClassifyCommitError(t *testing.T) {
	t.Parallel()
	wrappedFiltered := fmt.Errorf("commit for worker w in command c: %w", worktreepkg.ErrAllFilesFiltered)
	policyErr := &worktreepkg.CommitPolicyViolationError{
		Violations: []worktreepkg.CommitPolicyViolation{
			{Code: "max_files_exceeded", Message: "too many"},
		},
	}
	policyEmpty := &worktreepkg.CommitPolicyViolationError{}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"all_files_filtered", wrappedFiltered, "all_files_filtered"},
		{"policy_with_code", policyErr, "policy_violation:max_files_exceeded"},
		{"policy_no_violations", policyEmpty, "policy_violation:unknown"},
		{"generic", errorspkg.New("boom"), "generic:boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyCommitError(tc.err)
			if got != tc.want {
				t.Errorf("classifyCommitError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// --- Phase B/C commit failure → signal flow integration tests ---

// TestPhaseBC_CommitFailure_AllFilesFiltered_Flow drives a real worktree
// manager through periodicScanPhaseB+C with a worker whose only dirty files
// are sensitive (.env). It verifies the full P1 contract:
//   - CommitFailures recorded with classified Reason
//   - MergeToIntegration skipped (no Conflicts/Error)
//   - SyncFromIntegration not invoked (worker stays Active, not synced)
//   - Integration status transitioned to Failed
//   - MarkPhaseMerged NOT recorded
//   - commit_failed signal emitted with Reason populated
func TestPhaseBC_CommitFailure_FlowTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		mutateCfg   func(cfg *model.WorktreeConfig)
		writeDirty  func(t *testing.T, wtPath string)
		wantReason  string
	}{
		{
			name:      "all_files_filtered",
			mutateCfg: func(cfg *model.WorktreeConfig) {},
			writeDirty: func(t *testing.T, wtPath string) {
				// Use a file that matches sensitiveFilePatterns but is unlikely
				// to appear in the user's global gitignore (e.g., .env often is).
				if err := os.WriteFile(filepath.Join(wtPath, "credentials.json"), []byte("S=1\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(wtPath, "secrets.secret"), []byte("S=1\n"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: "all_files_filtered",
		},
		{
			name: "policy_violation_max_files",
			mutateCfg: func(cfg *model.WorktreeConfig) {
				cfg.CommitPolicy = model.CommitPolicyConfig{MaxFiles: ptr.Int(1)}
			},
			writeDirty: func(t *testing.T, wtPath string) {
				for i := 0; i < 4; i++ {
					if err := os.WriteFile(filepath.Join(wtPath, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0644); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantReason: "policy_violation:max_files_exceeded",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			projectRoot := initTestGitRepo(t)
			maestroDir := filepath.Join(projectRoot, ".maestro")
			for _, sub := range []string{"queue", "results", "logs", "state/commands", "state/worktrees"} {
				if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
					t.Fatal(err)
				}
			}

			wtCfg := model.WorktreeConfig{
				Enabled: true, BaseBranch: "main", PathPrefix: ".maestro/worktrees",
				AutoCommit: true, AutoMerge: true, MergeStrategy: "ort",
			}
			tc.mutateCfg(&wtCfg)

			cfg := model.Config{
				Agents:   model.AgentsConfig{Workers: model.WorkerConfig{Count: 1}},
				Watcher:  model.WatcherConfig{DispatchLeaseSec: 300},
				Worktree: wtCfg,
			}
			qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelError)
			wm := NewWorktreeManager(maestroDir, wtCfg, log.New(&bytes.Buffer{}, "", 0), LogLevelError)
			qh.SetWorktreeManager(wm)

			commandID := "cmd_pf_" + tc.name
			if err := wm.EnsureWorkerWorktree(commandID, "worker1"); err != nil {
				t.Fatalf("EnsureWorkerWorktree: %v", err)
			}
			wtPath, err := wm.GetWorkerPath(commandID, "worker1")
			if err != nil {
				t.Fatalf("GetWorkerPath: %v", err)
			}
			tc.writeDirty(t, wtPath)

			// Build minimal phaseAResult with one worktree merge item.
			pa := phaseAResult{
				scanStart: time.Now(),
				work: deferredWork{
					worktreeMerges: []worktreeMergeItem{{
						CommandID:      commandID,
						PhaseID:        "phase1",
						WorkerIDs:      []string{"worker1"},
						WorkerPurposes: map[string]string{"worker1": "test"},
					}},
				},
			}

			pb := qh.periodicScanPhaseB(context.Background(), pa)
			if len(pb.worktreeMerges) != 1 {
				t.Fatalf("expected 1 worktreeMerge result, got %d", len(pb.worktreeMerges))
			}
			mr := pb.worktreeMerges[0]
			if len(mr.CommitFailures) != 1 {
				t.Fatalf("expected 1 CommitFailure, got %d", len(mr.CommitFailures))
			}
			if mr.CommitFailures[0].Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q (err=%v)", mr.CommitFailures[0].Reason, tc.wantReason, mr.CommitFailures[0].Error)
			}
			if mr.Error != nil || len(mr.Conflicts) != 0 {
				t.Errorf("merge should have been skipped: err=%v conflicts=%v", mr.Error, mr.Conflicts)
			}

			// Integration must have been marked Failed.
			state, err := wm.GetCommandState(commandID)
			if err != nil {
				t.Fatalf("GetCommandState: %v", err)
			}
			if state.Integration.Status != model.IntegrationStatusFailed {
				t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusFailed)
			}

			// Run phase C and verify commit_failed signal with Reason was emitted,
			// and MarkPhaseMerged was NOT called for the failing phase.
			qh.periodicScanPhaseC(pa, pb)

			signalQueue, _ := qh.queueStore.LoadPlannerSignalQueue()
			var found *model.PlannerSignal
			for i := range signalQueue.Signals {
				s := &signalQueue.Signals[i]
				if s.Kind == "commit_failed" && s.CommandID == commandID && s.WorkerID == "worker1" {
					found = s
					break
				}
			}
			if found == nil {
				t.Fatalf("commit_failed signal not found in %+v", signalQueue.Signals)
			}
			if found.Reason != tc.wantReason {
				t.Errorf("signal Reason = %q, want %q", found.Reason, tc.wantReason)
			}

			// MarkPhaseMerged must not have been recorded for phase1.
			state2, _ := wm.GetCommandState(commandID)
			if _, merged := state2.MergedPhases["phase1"]; merged {
				t.Errorf("phase1 should not be marked merged after commit failure")
			}
		})
	}
}

// TestPeriodicScanPhaseC_MergeConflictSignalStructuredFields verifies that
// MVP-1 structured conflict fields (BaseRef/OursRef/TheirsRef/Files) are
// propagated from MergeConflict into the emitted PlannerSignal, while the
// legacy free-form Message field continues to embed the same values for
// backward compatibility with CSV-style consumers.
func TestPeriodicScanPhaseC_MergeConflictSignalStructuredFields(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: false}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	commandID := "cmd_mc_struct"
	pa := phaseAResult{scanStart: time.Now()}
	pb := phaseBResult{
		worktreeMerges: []worktreeMergeResult{{
			Item: worktreeMergeItem{
				CommandID: commandID,
				PhaseID:   "phase1",
				WorkerIDs: []string{"worker1"},
			},
			Conflicts: []model.MergeConflict{{
				WorkerID:      "worker1",
				ConflictFiles: []string{"a.go", "b.go"},
				BaseRef:       "base_sha",
				OursRef:       "ours_sha",
				TheirsRef:     "theirs_sha",
			}},
		}},
	}

	qh.periodicScanPhaseC(pa, pb)

	signalQueue, _ := qh.queueStore.LoadPlannerSignalQueue()
	var found *model.PlannerSignal
	for i := range signalQueue.Signals {
		s := &signalQueue.Signals[i]
		if s.Kind == "merge_conflict" && s.CommandID == commandID {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("merge_conflict signal not found: %+v", signalQueue.Signals)
	}
	if found.ConflictBaseRef != "base_sha" || found.ConflictOursRef != "ours_sha" || found.ConflictTheirsRef != "theirs_sha" {
		t.Errorf("structured refs mismatch: base=%q ours=%q theirs=%q",
			found.ConflictBaseRef, found.ConflictOursRef, found.ConflictTheirsRef)
	}
	if len(found.ConflictFiles) != 2 || found.ConflictFiles[0] != "a.go" || found.ConflictFiles[1] != "b.go" {
		t.Errorf("ConflictFiles mismatch: %v", found.ConflictFiles)
	}
	if found.WorkerID != "worker1" {
		t.Errorf("WorkerID = %q, want worker1", found.WorkerID)
	}
	// Backward-compat: legacy CSV-style Message must still embed refs and files.
	if !strings.Contains(found.Message, "base:base_sha") ||
		!strings.Contains(found.Message, "ours:ours_sha") ||
		!strings.Contains(found.Message, "theirs:theirs_sha") ||
		!strings.Contains(found.Message, "a.go, b.go") {
		t.Errorf("legacy Message missing structured fields: %q", found.Message)
	}
}

// TestPlannerSignal_StructuredConflictFields_RoundTrip verifies that the new
// MVP-1 fields survive a YAML marshal/unmarshal round trip and that absent
// values stay omitted (preserving on-disk compatibility for non-conflict
// signal kinds).
func TestPlannerSignal_StructuredConflictFields_RoundTrip(t *testing.T) {
	t.Parallel()
	orig := model.PlannerSignal{
		Kind:              "merge_conflict",
		CommandID:         "cmd1",
		PhaseID:           "phase1",
		WorkerID:          "worker1",
		Message:           "[maestro] kind:merge_conflict ...",
		ConflictBaseRef:   "base_sha",
		ConflictOursRef:   "ours_sha",
		ConflictTheirsRef: "theirs_sha",
		ConflictFiles:     []string{"a.go", "b.go"},
		CreatedAt:         "2026-01-01T00:00:00Z",
		UpdatedAt:         "2026-01-01T00:00:00Z",
	}
	data, err := yamlv3.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got model.PlannerSignal
	if err := yamlv3.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ConflictBaseRef != orig.ConflictBaseRef ||
		got.ConflictOursRef != orig.ConflictOursRef ||
		got.ConflictTheirsRef != orig.ConflictTheirsRef ||
		len(got.ConflictFiles) != 2 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Non-conflict signals: empty fields must be omitted from YAML output.
	plain := model.PlannerSignal{
		Kind:      "commit_failed",
		CommandID: "cmd1",
		PhaseID:   "phase1",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	data2, err := yamlv3.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	out := string(data2)
	for _, key := range []string{"conflict_base_ref", "conflict_ours_ref", "conflict_theirs_ref", "conflict_files"} {
		if strings.Contains(out, key) {
			t.Errorf("expected %q to be omitted from non-conflict signal yaml: %s", key, out)
		}
	}
}
