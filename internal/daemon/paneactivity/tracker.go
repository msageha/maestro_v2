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
	"os"
	"regexp"
	"strings"
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
	// RateLinesHash covers only the tail lines that carry a transfer /
	// progress rate marker (MB/s, it/s, ETA …), hashed WITHOUT numeric
	// normalisation. Tools that report progress purely through numbers
	// (bare wget/dd/tqdm driven directly in the pane) keep a stable
	// normalised TailHash — their only cross-scan delta is numeric — so
	// this hash is the dedicated weak signal for them (W-P1). Empty when
	// the tail has no rate-marker lines.
	RateLinesHash string
	CapturedAt    time.Time
	MatchedBusy   bool
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
	// blockedSince records, per agent, the wall-clock timestamp of the FIRST
	// scan in the current consecutive run of VerdictBlocked observations.
	// Cleared whenever the agent is observed in any non-blocked state. The
	// queue scanner consults this to enforce a short blocked-prompt timeout
	// (orders of magnitude tighter than circuit_breaker.progress_timeout):
	// if a pane has been wedged on a confirmation prompt for longer than
	// the threshold, the in-flight task is failed so Planner / Orchestrator
	// can route around it instead of waiting 30 min for the back-stop.
	blockedSince map[string]time.Time
	// blockedClass records the current blocked prompt subclass for each
	// agent. It is updated on every blocked observation while blockedSince
	// remains stamped to the first observation in the current run.
	blockedClass map[string]string
	// hintLogState throttles the per-agent
	// `pane_blocked_prompt_hint_unmatched` DEBUG log. The hint regex is
	// deliberately broad (anything that looks like a prompt marker —
	// `❯`, `Yes/No`, `Do you want`, `Proceed?`, etc.) and ordinary Claude
	// pane output routinely contains those markers without being a
	// genuine block. Logging unconditionally on every scan tick produced
	// hundreds of identical lines per minute (Report 2026-05-06 issue-4).
	// We log at most once per agent per (tail-hash, blockedHintLogWindow)
	// window so the signal stays usable for hang triage.
	hintLogState map[string]hintLogEntry
	busyRegex    *regexp.Regexp
	blockedRegex *regexp.Regexp
	hashFunc     func(string) string
}

// hintLogEntry records the most recent `pane_blocked_prompt_hint_unmatched`
// emission for one agent. Used to suppress duplicate DEBUG noise.
type hintLogEntry struct {
	at        time.Time
	hintClass string
}

// blockedHintLogWindow is the per-(agent, hint-class) throttle window
// for repeating the `pane_blocked_prompt_hint_unmatched` DEBUG line.
// Genuine wedges are caught by the strict `pane_blocked_prompt_detected`
// path, which is independent of this throttle, so this DEBUG hint only
// needs to surface "scrollback contains a marker the strict detector
// missed" once per investigation window. 30s was too tight — 30 min
// runs produced 11–16 emissions per worker (Report 2026-05-06 P1) —
// because the scrollback marker stays visible across many scan ticks
// and only the first emission is informative. 5 minutes keeps a
// genuine wedge surfacing fresh evidence within an investigation
// session while dropping the steady-state noise.
const blockedHintLogWindow = 5 * time.Minute

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
//
// Pattern A is intentionally loose around the digit-and-dot core:
// claude-code historically renders the choice line as `❯ 1. Yes` but
// formatting drift (`❯1. Yes`, `❯ 1.Yes`, leading non-tab whitespace
// from a wrapped continuation marker, etc.) was observed in field
// reports. The `[ \t]*` slots accept the variation; the trailing
// non-whitespace requirement still keeps "❯ 1. " alone (a half-rendered
// frame) from triggering. Pattern D (broad Bash/tool approval banners)
// covers the case where the runtime renders an "approval required"
// dialog with a literal `Do you want` line — Reports of 2026-05-04
// pinned a real reproduction where the structured arrow-cursor line
// did not appear in the daemon's pane capture but the textual
// "Do you want to ..." line did.
// boxOrSpace matches optional leading whitespace plus optional
// box-drawing characters (U+2500-U+257F) at the start of a line.
// Claude Code 2.x renders approval prompts wrapped in a Unicode box —
// "│ Do you want to proceed?" — so any pattern that anchored on bare
// `^[ \t]*` missed the prompt entirely (Reports of 2026-05-05).
const boxOrSpace = `[ \t\x{2500}-\x{257F}│┃║▌▍▎▏▐▕]*`

