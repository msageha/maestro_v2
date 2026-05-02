// Package paneactivity tracks per-agent tmux pane content across queue
// scans so the lease-expiry path can decide between "still working" and
// "actually dead" without relying on operator-tuned timeouts.
//
// Liveness is derived from observable pane state:
//
//  1. Each periodic scan, the daemon records a snapshot of every
//     in-progress agent's pane (content hash + busy_pattern match) via
//     RecordObservation.
//  2. When a lease appears to have expired, IsActive compares the
//     current snapshot against the previous one (>= scan_interval old).
//     A change in the hash, OR a busy_pattern still matching, is taken
//     as evidence of liveness and the caller proactively extends the
//     lease.
//  3. The hard cap stays in max_in_progress_min: an agent whose pane is
//     completely silent for the full window is concluded dead and
//     released.
//
// Cross-scan comparison (typically 60 seconds apart) is far less
// false-negative-prone than an in-line activity probe — short "thinking"
// pauses do not collapse the verdict to Idle.
package paneactivity

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sync"
	"time"
)

// Snapshot captures the observable state of an agent's pane at a single
// moment.
//
// ContentHash is a SHA-256 digest of the joined pane capture so it is
// width-independent. TailHash hashes only the last activeTailLines lines —
// this is the one that decides "is the bottom of the pane making forward
// progress?" because background cursor blinks, scrollback churn, and
// occasional repaints elsewhere in the pane all change ContentHash without
// indicating real work. MatchedBusy records whether the busy-pattern
// regex (set via SetBusyPattern) matched in the *tail* — historical
// "Thinking" markers in scrollback used to pin a hung pane to Active.
type Snapshot struct {
	ContentHash string
	TailHash    string
	CapturedAt  time.Time
	MatchedBusy bool
}

// activeTailLines is the number of trailing lines used for forward-progress
// detection (TailHash + busy-pattern match). The agent's bottom-of-pane is
// where genuine progress (next prompt, output streaming, busy spinners)
// shows up; scrollback above is irrelevant for liveness even when the full
// hash differs. 12 lines is generous enough to cover a multi-line prompt
// or progress block while excluding most stale scrollback.
const activeTailLines = 12

// livenessNoiseRegexp folds out classes of pane content that change every
// scan but signal NO forward progress. Used by normalizeForLiveness to
// produce a stable hash for the cross-scan delta check.
//
// Patterns covered:
//
//   - Numeric runs followed by an optional unit token (digits with an
//     optional decimal point, optional whitespace, optional contiguous
//     letters). Every LLM agent UI updates timer / token counters / byte
//     sizes every second while the underlying long-running process
//     produces no real output. The unit suffix is included so a timer
//     "45s" → "1m 5s" rolls into the same normalised marker even when
//     the unit letter changes alongside the digit; without this, the
//     numeric-only normalisation left an "s/m" delta that pinned the
//     verdict to Active. ("✻ Thinking… (5s · ↑ 1234 tokens · 2.3MB)" →
//     "✻ Thinking… (N · ↑ N · N)").
//
//   - Spinner / progress characters in the Unicode ranges Claude /
//     Codex / Gemini UIs typically rotate through:
//
//   - braille patterns U+2800-U+28FF,
//
//   - dingbats / shapes U+2700-U+27BF,
//
//   - geometric shapes U+25A0-U+25FF (covers ◆◇◈◉ etc.),
//
//   - block elements U+2580-U+259F (covers progress-bar glyphs
//     ░▒▓█▌▐▀▄ that flip per render),
//
//   - a handful of explicit spinner glyphs (✻ ✼ ✽ ✾ ✿ ◐ ◓ ◑ ◒
//     ⏳ ⏰ ⌛).
//
//     Without this normalisation a rotating spinner or progress bar
//     forces a new tail hash every scan and pins the verdict to Active
//     even when nothing else changed.
//
// Real forward progress — streaming filenames, log lines, test names —
// still changes the non-noise text, so the normalised hash still moves
// and the verdict resolves to Active correctly.
var livenessNoiseRegexp = regexp.MustCompile(
	// Activity-verb tokens that LLM agents flip between every animation
	// frame ("Thinking…" → "Thinking..." → "Working…" → "Pondering…"
	// → "Crafting…"). Without these the verb churn alone produces a
	// fresh tail hash and the pane is mis-classified as Active even
	// though no real progress has occurred. The list covers the verbs
	// used by Claude Code, Codex, and Gemini UIs as well as research-
	// style LLM front-ends. Match is case-insensitive and consumes the
	// trailing ellipsis / dots that typically animate alongside.
	`(?i)\b(?:Thinking|Working|Running|Processing|Analyzing|Generating|` +
		`Streaming|Loading|Crafting|Compiling|Building|Researching|` +
		`Reviewing|Pondering|Devising|Frobnicating|Synthesizing|` +
		`Simmering|Computing|Considering|Cogitating|Reasoning|` +
		`Hypothesizing|Brainstorming|Planning|Drafting|Reading|Writing|` +
		`Searching|Exploring|Investigating|Evaluating|Reflecting|` +
		`Strategizing|Pondering)\w*[.…]*` +
		// Run of whitespace-separated tokens where each token contains at
		// least one digit. Matches "45s", "1m 5s", "1234 tokens",
		// "12.3k tokens", "Step 5 of 12", "(2m 32s · ↑ 1234 tokens)" — every
		// shape the agent UI emits when only the timer / counter ticks.
		`|(?:\S*\d\S*\s*)+` +
		// Common UI hint phrases that appear/disappear with frame timing
		// but do not represent forward progress. Listed explicitly because
		// they don't contain digits and don't fit the activity-verb list
		// (they're whole phrases, not a single verb).
		`|esc\s+to\s+interrupt` +
		`|tab\s+to\s+(?:cycle|switch|interrupt)` +
		`|ctrl[+-]\w+\s+to\s+\w+` +
		`|[\x{2800}-\x{28FF}]` + // braille spinner range
		`|[\x{2700}-\x{27BF}]` + // dingbats / spinner glyphs
		`|[\x{25A0}-\x{25FF}]` + // geometric shapes (◆◇◈◉◐◑◒◓ et al.)
		`|[\x{2580}-\x{259F}]` + // block elements (progress bars: ░▒▓█▌▐▀▄)
		`|[✻✼✽✾✿⏳⏰⌛]`, // common LLM agent status glyphs
)

