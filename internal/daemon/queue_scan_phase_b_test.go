package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/dispatch"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// mockDispatcher is a minimal QueueDispatcher for unit-testing stepDispatchWork.
// Each Dispatch* method returns an error looked up by item index.
type mockDispatcher struct {
	commandErrors      map[int]error // key: call index for DispatchCommand
	taskErrors         map[int]error // key: call index for DispatchTask
	notificationErrors map[int]error // key: call index for DispatchNotification
	commandCalls       int
	taskCalls          int
	notificationCalls  int
}

func (m *mockDispatcher) DispatchCommand(cmd *model.Command) error {
	idx := m.commandCalls
	m.commandCalls++
	if err, ok := m.commandErrors[idx]; ok {
		return err
	}
	return nil
}

func (m *mockDispatcher) DispatchTask(task *model.Task, workerID string) error {
	idx := m.taskCalls
	m.taskCalls++
	if err, ok := m.taskErrors[idx]; ok {
		return err
	}
	return nil
}

func (m *mockDispatcher) DispatchNotification(ntf *model.Notification) error {
	idx := m.notificationCalls
	m.notificationCalls++
	if err, ok := m.notificationErrors[idx]; ok {
		return err
	}
	return nil
}

func (m *mockDispatcher) SortPendingCommands(commands []model.Command) []int       { return nil }
func (m *mockDispatcher) SortPendingTasks(tasks []model.Task) []int                { return nil }
func (m *mockDispatcher) SortPendingNotifications(ntfs []model.Notification) []int { return nil }
func (m *mockDispatcher) SetEventBus(bus *events.Bus)                              {}
func (m *mockDispatcher) SetQualityGate(qg dispatch.GateChecker)                   {}
func (m *mockDispatcher) SetWorktreeManager(wm dispatch.WorktreeResolver)          {}

// newPhaseBTestQueueHandler creates a minimal QueueHandler with a captured log buffer.
func newPhaseBTestQueueHandler(t *testing.T, logBuf *bytes.Buffer) *QueueHandler {
	t.Helper()
	maestroDir := t.TempDir()
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
	}
	logger := log.New(logBuf, "", 0)
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), logger, LogLevelDebug)
	return qh
}

func TestTrackPartialDispatch_AllSucceeded(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)

	result := &phaseBResult{
		dispatches: []dispatchResult{
			{Item: dispatchItem{Kind: "command"}, Success: true},
			{Item: dispatchItem{Kind: "task"}, Success: true},
		},
	}

	qh.trackPartialDispatch(result)

	if len(result.recoveryHints) != 0 {
		t.Errorf("expected 0 recovery hints when all succeeded, got %d: %v", len(result.recoveryHints), result.recoveryHints)
	}
	if strings.Contains(logBuf.String(), "phase_b_partial_dispatch") {
		t.Errorf("expected no partial dispatch log when all succeeded, got: %s", logBuf.String())
	}
}

func TestTrackPartialDispatch_AllFailed(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)

	result := &phaseBResult{
		dispatches: []dispatchResult{
			{Item: dispatchItem{Kind: "command"}, Success: false, Error: fmt.Errorf("err1")},
			{Item: dispatchItem{Kind: "task"}, Success: false, Error: fmt.Errorf("err2")},
		},
	}

	qh.trackPartialDispatch(result)

	if len(result.recoveryHints) != 0 {
		t.Errorf("expected 0 recovery hints when all failed (not partial), got %d: %v", len(result.recoveryHints), result.recoveryHints)
	}
	if strings.Contains(logBuf.String(), "phase_b_partial_dispatch") {
		t.Errorf("expected no partial dispatch log when all failed, got: %s", logBuf.String())
	}
}

func TestTrackPartialDispatch_Empty(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)

	result := &phaseBResult{}
	qh.trackPartialDispatch(result)

	if len(result.recoveryHints) != 0 {
		t.Errorf("expected 0 recovery hints for empty dispatches, got %d", len(result.recoveryHints))
	}
}

