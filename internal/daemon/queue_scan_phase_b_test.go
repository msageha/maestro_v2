package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/msageha/maestro_v2/internal/agent"
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

func (m *mockDispatcher) DispatchCommand(_ context.Context, cmd *model.Command) error {
	idx := m.commandCalls
	m.commandCalls++
	if err, ok := m.commandErrors[idx]; ok {
		return err
	}
	return nil
}

func (m *mockDispatcher) DispatchTask(_ context.Context, task *model.Task, workerID string, _ bool) error {
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
func (m *mockDispatcher) SetTaskAliveChecker(c dispatch.TaskAliveChecker)          {}
func (m *mockDispatcher) SetRepairHintProvider(f func(*model.Task) string)         {}

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
	if !strings.Contains(logOutput, "phase_b_partial_dispatch: total=6 succeeded=4 failed=2 uncertain=0") {
		t.Errorf("log missing summary line: %s", logOutput)
	}
	if !strings.Contains(logOutput, "phase_b_partial_dispatch_detail: kind=command succeeded=1 failed=1 uncertain=0") {
		t.Errorf("log missing command detail: %s", logOutput)
	}
	if !strings.Contains(logOutput, "phase_b_partial_dispatch_detail: kind=task succeeded=2 failed=1 uncertain=0") {
		t.Errorf("log missing task detail: %s", logOutput)
	}
	// notification had 0 failures, should NOT appear in detail log
	if strings.Contains(logOutput, "kind=notification") {
		t.Errorf("log should not contain notification detail when notification had no failures: %s", logOutput)
	}
}

// TestTrackPartialDispatch_UncertainOnlyDoesNotEscalate asserts that a
// dispatchResult with Success=false and Error wrapping
// ErrSubmitConfirmUncertain (worker pane received the paste but failed
// the submit-confirm probe) is bucketed out of the failed count, so the
// operator-facing log stays at INFO when the rest of the batch
// succeeded. WARN is reserved for genuine delivery failures.
func TestTrackPartialDispatch_UncertainOnlyDoesNotEscalate(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)

	result := &phaseBResult{
		dispatches: []dispatchResult{
			{Item: dispatchItem{Kind: "task"}, Success: true},
			{Item: dispatchItem{Kind: "task"}, Success: false, Error: fmt.Errorf("wrapped: %w", agent.ErrSubmitConfirmUncertain)},
		},
	}

	qh.trackPartialDispatch(result)

	if len(result.recoveryHints) != 0 {
		t.Errorf("uncertain-only batch must NOT emit a partial-dispatch recovery hint, got %d hint(s)", len(result.recoveryHints))
	}
	logOutput := logBuf.String()
	if strings.Contains(logOutput, "phase_b_partial_dispatch:") {
		t.Errorf("uncertain-only batch must NOT emit phase_b_partial_dispatch WARN: %s", logOutput)
	}
	if !strings.Contains(logOutput, "phase_b_dispatch_with_uncertain") {
		t.Errorf("uncertain-only batch must emit INFO summary phase_b_dispatch_with_uncertain: %s", logOutput)
	}
	if !strings.Contains(logOutput, "uncertain=1") {
		t.Errorf("INFO summary must report uncertain=1: %s", logOutput)
	}
}

