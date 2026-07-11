package agent

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

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
		GetPaneCurrentCommandFn: func(_ string) (string, error) {
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

func TestDetectBusy_PatternMatched_StableContent_ReturnsIdle(t *testing.T) {
	// Stable content → idle, even when busy pattern matches stale output.
	content := "Working on task..."
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: content,
		joinedContent:  []string{content, content}, // same hash, pattern matched but stable
	}
	busyRegex := regexp.MustCompile("Working|Thinking")
	bd := newTestBusyDetector(mock, busyRegex, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle (stable content, stale pattern), got %s", verdict)
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
		CapturePaneFn: func(paneTarget string, lastN int) (string, error) {
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
		CapturePaneJoinedFn: func(paneTarget string, lastN int) (string, error) {
			return "", fmt.Errorf("joined capture failed")
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictUndecided {
		t.Errorf("expected VerdictUndecided on joined capture error, got %s", verdict)
	}
}

// --- CPU-based busy override (silent subprocess detection) ---

// fakeProcessSampler returns values from seq in order (clamped to the last
// entry once exhausted), or err on every call if set. rootPID is recorded so
// tests can assert the probe forwarded the parsed pane_pid correctly.
type fakeProcessSampler struct {
	seq        []float64
	idx        int
	err        error
	gotRootPID []int
}

func (f *fakeProcessSampler) descendantCPUSeconds(_ context.Context, rootPID int) (float64, error) {
	f.gotRootPID = append(f.gotRootPID, rootPID)
	if f.err != nil {
		return 0, f.err
	}
	if len(f.seq) == 0 {
		return 0, nil
	}
	i := f.idx
	if i >= len(f.seq) {
		i = len(f.seq) - 1
	}
	f.idx++
	return f.seq[i], nil
}

// fastCPUConfig is fastConfig() plus a short, real (but sub-100ms) CPU probe
// window so the fast-path override tests exercise the actual sleep+threshold
// logic without materially slowing the suite. Threshold = window * 0.5.
func fastCPUConfig() busyDetectorConfig {
	cfg := fastConfig()
	cfg.CPUProbeWindow = 20 * time.Millisecond // threshold = 10ms
	return cfg
}

func TestDetectBusy_FastPathWouldBeIdle_CPUIncreaseOverridesToBusy(t *testing.T) {
	// Prompt glyph visible, no busy pattern: would hit the Bug-N fast path
	// and return VerdictIdle, except a live subprocess is silently
	// accumulating CPU time (e.g. a `make` build with no terminal output).
	// Delta (1.0s) is far above the 10ms threshold.
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "❯ ",
		panePID:        "4242",
	}
	bd := newTestBusyDetector(mock, nil, fastCPUConfig())
	bd.processSampler = &fakeProcessSampler{seq: []float64{10, 11}}

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictBusy {
		t.Errorf("expected VerdictBusy (CPU probe overrides fast-path idle), got %s", verdict)
	}
}

func TestDetectBusy_FastPathIdle_NoCPUIncrease_StaysIdle(t *testing.T) {
	// Same setup as above, but the sampler shows no CPU movement: the
	// override must not fire, preserving the existing fast-path idle verdict.
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "❯ ",
		panePID:        "4242",
	}
	bd := newTestBusyDetector(mock, nil, fastCPUConfig())
	bd.processSampler = &fakeProcessSampler{seq: []float64{10, 10}} // no change

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle (no CPU movement), got %s", verdict)
	}
}

func TestDetectBusy_FastPathIdle_CPUIncreaseBelowThreshold_StaysIdle(t *testing.T) {
	// A small delta (light background chatter from an idle MCP server, dev
	// server, etc.) below the 50%-of-window threshold must not flip the
	// verdict -- otherwise any pane with a noisy-but-idle child process
	// would be permanently misjudged busy.
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "❯ ",
		panePID:        "4242",
	}
	bd := newTestBusyDetector(mock, nil, fastCPUConfig()) // threshold = 10ms
	bd.processSampler = &fakeProcessSampler{seq: []float64{10, 10.001}}

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle (delta below threshold), got %s", verdict)
	}
}

