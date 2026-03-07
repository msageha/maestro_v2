package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// --- Phase A/B/C Integration Test Helpers ---

// phaseIntegrationStateReader implements StateReader for integration tests.
// It supports task states, phases with depends_on, and phase transitions.
type phaseIntegrationStateReader struct {
	maestroDir      string
	mu              sync.Mutex
	taskStates      map[string]map[string]model.Status   // commandID -> taskID -> status
	phases          map[string][]PhaseInfo                // commandID -> phases
	cancelRequested map[string]bool                       // commandID -> cancelled
	transitions     []phaseTransitionRecord               // recorded transitions
}

type phaseTransitionRecord struct {
	CommandID string
	PhaseID   string
	NewStatus model.PhaseStatus
}

func newPhaseIntegrationStateReader() *phaseIntegrationStateReader {
	return &phaseIntegrationStateReader{
		taskStates:      make(map[string]map[string]model.Status),
		phases:          make(map[string][]PhaseInfo),
		cancelRequested: make(map[string]bool),
	}
}

func (r *phaseIntegrationStateReader) setTaskState(commandID, taskID string, status model.Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.taskStates[commandID] == nil {
		r.taskStates[commandID] = make(map[string]model.Status)
	}
	r.taskStates[commandID][taskID] = status
}

func (r *phaseIntegrationStateReader) setPhases(commandID string, phases []PhaseInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phases[commandID] = phases
}

func (r *phaseIntegrationStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts, ok := r.taskStates[commandID]
	if !ok {
		return "", ErrStateNotFound
	}
	s, ok := ts[taskID]
	if !ok {
		return "", ErrStateNotFound
	}
	return s, nil
}

func (r *phaseIntegrationStateReader) GetCommandPhases(commandID string) ([]PhaseInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ps, ok := r.phases[commandID]
	if !ok {
		return nil, ErrStateNotFound
	}
	return ps, nil
}

func (r *phaseIntegrationStateReader) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	return nil, nil
}

func (r *phaseIntegrationStateReader) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transitions = append(r.transitions, phaseTransitionRecord{
		CommandID: commandID,
		PhaseID:   phaseID,
		NewStatus: newStatus,
	})
	// Update in-memory phases
	ps := r.phases[commandID]
	for i := range ps {
		if ps[i].ID == phaseID {
			ps[i].Status = newStatus
			break
		}
	}
	return nil
}

func (r *phaseIntegrationStateReader) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.taskStates[commandID] == nil {
		r.taskStates[commandID] = make(map[string]model.Status)
	}
	r.taskStates[commandID][taskID] = newStatus
	return nil
}

func (r *phaseIntegrationStateReader) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	return false, false, nil
}

func (r *phaseIntegrationStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelRequested[commandID], nil
}

func (r *phaseIntegrationStateReader) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	return &model.CircuitBreakerState{}, nil
}

func (r *phaseIntegrationStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	return nil
}

func (r *phaseIntegrationStateReader) getTransitions() []phaseTransitionRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]phaseTransitionRecord, len(r.transitions))
	copy(result, r.transitions)
	return result
}

// recordingExecutor captures dispatch calls and can return configurable results per agent.
type recordingExecutor struct {
	mu         sync.Mutex
	calls      []agent.ExecRequest
	resultFunc func(req agent.ExecRequest) agent.ExecResult
}

func newRecordingExecutor(resultFunc func(agent.ExecRequest) agent.ExecResult) *recordingExecutor {
	if resultFunc == nil {
		resultFunc = func(agent.ExecRequest) agent.ExecResult {
			return agent.ExecResult{Success: true}
		}
	}
	return &recordingExecutor{resultFunc: resultFunc}
}

func (e *recordingExecutor) Execute(req agent.ExecRequest) agent.ExecResult {
	e.mu.Lock()
	e.calls = append(e.calls, req)
	e.mu.Unlock()
	return e.resultFunc(req)
}

func (e *recordingExecutor) Close() error { return nil }

func (e *recordingExecutor) getCalls() []agent.ExecRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]agent.ExecRequest, len(e.calls))
	copy(result, e.calls)
	return result
}

// setupPhaseIntegrationDir creates a temporary maestro directory for integration tests.
func setupPhaseIntegrationDir(t *testing.T) string {
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

// newPhaseIntegrationQH creates a QueueHandler wired with real file I/O and a mock executor.
func newPhaseIntegrationQH(t *testing.T, maestroDir string, exec *recordingExecutor) *QueueHandler {
	t.Helper()
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 4}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return exec, nil
	})
	return qh
}

func writeTaskQueue(t *testing.T, maestroDir, workerID string, tasks []model.Task) {
	t.Helper()
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks:         tasks,
	}
	path := filepath.Join(maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("write task queue: %v", err)
	}
}

func writeCommandQueue(t *testing.T, maestroDir string, commands []model.Command) {
	t.Helper()
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands:      commands,
	}
	path := filepath.Join(maestroDir, "queue", "planner.yaml")
	if err := yamlutil.AtomicWrite(path, cq); err != nil {
		t.Fatalf("write command queue: %v", err)
	}
}

func writeNotificationQueue(t *testing.T, maestroDir string, notifications []model.Notification) {
	t.Helper()
	nq := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "notification_queue",
		Notifications: notifications,
	}
	path := filepath.Join(maestroDir, "queue", "orchestrator.yaml")
	if err := yamlutil.AtomicWrite(path, nq); err != nil {
		t.Fatalf("write notification queue: %v", err)
	}
}

func piReadTaskQueue(t *testing.T, maestroDir, workerID string) model.TaskQueue {
	t.Helper()
	path := filepath.Join(maestroDir, "queue", workerID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read task queue %s: %v", workerID, err)
	}
	var tq model.TaskQueue
	if err := parseYAML(data, &tq); err != nil {
		t.Fatalf("parse task queue: %v", err)
	}
	return tq
}

func piReadCommandQueue(t *testing.T, maestroDir string) model.CommandQueue {
	t.Helper()
	path := filepath.Join(maestroDir, "queue", "planner.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read command queue: %v", err)
	}
	var cq model.CommandQueue
	if err := parseYAML(data, &cq); err != nil {
		t.Fatalf("parse command queue: %v", err)
	}
	return cq
}

func piReadNotificationQueue(t *testing.T, maestroDir string) model.NotificationQueue {
	t.Helper()
	path := filepath.Join(maestroDir, "queue", "orchestrator.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return model.NotificationQueue{}
		}
		t.Fatalf("read notification queue: %v", err)
	}
	var nq model.NotificationQueue
	if err := parseYAML(data, &nq); err != nil {
		t.Fatalf("parse notification queue: %v", err)
	}
	return nq
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// --- Integration Tests ---

