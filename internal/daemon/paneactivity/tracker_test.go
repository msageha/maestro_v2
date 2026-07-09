package paneactivity

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// replaceN substitutes the literal "N" placeholder in s with the decimal
// representation of n. Used by ticker-based liveness tests that build
// frames whose only difference is an elapsed-time counter.
func replaceN(s string, n int) string {
	return strings.ReplaceAll(s, "N", strconv.Itoa(n))
}

func TestTracker_BusyPatternAlone(t *testing.T) {
	re := regexp.MustCompile(`Working|Thinking|Sending`)
	tr := New(re)

	now := time.Now().UTC()
	if !tr.IsActiveAfter("worker1", "  ✻ Thinking…", time.Minute, now) {
		t.Fatalf("busy_pattern match must yield active even without prior snapshot")
	}
}

func TestTracker_HashDeltaAcrossScans(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)
	t2 := t1.Add(60 * time.Second)

	// First observation establishes the baseline; without a previous
	// snapshot the verdict must be inactive (no evidence either way).
	if active := tr.Observe("worker1", "stable content", time.Minute, t0); active {
		t.Fatalf("first observation must not declare active without baseline")
	}

	// Second observation with identical content must remain inactive —
	// the hash hasn't changed despite the cross-scan delta.
	if active := tr.Observe("worker1", "stable content", time.Minute, t1); active {
		t.Fatalf("identical content across scans must be inactive (no hash delta)")
	}

	// Third observation with different content must flip to active.
	if active := tr.Observe("worker1", "stable content + new line", time.Minute, t2); !active {
		t.Fatalf("cross-scan hash delta must be active")
	}
}

func TestTracker_SameScanIsNotActive(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()

	tr.Observe("worker1", "first capture", time.Minute, t0)
	// A second capture with different content but no time has passed
	// should NOT count as active because the previous snapshot is too
	// young — the call may be the same scan firing twice.
	if active := tr.Observe("worker1", "second capture", time.Minute, t0); active {
		t.Fatalf("same-scan observation must not declare active even with hash delta")
	}
}

// TestTracker_TailFrozenScrollbackChurnIsIdle covers the stuck-process
// scenario the user reported (pnpm install hung at "Recreating ..." while
// the surrounding agent UI keeps repainting). The bottom-of-pane is
// frozen for 11+ minutes, but the historical lines above keep changing.
// Tail-only liveness detection must classify this as idle so the lease
// can release rather than infinitely auto-extending.
func TestTracker_TailFrozenScrollbackChurnIsIdle(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)
	t2 := t1.Add(60 * time.Second)

	stuckTail := func(prefix string) string {
		// Build a pane whose scrollback (prefix lines) changes between
		// scans but whose bottom is frozen at the same "Recreating …"
		// status the user observed. The prefix block is wider than
		// activeTailLines so the tail window is entirely the static
		// suffix.
		out := ""
		for i := 0; i < 30; i++ {
			out += prefix + "\n"
		}
		// Frozen tail: at least activeTailLines (12) static lines.
		for i := 0; i < 11; i++ {
			out += "static line " + string(rune('a'+i)) + "\n"
		}
		out += "Recreating .../worker2/web/node_modules\n"
		return out
	}

	if v := tr.ObserveVerdict("worker1", stuckTail("scroll-A"), time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first capture should be Uncertain (no baseline yet), got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", stuckTail("scroll-B"), time.Minute, t1); v != VerdictIdle {
		t.Fatalf("scrollback churn with frozen tail must be VerdictIdle, got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", stuckTail("scroll-C"), time.Minute, t2); v != VerdictIdle {
		t.Fatalf("repeat scrollback churn with frozen tail must remain VerdictIdle, got %v", v)
	}
}

// TestTracker_TailChangesAreActive complements the frozen-tail case: when
// the bottom-of-pane *does* change in non-numeric content across scans
// (real output streaming, e.g. filenames / status text), the verdict must
// be VerdictActive even if the surrounding scrollback stays the same.
// Pure numeric changes (timer ticks, byte counters) are excluded by the
// normalisation layer and tested separately in
// TestTracker_NumericTickerInTailIsIdle.
func TestTracker_TailChangesAreActive(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)

	tr.ObserveVerdict("worker1", "constant scrollback\nrunning task_a\n", time.Minute, t0)
	if v := tr.ObserveVerdict("worker1", "constant scrollback\nrunning task_b\n", time.Minute, t1); v != VerdictActive {
		t.Fatalf("non-numeric tail change must be VerdictActive, got %v", v)
	}
}

