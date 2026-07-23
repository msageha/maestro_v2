package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// paneTailWarnOptedIn reports whether the operator has explicitly opted in
// to having the post-/clear-failure pane tail surfaced at warn level.
// Default is false so privacy-sensitive deployments do not see prior
// worker prompts in the daemon log.
func paneTailWarnOptedIn() bool {
	v := os.Getenv("MAESTRO_LOG_PANE_TAIL")
	return v == "1" || v == "true" || v == "TRUE" || v == "yes"
}

// messageDeliverer handles message delivery and /clear confirmation for
// tmux-based agent communication. Isolates the send/clear responsibility
// from dispatch routing and lifecycle management.
type messageDeliverer struct {
	paneIO    PaneIO
	paneState *paneStateManager
	config    *model.WatcherConfig // shared with Executor so test mutations propagate
	execCfg   ExecutorConfig
	logger    *log.Logger
	logLevel  logLevel
	paneMu    sync.Map // map[string]*sync.Mutex — per-pane delivery lock; bounded by the formation's static pane set (entries are never removed)
}

// Submit-probe tuning.
//
// These constants govern how aggressively the deliverer polls a tmux pane
// after sending a multi-line message to confirm that Claude Code actually
// submitted (vs. leaving the text dangling at the prompt as a paste
// placeholder). They are intentionally NOT routed through model.WatcherConfig
// today because:
//   - They are tuned against Claude Code's pasted-text UX timing, not user
//     workflow knobs;
//   - Both the cap (8 attempts) and the cadence (750ms) are stable across
//     all currently-supported runtimes.
const (
	// submitRetryProbeDelay is how long the deliverer waits between probes
	// of pane content after sending Enter, giving Claude Code time to
	// reflect submission.
	submitRetryProbeDelay = 750 * time.Millisecond
	// maxSubmitProbeAttempts is the BASE cap on the probe retry loop, keeping
	// delivery latency bounded even when the pane is genuinely stuck. Tuned to
	// `8 * submitRetryProbeDelay = 6s` — long enough for slow paste flows on a
	// small-context model without blocking the dispatcher when the worker is
	// wedged. The probe early-exits the instant a confirm signal appears, so
	// this cap only bites when nothing is observed at all.
	maxSubmitProbeAttempts = 8
	// maxSubmitProbeAttemptsLargeCtx widens the window for large-context /
	// slow-starting models (1M-context or Opus). First-token latency after a
	// large task prompt scales with context window and model size, and in
	// managed / high-latency environments a 1M Opus pane can take longer than
	// the base 6s window to render its first activity marker. That surfaced as
	// a false "submit uncertain" exhaustion even though delivery succeeded.
	// `20 * 750ms = 15s`. Early-exit on any confirm signal means the wider cap
	// costs nothing on the common fast path. See submitProbeAttemptsForPane.
	maxSubmitProbeAttemptsLargeCtx = 20
	// pastedTextPlaceholder is the marker Claude Code shows in lieu of the
	// actual pasted content while it waits for an explicit submit.
	pastedTextPlaceholder = "Pasted text #"
	// submitPromptSearchLines is the number of bottom lines of the pane
	// joined together when looking for the placeholder. Matches the worst
	// case where Claude Code has wrapped the placeholder onto a couple of
	// lines plus trailing UI chrome.
	submitPromptSearchLines = 8
	// maxClearConfirmCaptureErrors aborts a /clear confirmation attempt when
	// tmux capture repeatedly fails; continuing until timeout cannot confirm
	// success and only delays the next recovery action.
	maxClearConfirmCaptureErrors = 3
)

type submitProbeStatus string

const (
	submitProbeNotNeeded     submitProbeStatus = "not_needed"
	submitProbeConfirmed     submitProbeStatus = "confirmed"
	submitProbeRetried       submitProbeStatus = "retried"
	submitProbeCaptureFailed submitProbeStatus = "capture_failed"
	submitProbeExhausted     submitProbeStatus = "exhausted"
	// submitProbeAgentError means the pane showed an "API Error" banner
	// instead of processing the delivered message. Unlike submitProbeExhausted
	// (no signal observed) this is a definitive negative signal: the agent
	// runtime rejected or failed on the message. Kept distinct from
	// Uncertain() so callers surface it as a clear error rather than an
	// ambiguous one.
	submitProbeAgentError submitProbeStatus = "agent_error"
)

type submitProbeResult struct {
	Status   submitProbeStatus
	Attempts int
}

func newMessageDeliverer(paneIO PaneIO, paneState *paneStateManager, cfg *model.WatcherConfig, execCfg ExecutorConfig, logger *log.Logger, ll logLevel) *messageDeliverer {
	return &messageDeliverer{
		paneIO:    paneIO,
		paneState: paneState,
		config:    cfg,
		execCfg:   execCfg,
		logger:    logger,
		logLevel:  ll,
	}
}

// getPaneMutex returns the per-pane mutex for the given pane target, creating
// one on first access. This serializes the shell guard check → send → status
// update sequence to prevent concurrent deliveries from interleaving.
func (d *messageDeliverer) getPaneMutex(paneTarget string) *sync.Mutex {
	v, _ := d.paneMu.LoadOrStore(paneTarget, &sync.Mutex{})
	// Type assertion is safe: only *sync.Mutex values are stored in paneMu.
	mu, ok := v.(*sync.Mutex)
	if !ok {
		mu = &sync.Mutex{}
		d.paneMu.Store(paneTarget, mu)
	}
	return mu
}