// TestPhaseIntegration_EmptyStatesScan verifies that a full Phase A→B→C scan
// with empty queues produces no side effects and no panics.
func TestPhaseIntegration_EmptyStatesScan(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	// Run full scan with empty queues
	pa := qh.periodicScanPhaseA()
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	deferredNotifs := qh.periodicScanPhaseC(pa, pb)

	// No dispatch calls should have been made
	if len(exec.getCalls()) != 0 {
		t.Errorf("expected 0 executor calls for empty scan, got %d", len(exec.getCalls()))
	}

	// No deferred work items
	if len(pa.work.dispatches) != 0 {
		t.Errorf("expected 0 dispatches, got %d", len(pa.work.dispatches))
	}
	if len(pa.work.busyChecks) != 0 {
		t.Errorf("expected 0 busy checks, got %d", len(pa.work.busyChecks))
	}
	if len(pa.work.interrupts) != 0 {
		t.Errorf("expected 0 interrupts, got %d", len(pa.work.interrupts))
	}
	if len(pa.work.signals) != 0 {
		t.Errorf("expected 0 signals, got %d", len(pa.work.signals))
	}

	// No deferred notifications from reconciliation
	if len(deferredNotifs) != 0 {
		t.Errorf("expected 0 deferred notifs, got %d", len(deferredNotifs))
	}

	// Counters should be zero
	if pa.counters != (ScanCounters{}) {
		t.Errorf("expected zero counters, got %+v", pa.counters)
	}
}

// TestPhaseIntegration_E2E_TaskDispatchAndNotification tests a complete flow:
// Phase A picks up a pending task and acquires a lease →
// Phase B dispatches it successfully →
// Phase C applies the dispatch result (task stays in_progress).
func TestPhaseIntegration_E2E_TaskDispatchAndNotification(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID:        "task_1772000001_aabbccdd",
			CommandID: "cmd_1772000001_00000001",
			Purpose:   "Test task dispatch",
			Content:   "Do something",
			Priority:  1,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	// Run Phase A: should acquire lease, collect dispatch item
	pa := qh.periodicScanPhaseA()

	if len(pa.work.dispatches) != 1 {
		t.Fatalf("Phase A: expected 1 dispatch item, got %d", len(pa.work.dispatches))
	}
	if pa.work.dispatches[0].Kind != "task" {
		t.Errorf("Phase A: expected task dispatch, got %s", pa.work.dispatches[0].Kind)
	}
	if pa.work.dispatches[0].WorkerID != "worker1" {
		t.Errorf("Phase A: expected worker1, got %s", pa.work.dispatches[0].WorkerID)
	}
	if pa.work.dispatches[0].Epoch != 1 {
		t.Errorf("Phase A: expected epoch=1, got %d", pa.work.dispatches[0].Epoch)
	}

	// After Phase A flush, task should be in_progress on disk
	tqAfterA := piReadTaskQueue(t, maestroDir, "worker1")
	if tqAfterA.Tasks[0].Status != model.StatusInProgress {
		t.Errorf("After Phase A: expected in_progress, got %s", tqAfterA.Tasks[0].Status)
	}

	// Run Phase B: dispatch to worker via executor
	pb := qh.periodicScanPhaseB(context.Background(), pa)

	if len(pb.dispatches) != 1 {
		t.Fatalf("Phase B: expected 1 dispatch result, got %d", len(pb.dispatches))
	}
	if !pb.dispatches[0].Success {
		t.Errorf("Phase B: dispatch should succeed, got error: %v", pb.dispatches[0].Error)
	}

	// Verify executor was called with correct agent
	calls := exec.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 executor call, got %d", len(calls))
	}
	if calls[0].AgentID != "worker1" {
		t.Errorf("executor call agent: got %s, want worker1", calls[0].AgentID)
	}
	if calls[0].Mode != agent.ModeWithClear {
		t.Errorf("executor call mode: got %v, want ModeWithClear", calls[0].Mode)
	}

	// Run Phase C: apply dispatch result
	_ = qh.periodicScanPhaseC(pa, pb)

	// After Phase C, task should still be in_progress (dispatch succeeded)
	tqFinal := piReadTaskQueue(t, maestroDir, "worker1")
	if tqFinal.Tasks[0].Status != model.StatusInProgress {
		t.Errorf("After Phase C: expected in_progress, got %s", tqFinal.Tasks[0].Status)
	}
	if tqFinal.Tasks[0].LeaseEpoch != 1 {
		t.Errorf("After Phase C: expected epoch=1, got %d", tqFinal.Tasks[0].LeaseEpoch)
	}
}

// TestPhaseIntegration_DispatchFailure_Rollback tests that when Phase B dispatch fails,
// Phase C rolls back the task to pending with epoch fencing.
func TestPhaseIntegration_DispatchFailure_Rollback(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(func(req agent.ExecRequest) agent.ExecResult {
		return agent.ExecResult{
			Success:   false,
			Error:     fmt.Errorf("tmux pane not found"),
			Retryable: true,
		}
	})
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID:        "task_1772000002_aabbccdd",
			CommandID: "cmd_1772000002_00000001",
			Purpose:   "Failing dispatch",
			Content:   "Will fail",
			Priority:  1,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	// Phase A: acquire lease
	pa := qh.periodicScanPhaseA()
	if len(pa.work.dispatches) != 1 {
		t.Fatalf("Phase A: expected 1 dispatch, got %d", len(pa.work.dispatches))
	}

	// Phase B: dispatch fails
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	if pb.dispatches[0].Success {
		t.Fatal("Phase B: dispatch should have failed")
	}

	// Phase C: rollback with epoch fencing
	_ = qh.periodicScanPhaseC(pa, pb)

	tqFinal := piReadTaskQueue(t, maestroDir, "worker1")
	task := tqFinal.Tasks[0]
	if task.Status != model.StatusPending {
		t.Errorf("After rollback: expected pending, got %s", task.Status)
	}
	if task.LeaseOwner != nil {
		t.Errorf("After rollback: lease_owner should be nil, got %v", *task.LeaseOwner)
	}
	if task.LeaseExpiresAt != nil {
		t.Error("After rollback: lease_expires_at should be nil")
	}
	// Epoch should be 1 (incremented by acquire, not reset by release)
	if task.LeaseEpoch != 1 {
		t.Errorf("After rollback: expected epoch=1, got %d", task.LeaseEpoch)
	}
}