func TestDetectBusy_Stage3StableContent_CPUIncreaseOverridesToBusy(t *testing.T) {
	// No prompt glyph in the captured content, so the Bug-N fast path is
	// skipped and Stage 3 runs. Joined content is stable (same hash both
	// captures), which would normally mean idle -- except CPU time in the
	// pane's process tree increased substantially during the reused
	// activity-probe window (stableSec=1s, threshold=0.5s).
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "compiling silently, no prompt marker here",
		joinedContent:  []string{"same stable content"}, // rotates: same hash both captures
		panePID:        "4242",
	}
	cfg := fastConfig()
	cfg.IdleStableSec = 1
	bd := newTestBusyDetector(mock, nil, cfg)
	bd.processSampler = &fakeProcessSampler{seq: []float64{100, 101}}

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictBusy {
		t.Errorf("expected VerdictBusy (CPU probe overrides Stage 3 idle), got %s", verdict)
	}
}

func TestDetectBusy_Stage3StableContent_NoCPUIncrease_StaysIdle(t *testing.T) {
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "compiling silently, no prompt marker here",
		joinedContent:  []string{"same stable content"},
		panePID:        "4242",
	}
	cfg := fastConfig()
	cfg.IdleStableSec = 1
	bd := newTestBusyDetector(mock, nil, cfg)
	bd.processSampler = &fakeProcessSampler{seq: []float64{100, 100}}

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle (no CPU movement), got %s", verdict)
	}
}

func TestDetectBusy_Stage3StableContent_CPUIncreaseBelowThreshold_StaysIdle(t *testing.T) {
	// Small delta (0.1s) well below the 0.5s threshold (stableSec=1s): must
	// not override, same rationale as the fast-path threshold test above.
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "compiling silently, no prompt marker here",
		joinedContent:  []string{"same stable content"},
		panePID:        "4242",
	}
	cfg := fastConfig()
	cfg.IdleStableSec = 1
	bd := newTestBusyDetector(mock, nil, cfg)
	bd.processSampler = &fakeProcessSampler{seq: []float64{100, 100.1}}

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle (delta below threshold), got %s", verdict)
	}
}

func TestDetectBusy_NoProcessSampler_UnaffectedByCPUProbe(t *testing.T) {
	// processSampler left nil (the zero value from newTestBusyDetector):
	// processBusyProbe must short-circuit to false without touching the OS,
	// preserving pre-existing behavior for every caller that doesn't opt in.
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "❯ ",
		panePID:        "4242",
	}
	bd := newTestBusyDetector(mock, nil, fastCPUConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle (no processSampler configured), got %s", verdict)
	}
}

func TestDetectBusy_DisableCPUProbe_UnaffectedByCPUProbe(t *testing.T) {
	// DisableCPUProbe is the operator kill switch: even with a sampler
	// configured and a real CPU increase, the override must not fire.
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "❯ ",
		panePID:        "4242",
	}
	cfg := fastCPUConfig()
	cfg.DisableCPUProbe = true
	bd := newTestBusyDetector(mock, nil, cfg)
	bd.processSampler = &fakeProcessSampler{seq: []float64{10, 11}}

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle (CPU probe disabled), got %s", verdict)
	}
}

func TestProcessBusyProbe_ForwardsParsedPanePID(t *testing.T) {
	mock := &mockPaneIO{panePID: "4242"}
	bd := newTestBusyDetector(mock, nil, fastCPUConfig())
	sampler := &fakeProcessSampler{seq: []float64{10, 20}}
	bd.processSampler = sampler

	bd.processBusyProbe(context.Background(), "%0")

	if len(sampler.gotRootPID) != 2 {
		t.Fatalf("expected 2 sampler calls (before+after), got %d", len(sampler.gotRootPID))
	}
	for _, got := range sampler.gotRootPID {
		if got != 4242 {
			t.Errorf("expected rootPID=4242 forwarded to sampler, got %d", got)
		}
	}
}

