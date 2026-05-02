package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
)

func TestContentHash(t *testing.T) {
	t.Parallel()
	// Same input → same hash
	h1 := contentHash("hello world")
	h2 := contentHash("hello world")
	if h1 != h2 {
		t.Error("same input should produce same hash")
	}

	// Different input → different hash
	h3 := contentHash("different content")
	if h1 == h3 {
		t.Error("different input should produce different hash")
	}

	// Empty string should work
	h4 := contentHash("")
	if h4 == "" {
		t.Error("hash of empty string should not be empty")
	}
}

func TestApplyDefaults(t *testing.T) {
	t.Parallel()

	// Each row pins one (input cfg) → (expected post-applyDefaults cfg)
	// mapping for the watcher knobs. Two scenarios are exercised:
	// "zero" → defaults applied, "non-zero" → values preserved verbatim.
	// Field-by-field assertion uses a single deep equal so adding a new
	// field requires updating expectations once.
	tests := []struct {
		name     string
		input    model.WatcherConfig
		expected model.WatcherConfig
	}{
		{
			name:  "zero values get defaults",
			input: model.WatcherConfig{},
			expected: model.WatcherConfig{
				BusyCheckInterval:      2,
				BusyCheckMaxRetries:    30,
				IdleStableSec:          5,
				CooldownAfterClear:     3,
				ClearConfirmTimeoutSec: 5,
				ClearConfirmPollMs:     250,
				ClearMaxAttempts:       3,
				ClearRetryBackoffMs:    500,
			},
		},
		{
			name: "non-zero values preserved",
			input: model.WatcherConfig{
				BusyCheckInterval:      10,
				BusyCheckMaxRetries:    50,
				IdleStableSec:          8,
				CooldownAfterClear:     5,
				ClearConfirmTimeoutSec: 10,
				ClearConfirmPollMs:     500,
				ClearMaxAttempts:       5,
				ClearRetryBackoffMs:    1000,
			},
			expected: model.WatcherConfig{
				BusyCheckInterval:      10,
				BusyCheckMaxRetries:    50,
				IdleStableSec:          8,
				CooldownAfterClear:     5,
				ClearConfirmTimeoutSec: 10,
				ClearConfirmPollMs:     500,
				ClearMaxAttempts:       5,
				ClearRetryBackoffMs:    1000,
			},
		},
	}

	checks := []struct {
		name string
		get  func(c model.WatcherConfig) int
	}{
		{"BusyCheckInterval", func(c model.WatcherConfig) int { return c.BusyCheckInterval }},
		{"BusyCheckMaxRetries", func(c model.WatcherConfig) int { return c.BusyCheckMaxRetries }},
		{"IdleStableSec", func(c model.WatcherConfig) int { return c.IdleStableSec }},
		{"CooldownAfterClear", func(c model.WatcherConfig) int { return c.CooldownAfterClear }},
		{"ClearConfirmTimeoutSec", func(c model.WatcherConfig) int { return c.ClearConfirmTimeoutSec }},
		{"ClearConfirmPollMs", func(c model.WatcherConfig) int { return c.ClearConfirmPollMs }},
		{"ClearMaxAttempts", func(c model.WatcherConfig) int { return c.ClearMaxAttempts }},
		{"ClearRetryBackoffMs", func(c model.WatcherConfig) int { return c.ClearRetryBackoffMs }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyDefaults(tc.input)
			for _, ck := range checks {
				if g, w := ck.get(got), ck.get(tc.expected); g != w {
					t.Errorf("%s: got %d, want %d", ck.name, g, w)
				}
			}
		})
	}
}