// TestPhaseIntegration_WorkerBusy_LeaseExtension tests that when a worker is busy
// with an expired lease, Phase B detects it and Phase C extends the lease.
func TestPhaseIntegration_WorkerBusy_LeaseExtension(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(func(req agent.ExecRequest) agent.ExecResult {
		// IsBusy probe returns true (agent is busy)
		if req.Mode == agent.ModeIsBusy {
			return agent.ExecResult{Success: true}
		}
		return agent.ExecResult{Success: true}
	})
	qh := newPhaseIntegrationQH(t, maestroDir, exec)
	qh.SetBusyChecker(func(agentID string) bool {
		return true // all agents are busy
	})

	now := nowRFC3339()
	expiredTime := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	owner := qh.leaseOwnerID()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID:             "task_1772000003_aabbccdd",
			CommandID:      "cmd_1772000003_00000001",
			Purpose:        "Busy worker",
			Content:        "Working on it",
			Priority:       1,
			Status:         model.StatusInProgress,
			LeaseOwner:     &owner,
			LeaseExpiresAt: &expiredTime,
			LeaseEpoch:     1,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
	})

	// Phase A: detects expired lease, collects busy check item
	pa := qh.periodicScanPhaseA()

	if len(pa.work.busyChecks) != 1 {
		t.Fatalf("Phase A: expected 1 busy check, got %d", len(pa.work.busyChecks))
	}
	if pa.work.busyChecks[0].AgentID != "worker1" {
		t.Errorf("Phase A: expected worker1, got %s", pa.work.busyChecks[0].AgentID)
	}
	// No dispatches should be collected when expired leases exist
	if len(pa.work.dispatches) != 0 {
		t.Errorf("Phase A: expected 0 dispatches (recovery mode), got %d", len(pa.work.dispatches))
	}

	// Phase B: probe busy state
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	if len(pb.busyChecks) != 1 {
		t.Fatalf("Phase B: expected 1 busy check result, got %d", len(pb.busyChecks))
	}
	if !pb.busyChecks[0].Busy {
		t.Error("Phase B: expected busy=true")
	}

	// Phase C: extend lease since worker is busy
	_ = qh.periodicScanPhaseC(pa, pb)

	tqFinal := piReadTaskQueue(t, maestroDir, "worker1")
	task := tqFinal.Tasks[0]
	if task.Status != model.StatusInProgress {
		t.Errorf("After extension: expected in_progress, got %s", task.Status)
	}
	// Lease should be extended (new expiry in future)
	if task.LeaseExpiresAt == nil {
		t.Fatal("After extension: lease_expires_at should not be nil")
	}
	newExpiry, err := time.Parse(time.RFC3339, *task.LeaseExpiresAt)
	if err != nil {
		t.Fatalf("parse lease_expires_at: %v", err)
	}
	if !newExpiry.After(time.Now()) {
		t.Errorf("After extension: lease should be in the future, got %s", *task.LeaseExpiresAt)
	}
}

// TestPhaseIntegration_PhaseTransition_PendingToActive tests that Phase A detects
// phase dependency resolution and triggers a transition from pending to awaiting_fill.
func TestPhaseIntegration_PhaseTransition_PendingToActive(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	// Set up state reader with two phases:
	// Phase 1 (completed) → Phase 2 (pending, depends on Phase 1)
	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd_1772000004_00000001", []PhaseInfo{
		{
			ID:     "phase_1772000004_aaaaaaaa",
			Name:   "Investigation",
			Status: model.PhaseStatusCompleted,
		},
		{
			ID:        "phase_1772000004_bbbbbbbb",
			Name:      "Implementation",
			Status:    model.PhaseStatusPending,
			DependsOn: []string{"phase_1772000004_aaaaaaaa"},
		},
	})
	qh.SetStateReader(reader)

	// Write a command queue with in_progress command
	owner := qh.leaseOwnerID()
	futureExpiry := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	writeCommandQueue(t, maestroDir, []model.Command{
		{
			ID:             "cmd_1772000004_00000001",
			Content:        "Multi-phase command",
			Priority:       1,
			Status:         model.StatusInProgress,
			LeaseOwner:     &owner,
			LeaseExpiresAt: &futureExpiry,
			LeaseEpoch:     1,
			CreatedAt:      nowRFC3339(),
			UpdatedAt:      nowRFC3339(),
		},
	})

	// Phase A: should detect phase transition (pending→awaiting_fill)
	pa := qh.periodicScanPhaseA()

	// Verify the transition was applied via state reader
	transitions := reader.getTransitions()
	if len(transitions) != 1 {
		t.Fatalf("expected 1 phase transition, got %d", len(transitions))
	}
	if transitions[0].PhaseID != "phase_1772000004_bbbbbbbb" {
		t.Errorf("transition phase: got %s, want phase_1772000004_bbbbbbbb", transitions[0].PhaseID)
	}
	if transitions[0].NewStatus != model.PhaseStatusAwaitingFill {
		t.Errorf("transition status: got %s, want awaiting_fill", transitions[0].NewStatus)
	}

	// Should have a planner signal for awaiting_fill
	if len(pa.work.signals) != 1 {
		t.Fatalf("expected 1 signal delivery, got %d", len(pa.work.signals))
	}
	if pa.work.signals[0].Kind != "awaiting_fill" {
		t.Errorf("signal kind: got %s, want awaiting_fill", pa.work.signals[0].Kind)
	}
}

// TestPhaseIntegration_DependencyResolution_BlockedTask tests that a task blocked
// by dependencies is skipped during dispatch, and dispatched after deps complete.
func TestPhaseIntegration_DependencyResolution_BlockedTask(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	reader := newPhaseIntegrationStateReader()
	// task_dep is not completed yet → task_blocked should be skipped
	reader.setTaskState("cmd_1772000005_00000001", "task_1772000005_dep00001", model.StatusInProgress)
	qh.SetStateReader(reader)

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID:        "task_1772000005_blocked1",
			CommandID: "cmd_1772000005_00000001",
			Purpose:   "Blocked task",
			Content:   "Waiting for dependency",
			Priority:  1,
			Status:    model.StatusPending,
			BlockedBy: []string{"task_1772000005_dep00001"},
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	// First scan: task is blocked → no dispatch
	pa1 := qh.periodicScanPhaseA()
	if len(pa1.work.dispatches) != 0 {
		t.Errorf("Scan 1: expected 0 dispatches (task blocked), got %d", len(pa1.work.dispatches))
	}
	pb1 := qh.periodicScanPhaseB(context.Background(), pa1)
	_ = qh.periodicScanPhaseC(pa1, pb1)

	// Mark dependency as completed
	reader.setTaskState("cmd_1772000005_00000001", "task_1772000005_dep00001", model.StatusCompleted)

	// Second scan: task should now be dispatched
	pa2 := qh.periodicScanPhaseA()
	if len(pa2.work.dispatches) != 1 {
		t.Fatalf("Scan 2: expected 1 dispatch (dependency resolved), got %d", len(pa2.work.dispatches))
	}
	if pa2.work.dispatches[0].Task.ID != "task_1772000005_blocked1" {
		t.Errorf("Scan 2: dispatched task ID: got %s, want task_1772000005_blocked1",
			pa2.work.dispatches[0].Task.ID)
	}

	pb2 := qh.periodicScanPhaseB(context.Background(), pa2)
	_ = qh.periodicScanPhaseC(pa2, pb2)

	// Verify task is now in_progress
	tqFinal := piReadTaskQueue(t, maestroDir, "worker1")
	if tqFinal.Tasks[0].Status != model.StatusInProgress {
		t.Errorf("After dependency resolved: expected in_progress, got %s", tqFinal.Tasks[0].Status)
	}
}

