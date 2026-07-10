package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// --- sendAndConfirm isolated tests ---

func newTestDeliverer(mock *mockPaneIO) *messageDeliverer {
	cfg := model.WatcherConfig{}
	ps := newPaneStateManager(mock)
	return newMessageDeliverer(mock, ps, &cfg, DefaultExecutorConfig(), log.New(&bytes.Buffer{}, "", 0), logLevelDebug)
}

func TestSendAndConfirm_ShellGuard_RejectsShell(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "bash"
	mock.isShell = true
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "payload",
	}, "%0")

	if result.Error == nil {
		t.Fatal("expected error for shell guard rejection")
	}
	if !result.Retryable {
		t.Error("expected Retryable=true for shell rejection")
	}
	if len(mock.sentTexts) != 0 {
		t.Errorf("expected no text sent after shell guard rejection, got %v", mock.sentTexts)
	}
}

func TestSendAndConfirm_Success(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "hello agent",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true")
	}
	if len(mock.sentTexts) != 1 || mock.sentTexts[0] != "hello agent" {
		t.Errorf("expected sent text 'hello agent', got %v", mock.sentTexts)
	}
	if mock.userVars["status"] != "busy" {
		t.Errorf("expected status=busy, got %q", mock.userVars["status"])
	}
}

func TestSendAndConfirm_NilContext_UsesBackground(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		Message: "test",
		Context: nil, // nil context should be handled
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true with nil context")
	}
}

func TestSendAndConfirm_SendTextFails(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.sendTextErr = fmt.Errorf("tmux send error")
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "payload",
	}, "%0")

	if result.Error == nil {
		t.Fatal("expected error when SendTextAndSubmit fails")
	}
	if !result.Retryable {
		t.Error("expected Retryable=true for send failure")
	}
}

func TestSendAndConfirm_SetStatusFails(t *testing.T) {
	t.Parallel()
	// SetStatus failure must NOT propagate as a delivery error.
	// The message was already sent successfully, so returning an error
	// would cause the dispatcher's inline retry to re-deliver the same
	// envelope and trigger a duplicate plan_submit on the planner side.
	// The failure is logged at warn level and the result is treated as
	// success — the busy hint is best-effort.
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.SetUserVarFn = func(_, name, _ string) error {
		if name == "status" {
			return fmt.Errorf("uservar write error")
		}
		return nil
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "payload",
	}, "%0")

	if result.Error != nil {
		t.Fatalf("SetStatus failure must be best-effort, got error: %v", result.Error)
	}
	if !result.Success {
		t.Fatal("expected Success=true even when SetStatus fails")
	}
	if len(mock.sentTexts) != 1 {
		t.Errorf("expected text to be sent before status failure, got %v", mock.sentTexts)
	}
}

func TestSendAndConfirm_GetCommandError_DefersDelivery(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.GetPaneCurrentCommandFn = func(_ string) (string, error) {
		return "", fmt.Errorf("tmux error")
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		Message: "payload",
	}, "%0")

	// Fail-closed: an unverifiable pane must defer delivery (retryable),
	// never paste blind — if Claude crashed, the multi-line envelope would
	// execute line-by-line in the bare shell.
	if result.Error == nil {
		t.Fatal("expected retryable error when pane state is unverifiable")
	}
	if !result.Retryable {
		t.Error("expected Retryable=true so the dispatcher re-attempts delivery")
	}
	if len(mock.sentTexts) != 0 {
		t.Errorf("no text should be sent when the pane probe fails, got %v", mock.sentTexts)
	}
}

func TestSendAndConfirm_MultilinePastedPlaceholderRetriesEnter(t *testing.T) {
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.captureJoinedSeq = []mockResp{
		// Pre-paste baseline capture: the deliverer snapshots the pane
		// before SendTextAndSubmit so the submit probe's content-growth
		// detector starts with a real baseline instead of waiting for
		// its own first observation. Tests must supply this entry so
		// the scripted post-paste sequence stays aligned.
		{val: "Welcome\n❯ \n"},
		{val: "Welcome\n❯ [Pasted text #1 +248 lines]\n"},
		{val: "Working...\n"},
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "line one\nline two",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true")
	}
	if !callsContain(mock.calls, "SendKeys:Enter") {
		t.Fatalf("expected retry Enter after pasted-text placeholder, calls=%v", mock.calls)
	}
}