// sendAndConfirm sends the message and updates @status to busy.
// It includes a final shell guard to prevent sending to a bare shell if Claude
// crashed between ensureClaudeRunning and delivery.
// The entire check → send → status update sequence is protected by a per-pane
// mutex to prevent concurrent deliveries from interleaving (TOCTOU guard).
func (d *messageDeliverer) sendAndConfirm(req ExecRequest, paneTarget string) ExecResult {
	mu := d.getPaneMutex(paneTarget)
	mu.Lock()
	defer mu.Unlock()

	// Final shell guard: reject delivery if pane has fallen back to a shell.
	// This closes the timing window between ensureClaudeRunning and here.
	// Fail-closed on probe error: pasting a multi-line task envelope into a
	// bare shell executes every line as a command, so an unverifiable pane
	// defers delivery (retryable) instead of proceeding blind.
	cmd, err := d.paneIO.GetPaneCurrentCommand(paneTarget)
	if err != nil {
		d.log(logLevelWarn, "delivery_deferred agent_id=%s task_id=%s reason=pane_probe_failed error=%v",
			req.AgentID, req.TaskID, err)
		return ExecResult{Error: fmt.Errorf("pane state unverifiable before delivery: %w", err), Retryable: true}
	}
	if d.paneIO.IsShellCommand(cmd) {
		d.log(logLevelError, "delivery_rejected agent_id=%s task_id=%s reason=pane_is_shell cmd=%s",
			req.AgentID, req.TaskID, cmd)
		return ExecResult{Error: fmt.Errorf("pane is shell (%s), Claude not running", cmd), Retryable: true}
	}

	// Send message via paste-buffer + Enter for reliable multi-line delivery
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	// Capture a pre-paste pane snapshot for the Claude submit probe to use
	// as its initial growth baseline. Using the pre-paste hash makes any
	// post-submit pane change a valid confirmation signal from probe #1
	// onwards (otherwise probe #1 would be spent capturing the baseline,
	// shrinking the effective detection window).
	prePaste := d.captureSubmitProbeBaseline(paneTarget, req)
	if err := d.paneIO.SendTextAndSubmit(ctx, paneTarget, req.Message); err != nil {
		d.log(logLevelError, "delivery_error agent_id=%s task_id=%s error=send_text: %v",
			req.AgentID, req.TaskID, err)
		return ExecResult{Error: fmt.Errorf("send message: %w", err), Retryable: true}
	}
	probe, err := d.confirmSubmittedOrRetry(ctx, paneTarget, req, prePaste)
	if err != nil {
		d.log(logLevelError, "delivery_error agent_id=%s task_id=%s error=submit_confirm: %v",
			req.AgentID, req.TaskID, err)
		return ExecResult{Error: fmt.Errorf("confirm submitted: %w", err), Retryable: true}
	}
	if probe.AgentError() {
		// Definitive negative signal: the runtime rejected or failed on the
		// message (e.g. Claude Code's safety-classifier "API Error" banner).
		// Logged at WARN — unlike the uncertain path below, this is not a
		// detection gap, it is direct evidence the agent never processed the
		// envelope. Retryable stays false: the classifier verdict is
		// deterministic on the same content, so an inline resend would just
		// waste another API round trip for the same rejection. The command
		// stays in_progress and the queue-scan / R0-dispatch recovery path
		// re-delivers it on its own schedule, which at least spaces out the
		// repeated attempts instead of retrying immediately.
		d.log(logLevelWarn, "delivery_agent_api_error agent_id=%s task_id=%s command_id=%s attempts=%d "+
			"(agent runtime showed an API Error banner instead of processing the message; the message content "+
			"may have tripped a safety classifier — inspect the pane manually, e.g. `tmux capture-pane`)",
			req.AgentID, req.TaskID, req.CommandID, probe.Attempts)
		return ExecResult{Error: fmt.Errorf("%w: attempts=%d", ErrAgentAPIError, probe.Attempts), Retryable: false}
	}
	if probe.Uncertain() {
		err := fmt.Errorf("%w: status=%s attempts=%d", ErrSubmitConfirmUncertain, probe.Status, probe.Attempts)
		// Logged at INFO (not WARN) because this is the *upstream* false-
		// negative side: the paste already landed, the probe simply did
		// not see a confirming UI marker. The queue scan path emits
		// `dispatch_uncertain_assume_running` (WARN) right after as the
		// load-bearing operator-facing signal.
		d.log(logLevelInfo, "delivery_submit_unconfirmed agent_id=%s task_id=%s command_id=%s status=%s attempts=%d (non-retryable to avoid duplicate submit; queue path will surface dispatch_uncertain_assume_running for operator visibility)",
			req.AgentID, req.TaskID, req.CommandID, probe.Status, probe.Attempts)
		return ExecResult{Error: err, Retryable: false}
	}

	// Update @status to busy. This is a best-effort post-delivery hint used by
	// the watcher/UI; the message has already been delivered to the pane and
	// the agent will start processing regardless. Returning an error here
	// would cause the dispatcher's inline retry to re-deliver the same
	// envelope, leading to double plan_submit. Log + continue instead.
	if err := d.paneState.SetStatus(paneTarget, "busy"); err != nil {
		d.log(logLevelWarn, "set_status_failed agent_id=%s error=%v (delivery already succeeded; continuing)",
			req.AgentID, err)
	}

	d.log(logLevelInfo, "delivery_success agent_id=%s task_id=%s command_id=%s lease_epoch=%d",
		req.AgentID, req.TaskID, req.CommandID, req.LeaseEpoch)
	return ExecResult{Success: true}
}

func (r submitProbeResult) Uncertain() bool {
	return r.Status == submitProbeCaptureFailed || r.Status == submitProbeExhausted
}

// AgentError reports whether the probe observed a definitive agent-runtime
// API error rather than ambiguous silence. Checked before Uncertain() by
// callers so a confirmed error is never downgraded to "uncertain".
func (r submitProbeResult) AgentError() bool {
	return r.Status == submitProbeAgentError
}

// submitProbeBaseline captures a pane snapshot used to seed the Claude
// submit probe's content-growth detector before we paste the envelope.
// Empty Hash means we did not capture (e.g. probe was not needed for this
// runtime, or the capture itself failed and we want the probe to fall back
// to its post-paste baseline).
type submitProbeBaseline struct {
	Hash  string
	Lines int
}