// TestTracker_NumericTickerInTailIsIdle: when a hung process is wrapped by
// an LLM agent UI that ticks an elapsed-time counter, the tail hash differs
// every scan and could pin the verdict to Active forever. Numeric
// normalisation collapses timer noise so a hung process is detected as
// Idle while real output (filenames, log lines, test names) still flips
// the verdict to Active.
// TestTracker_NumericTickerWithVerbStaysActive: a pane whose tail
// contains an activity verb (Running…) plus a ticking timer — the
// canonical "subprocess is running, agent UI is rendering" shape — must
// resolve to Active so the lease is preserved while the subprocess
// completes. Pre-2026-05-05 this branch returned Idle and the lease
// was destructively released, double-dispatching the still-running
// task (Report 2026-05-05 P0-1). max_in_progress_min remains the
// wall-clock back-stop for genuinely hung panes.
func TestTracker_NumericTickerWithVerbStaysActive(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)
	t2 := t1.Add(60 * time.Second)

	// Same hung process; only the timer ticks.
	frame := func(elapsed int) string {
		out := ""
		// Pad scrollback so the relevant tail is dominated by the
		// status line.
		for i := 0; i < 30; i++ {
			out += "old log line\n"
		}
		out += "$ sh -c 'sleep 900'\n"
		out += "  ✻ Running… (Ns · esc to interrupt)\n"
		// Substitute N with the actual elapsed seconds.
		out = replaceN(out, elapsed)
		return out
	}

	if v := tr.ObserveVerdict("worker1", frame(5), time.Minute, t0); v != VerdictActive {
		t.Fatalf("verb tail must yield Active via default-hint, got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame(65), time.Minute, t1); v != VerdictActive {
		t.Fatalf("ticking timer with verb must remain Active; got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame(125), time.Minute, t2); v != VerdictActive {
		t.Fatalf("repeat ticker with verb must remain Active; got %v", v)
	}
}

// TestTracker_SpinnerWithVerbStaysActive: spinner glyph rotation plus a
// ticking counter alongside an activity verb (Running…) is the
// canonical agent-busy UI shape and must resolve to Active.
// max_in_progress_min provides the wall-clock back-stop for genuinely
// hung panes.
func TestTracker_SpinnerWithVerbStaysActive(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)
	t2 := t1.Add(60 * time.Second)

	frame := func(spinner rune, elapsed int) string {
		out := ""
		for i := 0; i < 30; i++ {
			out += "scrollback-" + string(rune('a'+i%26)) + "\n"
		}
		out += "$ sh -c 'sleep 600'\n"
		out += string(spinner) + " Running… (Ns · ↑ N tokens · esc to interrupt)\n"
		out = replaceN(out, elapsed)
		return out
	}

	if v := tr.ObserveVerdict("worker1", frame('✻', 5), time.Minute, t0); v != VerdictActive {
		t.Fatalf("first capture with verb tail must yield Active, got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame('⠋', 65), time.Minute, t1); v != VerdictActive {
		t.Fatalf("spinner+verb must remain Active; got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame('⠙', 125), time.Minute, t2); v != VerdictActive {
		t.Fatalf("repeated spinner+verb must remain Active; got %v", v)
	}
}

// TestTracker_VerbAnimationKeepsActiveByDefaultHint: a pane whose Claude
// Code TUI rotates an activity verb ("✻ Running… (45s)") between scans
// MUST be classified Active even when the hash is identical (the
// long-running subprocess holds the same animation frame for >60s).
//
// The earlier contract returned Idle for "verb animation only" so a
// genuinely hung pane could be released, but the package-proxy regression
// (Report 2026-05-05 P0-1) showed the inverse failure mode is much
// worse: pnpm install holding the same `✶ Verifying…` frame caused
// Idle → lease release → double-dispatch to the still-running worker.
// max_in_progress_min remains the wall-clock back-stop for genuinely
// hung panes, so over-permissive Active here only delays recovery; it
// never disables it.
func TestTracker_VerbAnimationKeepsActiveByDefaultHint(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)
	t2 := t1.Add(60 * time.Second)

	frame := func(verb string, elapsed int) string {
		out := ""
		for i := 0; i < 30; i++ {
			out += "scrollback line\n"
		}
		out += "$ claude  # waiting on tool call\n"
		out += "  ✻ " + verb + "… (" + strconv.Itoa(elapsed) + "s · esc to interrupt)\n"
		return out
	}

	if v := tr.ObserveVerdict("worker1", frame("Thinking", 5), time.Minute, t0); v != VerdictActive {
		t.Fatalf("verb animation tail must yield VerdictActive (default-hint catches the verb), got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame("Pondering", 65), time.Minute, t1); v != VerdictActive {
		t.Fatalf("repeated verb animation must remain VerdictActive; got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame("Crafting", 125), time.Minute, t2); v != VerdictActive {
		t.Fatalf("repeated verb animation must remain VerdictActive; got %v", v)
	}
}

// TestTracker_BareTimerWithoutVerbIsIdle pins the symmetric guarantee:
// when the tail has NO activity verb / spinner glyph and only a numeric
// timer is ticking, the cross-scan delta still resolves to Idle so a
// genuinely-hung pane that someone hand-rolled without an animation marker
// can still be released by the busy-check probe.
func TestTracker_BareTimerWithoutVerbIsIdle(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)
	t2 := t1.Add(60 * time.Second)

	frame := func(timer string) string {
		out := ""
		for i := 0; i < 30; i++ {
			out += "scrollback line\n"
		}
		// No activity verb, no spinner glyph — just a bare timer that
		// pure normalisation must collapse to a stable hash.
		out += "$ sh -c 'sleep 600'\n"
		out += "elapsed: " + timer + "\n"
		return out
	}

	if v := tr.ObserveVerdict("worker1", frame("45s"), time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first capture should be Uncertain, got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame("1m 5s"), time.Minute, t1); v != VerdictIdle {
		t.Fatalf("bare timer flip without activity verb must remain VerdictIdle; got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame("2m 5s"), time.Minute, t2); v != VerdictIdle {
		t.Fatalf("repeated bare timer flip must remain VerdictIdle; got %v", v)
	}
}

// TestTracker_JapaneseVerbAnimationIsActive verifies that the
// daemon-side default hint catches Japanese verb animations
// (claude-code Japanese-locale UI: "✶ 検証中…", "⎿ 待機中…"). This is
// the precise marker the package-proxy regression observed when
// pnpm install kept the TUI on `✶ 検証中…` for >60s.
func TestTracker_JapaneseVerbAnimationIsActive(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)

	frame := func(verb string) string {
		out := ""
		for i := 0; i < 30; i++ {
			out += "scrollback line\n"
		}
		out += "  ✶ " + verb + "… (45s · esc to interrupt)\n"
		return out
	}

	if v := tr.ObserveVerdict("worker1", frame("検証中"), time.Minute, t0); v != VerdictActive {
		t.Fatalf("Japanese verb 検証中 must trigger active-hint, got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame("検証中"), time.Minute, t1); v != VerdictActive {
		t.Fatalf("repeated Japanese verb must remain VerdictActive; got %v", v)
	}
}

// TestTracker_ClaudeSubprocessOutputMarkerIsActive: claude-code's `⎿ `
// prefix on subprocess stderr/stdout lines is unambiguous evidence the
// agent is rendering live tool output.
func TestTracker_ClaudeSubprocessOutputMarkerIsActive(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)

	frame := func(line string) string {
		out := ""
		for i := 0; i < 30; i++ {
			out += "scrollback line\n"
		}
		out += "  ⎿ " + line + "\n"
		return out
	}

	if v := tr.ObserveVerdict("worker1", frame("compiling foo.go"), time.Minute, t0); v != VerdictActive {
		t.Fatalf("⎿ subprocess marker must trigger active-hint, got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame("compiling foo.go"), time.Minute, t1); v != VerdictActive {
		t.Fatalf("repeated ⎿ marker must remain VerdictActive; got %v", v)
	}
}

// TestTracker_RealProgressOverridesNumericNormalisation: when the line
// content changes (filenames, status text, etc.) on top of timer ticks,
// the verdict must still resolve to Active.
func TestTracker_RealProgressOverridesNumericNormalisation(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)

	tr.ObserveVerdict("worker1", "running pytest test_a (1s)\n", time.Minute, t0)
	if v := tr.ObserveVerdict("worker1", "running pytest test_b (2s)\n", time.Minute, t1); v != VerdictActive {
		t.Fatalf("filename change must override numeric normalisation; got %v", v)
	}
}

// TestTracker_BusyPatternInScrollbackOnlyDoesNotPin pins the bug fix:
// "Thinking" appearing in scrollback alone must not keep the verdict
// pinned to Active when the tail of the pane has gone idle.
func TestTracker_BusyPatternInScrollbackOnlyDoesNotPin(t *testing.T) {
	re := regexp.MustCompile(`Working|Thinking|Sending`)
	tr := New(re)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)

	withScrollback := func(suffix string) string {
		// Old scrollback contains a "Thinking" line, but the bottom is
		// silent. activeTailLines is 12, so we put the busy keyword far
		// above the tail window.
		out := "Old: ✻ Thinking…\n"
		for i := 0; i < 30; i++ {
			out += "filler line\n"
		}
		out += suffix
		return out
	}

	if v := tr.ObserveVerdict("worker1", withScrollback("$ "), time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first capture should be Uncertain, got %v", v)
	}
	// Same content again — Thinking is in scrollback, tail is idle. Must
	// fall through to Idle, not pinned to Active.
	if v := tr.ObserveVerdict("worker1", withScrollback("$ "), time.Minute, t1); v != VerdictIdle {
		t.Fatalf("scrollback-only busy pattern must NOT pin verdict to Active; got %v", v)
	}
}

func TestTracker_ForgetAgentResetsBaseline(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	tr.Observe("worker1", "snapshot", time.Minute, t0)
	tr.ForgetAgent("worker1")
	t1 := t0.Add(2 * time.Minute)
	if active := tr.Observe("worker1", "different content", time.Minute, t1); active {
		t.Fatalf("after ForgetAgent, the next observation has no baseline so must be inactive")
	}
}

func TestTracker_LastSnapshotReportsCachedState(t *testing.T) {
	re := regexp.MustCompile(`Sending`)
	tr := New(re)
	now := time.Now().UTC()
	tr.Observe("worker1", "just chatting…", time.Minute, now)
	snap, ok := tr.LastSnapshot("worker1")
	if !ok {
		t.Fatalf("LastSnapshot must return true after Observe")
	}
	if snap.MatchedBusy {
		t.Errorf("MatchedBusy should be false for non-matching content")
	}
	if snap.ContentHash == "" {
		t.Errorf("ContentHash must be populated")
	}
	if !snap.CapturedAt.Equal(now) {
		t.Errorf("CapturedAt should match the time passed to Observe")
	}
}

func TestTracker_NilRegexDisablesPatternBranch(t *testing.T) {
	tr := New(nil)
	now := time.Now().UTC()
	if active := tr.IsActiveAfter("worker1", "Thinking…", time.Minute, now); active {
		t.Fatalf("with nil regex, pattern match must not flip the verdict")
	}
}

func TestTracker_SetBusyPatternUpdatesPattern(t *testing.T) {
	tr := New(nil)
	now := time.Now().UTC()
	if active := tr.IsActiveAfter("worker1", "Thinking…", time.Minute, now); active {
		t.Fatalf("nil regex baseline must report inactive")
	}
	tr.SetBusyPattern(regexp.MustCompile(`Thinking`))
	if active := tr.IsActiveAfter("worker1", "Thinking…", time.Minute, now); !active {
		t.Fatalf("after SetBusyPattern, matching content must flip to active")
	}
}

// TestTracker_ObserveVerdict_ReturnsUncertainOnFirstObservation pins the
// invariant: ObserveVerdict must distinguish "no baseline yet"
// (VerdictUncertain) from "baseline exists and shows no change"
// (VerdictIdle) so the caller can grace-extend instead of releasing on
// the first observation.
func TestTracker_ObserveVerdict_ReturnsUncertainOnFirstObservation(t *testing.T) {
	tr := New(nil)
	now := time.Now().UTC()
	v := tr.ObserveVerdict("worker1", "anything", time.Minute, now)
	if v != VerdictUncertain {
		t.Fatalf("first observation without baseline must yield VerdictUncertain, got %s", v)
	}
}

// TestTracker_ObserveVerdict_MatchesBusyPatternAsActive pins the
// fast-path: a known busy marker (e.g., "Thinking…") yields VerdictActive
// even on the very first observation, so workers that happen to be
// thinking when their lease first expires do not fall into the
// uncertain-then-released failure mode.
func TestTracker_ObserveVerdict_MatchesBusyPatternAsActive(t *testing.T) {
	tr := New(regexp.MustCompile(`Thinking`))
	now := time.Now().UTC()
	v := tr.ObserveVerdict("worker1", "✻ Thinking…", time.Minute, now)
	if v != VerdictActive {
		t.Fatalf("busy_pattern match must yield VerdictActive, got %s", v)
	}
}

// TestTracker_ObserveVerdict_DistinguishesIdleFromUncertain pins the
// trichotomy boundary that the legacy boolean Observe could not express:
// when there IS a previous snapshot at least minPrevAge old AND the
// content hash has not changed, the verdict is VerdictIdle (release is
// safe). That is materially different from the no-baseline case
// (VerdictUncertain — release is unsafe).
func TestTracker_ObserveVerdict_DistinguishesIdleFromUncertain(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	if v := tr.ObserveVerdict("worker1", "frozen", time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first observation must be VerdictUncertain, got %s", v)
	}
	t1 := t0.Add(90 * time.Second)
	if v := tr.ObserveVerdict("worker1", "frozen", time.Minute, t1); v != VerdictIdle {
		t.Fatalf("identical hash across scans must be VerdictIdle, got %s", v)
	}
}

// TestTracker_ObserveVerdict_UncertainOnSameScanDuplicate pins the
// minPrevAge guard at the verdict layer: two captures for the same
// agent within minPrevAge of each other must NOT register as Active
// even if the hash differs — the wall clock has not advanced enough
// for "the pane changed across scans" to be a coherent claim.
func TestTracker_ObserveVerdict_UncertainOnSameScanDuplicate(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	tr.ObserveVerdict("worker1", "first", time.Minute, t0)
	v := tr.ObserveVerdict("worker1", "second", time.Minute, t0) // same wall time
	if v != VerdictUncertain {
		t.Fatalf("same-scan duplicate must be VerdictUncertain, got %s", v)
	}
}

// TestTracker_ObserveVerdict_ConsecutiveUncertainCapsAtIdle pins the
// streak cap: consecutive no-baseline VerdictUncertain results for the
// same agent must downgrade to VerdictIdle once the streak crosses
// MaxUncertainStreak. Without this cap a stuck pane whose snapshot
// keeps getting cleared would be grace-extended forever and the
// busy-check release path would never run.
//
// We force the no-baseline path on the second call by ForgetAgent-ing
// between observations, simulating the production pattern where the
// snapshot keeps getting cleared (or never persisting) for the same
// agentID across consecutive lease-expiry observations.
func TestTracker_ObserveVerdict_ConsecutiveUncertainCapsAtIdle(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	// First observation: no baseline → Uncertain (streak = 1)
	if v := tr.ObserveVerdict("worker1", "first", time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first observation must be VerdictUncertain, got %s", v)
	}
	// Force no-baseline path on next call (simulates a baseline that
	// keeps getting reset / never persisting).
	tr.mu.Lock()
	delete(tr.snapshots, "worker1")
	tr.mu.Unlock()
	t1 := t0.Add(2 * time.Minute)
	// Second no-baseline observation → would be Uncertain again
	// (streak = 2) but capped to VerdictIdle so caller falls through
	// to busy-check.
	if v := tr.ObserveVerdict("worker1", "second", time.Minute, t1); v != VerdictIdle {
		t.Fatalf("consecutive no-baseline Uncertain past streak cap must downgrade to VerdictIdle, got %s", v)
	}
}

// TestTracker_ObserveVerdict_SameScanDuplicateNotStreaked pins that the
// no-baseline streak counter must NOT count same-scan duplicates
// (within minPrevAge). Same-scan duplicates are independent timing
// noise; counting them would erode the cap on legitimate workloads
// that happen to fire two captures in quick succession.
func TestTracker_ObserveVerdict_SameScanDuplicateNotStreaked(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	// First observation: no baseline → Uncertain, streak = 1
	if v := tr.ObserveVerdict("worker1", "first", time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first observation must be VerdictUncertain, got %s", v)
	}
	// Same-scan duplicate (within minPrevAge) — Uncertain via timing,
	// must NOT count toward the streak cap.
	t1 := t0.Add(time.Second)
	if v := tr.ObserveVerdict("worker1", "second", time.Minute, t1); v != VerdictUncertain {
		t.Fatalf("same-scan duplicate must remain VerdictUncertain (timing-only), got %s", v)
	}
	// Now a third call across a real cross-scan window — content
	// stable, baseline exists, time elapsed → Idle (not Uncertain).
	t2 := t1.Add(2 * time.Minute)
	if v := tr.ObserveVerdict("worker1", "second", time.Minute, t2); v != VerdictIdle {
		t.Fatalf("cross-scan stable content must yield VerdictIdle, got %s", v)
	}
}

// TestTracker_ObserveVerdict_BlockedPromptReturnsBlocked: when the
// captured pane content contains a confirmation-prompt shape (the
// agent CLI is blocked asking the operator to choose Yes/No),
// ObserveVerdict returns the dedicated VerdictBlocked. Callers treat
// VerdictBlocked the same as VerdictActive for lease extension (the
// agent process is alive) but MUST NOT refresh circuit-breaker
// progress timestamps so progress_timeout can still trip after the
// configured window. Earlier versions returned VerdictIdle which let
// the busy-check release the lease, racing with eventual operator
// approval and surfacing as FENCING_REJECT when the worker's already-
// in-flight Bash invocation finally fired (Reports 1 & 2 of
// 2026-05-03); a follow-up VerdictActive variant kept the lease alive
// but inadvertently refreshed last_progress_at via MarkProgress and
// defeated the back-stop (Reports of 2026-05-04).
func TestTracker_ObserveVerdict_BlockedPromptReturnsBlocked(t *testing.T) {
	tr := New(regexp.MustCompile(`Working|Thinking`))
	t0 := time.Now().UTC()

	content := "✻ Thinking… (some scrollback)\n" +
		"Do you want to create verify.sh?\n" +
		"❯ 1. Yes\n" +
		"  2. Yes, and allow Claude to edit its own settings for this session\n" +
		"  3. No\n"

	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("blocked confirmation prompt must yield VerdictBlocked, got %s", v)
	}
}