func TestSendAndConfirm_MultilinePromptThenPastedPlaceholderRetriesEnter(t *testing.T) {
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.captureJoinedSeq = []mockResp{
		{val: "Welcome\n❯ \n"},
		{val: "Welcome\n❯ [Pasted text #1 +257 lines]\n"},
		{val: "Thinking\n"},
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "line one\nline two",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !callsContain(mock.calls, "SendKeys:Enter") {
		t.Fatalf("expected retry Enter after delayed pasted-text placeholder, calls=%v", mock.calls)
	}
}

func TestSendAndConfirm_MultilineStartupThenPastedPlaceholderRetriesEnter(t *testing.T) {
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.captureJoinedSeq = []mockResp{
		{val: "Claude Code\nWelcome back\n"},
		{val: "Claude Code\nStill starting\n"},
		{val: "Welcome\n❯ [Pasted text #1 +247 lines]\n"},
		{val: "Working\n"},
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "line one\nline two",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !callsContain(mock.calls, "SendKeys:Enter") {
		t.Fatalf("expected retry Enter after startup-delayed pasted-text placeholder, calls=%v", mock.calls)
	}
}

func TestSendAndConfirm_SubmitProbeCaptureFailureIsNonRetryable(t *testing.T) {
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.captureJoinedSeq = []mockResp{{err: fmt.Errorf("capture failed")}}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "line one\nline two",
		Context: context.Background(),
	}, "%0")

	if result.Error == nil {
		t.Fatal("expected non-retryable error for uncertain submit confirmation")
	}
	if !errors.Is(result.Error, ErrSubmitConfirmUncertain) {
		t.Fatalf("expected ErrSubmitConfirmUncertain, got %v", result.Error)
	}
	if result.Retryable {
		t.Fatal("submit confirmation uncertainty must not be retryable")
	}
	if result.Success {
		t.Fatal("uncertain submit confirmation must not report success")
	}
	if len(mock.sentTexts) != 1 {
		t.Fatalf("expected exactly one send attempt, got %d", len(mock.sentTexts))
	}
}

// TestSendAndConfirm_ActivityFirstShortCircuitsStalePlaceholder verifies
// the false-negative guard: when a previous-turn pasted-text line is
// scrolled into the bottom-N-line search window while the live prompt is
// already empty and Claude is visibly processing ("Working..."), the
// activity check runs first and confirms the dispatch without firing
// unnecessary Enter retries.
func TestSendAndConfirm_ActivityFirstShortCircuitsStalePlaceholder(t *testing.T) {
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.captureJoinedSeq = []mockResp{
		{val: "Welcome\n❯ \n"}, // pre-paste baseline
		// Post-paste: activity is visible AND the live prompt area is now
		// empty, but a stale placeholder from a previous turn lingers in
		// the scrolled-up history. Old logic flagged this as
		// pasted_text_still_at_prompt and burnt the retry budget; new
		// logic short-circuits on the activity marker.
		{val: "❯ [Pasted text #0 +12 lines]\n⏺ Working...\n❯ \n"},
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_retest7",
		Message: "line one\nline two",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Fatal("expected Success=true once activity is visible")
	}
	// Crucially: NO retry Enter should have been sent — the new ordering
	// confirms on activity first, before evaluating the stale placeholder.
	if callsContain(mock.calls, "SendKeys:Enter") {
		t.Fatalf("activity-first ordering must NOT fire retry Enter when activity is already visible, calls=%v", mock.calls)
	}
}

func TestSubmittedActivityVisible(t *testing.T) {
	t.Parallel()
	if !submittedActivityVisible("Thinking\n") {
		t.Fatal("expected Thinking marker to indicate submitted activity")
	}
	if submittedActivityVisible("Claude Code\n❯ [Pasted text #1 +247 lines]\n") {
		t.Fatal("pasted placeholder alone should not indicate submitted activity")
	}
}