func TestTrackPartialDispatch_Partial(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)

	result := &phaseBResult{
		dispatches: []dispatchResult{
			{Item: dispatchItem{Kind: "command"}, Success: true},
			{Item: dispatchItem{Kind: "command"}, Success: false, Error: fmt.Errorf("cmd fail")},
			{Item: dispatchItem{Kind: "task"}, Success: true},
			{Item: dispatchItem{Kind: "task"}, Success: true},
			{Item: dispatchItem{Kind: "task"}, Success: false, Error: fmt.Errorf("task fail")},
			{Item: dispatchItem{Kind: "notification"}, Success: true},
		},
	}

	qh.trackPartialDispatch(result)

	// Must have exactly 1 recovery hint
	if len(result.recoveryHints) != 1 {
		t.Fatalf("expected 1 recovery hint, got %d: %v", len(result.recoveryHints), result.recoveryHints)
	}

	hint := result.recoveryHints[0]

	// Verify hint contains totals
	if !strings.Contains(hint, "total=6") {
		t.Errorf("hint missing total=6: %s", hint)
	}
	if !strings.Contains(hint, "succeeded=4") {
		t.Errorf("hint missing succeeded=4: %s", hint)
	}
	if !strings.Contains(hint, "failed=2") {
		t.Errorf("hint missing failed=2: %s", hint)
	}

	// Verify hint contains retry note
	if !strings.Contains(hint, "failed dispatches will be retried in the next scan cycle") {
		t.Errorf("hint missing retry note: %s", hint)
	}

	// Verify per-kind counts are present in hint
	if !strings.Contains(hint, "command_ok=1") || !strings.Contains(hint, "command_fail=1") {
		t.Errorf("hint missing command counts: %s", hint)
	}
	if !strings.Contains(hint, "task_ok=2") || !strings.Contains(hint, "task_fail=1") {
		t.Errorf("hint missing task counts: %s", hint)
	}

	// Verify logging
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "phase_b_partial_dispatch: total=6 succeeded=4 failed=2") {
		t.Errorf("log missing summary line: %s", logOutput)
	}
	if !strings.Contains(logOutput, "phase_b_partial_dispatch_detail: kind=command succeeded=1 failed=1") {
		t.Errorf("log missing command detail: %s", logOutput)
	}
	if !strings.Contains(logOutput, "phase_b_partial_dispatch_detail: kind=task succeeded=2 failed=1") {
		t.Errorf("log missing task detail: %s", logOutput)
	}
	// notification had 0 failures, should NOT appear in detail log
	if strings.Contains(logOutput, "kind=notification") {
		t.Errorf("log should not contain notification detail when notification had no failures: %s", logOutput)
	}
}

// --- workerCommitMessage tests ---

func TestWorkerCommitMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		purposes map[string]string
		workerID string
		want     string
	}{
		{
			name:     "with_purpose",
			purposes: map[string]string{"w1": "implement auth"},
			workerID: "w1",
			want:     "implement auth",
		},
		{
			name:     "empty_purpose_fallback",
			purposes: map[string]string{"w1": ""},
			workerID: "w1",
			want:     autoCommitFallbackMessage,
		},
		{
			name:     "missing_worker_fallback",
			purposes: map[string]string{"w2": "other"},
			workerID: "w1",
			want:     autoCommitFallbackMessage,
		},
		{
			name:     "nil_map_fallback",
			purposes: nil,
			workerID: "w1",
			want:     autoCommitFallbackMessage,
		},
		{
			name:     "truncated_at_72_chars",
			purposes: map[string]string{"w1": "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "bbb"},
			workerID: "w1",
			want:     "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "bb",
		},
		{
			name:     "exactly_72_chars",
			purposes: map[string]string{"w1": "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aa"},
			workerID: "w1",
			want:     "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aaaaaaaaaa" + "aa",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := workerCommitMessage(tt.purposes, tt.workerID)
			if got != tt.want {
				t.Errorf("workerCommitMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- stepLogPartialFailures tests ---

func TestStepLogPartialFailures_NoDispatchesOrSignals(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)

	result := &phaseBResult{}
	qh.stepLogPartialFailures(result)

	if strings.Contains(logBuf.String(), "phase_b_partial_failures") {
		t.Error("expected no log for empty results")
	}
}

func TestStepLogPartialFailures_AllSuccess(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)

	result := &phaseBResult{
		dispatches: []dispatchResult{
			{Success: true},
			{Success: true},
		},
		signals: []signalDeliveryResult{
			{Success: true},
		},
	}
	qh.stepLogPartialFailures(result)

	if strings.Contains(logBuf.String(), "phase_b_partial_failures") {
		t.Error("expected no log when all succeed")
	}
}

func TestStepLogPartialFailures_WithFailures(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)

	result := &phaseBResult{
		dispatches: []dispatchResult{
			{Success: true},
			{Success: false, Error: fmt.Errorf("fail")},
		},
		signals: []signalDeliveryResult{
			{Success: false, Error: fmt.Errorf("sig fail")},
		},
	}
	qh.stepLogPartialFailures(result)

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "phase_b_partial_failures") {
		t.Errorf("expected partial_failures log, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "dispatches_failed=1/2") {
		t.Errorf("expected dispatches_failed=1/2, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "signals_failed=1/1") {
		t.Errorf("expected signals_failed=1/1, got: %s", logOutput)
	}
}

// --- stepProbeBusyAgents tests ---

func TestStepProbeBusyAgents_CollectsResults(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)
	qh.scanExecutor.busyChecker = BusyCheckerFunc(func(agentID string) bool {
		return agentID == "worker1"
	})

	pa := &phaseAResult{
		work: deferredWork{
			busyChecks: []busyCheckItem{
				{Kind: "task", EntryID: "t1", AgentID: "worker1"},
				{Kind: "task", EntryID: "t2", AgentID: "worker2"},
			},
		},
	}
	result := &phaseBResult{}

	qh.stepProbeBusyAgents(context.Background(), pa, result)

	if len(result.busyChecks) != 2 {
		t.Fatalf("expected 2 busy check results, got %d", len(result.busyChecks))
	}
	if !result.busyChecks[0].Busy {
		t.Error("expected worker1 to be busy")
	}
	if result.busyChecks[1].Busy {
		t.Error("expected worker2 to not be busy")
	}
}

func TestStepProbeBusyAgents_ContextCancelled(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)
	qh.scanExecutor.busyChecker = BusyCheckerFunc(func(agentID string) bool {
		return true
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	pa := &phaseAResult{
		work: deferredWork{
			busyChecks: []busyCheckItem{
				{Kind: "task", EntryID: "t1", AgentID: "worker1"},
				{Kind: "task", EntryID: "t2", AgentID: "worker2"},
				{Kind: "task", EntryID: "t3", AgentID: "worker3"},
			},
		},
	}
	result := &phaseBResult{}

	qh.stepProbeBusyAgents(ctx, pa, result)

	// With cancelled context, fewer results should have been collected
	if len(result.busyChecks) > len(pa.work.busyChecks) {
		t.Errorf("results (%d) should not exceed items (%d)", len(result.busyChecks), len(pa.work.busyChecks))
	}
}

func TestStepDispatchWork_PartialDispatch_Integration(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)

	// Replace dispatcher with mock that fails the second task dispatch
	mock := &mockDispatcher{
		taskErrors: map[int]error{
			1: fmt.Errorf("agent unavailable"),
		},
	}
	qh.dispatcher = mock

	pa := &phaseAResult{
		work: deferredWork{
			dispatches: []dispatchItem{
				{Kind: "task", Task: &model.Task{ID: "t1", CommandID: "cmd1"}, WorkerID: "w1"},
				{Kind: "task", Task: &model.Task{ID: "t2", CommandID: "cmd1"}, WorkerID: "w2"},
				{Kind: "command", Command: &model.Command{ID: "cmd1"}},
			},
		},
	}
	result := &phaseBResult{
		dispatches: make([]dispatchResult, 0, 3),
	}

	qh.stepDispatchWork(context.Background(), pa, result)

	// Verify dispatch results: t1 ok, t2 fail, cmd1 ok
	if len(result.dispatches) != 3 {
		t.Fatalf("expected 3 dispatch results, got %d", len(result.dispatches))
	}
	if !result.dispatches[0].Success {
		t.Errorf("dispatch[0] (task t1) should have succeeded")
	}
	if result.dispatches[1].Success {
		t.Errorf("dispatch[1] (task t2) should have failed")
	}
	if !result.dispatches[2].Success {
		t.Errorf("dispatch[2] (command cmd1) should have succeeded")
	}

	// Verify partial dispatch was detected
	if len(result.recoveryHints) != 1 {
		t.Fatalf("expected 1 recovery hint, got %d: %v", len(result.recoveryHints), result.recoveryHints)
	}
	hint := result.recoveryHints[0]
	if !strings.Contains(hint, "partial_dispatch") {
		t.Errorf("hint should contain 'partial_dispatch': %s", hint)
	}
	if !strings.Contains(hint, "total=3") {
		t.Errorf("hint should contain total=3: %s", hint)
	}
	if !strings.Contains(hint, "succeeded=2") {
		t.Errorf("hint should contain succeeded=2: %s", hint)
	}
	if !strings.Contains(hint, "failed=1") {
		t.Errorf("hint should contain failed=1: %s", hint)
	}

	// Verify warning was logged
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "phase_b_partial_dispatch:") {
		t.Errorf("expected partial dispatch warning in log: %s", logOutput)
	}
}