var defaultBlockedRegex = regexp.MustCompile(
	`(?m)` +
		// Pattern A: arrow cursor on a numbered line. Allow box-drawing
		// prefix so a `│ ❯ 1. Yes` line inside a Claude Code approval
		// box still matches.
		`(^` + boxOrSpace + `❯[ \t]*\d+\.[ \t]*\S)` +
		// Pattern B: a (y/n) / [Y/n] / [yes/no] tail still showing on the
		// last visible line of the pane.
		`|((\(|\[)[Yy](es)?/[Nn](o)?(\)|\])\??\s*$)` +
		// Pattern C: arrow cursor on a non-numbered line followed by an
		// option keyword (Yes/No/Cancel/Allow/Deny) — covers menus that drop
		// the numbering. Box-drawing prefix allowed (see Pattern A).
		`|(^` + boxOrSpace + `❯[ \t]+(Yes|No|Cancel|Allow|Deny)\b)` +
		// Pattern D: textual approval banner used by claude-code's Bash
		// tool. NEITHER end is anchored:
		//   - daemon's tmux capture may include trailing render artifacts
		//     on the same row;
		//   - claude-code 2.x wraps the prompt in a Unicode box so the
		//     leading char is `│ ` not whitespace.
		// Just look for the literal phrase anywhere on a line.
		`|(Do you want to )` +
		// Pattern E: literal "Bash command" approval banner that
		// claude-code emits before the choice menu. Same loose anchoring
		// as Pattern D.
		`|(Bash command\b)` +
		// Pattern F: Claude Code "1. Yes" / "2. No" choice list line.
		// Captured even without the cursor character present — useful
		// when the cursor line scrolled past the visible region but
		// the choice list itself is still in the captured frame.
		`|(^` + boxOrSpace + `\d+\.[ \t]+(Yes|No|Cancel|Allow|Deny)\b)` +
		// Pattern G: Codex / Gemini / generic CLI approval banners
		// that use phrasing like "Approve this command?",
		// "Allow this action?", "Confirm:". Box-drawing prefix allowed.
		`|(^` + boxOrSpace + `(Approve|Allow|Confirm)\b.*\?)` +
		// Pattern H: file-edit confirmation banner from claude-code 2.x.
		// The runtime asks "Do you want to make this edit to <file>?"
		// before modifying runtime-protected paths even with
		// --dangerously-skip-permissions.
		`|(make this edit to )`)

// defaultBlockedTailRegex matches narrow prompt markers that are evaluated
// against the pane tail (~5 lines) only, never the full capture. Short
// fragments like `Proceed?` or `unsandboxed)` can appear inside worker output
// (man pages, commit messages, docstrings, quotations); restricting them to
// the tail removes the scrollback false-positive while still catching the
// real prompt, which always lives on the bottom row (Report 2026-05-06 issue-3).
// defaultBlockedRegex's patterns A-H stay full-capture because Unicode boxes
// and mid-scroll prompts can land outside the tail; I/J belong here.
var defaultBlockedTailRegex = regexp.MustCompile(
	`(?m)` +
		// Pattern I (tail-only): bare "Proceed?" prompt with Y/N suffix.
		// Requiring the suffix avoids matching natural prose like "Should
		// we proceed?" or commit messages.
		`(Proceed\?\s*[(\[]?[YyNn])` +
		// Pattern J (tail-only): `Bash command (unsandboxed)` banner. The
		// "Bash command " prefix is required so prose containing
		// "(unsandboxed)" alone does not match.
		`|(Bash command \(unsandboxed\))`)

// unrecoverableBlockedRegex matches prompt literals that cannot be cleared
// by auto-approval when bypassPermissions is disabled by managed policy:
// protected-path edit confirmations.
// The queue scanner uses this subclass to fail on a shorter timeout so the
// daemon can repair/replan instead of waiting on operator-only prompts.
// It is evaluated against the pane tail only (Fix E): scrollback can
// legitimately contain "make this edit to" from an already-cleared prompt
// (or quoted task text), and classifying on stale scrollback would fail a
// healthy in-flight task on the shortened unrecoverable timeout.
var unrecoverableBlockedRegex = regexp.MustCompile(`(?im)make this edit to ?`)

// unrecoverableUnsandboxedBlockedRegex matches the unsandboxed Bash approval
// banner. It is evaluated against the pane tail only because old scrollback can
// legitimately mention this phrase after the prompt has cleared.
var unrecoverableUnsandboxedBlockedRegex = regexp.MustCompile(`(?im)Bash command \(unsandboxed\)`)

// blockedHintRegex matches surface symptoms of an interactive prompt
// even when the strict patterns above did not fire. Used purely for a
// debug log so operators can see WHY a pane was not detected as blocked.
var blockedHintRegex = regexp.MustCompile(`(?m)❯|\bYes\b/\bNo\b|Proceed\?|Do you want|approval required|Confirm\?`)

