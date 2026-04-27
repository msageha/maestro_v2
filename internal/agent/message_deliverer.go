package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// messageDeliverer handles message delivery and /clear confirmation for
// tmux-based agent communication. Extracted from Executor to isolate
// the send/clear responsibility from dispatch routing and lifecycle management.
type messageDeliverer struct {
	paneIO    PaneIO
	paneState *paneStateManager
	config    *model.WatcherConfig // shared with Executor so test mutations propagate
	execCfg   ExecutorConfig
	logger    *log.Logger
	logLevel  logLevel
	paneMu    sync.Map // map[string]*sync.Mutex — per-pane delivery lock
}

// Submit-probe tuning — F-015.
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
//
// Promote them to WatcherConfig when a runtime emerges that needs different
// values; the existing call sites already read these constants by name so
// the migration is mechanical.
const (
	// submitRetryProbeDelay is how long the deliverer waits between probes
	// of pane content after sending Enter, giving Claude Code time to
	// reflect submission.
	submitRetryProbeDelay = 750 * time.Millisecond
	// maxSubmitProbeAttempts caps the probe retry loop to keep delivery
	// latency bounded even when the pane is genuinely stuck. Tuned to
	// `8 * submitRetryProbeDelay = 6s` — long enough for slow paste flows
	// without blocking the dispatcher when the worker is wedged.
	maxSubmitProbeAttempts = 8
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

// removePaneMutex deletes the per-pane mutex entry for the given pane target.
// Call this when a pane is no longer in use to prevent unbounded growth of
// the sync.Map. Currently called when an agent is removed from the watcher's
// tracked set. If new call sites are added that create pane entries, ensure
// corresponding cleanup calls are added.
func (d *messageDeliverer) removePaneMutex(paneTarget string) {
	d.paneMu.Delete(paneTarget)
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
	if cmd, err := d.paneIO.GetPaneCurrentCommand(paneTarget); err == nil {
		if d.paneIO.IsShellCommand(cmd) {
			d.log(logLevelError, "delivery_rejected agent_id=%s task_id=%s reason=pane_is_shell cmd=%s",
				req.AgentID, req.TaskID, cmd)
			return ExecResult{Error: fmt.Errorf("pane is shell (%s), Claude not running", cmd), Retryable: true}
		}
	}

	// Send message via paste-buffer + Enter for reliable multi-line delivery
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := d.paneIO.SendTextAndSubmit(ctx, paneTarget, req.Message); err != nil {
		d.log(logLevelError, "delivery_error agent_id=%s task_id=%s error=send_text: %v",
			req.AgentID, req.TaskID, err)
		return ExecResult{Error: fmt.Errorf("send message: %w", err), Retryable: true}
	}
	probe, err := d.confirmSubmittedOrRetry(ctx, paneTarget, req)
	if err != nil {
		d.log(logLevelError, "delivery_error agent_id=%s task_id=%s error=submit_confirm: %v",
			req.AgentID, req.TaskID, err)
		return ExecResult{Error: fmt.Errorf("confirm submitted: %w", err), Retryable: true}
	}
	if probe.Uncertain() {
		err := fmt.Errorf("%w: status=%s attempts=%d", ErrSubmitConfirmUncertain, probe.Status, probe.Attempts)
		d.log(logLevelWarn, "delivery_submit_unconfirmed agent_id=%s task_id=%s command_id=%s status=%s attempts=%d (non-retryable to avoid duplicate submit)",
			req.AgentID, req.TaskID, req.CommandID, probe.Status, probe.Attempts)
		return ExecResult{Error: err, Retryable: false}
	}

	// Update @status to busy. This is a best-effort post-delivery hint used by
	// the watcher/UI; the message has already been delivered to the pane and
	// the agent will start processing regardless. Returning an error here
	// (Bug L) caused the dispatcher's inline retry to re-deliver the same
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

func (d *messageDeliverer) confirmSubmittedOrRetry(ctx context.Context, paneTarget string, req ExecRequest) (submitProbeResult, error) {
	if !needsSubmitConfirmation(req.Message) {
		return submitProbeResult{Status: submitProbeNotNeeded}, nil
	}
	retried := false
	for attempt := 1; attempt <= maxSubmitProbeAttempts; attempt++ {
		if err := sleepCtx(ctx, submitRetryProbeDelay); err != nil {
			return submitProbeResult{Status: submitProbeExhausted, Attempts: attempt - 1}, fmt.Errorf("wait for submit probe: %w", err)
		}
		content, err := d.paneIO.CapturePaneJoined(paneTarget, d.execCfg.PromptReadyLines)
		if err != nil {
			d.log(logLevelWarn, "submit_confirm capture_failed agent_id=%s task_id=%s error=%v",
				req.AgentID, req.TaskID, err)
			return submitProbeResult{Status: submitProbeCaptureFailed, Attempts: attempt}, nil
		}
		if pastedTextPlaceholderAtPrompt(content) {
			d.log(logLevelWarn, "submit_confirm pasted_text_still_at_prompt agent_id=%s task_id=%s attempt=%d/%d",
				req.AgentID, req.TaskID, attempt, maxSubmitProbeAttempts)
			if err := d.paneIO.SendKeys(paneTarget, "Enter"); err != nil {
				return submitProbeResult{Status: submitProbeRetried, Attempts: attempt}, fmt.Errorf("send retry enter: %w", err)
			}
			retried = true
			continue
		}
		if submittedActivityVisible(content) {
			status := submitProbeConfirmed
			if retried {
				status = submitProbeRetried
			}
			return submitProbeResult{Status: status, Attempts: attempt}, nil
		}
	}
	// F-016: probe budget exhausted without seeing either the pasted-text
	// placeholder OR an activity marker. We deliberately fall through to a
	// non-error return rather than escalating to Retryable: re-delivering the
	// envelope risks double plan_submit (Bug L). Surface the situation as a
	// warn so operators can correlate post-hoc with worker progress.
	d.log(logLevelWarn,
		"submit_confirm probe_budget_exhausted agent_id=%s task_id=%s attempts=%d (non-retryable error returned to avoid double-submit; check worker progress manually if stalled)",
		req.AgentID, req.TaskID, maxSubmitProbeAttempts)
	return submitProbeResult{Status: submitProbeExhausted, Attempts: maxSubmitProbeAttempts}, nil
}

func needsSubmitConfirmation(message string) bool {
	return strings.Contains(message, "\n")
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

func pastedTextPlaceholderAtPrompt(content string) bool {
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < submitPromptSearchLines; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, pastedTextPlaceholder) &&
			(strings.Contains(trimmed, "❯") || strings.HasPrefix(trimmed, ">")) {
			return true
		}
		checked++
	}
	return false
}

// clearAndConfirm sends /clear and confirms it was processed by the target application.
// It retries up to ClearMaxAttempts times. Returns nil on confirmed clear, or an error
// if all attempts fail (fail-closed: caller must NOT proceed with delivery).
//
// Confirmation checks (per poll):
//  1. "/clear" text is NOT visible near the bottom of the pane (primary signal --
//     directly detects the production failure mode where /clear remains as unprocessed
//     text in the input field).
//  2. Pane content hash has changed from pre-clear snapshot (secondary signal).
//  3. Pane content is stable across two consecutive polls.
func (d *messageDeliverer) clearAndConfirm(ctx context.Context, paneTarget string) error {
	timeout := time.Duration(d.config.ClearConfirmTimeoutSec) * time.Second
	pollInterval := time.Duration(d.config.ClearConfirmPollMs) * time.Millisecond
	maxAttempts := d.config.ClearMaxAttempts
	backoffMs := d.config.ClearRetryBackoffMs

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("clear_and_confirm cancelled before attempt %d: %w", attempt, err)
		}

		// Capture pre-clear hash
		preClearContent, err := d.paneIO.CapturePaneJoined(paneTarget, d.execCfg.PromptReadyLines)
		preClearHashValid := err == nil
		if err != nil {
			d.log(logLevelWarn, "clear_confirm pre_capture error=%v attempt=%d (hash check disabled)", err, attempt)
		}
		preClearHash := contentHash(preClearContent)

		// Send /clear with double-enter for reliability
		if err := d.paneIO.SendCommand(paneTarget, "/clear"); err != nil {
			d.log(logLevelWarn, "clear_confirm send_clear error=%v attempt=%d", err, attempt)
			if attempt < maxAttempts {
				if err := sleepWithBackoff(ctx, backoffMs, attempt); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("clear_confirm: %w after %d attempts: %w", ErrClearSendFailed, maxAttempts, err)
		}

		// Wait before sending second Enter (configurable; default 500ms).
		// Claude's /clear command may trigger a completion prompt, requiring a
		// second Enter. The delay ensures the first Enter is processed.
		secondEnterDelay := time.Duration(d.config.ClearSecondEnterDelayMs) * time.Millisecond
		if err := sleepCtx(ctx, secondEnterDelay); err != nil {
			return fmt.Errorf("clear_confirm: wait cancelled: %w", err)
		}

		// Send second Enter to ensure /clear execution.
		if err := d.paneIO.SendKeys(paneTarget, "Enter"); err != nil {
			d.log(logLevelWarn, "clear_confirm send_second_enter error=%v attempt=%d", err, attempt)
			if attempt < maxAttempts {
				if err := sleepWithBackoff(ctx, backoffMs, attempt); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("clear_confirm: %w after %d attempts: %w", ErrSecondEnterFailed, maxAttempts, err)
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
			d.log(logLevelDebug, "clear_confirm confirmed attempt=%d", attempt)
			return nil
		}

		// Not confirmed -- retry with backoff
		d.log(logLevelWarn, "clear_confirm not_confirmed attempt=%d/%d", attempt, maxAttempts)
		if attempt < maxAttempts {
			if err := sleepWithBackoff(ctx, backoffMs, attempt); err != nil {
				return err
			}
		}
	}

	return fmt.Errorf("clear_confirm: %w after %d attempts", ErrClearNotConfirmed, maxAttempts)
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

	currentHash := contentHash(content)

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
//   - With valid pre-clear hash: require hash change + 2 stable polls (debounce).
//   - Without valid pre-clear hash: require 3 stable polls (stricter debounce as fallback).
func (p *clearConfirmationPoller) isConfirmed() bool {
	if p.preClearHashValid {
		return p.hashChanged && p.stableCount >= 2
	}
	return p.stableCount >= 3
}

// reset clears the poller state after a capture error.
func (p *clearConfirmationPoller) reset() {
	p.stableCount = 0
	p.prevPollHash = ""
	p.hashChanged = false
}

func (p *clearConfirmationPoller) log(level logLevel, format string, args ...any) {
	logf(p.logger, p.logLevel, level, "clear_poller", format, args...)
}

// backoffDuration returns the exponential delay used by sleepWithBackoff.
// Extracted as a pure function so unit tests can assert the algebra
// (`base * 2^(attempt-1)`) without paying the wall-clock cost of an actual
// Sleep — which previously made the timing-based assertion (F-057) flaky on
// loaded CI runners.
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