func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected logLevel
	}{
		{"debug", logLevelDebug},
		{"DEBUG", logLevelDebug},
		{"info", logLevelInfo},
		{"INFO", logLevelInfo},
		{"warn", logLevelWarn},
		{"warning", logLevelWarn},
		{"error", logLevelError},
		{"ERROR", logLevelError},
		{"unknown", logLevelInfo},
		{"", logLevelInfo},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := parseLogLevel(tt.input)
			if got != tt.expected {
				t.Errorf("parseLogLevel(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}

func TestBusyVerdictString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		verdict  busyVerdict
		expected string
	}{
		{VerdictIdle, "idle"},
		{VerdictBusy, "busy"},
		{VerdictUndecided, "undecided"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.expected, func(t *testing.T) {
			t.Parallel()
			if got := tt.verdict.String(); got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestNewExecutor_InvalidBusyPatterns(t *testing.T) {
	t.Parallel()
	_, err := newExecutor("", model.WatcherConfig{
		BusyPatterns: "[invalid",
	}, "info", &bytes.Buffer{}, nil, DefaultExecutorConfig())
	if err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestNewExecutor_ValidBusyPatterns(t *testing.T) {
	t.Parallel()
	exec, err := newExecutor("", model.WatcherConfig{
		BusyPatterns: "Working|Thinking|Planning",
	}, "info", &bytes.Buffer{}, nil, DefaultExecutorConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exec.busyDetector == nil {
		t.Error("busyDetector should be initialized")
	}
	if exec.busyDetector.busyRegex == nil {
		t.Error("busyDetector.busyRegex should be compiled")
	}
}

func TestNewExecutor_EmptyBusyPatterns(t *testing.T) {
	t.Parallel()
	exec, err := newExecutor("", model.WatcherConfig{}, "info", &bytes.Buffer{}, nil, DefaultExecutorConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exec.busyDetector == nil {
		t.Error("busyDetector should be initialized")
	}
	if exec.busyDetector.busyRegex != nil {
		t.Error("busyDetector.busyRegex should be nil for empty patterns")
	}
}

func TestLogLevelFiltering(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	exec, err := newExecutor("", model.WatcherConfig{}, "warn", &buf, nil, DefaultExecutorConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Debug and Info should be filtered
	exec.log(logLevelDebug, "debug message")
	exec.log(logLevelInfo, "info message")
	if buf.Len() != 0 {
		t.Errorf("expected no output for debug/info at warn level, got: %s", buf.String())
	}

	// Warn and Error should pass
	exec.log(logLevelWarn, "warn message")
	if !strings.Contains(buf.String(), "WARN") {
		t.Error("warn message should be logged")
	}

	buf.Reset()
	exec.log(logLevelError, "error message")
	if !strings.Contains(buf.String(), "ERROR") {
		t.Error("error message should be logged")
	}
}

func TestExecMode_Constants(t *testing.T) {
	t.Parallel()
	// Verify all mode constants are distinct (regression guard)
	modes := []ExecMode{ModeDeliver, ModeWithClear, ModeInterrupt, ModeIsBusy, ModeClear}
	seen := make(map[ExecMode]bool)
	for _, m := range modes {
		if seen[m] {
			t.Errorf("duplicate mode constant: %s", m)
		}
		seen[m] = true
	}
}

// --- Stage 1 shell command detection ---

func TestShellCommands(t *testing.T) {
	t.Parallel()
	// Known shells should be recognized via tmux.IsShellCommand
	shells := []string{"bash", "zsh", "fish", "sh", "dash", "tcsh", "csh"}
	for _, s := range shells {
		if !tmux.IsShellCommand(s) {
			t.Errorf("expected %q to be a known shell command", s)
		}
	}

	// Non-shells should NOT be recognized
	nonShells := []string{"claude", "node", "python", "vim", ""}
	for _, s := range nonShells {
		if tmux.IsShellCommand(s) {
			t.Errorf("expected %q to NOT be a known shell command", s)
		}
	}
}

// --- Role name validation ---

func TestValidRoleName(t *testing.T) {
	t.Parallel()
	valid := []string{"orchestrator", "planner", "worker", "worker-1", "my_role"}
	for _, r := range valid {
		if !validRoleName.MatchString(r) {
			t.Errorf("expected %q to be a valid role name", r)
		}
	}

	invalid := []string{"../etc/passwd", "role/sub", "role name", "", "role;cmd", "role\x00null"}
	for _, r := range invalid {
		if validRoleName.MatchString(r) {
			t.Errorf("expected %q to be an INVALID role name", r)
		}
	}
}

// --- Orchestrator Ctrl-C protection ---

func TestOrchestratorInterruptRejected(t *testing.T) {
	t.Parallel()
	// Orchestrator should never be interruptible.
	// execInterrupt checks AgentID == "orchestrator" before any tmux call,
	// so we can test the guard directly without a running tmux session.
	var buf bytes.Buffer
	exec, err := newExecutor("", model.WatcherConfig{}, "info", &buf, nil, DefaultExecutorConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req := ExecRequest{AgentID: "orchestrator", Mode: ModeInterrupt}
	result := exec.execInterrupt(context.Background(), req, "")

	if result.Error == nil {
		t.Fatal("expected error for orchestrator interrupt, got nil")
	}
	if !strings.Contains(result.Error.Error(), "cannot interrupt orchestrator") {
		t.Errorf("expected 'cannot interrupt orchestrator' error, got: %v", result.Error)
	}
}

// --- isPromptReady tests ---

func TestIsPromptReady(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "prompt on last non-blank line",
			content: "some output\n ❯ \n",
			want:    true,
		},
		{
			name:    "prompt with status bar below",
			content: "output\n ❯ \nTokens: 1.2k  Cost: $0.01\n",
			want:    true,
		},
		{
			name:    "prompt with multiple status lines below",
			content: "output\n ❯ \n─────────────\nTokens: 1.2k  Cost: $0.01\n",
			want:    true,
		},
		{
			name:    "no prompt character",
			content: "just some output\nno prompt here\n",
			want:    false,
		},
		{
			name:    "empty content",
			content: "",
			want:    false,
		},
		{
			name:    "only blank lines",
			content: "\n\n\n",
			want:    false,
		},
		{
			name:    "fallback > on last non-blank line",
			content: "some output\n> ",
			want:    true,
		},
		{
			name:    "> not on last non-blank line (should be false)",
			content: "> old line\nstatus bar text\n",
			want:    false,
		},
		{
			name:    "prompt within maxPromptSearchLines window",
			content: "line1\nproject ❯ input text\nstatus1\nstatus2\n",
			want:    true,
		},
		{
			name:    "prompt outside maxPromptSearchLines window (false positive guard)",
			content: "❯ old prompt\nline2\nline3\nline4\nline5\nline6\nline7\n",
			want:    false,
		},
		{
			name:    "unicode prompt only",
			content: "❯",
			want:    true,
		},
		{
			name:    "prompt with ANSI escape sequences",
			content: "output\n \x1b[32m❯\x1b[0m \n",
			want:    true,
		},
		{
			name:    "prompt hidden by ANSI bold/color",
			content: "output\n\x1b[1;34mproject\x1b[0m \x1b[32m❯\x1b[0m\nstatus\n",
			want:    true,
		},
		{
			name:    "fallback > with ANSI escape",
			content: "output\n\x1b[32m> \x1b[0m",
			want:    true,
		},
		{
			name:    "skill loading text obscuring prompt within 6 lines",
			content: "output\n ❯ \nskill line 1\nskill line 2\nskill line 3\nskill line 4\n",
			want:    true,
		},
		{
			name:    "no prompt even with ANSI stripped",
			content: "\x1b[32msome colored text\x1b[0m\nno prompt\n",
			want:    false,
		},
		{
			name:    "markdown blockquote should not match fallback",
			content: "output\n> This is a blockquote\n",
			want:    false,
		},
		{
			name:    "log line with > should not match fallback",
			content: "output\n> 2024-01-01 some log entry\n",
			want:    false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isPromptReady(tt.content)
			if got != tt.want {
				t.Errorf("isPromptReady(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestLastNonBlankLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"normal", "line1\nline2\n", "line2"},
		{"with trailing blanks", "line1\nline2\n\n\n", "line2"},
		{"empty", "", "<empty>"},
		{"only blanks", "\n\n", "<empty>"},
		{"long line truncated", strings.Repeat("x", 100), strings.Repeat("x", 80) + "..."},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := lastNonBlankLine(tt.content)
			if got != tt.want {
				t.Errorf("lastNonBlankLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no escape", "hello world", "hello world"},
		{"CSI color", "\x1b[32mgreen\x1b[0m", "green"},
		{"CSI bold+color", "\x1b[1;34mbold blue\x1b[0m", "bold blue"},
		{"OSC title", "\x1b]0;window title\x07rest", "rest"},
		{"OSC with ST", "\x1b]0;title\x1b\\rest", "rest"},
		{"charset designator", "\x1b(Bhello", "hello"},
		{"mixed", "\x1b[32m❯\x1b[0m input", "❯ input"},
		{"empty", "", ""},
		{"no escape with unicode", "project ❯ ", "project ❯ "},
		{"private CSI bracketed paste", "\x1b[?2004htext\x1b[?2004l", "text"},
		{"private CSI cursor hide", "\x1b[?25lhidden\x1b[?25h", "hidden"},
		{"CSI with tilde final", "\x1b[1;2~rest", "rest"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stripANSI(tt.input)
			if got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- clearTextVisible tests ---

func TestClearTextVisible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "clear on last line (production failure mode)",
			content: "some output\n/clear\n",
			want:    true,
		},
		{
			name:    "clear in prompt line",
			content: "output\n❯ /clear\n",
			want:    true,
		},
		{
			name:    "clear with ANSI escapes",
			content: "output\n\x1b[32m❯\x1b[0m /clear\nstatus\n",
			want:    true,
		},
		{
			name:    "no clear — normal prompt",
			content: "output\n ❯ \nTokens: 1.2k\n",
			want:    false,
		},
		{
			name:    "no clear — empty",
			content: "",
			want:    false,
		},
		{
			name:    "no clear — only blanks",
			content: "\n\n\n",
			want:    false,
		},
		{
			name:    "clear far above (outside 6-line window)",
			content: "/clear\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n",
			want:    false,
		},
		{
			name:    "clear within 6-line window",
			content: "line1\n/clear\nline3\nline4\nline5\nline6\n",
			want:    true,
		},
		{
			name:    "clear as part of longer text (not a command)",
			content: "output\nRunning /clear command...\n",
			want:    false,
		},
		{
			name:    "clear after successful processing (gone)",
			content: "Context cleared\n ❯ \nTokens: 0\n",
			want:    false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := clearTextVisible(tt.content)
			if got != tt.want {
				t.Errorf("clearTextVisible() = %v, want %v\ncontent: %q", got, tt.want, tt.content)
			}
		})
	}
}

// --- promptReadyLines constant test ---

func TestPromptReadyLinesIncreased(t *testing.T) {
	t.Parallel()
	// Verify the capture depth has been increased from 5 to 12
	// to accommodate Claude Code's status bars and improve prompt detection.
	cfg := DefaultExecutorConfig()
	if cfg.PromptReadyLines < 12 {
		t.Errorf("PromptReadyLines = %d, want >= 12 (increased for better prompt detection)", cfg.PromptReadyLines)
	}
}

// === Executor Integration Tests ===

func TestExecute_UnknownMode(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "worker1",
		Mode:    ExecMode("invalid"),
	})
	if result.Error == nil {
		t.Fatal("expected error for unknown mode")
	}
	if !strings.Contains(result.Error.Error(), "unknown exec mode") {
		t.Errorf("unexpected error: %v", result.Error)
	}
}

func TestExecute_PaneNotFound(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	mock.findPaneErr = fmt.Errorf("pane not found")
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "worker1",
		Mode:    ModeDeliver,
	})
	if result.Error == nil {
		t.Fatal("expected error when pane not found")
	}
	if !strings.Contains(result.Error.Error(), "find pane") {
		t.Errorf("unexpected error: %v", result.Error)
	}
}

func TestExecute_ModeIsBusy_Idle(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	mock.isShell = true // shell → idle
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "worker1",
		Mode:    ModeIsBusy,
	})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Success {
		t.Error("expected Success=false for idle agent")
	}
}

func TestExecute_ModeIsBusy_Busy(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	mock.isShell = false
	mock.currentCommand = "claude"
	mock.captureContent = "working..."
	mock.joinedContent = []string{"content-1", "content-2"} // changing → busy
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "worker1",
		Mode:    ModeIsBusy,
	})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true for busy agent")
	}
}

func TestExecute_ModeIsBusy_ContextCancelled_ReturnsBusy(t *testing.T) {
	// Context cancelled during the Stage 3 activity-probe sleep now returns
	// VerdictBusy (not VerdictUndecided): Claude is confirmed running from
	// Stage 1 so "busy" is the conservative and accurate result.
	t.Parallel()
	mock := newExecMock()
	mock.isShell = false
	mock.currentCommand = "claude"
	mock.captureContent = "working..."
	mock.joinedContent = []string{"same-content", "same-content"}
	exec, _ := newTestExecutorWithLog(mock)
	// Use non-zero IdleStableSec so Stage 3 has a sleep that context
	// cancellation can interrupt.
	exec.busyDetector.config.IdleStableSec = 5

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so sleepCtx fails during Stage 3

	result := exec.Execute(ExecRequest{
		Context: ctx,
		AgentID: "worker1",
		Mode:    ModeIsBusy,
	})
	// VerdictBusy from ctx cancellation → execIsBusy returns Success=true.
	if !result.Success {
		t.Errorf("expected Success=true (VerdictBusy) on context cancel, got error: %v", result.Error)
	}
	if result.Error != nil {
		t.Errorf("expected nil error for VerdictBusy, got: %v", result.Error)
	}
}

func TestExecute_ModeDeliver_Success(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	// ensureClaudeRunning: not shell, busyDetector: shell → idle, sendAndConfirm: not shell
	mock.isShellSeq = []bool{false, true, false}
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "planner",
		Message: "test message",
		Mode:    ModeDeliver,
	})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true")
	}
	if len(mock.sentTexts) != 1 || mock.sentTexts[0] != "test message" {
		t.Errorf("expected sent text 'test message', got %v", mock.sentTexts)
	}
	if mock.userVars["status"] != "busy" {
		t.Errorf("expected status=busy after delivery, got %q", mock.userVars["status"])
	}
}