func (d *messageDeliverer) captureSubmitProbeBaseline(paneTarget string, req ExecRequest) submitProbeBaseline {
	if !needsSubmitConfirmation(req.Message) {
		return submitProbeBaseline{}
	}
	if !d.runtimeUsesClaudePromptUX(paneTarget) {
		// confirmGenericRuntimeProgress takes its own baseline post-paste,
		// matching its "any change is progress" contract. Capturing here
		// would only invite false positives if the pane churns between
		// pre-paste and the runtime's first redraw.
		return submitProbeBaseline{}
	}
	content, err := d.paneIO.CapturePaneJoined(paneTarget, d.execCfg.PromptReadyLines)
	if err != nil {
		// Non-fatal: confirmClaudeSubmittedOrRetry will fall back to the
		// historical post-paste baseline path. Log at debug so we still
		// have a breadcrumb when diagnosing exhausted probes.
		d.log(logLevelDebug, "submit_probe_pre_paste_capture_failed agent_id=%s task_id=%s error=%v",
			req.AgentID, req.TaskID, err)
		return submitProbeBaseline{}
	}
	normalized := normalizeStablePane(content)
	return submitProbeBaseline{
		Hash:  contentHash(normalized),
		Lines: countNonBlankLines(normalized),
	}
}

func (d *messageDeliverer) confirmSubmittedOrRetry(ctx context.Context, paneTarget string, req ExecRequest, prePaste submitProbeBaseline) (submitProbeResult, error) {
	if !needsSubmitConfirmation(req.Message) {
		return submitProbeResult{Status: submitProbeNotNeeded}, nil
	}
	// The pasted-text placeholder ("Pasted text #") and activity markers
	// ("Thinking", "Working", "⏺", …) tested by submittedActivityVisible /
	// pastedTextPlaceholderAtPrompt are Claude Code-specific UI tokens. Other
	// runtimes (codex, gemini) never render them, so the probe is split per
	// UX family. Skipping the probe entirely on non-claude runtimes would
	// claim success when the pane is actually wedged on a blocking modal
	// (codex first-run trust prompt, gemini onboarding); each branch runs
	// the detection that matches the runtime it is observing.
	if d.runtimeUsesClaudePromptUX(paneTarget) {
		return d.confirmClaudeSubmittedOrRetry(ctx, paneTarget, req, prePaste)
	}
	return d.confirmGenericRuntimeProgress(ctx, paneTarget, req)
}

