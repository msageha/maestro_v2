package agent

import "testing"

func TestPaneStateManager_ClearReadyLifecycle(t *testing.T) {
	mock := newExecMock()
	psm := NewPaneStateManager(mock)
	pane := "%0"

	// Initially not clear-ready
	if psm.IsClearReady(pane) {
		t.Error("expected IsClearReady=false initially")
	}

	// Set clear-ready with PID
	if err := psm.SetClearReady(pane, "12345"); err != nil {
		t.Fatalf("SetClearReady: %v", err)
	}
	if !psm.IsClearReady(pane) {
		t.Error("expected IsClearReady=true after SetClearReady")
	}
	if mock.userVars["clear_ready"] != "true" {
		t.Errorf("expected clear_ready=true, got %q", mock.userVars["clear_ready"])
	}
	if mock.userVars["clear_ready_pid"] != "12345" {
		t.Errorf("expected clear_ready_pid=12345, got %q", mock.userVars["clear_ready_pid"])
	}

	// Reset
	if err := psm.ResetClearReady(pane); err != nil {
		t.Fatalf("ResetClearReady: %v", err)
	}
	if psm.IsClearReady(pane) {
		t.Error("expected IsClearReady=false after reset")
	}
	if mock.userVars["clear_ready"] != "" {
		t.Errorf("expected clear_ready empty, got %q", mock.userVars["clear_ready"])
	}
	if mock.userVars["clear_ready_pid"] != "" {
		t.Errorf("expected clear_ready_pid empty, got %q", mock.userVars["clear_ready_pid"])
	}
}

func TestPaneStateManager_DetectProcessRestart(t *testing.T) {
	mock := newExecMock()
	psm := NewPaneStateManager(mock)
	pane := "%0"

	// No stored PID — no restart detected
	restarted, pid, err := psm.DetectProcessRestart(pane)
	if err != nil {
		t.Fatalf("DetectProcessRestart: %v", err)
	}
	if restarted {
		t.Error("expected no restart with no stored PID")
	}
	if pid != "12345" {
		t.Errorf("expected PID 12345, got %s", pid)
	}

	// Set clear_ready with PID
	if err := psm.SetClearReady(pane, "12345"); err != nil {
		t.Fatalf("SetClearReady: %v", err)
	}

	// Same PID — no restart
	restarted, _, err = psm.DetectProcessRestart(pane)
	if err != nil {
		t.Fatalf("DetectProcessRestart: %v", err)
	}
	if restarted {
		t.Error("expected no restart with same PID")
	}
	if !psm.IsClearReady(pane) {
		t.Error("clear_ready should still be true")
	}

	// Different PID — restart detected
	mock.panePID = "99999"
	restarted, pid, err = psm.DetectProcessRestart(pane)
	if err != nil {
		t.Fatalf("DetectProcessRestart: %v", err)
	}
	if !restarted {
		t.Error("expected restart with different PID")
	}
	if pid != "99999" {
		t.Errorf("expected PID 99999, got %s", pid)
	}
	// clear_ready should be reset after restart
	if psm.IsClearReady(pane) {
		t.Error("expected IsClearReady=false after restart detection")
	}
}

func TestPaneStateManager_StatusAndCWD(t *testing.T) {
	mock := newExecMock()
	psm := NewPaneStateManager(mock)
	pane := "%0"

	// Set status
	if err := psm.SetStatus(pane, "busy"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if mock.userVars["status"] != "busy" {
		t.Errorf("expected status=busy, got %q", mock.userVars["status"])
	}

	// Set and get CWD
	if err := psm.SetCWD(pane, "/tmp/worktree"); err != nil {
		t.Fatalf("SetCWD: %v", err)
	}
	if got := psm.GetCWD(pane); got != "/tmp/worktree" {
		t.Errorf("expected CWD=/tmp/worktree, got %q", got)
	}

	// Empty CWD when not set
	mock.userVars = make(map[string]string)
	if got := psm.GetCWD(pane); got != "" {
		t.Errorf("expected empty CWD, got %q", got)
	}
}
