package daemon

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// mockExecutor implements AgentExecutor for testing.
type mockExecutor struct {
	calls  []agent.ExecRequest
	result agent.ExecResult
}

func (m *mockExecutor) Execute(req agent.ExecRequest) agent.ExecResult {
	m.calls = append(m.calls, req)
	return m.result
}

func (m *mockExecutor) Close() error { return nil }

func newTestCancelHandler() (*CancelHandler, *mockExecutor) {
	ch := NewCancelHandler("", model.Config{}, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	mock := &mockExecutor{result: agent.ExecResult{Success: true}}
	ch.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return mock, nil
	})
	return ch, mock
}

func TestIsCommandCancelRequested(t *testing.T) {
	ch, _ := newTestCancelHandler()

	// Not cancelled
	cmd := &model.Command{ID: "cmd1"}
	if ch.IsCommandCancelRequested(cmd) {
		t.Error("expected not cancelled")
	}

	// Cancelled
	cancelTime := time.Now().Format(time.RFC3339)
	cmd.CancelRequestedAt = &cancelTime
	if !ch.IsCommandCancelRequested(cmd) {
		t.Error("expected cancelled")
	}
}

func TestCancelPendingTasks(t *testing.T) {
	ch, _ := newTestCancelHandler()

	tasks := []model.Task{
		{ID: "t1", CommandID: "cmd1", Status: model.StatusPending, CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "t2", CommandID: "cmd1", Status: model.StatusInProgress, CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "t3", CommandID: "cmd2", Status: model.StatusPending, CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "t4", CommandID: "cmd1", Status: model.StatusPending, CreatedAt: time.Now().Format(time.RFC3339)},
	}

	results := ch.CancelPendingTasks(tasks, "cmd1")

	if len(results) != 2 {
		t.Fatalf("expected 2 cancelled, got %d", len(results))
	}

	// t1 and t4 should be cancelled
	if tasks[0].Status != model.StatusCancelled {
		t.Errorf("t1: got %s, want cancelled", tasks[0].Status)
	}
	if tasks[1].Status != model.StatusInProgress {
		t.Errorf("t2: got %s, want in_progress (not pending)", tasks[1].Status)
	}
	if tasks[2].Status != model.StatusPending {
		t.Errorf("t3: got %s, want pending (different command)", tasks[2].Status)
	}
	if tasks[3].Status != model.StatusCancelled {
		t.Errorf("t4: got %s, want cancelled", tasks[3].Status)
	}
}

func TestInterruptInProgressTasks(t *testing.T) {
	ch, mock := newTestCancelHandler()

	w := "worker1"
	tasks := []model.Task{
		{ID: "t1", CommandID: "cmd1", Status: model.StatusInProgress, LeaseOwner: &w, LeaseEpoch: 3,
			CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "t2", CommandID: "cmd1", Status: model.StatusPending,
			CreatedAt: time.Now().Format(time.RFC3339)},
	}

	results := ch.InterruptInProgressTasks(tasks, "cmd1")

	if len(results) != 1 {
		t.Fatalf("expected 1 interrupted, got %d", len(results))
	}

	if tasks[0].Status != model.StatusCancelled {
		t.Errorf("t1: got %s, want cancelled", tasks[0].Status)
	}
	if tasks[0].LeaseOwner != nil {
		t.Error("lease_owner should be nil after cancel")
	}

	// Verify interrupt was sent
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 interrupt call, got %d", len(mock.calls))
	}
	if mock.calls[0].Mode != agent.ModeInterrupt {
		t.Errorf("mode: got %s, want interrupt", mock.calls[0].Mode)
	}
	if mock.calls[0].AgentID != "worker1" {
		t.Errorf("agent_id: got %s, want worker1", mock.calls[0].AgentID)
	}
}

func TestBuildSyntheticResult(t *testing.T) {
	ch, _ := newTestCancelHandler()

	result := ch.BuildSyntheticResult(CancelledTaskResult{
		TaskID:    "t1",
		CommandID: "cmd1",
		Status:    "cancelled",
		Reason:    "command_cancelled",
	})

	if result["task_id"] != "t1" {
		t.Errorf("task_id: got %v", result["task_id"])
	}
	if result["synthetic"] != true {
		t.Errorf("synthetic: got %v", result["synthetic"])
	}
}

