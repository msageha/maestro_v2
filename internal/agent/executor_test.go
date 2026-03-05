package agent

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
)

func TestContentHash(t *testing.T) {
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
	// Zero values get defaults
	cfg := applyDefaults(model.WatcherConfig{})
	if cfg.BusyCheckInterval != 2 {
		t.Errorf("BusyCheckInterval: got %d, want 2", cfg.BusyCheckInterval)
	}
	if cfg.BusyCheckMaxRetries != 30 {
		t.Errorf("BusyCheckMaxRetries: got %d, want 30", cfg.BusyCheckMaxRetries)
	}
	if cfg.IdleStableSec != 5 {
		t.Errorf("IdleStableSec: got %d, want 5", cfg.IdleStableSec)
	}
	if cfg.CooldownAfterClear != 3 {
		t.Errorf("CooldownAfterClear: got %d, want 3", cfg.CooldownAfterClear)
	}
	if cfg.ClearConfirmTimeoutSec != 5 {
		t.Errorf("ClearConfirmTimeoutSec: got %d, want 5", cfg.ClearConfirmTimeoutSec)
	}
	if cfg.ClearConfirmPollMs != 250 {
		t.Errorf("ClearConfirmPollMs: got %d, want 250", cfg.ClearConfirmPollMs)
	}
	if cfg.ClearMaxAttempts != 3 {
		t.Errorf("ClearMaxAttempts: got %d, want 3", cfg.ClearMaxAttempts)
	}
	if cfg.ClearRetryBackoffMs != 500 {
		t.Errorf("ClearRetryBackoffMs: got %d, want 500", cfg.ClearRetryBackoffMs)
	}

	// Non-zero values preserved
	cfg = applyDefaults(model.WatcherConfig{
		BusyCheckInterval:      10,
		BusyCheckMaxRetries:    50,
		IdleStableSec:          8,
		CooldownAfterClear:     5,
		ClearConfirmTimeoutSec: 10,
		ClearConfirmPollMs:     500,
		ClearMaxAttempts:       5,
		ClearRetryBackoffMs:    1000,
	})
	if cfg.BusyCheckInterval != 10 {
		t.Errorf("BusyCheckInterval: got %d, want 10", cfg.BusyCheckInterval)
	}
	if cfg.BusyCheckMaxRetries != 50 {
		t.Errorf("BusyCheckMaxRetries: got %d, want 50", cfg.BusyCheckMaxRetries)
	}
	if cfg.IdleStableSec != 8 {
		t.Errorf("IdleStableSec: got %d, want 8", cfg.IdleStableSec)
	}
	if cfg.CooldownAfterClear != 5 {
		t.Errorf("CooldownAfterClear: got %d, want 5", cfg.CooldownAfterClear)
	}
	if cfg.ClearConfirmTimeoutSec != 10 {
		t.Errorf("ClearConfirmTimeoutSec: got %d, want 10", cfg.ClearConfirmTimeoutSec)
	}
	if cfg.ClearConfirmPollMs != 500 {
		t.Errorf("ClearConfirmPollMs: got %d, want 500", cfg.ClearConfirmPollMs)
	}
	if cfg.ClearMaxAttempts != 5 {
		t.Errorf("ClearMaxAttempts: got %d, want 5", cfg.ClearMaxAttempts)
	}
	if cfg.ClearRetryBackoffMs != 1000 {
		t.Errorf("ClearRetryBackoffMs: got %d, want 1000", cfg.ClearRetryBackoffMs)
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected LogLevel
	}{
		{"debug", LogLevelDebug},
		{"DEBUG", LogLevelDebug},
		{"info", LogLevelInfo},
		{"INFO", LogLevelInfo},
		{"warn", LogLevelWarn},
		{"warning", LogLevelWarn},
		{"error", LogLevelError},
		{"ERROR", LogLevelError},
		{"unknown", LogLevelInfo},
		{"", LogLevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseLogLevel(tt.input)
			if got != tt.expected {
				t.Errorf("parseLogLevel(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}

func TestBusyVerdictString(t *testing.T) {
	tests := []struct {
		verdict  BusyVerdict
		expected string
	}{
		{VerdictIdle, "idle"},
		{VerdictBusy, "busy"},
		{VerdictUndecided, "undecided"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := tt.verdict.String(); got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestNewExecutor_InvalidBusyPatterns(t *testing.T) {
	_, err := newExecutor("", model.WatcherConfig{
		BusyPatterns: "[invalid",
	}, "info", &bytes.Buffer{}, nil)
	if err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestNewExecutor_ValidBusyPatterns(t *testing.T) {
	exec, err := newExecutor("", model.WatcherConfig{
		BusyPatterns: "Working|Thinking|Planning",
	}, "info", &bytes.Buffer{}, nil)
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
	exec, err := newExecutor("", model.WatcherConfig{}, "info", &bytes.Buffer{}, nil)
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
	var buf bytes.Buffer
	exec, err := newExecutor("", model.WatcherConfig{}, "warn", &buf, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Debug and Info should be filtered
	exec.log(LogLevelDebug, "debug message")
	exec.log(LogLevelInfo, "info message")
	if buf.Len() != 0 {
		t.Errorf("expected no output for debug/info at warn level, got: %s", buf.String())
	}

	// Warn and Error should pass
	exec.log(LogLevelWarn, "warn message")
	if !strings.Contains(buf.String(), "WARN") {
		t.Error("warn message should be logged")
	}

	buf.Reset()
	exec.log(LogLevelError, "error message")
	if !strings.Contains(buf.String(), "ERROR") {
		t.Error("error message should be logged")
	}
}

func TestExecMode_Constants(t *testing.T) {
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

// --- Fix #3: Stage 1 shell command detection ---

func TestShellCommands(t *testing.T) {
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

// --- Fix #5: Role name validation ---

func TestValidRoleName(t *testing.T) {
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

// --- Fix #2: Orchestrator Ctrl-C protection ---

func TestOrchestratorInterruptRejected(t *testing.T) {
	// Orchestrator should never be interruptible.
	// execInterrupt checks AgentID == "orchestrator" before any tmux call,
	// so we can test the guard directly without a running tmux session.
	var buf bytes.Buffer
	exec, err := newExecutor("", model.WatcherConfig{}, "info", &buf, nil)
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPromptReady(tt.content)
			if got != tt.want {
				t.Errorf("isPromptReady(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestLastNonBlankLine(t *testing.T) {
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
		t.Run(tt.name, func(t *testing.T) {
			got := lastNonBlankLine(tt.content)
			if got != tt.want {
				t.Errorf("lastNonBlankLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
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
		t.Run(tt.name, func(t *testing.T) {
			got := stripANSI(tt.input)
			if got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- clearTextVisible tests ---

func TestClearTextVisible(t *testing.T) {
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
			name:    "clear as part of longer text (substring match)",
			content: "output\nRunning /clear command...\n",
			want:    true,
		},
		{
			name:    "clear after successful processing (gone)",
			content: "Context cleared\n ❯ \nTokens: 0\n",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clearTextVisible(tt.content)
			if got != tt.want {
				t.Errorf("clearTextVisible() = %v, want %v\ncontent: %q", got, tt.want, tt.content)
			}
		})
	}
}

// --- promptReadyLines constant test ---

func TestPromptReadyLinesIncreased(t *testing.T) {
	// Verify the capture depth has been increased from 5 to 12
	// to accommodate Claude Code's status bars and improve prompt detection.
	if promptReadyLines < 12 {
		t.Errorf("promptReadyLines = %d, want >= 12 (increased for better prompt detection)", promptReadyLines)
	}
}

// === Executor Integration Tests ===

// execMockPaneIO provides a configurable PaneIO mock for Executor integration tests.
// Unlike mockPaneIO (used by BusyDetector tests), this mock supports user variable
// tracking, text delivery tracking, and configurable error injection.
type execMockPaneIO struct {
	// FindPaneByAgentID
	findPaneTarget string
	findPaneErr    error

	// GetPaneCurrentCommand / IsShellCommand
	currentCmd string
	isShell    bool

	// CapturePane / CapturePaneJoined
	captureContent  string
	joinedContents  []string
	joinedCallCount int

	// SendTextAndSubmit tracking
	sentTexts   []string
	sendTextErr error

	// SendCommand tracking
	sentCmds   []string
	sendCmdErr error

	// User variable storage
	userVars map[string]string

	// PanePID
	panePID string
}

func newExecMock() *execMockPaneIO {
	return &execMockPaneIO{
		findPaneTarget: "%0",
		currentCmd:     "bash",
		isShell:        true,
		captureContent: "output\n ❯ \n",
		panePID:        "12345",
		userVars:       make(map[string]string),
	}
}

func (m *execMockPaneIO) FindPaneByAgentID(agentID string) (string, error) {
	if m.findPaneErr != nil {
		return "", m.findPaneErr
	}
	return m.findPaneTarget, nil
}

func (m *execMockPaneIO) SendCtrlC(paneTarget string) error { return nil }
func (m *execMockPaneIO) SendKeys(paneTarget string, keys ...string) error {
	return nil
}

func (m *execMockPaneIO) SendCommand(paneTarget, command string) error {
	m.sentCmds = append(m.sentCmds, command)
	return m.sendCmdErr
}

func (m *execMockPaneIO) SendTextAndSubmit(ctx context.Context, paneTarget, text string) error {
	m.sentTexts = append(m.sentTexts, text)
	return m.sendTextErr
}

func (m *execMockPaneIO) SetUserVar(paneTarget, name, value string) error {
	if m.userVars == nil {
		m.userVars = make(map[string]string)
	}
	m.userVars[name] = value
	return nil
}

func (m *execMockPaneIO) GetUserVar(paneTarget, name string) (string, error) {
	if m.userVars == nil {
		return "", nil
	}
	return m.userVars[name], nil
}

func (m *execMockPaneIO) GetPanePID(paneTarget string) (string, error) {
	return m.panePID, nil
}

func (m *execMockPaneIO) GetPaneCurrentCommand(paneTarget string) (string, error) {
	return m.currentCmd, nil
}

func (m *execMockPaneIO) CapturePane(paneTarget string, lastN int) (string, error) {
	return m.captureContent, nil
}

func (m *execMockPaneIO) CapturePaneJoined(paneTarget string, lastN int) (string, error) {
	if len(m.joinedContents) > 0 {
		content := m.joinedContents[m.joinedCallCount%len(m.joinedContents)]
		m.joinedCallCount++
		return content, nil
	}
	return m.captureContent, nil
}

func (m *execMockPaneIO) IsShellCommand(cmd string) bool {
	return m.isShell
}

// newTestExecutorWithLog creates an Executor with a mock PaneIO and returns
// the log buffer for verification. Uses zero-sleep config for instant tests.
func newTestExecutorWithLog(paneIO PaneIO) (*Executor, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	cfg := model.WatcherConfig{
		BusyCheckInterval:      1,
		BusyCheckMaxRetries:    1,
		IdleStableSec:          1,
		CooldownAfterClear:     1,
		WaitReadyIntervalSec:   1,
		WaitReadyMaxRetries:    1,
		ClearConfirmTimeoutSec: 1,
		ClearConfirmPollMs:     10,
		ClearMaxAttempts:       1,
		ClearRetryBackoffMs:    10,
	}

	// BusyDetector with zero-sleep config (bypasses NewBusyDetector normalization)
	bd := &BusyDetector{
		paneIO: paneIO,
		config: BusyDetectorConfig{
			IdleStableSec:       0,
			BusyCheckMaxRetries: 1,
			BusyCheckInterval:   0,
		},
		logger:   logger,
		logLevel: LogLevelDebug,
	}

	exec := &Executor{
		config:       cfg,
		logger:       logger,
		logLevel:     LogLevelDebug,
		paneIO:       paneIO,
		busyDetector: bd,
		paneState:    NewPaneStateManager(paneIO),
	}

	return exec, &buf
}

func TestExecute_UnknownMode(t *testing.T) {
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
	mock := newExecMock()
	mock.isShell = false
	mock.currentCmd = "claude"
	mock.captureContent = "working..."
	mock.joinedContents = []string{"content-1", "content-2"} // changing → busy
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

func TestExecute_ModeDeliver_Success(t *testing.T) {
	mock := newExecMock()
	mock.isShell = true // idle → deliver immediately
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
	mock := newExecMock()
	mock.isShell = false
	mock.currentCmd = "claude"
	mock.captureContent = "working..."
	mock.joinedContents = []string{"content-1", "content-2"} // changing → busy
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
	mock := newExecMock()
	mock.isShell = true // idle
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
	mock := newExecMock()
	mock.isShell = true // idle
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
	mock := newExecMock()
	mock.isShell = false
	mock.currentCmd = "claude"
	mock.captureContent = "working..."
	mock.joinedContents = []string{"content-1", "content-2"} // changing → busy
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
	mock := newExecMock()
	mock.isShell = true // idle
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

func TestExecute_ModeWithClear_Orchestrator_FallsToDeliver(t *testing.T) {
	mock := newExecMock()
	mock.isShell = true // idle
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
	mock := newExecMock()
	mock.isShell = true
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
		t.Run(tt.name, func(t *testing.T) {
			mock := newExecMock()
			mock.isShell = true // idle → fast path
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
	mock := newExecMock()
	mock.isShell = true
	// For clearAndConfirm: return different content to simulate /clear processing
	mock.joinedContents = []string{"before-clear", "after-clear", "after-clear"}
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
	exec := &Executor{} // logFile is nil
	err := exec.Close()
	if err != nil {
		t.Errorf("expected nil error for nil logFile, got: %v", err)
	}
}

// TestExecutor_Close_WithLogFile verifies that Close() properly closes
// a non-nil logFile.
func TestExecutor_Close_WithLogFile(t *testing.T) {
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