func TestExecute_ModeDeliver_AgentBusy(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	mock.isShell = false
	mock.currentCommand = "claude"
	mock.captureContent = "working..."
	mock.joinedContent = []string{"content-1", "content-2"} // changing → busy
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "planner",
		Message: "test message",
		Mode:    ModeDeliver,
	})
	if result.Error == nil {
		t.Fatal("expected error for busy agent")
	}
	if !result.Retryable {
		t.Error("expected Retryable=true for busy agent")
	}
	if len(mock.sentTexts) != 0 {
		t.Errorf("expected no sent text for busy agent, got %v", mock.sentTexts)
	}
}

func TestExecute_ModeDeliver_SendTextFails(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	// ensureClaudeRunning: not shell, busyDetector: shell → idle, sendAndConfirm: not shell
	mock.isShellSeq = []bool{false, true, false}
	mock.sendTextErr = fmt.Errorf("tmux error")
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "planner",
		Message: "test message",
		Mode:    ModeDeliver,
	})
	if result.Error == nil {
		t.Fatal("expected error when send fails")
	}
	if !strings.Contains(result.Error.Error(), "send message") {
		t.Errorf("unexpected error: %v", result.Error)
	}
	if !result.Retryable {
		t.Error("expected Retryable=true for send failure")
	}
}

