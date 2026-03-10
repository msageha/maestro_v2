package agent

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"regexp"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// mockPaneIO implements PaneIO for testing BusyDetector.
type mockPaneIO struct {
	currentCommand   string
	currentCommandFn func() (string, error)
	captureContent   string
	captureFn        func(paneTarget string, lastN int) (string, error)
	joinedContent    []string // successive CapturePaneJoined results
	joinedIdx        int
	joinedFn         func(paneTarget string, lastN int) (string, error)
	isShell          bool
}

func (m *mockPaneIO) FindPaneByAgentID(agentID string) (string, error) {
	return "%0", nil
}
func (m *mockPaneIO) SendCtrlC(paneTarget string) error { return nil }
func (m *mockPaneIO) SendKeys(paneTarget string, keys ...string) error {
	return nil
}
func (m *mockPaneIO) SendCommand(paneTarget, command string) error { return nil }
func (m *mockPaneIO) SendTextAndSubmit(ctx context.Context, paneTarget, text string) error {
	return nil
}
func (m *mockPaneIO) SetUserVar(paneTarget, name, value string) error { return nil }
func (m *mockPaneIO) GetUserVar(paneTarget, name string) (string, error) {
	return "", nil
}
func (m *mockPaneIO) GetPanePID(paneTarget string) (string, error) { return "12345", nil }

func (m *mockPaneIO) GetPaneCurrentCommand(paneTarget string) (string, error) {
	if m.currentCommandFn != nil {
		return m.currentCommandFn()
	}
	return m.currentCommand, nil
}

func (m *mockPaneIO) CapturePane(paneTarget string, lastN int) (string, error) {
	if m.captureFn != nil {
		return m.captureFn(paneTarget, lastN)
	}
	return m.captureContent, nil
}

func (m *mockPaneIO) CapturePaneJoined(paneTarget string, lastN int) (string, error) {
	if m.joinedFn != nil {
		return m.joinedFn(paneTarget, lastN)
	}
	if len(m.joinedContent) > 0 {
		content := m.joinedContent[m.joinedIdx%len(m.joinedContent)]
		m.joinedIdx++
		return content, nil
	}
	return m.captureContent, nil
}

func (m *mockPaneIO) IsShellCommand(cmd string) bool {
	return m.isShell
}

func (m *mockPaneIO) RespawnPane(paneTarget, startDir string) error { return nil }

// newTestBusyDetector creates a BusyDetector for testing.
// Uses direct field assignment to bypass NewBusyDetector's default normalization,
// allowing zero-value IdleStableSec/BusyCheckInterval for instant test execution.
func newTestBusyDetector(paneIO PaneIO, busyRegex *regexp.Regexp, cfg BusyDetectorConfig) *BusyDetector {
	return &BusyDetector{
		paneIO:    paneIO,
		busyRegex: busyRegex,
		config:    cfg,
		logger:    log.New(&bytes.Buffer{}, "", 0),
		logLevel:  LogLevelDebug,
	}
}

func fastConfig() BusyDetectorConfig {
	return BusyDetectorConfig{
		IdleStableSec:       0, // no sleep in tests
		BusyCheckMaxRetries: 3,
		BusyCheckInterval:   0,
	}
}

// --- Stage 1: Shell Command Detection ---

func TestDetectBusy_ShellCommand_ReturnsIdle(t *testing.T) {
	mock := &mockPaneIO{
		currentCommand: "bash",
		isShell:        true,
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle for shell command, got %s", verdict)
	}
}