func TestProcessBusyProbe_PanePIDError_ReturnsFalse(t *testing.T) {
	mock := &mockPaneIO{getPIDErr: fmt.Errorf("tmux display-message failed")}
	bd := newTestBusyDetector(mock, nil, fastCPUConfig())
	bd.processSampler = &fakeProcessSampler{seq: []float64{10, 20}}

	if bd.processBusyProbe(context.Background(), "%0") {
		t.Error("expected false when GetPanePID errors")
	}
}

func TestProcessBusyProbe_UnparsablePanePID_ReturnsFalse(t *testing.T) {
	mock := &mockPaneIO{panePID: "not-a-pid"}
	bd := newTestBusyDetector(mock, nil, fastCPUConfig())
	bd.processSampler = &fakeProcessSampler{seq: []float64{10, 20}}

	if bd.processBusyProbe(context.Background(), "%0") {
		t.Error("expected false when pane_pid is not a valid integer")
	}
}

func TestProcessBusyProbe_SamplerError_ReturnsFalse(t *testing.T) {
	mock := &mockPaneIO{panePID: "4242"}
	bd := newTestBusyDetector(mock, nil, fastCPUConfig())
	bd.processSampler = &fakeProcessSampler{err: fmt.Errorf("ps: command not found")}

	if bd.processBusyProbe(context.Background(), "%0") {
		t.Error("expected false when the process sampler errors")
	}
}

func TestProcessBusyProbe_ContextCancelled_ReturnsTrue(t *testing.T) {
	// Fails closed to "busy" during the probe sleep, consistent with every
	// other sleep in this file (Stage 3, busy retry, soft retry): an
	// idle-preserving false here would be the one sleep that treats
	// cancellation as evidence of idleness instead of "state unknown".
	mock := &mockPaneIO{panePID: "4242"}
	// Non-zero probe window so the cancellation actually races the sleep.
	cfg := fastConfig()
	cfg.CPUProbeWindow = 5 * time.Second
	bd := newTestBusyDetector(mock, nil, cfg)
	bd.processSampler = &fakeProcessSampler{seq: []float64{10, 20}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if !bd.processBusyProbe(ctx, "%0") {
		t.Error("expected true (fail-closed to busy) when context is cancelled during the probe sleep")
	}
}

// --- Context Cancellation ---

func TestDetectBusy_ContextCancelled_ReturnsBusy(t *testing.T) {
	// Context cancelled during the Stage 3 activity-probe sleep should return
	// VerdictBusy (not VerdictUndecided): Claude is confirmed running from
	// Stage 1, so the conservative "busy" verdict is more accurate.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "some content",
		joinedContent:  []string{"content"},
	}
	// Use non-zero idle time to trigger sleep
	cfg := busyDetectorConfig{
		IdleStableSec:       1,
		BusyCheckMaxRetries: 1,
		BusyCheckInterval:   1,
	}
	bd := newTestBusyDetector(mock, nil, cfg)

	verdict := bd.DetectBusy(ctx, "%0")
	if verdict != VerdictBusy {
		t.Errorf("expected VerdictBusy on cancelled context, got %s", verdict)
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
		CapturePaneJoinedFn: func(paneTarget string, lastN int) (string, error) {
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
		CapturePaneJoinedFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			return fmt.Sprintf("changing-%d", callCount), nil
		},
	}
	cfg := busyDetectorConfig{
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
	// Context cancelled during the busy-retry sleep should return VerdictBusy.
	// We entered the retry loop because the agent was observed busy; the
	// context expiring doesn't change that — return the known state.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "content",
		CapturePaneJoinedFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			return fmt.Sprintf("changing-%d", callCount), nil
		},
	}
	cfg := busyDetectorConfig{
		IdleStableSec:       0,
		BusyCheckMaxRetries: 100,
		BusyCheckInterval:   1, // 1 second sleep per retry — ctx will cancel first
	}
	bd := newTestBusyDetector(mock, nil, cfg)

	verdict := bd.DetectBusyWithRetry(ctx, "%0", "worker1")
	if verdict != VerdictBusy {
		t.Errorf("expected VerdictBusy on context cancel during retry sleep, got %s", verdict)
	}
}