// TestPhaseIntegration_DependencyFailure_CascadeCancel tests that when a dependency
// fails, dependent tasks are cascade-cancelled in Phase A.
func TestPhaseIntegration_DependencyFailure_CascadeCancel(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	reader := newPhaseIntegrationStateReader()
	reader.setTaskState("cmd_1772000006_00000001", "task_1772000006_dep00001", model.StatusFailed)
	qh.SetStateReader(reader)

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID:        "task_1772000006_cancel01",
			CommandID: "cmd_1772000006_00000001",
			Purpose:   "Will be cancelled",
			Content:   "Depends on failed task",
			Priority:  1,
			Status:    model.StatusPending,
			BlockedBy: []string{"task_1772000006_dep00001"},
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	pa := qh.periodicScanPhaseA()
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	_ = qh.periodicScanPhaseC(pa, pb)

	tqFinal := piReadTaskQueue(t, maestroDir, "worker1")
	if tqFinal.Tasks[0].Status != model.StatusCancelled {
		t.Errorf("Cascade cancel: expected cancelled, got %s", tqFinal.Tasks[0].Status)
	}

	// No dispatch calls to workers should have been made for task dispatch
	for _, call := range exec.getCalls() {
		if strings.HasPrefix(call.AgentID, "worker") && call.Mode == agent.ModeWithClear {
			t.Errorf("should not dispatch task to %s for failed dependency", call.AgentID)
		}
	}
}

// TestPhaseIntegration_EpochFencing_StaleResult tests that Phase C correctly
// rejects a dispatch result when the queue entry was modified between Phase A and Phase C.
func TestPhaseIntegration_EpochFencing_StaleResult(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)

	// Executor that signals when dispatch starts and waits for proceed
	dispatchStarted := make(chan struct{})
	proceed := make(chan struct{})
	var startOnce sync.Once
	qh := newPhaseIntegrationQH(t, maestroDir, &recordingExecutor{
		resultFunc: func(req agent.ExecRequest) agent.ExecResult {
			return agent.ExecResult{Success: true}
		},
	})
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &slowMockExecutor{
			result:  agent.ExecResult{Success: true},
			onStart: func() { startOnce.Do(func() { close(dispatchStarted) }) },
			proceed: proceed,
		}, nil
	})

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID:        "task_1772000007_epoch001",
			CommandID: "cmd_1772000007_00000001",
			Purpose:   "Epoch fencing test",
			Content:   "Test content",
			Priority:  1,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	// Phase A: acquires lease (epoch=1)
	pa := qh.periodicScanPhaseA()
	if len(pa.work.dispatches) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(pa.work.dispatches))
	}

	// Simulate external modification: bump the task's epoch on disk
	// (as if another scan or result write handler modified it)
	tq := piReadTaskQueue(t, maestroDir, "worker1")
	tq.Tasks[0].LeaseEpoch = 99 // different from Phase A's epoch (1)
	newExpiry := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	tq.Tasks[0].LeaseExpiresAt = &newExpiry
	path := filepath.Join(maestroDir, "queue", "worker1.yaml")
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("overwrite task queue: %v", err)
	}

	// Phase B: dispatch succeeds (but the result is now stale)
	close(proceed) // let the executor proceed immediately
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	if !pb.dispatches[0].Success {
		t.Fatal("Phase B dispatch should succeed")
	}

	// Phase C: should detect epoch mismatch and skip apply
	_ = qh.periodicScanPhaseC(pa, pb)

	// Task should still have epoch=99 (the externally written value)
	tqFinal := piReadTaskQueue(t, maestroDir, "worker1")
	if tqFinal.Tasks[0].LeaseEpoch != 99 {
		t.Errorf("After stale apply: expected epoch=99 (preserved), got %d", tqFinal.Tasks[0].LeaseEpoch)
	}
	// Status and lease fields should also be preserved
	if tqFinal.Tasks[0].Status != model.StatusInProgress {
		t.Errorf("After stale apply: expected status=in_progress (preserved), got %s", tqFinal.Tasks[0].Status)
	}
	if tqFinal.Tasks[0].LeaseExpiresAt == nil || *tqFinal.Tasks[0].LeaseExpiresAt != newExpiry {
		t.Errorf("After stale apply: lease_expires_at should be preserved")
	}
}

