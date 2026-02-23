package daemon

import (
	"bytes"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
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
	ch := NewCancelHandler("", model.Config{}, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
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