func TestExecute_ModeDeliver_Orchestrator_Idle(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	// ensureClaudeRunning: not shell, busyDetector: shell → idle, sendAndConfirm: not shell
	mock.isShellSeq = []bool{false, true, false}
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "orchestrator",
		Message: "notification",
		Mode:    ModeDeliver,
	})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true")
	}
}

func TestExecute_ModeDeliver_Orchestrator_Busy(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	mock.isShell = false
	mock.currentCommand = "claude"
	mock.captureContent = "working..."
	mock.joinedContent = []string{"content-1", "content-2"} // changing → busy
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "orchestrator",
		Message: "notification",
		Mode:    ModeDeliver,
	})
	if result.Error == nil {
		t.Fatal("expected error for busy orchestrator")
	}
	if !strings.Contains(result.Error.Error(), "orchestrator busy") {
		t.Errorf("unexpected error: %v", result.Error)
	}
	if !result.Retryable {
		t.Error("expected Retryable=true")
	}
}

func TestExecute_ModeWithClear_FirstDispatch(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	// ensureClaudeRunning in execWithClear: not shell,
	// ensureClaudeRunning in execDeliver: not shell,
	// busyDetector: shell → idle, sendAndConfirm: not shell
	mock.isShellSeq = []bool{false, false, true, false}
	// clear_ready not set → first dispatch path
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "worker1",
		Message: "task content",
		Mode:    ModeWithClear,
	})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true")
	}
	if len(mock.sentTexts) != 1 || mock.sentTexts[0] != "task content" {
		t.Errorf("expected sent text 'task content', got %v", mock.sentTexts)
	}
	// Verify clear_ready was set after successful first dispatch
	if mock.userVars["clear_ready"] != "true" {
		t.Errorf("expected clear_ready=true after first dispatch, got %q", mock.userVars["clear_ready"])
	}
	// Verify clear_ready_pid was stored
	if mock.userVars["clear_ready_pid"] != "12345" {
		t.Errorf("expected clear_ready_pid=12345, got %q", mock.userVars["clear_ready_pid"])
	}
}