func TestCancelPendingTasks_AlreadyTerminal(t *testing.T) {
	ch, _ := newTestCancelHandler()

	tasks := []model.Task{
		{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted,
			CreatedAt: time.Now().Format(time.RFC3339)},
	}

	results := ch.CancelPendingTasks(tasks, "cmd1")
	if len(results) != 0 {
		t.Errorf("expected 0 cancelled (already terminal), got %d", len(results))
	}
}

// newTestCancelHandlerWithDir creates a CancelHandler backed by a real temp directory.
func newTestCancelHandlerWithDir(t *testing.T) (*CancelHandler, *mockExecutor, string) {
	t.Helper()
	maestroDir := filepath.Join(t.TempDir(), ".maestro")
	for _, sub := range []string{"results"} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatalf("create dir %s: %v", sub, err)
		}
	}
	ch := NewCancelHandler(maestroDir, model.Config{}, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	mock := &mockExecutor{result: agent.ExecResult{Success: true}}
	ch.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return mock, nil
	})
	return ch, mock, maestroDir
}

// stubStateReader implements StateReader for cancel_handler tests.
type stubStateReader struct {
	cancelRequested    bool
	cancelRequestedErr error
	updateTaskCalls    []stubUpdateTaskCall
	updateTaskErr      error
}

type stubUpdateTaskCall struct {
	CommandID       string
	TaskID          string
	NewStatus       model.Status
	CancelledReason string
}

func (s *stubStateReader) GetTaskState(string, string) (model.Status, error) {
	return "", ErrStateNotFound
}
func (s *stubStateReader) GetCommandPhases(string) ([]PhaseInfo, error) {
	return nil, ErrStateNotFound
}
func (s *stubStateReader) GetTaskDependencies(string, string) ([]string, error) {
	return nil, ErrStateNotFound
}
func (s *stubStateReader) IsSystemCommitReady(string, string) (bool, bool, error) {
	return false, false, ErrStateNotFound
}
func (s *stubStateReader) ApplyPhaseTransition(string, string, model.PhaseStatus) error {
	return ErrStateNotFound
}
func (s *stubStateReader) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	s.updateTaskCalls = append(s.updateTaskCalls, stubUpdateTaskCall{commandID, taskID, newStatus, cancelledReason})
	return s.updateTaskErr
}
func (s *stubStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	return s.cancelRequested, s.cancelRequestedErr
}
func (s *stubStateReader) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	return &model.CircuitBreakerState{}, nil
}
func (s *stubStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	return nil
}

func TestCancelHandler_WriteSyntheticResults_NewFile(t *testing.T) {
	ch, _, maestroDir := newTestCancelHandlerWithDir(t)

	results := []CancelledTaskResult{
		{TaskID: "t1", CommandID: "cmd1", Status: "cancelled", Reason: "command_cancel_requested"},
		{TaskID: "t2", CommandID: "cmd1", Status: "cancelled", Reason: "command_cancel_requested"},
	}

	ch.WriteSyntheticResults(results, "worker1")

	// Verify file was created with correct contents
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("unmarshal result file: %v", err)
	}
	if rf.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", rf.SchemaVersion)
	}
	if rf.FileType != "result_task" {
		t.Errorf("file_type = %q, want %q", rf.FileType, "result_task")
	}
	if len(rf.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(rf.Results))
	}
	for i, r := range rf.Results {
		if r.ID == "" {
			t.Errorf("result[%d]: expected non-empty ID", i)
		}
		if r.Status != model.StatusCancelled {
			t.Errorf("result[%d]: status = %q, want %q", i, r.Status, model.StatusCancelled)
		}
		if !r.PartialChangesPossible {
			t.Errorf("result[%d]: partial_changes_possible should be true", i)
		}
		if r.RetrySafe {
			t.Errorf("result[%d]: retry_safe should be false", i)
		}
	}
	if rf.Results[0].TaskID != "t1" {
		t.Errorf("result[0].task_id = %q, want %q", rf.Results[0].TaskID, "t1")
	}
	if rf.Results[1].TaskID != "t2" {
		t.Errorf("result[1].task_id = %q, want %q", rf.Results[1].TaskID, "t2")
	}
}

