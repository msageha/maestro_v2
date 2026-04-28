package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// === M1: clear_ready=true path ===

func TestExecWithClear_ClearReadyTrue_FullPath(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// Pre-set clear_ready=true and matching PID
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "12345"
	mock.panePID = "12345"
	// ensureClaudeRunning: not shell, busyDetector: shell → idle, sendAndConfirm guard: not shell
	mock.isShellSeq = []bool{false, true, false}

	// waitReady: CapturePane returns prompt-ready
	mock.capturePaneSeq = []mockResp{
		{val: "output\n ❯ \n"},
	}

	// clearAndConfirm: pre-clear capture, then poll returns changed + stable content
	mock.captureJoinedSeq = []mockResp{
		{val: "before-clear-content"},    // pre-clear hash
		{val: "after-clear-new-content"}, // poll 1 (hash changed, no /clear visible)
		{val: "after-clear-new-content"}, // poll 2 (stable)
		{val: "after-clear-new-content"}, // poll 3 (stable) → confirmed
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
	t.Parallel()
	mock := newMockPaneIO()
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "12345"
	mock.panePID = "12345"
	// ensureClaudeRunning: Claude is running
	mock.isShellSeq = []bool{false}

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
	t.Parallel()
	mock := newMockPaneIO()
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "99999" // different from current PID
	mock.panePID = "12345"
	// ensureClaudeRunning (execWithClear): not shell,
	// ensureClaudeRunning (execDeliver first dispatch): not shell,
	// busyDetector: shell → idle, sendAndConfirm guard: not shell
	mock.isShellSeq = []bool{false, false, true, false}

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
	t.Parallel()
	mock := newMockPaneIO()
	exec, _ := newCovExecutor(mock)

	err := exec.processManager.ensureWorkingDir(context.Background(), "%0", "/tmp/\x00injected")
	if err == nil {
		t.Fatal("expected error for control characters")
	}
	if !errors.Is(err, ErrControlChars) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEnsureWorkingDir_EmptyPath(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	exec, _ := newCovExecutor(mock)

	err := exec.processManager.ensureWorkingDir(context.Background(), "%0", "")
	if err != nil {
		t.Fatalf("expected nil for empty path, got: %v", err)
	}
}

func TestEnsureWorkingDir_SameCWD(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.userVars["cwd"] = "/project/worktree1"
	exec, _ := newCovExecutor(mock)

	err := exec.processManager.ensureWorkingDir(context.Background(), "%0", "/project/worktree1")
	if err != nil {
		t.Fatalf("expected nil for same CWD, got: %v", err)
	}
	// No commands should be sent
	if len(mock.sentCmds) > 0 {
		t.Errorf("expected no commands for same CWD, got: %v", mock.sentCmds)
	}
}

func TestEnsureWorkingDir_ContextCancelled(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// waitForShell will detect cancelled context
	mock.getCmdSeq = []mockResp{{val: "claude"}}
	mock.isShellSeq = []bool{false}
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := exec.processManager.ensureWorkingDir(ctx, "%0", "/new/dir")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestEnsureWorkingDir_RespawnAndRelaunch(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// waitForShell: pane returns to shell after respawn
	mock.getCmdSeq = []mockResp{{val: "bash"}}
	mock.isShellSeq = []bool{true}
	// waitReadyStrict: prompt detected
	mock.capturePaneSeq = []mockResp{{val: "output\n ❯ \n"}}
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.processManager.ensureWorkingDir(ctx, "%0", "/new/dir")
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

	// Verify relaunch command was sent (no cd needed — respawn-pane -c handles it).
	// Use isAgentLaunchCmd to match both bare and absolute-path forms.
	foundLaunch := false
	for _, cmd := range mock.sentCmds {
		if isAgentLaunchCmd(cmd) {
			foundLaunch = true
		}
	}
	if !foundLaunch {
		t.Errorf("expected an agent launch command to be sent, got cmds: %v", mock.sentCmds)
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
	t.Parallel()
	mock := newMockPaneIO()
	mock.respawnPaneErr = fmt.Errorf("tmux respawn error")
	exec, _ := newCovExecutor(mock)

	err := exec.processManager.ensureWorkingDir(context.Background(), "%0", "/new/dir")
	if err == nil {
		t.Fatal("expected error when RespawnPane fails")
	}
	if !errors.Is(err, ErrRespawnPane) {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRespawnPaneToProjectRoot_ResetsCWDAndClearReady pins the 2026-04-28
// (round 2) follow-up to the Stop hook posix_spawn '/bin/sh' ENOENT
// regression. The first fix added a stale-cwd probe to ensureWorkingDir,
// but that only fires at the next dispatch — claude-code's Stop hook can
// run between turns, while the worktree dir is being removed. Phase B
// now invokes RespawnPaneToProjectRoot before each `git worktree
// remove`, so the pane lands in the project root (a guaranteed-stable
// directory) before the cleanup's rm hits its old cwd.
//
// The pane is respawned, the @cwd label is reset (so the next
// ensureWorkingDir call is forced into a fresh respawn for the new
// task's worktree), and clear_ready is reset (paste-buffer mutex needs a
// re-acknowledged ready prompt before /clear runs again).
func TestRespawnPaneToProjectRoot_ResetsCWDAndClearReady(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.userVars["cwd"] = "/project/.maestro/worktrees/cmd_X/worker1"
	mock.userVars["clear_ready"] = "1"
	exec, buf := newCovExecutor(mock)
	exec.maestroDir = "/project/.maestro"

	if err := exec.RespawnPaneToProjectRoot("worker1"); err != nil {
		t.Fatalf("RespawnPaneToProjectRoot: %v", err)
	}

	wantTarget := "RespawnPane:/project"
	foundRespawn := false
	for _, call := range mock.calls {
		if call == wantTarget {
			foundRespawn = true
		}
	}
	if !foundRespawn {
		t.Errorf("expected %q, calls=%v", wantTarget, mock.calls)
	}
	if mock.userVars["cwd"] != "" {
		t.Errorf("cwd not reset, got %q", mock.userVars["cwd"])
	}
	if mock.userVars["clear_ready"] != "" {
		t.Errorf("clear_ready not reset, got %q", mock.userVars["clear_ready"])
	}
	if !strings.Contains(buf.String(), "respawn_to_project_root") {
		t.Errorf("expected respawn_to_project_root info log, got: %s", buf.String())
	}
	// 2026-04-28 retest2: respawn must mark the pane as "evicted" so
	// status.go does not flip the worker to "dead" while it sits in
	// shell waiting for the next dispatch.
	if got := mock.userVars["agent_state"]; got != "evicted" {
		t.Errorf("agent_state = %q, want \"evicted\"", got)
	}
}

// TestRespawnPaneToProjectRoot_PaneNotFoundIsNoOp pins fail-open behaviour
// for the case where the worker has no live pane (claude never started,
// or the pane was already torn down). Phase B should be free to call
// this for every worker associated with the command without worrying
// about whether each pane actually exists.
func TestRespawnPaneToProjectRoot_PaneNotFoundIsNoOp(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.findPaneErr = fmt.Errorf("pane not found")
	exec, _ := newCovExecutor(mock)
	exec.maestroDir = "/project/.maestro"

	if err := exec.RespawnPaneToProjectRoot("worker1"); err != nil {
		t.Fatalf("expected no error when pane missing, got: %v", err)
	}
	for _, call := range mock.calls {
		if strings.HasPrefix(call, "RespawnPane:") {
			t.Errorf("expected no respawn when pane is missing, got: %v", mock.calls)
		}
	}
}

// TestEnsureWorkingDir_StaleCwdForcesRespawn pins the 2026-04-28 fix for the
// "Stop hook posix_spawn ENOENT '/bin/sh'" warning seen in the E2E run.
// The Phase B publish/cleanup pipeline removes a worktree directory shortly
// after a Worker reports completion, which leaves the pane's tracked @cwd
// pointing at a deleted path. When claude-code's Stop hook then tries to
// spawn /bin/sh from that pane, node.js reports the chdir failure as an
// ENOENT on the binary itself. ensureWorkingDir now stat's the tracked cwd
// even when the requested path matches, and forces a respawn when the
// directory is missing so the pane lands somewhere real before the next
// dispatch.
func TestEnsureWorkingDir_StaleCwdForcesRespawn(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.userVars["cwd"] = "/project/worktree-deleted"
	// Provide the responses RespawnAndRelaunch needs — same shape as
	// TestEnsureWorkingDir_RespawnAndRelaunch — so the respawn path runs
	// to completion when the stale-cwd branch fires.
	mock.getCmdSeq = []mockResp{{val: "bash"}}
	mock.isShellSeq = []bool{true}
	mock.capturePaneSeq = []mockResp{{val: "output\n ❯ \n"}}
	exec, buf := newCovExecutor(mock)
	// Override the default test stub (which always reports "exists") so the
	// tracked cwd looks deleted from disk while the requested cwd is still
	// the same string.
	exec.processManager.dirExists = func(string) bool { return false }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.processManager.ensureWorkingDir(ctx, "%0", "/project/worktree-deleted")
	if err != nil {
		t.Fatalf("expected stale-cwd respawn to succeed, got: %v", err)
	}

	foundRespawn := false
	for _, call := range mock.calls {
		if call == "RespawnPane:/project/worktree-deleted" {
			foundRespawn = true
		}
	}
	if !foundRespawn {
		t.Errorf("expected RespawnPane to fire even though tracked cwd matches workingDir, calls: %v", mock.calls)
	}
	if !strings.Contains(buf.String(), "working_dir_stale_cwd_respawn") {
		t.Errorf("expected stale-cwd info log, got buffer: %s", buf.String())
	}
}

// === M3: clearAndConfirm retry tests ===

func TestClearAndConfirm_SendCommandFailsAllAttempts(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
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
	if !errors.Is(err, ErrClearSendFailed) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClearAndConfirm_ContextCancelledBeforeAttempt(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := exec.clearAndConfirm(ctx, "%0")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClearAndConfirm_NotConfirmedAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// SendCommand succeeds but content always shows "/clear" (not processed)
	mock.captureJoinedSeq = []mockResp{
		{val: "before"},     // pre-clear hash attempt 1
		{val: "❯ /clear\n"}, // poll: /clear still visible
		{val: "before"},     // pre-clear hash attempt 2
		{val: "❯ /clear\n"}, // poll: /clear still visible
		{val: "before"},     // pre-clear hash attempt 3
		{val: "❯ /clear\n"}, // poll: /clear still visible
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
	if !errors.Is(err, ErrClearNotConfirmed) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestClearAndConfirm_SuccessOnFirstAttempt(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// Pre-clear hash, then poll returns different stable content
	mock.captureJoinedSeq = []mockResp{
		{val: "before-clear"},
		{val: "after-clear"}, // poll 1: hash changed, no /clear
		{val: "after-clear"}, // poll 2: stable → confirmed
	}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.clearAndConfirm(ctx, "%0")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

// TestClearAndConfirm_SendsClearExactlyOncePerAttempt is the regression
// guard for the 2026-04-27 doubled-/clear bug. clearAndConfirm used to
// follow SendCommand("/clear") with a second SendKeys("Enter") to handle a
// completion-prompt UX that older Claude Code releases displayed for slash
// commands. Claude Code 2.x executes /clear immediately on the first Enter
// and treats the trailing Enter as "re-run last command", so the worker
// pane logged TWO /clear executions on every task transition. The fix
// removes the second Enter unconditionally; this test pins that contract by
// asserting that SendCommand("/clear") is the only call recorded against
// the mock when the post-clear poller confirms processing on the first
// attempt — no SendKeys with bare "Enter" should appear.
func TestClearAndConfirm_SendsClearExactlyOncePerAttempt(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// Pre-clear capture differs from post-clear capture so the poller's
	// hash-changed criterion is satisfied on the first poll, then the
	// second poll returns identical content for stability.
	mock.captureJoinedSeq = []mockResp{
		{val: "before-clear"}, // pre-clear snapshot
		{val: "❯ \n"},         // poll 1: /clear text gone, hash changed
		{val: "❯ \n"},         // poll 2: stable → confirmed
	}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := exec.clearAndConfirm(ctx, "%0"); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	if got := len(mock.sentCmds); got != 1 || mock.sentCmds[0] != "/clear" {
		t.Fatalf("expected exactly one SendCommand(\"/clear\") call, got %v", mock.sentCmds)
	}
	for _, call := range mock.calls {
		if call == "SendKeys:Enter" {
			t.Fatalf("clearAndConfirm must not send a bare Enter after /clear (would re-run /clear in Claude Code 2.x), calls=%v", mock.calls)
		}
	}
}

// TestClearAndConfirm_LogsExactlyOneSendInvocationOnSuccess pins the
// observability contract added during the 2026-04-27 doubled-/clear pane
// investigation. The pane transcript shows "/clear" twice per task
// transition; to distinguish a true double-send from a Claude Code
// rendering quirk, clearAndConfirm emits a `clear_send_invocation` INFO
// log immediately before each underlying SendCommand("/clear"). Operators
// grep this token in agent_executor.log: one entry per visible "/clear" in
// the pane → real bug, one entry per task transition (with two visible
// "/clear" lines) → cosmetic. This test guards both directions:
//
//  1. Exactly one `clear_send_invocation` line per attempt on the success
//     path (no off-by-one or accidental loop multiplier),
//  2. Exactly one `clear_send_done` line on the success path (we don't
//     accidentally emit the begin/done pair twice).
func TestClearAndConfirm_LogsExactlyOneSendInvocationOnSuccess(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.captureJoinedSeq = []mockResp{
		{val: "before-clear"},
		{val: "❯ \n"},
		{val: "❯ \n"},
	}

	exec, buf := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := exec.clearAndConfirm(ctx, "%0"); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	logs := buf.String()
	if got := strings.Count(logs, "clear_send_invocation"); got != 1 {
		t.Fatalf("expected exactly one clear_send_invocation log on success, got %d. Full log:\n%s", got, logs)
	}
	if got := strings.Count(logs, "clear_send_done"); got != 1 {
		t.Fatalf("expected exactly one clear_send_done log on success, got %d. Full log:\n%s", got, logs)
	}
	if got := strings.Count(logs, "clear_send_begin"); got != 1 {
		t.Fatalf("expected exactly one clear_send_begin log per call, got %d. Full log:\n%s", got, logs)
	}
}

// === M4: waitStable / waitReady tests ===

func TestWaitStable_ContentStable_PromptReady(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
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

	err := exec.processManager.waitStable(context.Background(), "%0", false)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestWaitStable_ContentUnstable(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// Two CapturePaneJoined calls return different content (unstable)
	mock.captureJoinedSeq = []mockResp{
		{val: "content-v1"},
		{val: "content-v2"},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitStable(context.Background(), "%0", false)
	if err == nil {
		t.Fatal("expected error for unstable content")
	}
	if !errors.Is(err, ErrNotStable) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitStable_CaptureError_FirstCapture(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.captureJoinedSeq = []mockResp{
		{err: fmt.Errorf("capture failed")},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitStable(context.Background(), "%0", false)
	if err == nil {
		t.Fatal("expected error on capture failure")
	}
	if !errors.Is(err, ErrCapturePane) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitStable_CaptureError_SecondCapture(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.captureJoinedSeq = []mockResp{
		{val: "content"},
		{err: fmt.Errorf("second capture failed")},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitStable(context.Background(), "%0", false)
	if err == nil {
		t.Fatal("expected error on second capture failure")
	}
}

func TestWaitStable_SoftPromptCheck_NoPrompt(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
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
	err := exec.processManager.waitStable(context.Background(), "%0", true)
	if err != nil {
		t.Fatalf("expected nil with soft prompt check, got: %v", err)
	}
}

func TestWaitStable_HardPromptCheck_NoPrompt(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.captureJoinedSeq = []mockResp{
		{val: "stable"},
		{val: "stable"},
	}
	mock.capturePaneSeq = []mockResp{
		{val: "no prompt here"},
	}

	exec, _ := newCovExecutor(mock)

	// softPromptCheck=false → should fail
	err := exec.processManager.waitStable(context.Background(), "%0", false)
	if err == nil {
		t.Fatal("expected error with hard prompt check and no prompt")
	}
	if !errors.Is(err, ErrNoPrompt) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitStable_SoftPromptCheck_CaptureError(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
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
	err := exec.processManager.waitStable(context.Background(), "%0", true)
	if err != nil {
		t.Fatalf("expected nil with soft prompt check + capture error, got: %v", err)
	}
}

func TestWaitStable_ContextCancelled(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := exec.processManager.waitStable(ctx, "%0", false)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitReady_PromptDetectedImmediately(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.capturePaneSeq = []mockResp{
		{val: "output\n ❯ \n"},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitReady(context.Background(), "%0")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestWaitReady_PromptDetectedAfterRetries(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.capturePaneSeq = []mockResp{
		{val: "not ready yet"},   // attempt 0: no prompt
		{val: "still not ready"}, // attempt 1: no prompt
		{val: "output\n ❯ \n"},   // attempt 2: prompt found
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitReady(context.Background(), "%0")
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
}

func TestWaitReady_FallbackProceeds(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// No prompt ever detected → fallback (proceeds with warning)
	mock.capturePaneSeq = []mockResp{
		{val: "no prompt"},
	}

	exec, _ := newCovExecutor(mock)

	// waitReady with fallback should return nil (proceeds)
	err := exec.processManager.waitReady(context.Background(), "%0")
	if err != nil {
		t.Fatalf("expected nil (fallback), got: %v", err)
	}
}

func TestWaitReady_CaptureErrorsExhausted(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// All capture attempts fail
	mock.capturePaneSeq = []mockResp{
		{err: fmt.Errorf("tmux error")},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitReady(context.Background(), "%0")
	if err == nil {
		t.Fatal("expected error when all captures fail")
	}
	if !errors.Is(err, ErrCapturePane) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitReady_ContextCancelled(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.capturePaneSeq = []mockResp{
		{val: "not ready"},
	}
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := exec.processManager.waitReady(ctx, "%0")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestWaitReadyStrict_PromptDetected(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.capturePaneSeq = []mockResp{
		{val: "output\n ❯ \n"},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitReadyStrict(context.Background(), "%0")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestWaitReadyStrict_NoPrompt_Fails(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.capturePaneSeq = []mockResp{
		{val: "no prompt ever"},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitReadyStrict(context.Background(), "%0")
	if err == nil {
		t.Fatal("expected error when prompt never detected (strict mode)")
	}
	if !errors.Is(err, ErrPromptNotDetected) {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestWaitReadyStrict_CodexRuntime_ReadyWhenNotShell verifies that for the
// codex runtime (no '❯' prompt glyph), readiness is inferred from the pane's
// current command not being a shell plus rendered content — addressing the
// production failure `wait for claude ready: waitReadyStrict: Claude prompt
// not detected` observed when runtime=codex.
func TestWaitReadyStrict_CodexRuntime_ReadyWhenNotShell(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// @runtime pane var signals codex
	if mock.userVars == nil {
		mock.userVars = make(map[string]string)
	}
	mock.userVars["runtime"] = "codex"
	// Pane is running the codex binary (not a shell).
	mock.currentCommand = "codex"
	mock.isShell = false
	// The TUI has painted at least some content (no '❯' glyph — codex does
	// not use it).
	mock.capturePaneSeq = []mockResp{
		{val: "codex v0.1\nuser> \n"},
	}

	exec, _ := newCovExecutor(mock)

	if err := exec.processManager.waitReadyStrict(context.Background(), "%0"); err != nil {
		t.Fatalf("expected success for codex runtime with running TUI, got: %v", err)
	}
}

// TestWaitReadyStrict_GeminiRuntime_ReadyWhenNotShell mirrors the codex case
// for the gemini runtime.
func TestWaitReadyStrict_GeminiRuntime_ReadyWhenNotShell(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	if mock.userVars == nil {
		mock.userVars = make(map[string]string)
	}
	mock.userVars["runtime"] = "gemini"
	mock.currentCommand = "gemini"
	mock.isShell = false
	mock.capturePaneSeq = []mockResp{
		{val: "Gemini CLI ready\n> \n"},
	}

	exec, _ := newCovExecutor(mock)

	if err := exec.processManager.waitReadyStrict(context.Background(), "%0"); err != nil {
		t.Fatalf("expected success for gemini runtime with running TUI, got: %v", err)
	}
}

// TestWaitReadyStrict_CodexRuntime_FailsWhenShell verifies strict-mode
// failure when the pane is still at a shell (runtime never started) — the
// check must NOT fall back to claude's '❯' glyph and accept any output.
func TestWaitReadyStrict_CodexRuntime_FailsWhenShell(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	if mock.userVars == nil {
		mock.userVars = make(map[string]string)
	}
	mock.userVars["runtime"] = "codex"
	mock.currentCommand = "bash"
	mock.isShell = true
	mock.capturePaneSeq = []mockResp{
		{val: "$ codex\n"},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitReadyStrict(context.Background(), "%0")
	if err == nil {
		t.Fatal("expected ErrPromptNotDetected when codex pane is still at shell")
	}
	if !errors.Is(err, ErrPromptNotDetected) {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestWaitReadyStrict_CodexRuntime_FailsWhenEmpty verifies that a
// non-shell pane with no rendered content is not treated as ready. This
// guards against the edge case where the runtime binary has been exec'd but
// has not painted its TUI yet.
func TestWaitReadyStrict_CodexRuntime_FailsWhenEmpty(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	if mock.userVars == nil {
		mock.userVars = make(map[string]string)
	}
	mock.userVars["runtime"] = "codex"
	mock.currentCommand = "codex"
	mock.isShell = false
	mock.capturePaneSeq = []mockResp{
		{val: "   \n\n   \n"}, // blank content — TUI not painted
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitReadyStrict(context.Background(), "%0")
	if err == nil {
		t.Fatal("expected ErrPromptNotDetected when codex pane is blank")
	}
	if !errors.Is(err, ErrPromptNotDetected) {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestWaitReady_ClaudeRuntime_ExplicitStillUsesPromptGlyph confirms that
// setting @runtime=claude-code explicitly (not relying on the fallback)
// still uses isPromptReady. This protects claude's stricter check from
// accidentally being bypassed by the runtime-dispatch refactor.
func TestWaitReady_ClaudeRuntime_ExplicitStillUsesPromptGlyph(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	if mock.userVars == nil {
		mock.userVars = make(map[string]string)
	}
	mock.userVars["runtime"] = "claude-code"
	// Non-shell process + rendered content WITHOUT '❯' — must NOT be
	// treated as ready for claude-code.
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.capturePaneSeq = []mockResp{
		{val: "Loading..."},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitReadyStrict(context.Background(), "%0")
	if err == nil {
		t.Fatal("expected ErrPromptNotDetected for claude-code without '❯'")
	}
	if !errors.Is(err, ErrPromptNotDetected) {
		t.Errorf("unexpected error: %v", err)
	}
}

// === M5: waitForShell tests ===

func TestWaitForShell_Immediate(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.getCmdSeq = []mockResp{{val: "bash"}}
	mock.isShellSeq = []bool{true}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitForShell(context.Background(), "%0")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestWaitForShell_ShellAfterPolls(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
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

	err := exec.processManager.waitForShell(ctx, "%0")
	if err != nil {
		t.Fatalf("expected success after polls, got: %v", err)
	}
}

func TestWaitForShell_ConsecutiveErrors(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
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

	err := exec.processManager.waitForShell(ctx, "%0")
	if err == nil {
		t.Fatal("expected error for consecutive errors")
	}
	if !errors.Is(err, ErrConsecutiveErrors) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWaitForShell_ErrorRecovery(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
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

	err := exec.processManager.waitForShell(ctx, "%0")
	if err != nil {
		t.Fatalf("expected success after error recovery, got: %v", err)
	}
}

func TestWaitForShell_ContextCancelled(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// Never returns shell → would loop forever without context cancel
	mock.getCmdSeq = []mockResp{{val: "claude"}}
	mock.isShellSeq = []bool{false}

	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := exec.processManager.waitForShell(ctx, "%0")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestWaitForShell_TimeoutViaContext(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	// Never returns shell → context timeout triggers before maxAttempts
	mock.getCmdSeq = []mockResp{{val: "claude"}}
	mock.isShellSeq = []bool{false}

	exec, _ := newCovExecutor(mock)

	// Short timeout to exercise the "shell not found" path without waiting 15s
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := exec.processManager.waitForShell(ctx, "%0")
	if err == nil {
		t.Fatal("expected error when shell not detected")
	}
	// Could be either "cancelled" or "shell not detected" depending on timing
}

// === Additional edge case tests from codex review ===

// M1 supplement: clear_ready=true with VerdictBusy after clear
func TestExecWithClear_ClearReadyTrue_BusyAfterClear(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
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

	// ensureClaudeRunning + busyDetector: not shell → proceed to stages 2/3
	mock.isShell = false
	mock.getCmdSeq = []mockResp{
		{val: "claude"}, // ensureClaudeRunning
		{val: "claude"}, // busyDetector stage 1
		{val: "claude"}, // retry
	}
	mock.isShellSeq = []bool{false, false, false}

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

// M1 supplement: clear_ready=true with persistent VerdictUndecided → soft retry
// fires, but context timeout triggers cancellation during the soft retry sleep,
// causing VerdictUndecided to be returned (not promoted to idle).
func TestExecWithClear_ClearReadyTrue_UndecidedAfterClear(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
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

	mock.isShell = false
	mock.getCmdSeq = []mockResp{
		{val: "claude"}, // ensureClaudeRunning
		{val: "claude"}, // busyDetector
	}
	mock.isShellSeq = []bool{false, false}

	exec, _ := newCovExecutor(mock)
	// Use a short timeout so the context cancels during the soft retry sleep
	// (undecidedSoftRetryInterval = 1s with BusyCheckInterval=0).
	// The initial detectWithUndecidedRetry completes instantly (IdleStableSec=0),
	// then the soft retry sleep of 1s is interrupted by the 500ms timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
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
	t.Parallel()
	mock := newMockPaneIO()
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "99999"
	mock.getCmdSeq = []mockResp{{val: "bash"}}
	mock.isShellSeq = []bool{true}
	mock.capturePaneSeq = []mockResp{{val: "output\n ❯ \n"}}
	exec, _ := newCovExecutor(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := exec.processManager.ensureWorkingDir(ctx, "%0", "/new/dir")
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
	t.Parallel()
	mock := newMockPaneIO()
	// Pre-clear capture fails → preClearHashValid=false
	// Then poll returns stable content 3 times (stricter fallback requires 3 stable polls)
	mock.captureJoinedSeq = []mockResp{
		{err: fmt.Errorf("pre-capture error")}, // pre-clear → hash invalid
		{val: "post-clear"},                    // poll 1
		{val: "post-clear"},                    // poll 2
		{val: "post-clear"},                    // poll 3 → confirmed (3 stable)
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
	t.Parallel()
	mock := newMockPaneIO()
	mock.captureJoinedSeq = []mockResp{
		{val: "stable"},
		{val: "stable"},
	}
	mock.capturePaneSeq = []mockResp{
		{err: fmt.Errorf("prompt capture failed")},
	}

	exec, _ := newCovExecutor(mock)

	// softPromptCheck=false → capture error is fatal
	err := exec.processManager.waitStable(context.Background(), "%0", false)
	if err == nil {
		t.Fatal("expected error when prompt capture fails in hard mode")
	}
	if !errors.Is(err, ErrCapturePane) {
		t.Errorf("unexpected error: %v", err)
	}
}

// M4 supplement: waitReadyStrict capture errors exhausted
func TestWaitReadyStrict_CaptureErrorsExhausted(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.capturePaneSeq = []mockResp{
		{err: fmt.Errorf("err")},
	}

	exec, _ := newCovExecutor(mock)

	err := exec.processManager.waitReadyStrict(context.Background(), "%0")
	if err == nil {
		t.Fatal("expected error when all captures fail in strict mode")
	}
	if !errors.Is(err, ErrCapturePane) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSleepCtx_NormalCompletion(t *testing.T) {
	t.Parallel()
	err := sleepCtx(context.Background(), 1*time.Millisecond)
	if err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
}

func TestSleepCtx_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sleepCtx(ctx, 10*time.Second)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// === ensureClaudeRunning direct tests ===

func TestEnsureClaudeRunning_ClaudeAlreadyRunning(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.getCmdSeq = []mockResp{{val: "node"}}
	mock.isShellSeq = []bool{false}

	exec, _ := newCovExecutor(mock)
	err := exec.processManager.ensureClaudeRunning(context.Background(), "%0", "worker1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should not have sent any re-launch command
	if callsContainLaunchCmd(mock.calls) {
		t.Error("should not re-launch when Claude is already running")
	}
}

func TestEnsureClaudeRunning_ShellDetected_Relaunch(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.getCmdSeq = []mockResp{{val: "bash"}}
	mock.isShellSeq = []bool{true}
	// waitReadyStrict: CapturePane returns prompt-ready
	mock.capturePaneSeq = []mockResp{{val: "output\n ❯ \n"}}

	exec, _ := newCovExecutor(mock)
	err := exec.processManager.ensureClaudeRunning(context.Background(), "%0", "worker1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !callsContainLaunchCmd(mock.calls) {
		t.Error("should re-launch Claude when shell detected")
	}
	// clear_ready should be reset
	if mock.userVars["clear_ready"] != "" {
		t.Errorf("expected clear_ready to be reset, got %q", mock.userVars["clear_ready"])
	}
}

func TestEnsureClaudeRunning_RelaunchFails(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.getCmdSeq = []mockResp{{val: "zsh"}}
	mock.isShellSeq = []bool{true}
	mock.sendCmdErrSeq = []error{fmt.Errorf("tmux send error")}

	exec, _ := newCovExecutor(mock)
	err := exec.processManager.ensureClaudeRunning(context.Background(), "%0", "worker1")
	if err == nil {
		t.Fatal("expected error when re-launch fails")
	}
	if !errors.Is(err, ErrRelaunch) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEnsureClaudeRunning_WaitReadyTimeout(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.getCmdSeq = []mockResp{{val: "bash"}}
	mock.isShellSeq = []bool{true}
	// waitReadyStrict: CapturePane always returns non-prompt content → timeout
	mock.capturePaneSeq = []mockResp{{val: "still loading..."}}

	exec, _ := newCovExecutor(mock)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := exec.processManager.ensureClaudeRunning(ctx, "%0", "worker1")
	if err == nil {
		t.Fatal("expected error when waitReadyStrict times out")
	}
	if !errors.Is(err, ErrWaitClaudeReady) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEnsureClaudeRunning_GetCmdError_ReturnsError(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.getCmdSeq = []mockResp{{err: fmt.Errorf("tmux error")}}

	exec, _ := newCovExecutor(mock)
	err := exec.processManager.ensureClaudeRunning(context.Background(), "%0", "worker1")
	if err == nil {
		t.Fatal("expected error when GetPaneCurrentCommand fails")
	}
	if !errors.Is(err, ErrCheckPaneCommand) {
		t.Errorf("expected ErrCheckPaneCommand in error chain, got: %v", err)
	}
	// Should not have attempted re-launch
	if callsContainLaunchCmd(mock.calls) {
		t.Error("should not re-launch on GetPaneCurrentCommand error")
	}
}