// normalizeForLiveness collapses pane noise (numeric runs, spinner glyphs)
// to a single marker so the hash is stable across timer ticks and animation
// frames but still sensitive to genuine progress in the rest of the line.
// The replacement marker is a token chosen to be visible in test failures
// yet unlikely to appear in real pane content.
func normalizeForLiveness(s string) string {
	return livenessNoiseRegexp.ReplaceAllString(s, "N")
}

// tailLines returns the last activeTailLines lines of content joined back
// with newline, preserving any trailing newline so the result is a
// stable string for hashing and regex matching. When content has fewer
// lines than the cap, the entire input is returned.
func tailLines(content string) string {
	if content == "" {
		return ""
	}
	// Walk backwards counting newline boundaries so we don't allocate a
	// full split slice for long pane captures (typical pane is ~24 rows
	// but tmux history-limit can balloon this).
	start := len(content)
	count := 0
	// Skip a single trailing newline so the count corresponds to visible
	// lines rather than the empty-string after the last \n.
	if start > 0 && content[start-1] == '\n' {
		start--
	}
	for start > 0 && count < activeTailLines {
		if content[start-1] == '\n' {
			count++
			if count == activeTailLines {
				break
			}
		}
		start--
	}
	return content[start:]
}

// Tracker holds per-agent pane snapshots and the busy-pattern regex used
// to interpret pane content. Methods are safe for concurrent use.
//
// Memory usage is O(active_agents) — typically 4 workers + planner +
// orchestrator. Snapshots are overwritten in place on each
// RecordObservation, so the map does not grow.
//
// uncertainStreak counts consecutive VerdictUncertain results per agent
// since the last Active/Idle outcome. Callers grace-extend leases on
// VerdictUncertain to give a brand-new agent a chance to establish a
// baseline; if the same agent keeps returning Uncertain (e.g. a stuck
// pane whose content keeps churning just enough that the previous
// snapshot is always within minPrevAge, or a same-scan pathology),
// MaxUncertainStreak caps the grace path so the next call falls
// through to VerdictIdle and the busy-check release path can run.
//
// blockedRegex detects panes that have entered a confirmation prompt
// (typically the underlying CLI agent — Claude Code, etc. — asking
// "Yes/No?" before performing a tool call). When the regex matches,
// ObserveVerdict short-circuits to VerdictIdle regardless of busy or
// hash signals, because a blocked pane is not making progress no matter
// how lively the surrounding scrollback looks. The default pattern is
// hard-coded (no operator-tuned knob) — adding configurability would
// just create another way for the system to drift between agents.
type Tracker struct {
	mu              sync.RWMutex
	snapshots       map[string]Snapshot
	uncertainStreak map[string]int
	busyRegex       *regexp.Regexp
	blockedRegex    *regexp.Regexp
	hashFunc        func(string) string
}