// TestTracker_ObserveVerdict_BlockedPromptInBox: claude-code 2.x wraps
// the approval prompt in a Unicode box with `│ ` line prefixes. The
// blocked-prompt regex must match through the box-drawing prefix so the
// daemon observes the prompt and the blocked-pane timeout can fail the
// task. Pre-2026-05-05 the strict `^[ \t]*` anchor missed these and the
// pane sat wedged for 30 minutes until the circuit breaker tripped
// (Report 2026-05-05 P0-2).
func TestTracker_ObserveVerdict_BlockedPromptInBox(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()

	content := "● Bash(pnpm install)\n" +
		"  ⎿ Running…\n" +
		"\n" +
		"╭───────────────────────────────────────────────╮\n" +
		"│ Bash command                                  │\n" +
		"│   pnpm install                                │\n" +
		"│                                               │\n" +
		"│ Do you want to proceed?                       │\n" +
		"│ ❯ 1. Yes                                      │\n" +
		"│   2. Yes, and don't ask again                 │\n" +
		"│   3. No                                       │\n" +
		"╰───────────────────────────────────────────────╯\n"

	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("box-wrapped approval prompt must yield VerdictBlocked, got %s", v)
	}
}

// TestTracker_ObserveVerdict_BlockedFileEditPrompt: claude-code 2.x asks
// "Do you want to make this edit to <file>?" before modifying paths
// like .claude/verify.sh even with --dangerously-skip-permissions
// (Report 2026-05-05). Pattern H must catch this.
func TestTracker_ObserveVerdict_BlockedFileEditPrompt(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()

	content := "Edit operation requested.\n" +
		"\n" +
		"│ Do you want to make this edit to verify.sh?   │\n" +
		"│ ❯ 1. Yes                                      │\n" +
		"│   2. No                                       │\n"

	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("file-edit confirmation must yield VerdictBlocked, got %s", v)
	}
}