func TestDetectBusy_GetCommandError_ReturnsUndecided(t *testing.T) {
	mock := &mockPaneIO{
		currentCommandFn: func() (string, error) {
			return "", fmt.Errorf("tmux not available")
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictUndecided {
		t.Errorf("expected VerdictUndecided on error, got %s", verdict)
	}
}

// --- Stage 2: Pattern Matching ---

func TestDetectBusy_NoPattern_StableContent_ReturnsIdle(t *testing.T) {
	content := "some stable content\n❯ "
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: content,
		joinedContent:  []string{content, content}, // same hash
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle (no pattern, stable), got %s", verdict)
	}
}

func TestDetectBusy_PatternMatched_StableContent_ReturnsUndecided(t *testing.T) {
	content := "Working on task..."
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: content,
		joinedContent:  []string{content, content}, // same hash but pattern matched
	}
	busyRegex := regexp.MustCompile("Working|Thinking")
	bd := newTestBusyDetector(mock, busyRegex, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictUndecided {
		t.Errorf("expected VerdictUndecided (pattern matched, stable), got %s", verdict)
	}
}

// --- Stage 3: Activity Probe ---

func TestDetectBusy_ContentChanging_ReturnsBusy(t *testing.T) {
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "initial content",
		joinedContent:  []string{"content v1", "content v2"}, // different hashes
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictBusy {
		t.Errorf("expected VerdictBusy (content changing), got %s", verdict)
	}
}

func TestDetectBusy_CaptureError_ReturnsUndecided(t *testing.T) {
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureFn: func(paneTarget string, lastN int) (string, error) {
			return "", fmt.Errorf("capture failed")
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictUndecided {
		t.Errorf("expected VerdictUndecided on capture error, got %s", verdict)
	}
}

func TestDetectBusy_JoinedCaptureError_ReturnsUndecided(t *testing.T) {
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "normal content",
		joinedFn: func(paneTarget string, lastN int) (string, error) {
			return "", fmt.Errorf("joined capture failed")
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictUndecided {
		t.Errorf("expected VerdictUndecided on joined capture error, got %s", verdict)
	}
}

// --- Context Cancellation ---

func TestDetectBusy_ContextCancelled_ReturnsUndecided(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "some content",
		joinedContent:  []string{"content"},
	}
	// Use non-zero idle time to trigger sleep
	cfg := BusyDetectorConfig{
		IdleStableSec:       1,
		BusyCheckMaxRetries: 1,
		BusyCheckInterval:   1,
	}
	bd := newTestBusyDetector(mock, nil, cfg)

	verdict := bd.DetectBusy(ctx, "%0")
	if verdict != VerdictUndecided {
		t.Errorf("expected VerdictUndecided on cancelled context, got %s", verdict)
	}
}

// --- DetectBusyWithRetry ---

func TestDetectBusyWithRetry_ImmediateIdle(t *testing.T) {
	mock := &mockPaneIO{
		currentCommand: "bash",
		isShell:        true,
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle on immediate idle, got %s", verdict)
	}
}

func TestDetectBusyWithRetry_BusyThenIdle(t *testing.T) {
	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "content",
		joinedFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			// First 2 calls (first detect round): changing content → busy
			// Second 2 calls (first retry): stable content → idle
			if callCount <= 2 {
				return fmt.Sprintf("content-%d", callCount), nil
			}
			return "stable-content", nil
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle after retry, got %s", verdict)
	}
}

func TestDetectBusyWithRetry_StaysBusy_ExhaustsRetries(t *testing.T) {
	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "content",
		joinedFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			return fmt.Sprintf("changing-%d", callCount), nil
		},
	}
	cfg := BusyDetectorConfig{
		IdleStableSec:       0,
		BusyCheckMaxRetries: 2,
		BusyCheckInterval:   0,
	}
	bd := newTestBusyDetector(mock, nil, cfg)

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictBusy {
		t.Errorf("expected VerdictBusy after exhausting retries, got %s", verdict)
	}
}

func TestDetectBusyWithRetry_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "content",
		joinedFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			return fmt.Sprintf("changing-%d", callCount), nil
		},
	}
	cfg := BusyDetectorConfig{
		IdleStableSec:       0,
		BusyCheckMaxRetries: 100,
		BusyCheckInterval:   1, // 1 second sleep per retry — ctx will cancel first
	}
	bd := newTestBusyDetector(mock, nil, cfg)

	verdict := bd.DetectBusyWithRetry(ctx, "%0", "worker1")
	if verdict != VerdictUndecided {
		t.Errorf("expected VerdictUndecided on context cancel, got %s", verdict)
	}
}

func TestDetectBusyWithRetry_UndecidedRetriesThenReturns(t *testing.T) {
	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			// All calls fail → persistent Undecided
			return "", fmt.Errorf("persistent capture error")
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictUndecided {
		t.Errorf("expected VerdictUndecided after retries, got %s", verdict)
	}
	// 1 initial + undecidedImmediateRetries retries = 3 total calls
	expectedCalls := 1 + undecidedImmediateRetries
	if callCount != expectedCalls {
		t.Errorf("expected %d captureFn calls (1 initial + %d retries), got %d",
			expectedCalls, undecidedImmediateRetries, callCount)
	}
}

func TestDetectBusyWithRetry_UndecidedThenIdle(t *testing.T) {
	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			if callCount <= 1 {
				return "", fmt.Errorf("transient capture error")
			}
			return "normal content", nil
		},
		joinedContent: []string{"stable", "stable"},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle after undecided retry, got %s", verdict)
	}
}

func TestDetectBusyWithRetry_BusyThenUndecidedThenIdle(t *testing.T) {
	// Simulates: first detect → Busy, then in retry loop detect → Undecided
	// (transient error), immediate retry → Idle. Tests that undecided retry
	// logic is applied within the busy retry loop.
	detectCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "content",
		joinedFn: func(paneTarget string, lastN int) (string, error) {
			detectCount++
			// First 2 calls (detect round 1): changing content → Busy
			if detectCount <= 2 {
				return fmt.Sprintf("changing-%d", detectCount), nil
			}
			// Next call (retry round, detect attempt 1): capture error → Undecided
			if detectCount == 3 {
				return "", fmt.Errorf("transient error")
			}
			// Subsequent calls (undecided immediate retry): stable → Idle
			return "stable-content", nil
		},
		captureFn: func(paneTarget string, lastN int) (string, error) {
			// After detect 3 triggers Undecided via joinedFn error,
			// the immediate retry calls DetectBusy again which calls CapturePane
			return "content", nil
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle after busy→undecided→idle, got %s", verdict)
	}
}