// terminalErrorRegex matches non-recoverable error frames produced by
// the agent runtime (Claude API, Codex, Gemini). Detecting these in the
// pane lets the queue scanner fail the in-flight task immediately
// instead of waiting for max_in_progress_min (~30 min) — the LLM cannot
// self-recover from a content-policy rejection or a 4xx error frame, so
// extending the lease is wasted wall-clock and ends in the same task
// being re-dispatched onto the still-stale TUI (Report 2026-05-06 P0-2).
//
// The list is intentionally narrow: only error shapes that are
// definitively terminal at the runtime level. Transient network errors
// (ECONNRESET, 502, 503) are NOT matched here because the runtime
// usually retries them internally; matching them would convert a
// transient hiccup into a hard task failure.
var terminalErrorRegex = regexp.MustCompile(
	`(?i)` +
		// Claude API HTTP error envelope. The runtime renders this as a
		// boxed "API Error: <code> <body>" frame on the TUI when the
		// request fails irrecoverably (content filter, invalid request,
		// model-not-found, organization disabled, etc.).
		`API Error: 4\d\d\b` +
		// Anthropic / OpenAI structured error type for malformed or
		// policy-violating requests. Surfaces directly in some runtime
		// error envelopes when streaming JSON is rendered to the pane.
		`|invalid_request_error\b` +
		// Anthropic content-filter rejection. The pane shows the policy
		// reason verbatim; this is the canonical terminal-error string
		// from Report 2026-05-06 P0-2.
		`|Output blocked by content filtering policy` +
		// Anthropic permission_error: organization or API key restricted
		// from the requested action. Always terminal.
		`|permission_error\b` +
		// authentication_error: stale or revoked credentials. Operator
		// must rotate the key — definitively terminal.
		`|authentication_error\b` +
		// Anthropic safety-classifier rejection: "<model>'s safeguards
		// flagged this message for a <topic> topic. ... apply for an
		// exemption: https://claude.com/form/cyber-use-case". Observed
		// in production blocking both Planner and Worker panes on a
		// CyberGym benchmark task (cybersecurity-flagged content) — the
		// classifier verdict is deterministic on the same content, so
		// this is exactly as terminal as the 4xx / content-filter cases
		// above: extending the lease only re-renders the same banner.
		`|safeguards flagged this message`)

// contextBudgetExhaustedRegex detects the Claude Code TUI context-usage
// indicator (`97% used`, `99% used`, `100% used`). At >=97% the next turn
// gets truncated / auto-cleared by the runtime and the in-flight task
// silently disappears (Report 2026-05-06 P1 NEW: 97% → 63% reset →
// dispatch_task_failed). Detection short-circuits the 30 min
// max_in_progress_min back-stop with a VerdictTerminalError-equivalent
// fast-fail (task drop + pane respawn → retry on a different worker / fresh
// epoch). The 97% threshold is deliberately conservative: research / heavy
// output tasks legitimately reach the low 90s, while measured runs show
// reset begins at 97%. Evaluated against the tail only — the indicator
// always renders on the bottom row, and full-capture matching would pick
// up older indicator lines from scrollback.
var contextBudgetExhaustedRegex = regexp.MustCompile(
	`\b(9[7-9]|100)\s*%\s+used\b`,
)