// TestTracker_ObserveVerdict_BlockedShellPromptReturnsBlocked exercises
// the (y/n) tail variant — same dedicated VerdictBlocked output for
// shell-style confirmations issued by language-agnostic CLIs (npm,
// brew, apt, etc.).
func TestTracker_ObserveVerdict_BlockedShellPromptReturnsBlocked(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()

	content := "Some output\n...\nProceed? (Y/n)"
	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("shell (Y/n) prompt must yield VerdictBlocked, got %s", v)
	}
}

// TestTracker_BlockedSince_StampedOnFirstBlockedAndStable verifies that
// BlockedSince is recorded on the FIRST blocked observation and preserved
// across consecutive blocked outcomes so callers can compute total wedged
// time. The queue scanner relies on this to enforce a tight blocked-pane
// timeout that fails the in-flight task before circuit_breaker
// progress_timeout (30 min default) would.
func TestTracker_BlockedSince_StampedOnFirstBlockedAndStable(t *testing.T) {
	tr := New(nil)
	if _, ok := tr.BlockedSince("worker1"); ok {
		t.Fatalf("BlockedSince must be unset before any observation")
	}

	t0 := time.Now().UTC()
	content := "Do you want to proceed?\n❯ 1. Yes\n  2. No"
	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("first blocked observation must yield VerdictBlocked, got %s", v)
	}
	first, ok := tr.BlockedSince("worker1")
	if !ok || !first.Equal(t0) {
		t.Fatalf("BlockedSince must be stamped to t0 on first blocked, got ok=%v ts=%v", ok, first)
	}

	// Consecutive blocked observation MUST NOT slide the timestamp forward —
	// callers compute (now - blockedSince) as the wedged duration.
	t1 := t0.Add(2 * time.Minute)
	if v := tr.ObserveVerdict("worker1", content, time.Minute, t1); v != VerdictBlocked {
		t.Fatalf("second blocked observation must yield VerdictBlocked, got %s", v)
	}
	second, ok := tr.BlockedSince("worker1")
	if !ok || !second.Equal(t0) {
		t.Fatalf("BlockedSince must remain at t0 across consecutive blocked, got ok=%v ts=%v", ok, second)
	}
}