func TestCancelHandler_WriteSyntheticResults_AppendExisting(t *testing.T) {
	ch, _, maestroDir := newTestCancelHandlerWithDir(t)

	// Pre-populate with an existing result
	existingRF := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "result_existing",
				TaskID:    "t0",
				CommandID: "cmd0",
				Status:    model.StatusCompleted,
				Summary:   "already done",
				CreatedAt: "2026-01-01T00:00:00Z",
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	existingData, err := yamlv3.Marshal(existingRF)
	if err != nil {
		t.Fatalf("marshal existing: %v", err)
	}
	if err := os.WriteFile(resultPath, existingData, 0644); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	// Append a synthetic result
	ch.WriteSyntheticResults([]CancelledTaskResult{
		{TaskID: "t1", CommandID: "cmd1", Status: "cancelled", Reason: "command_cancel_requested"},
	}, "worker1")

	// Verify both exist
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rf.Results) != 2 {
		t.Fatalf("expected 2 results (1 existing + 1 synthetic), got %d", len(rf.Results))
	}
	if rf.Results[0].TaskID != "t0" {
		t.Errorf("result[0].task_id = %q, want existing %q", rf.Results[0].TaskID, "t0")
	}
	if rf.Results[1].TaskID != "t1" {
		t.Errorf("result[1].task_id = %q, want synthetic %q", rf.Results[1].TaskID, "t1")
	}
}

func TestCancelHandler_WriteSyntheticResults_EmptyInput(t *testing.T) {
	ch, _, maestroDir := newTestCancelHandlerWithDir(t)

	// Empty input → no-op, file should not be created
	ch.WriteSyntheticResults(nil, "worker1")

	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	if _, err := os.Stat(resultPath); err == nil {
		t.Error("expected no file created for empty input")
	}

	ch.WriteSyntheticResults([]CancelledTaskResult{}, "worker1")
	if _, err := os.Stat(resultPath); err == nil {
		t.Error("expected no file created for empty slice")
	}
}

func TestCancelHandler_InterruptInProgressTasksDeferred_Basic(t *testing.T) {
	ch, _, _ := newTestCancelHandlerWithDir(t)
	sr := &stubStateReader{}
	ch.SetStateReader(sr)

	w := "worker1"
	exp := "2026-01-01T01:00:00Z"
	tasks := []model.Task{
		{ID: "t1", CommandID: "cmd1", Status: model.StatusInProgress, LeaseOwner: &w, LeaseEpoch: 5, LeaseExpiresAt: &exp,
			CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "t2", CommandID: "cmd1", Status: model.StatusPending,
			CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "t3", CommandID: "cmd2", Status: model.StatusInProgress, LeaseOwner: &w, LeaseEpoch: 2,
			CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "t4", CommandID: "cmd1", Status: model.StatusInProgress, LeaseOwner: nil, LeaseEpoch: 1,
			CreatedAt: time.Now().Format(time.RFC3339)},
	}

	results, interrupts := ch.InterruptInProgressTasksDeferred(tasks, "cmd1", "worker1")

	// Only t1 and t4 are in_progress for cmd1; t2 is pending, t3 is different command
	if len(results) != 2 {
		t.Fatalf("expected 2 cancelled results, got %d", len(results))
	}
	if results[0].TaskID != "t1" {
		t.Errorf("results[0].task_id = %q, want %q", results[0].TaskID, "t1")
	}
	if results[1].TaskID != "t4" {
		t.Errorf("results[1].task_id = %q, want %q", results[1].TaskID, "t4")
	}

	// Only t1 has a LeaseOwner, so only 1 interrupt item
	if len(interrupts) != 1 {
		t.Fatalf("expected 1 interrupt item, got %d", len(interrupts))
	}
	if interrupts[0].TaskID != "t1" {
		t.Errorf("interrupt[0].task_id = %q, want %q", interrupts[0].TaskID, "t1")
	}
	if interrupts[0].Epoch != 5 {
		t.Errorf("interrupt[0].epoch = %d, want 5", interrupts[0].Epoch)
	}

	// Verify in-memory state mutation
	if tasks[0].Status != model.StatusCancelled {
		t.Errorf("t1 status = %q, want cancelled", tasks[0].Status)
	}
	if tasks[0].LeaseOwner != nil {
		t.Error("t1 lease_owner should be nil after deferred cancel")
	}
	if tasks[0].LeaseExpiresAt != nil {
		t.Error("t1 lease_expires_at should be nil after deferred cancel")
	}
	if tasks[3].Status != model.StatusCancelled {
		t.Errorf("t4 status = %q, want cancelled", tasks[3].Status)
	}

	// t2 (pending) should be untouched
	if tasks[1].Status != model.StatusPending {
		t.Errorf("t2 status = %q, want pending", tasks[1].Status)
	}

	// stateReader.UpdateTaskState should have been called for t1 and t4
	if len(sr.updateTaskCalls) != 2 {
		t.Fatalf("expected 2 UpdateTaskState calls, got %d", len(sr.updateTaskCalls))
	}
}

