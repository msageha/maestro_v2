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
func TestTracker_NumericTickerInTailIsIdle(t *testing.T) {
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

	if v := tr.ObserveVerdict("worker1", frame(5), time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first capture should be Uncertain, got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame(65), time.Minute, t1); v != VerdictIdle {
		t.Fatalf("ticking timer alone must NOT pin verdict to Active; got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame(125), time.Minute, t2); v != VerdictIdle {
		t.Fatalf("repeat ticker must remain VerdictIdle; got %v", v)
	}
}

// TestTracker_SpinnerOnlyChangeIsIdle: spinner glyph rotation
// (✻ → ⠋ → ⠙ → ...) plus a ticking counter is pure UI animation, not
// real progress. Both numeric and spinner noise must be collapsed
// before hashing so the verdict resolves to Idle.
func TestTracker_SpinnerOnlyChangeIsIdle(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)
	t2 := t1.Add(60 * time.Second)

	// Build a frame with a rotating spinner + ticking counter; nothing
	// else changes. The bottom of the pane is the agent UI status line
	// the user reproduced in the e2e regression.
	frame := func(spinner rune, elapsed int) string {
		out := ""
		// Pad scrollback so the relevant tail dominates.
		for i := 0; i < 30; i++ {
			out += "scrollback-" + string(rune('a'+i%26)) + "\n"
		}
		out += "$ sh -c 'sleep 600'\n"
		out += string(spinner) + " Running… (Ns · ↑ N tokens · esc to interrupt)\n"
		out = replaceN(out, elapsed)
		return out
	}

	if v := tr.ObserveVerdict("worker1", frame('✻', 5), time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first capture should be Uncertain, got %v", v)
	}
	// Spinner advances and timer ticks — pure UI animation, no real
	// progress. Must NOT be Active.
	if v := tr.ObserveVerdict("worker1", frame('⠋', 65), time.Minute, t1); v != VerdictIdle {
		t.Fatalf("spinner+timer-only change must NOT pin verdict to Active; got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame('⠙', 125), time.Minute, t2); v != VerdictIdle {
		t.Fatalf("repeated spinner+timer change must remain VerdictIdle; got %v", v)
	}
}

// TestTracker_TimerUnitFlipIsIdle: a timer counter rolling from "45s" to
// "1m 5s" between scans must collapse to the same hash. Pure numeric
// normalisation would leave the trailing unit letters "s" -> "m 5s"
// differing; unit-aware normalisation collapses the digit-and-unit pair
// so a unit roll-over produces a stable hash.
func TestTracker_TimerUnitFlipIsIdle(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)
	t2 := t1.Add(60 * time.Second)

	frame := func(timer string) string {
		out := ""
		for i := 0; i < 30; i++ {
			out += "scrollback line\n"
		}
		out += "$ sh -c 'sleep 600'\n"
		out += "  ✻ Running… (" + timer + " · esc to interrupt)\n"
		return out
	}

	if v := tr.ObserveVerdict("worker1", frame("45s"), time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first capture should be Uncertain, got %v", v)
	}
	// Timer rolls "45s" → "1m 5s": digit AND unit changed.
	if v := tr.ObserveVerdict("worker1", frame("1m 5s"), time.Minute, t1); v != VerdictIdle {
		t.Fatalf("digit+unit-flip in timer must NOT pin verdict to Active; got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame("2m 5s"), time.Minute, t2); v != VerdictIdle {
		t.Fatalf("repeated digit+unit-flip must remain VerdictIdle; got %v", v)
	}
}

// TestTracker_TokenCountFlipIsIdle: tokens counter "1234 tokens" → "12.3k
// tokens" is a unit roll-over that pure numeric normalisation would still
// leave with a residual hash delta because the inserted "k" is alphabetic.
func TestTracker_TokenCountFlipIsIdle(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()
	t1 := t0.Add(60 * time.Second)

	frame := func(tokens string) string {
		out := ""
		for i := 0; i < 30; i++ {
			out += "scrollback line\n"
		}
		out += "  ✻ Running… (5s · ↑ " + tokens + " · esc to interrupt)\n"
		return out
	}

	if v := tr.ObserveVerdict("worker1", frame("1234 tokens"), time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first capture should be Uncertain, got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame("12.3k tokens"), time.Minute, t1); v != VerdictIdle {
		t.Fatalf("token unit roll-over must NOT pin verdict to Active; got %v", v)
	}
}

// TestTracker_ActivityVerbAnimationIsIdle: a hung worker pane whose
// Claude Code UI rotates the activity verb between scans ("Thinking…" ->
// "Pondering…" -> "Crafting…") would otherwise produce a fresh tail hash
// every observation. The verb churn must be collapsed at the
// normalisation layer; no busy_pattern alone can distinguish a real
// agent doing real work (which also matches the verbs) from a stuck
// one whose only motion is the cosmetic verb spinner.
func TestTracker_ActivityVerbAnimationIsIdle(t *testing.T) {
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

	if v := tr.ObserveVerdict("worker1", frame("Thinking", 5), time.Minute, t0); v != VerdictUncertain {
		t.Fatalf("first capture should be Uncertain, got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame("Pondering", 65), time.Minute, t1); v != VerdictIdle {
		t.Fatalf("verb animation must NOT pin verdict to Active; got %v", v)
	}
	if v := tr.ObserveVerdict("worker1", frame("Crafting", 125), time.Minute, t2); v != VerdictIdle {
		t.Fatalf("repeated verb animation must remain VerdictIdle; got %v", v)
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

// TestTracker_ObserveVerdict_BlockedPromptForcesIdle: when the captured
// pane content contains a confirmation-prompt shape (the agent CLI is
// blocked asking the operator to choose Yes/No), ObserveVerdict
// overrides to VerdictIdle so the lease can drain and the busy-check
// release path can pick up. Critical because the busy-pattern match
// would otherwise pin the verdict to Active forever, leaving the
// worker stuck on the prompt while the daemon perceives liveness from
// leftover scrollback.
func TestTracker_ObserveVerdict_BlockedPromptForcesIdle(t *testing.T) {
	tr := New(regexp.MustCompile(`Working|Thinking`))
	t0 := time.Now().UTC()

	// Real Claude Code blocked-on-confirmation pane snapshot — note the
	// "Thinking" word in scrollback that would otherwise win the
	// busy-pattern fast path.
	content := "✻ Thinking… (some scrollback)\n" +
		"Do you want to create verify.sh?\n" +
		"❯ 1. Yes\n" +
		"  2. Yes, and allow Claude to edit its own settings for this session\n" +
		"  3. No\n"

	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictIdle {
		t.Fatalf("blocked confirmation prompt must yield VerdictIdle (force release), got %s", v)
	}
}

// TestTracker_ObserveVerdict_BlockedShellPromptForcesIdle exercises the
// (y/n) tail variant of the blocked detector so the same recovery path
// works for shell-style confirmations issued by language-agnostic CLIs
// (npm, brew, apt, etc.).
func TestTracker_ObserveVerdict_BlockedShellPromptForcesIdle(t *testing.T) {
	tr := New(nil)
	t0 := time.Now().UTC()

	content := "Some output\n...\nProceed? (Y/n)"
	if v := tr.ObserveVerdict("worker1", content, time.Minute, t0); v != VerdictIdle {
		t.Fatalf("shell (Y/n) prompt must yield VerdictIdle, got %s", v)
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
