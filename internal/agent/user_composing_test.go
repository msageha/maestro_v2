package agent

import (
	"errors"
	"testing"
)

func TestPromptInputText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		content   string
		wantText  string
		wantFound bool
	}{
		{
			name:      "empty input box",
			content:   "output\n ❯ \n",
			wantText:  "",
			wantFound: true,
		},
		{
			name:      "draft text after glyph",
			content:   "output\n│ ❯ fix the login bug │\n╰───╯\n",
			wantText:  "fix the login bug",
			wantFound: true,
		},
		{
			name:      "bordered empty box",
			content:   "│ ❯ │\n",
			wantText:  "",
			wantFound: true,
		},
		{
			name:      "no prompt visible",
			content:   "compiling...\nstill working\n",
			wantText:  "",
			wantFound: false,
		},
		{
			name:      "status bar below prompt",
			content:   "│ ❯ draft │\n? for shortcuts\n",
			wantText:  "draft",
			wantFound: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			text, found := promptInputText(tt.content)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
		})
	}
}

// newComposingMock prepares the orchestrator ModeDeliver path so that busy
// detection short-circuits at Stage 1 (shell → idle) and the user-composing
// guard's CapturePane calls consume capturePaneSeq.
// isShellSeq: ensureClaudeRunning (not shell) → busy Stage 1 (shell → idle)
// → sendAndConfirm (not shell).
func newComposingMock(captures ...string) *mockPaneIO {
	mock := newExecMock()
	mock.isShellSeq = []bool{false, true, false}
	for _, c := range captures {
		mock.capturePaneSeq = append(mock.capturePaneSeq, mockResp{val: c})
	}
	return mock
}

func TestExecute_ModeDeliver_Orchestrator_UserComposing_Defers(t *testing.T) {
	t.Parallel()
	// Input text changes between the two probes → human actively typing.
	mock := newComposingMock("│ ❯ deploy the │\n", "│ ❯ deploy the new │\n")
	exec, _ := newTestExecutorWithLog(mock)
	exec.execCfg.UserComposingProbeInterval = 0

	result := exec.Execute(ExecRequest{
		AgentID: "orchestrator",
		Message: "[maestro] kind:command_completed",
		Mode:    ModeDeliver,
	})
	if result.Error == nil {
		t.Fatal("expected deferral error while user is composing")
	}
	if !errors.Is(result.Error, ErrUserComposing) {
		t.Errorf("error must wrap ErrUserComposing, got: %v", result.Error)
	}
	if !result.Retryable {
		t.Error("composing deferral must be retryable")
	}
	if len(mock.sentTexts) != 0 {
		t.Errorf("nothing should be pasted while composing, got %v", mock.sentTexts)
	}
}

func TestExecute_ModeDeliver_Orchestrator_StaticDraft_Delivers(t *testing.T) {
	t.Parallel()
	// Identical non-empty input across probes: placeholder hint or abandoned
	// draft. Deferring would block notifications indefinitely, so delivery
	// proceeds (pre-guard behavior). The final capture returns a clean prompt
	// for the post-send confirmation probes (sequence clamps to last).
	mock := newComposingMock("│ ❯ stale draft │\n", "│ ❯ stale draft │\n", "output\n ❯ \n")
	exec, _ := newTestExecutorWithLog(mock)
	exec.execCfg.UserComposingProbeInterval = 0

	result := exec.Execute(ExecRequest{
		AgentID: "orchestrator",
		Message: "notification",
		Mode:    ModeDeliver,
	})
	if result.Error != nil {
		t.Fatalf("static draft must not block delivery: %v", result.Error)
	}
	if len(mock.sentTexts) != 1 {
		t.Errorf("expected 1 delivery, got %v", mock.sentTexts)
	}
}

func TestExecute_ModeDeliver_Orchestrator_EmptyInput_Delivers(t *testing.T) {
	t.Parallel()
	mock := newComposingMock("output\n ❯ \n")
	exec, _ := newTestExecutorWithLog(mock)
	exec.execCfg.UserComposingProbeInterval = 0

	result := exec.Execute(ExecRequest{
		AgentID: "orchestrator",
		Message: "notification",
		Mode:    ModeDeliver,
	})
	if result.Error != nil {
		t.Fatalf("empty input box must deliver immediately: %v", result.Error)
	}
	if len(mock.sentTexts) != 1 {
		t.Errorf("expected 1 delivery, got %v", mock.sentTexts)
	}
}
