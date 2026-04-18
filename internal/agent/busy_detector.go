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

	// Stage 3: Activity probe (hash comparison over stableSec).
	// Uses CapturePaneJoined (-J) for width-independent hash stability.
	joinedContent, err := bd.paneIO.CapturePaneJoined(paneTarget, bd.config.BusyHintLines)
	if err != nil {
		bd.log("busy_detection joined capture error=%v", err)
		return VerdictUndecided
	}
	hashA := contentHash(joinedContent)
	if err := sleepCtx(ctx, time.Duration(stableSec)*time.Second); err != nil {
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
	if hashChanged {
		verdict = VerdictBusy
	} else {
		// Stable content → idle, regardless of pattern match.
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
// For VerdictUndecided (from tmux errors or context cancellation):
// soft retries with a short interval (half of BusyCheckInterval).
// If all soft retries still yield Undecided, the verdict is promoted to
// VerdictIdle.
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
			bd.log("undecided_soft_retry sleep cancelled: %v", err)
			return VerdictUndecided
		}
		verdict := bd.detectBusy(ctx, paneTarget, stableSec)
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