func TestDetectBusyWithRetry_UndecidedRetriesThenPromotedToIdle(t *testing.T) {
	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		CapturePaneFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			// All calls fail → persistent Undecided
			return "", fmt.Errorf("persistent capture error")
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	// Persistent undecided across soft retries is promoted to idle.
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle (promoted) after soft retries, got %s", verdict)
	}
	// Calls: initial detectWithUndecidedRetry (1+undecidedImmediateRetries=1) +
	//        soft retries × detectBusy (undecidedSoftRetries × 1 = 2) = 3 total
	expectedCalls := (1 + undecidedImmediateRetries) + undecidedSoftRetries
	if callCount != expectedCalls {
		t.Errorf("expected %d CapturePaneFn calls, got %d", expectedCalls, callCount)
	}
}

func TestDetectBusyWithRetry_UndecidedThenIdle(t *testing.T) {
	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		CapturePaneFn: func(paneTarget string, lastN int) (string, error) {
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
		CapturePaneJoinedFn: func(paneTarget string, lastN int) (string, error) {
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
		CapturePaneFn: func(paneTarget string, lastN int) (string, error) {
			// After detect 3 triggers Undecided via CapturePaneJoinedFn error,
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

// --- busyDetector Logging ---

func TestBusyDetector_LogPrefix(t *testing.T) {
	var buf bytes.Buffer
	mock := &mockPaneIO{
		currentCommand: "bash",
		isShell:        true,
	}
	bd := newBusyDetector(mock, nil, fastConfig(), log.New(&buf, "", 0), logLevelDebug)

	bd.DetectBusy(context.Background(), "%0")

	output := buf.String()
	if output == "" {
		t.Error("expected log output")
	}
	if !strings.Contains(output, "busy_detector:") {
		t.Errorf("expected 'busy_detector:' prefix in log, got: %s", output)
	}
}

// --- newBusyDetector defaults ---

func TestNewBusyDetector_ZeroConfig_BusyHintLinesNormalized(t *testing.T) {
	bd := newBusyDetector(&mockPaneIO{}, nil, busyDetectorConfig{}, log.New(&bytes.Buffer{}, "", 0), logLevelDebug)

	// BusyHintLines and CPUProbeWindow are the fields normalized by newBusyDetector.
	if bd.config.BusyHintLines != 12 {
		t.Errorf("BusyHintLines: got %d, want 12", bd.config.BusyHintLines)
	}
	if bd.config.CPUProbeWindow != defaultCPUProbeWindow {
		t.Errorf("CPUProbeWindow: got %v, want %v", bd.config.CPUProbeWindow, defaultCPUProbeWindow)
	}
	// Other fields are expected to be pre-normalized via applyDefaults;
	// newBusyDetector no longer normalizes them.
	if bd.config.IdleStableSec != 0 {
		t.Errorf("IdleStableSec: got %d, want 0 (not normalized by newBusyDetector)", bd.config.IdleStableSec)
	}
	if bd.config.BusyCheckMaxRetries != 0 {
		t.Errorf("BusyCheckMaxRetries: got %d, want 0 (not normalized by newBusyDetector)", bd.config.BusyCheckMaxRetries)
	}
	if bd.config.BusyCheckInterval != 0 {
		t.Errorf("BusyCheckInterval: got %d, want 0 (not normalized by newBusyDetector)", bd.config.BusyCheckInterval)
	}
}

func TestNewBusyDetector_ExplicitConfigPreserved(t *testing.T) {
	cfg := busyDetectorConfig{
		IdleStableSec:       10,
		BusyCheckMaxRetries: 50,
		BusyCheckInterval:   5,
	}
	bd := newBusyDetector(&mockPaneIO{}, nil, cfg, log.New(&bytes.Buffer{}, "", 0), logLevelDebug)

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

// --- Executor → busyDetector wiring ---

func TestNewExecutor_WiresBusyDetectorConfig(t *testing.T) {
	exec, err := newExecutor("", model.WatcherConfig{
		IdleStableSec:       8,
		BusyCheckMaxRetries: 20,
		BusyCheckInterval:   3,
		BusyPatterns:        "Working",
	}, "info", &bytes.Buffer{}, nil, DefaultExecutorConfig())
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
	exec, err := newExecutor("", model.WatcherConfig{}, "info", &bytes.Buffer{}, nil, DefaultExecutorConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bd := exec.busyDetector
	if bd == nil {
		t.Fatal("busyDetector should be initialized")
	}
	// applyDefaults normalizes WatcherConfig before busyDetector construction
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

// --- mockPaneIO Fn callback error injection ---

func TestMockPaneIO_SendCtrlCFn_ErrorInjection(t *testing.T) {
	injectedErr := fmt.Errorf("ctrl-c failed")
	mock := &mockPaneIO{
		SendCtrlCFn: func(paneTarget string) error {
			return injectedErr
		},
	}
	if err := mock.SendCtrlC("%0"); err != injectedErr {
		t.Errorf("expected injected error, got %v", err)
	}
}

func TestMockPaneIO_SendCtrlCFn_NilDefault(t *testing.T) {
	mock := &mockPaneIO{}
	if err := mock.SendCtrlC("%0"); err != nil {
		t.Errorf("expected nil error when SendCtrlCFn is nil, got %v", err)
	}
}

func TestMockPaneIO_SendCommandFn_ErrorInjection(t *testing.T) {
	injectedErr := fmt.Errorf("send command failed")
	mock := &mockPaneIO{
		SendCommandFn: func(paneTarget, command string) error {
			return injectedErr
		},
	}
	if err := mock.SendCommand("%0", "echo hello"); err != injectedErr {
		t.Errorf("expected injected error, got %v", err)
	}
}

func TestMockPaneIO_SendCommandFn_NilDefault(t *testing.T) {
	mock := &mockPaneIO{}
	if err := mock.SendCommand("%0", "echo hello"); err != nil {
		t.Errorf("expected nil error when SendCommandFn is nil, got %v", err)
	}
}

func TestMockPaneIO_FindPaneByAgentIDFn_ErrorInjection(t *testing.T) {
	injectedErr := fmt.Errorf("pane not found")
	mock := &mockPaneIO{
		FindPaneByAgentIDFn: func(agentID string) (string, error) {
			return "", injectedErr
		},
	}
	_, err := mock.FindPaneByAgentID("worker1")
	if err != injectedErr {
		t.Errorf("expected injected error, got %v", err)
	}
}

func TestMockPaneIO_RespawnPaneFn_ErrorInjection(t *testing.T) {
	injectedErr := fmt.Errorf("respawn failed")
	mock := &mockPaneIO{
		RespawnPaneFn: func(paneTarget, startDir string) error {
			return injectedErr
		},
	}
	if err := mock.RespawnPane("%0", "/tmp"); err != injectedErr {
		t.Errorf("expected injected error, got %v", err)
	}
}

func TestMockPaneIO_GetPanePIDFn_ErrorInjection(t *testing.T) {
	injectedErr := fmt.Errorf("pid lookup failed")
	mock := &mockPaneIO{
		GetPanePIDFn: func(paneTarget string) (string, error) {
			return "", injectedErr
		},
	}
	_, err := mock.GetPanePID("%0")
	if err != injectedErr {
		t.Errorf("expected injected error, got %v", err)
	}
}

func TestMockPaneIO_IsShellCommandFn_Override(t *testing.T) {
	mock := &mockPaneIO{
		isShell: false,
		IsShellCommandFn: func(cmd string) bool {
			return cmd == "zsh"
		},
	}
	if !mock.IsShellCommand("zsh") {
		t.Error("expected IsShellCommandFn to override isShell field")
	}
	if mock.IsShellCommand("claude") {
		t.Error("expected false for non-shell command")
	}
}

// --- busyDetector: multiple simultaneous errors ---

func TestDetectBusy_CommandAndCaptureErrors_ReturnsUndecided(t *testing.T) {
	// When GetPaneCurrentCommand errors, DetectBusy returns Undecided
	// before CapturePane is ever called.
	captureCallCount := 0
	mock := &mockPaneIO{
		GetPaneCurrentCommandFn: func(_ string) (string, error) {
			return "", fmt.Errorf("command error")
		},
		CapturePaneFn: func(paneTarget string, lastN int) (string, error) {
			captureCallCount++
			return "", fmt.Errorf("capture error")
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictUndecided {
		t.Errorf("expected VerdictUndecided, got %s", verdict)
	}
	if captureCallCount != 0 {
		t.Errorf("CapturePane should not be called after command error, got %d calls", captureCallCount)
	}
}

func TestDetectBusy_CaptureAndJoinedErrors_ReturnsUndecided(t *testing.T) {
	// CapturePane error → Undecided returned before CapturePaneJoined is called.
	joinedCallCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		CapturePaneFn: func(paneTarget string, lastN int) (string, error) {
			return "", fmt.Errorf("capture error")
		},
		CapturePaneJoinedFn: func(paneTarget string, lastN int) (string, error) {
			joinedCallCount++
			return "", fmt.Errorf("joined error")
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusy(context.Background(), "%0")
	if verdict != VerdictUndecided {
		t.Errorf("expected VerdictUndecided, got %s", verdict)
	}
	if joinedCallCount != 0 {
		t.Errorf("CapturePaneJoined should not be called after capture error, got %d calls", joinedCallCount)
	}
}

// --- busyDetector: error recovery paths ---

func TestDetectBusyWithRetry_CaptureErrorThenRecovery_ReturnsIdle(t *testing.T) {
	// First DetectBusy call: CapturePane errors → Undecided.
	// Immediate retry: CapturePane succeeds, stable content → Idle.
	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		CapturePaneFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			if callCount == 1 {
				return "", fmt.Errorf("transient capture error")
			}
			return "stable content", nil
		},
		joinedContent: []string{"stable", "stable"},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle after capture error recovery, got %s", verdict)
	}
}

func TestDetectBusyWithRetry_JoinedErrorThenRecovery_ReturnsIdle(t *testing.T) {
	// First DetectBusy: CapturePaneJoined errors → Undecided.
	// Immediate retry: CapturePaneJoined succeeds, stable content → Idle.
	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "content",
		CapturePaneJoinedFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			if callCount == 1 {
				return "", fmt.Errorf("transient joined error")
			}
			return "stable-content", nil
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle after joined error recovery, got %s", verdict)
	}
}

func TestDetectBusyWithRetry_CommandErrorThenRecovery_ReturnsIdle(t *testing.T) {
	// First DetectBusy: GetPaneCurrentCommand errors → Undecided.
	// Immediate retry: command succeeds, shell → Idle.
	callCount := 0
	mock := &mockPaneIO{
		isShell: true,
		GetPaneCurrentCommandFn: func(_ string) (string, error) {
			callCount++
			if callCount == 1 {
				return "", fmt.Errorf("transient command error")
			}
			return "bash", nil
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle after command error recovery, got %s", verdict)
	}
}

func TestDetectBusyWithRetry_AllRetriesFail_PromotedToIdle(t *testing.T) {
	// All DetectBusy calls error → persistent Undecided → soft retries also fail
	// → promoted to VerdictIdle (sustained undecided = likely idle with stale output).
	mock := &mockPaneIO{
		GetPaneCurrentCommandFn: func(_ string) (string, error) {
			return "", fmt.Errorf("persistent command error")
		},
	}
	bd := newTestBusyDetector(mock, nil, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle (promoted) after all retries fail, got %s", verdict)
	}
}

// --- Soft Retry for VerdictUndecided ---

func TestSoftRetryUndecided_ResolvesToIdle(t *testing.T) {
	// With stable content, pattern match returns Idle directly (no soft retry).
	// The stale busy pattern does not prevent idle detection.
	busyRegex := regexp.MustCompile("Working|Thinking")
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "Working on task...",
		joinedContent:  []string{"stable-content", "stable-content"},
	}
	bd := newTestBusyDetector(mock, busyRegex, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle for stable content with stale pattern, got %s", verdict)
	}
}

func TestSoftRetryUndecided_StableWithPattern_ReturnsIdleDirectly(t *testing.T) {
	// When content is stable, pattern match returns Idle directly without
	// entering the soft retry path, even if content would later change.
	busyRegex := regexp.MustCompile("Working")
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "Working on task...",
		joinedContent:  []string{"Working on task...", "Working on task..."},
	}
	bd := newTestBusyDetector(mock, busyRegex, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle for stable content with pattern, got %s", verdict)
	}
}

func TestSoftRetryUndecided_PersistentStableWithPattern_ReturnsIdle(t *testing.T) {
	// Pattern matched + stable content returns Idle directly (no promotion needed).
	busyRegex := regexp.MustCompile("Working")
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "Working on task...",
		joinedContent:  []string{"stable-content", "stable-content"},
	}
	bd := newTestBusyDetector(mock, busyRegex, fastConfig())

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictIdle {
		t.Errorf("expected VerdictIdle for stable content with pattern, got %s", verdict)
	}
}

func TestSoftRetryUndecided_ContextCancelled(t *testing.T) {
	// Context cancelled during soft-retry sleep → returns VerdictBusy.
	// Capture errors trigger Undecided and enter the soft-retry path; when
	// the ctx expires before the soft-retry interval elapses the caller gets
	// VerdictBusy (conservative) instead of the ambiguous VerdictUndecided.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		CapturePaneFn: func(paneTarget string, lastN int) (string, error) {
			return "", fmt.Errorf("persistent capture error")
		},
	}
	cfg := busyDetectorConfig{
		IdleStableSec:       0,
		BusyCheckMaxRetries: 3,
		BusyCheckInterval:   2, // soft retry interval = max(1, 2/2) = 1 second → ctx will cancel
	}
	bd := newTestBusyDetector(mock, nil, cfg)

	verdict := bd.DetectBusyWithRetry(ctx, "%0", "worker1")
	if verdict != VerdictBusy {
		t.Errorf("expected VerdictBusy on context cancel during soft retry, got %s", verdict)
	}
}

func TestSoftRetryUndecided_BusyDetectionPreserved(t *testing.T) {
	// Truly busy agents (content changing) are still detected as busy,
	// not affected by the soft retry path.
	callCount := 0
	mock := &mockPaneIO{
		currentCommand: "claude",
		isShell:        false,
		captureContent: "content",
		CapturePaneJoinedFn: func(paneTarget string, lastN int) (string, error) {
			callCount++
			return fmt.Sprintf("changing-%d", callCount), nil
		},
	}
	cfg := busyDetectorConfig{
		IdleStableSec:       0,
		BusyCheckMaxRetries: 2,
		BusyCheckInterval:   0,
	}
	bd := newTestBusyDetector(mock, nil, cfg)

	verdict := bd.DetectBusyWithRetry(context.Background(), "%0", "worker1")
	if verdict != VerdictBusy {
		t.Errorf("expected VerdictBusy for truly busy agent, got %s", verdict)
	}
}

// --- Bug N: claude-code fast-path skip activity probe ---

// TestDetectBusy_BugN_ClaudePromptVisible_SkipsActivityProbe asserts that when
// the runtime is claude-code, the input prompt glyph (❯) is visible, and no
// busy pattern matched, the activity probe (CapturePaneJoined) is skipped
// entirely — the verdict is idle without waiting IdleStableSec for hash stability.
//
// Bug N regression: without this fast path, the Planner/Orchestrator pane's
// continuous TUI churn (status bar, cursor blink, "auto-update available"
// notices) made hash unstable across the 5-second activity probe, returning
// VerdictBusy and forcing 20+ second delivery delays via DetectBusyWithRetry.
func TestDetectBusy_BugN_ClaudePromptVisible_SkipsActivityProbe(t *testing.T) {
	mock := newMockPaneIO()
	mock.userVars["runtime"] = "claude-code"
	mock.currentCommand = "claude"
	mock.isShell = false
	// Prompt visible at the bottom (idle, awaiting input)
	mock.captureContent = "previous output\nmore output\n❯ "
	// Make activity probe captures DIFFERENT — proves the fast path skipped
	// them (otherwise hash mismatch would cause VerdictBusy).
	mock.joinedContent = []string{"frame-A\nspinner-◐", "frame-B\nspinner-◓"}

	bd := newTestBusyDetector(mock, nil, fastConfig())
	verdict := bd.DetectBusy(context.Background(), "%0")

	if verdict != VerdictIdle {
		t.Fatalf("expected VerdictIdle from claude fast path, got %s", verdict)
	}
	// Activity probe must NOT have been invoked.
	for _, c := range mock.calls {
		if c == "CapturePaneJoined" {
			t.Fatalf("Bug N regression: activity probe was invoked despite claude prompt being visible (calls=%v)", mock.calls)
		}
	}
}

// TestDetectBusy_BugN_NonClaudeRuntime_NoFastPath asserts that the fast path
// is gated on runtime=claude-code. For codex (no `❯` prompt convention), we
// must fall through to the activity probe, which is the only reliable signal
// for non-claude runtimes.
func TestDetectBusy_BugN_NonClaudeRuntime_NoFastPath(t *testing.T) {
	mock := newMockPaneIO()
	mock.userVars["runtime"] = "codex"
	mock.currentCommand = "codex"
	mock.isShell = false
	// Even if `❯` happens to appear (e.g., in user output), codex doesn't
	// use it as a prompt marker — the fast path must NOT trigger.
	mock.captureContent = "code blob\n❯ output sample"
	mock.joinedContent = []string{"stable", "stable"} // hash stable → idle via Stage 3

	bd := newTestBusyDetector(mock, nil, fastConfig())
	verdict := bd.DetectBusy(context.Background(), "%0")

	if verdict != VerdictIdle {
		t.Fatalf("expected VerdictIdle for codex (Stage 3 stable), got %s", verdict)
	}
	// Activity probe MUST have been invoked for codex (proves no fast-path).
	probed := false
	for _, c := range mock.calls {
		if c == "CapturePaneJoined" {
			probed = true
			break
		}
	}
	if !probed {
		t.Fatalf("expected activity probe to run for codex runtime, but CapturePaneJoined was never called: calls=%v", mock.calls)
	}
}

// TestDetectBusy_BugN_ClaudeBusyPatternMatched_NoFastPath asserts that even
// when the prompt glyph appears in old content above, an active busy pattern
// in the recent capture window blocks the fast path so the activity probe
// runs and correctly detects the busy state.
func TestDetectBusy_BugN_ClaudeBusyPatternMatched_NoFastPath(t *testing.T) {
	mock := newMockPaneIO()
	mock.userVars["runtime"] = "claude-code"
	mock.currentCommand = "claude"
	mock.isShell = false
	// Stale prompt visible above + active spinner below.
	mock.captureContent = "❯ previous turn\n\nWorking… (esc to interrupt)"
	mock.joinedContent = []string{"frame-A", "frame-B"} // changing → busy

	busyRegex := regexp.MustCompile("Working|esc to interrupt")
	bd := newTestBusyDetector(mock, busyRegex, fastConfig())
	verdict := bd.DetectBusy(context.Background(), "%0")

	if verdict != VerdictBusy {
		t.Fatalf("expected VerdictBusy when busy pattern matches (fast path must not bypass), got %s", verdict)
	}
}

func TestUndecidedSoftRetryInterval(t *testing.T) {
	tests := []struct {
		name              string
		busyCheckInterval int
		want              time.Duration
	}{
		{"default interval 2s", 2, 1 * time.Second},
		{"large interval 10s", 10, 5 * time.Second},
		{"zero interval uses min", 0, 1 * time.Second},
		{"interval 1 uses min", 1, 1 * time.Second},
		{"interval 3 rounds down", 3, 1 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bd := &busyDetector{config: busyDetectorConfig{BusyCheckInterval: tt.busyCheckInterval}}
			got := bd.undecidedSoftRetryInterval()
			if got != tt.want {
				t.Errorf("undecidedSoftRetryInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}
