package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClassifyError_SessionEquivalentMessages(t *testing.T) {
	t.Parallel()
	cases := []string{
		"session not found",
		"can't find session: maestro-foo",
		"no such session",
		"no sessions",
		// Newly-classified messages: tmux returns these when a target-bearing
		// command runs against an implicit/empty target (e.g. kill-session
		// after the session is already gone). Treating them as
		// session-equivalent keeps idempotent callers like KillSession from
		// reporting cleanup failure.
		"no current target",
		"no current client",
		"no current session",
	}
	for _, stderr := range cases {
		t.Run(stderr, func(t *testing.T) {
			t.Parallel()
			cls := classifyError("kill-session", stderr, fmt.Errorf("exit status 1"))
			if cls.Kind != ErrKindSession {
				t.Fatalf("classifyError(%q) kind = %v, want ErrKindSession", stderr, cls.Kind)
			}
			if !errors.Is(cls, ErrTmuxSession) {
				t.Fatalf("classifyError(%q) should match ErrTmuxSession via errors.Is", stderr)
			}
		})
	}
}

// testSessionSeq provides unique suffixes for test session names.
var testSessionSeq atomic.Int64

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found, skipping")
	}
	// Verify tmux server is accessible (not just installed)
	out, err := exec.Command("tmux", "list-sessions").CombinedOutput()
	if err != nil {
		outStr := string(out)
		// "no server running" and "error connecting ... (No such file or
		// directory)" are both expected when no server has started yet —
		// tmux will start one on CreateSession. Only permission and other
		// connectivity errors mean tmux is genuinely unusable.
		if strings.Contains(outStr, "Operation not permitted") ||
			strings.Contains(outStr, "Permission denied") ||
			(strings.Contains(outStr, "error connecting") &&
				!strings.Contains(outStr, "No such file or directory")) {
			t.Skipf("tmux server not accessible: %s", strings.TrimSpace(outStr))
		}
	}
}

// useTestSession sets a unique, isolated session name for the test.
// It saves the original session name and restores it on cleanup.
// The cleanup also kills the test session by its captured name,
// avoiding the bug where GetSessionName() at cleanup time returns
// a different test's session name.
func useTestSession(t *testing.T) string {
	t.Helper()

	origName := GetSessionName()
	testName := fmt.Sprintf("maestro-test-%d-%d", time.Now().UnixNano(), testSessionSeq.Add(1))
	SetSessionName(testName)

	// Capture the concrete name for cleanup — do NOT call GetSessionName() later.
	capturedName := GetSessionName()
	t.Cleanup(func() {
		exec.Command("tmux", "kill-session", "-t", capturedName).Run()
		SetSessionName(origName)
	})

	return capturedName
}

// TestBuildMaestroSessionName_HumanReadable pins the attach-UX contract:
// the session name is exactly "maestro-<projectName>" so operators can
// `tmux attach -t maestro-<projectName>` once the per-instance socket is
// selected (see `maestro attach`). Per-checkout collision is resolved at
// the socket layer, not via a name suffix.
func TestBuildMaestroSessionName_HumanReadable(t *testing.T) {
	if got := BuildMaestroSessionName("proj"); got != "maestro-proj" {
		t.Fatalf("session name = %q, want %q", got, "maestro-proj")
	}
	if got := BuildMaestroSessionName("maestro_v2"); got != "maestro-maestro_v2" {
		t.Fatalf("session name = %q, want %q", got, "maestro-maestro_v2")
	}
}

