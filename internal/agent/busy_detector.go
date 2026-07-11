package agent

import (
	"context"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// busyDetectorConfig holds configuration for the busyDetector.
// Values should be pre-normalized (non-zero) via applyDefaults before use.
type busyDetectorConfig struct {
	IdleStableSec       int
	BusyCheckMaxRetries int
	BusyCheckInterval   int
	BusyHintLines       int
	// CPUProbeWindow is the sampling window for the CPU-based busy override
	// used by the Stage 2 fast path (see processBusyProbe), which has no
	// existing sleep to piggyback on. Normalized to defaultCPUProbeWindow
	// when <= 0 by newBusyDetector; tests constructing busyDetector directly
	// can leave it at the zero value for an instant (non-sleeping) probe.
	// Stage 3 reuses its own activity-probe sleep instead of this window
	// (see detectBusy) and pays no extra latency.
	CPUProbeWindow time.Duration
	// DisableCPUProbe turns the CPU-based override off entirely (kill switch
	// for the rare deployment where a pane's process tree legitimately runs
	// noisy long-lived children -- e.g. a dev server or headless browser
	// spawned as an MCP tool -- that would otherwise pin every idle check to
	// "busy"). Not normalized; false (enabled) is the zero value default.
	DisableCPUProbe bool
}

// busyDetector performs 3-stage busy detection on tmux panes.
//
// Stage 1: pane_current_command — quick gate (shell → idle).
// Stage 2: Pattern hint — regex match on last busyHintLines lines.
// Stage 3: Activity probe — hash comparison over IdleStableSec.
//
// Both idle-leaning stages (the Stage 2 fast-path and Stage 3's stable-content
// branch) are text/rendering based and can misjudge a pane as idle while a
// silent, long-running subprocess (e.g. `make`, a fuzzer) is genuinely
// executing with no terminal output for minutes at a time. Before committing
// to either idle verdict, processBusyProbe cross-checks ground truth: did any
// process in the pane's process tree accumulate CPU time during a short
// window? See processBusyProbe.
type busyDetector struct {
	paneIO         PaneIO
	busyRegex      *regexp.Regexp
	config         busyDetectorConfig
	logger         *log.Logger
	logLevel       logLevel
	processSampler processSampler
}

// defaultCPUProbeWindow is the sampling window for the fast-path CPU probe.
// Short: cpuSeconds now carries fractional (centisecond) precision (see
// process_sampler.go), so even a 300ms window reliably observes a delta from
// a genuinely compute-bound subprocess (which pins a core near 100%), while
// staying far below IdleStableSec's multi-second cost.
const defaultCPUProbeWindow = 300 * time.Millisecond

// cpuProbeBusyThresholdRatio is the fraction of the sampling window that must
// be accounted for by accumulated CPU time before the override fires "busy".
// A plain "any increase" check (ratio 0) is too sensitive: idle MCP servers,
// dev servers, or headless browsers spawned as tools routinely burn a few
// percent CPU while genuinely idle, and would otherwise pin the pane to
// VerdictBusy forever. Requiring >=50% of the window as accumulated CPU time
// demands sustained, substantial work (a real compile/fuzz process), which
// idle background chatter will not produce.
const cpuProbeBusyThresholdRatio = 0.5

// newBusyDetector creates a busyDetector. IdleStableSec, BusyCheckMaxRetries,
// and BusyCheckInterval must be pre-normalized via applyDefaults before use.
// BusyHintLines and CPUProbeWindow are normalized here (sourced from
// ExecutorConfig, not WatcherConfig).
func newBusyDetector(paneIO PaneIO, busyRegex *regexp.Regexp, cfg busyDetectorConfig, logger *log.Logger, logLevel logLevel) *busyDetector {
	if cfg.BusyHintLines <= 0 {
		cfg.BusyHintLines = 12
	}
	if cfg.CPUProbeWindow <= 0 {
		cfg.CPUProbeWindow = defaultCPUProbeWindow
	}
	return &busyDetector{
		paneIO:         paneIO,
		busyRegex:      busyRegex,
		config:         cfg,
		logger:         logger,
		logLevel:       logLevel,
		processSampler: psProcessSampler{},
	}
}

// DetectBusy performs one round of the 3-stage busy detection algorithm.
// Returns VerdictBusy if ctx is cancelled during the activity probe sleep
// (conservative: Claude is confirmed running from Stage 1, so "busy" is more
// accurate than the ambiguous VerdictUndecided).
// Returns VerdictUndecided only for genuine tmux capture errors.
func (bd *busyDetector) DetectBusy(ctx context.Context, paneTarget string) busyVerdict {
	return bd.detectBusy(ctx, paneTarget, bd.config.IdleStableSec)
}

// detectBusy is the internal implementation with configurable activity probe
// duration. stableSec controls the Stage 3 sleep duration; callers use the full
// IdleStableSec for initial probes and a shorter value for soft retries where
// content stability has already been confirmed.
func (bd *busyDetector) detectBusy(ctx context.Context, paneTarget string, stableSec int) busyVerdict {
	// Stage 1: pane_current_command — quick gate
	cmd, err := bd.paneIO.GetPaneCurrentCommand(paneTarget)
	if err != nil {
		bd.log("busy_detection pane_current_command error=%v", err)
		return VerdictUndecided
	}
	bd.log("busy_detection started; pane_current_command=%s", cmd)

	// If the pane is running only a shell, no agent CLI is active → idle.
	if bd.paneIO.IsShellCommand(cmd) {
		bd.log("busy_detection pane running shell %q → idle", cmd)
		return VerdictIdle
	}

	// Stage 2: Pattern hint from last busyHintLines lines.
	// Uses CapturePane (no -J) to preserve line boundaries for regex matching.
	content, err := bd.paneIO.CapturePane(paneTarget, bd.config.BusyHintLines)
	if err != nil {
		bd.log("busy_detection capture_pane error=%v", err)
		return VerdictUndecided
	}

	patternMatched := bd.busyRegex != nil && bd.busyRegex.MatchString(content)

	hintStr := "not_matched"
	if patternMatched {
		hintStr = "matched"
	}
	bd.log("busy_detection busy_pattern_hint=%s", hintStr)

	// Bug N: claude-code fast-path. When the runtime is claude-code and the
	// input prompt glyph (❯) is visible in the recently captured content
	// (Stage 2 window) AND no busy pattern matched, the agent is awaiting
	// input — declare idle without running the 5s activity probe. Without
	// this shortcut, claude-code's TUI churn (status bar tips, cursor
	// blink, "auto-update available" notices) flips Stage 3 hash and
	// causes 20+ second delays before Planner/Orchestrator delivery.
	//
	// Safety: pattern hint already guards against the spinner / "esc to
	// interrupt" markers that appear during processing, so combining
	// !patternMatched with prompt-visible is a strong idle signal. Other
	// runtimes (codex, gemini) lack a stable prompt glyph and fall through
	// to the activity probe.
	if !patternMatched && bd.isClaudeReadyFast(paneTarget, content) {
		if bd.processBusyProbe(ctx, paneTarget) {
			bd.log("busy_detection cpu_probe_overrides_fast_path prompt_visible pattern_hint=%s → busy", hintStr)
			return VerdictBusy
		}
		bd.log("busy_detection claude_fast_path_idle prompt_visible pattern_hint=%s", hintStr)
		return VerdictIdle
	}

	// Stage 3: Activity probe (hash comparison over stableSec).
	// Uses CapturePaneJoined (-J) for width-independent hash stability.
	joinedContent, err := bd.paneIO.CapturePaneJoined(paneTarget, bd.config.BusyHintLines)
	if err != nil {
		bd.log("busy_detection joined capture error=%v", err)
		return VerdictUndecided
	}
	hashA := contentHash(joinedContent)

	// Sample CPU now so the activity-probe sleep below doubles as the CPU
	// sampling window too -- this path pays no extra latency for the
	// ground-truth check, unlike the fast path above which has no sleep to
	// reuse and runs its own short dedicated probe.
	cpuBefore, cpuBeforeOK := bd.sampleCPUSeconds(ctx, paneTarget)

	if err := sleepCtx(ctx, time.Duration(stableSec)*time.Second); err != nil {
		// Context cancelled during activity probe. We already confirmed Claude is
		// running (Stage 1) and captured initial content (Stage 3a), but couldn't
		// complete the stability comparison. Return VerdictBusy (conservative) so
		// the caller retries with a fresh context, rather than VerdictUndecided
		// which would surface as "undecided_after_probes" in normal delivery.
		bd.log("busy_detection activity_probe sleep cancelled: %v → treating as busy", err)
		return VerdictBusy
	}

	joinedContent2, err := bd.paneIO.CapturePaneJoined(paneTarget, bd.config.BusyHintLines)
	if err != nil {
		bd.log("busy_detection second capture error=%v", err)
		return VerdictUndecided
	}
	hashB := contentHash(joinedContent2)

	hashChanged := hashA != hashB

	cpuBusy := false
	if !hashChanged && cpuBeforeOK {
		if cpuAfter, ok := bd.sampleCPUSeconds(ctx, paneTarget); ok {
			delta := cpuAfter - cpuBefore
			threshold := float64(stableSec) * cpuProbeBusyThresholdRatio
			cpuBusy = delta >= threshold
			bd.log("busy_detection stage3_cpu_probe before=%.2fs after=%.2fs delta=%.2fs threshold=%.2fs busy=%v",
				cpuBefore, cpuAfter, delta, threshold, cpuBusy)
		}
	}

	var verdict busyVerdict
	switch {
	case hashChanged:
		verdict = VerdictBusy
	case cpuBusy:
		// Stable content but the pane's process tree accumulated substantial
		// CPU time during the same window: a silent build/fuzz command with
		// no terminal output would otherwise be misjudged idle here.
		bd.log("busy_detection cpu_probe_overrides_stage3 stable_content pattern_hint=%s → busy", hintStr)
		verdict = VerdictBusy
	default:
		// Stable content, no CPU activity → idle, regardless of pattern match.
		// When content hasn't changed over the probe interval, any matching
		// busy pattern is stale output from a previous agent turn — not
		// evidence of current activity. This eliminates false undecided
		// verdicts that previously caused delivery timeouts.
		verdict = VerdictIdle
	}

	bd.log("busy_detection activity_probe hash_changed=%v pattern_hint=%s verdict=%s",
		hashChanged, hintStr, verdict)
	return verdict
}

// undecidedImmediateRetries is set to 0: each DetectBusy call includes an
// IdleStableSec sleep for the activity probe, so "immediate" retries actually
// take IdleStableSec seconds each — far from instant. The softRetryUndecided
// mechanism handles tmux error recovery more efficiently using shorter
// activity probes (softRetryStableSec).
const undecidedImmediateRetries = 0

// undecidedSoftRetries is the number of soft retries when VerdictUndecided
// persists after the initial probe (typically from tmux capture errors or
// context cancellation). Each soft retry waits a short interval (half of
// BusyCheckInterval, min 1s) before re-probing with a shorter activity probe
// (softRetryStableSec instead of IdleStableSec). If all soft retries still
// return Undecided, the verdict is promoted to VerdictIdle.
const undecidedSoftRetries = 2

// softRetryStableSec is the activity probe duration for soft retries.
// Shorter than IdleStableSec since soft retries target error recovery
// (transient tmux capture failures) rather than initial stability checks.
// Bounded by IdleStableSec at runtime to respect test configurations.
const softRetryStableSec = 1

// detectWithUndecidedRetry runs DetectBusy and, if the result is
// VerdictUndecided, retries up to undecidedImmediateRetries times with no
// sleep delay (targeting transient tmux capture errors that resolve instantly).
func (bd *busyDetector) detectWithUndecidedRetry(ctx context.Context, paneTarget, agentID string) busyVerdict {
	verdict := bd.DetectBusy(ctx, paneTarget)
	for i := 0; i < undecidedImmediateRetries && verdict == VerdictUndecided; i++ {
		bd.log("busy_undecided_retry retry=%d/%d agent_id=%s",
			i+1, undecidedImmediateRetries, agentID)
		verdict = bd.DetectBusy(ctx, paneTarget)
	}
	return verdict
}

// DetectBusyWithRetry runs busy detection with retry loops for both
// VerdictUndecided and VerdictBusy.
//
// For VerdictUndecided (from genuine tmux capture errors only): soft retries
// with a short interval (half of BusyCheckInterval). If all soft retries still
// yield Undecided, the verdict is promoted to VerdictIdle.
//
// For VerdictBusy: standard retry loop with BusyCheckInterval sleeps,
// up to BusyCheckMaxRetries attempts.
//
// Context cancellation during any sleep returns VerdictBusy (not
// VerdictUndecided) so callers receive a clear retryable "busy" signal
// rather than the ambiguous "undecided_after_probes" condition.
//
// agentID is used only for log messages.
func (bd *busyDetector) DetectBusyWithRetry(ctx context.Context, paneTarget, agentID string) busyVerdict {
	verdict := bd.detectWithUndecidedRetry(ctx, paneTarget, agentID)

	if verdict == VerdictIdle {
		return verdict
	}

	// Soft retry for VerdictUndecided: short wait + re-probe.
	// Pattern matched with stable content often resolves to idle after the
	// pane refreshes during agent state transitions.
	if verdict == VerdictUndecided {
		return bd.softRetryUndecided(ctx, paneTarget, agentID)
	}

	// VerdictBusy: standard retry loop.
	for i := 1; i <= bd.config.BusyCheckMaxRetries; i++ {
		bd.log("busy_retry retry=%d/%d agent_id=%s verdict=%s",
			i, bd.config.BusyCheckMaxRetries, agentID, verdict)
		if err := sleepCtx(ctx, time.Duration(bd.config.BusyCheckInterval)*time.Second); err != nil {
			// Context cancelled during retry sleep. We are in the busy retry
			// loop because the agent was observed busy, so return VerdictBusy
			// (not VerdictUndecided) — the state is known, not ambiguous.
			bd.log("busy_retry sleep cancelled: %v → treating as busy", err)
			return VerdictBusy
		}

		verdict = bd.detectWithUndecidedRetry(ctx, paneTarget, agentID)
		if verdict == VerdictUndecided {
			return bd.softRetryUndecided(ctx, paneTarget, agentID)
		}
		if verdict != VerdictBusy {
			return verdict
		}
	}

	return VerdictBusy
}

// softRetryUndecided performs soft retries for VerdictUndecided with a short
// sleep between probes. This handles transient tmux capture errors that cause
// undecided verdicts. If all soft retries still return Undecided, the verdict
// is promoted to VerdictIdle.
func (bd *busyDetector) softRetryUndecided(ctx context.Context, paneTarget, agentID string) busyVerdict {
	interval := bd.undecidedSoftRetryInterval()
	// Use shorter activity probe; bounded by IdleStableSec for test configs.
	stableSec := softRetryStableSec
	if bd.config.IdleStableSec < stableSec {
		stableSec = bd.config.IdleStableSec
	}
	for i := 0; i < undecidedSoftRetries; i++ {
		bd.log("undecided_soft_retry retry=%d/%d agent_id=%s interval=%s stable_sec=%d",
			i+1, undecidedSoftRetries, agentID, interval, stableSec)
		if err := sleepCtx(ctx, interval); err != nil {
			// Context cancelled during soft-retry sleep. Return VerdictBusy so
			// the caller propagates a retryable "busy" error rather than
			// surfacing VerdictUndecided ("undecided_after_probes") in normal
			// delivery flows where the context simply timed out. Shutdown and
			// delivery timeouts share this ctx in this layer, so cancellation is
			// treated as a conservative fail-closed busy verdict.
			bd.log("undecided_soft_retry sleep cancelled: %v → treating as busy", err)
			return VerdictBusy
		}
		verdict := bd.detectBusy(ctx, paneTarget, stableSec)
		if verdict != VerdictUndecided {
			return verdict
		}
	}
	bd.logAt(logLevelInfo, "undecided_promoted_to_idle agent_id=%s after_soft_retries=%d", agentID, undecidedSoftRetries)
	return VerdictIdle
}

// undecidedSoftRetryInterval returns the sleep duration between soft retries.
// Uses half of BusyCheckInterval with a minimum of 1 second.
func (bd *busyDetector) undecidedSoftRetryInterval() time.Duration {
	sec := bd.config.BusyCheckInterval / 2
	if sec < 1 {
		sec = 1
	}
	return time.Duration(sec) * time.Second
}

// isClaudeReadyFast reports whether the pane's runtime is claude-code AND
// the captured content shows the input prompt glyph (❯). This is the
// fast-path idle signal used to skip the activity probe.
//
// Runtime resolution is fail-closed for non-claude runtimes: when the
// runtime user variable is missing/unreadable or set to anything other
// than claude-code, this returns false and the caller falls through to the
// existing activity probe. Codex and gemini lack a stable prompt glyph, so
// the probe is the right fallback for them.
func (bd *busyDetector) isClaudeReadyFast(paneTarget, content string) bool {
	runtime, err := bd.paneIO.GetUserVar(paneTarget, "runtime")
	if err != nil {
		bd.log("claude_fast_path runtime_read_failed pane=%s error=%v (no fast-path)", paneTarget, err)
		return false
	}
	// Treat empty runtime as the default (claude-code) consistent with
	// other runtime-aware code paths in this package (process_manager.paneRuntime).
	if runtime == "" {
		runtime = model.DefaultRuntime()
	}
	if runtime != model.RuntimeClaudeCode {
		return false
	}
	return isPromptReady(content)
}

// sampleCPUSeconds returns the cumulative CPU time (fractional seconds)
// across the pane's process tree at this instant, or ok=false if the signal
// is unavailable (disabled, no sampler configured, pane_pid unreadable, or
// the sampler itself errored — e.g. a systemic `ps` output parse failure).
// A false result is NOT evidence of idleness; see processBusyProbe.
func (bd *busyDetector) sampleCPUSeconds(ctx context.Context, paneTarget string) (float64, bool) {
	if bd.config.DisableCPUProbe || bd.processSampler == nil {
		return 0, false
	}
	panePID, err := bd.paneIO.GetPanePID(paneTarget)
	if err != nil {
		bd.log("cpu_probe get_pane_pid error=%v", err)
		return 0, false
	}
	root, err := strconv.Atoi(strings.TrimSpace(panePID))
	if err != nil {
		bd.log("cpu_probe parse_pane_pid pid=%q error=%v", panePID, err)
		return 0, false
	}
	seconds, err := bd.processSampler.descendantCPUSeconds(ctx, root)
	if err != nil {
		bd.log("cpu_probe sample error=%v", err)
		return 0, false
	}
	return seconds, true
}

// processBusyProbe runs a short, self-contained CPU-time probe (its own
// sampling window: before-sample, sleep, after-sample) and reports whether
// the pane's process tree accumulated enough CPU time — at least
// cpuProbeBusyThresholdRatio of the window — to count as ground-truth
// evidence of real work. Used by the Stage 2 fast path, which has no
// existing sleep to piggyback on; Stage 3 instead reuses its own
// activity-probe sleep for the same check (see detectBusy) at no added cost.
//
// This is independent of anything rendered on screen — it catches a
// genuinely busy but silent subprocess (e.g. `make`/`configure`/a fuzzer
// producing no terminal output for minutes) that the text-hash and
// pattern-based checks above cannot see.
//
// A false result is NOT evidence of idleness: it only means this particular
// signal was inconclusive (disabled, no sampler, pane_pid unavailable, or
// truly insufficient CPU consumed during the window — which also covers
// I/O-bound waits). Callers must treat this as a veto only in the busy
// direction and keep their existing idle verdict otherwise. The one
// exception is context cancellation during the probe sleep, which — like
// every other sleep in this file — fails closed to "busy" rather than
// silently preserving whatever idle verdict the caller was about to return.
func (bd *busyDetector) processBusyProbe(ctx context.Context, paneTarget string) bool {
	before, ok := bd.sampleCPUSeconds(ctx, paneTarget)
	if !ok {
		return false
	}
	if err := sleepCtx(ctx, bd.config.CPUProbeWindow); err != nil {
		bd.log("cpu_probe sleep cancelled: %v → treating as busy", err)
		return true
	}
	after, ok := bd.sampleCPUSeconds(ctx, paneTarget)
	if !ok {
		return false
	}

	delta := after - before
	threshold := bd.config.CPUProbeWindow.Seconds() * cpuProbeBusyThresholdRatio
	busy := delta >= threshold
	bd.log("cpu_probe before=%.2fs after=%.2fs delta=%.2fs threshold=%.2fs busy=%v", before, after, delta, threshold, busy)
	return busy
}

func (bd *busyDetector) log(format string, args ...any) {
	logf(bd.logger, bd.logLevel, logLevelDebug, "busy_detector", format, args...)
}

// logAt emits a log message at the specified level instead of the default Debug.
// Use for operationally significant events (e.g., undecided→idle promotion).
func (bd *busyDetector) logAt(level logLevel, format string, args ...any) {
	logf(bd.logger, bd.logLevel, level, "busy_detector", format, args...)
}