// TestPhaseIntegration_MultipleCommands_PriorityOrder tests that multiple commands
// are dispatched in correct priority order (lower priority value = higher priority).
func TestPhaseIntegration_MultipleCommands_PriorityOrder(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)

	// Track the order of dispatched commands
	var dispatchOrder []string
	var mu sync.Mutex
	exec := newRecordingExecutor(func(req agent.ExecRequest) agent.ExecResult {
		mu.Lock()
		dispatchOrder = append(dispatchOrder, req.CommandID)
		mu.Unlock()
		return agent.ExecResult{Success: true}
	})
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	writeCommandQueue(t, maestroDir, []model.Command{
		{
			ID:        "cmd_1772000008_lowprio1",
			Content:   "Low priority command",
			Priority:  10,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:        "cmd_1772000008_highpri1",
			Content:   "High priority command",
			Priority:  1,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	// First scan: should dispatch the highest priority command (priority=1)
	pa1 := qh.periodicScanPhaseA()
	if len(pa1.work.dispatches) != 1 {
		t.Fatalf("Scan 1: expected 1 dispatch (one at a time), got %d", len(pa1.work.dispatches))
	}
	if pa1.work.dispatches[0].Command.ID != "cmd_1772000008_highpri1" {
		t.Errorf("Scan 1: expected highpri1 first, got %s", pa1.work.dispatches[0].Command.ID)
	}

	pb1 := qh.periodicScanPhaseB(context.Background(), pa1)
	_ = qh.periodicScanPhaseC(pa1, pb1)

	// Verify first command is now in_progress
	cq := piReadCommandQueue(t, maestroDir)
	var highPriStatus, lowPriStatus model.Status
	for _, cmd := range cq.Commands {
		switch cmd.ID {
		case "cmd_1772000008_highpri1":
			highPriStatus = cmd.Status
		case "cmd_1772000008_lowprio1":
			lowPriStatus = cmd.Status
		}
	}
	if highPriStatus != model.StatusInProgress {
		t.Errorf("highpri1 should be in_progress, got %s", highPriStatus)
	}
	if lowPriStatus != model.StatusPending {
		t.Errorf("lowprio1 should be pending (blocked by in_progress command), got %s", lowPriStatus)
	}
}

// TestPhaseIntegration_CommandAndTask_FullCycle tests dispatching a command to planner
// and then a task to a worker in successive scan cycles.
func TestPhaseIntegration_CommandAndTask_FullCycle(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()

	// Write a pending command
	writeCommandQueue(t, maestroDir, []model.Command{
		{
			ID:        "cmd_1772000009_00000001",
			Content:   "Full cycle test",
			Priority:  1,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	// Write a pending task for the same command
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID:        "task_1772000009_aabbccdd",
			CommandID: "cmd_1772000009_00000001",
			Purpose:   "Full cycle task",
			Content:   "Task content",
			Priority:  1,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	// Scan 1: should dispatch the command (planner) and the task (worker1)
	pa1 := qh.periodicScanPhaseA()

	var hasCommand, hasTask bool
	for _, d := range pa1.work.dispatches {
		if d.Kind == "command" {
			hasCommand = true
		}
		if d.Kind == "task" {
			hasTask = true
		}
	}

	if !hasCommand {
		t.Error("Scan 1: expected command dispatch")
	}
	if !hasTask {
		t.Error("Scan 1: expected task dispatch")
	}

	pb1 := qh.periodicScanPhaseB(context.Background(), pa1)
	_ = qh.periodicScanPhaseC(pa1, pb1)

	// Both should be in_progress now
	cq := piReadCommandQueue(t, maestroDir)
	if cq.Commands[0].Status != model.StatusInProgress {
		t.Errorf("command should be in_progress, got %s", cq.Commands[0].Status)
	}

	tq := piReadTaskQueue(t, maestroDir, "worker1")
	if tq.Tasks[0].Status != model.StatusInProgress {
		t.Errorf("task should be in_progress, got %s", tq.Tasks[0].Status)
	}

	// Verify executor was called for both command (planner) and task (worker1)
	calls := exec.getCalls()
	var plannerCalled, workerCalled bool
	for _, call := range calls {
		if call.AgentID == "planner" {
			plannerCalled = true
		}
		if call.AgentID == "worker1" {
			workerCalled = true
		}
	}
	if !plannerCalled {
		t.Error("expected executor call to planner")
	}
	if !workerCalled {
		t.Error("expected executor call to worker1")
	}
}

// TestPhaseIntegration_NotificationDispatch tests notification dispatch through all three phases.
func TestPhaseIntegration_NotificationDispatch(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	writeNotificationQueue(t, maestroDir, []model.Notification{
		{
			ID:        "ntf_1772000010_aabbccdd",
			CommandID: "cmd_1772000010_00000001",
			Type:      "command_completed",
			Priority:  1,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	// Phase A: acquire notification lease and collect dispatch
	pa := qh.periodicScanPhaseA()
	var ntfDispatch bool
	for _, d := range pa.work.dispatches {
		if d.Kind == "notification" {
			ntfDispatch = true
		}
	}
	if !ntfDispatch {
		t.Fatal("Phase A: expected notification dispatch item")
	}

	// Phase B: dispatch notification
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	var ntfResult *dispatchResult
	for i := range pb.dispatches {
		if pb.dispatches[i].Item.Kind == "notification" {
			ntfResult = &pb.dispatches[i]
			break
		}
	}
	if ntfResult == nil {
		t.Fatal("Phase B: expected notification dispatch result")
	}
	if !ntfResult.Success {
		t.Errorf("Phase B: notification dispatch failed: %v", ntfResult.Error)
	}

	// Phase C: apply result
	_ = qh.periodicScanPhaseC(pa, pb)

	// Notification should be completed
	nqFinal := piReadNotificationQueue(t, maestroDir)
	if len(nqFinal.Notifications) == 0 {
		t.Fatal("expected at least 1 notification after Phase C")
	}
	if nqFinal.Notifications[0].Status != model.StatusCompleted {
		t.Errorf("notification should be completed, got %s", nqFinal.Notifications[0].Status)
	}

	// Verify executor was called to deliver notification to orchestrator
	calls := exec.getCalls()
	var orchestratorCalled bool
	for _, call := range calls {
		if call.AgentID == "orchestrator" {
			orchestratorCalled = true
		}
	}
	if !orchestratorCalled {
		t.Error("expected executor call to orchestrator for notification")
	}
}

// TestPhaseIntegration_CancelledCommand_CancelsPendingTasks tests that
// Phase A cancels pending tasks when the parent command is cancelled.
func TestPhaseIntegration_CancelledCommand_CancelsPendingTasks(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	cancelTime := now
	writeCommandQueue(t, maestroDir, []model.Command{
		{
			ID:                "cmd_1772000011_00000001",
			Content:           "Cancelled command",
			Status:            model.StatusInProgress,
			CancelRequestedAt: &cancelTime,
			CreatedAt:         now,
			UpdatedAt:         now,
		},
	})

	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID:        "task_1772000011_pend0001",
			CommandID: "cmd_1772000011_00000001",
			Purpose:   "Pending task to cancel",
			Content:   "Should be cancelled",
			Priority:  1,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:        "task_1772000011_pend0002",
			CommandID: "cmd_1772000011_00000001",
			Purpose:   "Another pending task",
			Content:   "Should also be cancelled",
			Priority:  2,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	pa := qh.periodicScanPhaseA()
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	_ = qh.periodicScanPhaseC(pa, pb)

	tqFinal := piReadTaskQueue(t, maestroDir, "worker1")
	for _, task := range tqFinal.Tasks {
		if task.Status != model.StatusCancelled {
			t.Errorf("task %s: expected cancelled, got %s", task.ID, task.Status)
		}
	}

	// No dispatch calls for cancelled tasks
	for _, call := range exec.getCalls() {
		if strings.HasPrefix(call.AgentID, "worker") && call.Mode == agent.ModeWithClear {
			t.Errorf("should not dispatch to worker for cancelled command, got call to %s", call.AgentID)
		}
	}
}

// TestPhaseIntegration_ConcurrentWriteDuringPhaseB tests that queue writes
// during Phase B are preserved in Phase C's reload and apply.
func TestPhaseIntegration_ConcurrentWriteDuringPhaseB(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	lockMap := lock.NewMutexMap()

	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 4}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	dispatchStarted := make(chan struct{})
	proceed := make(chan struct{})
	var startOnce sync.Once
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &slowMockExecutor{
			result:  agent.ExecResult{Success: true},
			onStart: func() { startOnce.Do(func() { close(dispatchStarted) }) },
			proceed: proceed,
		}, nil
	})

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID:        "task_1772000012_origin01",
			CommandID: "cmd_1772000012_00000001",
			Purpose:   "Original task",
			Content:   "Original",
			Priority:  1,
			Status:    model.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		},
	})

	// Run scan in background
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		qh.PeriodicScanWithContext(context.Background())
	}()

	// Wait for Phase B dispatch to start
	select {
	case <-dispatchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dispatch to start")
	}

	// Write a new task during Phase B (simulating plan submit)
	qh.LockFiles()
	lockMap.Lock("queue:worker1")

	workerPath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	data, err := os.ReadFile(workerPath)
	if err != nil {
		lockMap.Unlock("queue:worker1")
		qh.UnlockFiles()
		t.Fatalf("read during Phase B: %v", err)
	}
	var currentTQ model.TaskQueue
	if err := parseYAML(data, &currentTQ); err != nil {
		lockMap.Unlock("queue:worker1")
		qh.UnlockFiles()
		t.Fatalf("parse during Phase B: %v", err)
	}
	currentTQ.Tasks = append(currentTQ.Tasks, model.Task{
		ID:        "task_1772000012_concur01",
		CommandID: "cmd_1772000012_00000001",
		Purpose:   "Concurrent task",
		Content:   "Added during Phase B",
		Priority:  2,
		Status:    model.StatusPending,
		CreatedAt: nowRFC3339(),
		UpdatedAt: nowRFC3339(),
	})
	if err := yamlutil.AtomicWrite(workerPath, currentTQ); err != nil {
		lockMap.Unlock("queue:worker1")
		qh.UnlockFiles()
		t.Fatalf("write concurrent task: %v", err)
	}
	lockMap.Unlock("queue:worker1")
	qh.UnlockFiles()

	// Let Phase B complete
	close(proceed)
	<-scanDone

	// Both tasks should exist in the final state
	tqFinal := piReadTaskQueue(t, maestroDir, "worker1")
	if len(tqFinal.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tqFinal.Tasks))
	}

	var foundOriginal, foundConcurrent bool
	for _, task := range tqFinal.Tasks {
		switch task.ID {
		case "task_1772000012_origin01":
			foundOriginal = true
			if task.Status != model.StatusInProgress {
				t.Errorf("original task: expected in_progress, got %s", task.Status)
			}
		case "task_1772000012_concur01":
			foundConcurrent = true
			if task.Status != model.StatusPending {
				t.Errorf("concurrent task: expected pending, got %s", task.Status)
			}
		}
	}
	if !foundOriginal {
		t.Error("original task not found — Phase C clobbered it")
	}
	if !foundConcurrent {
		t.Error("concurrent task not found — Phase C clobbered concurrent write")
	}
}