// TestBuildMaestroSocketNameResolvesSymlinks pins that the canonicalisation
// step inside BuildMaestroSocketName resolves symlinks, so /tmp/... and
// /private/tmp/... on macOS land on the same per-instance tmux server.
// Without this, two `maestro` invocations from the same checkout via
// different path forms would spawn two tmux servers and lose isolation.
func TestBuildMaestroSocketNameResolvesSymlinks(t *testing.T) {
	realDir := t.TempDir()
	linkParent := t.TempDir()
	linkDir := filepath.Join(linkParent, "maestro-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	fromReal := BuildMaestroSocketName("proj", realDir)
	fromLink := BuildMaestroSocketName("proj", linkDir)
	if fromReal != fromLink {
		t.Fatalf("socket name mismatch for real and symlink paths: real=%q link=%q", fromReal, fromLink)
	}
}

// TestBuildMaestroSocketName_StableAndPerProject pins post-2026-05-06
// P0: per-instance socket isolation derives a stable, per-project socket
// name so two checkouts of the same project (or two different projects)
// get distinct tmux servers, eliminating cross-instance interference
// (SESSION_LOST race, autoAcceptTrustDialog hijack, etc.).
func TestBuildMaestroSocketName_StableAndPerProject(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	a1 := BuildMaestroSocketName("proj-a", dir1)
	a2 := BuildMaestroSocketName("proj-a", dir1)
	if a1 != a2 {
		t.Errorf("same project+dir should produce stable socket: %q vs %q", a1, a2)
	}

	b := BuildMaestroSocketName("proj-b", dir1)
	if a1 == b {
		t.Errorf("different project should produce different socket: both %q", a1)
	}

	a1d2 := BuildMaestroSocketName("proj-a", dir2)
	if a1 == a1d2 {
		t.Errorf("different dir should produce different socket: both %q", a1)
	}
}

// TestTmuxArgs_PrependsSocketFlag pins that tmuxArgs prepends `-L <socket>`
// to the argv when a socket is set, and is a no-op when unset. This is
// the fan-in for instance isolation; missing it means a particular call
// site bypasses the per-instance socket and reaches the default tmux
// server, recreating the exact race the change is meant to fix.
func TestTmuxArgs_PrependsSocketFlag(t *testing.T) {
	orig := GetTmuxSocket()
	t.Cleanup(func() { SetTmuxSocket(orig) })

	SetTmuxSocket("")
	if got := tmuxArgs([]string{"has-session"}); len(got) != 1 || got[0] != "has-session" {
		t.Errorf("empty socket should pass-through, got %v", got)
	}

	SetTmuxSocket("maestro-test")
	got := tmuxArgs([]string{"has-session", "-t", "x"})
	want := []string{"-L", "maestro-test", "has-session", "-t", "x"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestSetTmuxSocket_Sanitizes pins that the socket name sanitizer
// strips unsafe characters (matching the session name policy) so the
// socket name is safe to embed in `tmux -L <socket>` and the resulting
// socket file path.
func TestSetTmuxSocket_Sanitizes(t *testing.T) {
	orig := GetTmuxSocket()
	t.Cleanup(func() { SetTmuxSocket(orig) })

	SetTmuxSocket("foo/bar:baz qux")
	got := GetTmuxSocket()
	if got != "foo_bar_baz_qux" {
		t.Errorf("expected sanitized socket name, got %q", got)
	}

	SetTmuxSocket("")
	if GetTmuxSocket() != "" {
		t.Errorf("empty socket should disable -L flag, got %q", GetTmuxSocket())
	}
}

func TestSessionLifecycle(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if SessionExists() {
		t.Fatal("session should not exist initially")
	}

	if _, err := CreateSessionWithServerOptions("test-window", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if !SessionExists() {
		t.Fatal("session should exist after creation")
	}

	if err := KillSession(); err != nil {
		t.Fatalf("kill session: %v", err)
	}

	if SessionExists() {
		t.Fatal("session should not exist after kill")
	}
}

func TestUserVariables(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if _, err := CreateSessionWithServerOptions("test", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := "=" + GetSessionName() + ":0.0"

	// Set user variables
	vars := map[string]string{
		"agent_id": "orchestrator",
		"role":     "orchestrator",
		"model":    "opus",
		"status":   "idle",
	}
	for k, v := range vars {
		if err := SetUserVar(paneTarget, k, v); err != nil {
			t.Fatalf("set @%s: %v", k, err)
		}
	}

	// Read back and verify
	for k, want := range vars {
		got, err := GetUserVar(paneTarget, k)
		if err != nil {
			t.Fatalf("get @%s: %v", k, err)
		}
		if got != want {
			t.Errorf("@%s: got %q, want %q", k, got, want)
		}
	}
}

func TestCreateWindowAndListPanes(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if _, err := CreateSessionWithServerOptions("orchestrator", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := CreateWindow("planner"); err != nil {
		t.Fatalf("create window: %v", err)
	}

	// List all panes across session
	panes, err := ListAllPanes("#{window_name}")
	if err != nil {
		t.Fatalf("list all panes: %v", err)
	}

	windowNames := make(map[string]bool)
	for _, p := range panes {
		windowNames[strings.TrimSpace(p)] = true
	}

	if !windowNames["orchestrator"] {
		t.Error("expected orchestrator window")
	}
	if !windowNames["planner"] {
		t.Error("expected planner window")
	}
}

func TestSetupWorkerGrid(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if _, err := CreateSessionWithServerOptions("orchestrator", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := CreateWindow("workers"); err != nil {
		t.Fatalf("create window: %v", err)
	}

	workerWindow := "=" + GetSessionName() + ":workers"
	panes, err := SetupWorkerGrid(workerWindow, 4)
	if err != nil {
		t.Fatalf("setup worker grid: %v", err)
	}

	if len(panes) != 4 {
		t.Fatalf("expected 4 panes, got %d", len(panes))
	}

	// Verify row-major order: top-left, top-right, bottom-left, bottom-right
	type pos struct{ top, left string }
	positions := make([]pos, len(panes))
	for i, pane := range panes {
		topStr, err := output("display-message", "-t", pane, "-p", "#{pane_top}")
		if err != nil {
			t.Fatalf("get pane_top for %s: %v", pane, err)
		}
		leftStr, err := output("display-message", "-t", pane, "-p", "#{pane_left}")
		if err != nil {
			t.Fatalf("get pane_left for %s: %v", pane, err)
		}
		positions[i] = pos{
			top:  strings.TrimSpace(topStr),
			left: strings.TrimSpace(leftStr),
		}
	}

	// Convert positions to integers for correct numeric comparison
	toInt := func(s string) int {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("failed to parse position %q as integer: %v", s, err)
		}
		return n
	}

	// pane 0 (top-left) should be above pane 2 (bottom-left)
	if toInt(positions[0].top) >= toInt(positions[2].top) {
		t.Errorf("pane 0 top (%s) should be less than pane 2 top (%s)", positions[0].top, positions[2].top)
	}
	// pane 0 (top-left) should be left of pane 1 (top-right)
	if toInt(positions[0].left) >= toInt(positions[1].left) {
		t.Errorf("pane 0 left (%s) should be less than pane 1 left (%s)", positions[0].left, positions[1].left)
	}
	// pane 1 (top-right) should be above pane 3 (bottom-right)
	if toInt(positions[1].top) >= toInt(positions[3].top) {
		t.Errorf("pane 1 top (%s) should be less than pane 3 top (%s)", positions[1].top, positions[3].top)
	}
	// pane 2 (bottom-left) should be left of pane 3 (bottom-right)
	if toInt(positions[2].left) >= toInt(positions[3].left) {
		t.Errorf("pane 2 left (%s) should be less than pane 3 left (%s)", positions[2].left, positions[3].left)
	}
}

func TestCapturePane(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if _, err := CreateSessionWithServerOptions("test", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := "=" + GetSessionName() + ":0.0"
	waitForShell(t, paneTarget)

	// Send a marker string via echo so we can verify CapturePane content
	marker := "CAPTURE_TEST_MARKER_12345"
	if err := SendCommand(paneTarget, "echo "+marker); err != nil {
		t.Fatalf("send echo command: %v", err)
	}
	var content string
	require.Eventually(t, func() bool {
		var err error
		content, err = CapturePane(paneTarget, 10)
		return err == nil && strings.Contains(content, marker)
	}, 5*time.Second, 100*time.Millisecond, "pane should contain marker %q", marker)
}

func TestFindPaneByAgentID(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if _, err := CreateSessionWithServerOptions("test", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := "=" + GetSessionName() + ":0.0"
	SetUserVar(paneTarget, "agent_id", "test-agent")

	found, err := FindPaneByAgentID("test-agent")
	if err != nil {
		t.Fatalf("find pane: %v", err)
	}
	// FindPaneByAgentID returns the immutable pane ID (%N), not the
	// session:window.pane form — pane indices are renumbered when panes
	// close, which would retarget in-flight sends (see its doc comment).
	wantID, err := output("display-message", "-t", paneTarget, "-p", "#{pane_id}")
	if err != nil {
		t.Fatalf("resolve expected pane id: %v", err)
	}
	if want := strings.TrimSpace(wantID); found != want {
		t.Errorf("got %q, want %q", found, want)
	}

	// Non-existent agent
	_, err = FindPaneByAgentID("nonexistent")
	if err == nil {
		t.Error("expected error for non-existent agent")
	}
}

// waitForShell waits until the pane is running an interactive shell and
// accepting input. Prompt-glyph detection (looking for $ / > / %) is
// deliberately avoided: customised prompts (powerline, p10k, starship)
// often contain none of those characters, which made every capture-based
// test fail on such machines. This mirrors the production readiness probe
// (waitForShellReady): wait for pane_current_command to become a known
// shell, then confirm interactivity with a sentinel echo.
func waitForShell(t *testing.T, paneTarget string) {
	t.Helper()
	require.Eventually(t, func() bool {
		cmd, err := GetPaneCurrentCommand(paneTarget)
		return err == nil && IsShellCommand(cmd)
	}, 5*time.Second, 100*time.Millisecond, "shell not running in pane %s", paneTarget)

	// Confirm the shell is accepting interactive input with a sentinel echo.
	// The sentinel is resent periodically: a shell whose rc init is still
	// running can swallow the first send — the exact race production's
	// confirmShellInteractive handles (fail-open there; tests fail closed).
	sentinel := fmt.Sprintf("__TMUXTEST_RDY_%d_%d__", time.Now().UnixNano(), testSessionSeq.Add(1))
	deadline := time.Now().Add(10 * time.Second)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		if err := SendCommand(paneTarget, "echo "+sentinel); err != nil {
			t.Logf("waitForShell: sentinel send attempt %d to %s failed: %v", attempt, paneTarget, err)
		}
		settle := time.Now().Add(1 * time.Second)
		for time.Now().Before(settle) {
			// Joined capture: narrow panes (e.g. a 4-way split) wrap the
			// sentinel across visual lines, which a non-joined capture
			// would never match as a contiguous string.
			content, err := CapturePaneJoined(paneTarget, 15)
			if err == nil && strings.Contains(content, sentinel) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Fatalf("readiness sentinel not echoed in pane %s", paneTarget)
}

// TestBufNamePID_IncludesProcessPID pins the cross-process tmux buffer
// uniqueness invariant. tmux buffers live on the tmux server, NOT inside
// the maestro process, so two daemons sharing the same tmux server (one
// per formation) would otherwise both emit `maestro-msg-1`, race on
// `tmux load-buffer -b maestro-msg-1`, and silently overwrite each other's
// payload before paste-buffer fired. The 2026-04 audit reproduced this:
// a gemini-formation Planner pasted a codex-formation command because
// codex's load-buffer landed in the same buffer slot just before
// gemini's paste-buffer ran. The PID prefix makes the buffer name
// globally unique across daemons on the same host.
func TestBufNamePID_IncludesProcessPID(t *testing.T) {
	t.Parallel()
	if bufNamePID != os.Getpid() {
		t.Errorf("bufNamePID = %d, want os.Getpid() = %d (PID prefix is the cross-daemon collision guard)",
			bufNamePID, os.Getpid())
	}
}

func TestSendTextAndSubmit(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if _, err := CreateSessionWithServerOptions("test", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := "=" + GetSessionName() + ":0.0"
	waitForShell(t, paneTarget)

	// Start cat so we can verify text is submitted (cat echoes stdin to stdout)
	if err := SendCommand(paneTarget, "cat"); err != nil {
		t.Fatalf("start cat: %v", err)
	}
	// Poll until cat command is running (instead of fixed sleep)
	waitForCommand(t, paneTarget, "cat", 5*time.Second)

	multiLine := "line1\nline2\nline3"
	if err := SendTextAndSubmit(context.Background(), paneTarget, multiLine); err != nil {
		t.Fatalf("SendTextAndSubmit: %v", err)
	}

	wants := []string{"line1", "line2", "line3"}
	var content string
	require.Eventually(t, func() bool {
		var err error
		content, err = CapturePane(paneTarget, 20)
		if err != nil {
			return false
		}
		for _, w := range wants {
			if !strings.Contains(content, w) {
				return false
			}
		}
		return true
	}, 5*time.Second, 100*time.Millisecond, "pane should contain all expected lines")
	t.Logf("pane content:\n%s", content)
}

func TestSendTextAndSubmit_SingleLine(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if _, err := CreateSessionWithServerOptions("test", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := "=" + GetSessionName() + ":0.0"
	waitForShell(t, paneTarget)

	if err := SendCommand(paneTarget, "cat"); err != nil {
		t.Fatalf("start cat: %v", err)
	}
	// Poll until cat command is running (instead of fixed sleep)
	waitForCommand(t, paneTarget, "cat", 5*time.Second)

	if err := SendTextAndSubmit(context.Background(), paneTarget, "hello world"); err != nil {
		t.Fatalf("SendTextAndSubmit: %v", err)
	}

	var content string
	require.Eventually(t, func() bool {
		var err error
		content, err = CapturePane(paneTarget, 10)
		return err == nil && strings.Contains(content, "hello world")
	}, 5*time.Second, 100*time.Millisecond, "pane should contain 'hello world'")
	t.Logf("pane content:\n%s", content)
}

func TestSetupWorkerGrid_InvalidCount(t *testing.T) {
	_, err := SetupWorkerGrid("dummy", 0)
	if err == nil {
		t.Fatal("expected error for count 0")
	}
	if !strings.Contains(err.Error(), "1-8") {
		t.Errorf("error for count 0 should mention valid range 1-8, got: %v", err)
	}
	if !strings.Contains(err.Error(), "0") {
		t.Errorf("error for count 0 should mention the invalid value, got: %v", err)
	}

	_, err = SetupWorkerGrid("dummy", 9)
	if err == nil {
		t.Fatal("expected error for count 9")
	}
	if !strings.Contains(err.Error(), "1-8") {
		t.Errorf("error for count 9 should mention valid range 1-8, got: %v", err)
	}
	if !strings.Contains(err.Error(), "9") {
		t.Errorf("error for count 9 should mention the invalid value, got: %v", err)
	}
}

func TestSetSessionOption(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if _, err := CreateSessionWithServerOptions("test", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// SetSessionOption uses session name without "=" prefix due to tmux 3.6
	// set-option limitation. destroy-unattached is session-scoped.
	if err := SetSessionOption("destroy-unattached", "off"); err != nil {
		t.Fatalf("set destroy-unattached: %v", err)
	}

	// Verify destroy-unattached was set
	out, err := exec.Command("tmux", "show-options", "-t", GetSessionName(), "destroy-unattached").CombinedOutput()
	if err != nil {
		t.Fatalf("show-options: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "off") {
		t.Errorf("expected destroy-unattached off, got %s", strings.TrimSpace(string(out)))
	}
}

// TestDebugLevelForErrorKind pins the 2026-04-28 retest4 fix that
// demotes tmux runCtx debugLog prefix from "ERROR" to "DEBUG" when the
// classified error is an idempotent-cleanup case the caller explicitly
// handles. Without this, tmux_debug.log surfaced lines like:
//
//	runCtx ERROR args=[kill-session ...] kind=session stderr="can't find session"
//
// for `maestro up`'s pre-cleanup of a non-existent session — confusing
// log readers because the ERROR prefix was louder than the actual
// outcome (cleanup was a no-op, exactly as designed).
func TestDebugLevelForErrorKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind ErrorKind
		want string
	}{
		{ErrKindSession, "DEBUG"}, // can't find session — idempotent kill
		{ErrKindServer, "DEBUG"},  // no server running — idempotent restore
		{ErrKindPane, "ERROR"},    // pane not found — usually a real bug
		{ErrKindTimeout, "ERROR"}, // genuine slowness, must surface
		{ErrKindCommand, "ERROR"}, // catch-all for tmux failures
		{ErrKindCanceled, "ERROR"},
	}
	for _, tc := range cases {
		if got := debugLevelForErrorKind(tc.kind); got != tc.want {
			t.Errorf("debugLevelForErrorKind(%v) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// TestClassifyError_PermissionDistinctFromServer pins that permission
// failures (sandbox denying tmux socket access) are classified as
// ErrKindPermission, NOT as ErrKindServer. KillSession treats
// ErrKindServer/ErrKindSession as idempotent success ("already gone");
// folding permission errors into ErrKindServer made KillSession report
// success while the session was still alive behind the denied socket.
func TestClassifyError_PermissionDistinctFromServer(t *testing.T) {
	t.Parallel()
	cases := []string{
		"operation not permitted",
		"error connecting to /private/tmp/tmux-501/default (Operation not permitted)",
		"permission denied",
	}
	for _, stderr := range cases {
		t.Run(stderr, func(t *testing.T) {
			t.Parallel()
			cls := classifyError("kill-session", stderr, fmt.Errorf("exit status 1"))
			if cls.Kind != ErrKindPermission {
				t.Fatalf("classifyError(%q) kind = %v, want ErrKindPermission", stderr, cls.Kind)
			}
			if !errors.Is(cls, ErrTmuxPermission) {
				t.Fatalf("classifyError(%q) should match ErrTmuxPermission via errors.Is", stderr)
			}
			if errors.Is(cls, ErrTmuxServer) {
				t.Fatalf("classifyError(%q) must NOT match ErrTmuxServer (KillSession would treat it as idempotent success)", stderr)
			}
			if errors.Is(cls, ErrTmuxSession) {
				t.Fatalf("classifyError(%q) must NOT match ErrTmuxSession", stderr)
			}
		})
	}
}

// TestSessionExistsChecked pins the fail-closed session guard: absence of
// the session (or server) is (false, nil), while indeterminate failures
// must surface as an error so callers like RunUp do not destroy a
// possibly-live formation.
func TestSessionExistsChecked(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	exists, err := SessionExistsChecked()
	if err != nil {
		t.Fatalf("SessionExistsChecked before create: unexpected error: %v", err)
	}
	if exists {
		t.Fatal("session should not exist before creation")
	}

	if _, err := CreateSessionWithServerOptions("test", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	exists, err = SessionExistsChecked()
	if err != nil {
		t.Fatalf("SessionExistsChecked after create: unexpected error: %v", err)
	}
	if !exists {
		t.Fatal("session should exist after creation")
	}
}

// TestOutput_ErrorCarriesStderr pins that outputCtx extracts stderr from
// ExitError after the switch from CombinedOutput to Output: without the
// extraction, classifyError would receive an empty string and misclassify
// every failure as ErrKindCommand, breaking the idempotent-cleanup and
// pane-missing code paths that depend on error kinds.
func TestOutput_ErrorCarriesStderr(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if _, err := CreateSessionWithServerOptions("test", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	_, err := CapturePane("="+GetSessionName()+":nonexistent-window-xyz", 0)
	if err == nil {
		t.Fatal("expected error for nonexistent window target")
	}
	var terr *Error
	if !errors.As(err, &terr) {
		t.Fatalf("expected *tmux.Error, got %T: %v", err, err)
	}
	if terr.Stderr == "" {
		t.Fatal("Error.Stderr must carry tmux stderr (ExitError.Stderr extraction)")
	}
	if terr.Kind != ErrKindPane {
		t.Fatalf("kind = %v, want ErrKindPane (stderr-driven classification)", terr.Kind)
	}
}

// TestSendCommand_LeadingDashLiteral pins the "--" terminator in
// SendCommand: a command starting with "-" must be delivered literally
// instead of being parsed as a send-keys flag (which previously failed
// the send outright with "unknown flag").
func TestSendCommand_LeadingDashLiteral(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if _, err := CreateSessionWithServerOptions("test", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := "=" + GetSessionName() + ":0.0"
	waitForShell(t, paneTarget)

	if err := SendCommand(paneTarget, "-n leading-dash-marker"); err != nil {
		t.Fatalf("SendCommand with leading dash: %v", err)
	}
	require.Eventually(t, func() bool {
		content, err := CapturePane(paneTarget, 10)
		return err == nil && strings.Contains(content, "-n leading-dash-marker")
	}, 5*time.Second, 100*time.Millisecond, "pane should contain the literal leading-dash command")
}

// TestFormationPrimitives_NonZeroBaseIndex is the regression test for the
// base-index bug: with `set -g base-index 1` and `set -g pane-base-index 1`
// in tmux.conf (which per-instance sockets still read), the first window of
// a new session is index 1, so any hardcoded ":0" target fails with
// "no such window". The formation flow therefore addresses windows/panes
// exclusively via the IDs returned at creation time; this test runs those
// primitives against a dedicated server whose config shifts both indices.
func TestFormationPrimitives_NonZeroBaseIndex(t *testing.T) {
	requireTmux(t)

	confPath := filepath.Join(t.TempDir(), "tmux.conf")
	if err := os.WriteFile(confPath, []byte("set -g base-index 1\nset -g pane-base-index 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sock := fmt.Sprintf("maestro-test-bi-%d-%d", os.Getpid(), testSessionSeq.Add(1))
	// Seed session: starts the dedicated server with the custom config and
	// keeps it alive (default exit-empty would stop an empty server).
	seed := exec.Command("tmux", "-L", sock, "-f", confPath, "new-session", "-d", "-s", "seed")
	if out, err := seed.CombinedOutput(); err != nil {
		t.Skipf("cannot start dedicated tmux server: %v: %s", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	})

	origSocket := GetTmuxSocket()
	origName := GetSessionName()
	SetTmuxSocket(sock)
	SetSessionName(fmt.Sprintf("maestro-test-bi-%d", testSessionSeq.Add(1)))
	t.Cleanup(func() {
		SetTmuxSocket(origSocket)
		SetSessionName(origName)
	})

	orchWindow, err := CreateSessionWithServerOptions("orchestrator", map[string]string{
		"exit-empty": "off",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if !strings.HasPrefix(orchWindow, "@") {
		t.Fatalf("expected window ID (@N), got %q", orchWindow)
	}

	idx, err := output("display-message", "-t", orchWindow, "-p", "#{window_index}")
	if err != nil {
		t.Fatalf("query window index: %v", err)
	}
	if strings.TrimSpace(idx) != "1" {
		t.Fatalf("initial window index = %q, want 1 (base-index config not effective; test setup broken)", strings.TrimSpace(idx))
	}

	// Window options must be settable via the returned ID.
	if err := SetWindowOption(orchWindow, "remain-on-exit", "on"); err != nil {
		t.Fatalf("SetWindowOption on window ID: %v", err)
	}

	// The active pane must resolve through the window ID and accept user vars.
	orchPane, err := WindowActivePane(orchWindow)
	if err != nil {
		t.Fatalf("WindowActivePane: %v", err)
	}
	if !strings.HasPrefix(orchPane, "%") {
		t.Fatalf("expected pane ID (%%N), got %q", orchPane)
	}
	if err := SetUserVar(orchPane, "agent_id", "orchestrator"); err != nil {
		t.Fatalf("SetUserVar on pane ID: %v", err)
	}
	got, err := GetUserVar(orchPane, "agent_id")
	if err != nil {
		t.Fatalf("GetUserVar: %v", err)
	}
	if got != "orchestrator" {
		t.Fatalf("agent_id = %q, want orchestrator", got)
	}

	// CreateWindow must also return a usable ID under base-index 1.
	workersWindow, err := CreateWindow("workers")
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}
	if !strings.HasPrefix(workersWindow, "@") {
		t.Fatalf("expected window ID (@N), got %q", workersWindow)
	}

	panes, err := SetupWorkerGrid(workersWindow, 2)
	if err != nil {
		t.Fatalf("SetupWorkerGrid under pane-base-index 1: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("expected 2 worker panes, got %d", len(panes))
	}
	for _, pane := range panes {
		if _, err := output("display-message", "-t", pane, "-p", "#{pane_id}"); err != nil {
			t.Fatalf("worker pane target %q not addressable: %v", pane, err)
		}
	}

	if err := SelectWindow(orchWindow); err != nil {
		t.Fatalf("SelectWindow on window ID: %v", err)
	}
}