// defaultBlockedRegex is compiled once at package init. It matches the
// classic interactive-prompt shapes that block forward progress in an
// autonomous LLM Orchestration setting:
//
//   - Claude Code numbered choice with arrow cursor ("❯ 1. Yes")
//   - Generic numbered Yes/No selection list ("1. Yes", "2. No" within
//     two consecutive lines, with one of them prefixed by an arrow)
//   - Shell-style "(y/n)" / "[Y/n]" confirmation tails
//
// Patterns intentionally match the *prompt* shape rather than any
// language-specific keyword so the same detector works whether the
// underlying agent is Claude/Codex/Gemini or even a research/doc
// pipeline that calls into a CLI tool.
var defaultBlockedRegex = regexp.MustCompile(
	`(?m)` +
		// Pattern A: arrow cursor on a numbered line.
		`(^[ \t]*❯[ \t]*\d+\.[ \t]+\S)` +
		// Pattern B: a (y/n) / [Y/n] / [yes/no] tail still showing on the
		// last visible line of the pane.
		`|((\(|\[)[Yy](es)?/[Nn](o)?(\)|\])\??\s*$)` +
		// Pattern C: arrow cursor on a non-numbered line followed by an
		// option keyword (Yes/No/Cancel) — covers menus that drop the
		// numbering.
		`|(^[ \t]*❯[ \t]+(Yes|No|Cancel|Allow|Deny)\b)`)

// MaxUncertainStreak is the maximum number of consecutive VerdictUncertain
// outcomes per agent before the next Uncertain is downgraded to
// VerdictIdle. Capping at 1 (a single grace extension) preserves the
// "give a brand-new agent one cycle to establish a baseline" intent
// while preventing a stuck pane (e.g. Claude API content-filter error
// returning to prompt) from being grace-extended indefinitely.
const MaxUncertainStreak = 1

// New creates a Tracker pre-loaded with the given busy-pattern regex.
// A nil regex is acceptable — IsActive and RecordObservation simply
// skip the pattern-matched branch in that case. The blocked-prompt
// detector is wired with the package default; tests that need to
// disable or replace it can call SetBlockedPattern.
func New(busyRegex *regexp.Regexp) *Tracker {
	return &Tracker{
		snapshots:       make(map[string]Snapshot),
		uncertainStreak: make(map[string]int),
		busyRegex:       busyRegex,
		blockedRegex:    defaultBlockedRegex,
		hashFunc:        defaultHash,
	}
}

// SetBlockedPattern replaces the regex used to detect blocked
// confirmation prompts. Pass nil to disable the override entirely (then
// only busy/hash signals drive the verdict). Mostly useful for tests.
func (t *Tracker) SetBlockedPattern(re *regexp.Regexp) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.blockedRegex = re
}

// SetBusyPattern updates the regex used to flag in-progress activity
// markers (e.g., "Working/Thinking/Sending"). Pass nil to disable
// pattern-based liveness signalling — IsActive then relies solely on
// hash deltas across scans.
func (t *Tracker) SetBusyPattern(re *regexp.Regexp) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.busyRegex = re
}

// RecordObservation stores a fresh snapshot for agentID. content is the
// raw pane capture (from tmux capture-pane -pJ); the tracker hashes it
// internally so callers do not have to reach into hashing details.
//
// Both the full-content hash and a tail-only hash are stored. Liveness
// decisions in ObserveVerdict prefer the tail hash so a hung process
// whose bottom-of-pane output is frozen reads as Idle even if scrollback
// elsewhere in the pane has changed. The tail hash is computed over the
// numeric-normalised tail so a ticking timer (e.g., the LLM agent UI
// counting elapsed seconds while a child process is hung) does not by
// itself qualify as forward progress.
//
// Returns the snapshot it just stored — useful when the caller wants to
// inspect MatchedBusy without re-hashing.
func (t *Tracker) RecordObservation(agentID, content string, now time.Time) Snapshot {
	tail := tailLines(content)
	normalisedTail := normalizeForLiveness(tail)
	matched := false
	t.mu.RLock()
	if t.busyRegex != nil {
		// Busy pattern matches against the original tail (not the
		// normalised one) so patterns that legitimately reference digits
		// — though uncommon — keep working. The numeric normalisation
		// only governs the *cross-scan delta* hash.
		matched = t.busyRegex.MatchString(tail)
	}
	t.mu.RUnlock()

	snap := Snapshot{
		ContentHash: t.hashFunc(content),
		TailHash:    t.hashFunc(normalisedTail),
		CapturedAt:  now,
		MatchedBusy: matched,
	}
	t.mu.Lock()
	t.snapshots[agentID] = snap
	t.mu.Unlock()
	return snap
}