// TestTracker_BlockedSince_ClearedOnNonBlocked verifies that any
// non-blocked verdict (Active / Idle / Uncertain) clears the
// blocked-since timestamp so a subsequent block starts a fresh run.
func TestTracker_BlockedSince_ClearedOnNonBlocked(t *testing.T) {
	tr := New(regexp.MustCompile(`Thinking`))
	t0 := time.Now().UTC()
	blockedContent := "Do you want to proceed?\n❯ 1. Yes"
	if v := tr.ObserveVerdict("worker1", blockedContent, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("first observation must yield VerdictBlocked, got %s", v)
	}
	if _, ok := tr.BlockedSince("worker1"); !ok {
		t.Fatalf("BlockedSince must be set after a blocked observation")
	}

	// Active verdict (busy_pattern match) must clear blocked-since.
	t1 := t0.Add(time.Minute)
	if v := tr.ObserveVerdict("worker1", "✻ Thinking…", time.Minute, t1); v != VerdictActive {
		t.Fatalf("active observation must yield VerdictActive, got %s", v)
	}
	if _, ok := tr.BlockedSince("worker1"); ok {
		t.Fatalf("BlockedSince must be cleared after a non-blocked observation")
	}

	// New blocked run must stamp a fresh timestamp.
	t2 := t1.Add(time.Minute)
	if v := tr.ObserveVerdict("worker1", blockedContent, time.Minute, t2); v != VerdictBlocked {
		t.Fatalf("third observation must yield VerdictBlocked, got %s", v)
	}
	since, ok := tr.BlockedSince("worker1")
	if !ok || !since.Equal(t2) {
		t.Fatalf("BlockedSince must be stamped to t2 on the new blocked run, got ok=%v ts=%v", ok, since)
	}
}

func TestTracker_BlockedClass_UnrecoverablePrompts(t *testing.T) {
	t0 := time.Now().UTC()
	cases := []struct {
		name    string
		content string
	}{
		{
			name: "unsandboxed_command_tail_prompt",
			content: "some output\n" +
				"Bash command (unsandboxed)\n" +
				"Do you want to proceed?\n" +
				"❯ 1. Yes\n" +
				"  2. No\n",
		},
		{
			name: "protected_path_edit_prompt",
			content: "Edit operation requested.\n" +
				"│ Do you want to make this edit to /foo? │\n",
		},
		{
			name: "protected_path_edit_prompt_case_spacing_variant",
			content: "Edit operation requested.\n" +
				"│ Do you want to Make this edit to/foo? │\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := New(nil)
			if v := tr.ObserveVerdict("worker1", tc.content, time.Minute, t0); v != VerdictBlocked {
				t.Fatalf("ObserveVerdict = %s, want blocked", v)
			}
			if _, ok := tr.BlockedSince("worker1"); !ok {
				t.Fatalf("BlockedSince must be set")
			}
			if got := tr.BlockedClass("worker1"); got != "unrecoverable" {
				t.Fatalf("BlockedClass = %q, want unrecoverable", got)
			}
		})
	}
}

// TestTracker_BlockedClass_ScrollbackEditPromptNotUnrecoverable pins Fix E:
// the "make this edit to" literal classifies as unrecoverable from the pane
// TAIL only. Scrollback can legitimately retain the phrase from an
// already-cleared prompt (or quoted task text); classifying on it would fail
// a healthy in-flight task on the shortened unrecoverable timeout.
func TestTracker_BlockedClass_ScrollbackEditPromptNotUnrecoverable(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()

	// The edit-prompt literal sits deeper than activeTailLines above the
	// bottom; the pane is blocked on an ORDINARY prompt in the tail.
	content := "│ Do you want to make this edit to /foo? │\n" +
		strings.Repeat("build output line\n", activeTailLines+2) +
		"Do you want to proceed?\n❯ 1. Yes\n  2. No\n"

	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("ObserveVerdict = %s, want blocked", v)
	}
	if got := tr.BlockedClass("worker1"); got != "" {
		t.Fatalf("BlockedClass = %q, want empty (scrollback-only edit prompt must not classify unrecoverable)", got)
	}

	// The same literal inside the tail still classifies unrecoverable.
	tailContent := "some output\n│ Do you want to make this edit to /foo? │\n❯ 1. Yes\n  2. No\n"
	if v := tr.ObserveVerdict("worker2", tailContent, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("ObserveVerdict = %s, want blocked", v)
	}
	if got := tr.BlockedClass("worker2"); got != "unrecoverable" {
		t.Fatalf("BlockedClass = %q, want unrecoverable for tail edit prompt", got)
	}
}