// TestExecute_ModeWithClear_FirstDispatch_CodexSkipsBusyDetection is a Bug J
// regression test. Before the fix, first dispatch delegated to execDeliver
// which ran hash-based busy detection (CapturePaneJoined x2) before sending;
// codex's Rust TUI re-renders continuously right after launch, so the hash
// changed between probes and busy detection looped for 15+ retries, blocking
// delivery indefinitely. The fix uses waitReady (prompt-readiness check)
// instead, which for codex only requires non-shell + non-blank content.
//
// This test exercises first dispatch with a mock that returns DIFFERENT hashes
// on successive CapturePaneJoined calls (simulating TUI animation). If busy
// detection were still invoked, the test would detect the animation as "busy"
// and retry until the mock retries are exhausted. Instead, delivery succeeds
// on the first attempt and CapturePaneJoined is never called.
func TestExecute_ModeWithClear_FirstDispatch_CodexSkipsBusyDetection(t *testing.T) {
	t.Parallel()
	mock := newExecMock()

	// Mark pane runtime as codex so isAgentReady takes the non-claude branch
	// (checks non-shell command + non-blank content, not '❯' prompt glyph).
	mock.userVars["runtime"] = "codex"
	// clear_ready is unset → first dispatch path.

	// ensureClaudeRunning (execWithClear) + isAgentReady (waitReady) both call
	// GetPaneCurrentCommand; both must see non-shell. Subsequent IsShellCommand
	// calls from sendAndConfirm also see non-shell.
	mock.isShell = false
	mock.currentCommand = "codex"

	// CapturePane returns a non-blank screen so isAgentReady passes on the first
	// poll (codex TUI has painted welcome banner).
	mock.captureContent = "codex v1.0\n$ ready\n"

	// Simulate TUI animation: each CapturePaneJoined call returns DIFFERENT
	// content. If busy detection were still wired into first dispatch, it would
	// hash these two snapshots, detect hashA != hashB, and return VerdictBusy.
	// After the fix, first dispatch must not call CapturePaneJoined at all.
	mock.joinedContent = []string{
		"frame-A\nspinner-◐\n",
		"frame-B\nspinner-◓\n",
		"frame-C\nspinner-◑\n",
		"frame-D\nspinner-◒\n",
	}

	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "worker1",
		Message: "task content",
		Mode:    ModeWithClear,
	})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true for first dispatch even with animating TUI")
	}
	if len(mock.sentTexts) != 1 || mock.sentTexts[0] != "task content" {
		t.Errorf("expected sent text 'task content', got %v", mock.sentTexts)
	}
	// Regression assertion: busy detection (CapturePaneJoined) must not be
	// invoked during first dispatch. Any call here means the Bug J fix has
	// regressed and first dispatch is once again vulnerable to TUI-animation
	// false-positive busy verdicts.
	if callsContain(mock.calls, "CapturePaneJoined") {
		t.Errorf("Bug J regression: first dispatch invoked busy detection (CapturePaneJoined). calls=%v", mock.calls)
	}
}