// confirmClaudeSubmittedOrRetry runs the Claude Code-specific probe.
//
// Primary signals:
//   - "Pasted text #N" placeholder visible at the prompt → message stuck in
//     the input box; resend Enter and keep probing.
//   - Activity marker ("Thinking", "Working", "Running", "Bash(", "⏺") visible
//     anywhere in the captured viewport → submission confirmed.
//
// Secondary signal (content-growth fallback): some submissions complete fast
// enough — or push enough output that the marker scrolls past the captured
// PromptReadyLines window — that no probe sample catches a marker even when
// the Planner is plainly rendering output. To avoid the false-positive
// "uncertain → exhausted" outcome reported by the codex / gemini E2E run,
// the probe also remembers the first marker-free, placeholder-free snapshot
// as a baseline and treats subsequent samples that meet BOTH conditions
// below as confirmation:
//
//  1. Normalized content hash differs from the baseline. Normalization
//     (stripANSI + per-line right-trim + blank-line collapse) absorbs
//     cursor blinks and status-bar timer redraws.
//  2. Non-blank line count is strictly greater than the baseline. This
//     guards against false-positives from startup banner shuffles
//     ("Welcome back" → "Still starting") that change content without
//     adding new output. Real progress in Claude — a tool result, a
//     "Thinking" line, a streamed assistant response — appends to the
//     pane history, so the line count grows.
//
// The baseline is reset whenever a placeholder triggers a resend, since
// post-Enter constitutes a fresh observation phase and the prior baseline
// reflects the unsubmitted state.
func (d *messageDeliverer) confirmClaudeSubmittedOrRetry(ctx context.Context, paneTarget string, req ExecRequest, prePaste submitProbeBaseline) (submitProbeResult, error) {
	retried := false
	changeBaselineHash := prePaste.Hash
	changeBaselineLines := prePaste.Lines
	// A non-zero pre-paste hash means we already have a meaningful
	// baseline from before the SendTextAndSubmit call — content_growth
	// detection can fire on the very first probe rather than wasting the
	// first observation on baseline capture. An empty hash means the
	// pre-paste capture was skipped (probe not needed for this runtime
	// family) or failed (logged at debug); fall back to the historical
	// post-paste baseline path so behaviour is unchanged in those cases.
	haveChangeBaseline := prePaste.Hash != ""
	maxAttempts := d.submitProbeAttemptsForPane(paneTarget)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := sleepCtx(ctx, submitRetryProbeDelay); err != nil {
			return submitProbeResult{Status: submitProbeExhausted, Attempts: attempt - 1}, fmt.Errorf("wait for submit probe: %w", err)
		}
		content, err := d.paneIO.CapturePaneJoined(paneTarget, d.execCfg.PromptReadyLines)
		if err != nil {
			d.log(logLevelWarn, "submit_confirm capture_failed agent_id=%s task_id=%s error=%v",
				req.AgentID, req.TaskID, err)
			return submitProbeResult{Status: submitProbeCaptureFailed, Attempts: attempt}, nil
		}
		// Placeholder-first ordering: a "[Pasted text #N +M lines]" placeholder
		// on the bottom-most prompt line is a definitive "not yet submitted"
		// signal — the freshly pasted message is still in the live input box.
		// It MUST be checked before the activity-marker probe. Planner and
		// Orchestrator panes are delivered without a preceding /clear, so the
		// *previous* turn's activity markers ("⏺", "Running", "Bash(", …) linger
		// in the captured viewport; checking activity first would short-circuit
		// to "confirmed" on a stale marker even when a lost Enter left the new
		// message unsubmitted, and the resend-Enter recovery here would never
		// run — silently dropping the delivery. pastedTextPlaceholderAtPrompt is
		// scoped to the bottom prompt line only, so a placeholder scrolled up
		// into history cannot false-positive a retry.
		if pastedTextPlaceholderAtPrompt(content) {
			d.log(logLevelWarn, "submit_confirm pasted_text_still_at_prompt agent_id=%s task_id=%s attempt=%d/%d",
				req.AgentID, req.TaskID, attempt, maxAttempts)
			if err := d.paneIO.SendKeys(paneTarget, "Enter"); err != nil {
				return submitProbeResult{Status: submitProbeRetried, Attempts: attempt}, fmt.Errorf("send retry enter: %w", err)
			}
			retried = true
			haveChangeBaseline = false
			continue
		}
		// API-error banner check: MUST run before the activity-marker probe
		// below. The banner is itself prefixed with "⏺" (see
		// agentAPIErrorMarker doc comment), so submittedActivityVisible would
		// otherwise treat a rejected/failed message as a confirmed delivery —
		// which is exactly the false positive that let a Planner pane showing
		// "⏺ API Error: ...safeguards flagged..." be logged as delivered while
		// the agent never processed the envelope (root cause of the recurring
		// dispatch_deadlock / no_state_file loop). Same stale-content caveat
		// as submittedActivityVisible below: Planner/Orchestrator panes are
		// delivered without a preceding /clear, so a banner from a PRIOR turn
		// can still be in the captured viewport; accepted for the same reason
		// activity markers accept it (scoping to the bottom line only would
		// miss a banner that has scrolled up by the time this probe samples).
		if agentAPIErrorVisible(content) {
			d.log(logLevelWarn, "submit_confirm agent_api_error agent_id=%s task_id=%s attempt=%d/%d",
				req.AgentID, req.TaskID, attempt, maxAttempts)
			return submitProbeResult{Status: submitProbeAgentError, Attempts: attempt}, nil
		}
		// No unsubmitted placeholder at the prompt: a visible activity marker
		// ("Thinking", tool invocation, ⏺ status, …) means the message was
		// received and the dispatcher's job is done.
		if submittedActivityVisible(content) {
			status := submitProbeConfirmed
			if retried {
				status = submitProbeRetried
			}
			return submitProbeResult{Status: status, Attempts: attempt}, nil
		}
		normalized := normalizeStablePane(content)
		normalizedHash := contentHash(normalized)
		nonBlankLines := countNonBlankLines(normalized)
		if !haveChangeBaseline {
			changeBaselineHash = normalizedHash
			changeBaselineLines = nonBlankLines
			haveChangeBaseline = true
			continue
		}
		if normalizedHash != changeBaselineHash && nonBlankLines > changeBaselineLines {
			d.log(logLevelInfo,
				"submit_confirm content_growth_fallback agent_id=%s task_id=%s attempt=%d baseline_lines=%d current_lines=%d "+
					"(no Claude UI marker captured but pane gained output lines — treating as confirmed)",
				req.AgentID, req.TaskID, attempt, changeBaselineLines, nonBlankLines)
			status := submitProbeConfirmed
			if retried {
				status = submitProbeRetried
			}
			return submitProbeResult{Status: status, Attempts: attempt}, nil
		}
	}
	// Probe budget exhausted without seeing either the pasted-text
	// placeholder OR an activity marker OR any content change. We
	// deliberately fall through to a non-error return rather than
	// escalating to Retryable: re-delivering the envelope risks double
	// plan_submit. Logged at INFO because dispatch_uncertain_assume_running
	// (which the queue path emits next) is the load-bearing operator
	// signal and queue lease recovery is the real failure detector.
	d.log(logLevelInfo,
		"submit_confirm probe_budget_exhausted agent_id=%s task_id=%s attempts=%d "+
			"(no marker captured within probe window; non-retryable to avoid double-submit, lease recovery will retry if the worker is genuinely stuck)",
		req.AgentID, req.TaskID, maxAttempts)
	return submitProbeResult{Status: submitProbeExhausted, Attempts: maxAttempts}, nil
}

// normalizeProbeSnapshot collapses transient differences in a tmux capture so
// the Claude submit probe's content-growth fallback only fires on real
// progress. It strips ANSI escapes, right-trims each line, drops the
// trailing newline so input ending in "\n" is not treated as having an
// extra blank trailing line, and collapses runs of blank lines. Cursor
// blinks and status-bar timer redraws share their normalized form, while a
// fresh output line / tool result / prompt clearing survives normalization
// and changes the hash. Kept package-local because it is only meaningful in
// the context of submit confirmation.
func normalizeProbeSnapshot(content string) string {
	stripped := strings.TrimRight(stripANSI(content), "\n")
	if stripped == "" {
		return ""
	}
	lines := strings.Split(stripped, "\n")
	var b strings.Builder
	b.Grow(len(stripped))
	prevBlank := false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if trimmed == "" {
			if prevBlank {
				continue
			}
			prevBlank = true
		} else {
			prevBlank = false
		}
		b.WriteString(trimmed)
		b.WriteByte('\n')
	}
	return b.String()
}

