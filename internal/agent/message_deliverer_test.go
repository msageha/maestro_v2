package agent

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// --- sendAndConfirm isolated tests ---

func newTestDeliverer(mock *mockPaneIO) *messageDeliverer {
	cfg := model.WatcherConfig{}
	ps := newPaneStateManager(mock)
	return newMessageDeliverer(mock, ps, &cfg, DefaultExecutorConfig(), log.New(&bytes.Buffer{}, "", 0), logLevelDebug)
}

func TestSendAndConfirm_ShellGuard_RejectsShell(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "bash"
	mock.isShell = true
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "payload",
	}, "%0")

	if result.Error == nil {
		t.Fatal("expected error for shell guard rejection")
	}
	if !result.Retryable {
		t.Error("expected Retryable=true for shell rejection")
	}
	if len(mock.sentTexts) != 0 {
		t.Errorf("expected no text sent after shell guard rejection, got %v", mock.sentTexts)
	}
}

func TestSendAndConfirm_Success(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "hello agent",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true")
	}
	if len(mock.sentTexts) != 1 || mock.sentTexts[0] != "hello agent" {
		t.Errorf("expected sent text 'hello agent', got %v", mock.sentTexts)
	}
	if mock.userVars["status"] != "busy" {
		t.Errorf("expected status=busy, got %q", mock.userVars["status"])
	}
}

func TestSendAndConfirm_NilContext_UsesBackground(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		Message: "test",
		Context: nil, // nil context should be handled
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true with nil context")
	}
}

func TestSendAndConfirm_SendTextFails(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.sendTextErr = fmt.Errorf("tmux send error")
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "payload",
	}, "%0")

	if result.Error == nil {
		t.Fatal("expected error when SendTextAndSubmit fails")
	}
	if !result.Retryable {
		t.Error("expected Retryable=true for send failure")
	}
}

func TestSendAndConfirm_SetStatusFails(t *testing.T) {
	t.Parallel()
	// Bug L: SetStatus failure must NOT propagate as a delivery error.
	// The message was already sent successfully, so returning an error
	// would cause the dispatcher's inline retry to re-deliver the same
	// envelope and trigger a duplicate plan_submit on the planner side.
	// The failure is logged at warn level and the result is treated as
	// success — the busy hint is best-effort.
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.SetUserVarFn = func(_, name, _ string) error {
		if name == "status" {
			return fmt.Errorf("uservar write error")
		}
		return nil
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "payload",
	}, "%0")

	if result.Error != nil {
		t.Fatalf("SetStatus failure must be best-effort, got error: %v", result.Error)
	}
	if !result.Success {
		t.Fatal("expected Success=true even when SetStatus fails")
	}
	if len(mock.sentTexts) != 1 {
		t.Errorf("expected text to be sent before status failure, got %v", mock.sentTexts)
	}
}

func TestSendAndConfirm_GetCommandError_ProceedsToSend(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.GetPaneCurrentCommandFn = func(_ string) (string, error) {
		return "", fmt.Errorf("tmux error")
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		Message: "payload",
	}, "%0")

	// When GetPaneCurrentCommand errors, shell guard is skipped → delivery proceeds
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true when command check errors (guard skipped)")
	}
}

// --- clearConfirmationPoller tests ---

func TestClearConfirmationPoller_IsConfirmed_WithValidPreClearHash(t *testing.T) {
	t.Parallel()
	p := &clearConfirmationPoller{
		preClearHashValid: true,
	}

	// Not confirmed: no hash change, no stable polls
	if p.isConfirmed() {
		t.Error("expected not confirmed with zero state")
	}

	// Hash changed but only 1 stable poll
	p.hashChanged = true
	p.stableCount = 1
	if p.isConfirmed() {
		t.Error("expected not confirmed with only 1 stable poll")
	}

	// Hash changed and 2 stable polls → confirmed
	p.stableCount = 2
	if !p.isConfirmed() {
		t.Error("expected confirmed with hash change + 2 stable polls")
	}
}