// IsActiveAfter reports whether the agent shows signs of liveness when
// the latest captured content is compared against the previously
// recorded snapshot.
//
// Activity is declared when ANY of:
//   - the busy-pattern regex matches the *tail* of the latest content
//     (so historical scrollback like an old "Thinking" line cannot pin a
//     stalled pane to Active),
//   - the tail-hash differs from the previous snapshot's tail-hash AND
//     the previous snapshot is at least minPrevAge old (a real cross-scan
//     forward-progress change, not a same-scan duplicate or scrollback
//     repaint).
//
// Callers MUST follow with RecordObservation to advance the rolling
// snapshot, which is why the canonical entry point Observe combines both
// steps. IsActiveAfter is exported separately for tests and for callers
// that want to inspect the verdict without rotating the cache.
func (t *Tracker) IsActiveAfter(agentID, content string, minPrevAge time.Duration, now time.Time) bool {
	t.mu.RLock()
	prev, hasPrev := t.snapshots[agentID]
	re := t.busyRegex
	t.mu.RUnlock()

	tail := tailLines(content)
	if re != nil && re.MatchString(tail) {
		return true
	}
	if !hasPrev {
		return false
	}
	if now.Sub(prev.CapturedAt) < minPrevAge {
		return false
	}
	return prev.TailHash != t.hashFunc(normalizeForLiveness(tail))
}

// Observe is the canonical entry point: it computes the activity verdict
// for content (relative to the previously stored snapshot) AND stores
// the new observation atomically so the next call sees today's snapshot
// as "previous". Callers should pass minPrevAge ≈ scan_interval so a
// single-scan window between observations is not interpreted as
// inactivity.
func (t *Tracker) Observe(agentID, content string, minPrevAge time.Duration, now time.Time) bool {
	active := t.IsActiveAfter(agentID, content, minPrevAge, now)
	_ = t.RecordObservation(agentID, content, now)
	return active
}

// Verdict is the trichotomous result of ObserveVerdict — it lets the
// caller distinguish "definitely active" from "no baseline yet, can't
// say" so the lease-expiry path can grace-extend rather than treating
// the second case as "definitely idle".
type Verdict int

const (
	// VerdictUnknown signals that the tracker cannot reason about this
	// agent at all (e.g., a nil tracker has been passed). Callers
	// should fall through to whatever conservative path they would have
	// run before pane-activity was introduced.
	VerdictUnknown Verdict = iota

	// VerdictActive — the busy-pattern matched, OR the captured content
	// hash differs from the previously recorded snapshot AND that
	// snapshot is at least minPrevAge old. Liveness is established.
	VerdictActive

	// VerdictIdle — there IS a previous snapshot at least minPrevAge
	// old AND the content hash has not changed AND the busy-pattern
	// did not match. The pane is observably static; the caller may
	// release the lease.
	VerdictIdle

	// VerdictUncertain — there is no usable previous snapshot to
	// compare against (first observation, or the previous snapshot is
	// too recent to be a meaningful "before" datapoint). Callers must
	// NOT release the lease on this verdict — there is genuinely not
	// enough information yet. Recommended response: extend the lease
	// once so the next scan has a baseline to compare against.
	VerdictUncertain
)

// String returns a stable token suitable for log lines.
func (v Verdict) String() string {
	switch v {
	case VerdictActive:
		return "active"
	case VerdictIdle:
		return "idle"
	case VerdictUncertain:
		return "uncertain"
	default:
		return "unknown"
	}
}