func TestAgentAPIErrorVisible(t *testing.T) {
	t.Parallel()
	if !agentAPIErrorVisible("⏺ API Error: Opus 4.8 (1M context)'s safeguards flagged this message\n") {
		t.Fatal("expected API Error banner to be detected")
	}
	if agentAPIErrorVisible("⏺ Thinking...\n") {
		t.Fatal("plain activity marker must not be misdetected as an API error")
	}
}

// TestSendAndConfirm_AgentAPIErrorDetected reproduces the maestro_v2 root
// cause behind the CyberGym arvo:10400 stall: Claude Code's API-error banner
// ("⏺ API Error: ...safeguards flagged...") is itself prefixed with the same
// "⏺" glyph submittedActivityVisible treats as a delivery-confirmed activity
// marker. Before this fix, that banner made sendAndConfirm report
// Success=true even though the agent never processed the message, so the
// daemon waited forever (dispatch_deadlock loop) for a state file that would
// never appear. This test locks in that the banner is now detected as a
// definitive agent error rather than a confirmed delivery.
func TestSendAndConfirm_AgentAPIErrorDetected(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.captureJoinedSeq = []mockResp{
		{val: "Welcome\n❯ \n"}, // pre-paste baseline
		{val: "⏺ API Error: Opus 4.8 (1M context)'s safeguards flagged this message for\n" +
			"  a cybersecurity topic.\n❯ \n"},
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID:   "planner",
		CommandID: "cmd_001",
		Message:   "line one\nline two",
		Context:   context.Background(),
	}, "%0")

	if result.Success {
		t.Fatal("expected Success=false when the pane shows an API Error banner")
	}
	if !errors.Is(result.Error, ErrAgentAPIError) {
		t.Fatalf("expected ErrAgentAPIError, got %v", result.Error)
	}
	if result.Retryable {
		t.Fatal("agent API error must not be retryable: the classifier verdict is deterministic on the same content, so an inline resend would just repeat the same rejection")
	}
}

func TestSendAndConfirm_SingleLineSkipsSubmitConfirmation(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "single line",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if callsContain(mock.calls, "CapturePaneJoined") {
		t.Fatalf("single-line delivery should not probe pane content, calls=%v", mock.calls)
	}
}

// TestSendAndConfirm_NonClaudeRuntimeProgressDetectsChange verifies the
// runtime-agnostic content-change probe used for codex / gemini workers.
// The Claude Code-specific markers ("Pasted text #" / "Thinking" / "⏺")
// never appear on those panes, so the deliverer compares pane snapshots
// over the probe window: any change is treated as evidence that the
// runtime accepted the input and started rendering, while a fully stable
// pane indicates a blocking modal swallowed the paste.
func TestSendAndConfirm_NonClaudeRuntimeProgressDetectsChange(t *testing.T) {
	t.Parallel()
	for _, rt := range []string{"codex", "gemini"} {
		rt := rt
		t.Run(rt+"_changing_pane_succeeds", func(t *testing.T) {
			t.Parallel()
			mock := newMockPaneIO()
			mock.currentCommand = rt
			mock.isShell = false
			mock.userVars["runtime"] = rt
			// Different content per CapturePaneJoined call: simulates a
			// runtime that is rendering after paste+Enter.
			mock.joinedContent = []string{
				"prompt: ▌\n",
				"prompt: ▌\nthinking…\n",
			}
			d := newTestDeliverer(mock)

			result := d.sendAndConfirm(ExecRequest{
				AgentID: "worker1",
				TaskID:  "task_001",
				Message: "first line\nsecond line\nthird line\n",
				Context: context.Background(),
			}, "%0")

			if result.Error != nil {
				t.Fatalf("unexpected error: %v", result.Error)
			}
			if !result.Success {
				t.Error("expected Success=true (pane content changed across probe window)")
			}
		})

		t.Run(rt+"_static_pane_marks_uncertain", func(t *testing.T) {
			t.Parallel()
			mock := newMockPaneIO()
			mock.currentCommand = rt
			mock.isShell = false
			mock.userVars["runtime"] = rt
			// Same content on every CapturePaneJoined call: simulates the
			// codex first-run trust prompt or another blocking modal that
			// swallowed the paste before the runtime saw it.
			mock.captureContent = "Do you trust the contents of this directory? [y/N]\n"
			d := newTestDeliverer(mock)

			result := d.sendAndConfirm(ExecRequest{
				AgentID: "worker1",
				TaskID:  "task_001",
				Message: "first line\nsecond line\nthird line\n",
				Context: context.Background(),
			}, "%0")

			if result.Success {
				t.Error("expected Success=false when pane is static across the probe window")
			}
			if result.Error == nil || !errors.Is(result.Error, ErrSubmitConfirmUncertain) {
				t.Errorf("expected ErrSubmitConfirmUncertain, got: %v", result.Error)
			}
		})
	}
}