func TestClearConfirmationPoller_IsConfirmed_WithoutValidPreClearHash(t *testing.T) {
	t.Parallel()
	p := &clearConfirmationPoller{
		preClearHashValid: false,
	}

	// Need 3 stable polls without valid hash
	p.stableCount = 2
	if p.isConfirmed() {
		t.Error("expected not confirmed with only 2 stable polls (no hash)")
	}

	p.stableCount = 3
	if !p.isConfirmed() {
		t.Error("expected confirmed with 3 stable polls (no hash)")
	}
}

func TestClearConfirmationPoller_Reset(t *testing.T) {
	t.Parallel()
	p := &clearConfirmationPoller{
		stableCount:  5,
		prevPollHash: "abc123",
		hashChanged:  true,
	}

	p.reset()

	if p.stableCount != 0 {
		t.Errorf("expected stableCount=0 after reset, got %d", p.stableCount)
	}
	if p.prevPollHash != "" {
		t.Errorf("expected empty prevPollHash after reset, got %q", p.prevPollHash)
	}
	if p.hashChanged {
		t.Error("expected hashChanged=false after reset")
	}
}

func TestClearConfirmationPoller_Poll_CaptureError_Resets(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.CapturePaneJoinedFn = func(_ string, _ int) (string, error) {
		return "", fmt.Errorf("capture error")
	}

	p := newClearConfirmationPoller(
		mock, "%0", "prehash", true, 12,
		log.New(&bytes.Buffer{}, "", 0), logLevelDebug,
	)
	p.stableCount = 3
	p.hashChanged = true

	result := p.poll()
	if result {
		t.Error("expected poll to return false on capture error")
	}
	if p.stableCount != 0 {
		t.Errorf("expected stableCount=0 after capture error, got %d", p.stableCount)
	}
}

func TestClearConfirmationPoller_Poll_ClearTextVisible_ResetsStable(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.CapturePaneJoinedFn = func(_ string, _ int) (string, error) {
		return "❯ /clear\n", nil
	}

	p := newClearConfirmationPoller(
		mock, "%0", "prehash", true, 12,
		log.New(&bytes.Buffer{}, "", 0), logLevelDebug,
	)
	p.stableCount = 3

	result := p.poll()
	if result {
		t.Error("expected poll to return false when /clear text visible")
	}
	if p.stableCount != 0 {
		t.Errorf("expected stableCount reset, got %d", p.stableCount)
	}
}

func TestClearConfirmationPoller_PollUntilTimeout_ContextCancelled(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.captureContent = "unchanging content"

	p := newClearConfirmationPoller(
		mock, "%0", "prehash", true, 12,
		log.New(&bytes.Buffer{}, "", 0), logLevelDebug,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.pollUntilTimeout(ctx, 5*time.Second, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// --- sleepWithBackoff tests ---

func TestSleepWithBackoff_NormalCompletion(t *testing.T) {
	t.Parallel()
	err := sleepWithBackoff(context.Background(), 1, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSleepWithBackoff_ExponentialIncrease(t *testing.T) {
	t.Parallel()
	// Attempt 1: 1ms, Attempt 2: 2ms, Attempt 3: 4ms
	start := time.Now()
	_ = sleepWithBackoff(context.Background(), 1, 3) // 4ms
	elapsed := time.Since(start)
	if elapsed < 3*time.Millisecond {
		t.Errorf("expected at least 3ms sleep for attempt 3 (2^2 * 1ms), got %v", elapsed)
	}
}

func TestSleepWithBackoff_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sleepWithBackoff(ctx, 10000, 1) // would be 10s if not cancelled
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// --- getPaneMutex / removePaneMutex tests ---

func TestRemovePaneMutex(t *testing.T) {
	t.Parallel()
	d := &messageDeliverer{}

	mu1 := d.getPaneMutex("%0")
	if mu1 == nil {
		t.Fatal("expected non-nil mutex")
	}

	d.removePaneMutex("%0")

	// After removal, a new mutex should be created
	mu2 := d.getPaneMutex("%0")
	if mu1 == mu2 {
		t.Error("expected different mutex after removal")
	}
}