func TestTracker_BlockedClass_OrdinaryBlockedAndNonBlocked(t *testing.T) {
	tr := New(regexp.MustCompile(`Thinking`))
	t0 := time.Now().UTC()

	ordinaryBlocked := "Do you want to proceed?\n❯ 1. Yes\n  2. No\n"
	if v := tr.ObserveVerdict("worker1", ordinaryBlocked, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("ordinary blocked prompt must yield VerdictBlocked, got %s", v)
	}
	if _, ok := tr.BlockedSince("worker1"); !ok {
		t.Fatalf("BlockedSince must be set for ordinary blocked prompt")
	}
	if got := tr.BlockedClass("worker1"); got != "" {
		t.Fatalf("ordinary blocked prompt BlockedClass = %q, want empty", got)
	}

	if v := tr.ObserveVerdict("worker1", "✻ Thinking…", time.Minute, t0.Add(time.Minute)); v != VerdictActive {
		t.Fatalf("active observation must yield VerdictActive, got %s", v)
	}
	if got := tr.BlockedClass("worker1"); got != "" {
		t.Fatalf("non-blocked active pane BlockedClass = %q, want empty", got)
	}
}

func TestTracker_BlockedClass_ClearedOnNonBlocked(t *testing.T) {
	tr := New(regexp.MustCompile(`Thinking`))
	t0 := time.Now().UTC()

	unrecoverableBlocked := "Bash command (unsandboxed)\nDo you want to proceed?\n❯ 1. Yes\n  2. No\n"
	if v := tr.ObserveVerdict("worker1", unrecoverableBlocked, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("unrecoverable blocked prompt must yield VerdictBlocked, got %s", v)
	}
	if got := tr.BlockedClass("worker1"); got != "unrecoverable" {
		t.Fatalf("BlockedClass = %q, want unrecoverable", got)
	}

	if v := tr.ObserveVerdict("worker1", "✻ Thinking…", time.Minute, t0.Add(time.Minute)); v != VerdictActive {
		t.Fatalf("active observation must yield VerdictActive, got %s", v)
	}
	if _, ok := tr.BlockedSince("worker1"); ok {
		t.Fatalf("BlockedSince must be cleared after non-blocked observation")
	}
	if got := tr.BlockedClass("worker1"); got != "" {
		t.Fatalf("BlockedClass = %q, want empty after non-blocked observation", got)
	}
}

// TestTracker_ObserveVerdict_BlockedDetectorDisablable pins that
// SetBlockedPattern(nil) disables the override — useful for callers
// that want to measure raw busy/hash signals.
func TestTracker_ObserveVerdict_BlockedDetectorDisablable(t *testing.T) {
	tr := New(regexp.MustCompile(`Thinking`))
	tr.SetBlockedPattern(nil)
	t0 := time.Now().UTC()

	content := "✻ Thinking…\n❯ 1. Yes\n  2. No"
	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictActive {
		t.Fatalf("with blocked detector disabled, busy_pattern must win → Active, got %s", v)
	}
}

// TestTracker_ObserveVerdict_ActiveResetsUncertainStreak pins that the
// streak cap is tied specifically to consecutive Uncertain — once an
// Active outcome breaks the run, the next Uncertain again gets a single
// grace pass. This preserves the original "give a brand-new agent one
// cycle" intent for agents that recover from a transient stall.
func TestTracker_ObserveVerdict_ActiveResetsUncertainStreak(t *testing.T) {
	tr := New(regexp.MustCompile(`Thinking`))
	t0 := time.Now().UTC()
	if v := tr.ObserveVerdict("worker1", "first", time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first observation must be VerdictUncertain, got %s", v)
	}
	// Active break (busy pattern matches) — streak resets.
	t1 := t0.Add(2 * time.Minute)
	if v := tr.ObserveVerdict("worker1", "✻ Thinking…", time.Minute, t1); v != VerdictActive {
		t.Fatalf("busy match must yield VerdictActive, got %s", v)
	}
	// Same-scan duplicate (within minPrevAge) immediately after Active
	// must still get one grace pass since the streak was reset.
	t2 := t1.Add(time.Second)
	if v := tr.ObserveVerdict("worker1", "second", time.Minute, t2); v != VerdictUncertain {
		t.Fatalf("Uncertain after streak reset must yield Uncertain (grace), got %s", v)
	}
}

// TestTracker_HintLogThrottle pins the post-2026-05-06 throttle on
// `pane_blocked_prompt_hint_unmatched`: a hint log for the same
// (agent, hint-class) is suppressed within blockedHintLogWindow, but a
// new hint class re-emits immediately. Pre-2026-05-06 this throttle
// keyed on the tail hash and let spinner / counter noise slip through
// (Report 2026-05-06 P1-3, P2-3). Direct unit test against
// shouldEmitHintLog so the assertion does not depend on slog output
// capture.
func TestTracker_HintLogThrottle(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()

	// First emission for an agent always passes.
	if !tr.shouldEmitHintLog("worker1", "❯", t0) {
		t.Fatalf("first hint log for agent must emit")
	}
	// Same hint class within the window is suppressed (even though
	// real tail bytes would have shifted because of timer / spinner
	// noise — keying on hint-class makes the throttle robust).
	if tr.shouldEmitHintLog("worker1", "❯", t0.Add(blockedHintLogWindow/2)) {
		t.Fatalf("duplicate hint log within window must be suppressed")
	}
	// Same hint class after the window passes.
	if !tr.shouldEmitHintLog("worker1", "❯", t0.Add(blockedHintLogWindow+time.Second)) {
		t.Fatalf("hint log after window must re-emit")
	}
	// Different hint class emits immediately, regardless of window.
	if !tr.shouldEmitHintLog("worker1", "do you want", t0.Add(blockedHintLogWindow+2*time.Second)) {
		t.Fatalf("changed hint class must re-emit immediately")
	}
	// Different agent has its own throttle bucket.
	if !tr.shouldEmitHintLog("worker2", "❯", t0) {
		t.Fatalf("different agent must have an independent throttle bucket")
	}
}