// defaultActiveHintRegex marks a pane Active when the tail shows an LLM
// agent's activity / spinner UI, even if the cross-scan content hash is
// identical (e.g. a long-running `pnpm install` keeps the same "✶ Checking…"
// line for >60s; the hash-delta heuristic would otherwise mis-classify Idle
// and race in a fresh dispatch — Report 2026-05-05 P0-1). Kept as a
// daemon-side default (not a config knob) so operators picking up a new
// binary inherit new runtime verbs (claude-code adds them every release)
// without editing config.yaml. Deliberately broad: false-positive Active is
// bounded by max_in_progress_min, while false-negative Idle on a busy agent
// destructively releases the lease.
var defaultActiveHintRegex = regexp.MustCompile(
	`(?i)` +
		// English activity verbs Claude Code / Codex / Gemini / generic
		// LLM CLIs use as spinner labels. Trailing word characters
		// permit "-ing", "ed", or other tense suffixes; the trailing
		// ellipsis / dots are common but optional so the verb alone
		// (no animation char) is still caught when the spinner glyph
		// scrolled out of the captured tail.
		`\b(?:Thinking|Working|Running|Processing|Analyzing|Generating|` +
		`Streaming|Loading|Crafting|Compiling|Building|Researching|` +
		`Reviewing|Pondering|Devising|Frobnicating|Synthesizing|` +
		`Simmering|Computing|Considering|Cogitating|Reasoning|` +
		`Hypothesizing|Brainstorming|Planning|Drafting|Reading|Writing|` +
		`Searching|Exploring|Investigating|Evaluating|Reflecting|` +
		`Strategizing|Cooking|Brewing|` +
		`Determining|Waiting|Fetching|Downloading|` +
		`Uploading|Installing|Resolving|Linking|Unpacking|Indexing|` +
		`Updating|Caching|Verifying|Validating|Testing|Checking)\w*` +
		// Optional trailing ellipsis / dots. Not required because some
		// runtimes show the verb without animation chars when the
		// surrounding spinner glyph scrolled away.
		`[.…]*` +
		// Japanese activity verbs (`〜中` = "in progress"). Same
		// rationale: each LLM Japanese-locale UI variant uses these
		// markers. Trailing pipe MUST stay on the same physical alternation
		// boundary — a stray `|` between alternations (e.g. `…調査中|` then
		// `|⎿\s` on the next concatenated string) collapses into `||` and
		// the empty alternative matches every input. Keep one continuous
		// alternation by removing trailing `|` from each chunk except the
		// last so the splice point is always non-empty.
		`|検証中|分析中|生成中|処理中|確認中|実行中|読込中|読み込み中|書込中|書き込み中` +
		`|計画中|思考中|編集中|作成中|更新中|削除中|展開中|構築中|準備中|送信中` +
		`|受信中|待機中|解析中|計算中|整理中|圧縮中|解凍中|検索中|調査中` +
		// Claude Code 2.x subprocess output marker — a curved arrow
		// preceding stderr/stdout lines from the underlying tool. Its
		// presence on the tail is unambiguous evidence the agent is
		// rendering live tool output, regardless of whether the line
		// content has actually changed since the previous scan.
		`|⎿\s` +
		// Common LLM agent spinner / status glyphs (subset of
		// livenessNoiseRegexp's normalisation set). Their presence in
		// the tail signals an active animation.
		`|[✻✼✽✾✿⏳⏰⌛◐◑◒◓◴◵◶◷⠁⠂⠃⠄⠅⠆⠇⠈⠉⠊⠋⠌⠍⠎⠏]`)

// progressRateLineRegex matches tail lines that carry a transfer / progress
// rate marker: byte-rate units (KB/s, MiB/s, …), iteration rates (it/s,
// items/s, tok/s), or an ETA field. Numeric-only progress tools (bare
// wget/dd/tqdm run directly in the pane, W-P1) update ONLY these numbers,
// which the liveness normalisation deliberately collapses — so their tail
// hash freezes and the pane reads Idle even though the tool is mid-transfer.
//
// The signal derived from these lines is a CROSS-SCAN DELTA of their raw
// (un-normalised) text, never a static match — see the rate-delta branch in
// ObserveVerdict. That shape is what keeps both known failure modes closed:
//   - a hung agent TUI's timer/counter churn (the 2026-05-06 P0) has no rate
//     units, so the signal stays inert regardless of how much the tail moves;
//   - a COMPLETED tool's final progress line ("… 5.2 MB/s" frozen in the
//     tail, dd's "bytes transferred in … (1.0 MB/s)" summary) no longer
//     changes, so the delta is zero and the pane correctly reads Idle. A
//     static activeHint-style match on the same units would pin such panes
//     Active forever, which is why W-P1 was originally deferred.
var progressRateLineRegex = regexp.MustCompile(
	`(?im)^.*(?:` +
		// Byte / bit rates: 5.2MB/s, 981 KiB/s, 12 Mb/s, 4kB/s.
		`\d[\d.,]*\s*[KMGTP]?i?[Bb]/s\b` +
		// Iteration rates: 12.4it/s, 3 items/s, 45 tok/s, 8 tokens/s.
		`|\d[\d.,]*\s*(?:it|items?|toks?|tokens?)/s\b` +
		// ETA fields: "ETA 00:12", "eta 3m2s", "ETA: 5s".
		`|\bETA:?\s+\S` +
		`).*$`,
)

// rateLines extracts the tail lines matched by progressRateLineRegex joined
// with newline, preserving their raw numeric content for delta hashing.
func rateLines(tail string) string {
	return strings.Join(progressRateLineRegex.FindAllString(tail, -1), "\n")
}

