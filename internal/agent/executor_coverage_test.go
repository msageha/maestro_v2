package agent

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// === Coverage Test Mock ===

// covMockPaneIO supports sequence-based responses for thorough coverage tests.
type covMockPaneIO struct {
	capturePaneSeq   []mockResp
	capturePaneIdx   int
	captureJoinedSeq []mockResp
	captureJoinedIdx int
	sendCmdErrSeq    []error
	sendCmdIdx       int
	sentCmds         []string
	sendKeysErr      error
	sendCtrlCErr     error
	respawnPaneErr   error
	sendTextErr      error
	sentTexts        []string
	getCmdSeq        []mockResp
	getCmdIdx        int
	isShellSeq       []bool
	isShellIdx       int
	defaultIsShell   bool
	userVars         map[string]string
	panePID          string
	getPIDErr        error
	calls            []string
}

type mockResp struct {
	val string
	err error
}

func newCovMock() *covMockPaneIO {
	return &covMockPaneIO{
		userVars:       make(map[string]string),
		panePID:        "12345",
		defaultIsShell: true,
	}
}

func (m *covMockPaneIO) seqIdx(idx, length int) int {
	if idx >= length {
		return length - 1
	}
	return idx
}

func (m *covMockPaneIO) FindPaneByAgentID(_ string) (string, error) { return "%0", nil }

func (m *covMockPaneIO) SendCtrlC(_ string) error {
	m.calls = append(m.calls, "SendCtrlC")
	return m.sendCtrlCErr
}

func (m *covMockPaneIO) SendKeys(_ string, keys ...string) error {
	m.calls = append(m.calls, "SendKeys:"+strings.Join(keys, ","))
	return m.sendKeysErr
}

func (m *covMockPaneIO) SendCommand(_, cmd string) error {
	m.calls = append(m.calls, "SendCommand:"+cmd)
	m.sentCmds = append(m.sentCmds, cmd)
	if len(m.sendCmdErrSeq) > 0 {
		idx := m.seqIdx(m.sendCmdIdx, len(m.sendCmdErrSeq))
		m.sendCmdIdx++
		return m.sendCmdErrSeq[idx]
	}
	return nil
}

func (m *covMockPaneIO) SendTextAndSubmit(_ context.Context, _, text string) error {
	m.calls = append(m.calls, "SendTextAndSubmit")
	m.sentTexts = append(m.sentTexts, text)
	return m.sendTextErr
}

func (m *covMockPaneIO) SetUserVar(_, name, value string) error {
	m.userVars[name] = value
	return nil
}

func (m *covMockPaneIO) GetUserVar(_, name string) (string, error) {
	return m.userVars[name], nil
}

func (m *covMockPaneIO) GetPanePID(_ string) (string, error) {
	return m.panePID, m.getPIDErr
}

func (m *covMockPaneIO) GetPaneCurrentCommand(_ string) (string, error) {
	m.calls = append(m.calls, "GetPaneCurrentCommand")
	if len(m.getCmdSeq) > 0 {
		idx := m.seqIdx(m.getCmdIdx, len(m.getCmdSeq))
		m.getCmdIdx++
		return m.getCmdSeq[idx].val, m.getCmdSeq[idx].err
	}
	return "bash", nil
}

func (m *covMockPaneIO) CapturePane(_ string, _ int) (string, error) {
	m.calls = append(m.calls, "CapturePane")
	if len(m.capturePaneSeq) > 0 {
		idx := m.seqIdx(m.capturePaneIdx, len(m.capturePaneSeq))
		m.capturePaneIdx++
		return m.capturePaneSeq[idx].val, m.capturePaneSeq[idx].err
	}
	return "output\n ❯ \n", nil
}

