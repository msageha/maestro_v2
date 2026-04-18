package agent

import (
	"context"
	"log"
	"regexp"
	"time"
)

// busyDetectorConfig holds configuration for the busyDetector.
// Values should be pre-normalized (non-zero) via applyDefaults before use.
type busyDetectorConfig struct {
	IdleStableSec       int
	BusyCheckMaxRetries int
	BusyCheckInterval   int
	BusyHintLines       int
}

// busyDetector performs 3-stage busy detection on tmux panes.
//
// Stage 1: pane_current_command — quick gate (shell → idle).
// Stage 2: Pattern hint — regex match on last busyHintLines lines.
// Stage 3: Activity probe — hash comparison over IdleStableSec.
type busyDetector struct {
	paneIO    PaneIO
	busyRegex *regexp.Regexp
	config    busyDetectorConfig
	logger    *log.Logger
	logLevel  logLevel
}

// newBusyDetector creates a busyDetector. IdleStableSec, BusyCheckMaxRetries,
// and BusyCheckInterval must be pre-normalized via applyDefaults before use.
// Only BusyHintLines is normalized here (sourced from ExecutorConfig, not WatcherConfig).
func newBusyDetector(paneIO PaneIO, busyRegex *regexp.Regexp, cfg busyDetectorConfig, logger *log.Logger, logLevel logLevel) *busyDetector {
	if cfg.BusyHintLines <= 0 {
		cfg.BusyHintLines = 5
	}
	return &busyDetector{
		paneIO:    paneIO,
		busyRegex: busyRegex,
		config:    cfg,
		logger:    logger,
		logLevel:  logLevel,
	}
}

// DetectBusy performs one round of the 3-stage busy detection algorithm.
// Returns VerdictUndecided if ctx is cancelled during the activity probe sleep.
func (bd *busyDetector) DetectBusy(ctx context.Context, paneTarget string) busyVerdict {
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

	// Stage 3: Activity probe (hash comparison over idle_stable_sec).
	// Uses CapturePaneJoined (-J) for width-independent hash stability.
	joinedContent, err := bd.paneIO.CapturePaneJoined(paneTarget, bd.config.BusyHintLines)
	if err != nil {
		bd.log("busy_detection joined capture error=%v", err)
		return VerdictUndecided
	}
	hashA := contentHash(joinedContent)
	if err := sleepCtx(ctx, time.Duration(bd.config.IdleStableSec)*time.Second); err != nil {
		bd.log("busy_detection activity_probe sleep cancelled: %v", err)
		return VerdictUndecided
	}

	joinedContent2, err := bd.paneIO.CapturePaneJoined(paneTarget, bd.config.BusyHintLines)
	if err != nil {
		bd.log("busy_detection second capture error=%v", err)
		return VerdictUndecided
	}
	hashB := contentHash(joinedContent2)

	hashChanged := hashA != hashB

	var verdict busyVerdict
	switch {
	case hashChanged:
		verdict = VerdictBusy
	case !patternMatched:
		verdict = VerdictIdle
	default:
		verdict = VerdictUndecided
	}

	bd.log("busy_detection activity_probe hash_changed=%v verdict=%s",
		hashChanged, verdict)
	return verdict
}

// undecidedImmediateRetries is the number of immediate retries for
// VerdictUndecided before the main busy-retry loop. These retries have no
// sleep delay, targeting transient tmux capture errors that resolve instantly.
const undecidedImmediateRetries = 2

// undecidedSoftRetries is the number of soft retries when VerdictUndecided
// persists after immediate retries. Each soft retry waits a short interval
// (half of BusyCheckInterval, min 1s) before re-probing, giving the agent
// pane time to update its display during state transitions (e.g., awaiting_fill,
// publish). If all soft retries still return Undecided (pattern matched + stable
// content across multiple probes), the verdict is promoted to VerdictIdle —
// sustained stable content strongly suggests the agent is idle with stale output.
const undecidedSoftRetries = 2

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
// For VerdictUndecided: soft retries with a short interval (half of
// BusyCheckInterval) to let pane output settle during state transitions.
// If all soft retries still yield Undecided, the verdict is promoted to
// VerdictIdle (sustained stable content = likely idle with stale output).
//
// For VerdictBusy: standard retry loop with BusyCheckInterval sleeps,
// up to BusyCheckMaxRetries attempts.
//
// agentID is used only for log messages.
// Returns VerdictUndecided if ctx is cancelled during retries.
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
			bd.log("busy_retry sleep cancelled: %v", err)
			return VerdictUndecided
		}

		verdict = bd.detectWithUndecidedRetry(ctx, paneTarget, agentID)
		if verdict != VerdictBusy {
			return verdict
		}
	}

	return VerdictBusy
}

// softRetryUndecided performs soft retries for VerdictUndecided with a short
// sleep between probes. If all soft retries still return Undecided, the verdict
// is promoted to VerdictIdle — sustained stable content across multiple probes
// spanning several seconds strongly indicates idle with stale output in the pane.
func (bd *busyDetector) softRetryUndecided(ctx context.Context, paneTarget, agentID string) busyVerdict {
	interval := bd.undecidedSoftRetryInterval()
	for i := 0; i < undecidedSoftRetries; i++ {
		bd.log("undecided_soft_retry retry=%d/%d agent_id=%s interval=%s",
			i+1, undecidedSoftRetries, agentID, interval)
		if err := sleepCtx(ctx, interval); err != nil {
			bd.log("undecided_soft_retry sleep cancelled: %v", err)
			return VerdictUndecided
		}
		verdict := bd.detectWithUndecidedRetry(ctx, paneTarget, agentID)
		if verdict != VerdictUndecided {
			return verdict
		}
	}
	bd.log("undecided_promoted_to_idle agent_id=%s after_soft_retries=%d", agentID, undecidedSoftRetries)
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

func (bd *busyDetector) log(format string, args ...any) {
	logf(bd.logger, bd.logLevel, logLevelDebug, "busy_detector", format, args...)
}