// countNonBlankLines returns the number of lines in s that contain at least
// one non-whitespace character. Used by the Claude submit probe to require
// that the pane "grows" before falling back to content-change confirmation.
func countNonBlankLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// volatileChromeMatchers match Claude Code TUI status/footer/hint lines that
// redraw independently of conversation progress: the context-usage bar, the
// permission-mode footer, rotating "Tip:" hints, and the spinner + elapsed /
// token-count status line. In the small worker panes (~40x11 when a formation
// splits one tmux window across four workers) the confirmation capture window
// (PromptReadyLines) is essentially the whole pane, so these volatile lines
// dominate the raw hash and defeat both heuristics: the /clear stability check
// never sees two identical consecutive polls, and the submit content-growth
// fallback finds the non-blank line count saturated by chrome. Stripping them
// before hashing makes both checks depend only on substantive content and thus
// independent of pane geometry and status-bar animation.
//
// Deliberately does NOT match tool-output lines (e.g. "⏺ Bash(...)"): those are
// substantive and are also the submit probe's primary activity signal.
var volatileChromeMatchers = []*regexp.Regexp{
	// Context / token usage bar: "[####----] 12% used · 88% remaining · Opus 4.8 (1M context) · /…"
	regexp.MustCompile(`\d+%\s+used\b`),
	// Progress-bar-only line: "[####----------------]".
	regexp.MustCompile(`^\[[#\s.\-]+\]$`),
	// Permission-mode footer: "⏸ manual mode on · ← for agents", "⏵⏵ accept edits on …".
	regexp.MustCompile(`(?i)(manual mode on|accept edits on|plan mode on|bypass permissions on|←\s*for agents)`),
	// Rotating hint: "Tip: …" (optionally prefixed by box-drawing "⎿"/"└"/"│").
	regexp.MustCompile(`^[⎿└│>\s]*Tip:`),
	// Spinner + elapsed/token status: "✻ Coalescing… (48s · ↓ 1.8k tokens …)",
	//   "◯ Explore  SS… 2m 26s · ↓ 67.9k tokens".
	regexp.MustCompile(`↓\s*[\d.]+k?\s*tokens`),
	// "(… · esc to interrupt)" progress hint.
	regexp.MustCompile(`\besc to interrupt\b`),
}