// completionSummaryLineRegex matches the static completion-summary line
// Claude Code renders after a turn finishes — "✻ Cooked for 1m 35s",
// "✻ Worked for 5m 4s", "✻ Sautéed for 40s" — which then stays on screen
// while the pane sits idle at the prompt. The glyph set overlaps
// defaultActiveHintRegex's spinner glyphs, so without stripping these
// lines first an idle pane showing a completion summary matches the
// activity hint forever and is lease-extended up to the 30-minute hard
// cap (E2E 2026-06-11: "lease_extend_pane_active … pane shows cross-scan
// activity" on a pane that had been idle for half an hour).
//
// Anchored to whole lines and to real duration units ("for 1m 35s", not
// "for 2 workers") so live status lines like "✻ Waiting for 2 workers"
// or a spinner "✶ Wrangling… (43s · ↑ 757 tokens)" are never stripped.
// [ \t] is used instead of \s so the match cannot cross newlines.
var completionSummaryLineRegex = regexp.MustCompile(
	`(?m)^[ \t]*[✻✶✼✽✾✿⏺][ \t]*\p{L}+(?:…|\.{3})?[ \t]+for[ \t]+` +
		`\d+(?:\.\d+)?(?:ms|s|m|h)(?:[ \t]+\d+(?:\.\d+)?(?:ms|s|m|h))*[ \t]*$`,
)

// stripCompletionSummary removes finished-turn summary lines from a pane
// tail before activity-hint matching. See completionSummaryLineRegex.
func stripCompletionSummary(tail string) string {
	return completionSummaryLineRegex.ReplaceAllString(tail, "")
}

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
		blockedSince:    make(map[string]time.Time),
		blockedClass:    make(map[string]string),
		hintLogState:    make(map[string]hintLogEntry),
		busyRegex:       busyRegex,
		blockedRegex:    defaultBlockedRegex,
		hashFunc:        defaultHash,
	}
}

// shouldEmitHintLog reports whether the
// `pane_blocked_prompt_hint_unmatched` DEBUG line should be emitted
// for agentID right now. The throttle key is (agentID, hint-class)
// — the broad classification of *which* prompt marker (`❯`, `Yes/No`,
// `Do you want`, `Proceed?`, `approval required`, `Confirm?`) is
// present — rather than the tail hash. Pre-2026-05-06 we keyed on
// tail-hash but the spinner / Cogitated counter / sub-second timer
// produced just enough cross-scan delta that the throttle slipped
// every few seconds (Report 2026-05-06 P1-3, P2-3 — 25 emissions per
// E2E run despite "30 s throttle"). Hint-class doesn't shift on
// noise so the first emission for a class is recorded and the next
// 30 s of the same class is suppressed; a *new* hint marker appearing
// immediately re-emits so a genuine wedge surfaces fresh evidence.
func (t *Tracker) shouldEmitHintLog(agentID, hintClass string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.hintLogState[agentID]
	if ok && prev.hintClass == hintClass && now.Sub(prev.at) < blockedHintLogWindow {
		return false
	}
	t.hintLogState[agentID] = hintLogEntry{at: now, hintClass: hintClass}
	return true
}

// classifyBlockedHint returns a short stable token identifying which
// prompt-shape marker matched in content. Used as the throttle key for
// `pane_blocked_prompt_hint_unmatched`. The marker is taken from
// blockedHintRegex's first match — if the regex evolves, this token
// list grows automatically.
func classifyBlockedHint(content string) string {
	m := blockedHintRegex.FindString(content)
	if m == "" {
		return ""
	}
	// Lowercase for stability against case variation. The hint patterns
	// are ASCII / known glyphs so ToLower is safe and cheap.
	return strings.ToLower(m)
}