// TestTracker_HintLogThrottle_StableAcrossSpinnerNoise pins the
// behaviour the previous tail-hash throttle missed: when the pane is
// stuck on a confirmation prompt, surrounding spinner / counter
// rotation should NOT defeat the throttle. classifyBlockedHint folds
// spinner-driven cross-scan delta into a stable hint class.
func TestTracker_HintLogThrottle_StableAcrossSpinnerNoise(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()

	frames := []string{
		"❯ 1. Yes\n  2. No\n✻ Cogitating for 1m 38s",
		"❯ 1. Yes\n  2. No\n✻ Cogitating for 1m 39s",
		"❯ 1. Yes\n  2. No\n✻ Cogitating for 1m 40s",
		"❯ 1. Yes\n  2. No\n◆ Pondering for 1m 41s",
	}
	emitted := 0
	for i, f := range frames {
		if hintClass := classifyBlockedHint(f); hintClass != "" {
			if tr.shouldEmitHintLog("worker1", hintClass, t0.Add(time.Duration(i)*5*time.Second)) {
				emitted++
			}
		}
	}
	if emitted != 1 {
		t.Errorf("expected exactly 1 emission across spinner/counter noise, got %d", emitted)
	}
}

// TestTracker_TerminalErrorDetectedFromPaneContent pins the post-2026-05-06
// P0-2 fix: when the agent runtime emits a non-recoverable error frame
// (Claude API 4xx, content filter, invalid_request_error), ObserveVerdict
// must return VerdictTerminalError so the queue scanner can fail the
// task immediately instead of extending the lease for max_in_progress_min.
func TestTracker_TerminalErrorDetectedFromPaneContent(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()

	cases := []struct {
		name    string
		content string
	}{
		{
			name: "claude content filter",
			content: `> Working on task...
API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"Output blocked by content filtering policy"}}`,
		},
		{
			name:    "anthropic invalid_request_error",
			content: `Claude returned: invalid_request_error: max_tokens exceeded`,
		},
		{
			name:    "permission_error",
			content: `API Error: 403 permission_error organization disabled`,
		},
		{
			name:    "authentication_error",
			content: `authentication_error: revoked api key`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr2 := New(nil)
			if v := tr2.ObserveVerdict("worker1", tc.content, time.Minute, t0); v != VerdictTerminalError {
				t.Fatalf("%s: expected VerdictTerminalError, got %s", tc.name, v)
			}
		})
	}
	_ = tr
}

// TestTracker_TerminalErrorBeatsActiveHint pins precedence: even when the
// pane content also contains spinner / activity verbs (a stale verb in
// scrollback alongside the terminal error frame), terminal-error detection
// MUST win. Without this precedence the lease would keep extending under
// VerdictActive while the runtime is permanently broken.
func TestTracker_TerminalErrorBeatsActiveHint(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	content := `Cogitating for 1m 38s
API Error: 400 Output blocked by content filtering policy
> _`
	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictTerminalError {
		t.Fatalf("terminal error must win over active hint; got %s", v)
	}
}

// TestTracker_BlockedDetector_BareProceedPrompt pins post-2026-05-06
// P2 #3: a pane wedged on a "Proceed? (y/n)" / "Proceed? [Y/n]" line
// (the leading "Do you want to ..." has scrolled off the captured
// region) must be classified as VerdictBlocked so the blocked-pane
// timeout fires. Y/N suffix is required to avoid false-positive on
// natural language scrollback (commit messages, man pages, docstrings).
func TestTracker_BlockedDetector_BareProceedPrompt(t *testing.T) {
	t0 := time.Now().UTC()

	cases := []string{
		"some output\nProceed? (y/n)",
		"...trailing context line\nProceed? [Y/n]",
		"banner above\nProceed? Y",
	}
	for _, content := range cases {
		tr := New(nil)
		if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictBlocked {
			t.Errorf("Proceed? prompt with Y/N suffix should be VerdictBlocked, got %s for content=%q", v, content)
		}
	}
}

// TestTracker_BlockedDetector_BashUnsandboxedBanner pins post-2026-05-06
// P2 #3: Claude Code's `Bash command (unsandboxed) ... Do you want to
// proceed?` confirmation must be classified as VerdictBlocked. This
// surfaces when the operator has `bypass permissions on` for normal
// Bash but the runtime still asks for a per-command sandbox bypass.
func TestTracker_BlockedDetector_BashUnsandboxedBanner(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()

	content := "Bash command (unsandboxed)\n  Do you want to proceed?\n  ❯ 1. Yes\n    2. No"
	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictBlocked {
		t.Fatalf("Bash (unsandboxed) approval banner should be VerdictBlocked, got %s", v)
	}
}

// TestTracker_BlockedDetector_NarrowPatternsAvoidFalsePositive pins
// post-2026-05-06 round-3 report: the narrow Pattern I (Proceed? + Y/N
// suffix) and Pattern J (Bash command (unsandboxed)) are tail-only so
// scrollback occurrences of bare "Proceed?" or "(unsandboxed) mode" in
// natural-language text (commit messages, man pages, docstrings) MUST
// NOT trigger VerdictBlocked. The wedged-pane timeout would otherwise
// fail healthy worker tasks just because a doc-style worker quoted
// such words in its output.
func TestTracker_BlockedDetector_NarrowPatternsAvoidFalsePositive(t *testing.T) {
	t0 := time.Now().UTC()

	notBlocked := []struct {
		name    string
		content string
	}{
		{
			name:    "scrollback_bare_proceed_in_commit_message",
			content: "commit abc123\n    feat: refactor\n    Proceed? after lint review.\n--- end of log ---\n  ✶ Working on next task",
		},
		{
			name:    "scrollback_proceed_in_man_page_quote",
			content: "  -y, --yes      Skip 'Proceed?' confirmation\n      Reads CONFIG\n\n  ✶ Reading file...",
		},
		{
			name:    "scrollback_test_name_with_proceed",
			content: "TestProceed?Cleanup PASSED\nTestNext PASSED\n\n  ✶ Running next suite",
		},
		{
			name:    "scrollback_unsandboxed_in_doc",
			content: "Note: when running (unsandboxed) mode, X happens\nSee README for details.\n\n  ✶ Checking dependencies",
		},
	}
	for _, tc := range notBlocked {
		t.Run(tc.name, func(t *testing.T) {
			tr := New(nil)
			if v := tr.ObserveVerdict("worker1", tc.content, time.Minute, t0); v == VerdictBlocked {
				t.Errorf("%s should NOT match VerdictBlocked, got %s", tc.name, v)
			}
		})
	}
}