// --- BusyDetector Logging ---

func TestBusyDetector_LogPrefix(t *testing.T) {
	var buf bytes.Buffer
	mock := &mockPaneIO{
		currentCommand: "bash",
		isShell:        true,
	}
	bd := NewBusyDetector(mock, nil, fastConfig(), log.New(&buf, "", 0), LogLevelDebug)

	bd.DetectBusy(context.Background(), "%0")

	output := buf.String()
	if output == "" {
		t.Error("expected log output")
	}
	if !contains(output, "busy_detector:") {
		t.Errorf("expected 'busy_detector:' prefix in log, got: %s", output)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- NewBusyDetector defaults ---

func TestNewBusyDetector_ZeroConfigNormalized(t *testing.T) {
	bd := NewBusyDetector(&mockPaneIO{}, nil, BusyDetectorConfig{}, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	if bd.config.IdleStableSec != 5 {
		t.Errorf("IdleStableSec: got %d, want 5", bd.config.IdleStableSec)
	}
	if bd.config.BusyCheckMaxRetries != 30 {
		t.Errorf("BusyCheckMaxRetries: got %d, want 30", bd.config.BusyCheckMaxRetries)
	}
	if bd.config.BusyCheckInterval != 2 {
		t.Errorf("BusyCheckInterval: got %d, want 2", bd.config.BusyCheckInterval)
	}
}

func TestNewBusyDetector_ExplicitConfigPreserved(t *testing.T) {
	cfg := BusyDetectorConfig{
		IdleStableSec:       10,
		BusyCheckMaxRetries: 50,
		BusyCheckInterval:   5,
	}
	bd := NewBusyDetector(&mockPaneIO{}, nil, cfg, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	if bd.config.IdleStableSec != 10 {
		t.Errorf("IdleStableSec: got %d, want 10", bd.config.IdleStableSec)
	}
	if bd.config.BusyCheckMaxRetries != 50 {
		t.Errorf("BusyCheckMaxRetries: got %d, want 50", bd.config.BusyCheckMaxRetries)
	}
	if bd.config.BusyCheckInterval != 5 {
		t.Errorf("BusyCheckInterval: got %d, want 5", bd.config.BusyCheckInterval)
	}
}

// --- Executor → BusyDetector wiring ---

func TestNewExecutor_WiresBusyDetectorConfig(t *testing.T) {
	exec, err := newExecutor("", model.WatcherConfig{
		IdleStableSec:       8,
		BusyCheckMaxRetries: 20,
		BusyCheckInterval:   3,
		BusyPatterns:        "Working",
	}, "info", &bytes.Buffer{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bd := exec.busyDetector
	if bd == nil {
		t.Fatal("busyDetector should be initialized")
	}
	if bd.config.IdleStableSec != 8 {
		t.Errorf("IdleStableSec: got %d, want 8", bd.config.IdleStableSec)
	}
	if bd.config.BusyCheckMaxRetries != 20 {
		t.Errorf("BusyCheckMaxRetries: got %d, want 20", bd.config.BusyCheckMaxRetries)
	}
	if bd.config.BusyCheckInterval != 3 {
		t.Errorf("BusyCheckInterval: got %d, want 3", bd.config.BusyCheckInterval)
	}
	if bd.busyRegex == nil {
		t.Error("busyRegex should be compiled from WatcherConfig.BusyPatterns")
	}
}

func TestNewExecutor_DefaultConfig_WiresBusyDetectorDefaults(t *testing.T) {
	exec, err := newExecutor("", model.WatcherConfig{}, "info", &bytes.Buffer{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bd := exec.busyDetector
	if bd == nil {
		t.Fatal("busyDetector should be initialized")
	}
	// applyDefaults normalizes WatcherConfig before BusyDetector construction
	if bd.config.IdleStableSec != 5 {
		t.Errorf("IdleStableSec: got %d, want 5 (default)", bd.config.IdleStableSec)
	}
	if bd.config.BusyCheckMaxRetries != 30 {
		t.Errorf("BusyCheckMaxRetries: got %d, want 30 (default)", bd.config.BusyCheckMaxRetries)
	}
	if bd.config.BusyCheckInterval != 2 {
		t.Errorf("BusyCheckInterval: got %d, want 2 (default)", bd.config.BusyCheckInterval)
	}
}