func TestCancelHandler_IsCommandCancelRequested_ViaStateReader(t *testing.T) {
	ch, _ := newTestCancelHandler()
	cmd := &model.Command{ID: "cmd1"}

	// Case 1: stateReader returns true → should be true
	sr := &stubStateReader{cancelRequested: true}
	ch.SetStateReader(sr)
	if !ch.IsCommandCancelRequested(cmd) {
		t.Error("expected cancelled via stateReader returning true")
	}

	// Case 2: stateReader returns false → should be false even if queue metadata says cancelled
	cancelTime := time.Now().Format(time.RFC3339)
	cmd.CancelRequestedAt = &cancelTime
	sr.cancelRequested = false
	if ch.IsCommandCancelRequested(cmd) {
		t.Error("expected not cancelled: stateReader returned false, should override queue metadata")
	}

	// Case 3: stateReader returns ErrStateNotFound → fall back to queue metadata
	sr.cancelRequestedErr = ErrStateNotFound
	if !ch.IsCommandCancelRequested(cmd) {
		t.Error("expected cancelled: stateReader returned ErrStateNotFound, should fall back to queue metadata")
	}

	// Case 4: stateReader returns ErrStateNotFound, no queue metadata → not cancelled
	cmd.CancelRequestedAt = nil
	if ch.IsCommandCancelRequested(cmd) {
		t.Error("expected not cancelled: stateReader returned ErrStateNotFound and no queue metadata")
	}
}

func TestCancelHandler_InterruptInProgressTasks_WorktreeCleanup(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create worktrees
	if err := wm.CreateForCommand("cmd_cancel_wt", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	// Make worker1 worktree dirty
	wtPath, _ := wm.GetWorkerPath("cmd_cancel_wt", "worker1")
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ch, _ := newTestCancelHandler()
	ch.SetWorktreeManager(wm)

	w := "worker1"
	tasks := []model.Task{
		{ID: "t1", CommandID: "cmd_cancel_wt", Status: model.StatusInProgress, LeaseOwner: &w, LeaseEpoch: 1,
			CreatedAt: time.Now().Format(time.RFC3339)},
	}

	results := ch.InterruptInProgressTasks(tasks, "cmd_cancel_wt")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Verify worktree is clean after cancel (DiscardWorkerChanges was called)
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, _ := cmd.Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("worktree should be clean after cancel, got: %s", out)
	}
}

func TestCancelHandler_InterruptInProgressTasksDeferred_WorktreeCleanup(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_cancel_def", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	// Make dirty
	wtPath, _ := wm.GetWorkerPath("cmd_cancel_def", "worker1")
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ch, _, _ := newTestCancelHandlerWithDir(t)
	ch.SetWorktreeManager(wm)

	w := "worker1"
	tasks := []model.Task{
		{ID: "t1", CommandID: "cmd_cancel_def", Status: model.StatusInProgress, LeaseOwner: &w, LeaseEpoch: 1,
			CreatedAt: time.Now().Format(time.RFC3339)},
	}

	results, _ := ch.InterruptInProgressTasksDeferred(tasks, "cmd_cancel_def", "worker1")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Verify worktree is clean
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, _ := cmd.Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("worktree should be clean after deferred cancel, got: %s", out)
	}
}

func TestCancelHandler_CleanupCommandWorktrees(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_cancel_cleanup", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	if !wm.HasWorktrees("cmd_cancel_cleanup") {
		t.Fatal("worktrees should exist before cleanup")
	}

	ch, _ := newTestCancelHandler()
	ch.SetWorktreeManager(wm)

	ch.CleanupCommandWorktrees("cmd_cancel_cleanup")

	if wm.HasWorktrees("cmd_cancel_cleanup") {
		t.Error("worktrees should be cleaned up after CleanupCommandWorktrees")
	}
}

func TestCancelHandler_CleanupCommandWorktrees_NilManager(t *testing.T) {
	ch, _ := newTestCancelHandler()
	// worktreeManager is nil — should not panic
	ch.CleanupCommandWorktrees("any_command")
}

func TestCancelHandler_SetWorktreeManager(t *testing.T) {
	ch, _ := newTestCancelHandler()
	if ch.worktreeManager != nil {
		t.Error("worktreeManager should be nil initially")
	}

	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	ch.SetWorktreeManager(wm)

	if ch.worktreeManager != wm {
		t.Error("worktreeManager should be set after SetWorktreeManager")
	}
}