func (m *covMockPaneIO) CapturePaneJoined(_ string, _ int) (string, error) {
	m.calls = append(m.calls, "CapturePaneJoined")
	if len(m.captureJoinedSeq) > 0 {
		idx := m.seqIdx(m.captureJoinedIdx, len(m.captureJoinedSeq))
		m.captureJoinedIdx++
		return m.captureJoinedSeq[idx].val, m.captureJoinedSeq[idx].err
	}
	return "output\n ❯ \n", nil
}

func (m *covMockPaneIO) IsShellCommand(_ string) bool {
	if len(m.isShellSeq) > 0 {
		idx := m.seqIdx(m.isShellIdx, len(m.isShellSeq))
		m.isShellIdx++
		return m.isShellSeq[idx]
	}
	return m.defaultIsShell
}

func (m *covMockPaneIO) RespawnPane(_, startDir string) error {
	m.calls = append(m.calls, "RespawnPane:"+startDir)
	return m.respawnPaneErr
}

// newCovExecutor creates an Executor with fast config for coverage tests.
func newCovExecutor(mock *covMockPaneIO) (*Executor, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	cfg := model.WatcherConfig{
		BusyCheckInterval:      0,
		BusyCheckMaxRetries:    1,
		IdleStableSec:          0,
		CooldownAfterClear:     0,
		WaitReadyIntervalSec:   0,
		WaitReadyMaxRetries:    2,
		ClearConfirmTimeoutSec: 2,
		ClearConfirmPollMs:     10,
		ClearMaxAttempts:       3,
		ClearRetryBackoffMs:    1,
	}

	bd := &BusyDetector{
		paneIO: mock,
		config: BusyDetectorConfig{
			IdleStableSec:       0,
			BusyCheckMaxRetries: 1,
			BusyCheckInterval:   0,
		},
		logger:   logger,
		logLevel: LogLevelDebug,
	}

	return &Executor{
		config:       cfg,
		logger:       logger,
		logLevel:     LogLevelDebug,
		paneIO:       mock,
		busyDetector: bd,
		paneState:    NewPaneStateManager(mock),
	}, &buf
}

// callsContain checks if the call log contains a specific entry.
func callsContain(calls []string, target string) bool {
	for _, c := range calls {
		if strings.Contains(c, target) {
			return true
		}
	}
	return false
}

// === M1: clear_ready=true path ===

func TestExecWithClear_ClearReadyTrue_FullPath(t *testing.T) {
	mock := newCovMock()
	// Pre-set clear_ready=true and matching PID
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "12345"
	mock.panePID = "12345"
	mock.defaultIsShell = true // busyDetector → idle

	// waitReady: CapturePane returns prompt-ready
	mock.capturePaneSeq = []mockResp{
		{val: "output\n ❯ \n"},
	}

	// clearAndConfirm: pre-clear capture, then poll returns changed + stable content
	mock.captureJoinedSeq = []mockResp{
		{val: "before-clear-content"},          // pre-clear hash
		{val: "after-clear-new-content"},        // poll 1 (hash changed, no /clear visible)
		{val: "after-clear-new-content"},        // poll 2 (stable)
		{val: "after-clear-new-content"},        // poll 3 (stable) → confirmed
		{val: "after-clear-new-content"},        // busyDetector stage 3 hash A
		{val: "after-clear-new-content"},        // busyDetector stage 3 hash B (same → idle)
	}

	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := exec.execWithClear(ctx, ExecRequest{
		Context: ctx,
		AgentID: "worker1",
		Message: "task payload",
		TaskID:  "task_001",
	}, "%0")

	if result.Error != nil {
		t.Fatalf("expected success, got error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true")
	}

	// Verify /clear was sent (subsequent dispatch path, not first dispatch)
	if !callsContain(mock.calls, "SendCommand:/clear") {
		t.Error("expected /clear to be sent in clear_ready=true path")
	}

	// Verify message was delivered
	if len(mock.sentTexts) != 1 || mock.sentTexts[0] != "task payload" {
		t.Errorf("expected sent text 'task payload', got %v", mock.sentTexts)
	}
}