// TestSendAndConfirm_ClaudePaneContentChangeFallbackConfirms covers the
// content-growth fallback: when an activity marker scrolls past the
// captured viewport or renders between probes, the Claude probe still
// treats any normalized content-change after the first marker-free,
// placeholder-free snapshot as confirmation. Without this fallback the
// probe would exhaust its 8-attempt budget and surface a spurious
// dispatch_command_failed.
func TestSendAndConfirm_ClaudePaneContentChangeFallbackConfirms(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.userVars["runtime"] = model.RuntimeClaudeCode
	// Two distinct snapshots without any Claude UI marker or pasted-text
	// placeholder. The probe should adopt the first as a baseline and
	// flip to confirmed on the second when the hash changes.
	mock.captureJoinedSeq = []mockResp{
		{val: "❯ \nstatusline foo\n"},
		{val: "❯ \nstatusline foo\nrendered output line\n"},
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "planner",
		TaskID:  "task_001",
		Message: "first line\nsecond line\n",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Fatal("expected Success=true via content-change fallback")
	}
}

// TestSendAndConfirm_ClaudePaneStaticContentRemainsExhausted ensures the
// content-change fallback does not erode the safety net for genuinely
// stalled panes. Same captures across the full probe window with no
// markers, no placeholder, and no normalized content change must still
// surface as exhausted (non-retryable, since blindly resending risks
// double-submit).
func TestSendAndConfirm_ClaudePaneStaticContentRemainsExhausted(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.userVars["runtime"] = model.RuntimeClaudeCode
	// Static content (the same pane snapshot every probe). No marker, no
	// placeholder, and no advance after normalization → exhausted.
	mock.captureContent = "❯ \nstatusline foo\n"
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "planner",
		TaskID:  "task_001",
		Message: "first line\nsecond line\n",
		Context: context.Background(),
	}, "%0")

	if result.Success {
		t.Fatal("expected Success=false for fully static probe window")
	}
	if !errors.Is(result.Error, ErrSubmitConfirmUncertain) {
		t.Fatalf("expected ErrSubmitConfirmUncertain, got %v", result.Error)
	}
	if result.Retryable {
		t.Fatal("static-pane uncertainty must not be retryable (would double-submit)")
	}
}

// TestSendAndConfirm_ClaudePlaceholderResetsContentChangeBaseline guards the
// nuance that triggering a re-Enter on the pasted-text placeholder must
// restart the content-change observation. Without the reset, the post-Enter
// snapshot would be compared against the pre-Enter (placeholder-bearing)
// baseline and any UI redraw would falsely confirm submission. With the
// reset, the probe re-establishes a baseline AFTER the resend and only
// confirms once the post-resend pane content actually advances.
func TestSendAndConfirm_ClaudePlaceholderResetsContentChangeBaseline(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.userVars["runtime"] = model.RuntimeClaudeCode
	mock.captureJoinedSeq = []mockResp{
		// Pre-paste baseline: the deliverer snapshots the pane before
		// SendTextAndSubmit so subsequent probes can detect any post-
		// submit growth from the very first attempt.
		{val: "Welcome\n❯ \n"},
		// Probe 1: placeholder visible → resend Enter, baseline reset.
		{val: "Welcome\n❯ [Pasted text #1 +12 lines]\n"},
		// Probe 2: placeholder cleared, no marker. Becomes the new baseline.
		{val: "Welcome\n❯ \n"},
		// Probe 3: pane now advances (Claude is rendering) → confirmed via
		// content-change against the post-resend baseline.
		{val: "Welcome\n❯ \nThinking\n"},
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "planner",
		TaskID:  "task_001",
		Message: "first line\nsecond line\n",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Fatal("expected Success=true after placeholder retry + content-change confirm")
	}
	if !callsContain(mock.calls, "SendKeys:Enter") {
		t.Fatalf("expected resend Enter on placeholder, calls=%v", mock.calls)
	}
}