func classifyBlockedPrompt(tail string) string {
	if unrecoverableBlockedRegex.MatchString(tail) {
		return "unrecoverable"
	}
	if unrecoverableUnsandboxedBlockedRegex.MatchString(tail) {
		return "unrecoverable"
	}
	return ""
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
	if rl := rateLines(tail); rl != "" {
		snap.RateLinesHash = t.hashFunc(rl)
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

	// VerdictBlocked — the captured content matched the blocked-prompt
	// detector (interactive confirmation / approval prompt). The
	// underlying agent process is alive (it's the one rendering the
	// prompt) but is NOT making forward progress. Callers should treat
	// the lease the same as VerdictActive (extend, do not release) so
	// the in-flight Bash invocation does not race with a new dispatch
	// epoch — but they MUST NOT refresh circuit-breaker progress
	// timestamps, otherwise progress_timeout never fires while the
	// pane is wedged on a prompt that the operator never approves.
	// Reports of 2026-05-04 confirmed pane-active extension was
	// continuously refreshing last_progress_at and defeating the
	// progress_timeout back-stop.
	VerdictBlocked

	// VerdictTerminalError — the captured content matched a non-recoverable
	// error displayed by the agent runtime (Claude API HTTP 4xx error,
	// content-filtering rejection, invalid_request_error, …). The agent
	// process is alive but the in-flight task cannot make progress: the
	// runtime has produced a hard error frame that the LLM cannot self-
	// recover from. Callers MUST fail the task immediately rather than
	// extending the lease — the alternative (Report 2026-05-06 P0-2) is a
	// 30-minute stuck pane that ultimately re-dispatches the same task to
	// the same stale TUI. The blocked-pane timeout (3 min) is too long for
	// this case because the error content will not change without
	// operator intervention.
	VerdictTerminalError
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
	case VerdictBlocked:
		return "blocked"
	case VerdictTerminalError:
		return "terminal_error"
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

	// Terminal-error fast path: if the pane content shows a runtime
	// error frame the LLM cannot recover from (Claude API 4xx,
	// content-policy rejection, invalid_request_error, …), the in-flight
	// task is dead in the water — no amount of lease extension will get
	// the worker unstuck because the runtime has already given up on
	// this turn. Surface this verdict so the caller can fail the task
	// immediately instead of waiting on max_in_progress_min (Report
	// 2026-05-06 P0-2: a content-filter rejection kept the pane "active"
	// for 30 min, then the same task was re-dispatched onto the still-
	// stale TUI). Runs BEFORE the blocked-prompt detector because a
	// terminal error frame can sit alongside scrollback that contains a
	// stale `❯` from an earlier prompt; classifying as terminal-error is
	// strictly more actionable than blocked.
	if terminalErrorRegex.MatchString(content) {
		t.mu.Lock()
		delete(t.uncertainStreak, agentID)
		// Keep BlockedSince / BlockedClass untouched — this verdict has
		// its own recovery path in the queue scanner, independent of the
		// blocked-pane timeout machinery.
		t.mu.Unlock()
		attrs := []any{
			"agent_id", agentID,
			"hint", "agent runtime emitted a terminal error frame (Claude API 4xx / content filter / invalid_request_error). The scanner will fail the in-flight task immediately and respawn the pane to clear the stale TUI — no operator action needed unless the error indicates configuration drift (revoked API key, etc.).",
		}
		if paneTailWarnOptedIn() {
			attrs = append(attrs, "tail", trimForLog(tailLines(content), 512))
		}
		slogc().Warn("pane_terminal_error_detected", attrs...)
		_ = t.RecordObservation(agentID, content, now)
		return VerdictTerminalError
	}

	// Context-budget-exhausted fast path: agent TUI shows >=97% context
	// usage. The next turn is at high risk of being silently truncated /
	// auto-cleared by the runtime, leaving the daemon's in-flight task
	// wedged. Treat as TerminalError so the queue scanner fails the task
	// immediately and respawns the pane (clears the stale TUI; the next
	// dispatch starts on a fresh epoch). Avoids the 30-min wedge observed
	// in Report 2026-05-06 P1 NEW.
	//
	// Tail-only match: the % indicator lives in the TUI status line at
	// the bottom. Matching against full capture would risk hits on
	// scrollback containing benchmark numbers like "97% coverage".
	tailForBudget := tailLines(content)
	if contextBudgetExhaustedRegex.MatchString(tailForBudget) {
		t.mu.Lock()
		delete(t.uncertainStreak, agentID)
		// Keep BlockedSince / BlockedClass untouched for consistency with
		// the terminal-error path; context exhaustion has its own
		// immediate failure path in the queue scanner.
		t.mu.Unlock()
		attrs := []any{
			"agent_id", agentID,
			"hint", "agent TUI shows >=97% context usage. The in-flight task is at high risk of being silently truncated by the runtime; failing it now and respawning the pane is preferable to a 30-min wedge. The same task will be retried on a fresh agent epoch.",
		}
		if paneTailWarnOptedIn() {
			attrs = append(attrs, "tail", trimForLog(tailForBudget, 256))
		}
		slogc().Warn("pane_context_budget_exhausted", attrs...)
		_ = t.RecordObservation(agentID, content, now)
		return VerdictTerminalError
	}

	// Blocked-prompt fast path: if the pane shows an interactive
	// confirmation prompt (e.g. Claude Code's "❯ 1. Yes" tool-call
	// confirmation), the worker process is alive and waiting on operator
	// input — it is NOT idle and the lease MUST NOT be released. Earlier
	// versions of this branch returned VerdictIdle which kicked the
	// caller into the busy-check / release path; the operator (or
	// auto-approval) eventually clearing the prompt then ran the
	// already-in-flight Bash invocation against a stale lease epoch and
	// surfaced as `FENCING_REJECT` (Reports 1 & 2 of 2026-05-03,
	// `maestro result write` from a worker pane). Since the agent is
	// still bound to the prompt, treat the verdict as Active so the
	// caller proactively extends the lease. The hard upper bound on
	// "blocked indefinitely" stays on the circuit breaker
	// (progress_timeout, default 30 min) and on max_in_progress_min,
	// neither of which depend on this verdict — so the failure mode of
	// a never-approved prompt is bounded recovery, not deadlock.
	//
	// This must run BEFORE the busy-pattern fast path because a
	// scrollback line containing "Working" or "Thinking" can otherwise
	// override the meaningful "blocked" state.
	// Two-stage detection:
	//   - blockedRe matches against the full capture (~200 line
	//     scrollback) so Unicode boxes and prompts mid-scroll still get
	//     caught.
	//   - defaultBlockedTailRegex matches against the tail (~5 lines)
	//     only, holding narrow patterns that would otherwise false-positive
	//     against scrollback noise.
	tailForBlocked := tailLines(content)
	blockedMatched := blockedRe != nil && blockedRe.MatchString(content)
	if !blockedMatched {
		blockedMatched = defaultBlockedTailRegex.MatchString(tailForBlocked)
	}
	if blockedMatched {
		t.mu.Lock()
		delete(t.uncertainStreak, agentID)
		// Stamp the start of the blocked run on the FIRST blocked
		// observation; preserve the existing timestamp on consecutive
		// blocked observations so callers can compute total wedged time.
		blockedSince, hadBlocked := t.blockedSince[agentID]
		if !hadBlocked {
			t.blockedSince[agentID] = now
			blockedSince = now
		}
		blockedClass := classifyBlockedPrompt(tailForBlocked)
		t.blockedClass[agentID] = blockedClass
		t.mu.Unlock()
		// Surface the detection so operators see why a pane was kept
		// active despite no forward progress. The queue scanner enforces
		// a short blocked-prompt timeout (consulted via BlockedSince) and
		// will fail the in-flight task if the wedge persists beyond it,
		// so the failure mode is bounded. Tail snippet is opt-in via
		// MAESTRO_LOG_PANE_TAIL because pane content can be sensitive.
		attrs := []any{
			"agent_id", agentID,
			"blocked_for_sec", int64(now.Sub(blockedSince).Seconds()),
			"hint", "pane is sitting on a confirmation/approval prompt. The scanner will fail the in-flight task once the blocked-prompt timeout elapses. Configure auto-approval / trusted command allowlists in your runtime settings (~/.claude / codex / gemini) so the prompt does not appear at all.",
		}
		if paneTailWarnOptedIn() {
			attrs = append(attrs, "tail", trimForLog(tailForBlocked, 512))
		}
		slogc().Warn("pane_blocked_prompt_detected", attrs...)
		_ = t.RecordObservation(agentID, content, now)
		return VerdictBlocked
	}
	// Not blocked → clear any pending blocked-since stamp so the next
	// blocked observation starts fresh. Cheap when nothing was set.
	t.mu.Lock()
	delete(t.blockedSince, agentID)
	delete(t.blockedClass, agentID)
	t.mu.Unlock()
	// Diagnostic when a strict-match miss leaves obvious prompt symptoms
	// in the captured content. Reports of 2026-05-04 found a real
	// reproduction where the user observed the prompt in the pane but
	// no `pane_blocked_prompt_detected` ever fired; without a debug
	// log there is no way to tell whether the regex missed, the
	// capture missed, or something else swallowed the verdict. This
	// emits at debug level so it does not affect production noise but
	// is available when operators are inspecting hangs.
	if hintClass := classifyBlockedHint(content); hintClass != "" {
		tail := tailLines(content)
		if t.shouldEmitHintLog(agentID, hintClass, now) {
			// Include capture size so operators can tell whether a missing
			// match is "regex did not cover this phrasing" vs "capture
			// returned a tiny snippet from a different pane". Pane content
			// snippet is opt-in via MAESTRO_LOG_PANE_TAIL because it can
			// contain command text or model output that is sensitive.
			attrs := []any{
				"agent_id", agentID,
				"hint_class", hintClass,
				"content_bytes", len(content),
				"tail_bytes", len(tail),
				"hint", "pane content contains prompt-like markers (❯ / Yes/No / Do you want / Proceed?) but the strict detector did not match. If this is a genuine block, broaden defaultBlockedRegex; if capture size is small, the wrong pane may have been captured; if not, ignore.",
			}
			if paneTailWarnOptedIn() {
				attrs = append(attrs, "tail", trimForLog(tail, 512), "head", trimForLogHead(content, 512))
			}
			slogc().Debug("pane_blocked_prompt_hint_unmatched", attrs...)
		}
	}

	tail := tailLines(content)
	tailHash := t.hashFunc(normalizeForLiveness(tail))
	rateLinesHash := ""
	if rl := rateLines(tail); rl != "" {
		rateLinesHash = t.hashFunc(rl)
	}
	// activeHintMatched is true when the raw tail contains an
	// always-on activity marker (defaultActiveHintRegex). The operator's
	// busy_pattern is honoured first if set, but the daemon-side default
	// catches markers an operator config probably never lists (Japanese
	// verbs, Claude Code 2.x verbs, ⎿ subprocess output prefix, etc.).
	// This is a daemon-managed default by design — see the comment on
	// defaultActiveHintRegex for the rationale.
	//
	// Finished-turn summary lines ("✻ Cooked for 1m 35s") are stripped
	// first: they share the spinner glyph set but indicate the pane is
	// DONE, not active — see completionSummaryLineRegex.
	activeHintMatched := defaultActiveHintRegex.MatchString(stripCompletionSummary(tail))
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
	case activeHintMatched:
		// Daemon-side activity hint matched — tail shows an LLM agent
		// spinner / activity verb. The agent's UI is alive even if the
		// hash happens to be identical to the previous scan (long
		// subprocess holding the same animation frame). Without this
		// branch the hash-equal default would mis-classify as Idle and
		// the lease would be released on top of a still-running task —
		// the package-proxy P0-1 regression. max_in_progress_min remains
		// the wall-clock back-stop, so over-permissive Active here only
		// delays the eventual hard cap, never erases it.
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
	case rateLinesHash != "" && prev.RateLinesHash != "" && prev.RateLinesHash != rateLinesHash:
		// Rate-delta weak signal (W-P1): the tail's transfer/progress-rate
		// lines (MB/s, it/s, ETA …) changed across scans even though the
		// numeric-normalised tail hash did not — a tool that reports
		// progress purely through numbers (bare wget/dd/tqdm in the pane)
		// is mid-transfer. Scoping the delta to rate-marker lines keeps
		// the hung-TUI timer churn (2026-05-06 P0) inert — those lines
		// carry no rate units — and a COMPLETED tool's frozen progress
		// line produces no delta, so this cannot pin an idle pane Active
		// the way a static rate-unit hint would. max_in_progress_min
		// remains the wall-clock back-stop.
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
	delete(t.blockedSince, agentID)
	delete(t.blockedClass, agentID)
}

// BlockedSince reports the wall-clock timestamp of the FIRST observation
// in the agent's current consecutive run of VerdictBlocked outcomes, or
// the zero value when the agent is not currently observed as blocked.
// Cleared on the next non-blocked observation, so callers see only an
// uninterrupted blocked streak. Used by the queue scanner to enforce a
// blocked-prompt timeout that is tighter than circuit_breaker progress
// timeout.
func (t *Tracker) BlockedSince(agentID string) (time.Time, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ts, ok := t.blockedSince[agentID]
	return ts, ok
}

// BlockedClass returns the classification of the current blocked prompt
// for agentID: "unrecoverable" for prompts that cannot be cleared without
// operator action when bypassPermissions is disabled (unsandboxed-command
// and protected-path-edit confirmations), or "" for an ordinary blocked
// prompt / when the agent is not blocked. Pairs with BlockedSince.
func (t *Tracker) BlockedClass(agentID string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.blockedClass[agentID]
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

// paneTailWarnOptedIn reports whether the operator has opted in to having
// pane-tail snippets included in pane_blocked_prompt_detected log lines.
// Default is off so privacy-sensitive deployments do not see operator
// command text or model output in daemon logs.
func paneTailWarnOptedIn() bool {
	v := strings.TrimSpace(os.Getenv("MAESTRO_LOG_PANE_TAIL"))
	return v == "1" || v == "true" || v == "TRUE" || v == "yes"
}

// trimForLog returns s truncated to at most maxBytes, taking the trailing
// segment because the bottom of a pane capture is where the active
// confirmation prompt lives.
func trimForLog(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[len(s)-maxBytes:]
}

// trimForLogHead returns s truncated to at most maxBytes from the
// beginning of the string. Used by the blocked-hint diagnostic so
// operators can see whether the prompt is rendered above the captured
// tail when the strict regex misses.
func trimForLogHead(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes]
}