// ObserveVerdict combines IsActiveAfter's branching with RecordObservation
// so the caller atomically (a) gets a three-way liveness verdict and
// (b) advances the rolling baseline for the next call.
//
// The trichotomy matters for the lease-expiry path: when the tracker has
// never recorded a snapshot for an agent (daemon just started, agent newly
// admitted, etc.), VerdictUncertain lets the caller distinguish "no
// baseline yet" from "baseline exists and shows no change" so the
// response can be a one-cycle grace extension instead of a destructive
// release.
func (t *Tracker) ObserveVerdict(agentID, content string, minPrevAge time.Duration, now time.Time) Verdict {
	t.mu.RLock()
	prev, hasPrev := t.snapshots[agentID]
	re := t.busyRegex
	blockedRe := t.blockedRegex
	streak := t.uncertainStreak[agentID]
	t.mu.RUnlock()

	// Blocked-prompt fast path: if the pane shows an interactive
	// confirmation prompt (e.g. Claude Code's "❯ 1. Yes" tool-call
	// confirmation), the agent is not making progress regardless of how
	// lively the surrounding scrollback looks. Force VerdictIdle so the
	// caller falls through to the busy-check / release path; the lease
	// expires, the task is force-released, and the recovery routines
	// (paused_for_replan / R10 dead-letter) can take it from there
	// without an operator manually clearing the prompt.
	//
	// This must run BEFORE the busy-pattern fast path because a
	// scrollback line containing "Working" or "Thinking" can otherwise
	// pin the verdict to Active while the new bottom-of-pane prompt
	// blocks forward progress.
	if blockedRe != nil && blockedRe.MatchString(content) {
		t.mu.Lock()
		delete(t.uncertainStreak, agentID)
		t.mu.Unlock()
		_ = t.RecordObservation(agentID, content, now)
		return VerdictIdle
	}

	tail := tailLines(content)
	tailHash := t.hashFunc(normalizeForLiveness(tail))
	var verdict Verdict
	// uncertainCounted is true when this Uncertain outcome should count
	// toward the consecutive-uncertain streak. We only count the
	// no-baseline path: same-scan duplicates (minPrevAge protection)
	// are independent timing noise and should not erode the streak cap
	// the way a "still no baseline despite repeat calls" pattern does.
	uncertainCounted := false
	switch {
	case re != nil && re.MatchString(tail):
		verdict = VerdictActive
	case !hasPrev:
		verdict = VerdictUncertain
		uncertainCounted = true
	case now.Sub(prev.CapturedAt) < minPrevAge:
		// Same-scan re-capture: not enough wall time has passed to
		// claim "things are not moving". Treat as uncertain so the
		// caller does not destructively release on noise.
		verdict = VerdictUncertain
	case prev.TailHash != tailHash:
		// The tail of the pane changed across scans — genuine forward
		// progress (output streaming, prompt updating, etc.). Scrollback
		// churn alone cannot satisfy this branch because the tail hash
		// only covers the bottom activeTailLines.
		verdict = VerdictActive
	default:
		verdict = VerdictIdle
	}

	// Cap consecutive no-baseline Uncertain results so a stuck pane
	// cannot be grace-extended indefinitely. Once an agent has accrued
	// MaxUncertainStreak no-baseline Uncertain results in a row, the
	// next no-baseline Uncertain is downgraded to VerdictIdle so the
	// caller falls through to the busy-check probe (which can release
	// the lease).
	if uncertainCounted && streak >= MaxUncertainStreak {
		verdict = VerdictIdle
		uncertainCounted = false
	}

	t.mu.Lock()
	if uncertainCounted {
		t.uncertainStreak[agentID] = streak + 1
	} else if verdict != VerdictUncertain {
		delete(t.uncertainStreak, agentID)
	}
	t.mu.Unlock()

	_ = t.RecordObservation(agentID, content, now)
	return verdict
}

// ForgetAgent drops the cached snapshot for agentID — used when an
// agent's pane has been intentionally evicted (e.g., during cleanup) so
// the next observation is treated as a fresh baseline rather than
// compared against stale content.
func (t *Tracker) ForgetAgent(agentID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.snapshots, agentID)
	delete(t.uncertainStreak, agentID)
}

// LastSnapshot returns the most recently recorded snapshot for agentID.
// The boolean is false when no snapshot has been recorded yet. Useful
// for diagnostics and test introspection.
func (t *Tracker) LastSnapshot(agentID string) (Snapshot, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	snap, ok := t.snapshots[agentID]
	return snap, ok
}

func defaultHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