// TestSendAndConfirm_PrePasteBaselineConfirmsOnFirstProbe pins the
// pre-paste baseline contract: a pane that grows by even a single line
// on the very first probe is treated as confirmed. Without this
// behaviour the first probe is wasted on baseline capture and a worker
// that finishes its only visible growth before probe #2 is
// misclassified as exhausted.
//
// Scenario: pre-paste pane has 1 non-blank line ("❯"). Probe #1
// captures 2 non-blank lines ("❯", "Pasted snippet"); no activity
// marker, no placeholder. The probe sees 2 > 1 (vs pre-paste baseline)
// and confirms immediately.
func TestSendAndConfirm_PrePasteBaselineConfirmsOnFirstProbe(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	mock.userVars["runtime"] = model.RuntimeClaudeCode
	mock.captureJoinedSeq = []mockResp{
		// Pre-paste: empty prompt (1 non-blank line).
		{val: "❯\n"},
		// Probe #1: pane has grown by one line (no activity marker, no
		// placeholder, but content visibly changed). With pre-paste
		// baseline = 1 line, current = 2 lines → growth detected,
		// confirmed without waiting for the marker.
		{val: "❯\npayload-echoed-by-claude\n"},
	}
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_pre_paste_pin",
		Message: "payload-line-one\npayload-line-two",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("expected no error from pre-paste baseline confirmation, got: %v", result.Error)
	}
	if !result.Success {
		t.Fatal("expected Success=true (probe #1 should confirm via content_growth vs pre-paste baseline)")
	}
}

