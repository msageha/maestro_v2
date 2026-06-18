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
	"github.com/msageha/maestro_v2/internal/testutil/mocks"
)

func newTestCancelHandler() (*CancelHandler, *mocks.MockExecutor) {
	cfg := model.Config{}
	ep := newTestExecutorProvider("", cfg)
	mock := &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}
	ep.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return mock, nil
	})
	ch := NewCancelHandler("", cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug, ep)
	return ch, mock
}

func TestIsCommandCancelRequested(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestCancelPendingTasks_AlreadyTerminal(t *testing.T) {
	t.Parallel()
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
func newTestCancelHandlerWithDir(t *testing.T) (*CancelHandler, *mocks.MockExecutor, string) {
	t.Helper()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{}
	ep := newTestExecutorProvider(maestroDir, cfg)
	mock := &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}
	ep.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return mock, nil
	})
	ch := NewCancelHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug, ep)
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
func (s *stubStateReader) GetEffectiveTaskStatus(string, string) (model.Status, error) {
	return "", ErrStateNotFound
}
func (s *stubStateReader) GetEffectiveTaskStatusForCompletion(string, string) (model.Status, error) {
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
func (s *stubStateReader) SetPhaseCancelledReason(string, string, *string) error {
	return nil
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
func (s *stubStateReader) HasNonTerminalTaskState(commandID string) (bool, error) {
	return false, nil
}
func (s *stubStateReader) GetNonTerminalTaskStates(commandID string) (map[string]model.Status, error) {
	return nil, nil
}
func (s *stubStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	return nil
}
func (s *stubStateReader) MarkAwaitingFillStallNotified(string, string, string) error { return nil }
func (s *stubStateReader) MarkCircuitBreakerProgress(string) error                    { return nil }

func TestCancelHandler_WriteSyntheticResults_NewFile(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestCancelHandler_CollectCancelInterruptItems_Basic(t *testing.T) {
	t.Parallel()
	ch, _, _ := newTestCancelHandlerWithDir(t)
	sr := &stubStateReader{}
	ch.SetStateReader(sr)

	// Use daemon:{pid} format for LeaseOwner (production format),
	// while workerID param is the agent ID — verifies we use workerID, not LeaseOwner.
	lease := "daemon:12345"
	exp := "2026-01-01T01:00:00Z"
	tasks := []model.Task{
		{ID: "t1", CommandID: "cmd1", Status: model.StatusInProgress, LeaseOwner: &lease, LeaseEpoch: 5, LeaseExpiresAt: &exp,
			CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "t2", CommandID: "cmd1", Status: model.StatusPending,
			CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "t3", CommandID: "cmd2", Status: model.StatusInProgress, LeaseOwner: &lease, LeaseEpoch: 2,
			CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "t4", CommandID: "cmd1", Status: model.StatusInProgress, LeaseOwner: nil, LeaseEpoch: 1,
			CreatedAt: time.Now().Format(time.RFC3339)},
	}

	marks, interrupts := ch.CollectCancelInterruptItems(tasks, "cmd1", "worker1")

	// Only t1 and t4 are in_progress for cmd1; t2 is pending, t3 is different command
	if len(marks) != 2 {
		t.Fatalf("expected 2 cancel marks, got %d", len(marks))
	}
	if marks[0].TaskID != "t1" || marks[1].TaskID != "t4" {
		t.Errorf("marks task ids = [%q, %q], want [t1, t4]", marks[0].TaskID, marks[1].TaskID)
	}
	if marks[0].LeaseEpoch != 5 {
		t.Errorf("marks[0].lease_epoch = %d, want 5", marks[0].LeaseEpoch)
	}

	// Only t1 has a LeaseOwner, so only 1 interrupt item
	if len(interrupts) != 1 {
		t.Fatalf("expected 1 interrupt item, got %d", len(interrupts))
	}
	if interrupts[0].TaskID != "t1" || interrupts[0].Epoch != 5 || interrupts[0].WorkerID != "worker1" {
		t.Errorf("interrupt[0] = %+v", interrupts[0])
	}

	// CollectCancelInterruptItems must NOT mutate task state — Phase C performs
	// the cancellation under scanMu so workers racing to completion are not
	// clobbered.
	for i, want := range []model.Status{
		model.StatusInProgress, model.StatusPending,
		model.StatusInProgress, model.StatusInProgress,
	} {
		if tasks[i].Status != want {
			t.Errorf("tasks[%d].status = %q, want %q (collection must not mutate)", i, tasks[i].Status, want)
		}
	}
	if tasks[0].LeaseOwner == nil {
		t.Error("t1 lease_owner should be preserved during collection")
	}

	// stateReader.UpdateTaskState must NOT be called during collection.
	if len(sr.updateTaskCalls) != 0 {
		t.Fatalf("expected 0 UpdateTaskState calls during collection, got %d", len(sr.updateTaskCalls))
	}
}

func TestCancelHandler_ApplyCancelMark(t *testing.T) {
	t.Parallel()
	ch, _, _ := newTestCancelHandlerWithDir(t)
	sr := &stubStateReader{}
	ch.SetStateReader(sr)

	lease := "daemon:1"
	exp := "2026-01-01T01:00:00Z"
	task := model.Task{
		ID: "t1", CommandID: "cmd1", Status: model.StatusInProgress,
		LeaseOwner: &lease, LeaseExpiresAt: &exp, LeaseEpoch: 7,
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	res, applied := ch.ApplyCancelMark(&task, 7)
	if !applied {
		t.Fatal("expected ApplyCancelMark to apply on matching epoch")
	}
	if task.Status != model.StatusCancelled || task.LeaseOwner != nil || task.LeaseExpiresAt != nil {
		t.Errorf("task not transitioned correctly: %+v", task)
	}
	if res.TaskID != "t1" || res.Reason != "command_cancel_requested" {
		t.Errorf("unexpected result: %+v", res)
	}
	if len(sr.updateTaskCalls) != 1 {
		t.Errorf("expected 1 UpdateTaskState call, got %d", len(sr.updateTaskCalls))
	}

	// Race: worker already completed (terminal status). Apply must be a no-op.
	completed := model.Task{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted, LeaseEpoch: 3}
	if _, applied := ch.ApplyCancelMark(&completed, 3); applied {
		t.Error("expected ApplyCancelMark to be no-op on terminal task")
	}
	if completed.Status != model.StatusCompleted {
		t.Errorf("terminal task mutated: %s", completed.Status)
	}

	// Race: lease epoch advanced (re-dispatched to a new worker generation).
	stale := model.Task{ID: "t3", CommandID: "cmd1", Status: model.StatusInProgress, LeaseEpoch: 9}
	if _, applied := ch.ApplyCancelMark(&stale, 7); applied {
		t.Error("expected ApplyCancelMark to be no-op on epoch mismatch")
	}
	if stale.Status != model.StatusInProgress {
		t.Errorf("stale task mutated: %s", stale.Status)
	}
}

func TestCancelHandler_IsCommandCancelRequested_ViaStateReader(t *testing.T) {
	t.Parallel()
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

func TestCancelHandler_CollectCancelInterruptItems_DoesNotTouchWorktree(t *testing.T) {
	t.Parallel()
	// H4: DiscardWorkerChanges has been moved out of CancelHandler into
	// Phase B (queue_scan_phase_b.go), executed AFTER the tmux interrupt
	// completes. Collection in Phase A must not touch the worktree.
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.EnsureWorkerWorktree("cmd_cancel_def", "worker1"); err != nil {
		t.Fatal(err)
	}
	wtPath, _ := wm.GetWorkerPath("cmd_cancel_def", "worker1")
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ch, _, _ := newTestCancelHandlerWithDir(t)
	ch.SetWorktreeManager(wm)

	lease := "daemon:99999"
	tasks := []model.Task{
		{ID: "t1", CommandID: "cmd_cancel_def", Status: model.StatusInProgress, LeaseOwner: &lease, LeaseEpoch: 1,
			CreatedAt: time.Now().Format(time.RFC3339)},
	}

	marks, interrupts := ch.CollectCancelInterruptItems(tasks, "cmd_cancel_def", "worker1")
	if len(marks) != 1 || len(interrupts) != 1 {
		t.Fatalf("expected 1 mark + 1 interrupt, got %d marks / %d interrupts", len(marks), len(interrupts))
	}

	// The worktree should STILL be dirty — Phase B is responsible for the
	// reset, not Phase A collection.
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, _ := cmd.Output()
	if strings.TrimSpace(string(out)) == "" {
		t.Error("worktree should still be dirty after Phase A collection (cleanup is now Phase B's job)")
	}
}

func TestCancelHandler_SetWorktreeManager(t *testing.T) {
	t.Parallel()
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

// --- Cancel cache tests (Fix 1 & 3: cancel-dispatch race) ---

func TestCancelCache_MarkAndCheck(t *testing.T) {
	t.Parallel()
	c := newCancelCache()

	if c.IsMarked("cmd1") {
		t.Error("expected cmd1 not marked initially")
	}

	c.Mark("cmd1")
	if !c.IsMarked("cmd1") {
		t.Error("expected cmd1 marked after Mark()")
	}
	if c.IsMarked("cmd2") {
		t.Error("expected cmd2 not marked")
	}

	c.Remove("cmd1")
	if c.IsMarked("cmd1") {
		t.Error("expected cmd1 not marked after Remove()")
	}
}

func TestCancelCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := newCancelCache()
	done := make(chan struct{})

	// Concurrent writers
	for i := 0; i < 10; i++ {
		go func(id int) {
			defer func() { done <- struct{}{} }()
			cmdID := "cmd_" + string(rune('A'+id))
			c.Mark(cmdID)
			_ = c.IsMarked(cmdID)
			c.Remove(cmdID)
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestCancelHandler_CacheCancelRequest(t *testing.T) {
	t.Parallel()
	ch, _ := newTestCancelHandler()

	if ch.IsDispatchBlocked("cmd1") {
		t.Error("expected not blocked initially")
	}

	ch.CacheCancelRequest("cmd1")
	if !ch.IsDispatchBlocked("cmd1") {
		t.Error("expected blocked after CacheCancelRequest")
	}

	ch.ClearCancelCache("cmd1")
	if ch.IsDispatchBlocked("cmd1") {
		t.Error("expected not blocked after ClearCancelCache")
	}
}

func TestCancelPendingTasks_UpdatesCache(t *testing.T) {
	t.Parallel()
	ch, _ := newTestCancelHandler()

	tasks := []model.Task{
		{ID: "task1", CommandID: "cmd1", Status: model.StatusPending},
	}
	ch.CancelPendingTasks(tasks, "cmd1")

	// After CancelPendingTasks, the cache should be marked
	if !ch.IsDispatchBlocked("cmd1") {
		t.Error("expected cache marked after CancelPendingTasks")
	}
}

func TestCollectCancelInterruptItems_UpdatesCache(t *testing.T) {
	t.Parallel()
	ch, _ := newTestCancelHandler()

	owner := "daemon:1234"
	tasks := []model.Task{
		{ID: "task1", CommandID: "cmd1", Status: model.StatusInProgress, LeaseOwner: &owner, LeaseEpoch: 1},
	}
	ch.CollectCancelInterruptItems(tasks, "cmd1", "worker1")

	if !ch.IsDispatchBlocked("cmd1") {
		t.Error("expected cache marked after CollectCancelInterruptItems")
	}
}