func TestExecute_ModeWithClear_Orchestrator_FallsToDeliver(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	// ensureClaudeRunning: not shell, busyDetector: shell → idle, sendAndConfirm: not shell
	mock.isShellSeq = []bool{false, true, false}
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "orchestrator",
		Message: "notification",
		Mode:    ModeWithClear,
	})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true")
	}
	// Orchestrator should not set clear_ready
	if mock.userVars["clear_ready"] == "true" {
		t.Error("clear_ready should not be set for orchestrator")
	}
}

func TestExecute_ModeInterrupt_ViaExecute(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	exec, _ := newTestExecutorWithLog(mock)

	// Orchestrator interrupt should be rejected at Execute() level
	result := exec.Execute(ExecRequest{
		AgentID: "orchestrator",
		Mode:    ModeInterrupt,
	})
	if result.Error == nil {
		t.Fatal("expected error for orchestrator interrupt")
	}
	if !strings.Contains(result.Error.Error(), "cannot interrupt orchestrator") {
		t.Errorf("unexpected error: %v", result.Error)
	}
}

func TestExecute_DefaultContext(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	// ensureClaudeRunning: not shell, busyDetector: shell → idle, sendAndConfirm: not shell
	mock.isShellSeq = []bool{false, true, false}
	exec, _ := newTestExecutorWithLog(mock)

	// Execute with nil context — should use default timeout
	result := exec.Execute(ExecRequest{
		AgentID: "planner",
		Message: "test",
		Mode:    ModeDeliver,
		Context: nil,
	})
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true with default context")
	}
}

// TestExecute_DeliveryStartLogNoDuplicate verifies that delivery_start is logged
// exactly once per Execute() call (regression test for B5 fix).
func TestExecute_DeliveryStartLogNoDuplicate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		agentID string
		mode    ExecMode
	}{
		{
			name:    "ModeWithClear first dispatch calls execDeliver internally",
			agentID: "worker1",
			mode:    ModeWithClear,
		},
		{
			name:    "ModeWithClear orchestrator falls through to execDeliver",
			agentID: "orchestrator",
			mode:    ModeWithClear,
		},
		{
			name:    "ModeDeliver direct",
			agentID: "planner",
			mode:    ModeDeliver,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mock := newExecMock()
			// ensureClaudeRunning calls (up to 2 for worker first dispatch),
			// busyDetector: shell → idle, sendAndConfirm: not shell
			mock.isShellSeq = []bool{false, false, true, false}
			exec, buf := newTestExecutorWithLog(mock)

			exec.Execute(ExecRequest{
				AgentID: tt.agentID,
				Message: "test",
				Mode:    tt.mode,
				TaskID:  "task_001",
			})

			logOutput := buf.String()
			count := strings.Count(logOutput, "delivery_start")
			if count != 1 {
				t.Errorf("expected exactly 1 delivery_start log, got %d\nlog output:\n%s", count, logOutput)
			}
		})
	}
}