// TestTrackPartialDispatch_MixedFailedAndUncertain pins the WARN-with-bucket
// path: when a batch contains BOTH a real failure AND an uncertain entry,
// the WARN must surface and uncertain count must be reported separately.
func TestTrackPartialDispatch_MixedFailedAndUncertain(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)

	result := &phaseBResult{
		dispatches: []dispatchResult{
			{Item: dispatchItem{Kind: "task"}, Success: true},
			{Item: dispatchItem{Kind: "task"}, Success: false, Error: fmt.Errorf("genuine task failure")},
			{Item: dispatchItem{Kind: "task"}, Success: false, Error: fmt.Errorf("submit uncertain: %w", agent.ErrSubmitConfirmUncertain)},
		},
	}

	qh.trackPartialDispatch(result)

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "phase_b_partial_dispatch: total=3 succeeded=1 failed=1 uncertain=1") {
		t.Errorf("mixed batch summary missing or wrong: %s", logOutput)
	}
	if len(result.recoveryHints) != 1 {
		t.Fatalf("expected 1 hint, got %d", len(result.recoveryHints))
	}
	if !strings.Contains(result.recoveryHints[0], "uncertain=1") {
		t.Errorf("hint must report uncertain=1: %s", result.recoveryHints[0])
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
		{
			// "ab" (2B) + 24 × "あ" (72B) = 74B > 72B. Byte index 72 splits
			// the 24th "あ" (cut after its 1st byte), so the partial UTF-8
			// sequence must be stripped → "ab" + 23 × "あ" = 71B.
			name:     "japanese_truncation_strips_partial_rune",
			purposes: map[string]string{"w1": "ab" + strings.Repeat("あ", 24)},
			workerID: "w1",
			want:     "ab" + strings.Repeat("あ", 23),
		},
		{
			// 25 × "あ" = 75B; cut at 72 lands exactly on a rune boundary
			// (24 × 3 = 72), so the result is the full 24-rune prefix with
			// no partial bytes to strip.
			name:     "japanese_truncation_at_rune_boundary",
			purposes: map[string]string{"w1": strings.Repeat("あ", 25)},
			workerID: "w1",
			want:     strings.Repeat("あ", 24),
		},
		{
			// "実装" (6B) + 67 × "a" = 73B > 72B. Cut at byte 72 lands on the
			// last 'a' (byte index 71); both prefix runes are preserved.
			name:     "japanese_prefix_then_ascii_clean_cut",
			purposes: map[string]string{"w1": "実装" + strings.Repeat("a", 67)},
			workerID: "w1",
			want:     "実装" + strings.Repeat("a", 66),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := workerCommitMessage(tt.purposes, tt.workerID)
			if got != tt.want {
				t.Errorf("workerCommitMessage() = %q, want %q", got, tt.want)
			}
			if len(got) > 72 {
				t.Errorf("workerCommitMessage() length %d exceeds max 72", len(got))
			}
			if !utf8.ValidString(got) {
				t.Errorf("workerCommitMessage() produced invalid UTF-8: %q", got)
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

// TestSignalCascadeTracker_TripsAtThreshold pins the cascade break behaviour
// added to absorb extended tmux degradation: once threshold consecutive
// failures occur in the same scan tick, the tracker reports broken=true and
// remains broken until discarded. A success in between resets the counter
// only while still unbroken; once tripped the tracker is sticky for the tick.
func TestSignalCascadeTracker_TripsAtThreshold(t *testing.T) {
	t.Parallel()
	tr := signalCascadeTracker{threshold: 3}

	if tr.recordFailure() {
		t.Fatalf("first failure should not trip threshold=3 tracker")
	}
	if tr.recordFailure() {
		t.Fatalf("second failure should not trip threshold=3 tracker")
	}
	tr.recordSuccess() // resets while not yet broken
	if tr.isBroken() {
		t.Fatalf("intermittent success must keep tracker unbroken")
	}
	if tr.recordFailure() {
		t.Fatalf("first failure after reset should not re-trip threshold=3 tracker")
	}
	if tr.recordFailure() {
		t.Fatalf("second failure after reset should not trip threshold=3 tracker")
	}
	if !tr.recordFailure() {
		t.Fatalf("third consecutive failure must trip threshold=3 tracker")
	}
	if !tr.isBroken() {
		t.Fatalf("isBroken must report true after threshold reached")
	}
	// Sticky: a late success does not un-break the tracker for the rest of
	// the scan tick.
	tr.recordSuccess()
	if !tr.isBroken() {
		t.Fatalf("recordSuccess after broken must NOT clear broken flag")
	}
}

// TestSignalCascadeTracker_DisabledWhenThresholdZero pins that a zero or
// negative threshold leaves the tracker permanently unbroken — the operator
// can fall back to "old behaviour" if a future config knob exposes the value.
func TestSignalCascadeTracker_DisabledWhenThresholdZero(t *testing.T) {
	t.Parallel()
	tr := signalCascadeTracker{threshold: 0}
	for i := 0; i < 100; i++ {
		if tr.recordFailure() {
			t.Fatalf("threshold=0 must never trip; tripped at i=%d", i)
		}
	}
	if tr.isBroken() {
		t.Fatalf("threshold=0 tracker should never report broken")
	}
}

// TestRecordCascadeBreakOutcome_AccumulatesAcrossScans asserts the
// cross-scan meta-circuit: per-tick cascade-break caps log/CPU spend
// within a scan, but a long-tail tmux-server outage that trips break
// every tick escalates to ERROR after a few consecutive trips so the
// operator sees the degradation above the per-tick WARN noise.
//
// The recovery branch must clear the counter exactly once when a successful
// scan tick (any signal delivered, no break) lands.
func TestRecordCascadeBreakOutcome_AccumulatesAcrossScans(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)

	// Three consecutive broken ticks: counter climbs to 3 (== threshold).
	for i := 1; i <= sustainedCascadeBreakThreshold; i++ {
		qh.recordCascadeBreakOutcome(true, 5)
		if got := qh.consecutiveCascadeBreakScans.Load(); int(got) != i {
			t.Fatalf("after %d broken ticks, counter = %d, want %d", i, got, i)
		}
	}

	// One more broken tick still climbs (no automatic clamp); the ERROR
	// log fires every tick from threshold onward.
	qh.recordCascadeBreakOutcome(true, 5)
	if got := qh.consecutiveCascadeBreakScans.Load(); int(got) != sustainedCascadeBreakThreshold+1 {
		t.Fatalf("counter must keep counting past threshold, got %d", got)
	}

	// A clean tick clears the counter.
	qh.recordCascadeBreakOutcome(false, 5)
	if got := qh.consecutiveCascadeBreakScans.Load(); got != 0 {
		t.Fatalf("clean tick must reset counter, got %d", got)
	}
}

// TestRecordCascadeBreakOutcome_NoSignalsHoldsCounter pins that an empty
// scan tick (totalSignals == 0) is a no-op for the counter. We cannot tell
// "tmux healthy but quiet" from "tmux broken; cascade-break suppressed
// everything" without a delivery attempt, so the counter stays at its
// previous value.
func TestRecordCascadeBreakOutcome_NoSignalsHoldsCounter(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)

	// Bring counter up to 2 via two broken ticks.
	qh.recordCascadeBreakOutcome(true, 3)
	qh.recordCascadeBreakOutcome(true, 3)
	if got := qh.consecutiveCascadeBreakScans.Load(); got != 2 {
		t.Fatalf("setup: expected counter=2, got %d", got)
	}

	// Empty tick must NOT clear the counter even though tripped=false.
	qh.recordCascadeBreakOutcome(false, 0)
	if got := qh.consecutiveCascadeBreakScans.Load(); got != 2 {
		t.Fatalf("no-signals tick must hold counter steady, got %d", got)
	}

	// First non-empty clean tick clears it.
	qh.recordCascadeBreakOutcome(false, 1)
	if got := qh.consecutiveCascadeBreakScans.Load(); got != 0 {
		t.Fatalf("non-empty clean tick must reset counter, got %d", got)
	}
}