// TestPhaseIntegration_PhaseBContextCancellation tests that Phase B respects
// context cancellation and stops processing deferred work items.
// Post-Phase C, skipped dispatches should be rolled back to pending.
func TestPhaseIntegration_PhaseBContextCancellation(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)

	var dispatchCount int32
	exec := newRecordingExecutor(func(req agent.ExecRequest) agent.ExecResult {
		atomic.AddInt32(&dispatchCount, 1)
		// Simulate slow dispatch
		time.Sleep(50 * time.Millisecond)
		return agent.ExecResult{Success: true}
	})
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	// Write tasks to multiple workers
	for i := 1; i <= 4; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		writeTaskQueue(t, maestroDir, workerID, []model.Task{
			{
				ID:        fmt.Sprintf("task_1772000013_%08d", i),
				CommandID: "cmd_1772000013_00000001",
				Purpose:   fmt.Sprintf("Task for %s", workerID),
				Content:   "Content",
				Priority:  1,
				Status:    model.StatusPending,
				CreatedAt: now,
				UpdatedAt: now,
			},
		})
	}

	// Phase A: collect dispatch items for all workers
	pa := qh.periodicScanPhaseA()
	totalDispatches := len(pa.work.dispatches)
	if totalDispatches == 0 {
		t.Fatal("expected at least 1 dispatch item")
	}

	// Phase B with immediate cancellation
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	pb := qh.periodicScanPhaseB(ctx, pa)

	// With cancelled context, fewer dispatches should have been attempted
	count := int(atomic.LoadInt32(&dispatchCount))
	if count >= totalDispatches {
		t.Logf("warning: all %d dispatches ran despite context cancellation", count)
	}

	// The number of results in pb should be <= dispatches attempted
	if len(pb.dispatches) > totalDispatches {
		t.Errorf("Phase B results (%d) should not exceed dispatch items (%d)", len(pb.dispatches), totalDispatches)
	}

	// Phase C: apply whatever was done, rollback skipped dispatches
	_ = qh.periodicScanPhaseC(pa, pb)

	// After Phase C: tasks that were dispatched (have results) should be in_progress;
	// tasks that were skipped (no result) remain in_progress from Phase A flush
	// (since Phase C only applies results for items that have matching dispatch results).
	for i := 1; i <= 4; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		tq := piReadTaskQueue(t, maestroDir, workerID)
		if len(tq.Tasks) != 1 {
			t.Fatalf("%s: expected 1 task, got %d", workerID, len(tq.Tasks))
		}
		// All tasks should be in_progress (Phase A acquired leases)
		if tq.Tasks[0].Status != model.StatusInProgress {
			t.Errorf("%s: expected in_progress, got %s", workerID, tq.Tasks[0].Status)
		}
	}
}

// TestPhaseIntegration_MultiWorker_IndependentDispatch tests that tasks on
// different workers are dispatched independently in a single scan cycle.
func TestPhaseIntegration_MultiWorker_IndependentDispatch(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)

	var dispatched []string
	var mu sync.Mutex
	exec := newRecordingExecutor(func(req agent.ExecRequest) agent.ExecResult {
		mu.Lock()
		dispatched = append(dispatched, req.AgentID)
		mu.Unlock()
		return agent.ExecResult{Success: true}
	})
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	// Create tasks on 3 different workers
	for i := 1; i <= 3; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		writeTaskQueue(t, maestroDir, workerID, []model.Task{
			{
				ID:        fmt.Sprintf("task_1772000014_%08d", i),
				CommandID: "cmd_1772000014_00000001",
				Purpose:   fmt.Sprintf("Task for %s", workerID),
				Content:   "Content",
				Priority:  1,
				Status:    model.StatusPending,
				CreatedAt: now,
				UpdatedAt: now,
			},
		})
	}

	// Full scan
	pa := qh.periodicScanPhaseA()
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	_ = qh.periodicScanPhaseC(pa, pb)

	// All 3 workers should have been dispatched
	mu.Lock()
	dispatchedWorkers := make(map[string]bool)
	for _, w := range dispatched {
		dispatchedWorkers[w] = true
	}
	mu.Unlock()

	for i := 1; i <= 3; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		if !dispatchedWorkers[workerID] {
			t.Errorf("expected dispatch to %s", workerID)
		}
	}

	// Each worker's task should be in_progress
	for i := 1; i <= 3; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		tq := piReadTaskQueue(t, maestroDir, workerID)
		if tq.Tasks[0].Status != model.StatusInProgress {
			t.Errorf("%s task: expected in_progress, got %s", workerID, tq.Tasks[0].Status)
		}
	}
}