// isVolatileChromeLine reports whether a (already ANSI-stripped, trimmed) line
// is Claude Code TUI chrome that redraws independently of real progress.
func isVolatileChromeLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	for _, re := range volatileChromeMatchers {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// stripVolatileChrome removes volatile TUI chrome lines from a normalized pane
// snapshot so a confirmation hash reflects only substantive content.
func stripVolatileChrome(normalized string) string {
	if normalized == "" {
		return ""
	}
	// TrimRight avoids a spurious empty trailing element from the final "\n"
	// that normalizeProbeSnapshot appends, matching its output shape.
	var b strings.Builder
	b.Grow(len(normalized))
	for _, line := range strings.Split(strings.TrimRight(normalized, "\n"), "\n") {
		if isVolatileChromeLine(line) {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// normalizeStablePane produces a confirmation-stable view of a pane capture:
// normalizeProbeSnapshot (ANSI strip + right-trim + blank collapse) followed by
// stripVolatileChrome. Two captures taken while nothing substantive changes
// hash-equal even in a tiny, animated pane, which is the precondition both the
// /clear stability check and the submit content-growth fallback rely on.
func normalizeStablePane(content string) string {
	return stripVolatileChrome(normalizeProbeSnapshot(content))
}

// submitProbeAttemptsForPane returns the submit-probe attempt cap for a pane,
// widening it for large-context / slow-starting models (see
// maxSubmitProbeAttemptsLargeCtx). The @model pane variable is read best-effort;
// any read failure falls back to the base cap, preserving historical behaviour.
func (d *messageDeliverer) submitProbeAttemptsForPane(paneTarget string) int {
	m, err := d.paneIO.GetUserVar(paneTarget, "model")
	if err != nil || m == "" {
		return maxSubmitProbeAttempts
	}
	lm := strings.ToLower(m)
	if strings.Contains(lm, "[1m]") || strings.Contains(lm, "opus") {
		return maxSubmitProbeAttemptsLargeCtx
	}
	return maxSubmitProbeAttempts
}

// confirmGenericRuntimeProgress is the runtime-agnostic submit confirmation
// used by non-claude-code worker panes. It captures the joined pane content
// once as the post-Enter baseline and then polls for any change across the
// probe window. An accepted input drives observable churn — input box clears,
// the conversation history grows, a thinking spinner advances. A pane that
// stays byte-for-byte identical across the full window is the signature of
// a blocking modal (codex first-run trust prompt, gemini onboarding) that
// swallowed the paste before the runtime saw it. We treat that as the same
// "uncertain" outcome the Claude path uses: the dispatcher keeps the
// envelope non-retryable to avoid double-submit, and lease expiry drives a
// fresh attempt once the operator clears the modal.
func (d *messageDeliverer) confirmGenericRuntimeProgress(ctx context.Context, paneTarget string, req ExecRequest) (submitProbeResult, error) {
	initial, err := d.paneIO.CapturePaneJoined(paneTarget, d.execCfg.PromptReadyLines)
	if err != nil {
		d.log(logLevelWarn, "submit_confirm capture_failed agent_id=%s task_id=%s error=%v",
			req.AgentID, req.TaskID, err)
		return submitProbeResult{Status: submitProbeCaptureFailed, Attempts: 1}, nil
	}
	initialHash := contentHash(initial)
	for attempt := 1; attempt <= maxSubmitProbeAttempts; attempt++ {
		if err := sleepCtx(ctx, submitRetryProbeDelay); err != nil {
			return submitProbeResult{Status: submitProbeExhausted, Attempts: attempt - 1}, fmt.Errorf("wait for submit probe: %w", err)
		}
		snapshot, err := d.paneIO.CapturePaneJoined(paneTarget, d.execCfg.PromptReadyLines)
		if err != nil {
			d.log(logLevelWarn, "submit_confirm capture_failed agent_id=%s task_id=%s attempt=%d error=%v",
				req.AgentID, req.TaskID, attempt, err)
			return submitProbeResult{Status: submitProbeCaptureFailed, Attempts: attempt}, nil
		}
		if contentHash(snapshot) != initialHash {
			return submitProbeResult{Status: submitProbeConfirmed, Attempts: attempt}, nil
		}
	}
	d.log(logLevelWarn,
		"submit_confirm probe_budget_exhausted_no_progress agent_id=%s task_id=%s attempts=%d "+
			"(pane content stable across %s probe window; input may not have reached the runtime — "+
			"check for blocking prompts like codex first-run trust)",
		req.AgentID, req.TaskID, maxSubmitProbeAttempts,
		time.Duration(maxSubmitProbeAttempts)*submitRetryProbeDelay)
	return submitProbeResult{Status: submitProbeExhausted, Attempts: maxSubmitProbeAttempts}, nil
}

func needsSubmitConfirmation(message string) bool {
	return strings.Contains(message, "\n")
}

// runtimeUsesClaudePromptUX reports whether the @runtime pane variable
// indicates a runtime whose UI emits the Claude Code-specific pasted-text
// placeholder + activity markers. Mirrors the fail-open shape used by
// ClaudeProcessManager.paneRuntime: any read failure is treated as the
// default (claude-code), which preserves the historical probe behaviour for
// panes that were created before the @runtime variable existed.
func (d *messageDeliverer) runtimeUsesClaudePromptUX(paneTarget string) bool {
	rt, err := d.paneIO.GetUserVar(paneTarget, "runtime")
	if err != nil {
		return true
	}
	if rt == "" {
		return true
	}
	return rt == model.RuntimeClaudeCode
}

func submittedActivityVisible(content string) bool {
	clean := stripANSI(content)
	for _, marker := range []string{"Thinking", "Working", "Running", "Bash(", "⏺"} {
		if strings.Contains(clean, marker) {
			return true
		}
	}
	return false
}

// agentAPIErrorMarker is the literal banner Claude Code prints when the
// runtime API rejects or fails on a message (e.g. a safety-classifier
// rejection for cybersecurity-flagged content, or a transient API failure).
// The banner is itself prefixed with the same "⏺" glyph used for normal
// activity output ("⏺ API Error: ..."), so it MUST be checked before
// submittedActivityVisible: otherwise the "⏺" match alone would confirm
// delivery even though the agent never processed the message.
const agentAPIErrorMarker = "API Error"

// agentAPIErrorVisible reports whether the captured pane content shows the
// Claude Code "API Error" banner anywhere in the viewport. Unlike the
// pasted-text-placeholder check (scoped to the bottom prompt line only), this
// intentionally scans the whole captured content: the banner can still be
// visible a few lines above the prompt on the next probe tick, and missing it
// there would let the content-growth fallback confirm a delivery that never
// actually landed.
func agentAPIErrorVisible(content string) bool {
	return strings.Contains(stripANSI(content), agentAPIErrorMarker)
}

// pastedTextPlaceholderAtPrompt reports whether the live input area at the
// bottom of the pane still shows the "[Pasted text #N +M lines]" placeholder
// — the signal that Claude Code received the paste but has NOT yet processed
// the Enter that submits it. The probe loop uses this to decide whether to
// re-fire Enter.
//
// Scoping is critical: lines above the bottom-most prompt line are scrolled-
// in pane history from previous turns. A "❯ [Pasted text #1 +247 lines]" in
// the history (e.g. the prior task's input area, now scrolled up two screens)
// must NOT trigger a retry, otherwise every dispatch on a hot pane racks up
// false-positive retries. Consider ONLY the first prompt-marker line walked
// back from the bottom; anything above is history.
func pastedTextPlaceholderAtPrompt(content string) bool {
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < submitPromptSearchLines; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		isPromptLine := strings.Contains(trimmed, "❯") || strings.HasPrefix(trimmed, ">")
		if !isPromptLine {
			checked++
			continue
		}
		// Bottom-most prompt line found. If it still carries the
		// placeholder the live input area has the unsubmitted paste;
		// otherwise the prompt is empty / activity has consumed the
		// turn and we treat history matches above as irrelevant.
		return strings.Contains(trimmed, pastedTextPlaceholder)
	}
	return false
}

// clearAndConfirm sends /clear exactly once per attempt and confirms it was
// processed by the target application. It retries up to ClearMaxAttempts times.
// Returns nil on confirmed clear, or an error if all attempts fail (fail-closed:
// caller must NOT proceed with delivery).
//
// Send semantics: SendCommand pastes "/clear" with `send-keys -l` then sends
// `Enter` as one logical action. Claude Code 2.x executes /clear immediately
// on the first Enter and treats a subsequent Enter on the (now-cleared) input
// as "re-run last command", which would cause /clear to fire twice on every
// task transition. No second Enter is sent; reliability is preserved by the
// existing pollUntilTimeout confirmation and the per-attempt resend in this
// loop.
//
// Confirmation checks (per poll, evaluated by clearConfirmationPoller):
//  1. "/clear" text is NOT visible near the bottom of the pane (primary signal --
//     directly detects the production failure mode where /clear remained as
//     unprocessed text in the input field). Stability alone never confirms.
//  2. Pane content hash has changed from the pre-clear snapshot (secondary signal).
//  3. Pane content is stable across two consecutive polls.
//
// Per-attempt resend (not per-Enter retry): if pollUntilTimeout cannot confirm
// processing within ClearConfirmTimeoutSec, the next attempt resends "/clear"
// in full. This keeps each attempt single-shot from the runtime's perspective —
// the runtime sees one /clear per attempt, never one followed by a stray Enter.
func (d *messageDeliverer) clearAndConfirm(ctx context.Context, paneTarget string) error {
	timeout := time.Duration(d.config.ClearConfirmTimeoutSec) * time.Second
	pollInterval := time.Duration(d.config.ClearConfirmPollMs) * time.Millisecond
	maxAttempts := d.config.ClearMaxAttempts
	backoffMs := d.config.ClearRetryBackoffMs

	// Pane transcripts on Claude Code 2.x show "/clear" twice per task
	// transition even when the daemon only invokes a single SendCommand
	// (echo + slash-command history line). The clear_send_invocation INFO
	// log here is the canonical counter for "how many times did the daemon
	// put '/clear' on the wire": one entry per pane "/clear" line means
	// the doubling is purely a transcript artifact; two or more entries
	// per transition would indicate a real double-send.
	d.log(logLevelInfo, "clear_send_begin pane=%s max_attempts=%d", paneTarget, maxAttempts)

	hashDisabledWarned := false

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("clear_and_confirm cancelled before attempt %d: %w", attempt, err)
		}

		// Capture pre-clear hash. A capture failure here is recoverable —
		// the hash check is optional and the rest of the flow can continue
		// without it. Per-attempt noise is demoted to debug; we surface a
		// single warn breadcrumb the first time the hash check is disabled
		// within this call so the operator can correlate with the eventual
		// outcome (success or final-failure summary).
		preClearContent, err := d.paneIO.CapturePaneJoined(paneTarget, d.execCfg.PromptReadyLines)
		preClearHashValid := err == nil
		if err != nil {
			d.log(logLevelDebug, "clear_confirm pre_capture error=%v attempt=%d (hash check disabled)", err, attempt)
			if !hashDisabledWarned {
				d.log(logLevelWarn, "clear_confirm pre_capture_disabled pane=%s attempt=%d error=%v (hash check disabled for this clear cycle)", paneTarget, attempt, err)
				hashDisabledWarned = true
			}
		}
		// Hash the chrome-stripped view so the pre/post-clear comparison and the
		// poller's stability check see only substantive content, not the
		// context bar / mode footer / rotating tip that redraw on their own.
		preClearHash := contentHash(normalizeStablePane(preClearContent))

		// Send /clear once. SendCommand emits the literal "/clear" string and a
		// single Enter to submit it. No additional Enter is sent — see the
		// function-level comment for why a second Enter would cause doubled
		// /clear in Claude Code 2.x.
		d.log(logLevelInfo, "clear_send_invocation pane=%s attempt=%d/%d", paneTarget, attempt, maxAttempts)
		if err := d.paneIO.SendCommand(paneTarget, "/clear"); err != nil {
			// Non-final attempts retry; treat the per-attempt error as
			// debug. Final attempt drops out of the loop and surfaces the
			// terminal failure with pane context below.
			if attempt < maxAttempts {
				d.log(logLevelDebug, "clear_confirm send_clear error=%v attempt=%d/%d (retrying)", err, attempt, maxAttempts)
				if bErr := sleepWithBackoff(ctx, backoffMs, attempt); bErr != nil {
					return bErr
				}
				continue
			}
			d.log(logLevelWarn, "clear_confirm send_clear failed pane=%s error=%v attempt=%d/%d (terminal)", paneTarget, err, attempt, maxAttempts)
			d.logPaneTailForClearFailure(paneTarget, "send_clear_failed")
			return fmt.Errorf("clear_confirm: %w after %d attempts: %w", ErrClearSendFailed, maxAttempts, err)
		}

		// Poll for confirmation within timeout window
		poller := newClearConfirmationPoller(
			d.paneIO, paneTarget, preClearHash, preClearHashValid,
			d.execCfg.PromptReadyLines, d.logger, d.logLevel,
		)
		confirmed, err := poller.pollUntilTimeout(ctx, timeout, pollInterval)
		if err != nil {
			return err // context cancelled
		}
		if confirmed {
			d.log(logLevelInfo, "clear_send_done pane=%s attempts_used=%d", paneTarget, attempt)
			return nil
		}

		// Not confirmed — debug per attempt, escalate to warn only when
		// the next iteration would exceed maxAttempts (final retry just
		// failed). The terminal log after the loop additionally captures
		// the pane tail for postmortem.
		if attempt < maxAttempts {
			d.log(logLevelDebug, "clear_confirm not_confirmed attempt=%d/%d (retrying)", attempt, maxAttempts)
			if err := sleepWithBackoff(ctx, backoffMs, attempt); err != nil {
				return err
			}
			continue
		}
		d.log(logLevelWarn, "clear_confirm not_confirmed pane=%s attempt=%d/%d (terminal)", paneTarget, attempt, maxAttempts)
	}

	d.logPaneTailForClearFailure(paneTarget, "not_confirmed")
	return fmt.Errorf("clear_confirm: %w after %d attempts", ErrClearNotConfirmed, maxAttempts)
}

// logPaneTailForClearFailure emits the bottom of the pane to aid debugging
// of /clear failures. The pane tail can contain prior worker prompts,
// approval-dialog text, or model output — anything visible at the bottom
// of the runtime UI — so it doubles as potentially sensitive operator
// data. Default is debug-level so the diagnostic is opt-in for
// privacy-sensitive deployments; setting MAESTRO_LOG_PANE_TAIL=1
// promotes it to warn for environments where the daemon log is the
// primary debugging surface.
//
// Best-effort: capture errors are reported at debug because the
// surrounding terminal log already conveys the underlying clear_confirm
// failure.
func (d *messageDeliverer) logPaneTailForClearFailure(paneTarget, phase string) {
	tail, err := d.paneIO.CapturePaneJoined(paneTarget, d.execCfg.PromptReadyLines)
	if err != nil {
		d.log(logLevelDebug, "clear_confirm pane_tail capture error=%v phase=%s", err, phase)
		return
	}
	// Trim very long captures to keep log lines bounded; the trailing few
	// hundred bytes are where Claude Code's prompt state and any approval
	// dialog live.
	const maxTailBytes = 1024
	if len(tail) > maxTailBytes {
		tail = tail[len(tail)-maxTailBytes:]
	}
	level := logLevelDebug
	if paneTailWarnOptedIn() {
		level = logLevelWarn
	}
	d.log(level, "clear_confirm pane_tail pane=%s phase=%s tail=%q", paneTarget, phase, tail)
}

// log delegates to the package-level logf function which uses time.Now()
// for timestamp formatting. This is acceptable for logging purposes.
func (d *messageDeliverer) log(level logLevel, format string, args ...any) {
	logf(d.logger, d.logLevel, level, "agent_executor", format, args...)
}

// --- Clear Confirmation Poller ---

// clearConfirmationPoller encapsulates the state machine for polling pane
// content to confirm that a /clear command was processed.
//
// Wall clock: pollUntilTimeout uses time.Now() directly for deadline
// calculation. For deterministic testing, a Clock interface (similar to
// metrics.Clock) could be injected. Current test coverage uses real timers
// via integration-style tests, which is acceptable given the I/O-bound nature.
type clearConfirmationPoller struct {
	paneIO            PaneIO
	paneTarget        string
	preClearHash      string
	preClearHashValid bool
	promptReadyLines  int
	logger            *log.Logger
	logLevel          logLevel

	// internal state
	stableCount              int
	hashChanged              bool
	substantiveEmpty         bool
	prevPollHash             string
	consecutiveCaptureErrors int
}

func newClearConfirmationPoller(
	paneIO PaneIO, paneTarget, preClearHash string, hashValid bool,
	promptReadyLines int, logger *log.Logger, ll logLevel,
) *clearConfirmationPoller {
	return &clearConfirmationPoller{
		paneIO:            paneIO,
		paneTarget:        paneTarget,
		preClearHash:      preClearHash,
		preClearHashValid: hashValid,
		promptReadyLines:  promptReadyLines,
		logger:            logger,
		logLevel:          ll,
	}
}

// pollUntilTimeout polls the pane within the timeout window to confirm /clear was processed.
// Returns (true, nil) if confirmed, (false, nil) if timed out, or (false, err) if ctx is cancelled.
func (p *clearConfirmationPoller) pollUntilTimeout(ctx context.Context, timeout, pollInterval time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return false, fmt.Errorf("clear_confirm poll cancelled: %w", err)
		}

		confirmed, err := p.poll()
		if err != nil {
			return false, err
		}
		if confirmed {
			return true, nil
		}
	}

	return false, nil
}

