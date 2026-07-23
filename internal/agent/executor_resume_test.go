package agent

import (
	"strings"
	"testing"
)

// newResumeReadyMock returns a mock pane whose state satisfies every
// canResumeInPlace check for taskID: agent process alive (non-shell), pane
// PID matching clear_ready_pid, clear_ready set, @last_task_id stamped, and
// cwd inside the worktree.
func newResumeReadyMock(taskID string) *mockPaneIO {
	mock := newExecMock()
	mock.isShell = false
	mock.currentCommand = "claude"
	mock.currentPath = "/project/worktree1"
	mock.userVars["clear_ready"] = "true"
	mock.userVars["clear_ready_pid"] = "12345" // matches mock.panePID
	mock.userVars["last_task_id"] = taskID
	return mock
}

// Issue #55: when the pane still holds the interrupted task's session, a
// ModeWithClear request carrying ResumeMessage delivers the nudge in place —
// no /clear, no full envelope.
func TestExecute_ModeWithClear_ResumeDeliversNudgeWithoutClear(t *testing.T) {
	t.Parallel()
	mock := newResumeReadyMock("task_resume_1")
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID:       "worker1",
		Message:       "FULL ENVELOPE",
		ResumeMessage: "RESUME NUDGE",
		Mode:          ModeWithClear,
		TaskID:        "task_resume_1",
		WorkingDir:    "/project/worktree1",
	})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Fatal("expected Success=true")
	}
	if len(mock.sentTexts) != 1 || mock.sentTexts[0] != "RESUME NUDGE" {
		t.Errorf("sentTexts = %v, want exactly the resume nudge", mock.sentTexts)
	}
	for _, cmd := range mock.sentCmds {
		if strings.Contains(cmd, "/clear") {
			t.Errorf("resume path must not send /clear; sentCmds = %v", mock.sentCmds)
		}
	}
}

// Issue #55 acceptance (c), executor side: a respawned pane (clear_ready
// reset — the same state a blocked-pane recovery or worktree-cleanup respawn
// leaves behind) fails the session-identity preflight, so the delivery falls
// back to the full envelope.
func TestExecute_ModeWithClear_ResumeFallsBackAfterRespawn(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	mock.isShell = false
	mock.currentCommand = "claude"
	// clear_ready intentionally NOT set: pane process was respawned.
	mock.userVars["last_task_id"] = "task_resume_2"
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID:       "worker1",
		Message:       "FULL ENVELOPE",
		ResumeMessage: "RESUME NUDGE",
		Mode:          ModeWithClear,
		TaskID:        "task_resume_2",
	})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if len(mock.sentTexts) != 1 || mock.sentTexts[0] != "FULL ENVELOPE" {
		t.Errorf("sentTexts = %v, want the full envelope (fallback)", mock.sentTexts)
	}
}

// canResumeInPlace unit coverage for each session-identity mismatch.
func TestCanResumeInPlace_MismatchReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(m *mockPaneIO)
		workingDir string
		wantOK     bool
		wantReason string
	}{
		{
			name:   "all_checks_pass",
			mutate: func(m *mockPaneIO) {},
			wantOK: true,
		},
		{
			name:       "pane_process_restarted",
			mutate:     func(m *mockPaneIO) { m.panePID = "99999" },
			wantReason: "pane_process_restarted",
		},
		{
			name:       "no_prior_dispatch",
			mutate:     func(m *mockPaneIO) { delete(m.userVars, "clear_ready") },
			wantReason: "no_prior_dispatch_on_pane",
		},
		{
			name: "agent_crashed_to_shell",
			mutate: func(m *mockPaneIO) {
				m.currentCommand = "zsh"
				m.isShell = true
			},
			wantReason: "agent_process_not_running",
		},
		{
			name:       "pane_holds_different_task",
			mutate:     func(m *mockPaneIO) { m.userVars["last_task_id"] = "task_other" },
			wantReason: "pane_holds_different_task",
		},
		{
			name:       "last_task_id_never_stamped",
			mutate:     func(m *mockPaneIO) { delete(m.userVars, "last_task_id") },
			wantReason: "last_task_id_unavailable",
		},
		{
			name:       "working_dir_mismatch",
			mutate:     func(m *mockPaneIO) { m.currentPath = "/project/worktree2" },
			workingDir: "/project/worktree1",
			wantReason: "working_dir_mismatch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mock := newResumeReadyMock("task_resume_x")
			tt.mutate(mock)
			exec, _ := newTestExecutorWithLog(mock)

			ok, reason := exec.canResumeInPlace("%0", ExecRequest{
				AgentID:    "worker1",
				TaskID:     "task_resume_x",
				WorkingDir: tt.workingDir,
			})
			if ok != tt.wantOK {
				t.Fatalf("ok = %t (reason=%q), want %t", ok, reason, tt.wantOK)
			}
			if !tt.wantOK && reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

// Successful task deliveries stamp @last_task_id so a later resume preflight
// can verify the pane's conversation belongs to the interrupted task.
func TestExecute_ModeWithClear_FirstDispatchStampsLastTaskID(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	mock.isShell = false
	mock.currentCommand = "claude"
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "worker1",
		Message: "task content",
		Mode:    ModeWithClear,
		TaskID:  "task_stamp_1",
	})
	if result.Error != nil || !result.Success {
		t.Fatalf("dispatch failed: success=%t err=%v", result.Success, result.Error)
	}
	if got := mock.userVars["last_task_id"]; got != "task_stamp_1" {
		t.Errorf("@last_task_id = %q, want task_stamp_1", got)
	}
}