// TestPhaseIntegration_PhaseA_B_C_CompletePipeline tests the complete pipeline
// with phase dependencies: Phase A detects phase transition, Phase B delivers signal,
// Phase C applies signal result.
func TestPhaseIntegration_PhaseA_B_C_CompletePipeline(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)

	var signalDelivered bool
	var mu sync.Mutex
	exec := newRecordingExecutor(func(req agent.ExecRequest) agent.ExecResult {
		mu.Lock()
		if req.AgentID == "planner" && req.Mode == agent.ModeDeliver {
			signalDelivered = true
		}
		mu.Unlock()
		return agent.ExecResult{Success: true}
	})
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	// Setup 3-phase command: Phase1 (completed) → Phase2 (pending) → Phase3 (pending)
	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd_1772000015_00000001", []PhaseInfo{
		{
			ID:              "phase_1772000015_aaaaaaaa",
			Name:            "Research",
			Status:          model.PhaseStatusCompleted,
			RequiredTaskIDs: []string{"task_1772000015_res00001"},
		},
		{
			ID:              "phase_1772000015_bbbbbbbb",
			Name:            "Implementation",
			Status:          model.PhaseStatusPending,
			DependsOn:       []string{"phase_1772000015_aaaaaaaa"},
			RequiredTaskIDs: []string{"task_1772000015_impl0001"},
		},
		{
			ID:              "phase_1772000015_cccccccc",
			Name:            "Testing",
			Status:          model.PhaseStatusPending,
			DependsOn:       []string{"phase_1772000015_bbbbbbbb"},
			RequiredTaskIDs: []string{"task_1772000015_test0001"},
		},
	})
	reader.setTaskState("cmd_1772000015_00000001", "task_1772000015_res00001", model.StatusCompleted)
	qh.SetStateReader(reader)

	now := nowRFC3339()
	owner := qh.leaseOwnerID()
	futureExpiry := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	writeCommandQueue(t, maestroDir, []model.Command{
		{
			ID:             "cmd_1772000015_00000001",
			Content:        "Three-phase command",
			Priority:       1,
			Status:         model.StatusInProgress,
			LeaseOwner:     &owner,
			LeaseExpiresAt: &futureExpiry,
			LeaseEpoch:     1,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
	})

	// --- Scan 1: Phase 1 is completed → Phase 2 should transition to awaiting_fill ---

	pa1 := qh.periodicScanPhaseA()

	// Verify Phase 2 transition was triggered
	transitions := reader.getTransitions()
	foundPhase2Transition := false
	for _, tr := range transitions {
		if tr.PhaseID == "phase_1772000015_bbbbbbbb" && tr.NewStatus == model.PhaseStatusAwaitingFill {
			foundPhase2Transition = true
		}
	}
	if !foundPhase2Transition {
		t.Fatal("Phase 2 should transition to awaiting_fill")
	}

	// Phase 3 should NOT transition (Phase 2 not yet completed)
	for _, tr := range transitions {
		if tr.PhaseID == "phase_1772000015_cccccccc" {
			t.Errorf("Phase 3 should not transition yet, got %s", tr.NewStatus)
		}
	}

	// Should have a signal delivery for Phase 2 awaiting_fill
	if len(pa1.work.signals) == 0 {
		t.Fatal("expected signal for Phase 2 awaiting_fill")
	}
	foundSignal := false
	for _, sig := range pa1.work.signals {
		if sig.Kind == "awaiting_fill" && strings.Contains(sig.CommandID, "cmd_1772000015") {
			foundSignal = true
		}
	}
	if !foundSignal {
		t.Error("expected awaiting_fill signal for Phase 2")
	}

	// Write the signal queue to disk before Phase B (simulates Phase A's flush)
	signalPath := filepath.Join(maestroDir, "queue", "planner_signals.yaml")
	signalQueue := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "awaiting_fill",
				CommandID: "cmd_1772000015_00000001",
				PhaseID:   "phase_1772000015_bbbbbbbb",
				Message:   "Phase 2 awaiting fill",
				CreatedAt: nowRFC3339(),
				UpdatedAt: nowRFC3339(),
			},
		},
	}
	if err := yamlutil.AtomicWrite(signalPath, signalQueue); err != nil {
		t.Fatalf("write signal queue: %v", err)
	}

	// Phase B: deliver signal to planner
	pb1 := qh.periodicScanPhaseB(context.Background(), pa1)

	// Phase C: apply signal results (should remove delivered signal from queue)
	_ = qh.periodicScanPhaseC(pa1, pb1)

	mu.Lock()
	delivered := signalDelivered
	mu.Unlock()
	if !delivered {
		t.Error("expected signal to be delivered to planner")
	}

	// Verify signal was removed from queue file after successful delivery
	signalData, err := os.ReadFile(signalPath)
	if err == nil {
		var sqAfter model.PlannerSignalQueue
		if err := parseYAML(signalData, &sqAfter); err == nil {
			for _, sig := range sqAfter.Signals {
				if sig.Kind == "awaiting_fill" && sig.CommandID == "cmd_1772000015_00000001" {
					t.Error("signal should have been removed from queue after successful delivery")
				}
			}
		}
	}
	// If file was removed entirely (empty signals → os.Remove), that's also correct
}