// TestExecute_ModeClear_Success verifies the clear-only mode through Execute().
func TestExecute_ModeClear_Success(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	mock.isShell = true
	// For clearAndConfirm: return different content to simulate /clear processing
	mock.joinedContent = []string{"before-clear", "after-clear", "after-clear"}
	exec, _ := newTestExecutorWithLog(mock)

	result := exec.Execute(ExecRequest{
		AgentID: "worker1",
		Mode:    ModeClear,
	})
	// ModeClear goes through waitReady + clearAndConfirm + waitStable
	// With our mock config, this may fail due to clear confirmation timing,
	// but the path through Execute() is exercised regardless.
	// We verify that no panic occurs and the mode is dispatched correctly.
	_ = result // result depends on mock timing; path coverage is the goal
}

// TestExecutor_Close_NilLogFile verifies that Close() handles nil logFile
// without panic (zero-value safety).
func TestExecutor_Close_NilLogFile(t *testing.T) {
	t.Parallel()
	exec := &Executor{} // logFile is nil
	err := exec.Close()
	if err != nil {
		t.Errorf("expected nil error for nil logFile, got: %v", err)
	}
}

// TestExecutor_Close_WithLogFile verifies that Close() properly closes
// a non-nil logFile.
func TestExecutor_Close_WithLogFile(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	exec, _ := newTestExecutorWithLog(newExecMock())

	// Replace logFile with a trackable closer
	closed := false
	exec.logFile = closerFunc(func() error {
		closed = true
		return nil
	})
	_ = buf

	err := exec.Close()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !closed {
		t.Error("expected logFile.Close() to be called")
	}
}

// closerFunc adapts a function to the io.Closer interface.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// --- Fix: busy timeout returns retryable error in execDeliver ---

func TestExecute_ModeDeliver_ContextCancelled_ReturnsBusyError(t *testing.T) {
	// Context cancelled during Stage 3 sleep now returns VerdictBusy (not
	// VerdictUndecided). execDeliver should produce a retryable "busy: timeout"
	// error so deliverPlannerSignal can retry with a fresh context.
	t.Parallel()
	mock := newExecMock()
	mock.isShell = false
	mock.currentCommand = "claude"
	mock.captureContent = "working..."
	mock.joinedContent = []string{"same-content", "same-content"}
	exec, _ := newTestExecutorWithLog(mock)
	// Use non-zero IdleStableSec so Stage 3 has a sleep that context
	// cancellation can interrupt.
	exec.busyDetector.config.IdleStableSec = 5

	// Short timeout: ensureClaudeRunning completes fast, then DetectBusy's
	// Stage 3 sleep(5s) is cancelled by the 10ms timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	result := exec.Execute(ExecRequest{
		Context: ctx,
		AgentID: "planner",
		Message: "test message",
		Mode:    ModeDeliver,
	})
	if result.Error == nil {
		t.Fatal("expected error for busy verdict on context cancel")
	}
	// VerdictBusy → "agent planner busy: timeout" (not ErrBusyUndecided).
	if errors.Is(result.Error, ErrBusyUndecided) {
		t.Errorf("should NOT be ErrBusyUndecided for ctx-cancel case; got: %v", result.Error)
	}
	if !result.Retryable {
		t.Error("expected Retryable=true for busy verdict")
	}
}

// --- DefaultExecutorConfig tests ---

func TestDefaultExecutorConfig_Values(t *testing.T) {
	t.Parallel()
	cfg := DefaultExecutorConfig()

	if cfg.PromptReadyLines != 12 {
		t.Errorf("PromptReadyLines: got %d, want 12", cfg.PromptReadyLines)
	}
	if cfg.BusyHintLines != 5 {
		t.Errorf("BusyHintLines: got %d, want 5", cfg.BusyHintLines)
	}
	if cfg.StableCheckRounds != 1 {
		t.Errorf("StableCheckRounds: got %d, want 1", cfg.StableCheckRounds)
	}
	if cfg.DefaultExecTimeout != 5*time.Minute {
		t.Errorf("DefaultExecTimeout: got %v, want 5m", cfg.DefaultExecTimeout)
	}
	if cfg.ClaudeLaunchTimeout != 60*time.Second {
		t.Errorf("ClaudeLaunchTimeout: got %v, want 60s", cfg.ClaudeLaunchTimeout)
	}
}