func TestNormalizeProbeSnapshot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "trims trailing whitespace per line",
			in:   "alpha   \nbeta\t\n",
			want: "alpha\nbeta\n",
		},
		{
			name: "collapses multiple blank lines",
			in:   "alpha\n\n\n\nbeta\n",
			want: "alpha\n\nbeta\n",
		},
		{
			name: "strips ANSI escapes",
			in:   "\x1b[31malpha\x1b[0m\nbeta\n",
			want: "alpha\nbeta\n",
		},
		{
			name: "preserves real content change",
			in:   "alpha\nbeta\nrendered\n",
			want: "alpha\nbeta\nrendered\n",
		},
		{
			name: "empty input returns empty",
			in:   "",
			want: "",
		},
		{
			name: "blank-only input returns empty",
			in:   "\n\n\n",
			want: "",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeProbeSnapshot(tt.in); got != tt.want {
				t.Fatalf("normalizeProbeSnapshot() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCountNonBlankLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"\n\n\n", 0},
		{"alpha\nbeta\n", 2},
		{"alpha\n\nbeta\n", 2},
		{"alpha\n  \t\nbeta\n", 2},
		{"alpha\nbeta\ngamma\n", 3},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := countNonBlankLines(tt.in); got != tt.want {
				t.Fatalf("countNonBlankLines(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestSendAndConfirm_DefaultRuntimeKeepsClaudeProbe guards against the
// runtime gate widening accidentally — claude-code panes (and panes whose
// @runtime variable is unset, which still default to claude-code) must
// continue to run the activity probe. Otherwise we'd lose the pasted-text
// retry that the original probe was added to handle.
func TestSendAndConfirm_DefaultRuntimeKeepsClaudeProbe(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.currentCommand = "claude"
	mock.isShell = false
	// Seed a "submitted" marker so the probe terminates on the first
	// attempt rather than burning the full budget; the assertion below
	// only cares that CapturePaneJoined ran at all.
	mock.captureContent = "❯ \n⏺ Working...\n"
	d := newTestDeliverer(mock)

	result := d.sendAndConfirm(ExecRequest{
		AgentID: "worker1",
		TaskID:  "task_001",
		Message: "first line\nsecond line\n",
		Context: context.Background(),
	}, "%0")

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !callsContain(mock.calls, "CapturePaneJoined") {
		t.Fatalf("claude runtime must still run the activity probe, calls=%v", mock.calls)
	}
}

func TestPastedTextPlaceholderAtPrompt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "claude prompt placeholder",
			content: "header\n❯ [Pasted text #1 +248 lines]\n",
			want:    true,
		},
		{
			name:    "ascii prompt placeholder",
			content: "header\n> [Pasted text #2 +12 lines]\n",
			want:    true,
		},
		{
			name:    "agent output mention is ignored",
			content: "I saw [Pasted text #1 +248 lines] in logs\n",
			want:    false,
		},
		{
			name:    "plain prompt without placeholder",
			content: "Thinking...\n❯ \n",
			want:    false,
		},
		{
			// A stale "[Pasted text #1 +247 lines]" line from a previous
			// turn scrolled into the bottom-N-line search window, with
			// the live input prompt below it empty. Only the bottom-most
			// prompt-marker line is consulted, so the stale history must
			// not trigger a still-at-prompt detection.
			name:    "stale history above empty prompt is ignored",
			content: "❯ [Pasted text #1 +247 lines]\n⏺ done\n❯ \n",
			want:    false,
		},
		{
			// Negative side: live prompt carries the placeholder while
			// history above also has one. Must remain true so the probe
			// fires the Enter retry on a genuinely-stuck paste.
			name:    "stale history above live placeholder still detects",
			content: "❯ [Pasted text #0 +10 lines]\n⏺ done\n❯ [Pasted text #1 +247 lines]\n",
			want:    true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := pastedTextPlaceholderAtPrompt(tt.content); got != tt.want {
				t.Fatalf("pastedTextPlaceholderAtPrompt() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- clearConfirmationPoller tests ---

func TestClearConfirmationPoller_IsConfirmed_WithValidPreClearHash(t *testing.T) {
	t.Parallel()
	p := &clearConfirmationPoller{
		preClearHashValid: true,
	}

	// Not confirmed: no hash change, no stable polls
	if p.isConfirmed() {
		t.Error("expected not confirmed with zero state")
	}

	// Hash changed but only 1 stable poll
	p.hashChanged = true
	p.stableCount = 1
	if p.isConfirmed() {
		t.Error("expected not confirmed with only 1 stable poll")
	}

	// Hash changed and 2 stable polls → confirmed
	p.stableCount = 2
	if !p.isConfirmed() {
		t.Error("expected confirmed with hash change + 2 stable polls")
	}
}

func TestClearConfirmationPoller_IsConfirmed_WithoutValidPreClearHash(t *testing.T) {
	t.Parallel()
	p := &clearConfirmationPoller{
		preClearHashValid: false,
	}

	// Need 3 stable polls without valid hash
	p.stableCount = 2
	if p.isConfirmed() {
		t.Error("expected not confirmed with only 2 stable polls (no hash)")
	}

	p.stableCount = 3
	if !p.isConfirmed() {
		t.Error("expected confirmed with 3 stable polls (no hash)")
	}
}

func TestClearConfirmationPoller_Reset(t *testing.T) {
	t.Parallel()
	p := &clearConfirmationPoller{
		stableCount:  5,
		prevPollHash: "abc123",
		hashChanged:  true,
	}

	p.reset()

	if p.stableCount != 0 {
		t.Errorf("expected stableCount=0 after reset, got %d", p.stableCount)
	}
	if p.prevPollHash != "" {
		t.Errorf("expected empty prevPollHash after reset, got %q", p.prevPollHash)
	}
	if p.hashChanged {
		t.Error("expected hashChanged=false after reset")
	}
}

func TestClearConfirmationPoller_Poll_CaptureError_Resets(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.CapturePaneJoinedFn = func(_ string, _ int) (string, error) {
		return "", fmt.Errorf("capture error")
	}

	p := newClearConfirmationPoller(
		mock, "%0", "prehash", true, 12,
		log.New(&bytes.Buffer{}, "", 0), logLevelDebug,
	)
	p.stableCount = 3
	p.hashChanged = true

	result, err := p.poll()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected poll to return false on capture error")
	}
	if p.stableCount != 0 {
		t.Errorf("expected stableCount=0 after capture error, got %d", p.stableCount)
	}
}

func TestClearConfirmationPoller_Poll_ClearTextVisible_ResetsStable(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.CapturePaneJoinedFn = func(_ string, _ int) (string, error) {
		return "❯ /clear\n", nil
	}

	p := newClearConfirmationPoller(
		mock, "%0", "prehash", true, 12,
		log.New(&bytes.Buffer{}, "", 0), logLevelDebug,
	)
	p.stableCount = 3

	result, err := p.poll()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected poll to return false when /clear text visible")
	}
	if p.stableCount != 0 {
		t.Errorf("expected stableCount reset, got %d", p.stableCount)
	}
}

func TestClearConfirmationPoller_PollUntilTimeout_ContextCancelled(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.captureContent = "unchanging content"

	p := newClearConfirmationPoller(
		mock, "%0", "prehash", true, 12,
		log.New(&bytes.Buffer{}, "", 0), logLevelDebug,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.pollUntilTimeout(ctx, 5*time.Second, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestClearConfirmationPoller_Poll_CaptureErrorBudget(t *testing.T) {
	t.Parallel()
	mock := newMockPaneIO()
	mock.CapturePaneJoinedFn = func(_ string, _ int) (string, error) {
		return "", fmt.Errorf("capture error")
	}

	p := newClearConfirmationPoller(
		mock, "%0", "prehash", true, 12,
		log.New(&bytes.Buffer{}, "", 0), logLevelDebug,
	)
	for i := 1; i < maxClearConfirmCaptureErrors; i++ {
		confirmed, err := p.poll()
		if err != nil {
			t.Fatalf("attempt %d returned unexpected error: %v", i, err)
		}
		if confirmed {
			t.Fatalf("attempt %d unexpectedly confirmed", i)
		}
	}
	confirmed, err := p.poll()
	if err == nil {
		t.Fatal("expected error after capture error budget is exhausted")
	}
	if confirmed {
		t.Fatal("capture errors must not confirm clear")
	}
}

// --- sleepWithBackoff tests ---

func TestSleepWithBackoff_NormalCompletion(t *testing.T) {
	t.Parallel()
	err := sleepWithBackoff(context.Background(), 1, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestBackoffDuration_ExactSchedule asserts the exponential schedule directly
// against the pure function so the test does not depend on scheduler latency.
func TestBackoffDuration_ExactSchedule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		baseMs  int
		attempt int
		want    time.Duration
	}{
		{1, 1, 1 * time.Millisecond},
		{1, 2, 2 * time.Millisecond},
		{1, 3, 4 * time.Millisecond},
		{1, 4, 8 * time.Millisecond},
		{50, 1, 50 * time.Millisecond},
		{50, 4, 400 * time.Millisecond},
	}
	for _, tc := range cases {
		got := backoffDuration(tc.baseMs, tc.attempt)
		if got != tc.want {
			t.Errorf("backoffDuration(base=%dms, attempt=%d) = %v, want %v", tc.baseMs, tc.attempt, got, tc.want)
		}
	}
}

func TestSleepWithBackoff_ExponentialIncrease(t *testing.T) {
	t.Parallel()
	// Coarse smoke test: a 50ms base attempt 3 schedules 200ms, well above
	// scheduler jitter. Strict equality is covered by
	// TestBackoffDuration_ExactSchedule.
	start := time.Now()
	_ = sleepWithBackoff(context.Background(), 50, 3) // expect ~200ms
	elapsed := time.Since(start)
	if elapsed < 150*time.Millisecond {
		t.Errorf("expected at least 150ms sleep for attempt 3 (2^2 * 50ms), got %v", elapsed)
	}
}

func TestSleepWithBackoff_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sleepWithBackoff(ctx, 10000, 1) // would be 10s if not cancelled
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}