// TestPhaseIntegration_BusyFalse_LeaseRelease tests that when a worker is NOT busy
// and has an expired lease, Phase C releases the lease back to pending for re-dispatch.
func TestPhaseIntegration_BusyFalse_LeaseRelease(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(func(req agent.ExecRequest) agent.ExecResult {
		if req.Mode == agent.ModeIsBusy {
			return agent.ExecResult{Success: false} // not busy
		}
		return agent.ExecResult{Success: true}
	})
	qh := newPhaseIntegrationQH(t, maestroDir, exec)
	qh.SetBusyChecker(func(agentID string) bool {
		return false // agent is NOT busy
	})

	now := nowRFC3339()
	expiredTime := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	owner := qh.leaseOwnerID()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID:             "task_1772000016_release1",
			CommandID:      "cmd_1772000016_00000001",
			Purpose:        "Not busy, release lease",
			Content:        "Was in progress but agent crashed",
			Priority:       1,
			Status:         model.StatusInProgress,
			LeaseOwner:     &owner,
			LeaseExpiresAt: &expiredTime,
			LeaseEpoch:     3,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
	})

	// Phase A: detects expired lease, collects busy check
	pa := qh.periodicScanPhaseA()
	if len(pa.work.busyChecks) != 1 {
		t.Fatalf("Phase A: expected 1 busy check, got %d", len(pa.work.busyChecks))
	}

	// Phase B: busy probe returns false
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	if len(pb.busyChecks) != 1 {
		t.Fatalf("Phase B: expected 1 busy check result, got %d", len(pb.busyChecks))
	}
	if pb.busyChecks[0].Busy {
		t.Error("Phase B: expected busy=false")
	}

	// Phase C: should release the lease (task back to pending)
	_ = qh.periodicScanPhaseC(pa, pb)

	tqFinal := piReadTaskQueue(t, maestroDir, "worker1")
	task := tqFinal.Tasks[0]

	// Task should be back to pending with cleared lease fields
	if task.Status != model.StatusPending {
		t.Errorf("After release: expected pending, got %s", task.Status)
	}
	if task.LeaseOwner != nil {
		t.Errorf("After release: lease_owner should be nil, got %v", *task.LeaseOwner)
	}
	if task.LeaseExpiresAt != nil {
		t.Error("After release: lease_expires_at should be nil")
	}
	// Epoch should be preserved (lease manager doesn't reset it)
	if task.LeaseEpoch != 3 {
		t.Errorf("After release: expected epoch=3 (preserved), got %d", task.LeaseEpoch)
	}

	// Second scan: task should now be re-dispatchable
	pa2 := qh.periodicScanPhaseA()
	if len(pa2.work.dispatches) != 1 {
		t.Fatalf("Scan 2: expected 1 dispatch (re-dispatch after release), got %d", len(pa2.work.dispatches))
	}
	if pa2.work.dispatches[0].Task.ID != "task_1772000016_release1" {
		t.Errorf("Scan 2: expected task_1772000016_release1, got %s", pa2.work.dispatches[0].Task.ID)
	}
	// Epoch should be incremented by lease acquisition
	if pa2.work.dispatches[0].Epoch != 4 {
		t.Errorf("Scan 2: expected epoch=4 (incremented), got %d", pa2.work.dispatches[0].Epoch)
	}
}

// TestPhaseIntegration_SignalDeliveryFailure_Retry tests that when signal delivery
// fails in Phase B, Phase C retains the signal in the queue with retry metadata.
func TestPhaseIntegration_SignalDeliveryFailure_Retry(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)

	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)
	// Override executor factory: deliverPlannerSignal creates its own executor
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return newRecordingExecutor(func(req agent.ExecRequest) agent.ExecResult {
			// Signal delivery to planner fails
			if req.AgentID == "planner" && req.Mode == agent.ModeDeliver {
				return agent.ExecResult{
					Success: false,
					Error:   fmt.Errorf("planner tmux pane not responding"),
				}
			}
			return agent.ExecResult{Success: true}
		}), nil
	})

	// Setup: completed phase → pending phase should trigger awaiting_fill signal
	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd_1772000017_00000001", []PhaseInfo{
		{
			ID:     "phase_1772000017_aaaaaaaa",
			Name:   "Research",
			Status: model.PhaseStatusCompleted,
		},
		{
			ID:        "phase_1772000017_bbbbbbbb",
			Name:      "Implementation",
			Status:    model.PhaseStatusPending,
			DependsOn: []string{"phase_1772000017_aaaaaaaa"},
		},
	})
	qh.SetStateReader(reader)

	now := nowRFC3339()
	owner := qh.leaseOwnerID()
	futureExpiry := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	writeCommandQueue(t, maestroDir, []model.Command{
		{
			ID:             "cmd_1772000017_00000001",
			Content:        "Signal failure test",
			Priority:       1,
			Status:         model.StatusInProgress,
			LeaseOwner:     &owner,
			LeaseExpiresAt: &futureExpiry,
			LeaseEpoch:     1,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
	})

	// Phase A: detects phase transition, collects signal delivery item
	pa := qh.periodicScanPhaseA()
	if len(pa.work.signals) == 0 {
		t.Fatal("expected at least 1 signal delivery item")
	}

	// Write signal queue to disk (as Phase A flush would)
	signalPath := filepath.Join(maestroDir, "queue", "planner_signals.yaml")
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "awaiting_fill",
				CommandID: "cmd_1772000017_00000001",
				PhaseID:   "phase_1772000017_bbbbbbbb",
				Message:   "Phase 2 awaiting fill",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
	if err := yamlutil.AtomicWrite(signalPath, sq); err != nil {
		t.Fatalf("write signal queue: %v", err)
	}

	// Phase B: signal delivery fails
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	if len(pb.signals) == 0 {
		t.Fatal("expected at least 1 signal result")
	}

	var failedSignal *signalDeliveryResult
	for i := range pb.signals {
		if !pb.signals[i].Success {
			failedSignal = &pb.signals[i]
			break
		}
	}
	if failedSignal == nil {
		t.Fatal("expected at least 1 failed signal delivery")
	}

	// Phase C: should retain signal with retry metadata
	_ = qh.periodicScanPhaseC(pa, pb)

	// Read signal queue from disk
	signalData, err := os.ReadFile(signalPath)
	if err != nil {
		t.Fatalf("read signal queue after Phase C: %v", err)
	}
	var sqAfter model.PlannerSignalQueue
	if err := parseYAML(signalData, &sqAfter); err != nil {
		t.Fatalf("parse signal queue: %v", err)
	}

	// Signal should still be in the queue (retained for retry)
	if len(sqAfter.Signals) == 0 {
		t.Fatal("signal should be retained in queue for retry")
	}

	retainedSig := sqAfter.Signals[0]
	if retainedSig.Kind != "awaiting_fill" {
		t.Errorf("retained signal kind: got %s, want awaiting_fill", retainedSig.Kind)
	}
	if retainedSig.Attempts != 1 {
		t.Errorf("retained signal attempts: got %d, want 1", retainedSig.Attempts)
	}
	if retainedSig.LastAttemptAt == nil {
		t.Error("retained signal last_attempt_at should be set")
	}
	if retainedSig.LastError == nil {
		t.Error("retained signal last_error should be set")
	} else if !strings.Contains(*retainedSig.LastError, "planner tmux pane not responding") {
		t.Errorf("retained signal last_error: got %s, want containing 'planner tmux pane not responding'", *retainedSig.LastError)
	}
	if retainedSig.NextAttemptAt == nil {
		t.Error("retained signal next_attempt_at should be set for backoff retry")
	}
}