// poll captures the current pane state and evaluates confirmation criteria.
// Returns true if /clear has been confirmed processed.
func (p *clearConfirmationPoller) poll() (bool, error) {
	content, err := p.paneIO.CapturePaneJoined(p.paneTarget, p.promptReadyLines)
	if err != nil {
		p.consecutiveCaptureErrors++
		p.log(logLevelDebug, "clear_confirm poll capture error=%v", err)
		p.reset()
		if p.consecutiveCaptureErrors >= maxClearConfirmCaptureErrors {
			return false, fmt.Errorf("clear_confirm: capture failed %d consecutive times: %w", p.consecutiveCaptureErrors, err)
		}
		return false, nil
	}
	p.consecutiveCaptureErrors = 0

	// Hash the chrome-stripped view so status-bar / footer / tip redraws in a
	// tiny pane do not perturb the stability check. clearTextVisible still runs
	// on the raw capture — the "/clear" text sits on the prompt line, which the
	// normalizer keeps.
	normalized := normalizeStablePane(content)
	currentHash := contentHash(normalized)
	p.substantiveEmpty = countNonBlankLines(normalized) == 0

	// Check 1 (primary): "/clear" text must NOT be visible near the bottom of the pane.
	if clearTextVisible(content) {
		p.log(logLevelDebug, "clear_confirm /clear text still visible")
		p.stableCount = 0
		p.prevPollHash = currentHash
		return false, nil
	}

	// Check 2 (mandatory when valid): hash must differ from pre-clear state.
	if p.preClearHashValid && currentHash != p.preClearHash {
		p.hashChanged = true
	}

	// Check 3: stability -- consecutive polls with same hash.
	if p.prevPollHash != "" && currentHash == p.prevPollHash {
		p.stableCount++
	} else {
		p.stableCount = 1
	}
	p.prevPollHash = currentHash

	return p.isConfirmed(), nil
}

