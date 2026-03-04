package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
		// "no server running" is expected — tmux will start on CreateSession.
		// But connectivity/permission errors mean tmux is unusable.
		if strings.Contains(outStr, "error connecting") ||
			strings.Contains(outStr, "Operation not permitted") ||
			strings.Contains(outStr, "Permission denied") {
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

func TestSessionLifecycle(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if SessionExists() {
		t.Fatal("session should not exist initially")
	}

	if err := CreateSession("test-window"); err != nil {
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

	if err := CreateSession("test"); err != nil {
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

	if err := CreateSession("orchestrator"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := CreateWindow("planner"); err != nil {
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

	if err := CreateSession("orchestrator"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := CreateWindow("workers"); err != nil {
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

	if err := CreateSession("test"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := "=" + GetSessionName() + ":0.0"
	waitForShell(t, paneTarget)

	// Send a marker string via echo so we can verify CapturePane content
	marker := "CAPTURE_TEST_MARKER_12345"
	if err := SendCommand(paneTarget, "echo "+marker); err != nil {
		t.Fatalf("send echo command: %v", err)
	}
	time.Sleep(1 * time.Second)

	content, err := CapturePane(paneTarget, 10)
	if err != nil {
		t.Fatalf("capture pane: %v", err)
	}
	if !strings.Contains(content, marker) {
		t.Errorf("CapturePane content does not contain marker %q\ncontent:\n%s", marker, content)
	}
}

func TestFindPaneByAgentID(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if err := CreateSession("test"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := "=" + GetSessionName() + ":0.0"
	SetUserVar(paneTarget, "agent_id", "test-agent")

	found, err := FindPaneByAgentID("test-agent")
	if err != nil {
		t.Fatalf("find pane: %v", err)
	}
	// FindPaneByAgentID returns the raw pane target from tmux (without "=" prefix)
	// because tmux's #{session_name} format returns the actual session name.
	wantRaw := GetSessionName() + ":0.0"
	if found != wantRaw {
		t.Errorf("got %q, want %q", found, wantRaw)
	}

	// Non-existent agent
	_, err = FindPaneByAgentID("nonexistent")
	if err == nil {
		t.Error("expected error for non-existent agent")
	}
}

// waitForShell waits until the shell is ready by polling CapturePane for the prompt.
func waitForShell(t *testing.T, paneTarget string) {
	t.Helper()
	for i := 0; i < 20; i++ {
		time.Sleep(250 * time.Millisecond)
		content, err := CapturePane(paneTarget, 5)
		if err != nil {
			continue
		}
		// Fish/zsh/bash shell prompts typically contain $ or > or %
		if strings.Contains(content, "$") || strings.Contains(content, ">") || strings.Contains(content, "%") {
			return
		}
	}
	t.Log("shell prompt not detected; proceeding anyway")
}

func TestSendTextAndSubmit(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if err := CreateSession("test"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := "=" + GetSessionName() + ":0.0"
	waitForShell(t, paneTarget)

	// Start cat so we can verify text is submitted (cat echoes stdin to stdout)
	if err := SendCommand(paneTarget, "cat"); err != nil {
		t.Fatalf("start cat: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	multiLine := "line1\nline2\nline3"
	if err := SendTextAndSubmit(context.Background(), paneTarget, multiLine); err != nil {
		t.Fatalf("SendTextAndSubmit: %v", err)
	}

	time.Sleep(1 * time.Second)

	content, err := CapturePane(paneTarget, 20)
	if err != nil {
		t.Fatalf("capture pane: %v", err)
	}

	for _, want := range []string{"line1", "line2", "line3"} {
		if !strings.Contains(content, want) {
			t.Errorf("pane content missing %q\ncontent:\n%s", want, content)
		}
	}
	t.Logf("pane content:\n%s", content)
}

func TestSendTextAndSubmit_SingleLine(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if err := CreateSession("test"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := "=" + GetSessionName() + ":0.0"
	waitForShell(t, paneTarget)

	if err := SendCommand(paneTarget, "cat"); err != nil {
		t.Fatalf("start cat: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	if err := SendTextAndSubmit(context.Background(), paneTarget, "hello world"); err != nil {
		t.Fatalf("SendTextAndSubmit: %v", err)
	}

	time.Sleep(1 * time.Second)

	content, err := CapturePane(paneTarget, 10)
	if err != nil {
		t.Fatalf("capture pane: %v", err)
	}

	if !strings.Contains(content, "hello world") {
		t.Errorf("pane content missing 'hello world'\ncontent:\n%s", content)
	}
	t.Logf("pane content:\n%s", content)
}

func TestSetupWorkerGrid_InvalidCount(t *testing.T) {
	_, err := SetupWorkerGrid("dummy", 0)
	if err == nil {
		t.Error("expected error for count 0")
	}
	_, err = SetupWorkerGrid("dummy", 9)
	if err == nil {
		t.Error("expected error for count 9")
	}
}

func TestSetSessionOption(t *testing.T) {
	requireTmux(t)
	useTestSession(t)

	if err := CreateSession("test"); err != nil {
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