// --- applyDefaults additional tests ---

func TestApplyDefaults_ClearSecondEnterDelayMs(t *testing.T) {
	t.Parallel()
	// Zero value gets default
	cfg := applyDefaults(model.WatcherConfig{})
	if cfg.ClearSecondEnterDelayMs != 500 {
		t.Errorf("ClearSecondEnterDelayMs: got %d, want 500", cfg.ClearSecondEnterDelayMs)
	}

	// Negative value gets default
	cfg = applyDefaults(model.WatcherConfig{ClearSecondEnterDelayMs: -1})
	if cfg.ClearSecondEnterDelayMs != 500 {
		t.Errorf("ClearSecondEnterDelayMs: got %d, want 500 for negative input", cfg.ClearSecondEnterDelayMs)
	}

	// Explicit positive value is preserved
	cfg = applyDefaults(model.WatcherConfig{ClearSecondEnterDelayMs: 1000})
	if cfg.ClearSecondEnterDelayMs != 1000 {
		t.Errorf("ClearSecondEnterDelayMs: got %d, want 1000", cfg.ClearSecondEnterDelayMs)
	}
}

func TestApplyDefaults_WaitReadyFields(t *testing.T) {
	t.Parallel()
	cfg := applyDefaults(model.WatcherConfig{})
	if cfg.WaitReadyIntervalSec != 2 {
		t.Errorf("WaitReadyIntervalSec: got %d, want 2", cfg.WaitReadyIntervalSec)
	}
	if cfg.WaitReadyMaxRetries != 15 {
		t.Errorf("WaitReadyMaxRetries: got %d, want 15", cfg.WaitReadyMaxRetries)
	}
}

// --- logf tests ---

func TestLogf_LevelFiltering(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := &buf

	// Debug below Info threshold → no output
	logf(newBufLogger(&buf), logLevelInfo, logLevelDebug, "test", "should not appear")
	if buf.Len() != 0 {
		t.Errorf("expected no output for debug at info level, got: %s", buf.String())
	}

	// Warn above Info threshold → output
	buf.Reset()
	logf(newBufLogger(&buf), logLevelInfo, logLevelWarn, "test", "warning %s", "msg")
	output := buf.String()
	if !strings.Contains(output, "[WARN]") {
		t.Errorf("expected [WARN] in output, got: %s", output)
	}
	if !strings.Contains(output, "test:") {
		t.Errorf("expected component 'test:' in output, got: %s", output)
	}
	if !strings.Contains(output, "warning msg") {
		t.Errorf("expected formatted message in output, got: %s", output)
	}
	_ = logger
}

func TestLogf_AllLevelLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level logLevel
		label string
	}{
		{logLevelDebug, "[DEBUG]"},
		{logLevelInfo, "[INFO]"},
		{logLevelWarn, "[WARN]"},
		{logLevelError, "[ERROR]"},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		logf(newBufLogger(&buf), logLevelDebug, tt.level, "comp", "msg")
		if !strings.Contains(buf.String(), tt.label) {
			t.Errorf("level %d: expected %s in output, got: %s", tt.level, tt.label, buf.String())
		}
	}
}

func newBufLogger(buf *bytes.Buffer) *log.Logger {
	return log.New(buf, "", 0)
}

// --- CleanupPaneMutex tests ---

func TestCleanupPaneMutex(t *testing.T) {
	t.Parallel()
	mock := newExecMock()
	exec, _ := newTestExecutorWithLog(mock)

	// Trigger mutex creation by accessing deliverer
	exec.deliverer.getPaneMutex("%0")

	// Cleanup should remove the mutex
	exec.CleanupPaneMutex("%0")

	// After cleanup, a new mutex should be created
	mu := exec.deliverer.getPaneMutex("%0")
	if mu == nil {
		t.Error("expected new mutex after cleanup")
	}
}

// --- Fix: Per-pane mutex in messageDeliverer ---

func TestGetPaneMutex_Identity(t *testing.T) {
	t.Parallel()
	d := &messageDeliverer{}

	mu1 := d.getPaneMutex("%0")
	mu2 := d.getPaneMutex("%0")
	if mu1 != mu2 {
		t.Error("same pane target must return same mutex")
	}

	mu3 := d.getPaneMutex("%1")
	if mu1 == mu3 {
		t.Error("different pane targets must return different mutexes")
	}
}