func TestExecWithClear_ClearReadyTrue_ClearFails_ResetsClearReady(t *testing.T) {
	mock := newCovMock()
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "12345"
	mock.panePID = "12345"

	// waitReady: prompt ready
	mock.capturePaneSeq = []mockResp{{val: "output\n ❯ \n"}}

	// clearAndConfirm: SendCommand always fails
	mock.sendCmdErrSeq = []error{
		fmt.Errorf("tmux error"),
		fmt.Errorf("tmux error"),
		fmt.Errorf("tmux error"),
	}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result := exec.execWithClear(ctx, ExecRequest{
		Context: ctx,
		AgentID: "worker1",
		Message: "task",
		TaskID:  "task_002",
	}, "%0")

	if result.Error == nil {
		t.Fatal("expected error when clearAndConfirm fails")
	}
	if !result.Retryable {
		t.Error("expected Retryable=true")
	}

	// clear_ready should be reset after failure
	if mock.userVars["clear_ready"] != "" {
		t.Errorf("expected clear_ready to be reset, got %q", mock.userVars["clear_ready"])
	}
}

func TestExecWithClear_ClearReadyTrue_ProcessRestarted(t *testing.T) {
	mock := newCovMock()
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "99999" // different from current PID
	mock.panePID = "12345"
	mock.defaultIsShell = true

	// After restart detection, clear_ready is reset → first dispatch path
	mock.capturePaneSeq = []mockResp{{val: "output\n ❯ \n"}}

	exec, _ := newCovExecutor(mock)
	ctx := context.Background()

	result := exec.execWithClear(ctx, ExecRequest{
		Context: ctx,
		AgentID: "worker1",
		Message: "task after restart",
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true (first dispatch after restart)")
	}

	// Should use deliver mode (no /clear sent since clear_ready was reset)
	if callsContain(mock.calls, "SendCommand:/clear") {
		t.Error("should not send /clear after process restart (first dispatch path)")
	}
}

// === M2: ensureWorkingDir tests ===

func TestEnsureWorkingDir_ControlChars(t *testing.T) {
	mock := newCovMock()
	exec, _ := newCovExecutor(mock)

	err := exec.ensureWorkingDir(context.Background(), "%0", "/tmp/\x00injected")
	if err == nil {
		t.Fatal("expected error for control characters")
	}
	if !strings.Contains(err.Error(), "control characters") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEnsureWorkingDir_EmptyPath(t *testing.T) {
	mock := newCovMock()
	exec, _ := newCovExecutor(mock)

	err := exec.ensureWorkingDir(context.Background(), "%0", "")
	if err != nil {
		t.Fatalf("expected nil for empty path, got: %v", err)
	}
}

func TestEnsureWorkingDir_SameCWD(t *testing.T) {
	mock := newCovMock()
	mock.userVars["cwd"] = "/project/worktree1"
	exec, _ := newCovExecutor(mock)

	err := exec.ensureWorkingDir(context.Background(), "%0", "/project/worktree1")
	if err != nil {
		t.Fatalf("expected nil for same CWD, got: %v", err)
	}
	// No commands should be sent
	if len(mock.sentCmds) > 0 {
		t.Errorf("expected no commands for same CWD, got: %v", mock.sentCmds)
	}
}

func TestEnsureWorkingDir_ContextCancelled(t *testing.T) {
	mock := newCovMock()
	// waitForShell will detect cancelled context
	mock.getCmdSeq = []mockResp{{val: "claude"}}
	mock.isShellSeq = []bool{false}
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := exec.ensureWorkingDir(ctx, "%0", "/new/dir")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestEnsureWorkingDir_RespawnAndRelaunch(t *testing.T) {
	mock := newCovMock()
	// waitForShell: pane returns to shell after respawn
	mock.getCmdSeq = []mockResp{{val: "bash"}}
	mock.isShellSeq = []bool{true}
	// waitReadyStrict: prompt detected
	mock.capturePaneSeq = []mockResp{{val: "output\n ❯ \n"}}
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.ensureWorkingDir(ctx, "%0", "/new/dir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify RespawnPane was called with the target directory
	foundRespawn := false
	for _, call := range mock.calls {
		if call == "RespawnPane:/new/dir" {
			foundRespawn = true
		}
	}
	if !foundRespawn {
		t.Error("expected RespawnPane to be called with /new/dir")
	}

	// Verify relaunch command was sent (no cd needed — respawn-pane -c handles it)
	foundLaunch := false
	for _, cmd := range mock.sentCmds {
		if cmd == "maestro agent launch" {
			foundLaunch = true
		}
	}
	if !foundLaunch {
		t.Error("expected 'maestro agent launch' to be sent")
	}

	// Verify CWD was updated
	if mock.userVars["cwd"] != "/new/dir" {
		t.Errorf("expected cwd='/new/dir', got %q", mock.userVars["cwd"])
	}

	// Verify clear_ready was reset
	if mock.userVars["clear_ready"] != "" {
		t.Errorf("expected clear_ready to be reset after dir change, got %q", mock.userVars["clear_ready"])
	}
}

func TestEnsureWorkingDir_RespawnPaneFails(t *testing.T) {
	mock := newCovMock()
	mock.respawnPaneErr = fmt.Errorf("tmux respawn error")
	exec, _ := newCovExecutor(mock)

	err := exec.ensureWorkingDir(context.Background(), "%0", "/new/dir")
	if err == nil {
		t.Fatal("expected error when RespawnPane fails")
	}
	if !strings.Contains(err.Error(), "respawn pane") {
		t.Errorf("unexpected error: %v", err)
	}
}

// === M3: clearAndConfirm retry tests ===

func TestClearAndConfirm_SendCommandFailsAllAttempts(t *testing.T) {
	mock := newCovMock()
	// All 3 attempts fail
	mock.sendCmdErrSeq = []error{
		fmt.Errorf("err1"),
		fmt.Errorf("err2"),
		fmt.Errorf("err3"),
	}
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.clearAndConfirm(ctx, "%0")
	if err == nil {
		t.Fatal("expected error when all send attempts fail")
	}
	if !strings.Contains(err.Error(), "send /clear failed after") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClearAndConfirm_ContextCancelledBeforeAttempt(t *testing.T) {
	mock := newCovMock()
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := exec.clearAndConfirm(ctx, "%0")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClearAndConfirm_NotConfirmedAfterMaxAttempts(t *testing.T) {
	mock := newCovMock()
	// SendCommand succeeds but content always shows "/clear" (not processed)
	mock.captureJoinedSeq = []mockResp{
		{val: "before"},         // pre-clear hash attempt 1
		{val: "❯ /clear\n"},     // poll: /clear still visible
		{val: "before"},         // pre-clear hash attempt 2
		{val: "❯ /clear\n"},     // poll: /clear still visible
		{val: "before"},         // pre-clear hash attempt 3
		{val: "❯ /clear\n"},     // poll: /clear still visible
	}

	exec, _ := newCovExecutor(mock)
	// Use shorter timeout to avoid 7s+ wait
	exec.config.ClearConfirmTimeoutSec = 1
	exec.config.ClearMaxAttempts = 2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.clearAndConfirm(ctx, "%0")
	if err == nil {
		t.Fatal("expected error when /clear not confirmed")
	}
	if !strings.Contains(err.Error(), "not confirmed after") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClearAndConfirm_SuccessOnFirstAttempt(t *testing.T) {
	mock := newCovMock()
	// Pre-clear hash, then poll returns different stable content
	mock.captureJoinedSeq = []mockResp{
		{val: "before-clear"},
		{val: "after-clear"},  // poll 1: hash changed, no /clear
		{val: "after-clear"},  // poll 2: stable → confirmed
	}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.clearAndConfirm(ctx, "%0")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestClearAndConfirm_SendKeysFailsRetries(t *testing.T) {
	mock := newCovMock()
	// SendCommand succeeds, but SendKeys (second Enter) fails
	mock.sendKeysErr = fmt.Errorf("sendkeys error")

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.clearAndConfirm(ctx, "%0")
	if err == nil {
		t.Fatal("expected error when SendKeys fails")
	}
	if !strings.Contains(err.Error(), "send second Enter failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

// === M4: waitStable / waitReady tests ===

func TestWaitStable_ContentStable_PromptReady(t *testing.T) {
	mock := newCovMock()
	// Two CapturePaneJoined calls return same content (stable)
	mock.captureJoinedSeq = []mockResp{
		{val: "stable-content"},
		{val: "stable-content"},
	}
	// CapturePane for prompt check
	mock.capturePaneSeq = []mockResp{
		{val: "output\n ❯ \n"},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.waitStable(context.Background(), "%0", false)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestWaitStable_ContentUnstable(t *testing.T) {
	mock := newCovMock()
	// Two CapturePaneJoined calls return different content (unstable)
	mock.captureJoinedSeq = []mockResp{
		{val: "content-v1"},
		{val: "content-v2"},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.waitStable(context.Background(), "%0", false)
	if err == nil {
		t.Fatal("expected error for unstable content")
	}
	if !strings.Contains(err.Error(), "not stable") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitStable_CaptureError_FirstCapture(t *testing.T) {
	mock := newCovMock()
	mock.captureJoinedSeq = []mockResp{
		{err: fmt.Errorf("capture failed")},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.waitStable(context.Background(), "%0", false)
	if err == nil {
		t.Fatal("expected error on capture failure")
	}
	if !strings.Contains(err.Error(), "capture pane for stability") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitStable_CaptureError_SecondCapture(t *testing.T) {
	mock := newCovMock()
	mock.captureJoinedSeq = []mockResp{
		{val: "content"},
		{err: fmt.Errorf("second capture failed")},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.waitStable(context.Background(), "%0", false)
	if err == nil {
		t.Fatal("expected error on second capture failure")
	}
}

func TestWaitStable_SoftPromptCheck_NoPrompt(t *testing.T) {
	mock := newCovMock()
	// Stable content
	mock.captureJoinedSeq = []mockResp{
		{val: "stable"},
		{val: "stable"},
	}
	// No prompt in final check
	mock.capturePaneSeq = []mockResp{
		{val: "no prompt here"},
	}

	exec, _ := newCovExecutor(mock)

	// softPromptCheck=true → should succeed despite no prompt
	err := exec.waitStable(context.Background(), "%0", true)
	if err != nil {
		t.Fatalf("expected nil with soft prompt check, got: %v", err)
	}
}

func TestWaitStable_HardPromptCheck_NoPrompt(t *testing.T) {
	mock := newCovMock()
	mock.captureJoinedSeq = []mockResp{
		{val: "stable"},
		{val: "stable"},
	}
	mock.capturePaneSeq = []mockResp{
		{val: "no prompt here"},
	}

	exec, _ := newCovExecutor(mock)

	// softPromptCheck=false → should fail
	err := exec.waitStable(context.Background(), "%0", false)
	if err == nil {
		t.Fatal("expected error with hard prompt check and no prompt")
	}
	if !strings.Contains(err.Error(), "no prompt detected") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitStable_SoftPromptCheck_CaptureError(t *testing.T) {
	mock := newCovMock()
	mock.captureJoinedSeq = []mockResp{
		{val: "stable"},
		{val: "stable"},
	}
	// CapturePane (for prompt check) fails
	mock.capturePaneSeq = []mockResp{
		{err: fmt.Errorf("capture error")},
	}

	exec, _ := newCovExecutor(mock)

	// soft mode → should succeed despite capture error
	err := exec.waitStable(context.Background(), "%0", true)
	if err != nil {
		t.Fatalf("expected nil with soft prompt check + capture error, got: %v", err)
	}
}

func TestWaitStable_ContextCancelled(t *testing.T) {
	mock := newCovMock()
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := exec.waitStable(ctx, "%0", false)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitReady_PromptDetectedImmediately(t *testing.T) {
	mock := newCovMock()
	mock.capturePaneSeq = []mockResp{
		{val: "output\n ❯ \n"},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.waitReady(context.Background(), "%0")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestWaitReady_PromptDetectedAfterRetries(t *testing.T) {
	mock := newCovMock()
	mock.capturePaneSeq = []mockResp{
		{val: "not ready yet"},     // attempt 0: no prompt
		{val: "still not ready"},   // attempt 1: no prompt
		{val: "output\n ❯ \n"},     // attempt 2: prompt found
	}

	exec, _ := newCovExecutor(mock)

	err := exec.waitReady(context.Background(), "%0")
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
}

func TestWaitReady_FallbackProceeds(t *testing.T) {
	mock := newCovMock()
	// No prompt ever detected → fallback (proceeds with warning)
	mock.capturePaneSeq = []mockResp{
		{val: "no prompt"},
	}

	exec, _ := newCovExecutor(mock)

	// waitReady with fallback should return nil (proceeds)
	err := exec.waitReady(context.Background(), "%0")
	if err != nil {
		t.Fatalf("expected nil (fallback), got: %v", err)
	}
}

func TestWaitReady_CaptureErrorsExhausted(t *testing.T) {
	mock := newCovMock()
	// All capture attempts fail
	mock.capturePaneSeq = []mockResp{
		{err: fmt.Errorf("tmux error")},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.waitReady(context.Background(), "%0")
	if err == nil {
		t.Fatal("expected error when all captures fail")
	}
	if !strings.Contains(err.Error(), "capture pane failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitReady_ContextCancelled(t *testing.T) {
	mock := newCovMock()
	mock.capturePaneSeq = []mockResp{
		{val: "not ready"},
	}
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := exec.waitReady(ctx, "%0")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestWaitReadyStrict_PromptDetected(t *testing.T) {
	mock := newCovMock()
	mock.capturePaneSeq = []mockResp{
		{val: "output\n ❯ \n"},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.waitReadyStrict(context.Background(), "%0")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestWaitReadyStrict_NoPrompt_Fails(t *testing.T) {
	mock := newCovMock()
	mock.capturePaneSeq = []mockResp{
		{val: "no prompt ever"},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.waitReadyStrict(context.Background(), "%0")
	if err == nil {
		t.Fatal("expected error when prompt never detected (strict mode)")
	}
	if !strings.Contains(err.Error(), "prompt not detected") {
		t.Errorf("unexpected error: %v", err)
	}
}

// === M5: waitForShell tests ===

func TestWaitForShell_Immediate(t *testing.T) {
	mock := newCovMock()
	mock.getCmdSeq = []mockResp{{val: "bash"}}
	mock.isShellSeq = []bool{true}

	exec, _ := newCovExecutor(mock)

	err := exec.waitForShell(context.Background(), "%0")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestWaitForShell_ShellAfterPolls(t *testing.T) {
	mock := newCovMock()
	// Claude running for 2 polls, then shell detected
	mock.getCmdSeq = []mockResp{
		{val: "claude"},
		{val: "claude"},
		{val: "bash"},
	}
	mock.isShellSeq = []bool{false, false, true}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := exec.waitForShell(ctx, "%0")
	if err != nil {
		t.Fatalf("expected success after polls, got: %v", err)
	}
}

func TestWaitForShell_ConsecutiveErrors(t *testing.T) {
	mock := newCovMock()
	// 5 consecutive errors → failure
	mock.getCmdSeq = []mockResp{
		{err: fmt.Errorf("err1")},
		{err: fmt.Errorf("err2")},
		{err: fmt.Errorf("err3")},
		{err: fmt.Errorf("err4")},
		{err: fmt.Errorf("err5")},
	}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := exec.waitForShell(ctx, "%0")
	if err == nil {
		t.Fatal("expected error for consecutive errors")
	}
	if !strings.Contains(err.Error(), "consecutive errors") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitForShell_ErrorRecovery(t *testing.T) {
	mock := newCovMock()
	// 3 errors, then success (under threshold of 5)
	mock.getCmdSeq = []mockResp{
		{err: fmt.Errorf("err1")},
		{err: fmt.Errorf("err2")},
		{err: fmt.Errorf("err3")},
		{val: "bash"},
	}
	mock.isShellSeq = []bool{true}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := exec.waitForShell(ctx, "%0")
	if err != nil {
		t.Fatalf("expected success after error recovery, got: %v", err)
	}
}

func TestWaitForShell_ContextCancelled(t *testing.T) {
	mock := newCovMock()
	// Never returns shell → would loop forever without context cancel
	mock.getCmdSeq = []mockResp{{val: "claude"}}
	mock.isShellSeq = []bool{false}

	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := exec.waitForShell(ctx, "%0")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestWaitForShell_TimeoutViaContext(t *testing.T) {
	mock := newCovMock()
	// Never returns shell → context timeout triggers before maxAttempts
	mock.getCmdSeq = []mockResp{{val: "claude"}}
	mock.isShellSeq = []bool{false}

	exec, _ := newCovExecutor(mock)

	// Short timeout to exercise the "shell not found" path without waiting 15s
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := exec.waitForShell(ctx, "%0")
	if err == nil {
		t.Fatal("expected error when shell not detected")
	}
	// Could be either "cancelled" or "shell not detected" depending on timing
}

// === Additional edge case tests from codex review ===

// M1 supplement: clear_ready=true with VerdictBusy after clear
func TestExecWithClear_ClearReadyTrue_BusyAfterClear(t *testing.T) {
	mock := newCovMock()
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "12345"
	mock.panePID = "12345"

	// waitReady: prompt ready
	mock.capturePaneSeq = []mockResp{
		{val: "output\n ❯ \n"}, // waitReady
		{val: "working..."},    // busyDetector stage 2 (CapturePane for pattern)
	}

	// clearAndConfirm succeeds
	mock.captureJoinedSeq = []mockResp{
		{val: "before"},        // pre-clear hash
		{val: "after"},         // poll 1 (changed)
		{val: "after"},         // poll 2 (stable → confirmed)
		{val: "busy-content1"}, // busyDetector stage 3 hash A
		{val: "busy-content2"}, // busyDetector stage 3 hash B (different → busy)
		{val: "busy-content3"}, // busyDetector retry stage 3 hash A
		{val: "busy-content4"}, // busyDetector retry stage 3 hash B (different → busy)
	}

	// busyDetector: not shell → proceed to stages 2/3
	mock.defaultIsShell = false
	mock.getCmdSeq = []mockResp{
		{val: "claude"}, // busyDetector stage 1
		{val: "claude"}, // retry
	}
	mock.isShellSeq = []bool{false, false}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := exec.execWithClear(ctx, ExecRequest{
		Context: ctx,
		AgentID: "worker1",
		Message: "task",
		TaskID:  "task_003",
	}, "%0")

	if result.Error == nil {
		t.Fatal("expected error when agent is busy after clear")
	}
	if !result.Retryable {
		t.Error("expected Retryable=true for busy after clear")
	}
	// Message should NOT have been sent
	if len(mock.sentTexts) > 0 {
		t.Errorf("message should not be sent when agent is busy, got %v", mock.sentTexts)
	}
}

// M1 supplement: clear_ready=true with VerdictUndecided returns sentinel error
func TestExecWithClear_ClearReadyTrue_UndecidedAfterClear(t *testing.T) {
	mock := newCovMock()
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "12345"
	mock.panePID = "12345"

	mock.capturePaneSeq = []mockResp{
		{val: "output\n ❯ \n"},                     // waitReady
		{err: fmt.Errorf("capture error stage 2")}, // busyDetector CapturePane → undecided
	}

	mock.captureJoinedSeq = []mockResp{
		{val: "before"},
		{val: "after"},
		{val: "after"},
	}

	mock.defaultIsShell = false
	mock.getCmdSeq = []mockResp{{val: "claude"}}
	mock.isShellSeq = []bool{false}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := exec.execWithClear(ctx, ExecRequest{
		Context: ctx,
		AgentID: "worker1",
		Message: "task",
	}, "%0")

	if result.Error == nil {
		t.Fatal("expected error for undecided verdict")
	}
	if !result.Retryable {
		t.Error("expected Retryable=true for undecided")
	}
}

// M2 supplement: clear_ready_pid also reset after CWD change
func TestEnsureWorkingDir_ResetsClearReadyPID(t *testing.T) {
	mock := newCovMock()
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "99999"
	mock.getCmdSeq = []mockResp{{val: "bash"}}
	mock.isShellSeq = []bool{true}
	mock.capturePaneSeq = []mockResp{{val: "output\n ❯ \n"}}
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.ensureWorkingDir(ctx, "%0", "/new/dir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both clear_ready AND clear_ready_pid should be reset
	if mock.userVars["clear_ready"] != "" {
		t.Errorf("expected clear_ready to be reset, got %q", mock.userVars["clear_ready"])
	}
	if mock.userVars["clear_ready_pid"] != "" {
		t.Errorf("expected clear_ready_pid to be reset, got %q", mock.userVars["clear_ready_pid"])
	}
}

// M3 supplement: pre-capture failure fallback (preClearHashValid=false)
func TestClearAndConfirm_PreCaptureFailure_FallbackToStricter(t *testing.T) {
	mock := newCovMock()
	// Pre-clear capture fails → preClearHashValid=false
	// Then poll returns stable content 3 times (stricter fallback requires 3 stable polls)
	mock.captureJoinedSeq = []mockResp{
		{err: fmt.Errorf("pre-capture error")}, // pre-clear → hash invalid
		{val: "post-clear"},                     // poll 1
		{val: "post-clear"},                     // poll 2
		{val: "post-clear"},                     // poll 3 → confirmed (3 stable)
	}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.clearAndConfirm(ctx, "%0")
	if err != nil {
		t.Fatalf("expected success with pre-capture fallback, got: %v", err)
	}
}

// M4 supplement: waitStable hard prompt check with CapturePane error
func TestWaitStable_HardPromptCheck_CaptureError(t *testing.T) {
	mock := newCovMock()
	mock.captureJoinedSeq = []mockResp{
		{val: "stable"},
		{val: "stable"},
	}
	mock.capturePaneSeq = []mockResp{
		{err: fmt.Errorf("prompt capture failed")},
	}

	exec, _ := newCovExecutor(mock)

	// softPromptCheck=false → capture error is fatal
	err := exec.waitStable(context.Background(), "%0", false)
	if err == nil {
		t.Fatal("expected error when prompt capture fails in hard mode")
	}
	if !strings.Contains(err.Error(), "capture pane for prompt check") {
		t.Errorf("unexpected error: %v", err)
	}
}

// M4 supplement: waitReadyStrict capture errors exhausted
func TestWaitReadyStrict_CaptureErrorsExhausted(t *testing.T) {
	mock := newCovMock()
	mock.capturePaneSeq = []mockResp{
		{err: fmt.Errorf("err")},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.waitReadyStrict(context.Background(), "%0")
	if err == nil {
		t.Fatal("expected error when all captures fail in strict mode")
	}
	if !strings.Contains(err.Error(), "capture pane failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSleepCtx_NormalCompletion(t *testing.T) {
	err := sleepCtx(context.Background(), 1*time.Millisecond)
	if err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
}

func TestSleepCtx_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sleepCtx(ctx, 10*time.Second)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}