// TestTracker_ContextBudgetExhausted_FastFail pins post-2026-05-06 P1
// NEW: when the agent TUI shows >=97% context usage in the tail, the
// pane verdict must be VerdictTerminalError so the queue scanner
// fails the in-flight task immediately and respawns the pane,
// avoiding the 30-min wedge observed in workspace benchmark.
func TestTracker_ContextBudgetExhausted_FastFail(t *testing.T) {
	t0 := time.Now().UTC()

	exhausted := []struct {
		name string
		tail string
	}{
		{name: "97_percent_used", tail: "  ✶ Working...\n  [###############---] 97% used"},
		{name: "99_percent_used", tail: "  Doing things\n  99% used"},
		{name: "100_percent_used", tail: "  Last turn\n  100% used"},
		{name: "98_pct_with_space", tail: "  status: 98 % used"},
	}
	for _, tc := range exhausted {
		t.Run(tc.name, func(t *testing.T) {
			tr := New(nil)
			if v := tr.ObserveVerdict("worker1", tc.tail, time.Minute, t0); v != VerdictTerminalError {
				t.Errorf("%s should be VerdictTerminalError, got %s", tc.name, v)
			}
		})
	}
}

// TestTracker_ContextBudgetExhausted_BelowThreshold pins that context
// usage below 97% does NOT trigger the fast-fail path, so legitimate
// large-output tasks (research / docs) are not aborted prematurely.
func TestTracker_ContextBudgetExhausted_BelowThreshold(t *testing.T) {
	t0 := time.Now().UTC()

	safe := []struct {
		name    string
		content string
	}{
		{name: "85_percent_used", content: "  ✶ Working...\n  85% used"},
		{name: "96_percent_used", content: "  ✶ Compiling\n  96% used"},
		{name: "scrollback_97_in_benchmark", content: "Benchmark: 97% coverage achieved\n--- end of report ---\n  ✶ Running next test"},
		{name: "scrollback_100_pct_in_doc", content: "100% of cases pass.\n  ✶ Reading file"},
	}
	for _, tc := range safe {
		t.Run(tc.name, func(t *testing.T) {
			tr := New(nil)
			if v := tr.ObserveVerdict("worker1", tc.content, time.Minute, t0); v == VerdictTerminalError {
				t.Errorf("%s should NOT trigger VerdictTerminalError, got %s (content=%q)", tc.name, v, tc.content)
			}
		})
	}
}

// TestTracker_CompletionSummaryIdlePaneIsNotActive pins the E2E
// 2026-06-11 regression: a Claude Code pane that finished its turn shows
// a static completion summary ("✻ Cooked for 1m 35s") above the idle
// prompt. The ✻ glyph belongs to defaultActiveHintRegex's spinner set, so
// without stripping completion-summary lines the idle pane matched the
// activity hint on every scan and the lease was extended up to the
// 30-minute hard cap.
func TestTracker_CompletionSummaryIdlePaneIsNotActive(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)
	t2 := t1.Add(60 * time.Second)

	idleFrame := "" +
		"  Unreleased セクションに対応する1行を追記しました。\n" +
		"✻ Cooked for 1m 35s\n" +
		"───────────────\n" +
		"❯\n" +
		"───────────────\n" +
		"  [#----] 5% used · 95% remaining\n" +
		"  ⏵⏵ accept edits on (shift+tab to cycle)\n"

	if v := tr.ObserveVerdict("worker2", idleFrame, time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first observation must be Uncertain (no baseline), got %v", v)
	}
	if v := tr.ObserveVerdict("worker2", idleFrame, time.Minute, t1); v != VerdictIdle {
		t.Fatalf("idle pane with completion summary must be VerdictIdle, got %v", v)
	}
	if v := tr.ObserveVerdict("worker2", idleFrame, time.Minute, t2); v != VerdictIdle {
		t.Fatalf("idle pane with completion summary must stay VerdictIdle, got %v", v)
	}
}

// TestTracker_LiveSpinnerStillActiveAfterCompletionStrip asserts the
// completion-summary strip does not break genuine activity detection: a
// pane whose tail contains BOTH a finished-turn summary and a live
// spinner ("✻ Thinking… (43s · ↑ 682 tokens)" — no "for <duration>"
// suffix) survives the strip and still yields VerdictActive.
func TestTracker_LiveSpinnerStillActiveAfterCompletionStrip(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)

	frame := func(elapsed int) string {
		return "✻ Cooked for 1m 35s\n" +
			"❯ next instruction\n" +
			"✻ Thinking… (" + strconv.Itoa(elapsed) + "s · ↑ 682 tokens)\n"
	}
	_ = tr.ObserveVerdict("worker1", frame(43), time.Minute, t0)
	if v := tr.ObserveVerdict("worker1", frame(103), time.Minute, t1); v != VerdictActive {
		t.Fatalf("live spinner must remain VerdictActive after completion-summary strip, got %v", v)
	}
}

// TestStripCompletionSummary_Patterns documents the exact line shapes the
// strip is expected to remove (finished-turn summaries) and keep (live
// spinners, subprocess markers).
func TestStripCompletionSummary_Patterns(t *testing.T) {
	stripped := []string{
		"✻ Cooked for 1m 35s",
		"✻ Worked for 5m 4s",
		"✻ Sautéed for 40s",
		"✻ Crunched for 26s",
		"✶ Brewed for 12s",
	}
	for _, s := range stripped {
		if got := stripCompletionSummary(s); strings.Contains(got, "for") {
			t.Errorf("expected completion summary %q to be stripped, got %q", s, got)
		}
	}
	kept := []string{
		"✶ Wrangling… (43s · ↑ 682 tokens)",
		"⎿ Waiting…",
		"✻ Thinking…",
	}
	for _, s := range kept {
		if got := stripCompletionSummary(s); got != s {
			t.Errorf("expected live marker %q to survive strip, got %q", s, got)
		}
	}
}

// TestStripCompletionSummary_LiveStatusWithForIsKept pins the codex-review
// finding on the first regex draft: "for <number> <noun>" status lines are
// live activity, not finished-turn summaries, and must survive the strip.
// Only "for <duration-unit>" qualifies as a completion summary.
func TestStripCompletionSummary_LiveStatusWithForIsKept(t *testing.T) {
	kept := []string{
		"✻ Waiting for 2 workers",
		"✻ Searching for 3 files",
		"✻ Polling for 12 results…",
	}
	for _, s := range kept {
		if got := stripCompletionSummary(s); got != s {
			t.Errorf("expected live status %q to survive strip, got %q", s, got)
		}
	}
}
