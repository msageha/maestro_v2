package tmux

import (
	"os/exec"
	"strings"
	"testing"
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
		t.Errorf("expected 4 panes, got %d", len(panes))
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
