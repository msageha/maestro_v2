package tmux

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

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

func cleanupSession(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		// Best-effort cleanup
		exec.Command("tmux", "kill-session", "-t", SessionName).Run()
	})
}

func TestSessionLifecycle(t *testing.T) {
	requireTmux(t)
	cleanupSession(t)

	// Kill any existing test session
	exec.Command("tmux", "kill-session", "-t", SessionName).Run()

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
	cleanupSession(t)
	exec.Command("tmux", "kill-session", "-t", SessionName).Run()

	if err := CreateSession("test"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := SessionName + ":0.0"

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
	cleanupSession(t)
	exec.Command("tmux", "kill-session", "-t", SessionName).Run()

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
	cleanupSession(t)
	exec.Command("tmux", "kill-session", "-t", SessionName).Run()

	if err := CreateSession("orchestrator"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := CreateWindow("workers"); err != nil {
		t.Fatalf("create window: %v", err)
	}

	workerWindow := SessionName + ":workers"
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

	// pane 0 (top-left) should be above pane 2 (bottom-left)
	if positions[0].top >= positions[2].top {
		t.Errorf("pane 0 top (%s) should be less than pane 2 top (%s)", positions[0].top, positions[2].top)
	}
	// pane 0 (top-left) should be left of pane 1 (top-right)
	if positions[0].left >= positions[1].left {
		t.Errorf("pane 0 left (%s) should be less than pane 1 left (%s)", positions[0].left, positions[1].left)
	}
	// pane 1 (top-right) should be above pane 3 (bottom-right)
	if positions[1].top >= positions[3].top {
		t.Errorf("pane 1 top (%s) should be less than pane 3 top (%s)", positions[1].top, positions[3].top)
	}
	// pane 2 (bottom-left) should be left of pane 3 (bottom-right)
	if positions[2].left >= positions[3].left {
		t.Errorf("pane 2 left (%s) should be less than pane 3 left (%s)", positions[2].left, positions[3].left)
	}
}

func TestCapturePane(t *testing.T) {
	requireTmux(t)
	cleanupSession(t)
	exec.Command("tmux", "kill-session", "-t", SessionName).Run()

	if err := CreateSession("test"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := SessionName + ":0.0"

	// Capture should succeed even if pane is empty
	content, err := CapturePane(paneTarget, 3)
	if err != nil {
		t.Fatalf("capture pane: %v", err)
	}
	// Content may be empty or contain a shell prompt
	_ = content
}

func TestFindPaneByAgentID(t *testing.T) {
	requireTmux(t)
	cleanupSession(t)
	exec.Command("tmux", "kill-session", "-t", SessionName).Run()

	if err := CreateSession("test"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := SessionName + ":0.0"
	SetUserVar(paneTarget, "agent_id", "test-agent")

	found, err := FindPaneByAgentID("test-agent")
	if err != nil {
		t.Fatalf("find pane: %v", err)
	}
	if found != paneTarget {
		t.Errorf("got %q, want %q", found, paneTarget)
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
	cleanupSession(t)
	exec.Command("tmux", "kill-session", "-t", SessionName).Run()

	if err := CreateSession("test"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := SessionName + ":0.0"
	waitForShell(t, paneTarget)

	// Start cat so we can verify text is submitted (cat echoes stdin to stdout)
	if err := SendCommand(paneTarget, "cat"); err != nil {
		t.Fatalf("start cat: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	multiLine := "line1\nline2\nline3"
	if err := SendTextAndSubmit(paneTarget, multiLine); err != nil {
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
	cleanupSession(t)
	exec.Command("tmux", "kill-session", "-t", SessionName).Run()

	if err := CreateSession("test"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	paneTarget := SessionName + ":0.0"
	waitForShell(t, paneTarget)

	if err := SendCommand(paneTarget, "cat"); err != nil {
		t.Fatalf("start cat: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	if err := SendTextAndSubmit(paneTarget, "hello world"); err != nil {
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
	cleanupSession(t)
	exec.Command("tmux", "kill-session", "-t", SessionName).Run()

	if err := CreateSession("test"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := SetSessionOption("remain-on-exit", "on"); err != nil {
		t.Fatalf("set remain-on-exit: %v", err)
	}

	if err := SetSessionOption("destroy-unattached", "off"); err != nil {
		t.Fatalf("set destroy-unattached: %v", err)
	}

	// Verify remain-on-exit was set
	out, err := exec.Command("tmux", "show-options", "-t", SessionName, "remain-on-exit").CombinedOutput()
	if err != nil {
		t.Fatalf("show-options: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "on") {
		t.Errorf("expected remain-on-exit on, got %s", strings.TrimSpace(string(out)))
	}
}