// isConfirmed evaluates whether the confirmation criteria are met.
//   - With valid pre-clear hash: require (hash change OR a substantive-empty
//     pane) + 2 stable polls (debounce). The substantive-empty branch handles
//     the case where the pre-clear capture was itself mostly chrome so the
//     normalized hash does not change, yet /clear plainly emptied the
//     conversation region — the definition of a successful clear.
//   - Without valid pre-clear hash: require 3 stable polls (stricter debounce).
func (p *clearConfirmationPoller) isConfirmed() bool {
	if p.preClearHashValid {
		return (p.hashChanged || p.substantiveEmpty) && p.stableCount >= 2
	}
	return p.stableCount >= 3
}

// reset clears the poller state after a capture error.
func (p *clearConfirmationPoller) reset() {
	p.stableCount = 0
	p.prevPollHash = ""
	p.hashChanged = false
	p.substantiveEmpty = false
}

func (p *clearConfirmationPoller) log(level logLevel, format string, args ...any) {
	logf(p.logger, p.logLevel, level, "clear_poller", format, args...)
}

// backoffDuration returns the exponential delay used by sleepWithBackoff.
// Pure function so unit tests can assert the algebra (`base * 2^(attempt-1)`)
// without paying the wall-clock cost of an actual Sleep.
func backoffDuration(baseMs, attempt int) time.Duration {
	return time.Duration(baseMs*(1<<(attempt-1))) * time.Millisecond
}

// sleepWithBackoff sleeps for an exponentially increasing duration based on the
// attempt number (1-indexed). baseMs is the base delay in milliseconds.
func sleepWithBackoff(ctx context.Context, baseMs, attempt int) error {
	if err := sleepCtx(ctx, backoffDuration(baseMs, attempt)); err != nil {
		return fmt.Errorf("clear_confirm backoff cancelled: %w", err)
	}
	return nil
}
