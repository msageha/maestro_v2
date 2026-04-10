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

// newBusyDetector creates a busyDetector. Zero-value config fields are
// normalized to safe defaults to prevent accidental tight loops.
func newBusyDetector(paneIO PaneIO, busyRegex *regexp.Regexp, cfg busyDetectorConfig, logger *log.Logger, logLevel logLevel) *busyDetector {
	if cfg.IdleStableSec <= 0 {
		cfg.IdleStableSec = 5
	}
	if cfg.BusyCheckMaxRetries <= 0 {
		cfg.BusyCheckMaxRetries = 30
	}
	if cfg.BusyCheckInterval <= 0 {
		cfg.BusyCheckInterval = 2
	}
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

// DetectBusyWithRetry runs busy detection with a retry loop on VerdictBusy.
// VerdictUndecided from transient errors gets up to undecidedImmediateRetries
// immediate retries (no sleep) both on the initial detection and within each
// busy-retry iteration, ensuring consistent handling. If all immediate retries
// still yield Undecided, it is returned as-is.
// agentID is used only for log messages.
// Returns VerdictUndecided if ctx is cancelled during retries.
func (bd *busyDetector) DetectBusyWithRetry(ctx context.Context, paneTarget, agentID string) busyVerdict {
	verdict := bd.detectWithUndecidedRetry(ctx, paneTarget, agentID)

	if verdict != VerdictBusy {
		return verdict
	}

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

func (bd *busyDetector) log(format string, args ...any) {
	logf(bd.logger, bd.logLevel, logLevelDebug, "busy_detector", format, args...)
}
