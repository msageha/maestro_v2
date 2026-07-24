package hud

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// ANSI SGR fragments. Only colors/attributes — cursor and screen control
// live in run.go. All are suppressed when RenderOptions.Color is false
// (NO_COLOR support).
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// Diffs carries the history anchors for the snapshot-diff view.
type Diffs struct {
	// Prev is the previous distinct sample ("change since the last time
	// anything moved"). Nil when the trail has fewer than 2 records.
	Prev *Record
	// Daily is the ~24h baseline record (History.Baseline). Nil when the
	// trail is empty.
	Daily *Record
}

// RenderOptions parameterises a frame. Everything is explicit so Render is
// a pure function of its inputs (terminal-independent, unit-testable).
type RenderOptions struct {
	Width    int
	Color    bool
	Now      time.Time
	Version  string
	Project  string // display name, e.g. base dir of the project
	Dir      string // resolved .maestro path
	Interval time.Duration
	// Note is an optional footer annotation (e.g. history write failures).
	Note string
}

// frame accumulates truncated, optionally colorised lines.
type frame struct {
	lines []string
	width int
	color bool
}

// add sanitizes then truncates: every line passes through sanitizeControls
// so externally sourced text (worker summaries, learnings, signal errors,
// IDs from on-disk YAML) cannot smuggle terminal control sequences into the
// operator's terminal. The renderer's own ANSI colors are applied after
// sanitization (in addc), never stripped by it.
func (f *frame) add(s string) {
	f.lines = append(f.lines, truncateRunes(sanitizeControls(s), f.width))
}

// addc adds a line wrapped in the given SGR sequence when color is on.
// Sanitization and truncation happen on the plain text so escape bytes
// never get cut and injected sequences never survive.
func (f *frame) addc(sgr, s string) {
	s = truncateRunes(sanitizeControls(s), f.width)
	if f.color && sgr != "" {
		s = sgr + s + ansiReset
	}
	f.lines = append(f.lines, s)
}

func (f *frame) section(title string) {
	f.add("")
	f.addc(ansiBold+ansiCyan, title)
}

func truncateRunes(s string, width int) string {
	if width <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// Render produces one full HUD frame as plain text (lines joined by \n, no
// cursor control). It is the terminal-independent core of the TUI.
func Render(s *Snapshot, d Diffs, opt RenderOptions) string {
	if opt.Width <= 0 {
		opt.Width = 100
	}
	f := &frame{width: opt.Width, color: opt.Color}

	renderHeader(f, s, opt)
	renderQueues(f, s)
	renderCommands(f, s)
	renderAttention(f, s)
	renderProgress(f, s, d, opt)
	renderUsage(f, s)
	renderSelfImprovement(f, s)
	renderResults(f, s)

	f.add("")
	footer := fmt.Sprintf("read-only · poll %s · quit: q+Enter or Ctrl-C", opt.Interval)
	if opt.Note != "" {
		footer += " · " + opt.Note
	}
	f.addc(ansiDim, footer)

	return strings.Join(f.lines, "\n")
}

func renderHeader(f *frame, s *Snapshot, opt RenderOptions) {
	title := fmt.Sprintf("MAESTRO HUD %s · %s", opt.Version, opt.Project)
	ts := s.CollectedAt.Format("2006-01-02 15:04:05")
	f.addc(ansiBold, padBetween(title, ts, f.width))
	f.addc(ansiDim, opt.Dir)

	state, sgr := daemonState(s, opt.Now)
	f.add("")
	line := "DAEMON     " + state
	if s.Metrics.Err != "" {
		line = "DAEMON     " + s.Metrics.Err
		sgr = ansiYellow
	}
	f.addc(sgr, line)
}

// daemonState derives liveness from the metrics heartbeat age. The daemon
// refreshes the heartbeat every scan tick (seconds), so a minute of silence
// means it is gone or wedged.
func daemonState(s *Snapshot, now time.Time) (string, string) {
	hb := s.Metrics.DaemonHeartbeat
	if hb == "" {
		return "unknown (no heartbeat in state/metrics.yaml)", ansiYellow
	}
	t, err := time.Parse(time.RFC3339, hb)
	if err != nil {
		return "unknown (unparseable heartbeat)", ansiYellow
	}
	age := now.Sub(t)
	switch {
	case age < 0:
		return "running (heartbeat in the future?)", ansiYellow
	case age <= 30*time.Second:
		return fmt.Sprintf("running (heartbeat %s ago)", fmtAge(age)), ansiGreen
	case age <= 3*time.Minute:
		return fmt.Sprintf("stale (heartbeat %s ago)", fmtAge(age)), ansiYellow
	default:
		return fmt.Sprintf("stopped? (last heartbeat %s ago)", fmtAge(age)), ansiRed
	}
}

func renderQueues(f *frame, s *Snapshot) {
	f.section("QUEUES (pending/in_progress)")
	if s.Queues.Err != "" {
		f.addc(ansiYellow, "  "+s.Queues.Err)
		return
	}
	if len(s.Queues.Rows) == 0 {
		f.addc(ansiDim, "  (no queue files)")
		return
	}
	parts := make([]string, 0, len(s.Queues.Rows))
	var pend, run int
	for _, q := range s.Queues.Rows {
		parts = append(parts, fmt.Sprintf("%s %d/%d", q.Name, q.Pending, q.InProgress))
		pend += q.Pending
		run += q.InProgress
	}
	f.add(fmt.Sprintf("  total %d/%d · %s", pend, run, strings.Join(parts, " · ")))
}

func renderCommands(f *frame, s *Snapshot) {
	f.section(fmt.Sprintf("COMMANDS (%d active / %d total)", s.Commands.ActiveCount, s.Commands.TotalCommands))
	if s.Commands.Err != "" {
		f.addc(ansiYellow, "  "+s.Commands.Err)
		return
	}
	if len(s.Commands.Rows) == 0 {
		f.addc(ansiDim, "  (no commands)")
		return
	}
	for _, row := range s.Commands.Rows {
		phase := fmt.Sprintf("phases %d/%d", row.PhasesDone, row.PhasesTotal)
		if row.ActivePhase != "" {
			phase += fmt.Sprintf(" (%s)", row.ActivePhase)
		}
		t := row.Tasks
		tasks := fmt.Sprintf("tasks %dok/%drun/%dpend/%dfail", t.Completed, t.InFlight, t.Pending, t.Failed)
		if t.Paused > 0 {
			tasks += fmt.Sprintf("/%dpaused", t.Paused)
		}
		if t.Cancelled > 0 {
			tasks += fmt.Sprintf("/%dcan", t.Cancelled)
		}
		integ := row.Integration
		if integ == "" {
			integ = "-"
		}
		sgr := ""
		switch row.PlanStatus {
		case "failed", "cancelled":
			sgr = ansiRed
		case "completed":
			sgr = ansiDim
		}
		f.addc(sgr, fmt.Sprintf("  %-34s %-10s %-24s %-32s integ=%s",
			row.CommandID, row.PlanStatus, phase, tasks, integ))
	}
}

func renderAttention(f *frame, s *Snapshot) {
	f.section("ATTENTION")
	summary := fmt.Sprintf("  signals %d · dead_letters %d · quarantine %d",
		s.Signals.Total, s.Attention.DeadLetterFiles, s.Attention.QuarantineFiles)
	alerts := 0
	if s.Metrics.Usage != nil {
		alerts = len(s.Metrics.Usage.BudgetAlerts)
	}
	if alerts > 0 {
		summary += fmt.Sprintf(" · budget alerts %d", alerts)
	}
	hot := s.Signals.Total > 0 || s.Attention.DeadLetterFiles > 0 || s.Attention.QuarantineFiles > 0 || alerts > 0
	if hot {
		f.addc(ansiYellow, summary)
	} else {
		f.addc(ansiDim, summary+" · (nothing needs you)")
	}
	if s.Signals.Err != "" {
		f.addc(ansiYellow, "  signals: "+s.Signals.Err)
	}
	for _, sg := range s.Signals.Rows {
		line := fmt.Sprintf("  [%s] %s %s", sg.Kind, sg.CommandID, sg.PhaseID)
		if sg.WorkerID != "" {
			line += " " + sg.WorkerID
		}
		line += fmt.Sprintf(" attempts=%d", sg.Attempts)
		if sg.LastError != "" {
			line += " err=" + sg.LastError
		}
		f.addc(ansiYellow, line)
	}
	if s.Metrics.Usage != nil {
		for _, a := range s.Metrics.Usage.BudgetAlerts {
			f.addc(ansiRed, "  BUDGET: "+a)
		}
	}
}

// progressRow is one Δ line of the snapshot-diff view.
type progressRow struct {
	label string
	cur   string
	prev  string
	daily string
}

func renderProgress(f *frame, s *Snapshot, d Diffs, opt RenderOptions) {
	dailyHdr := "Δ 24h"
	if d.Daily != nil {
		if ts, err := time.Parse(time.RFC3339, d.Daily.TS); err == nil {
			age := opt.Now.Sub(ts)
			if age < 23*time.Hour {
				dailyHdr = fmt.Sprintf("Δ since %s", fmtAge(age))
			}
		}
	}
	f.section("PROGRESS (snapshot diff)")
	if s.Metrics.Err != "" {
		f.addc(ansiYellow, "  "+s.Metrics.Err)
		return
	}
	if d.Prev == nil && d.Daily == nil {
		f.addc(ansiDim, "  (no history yet — diffs appear after the first change)")
	}

	cur := SnapshotGauges(s)
	rows := []progressRow{
		gaugeRowInt("tasks_completed", cur.TasksCompleted, d, func(g *Gauges) int { return g.TasksCompleted }),
		gaugeRowInt("tasks_failed", cur.TasksFailed, d, func(g *Gauges) int { return g.TasksFailed }),
		gaugeRowInt("tasks_dispatched", cur.TasksDispatched, d, func(g *Gauges) int { return g.TasksDispatched }),
		gaugeRowInt("dead_letters", cur.DeadLetters, d, func(g *Gauges) int { return g.DeadLetters }),
		gaugeRowInt("queue_pending", cur.QueuePending, d, func(g *Gauges) int { return g.QueuePending }),
		gaugeRowInt("learnings", cur.LearningsTotal, d, func(g *Gauges) int { return g.LearningsTotal }),
		gaugeRowTokens("tokens_in", cur.InputTokens, d, func(g *Gauges) int64 { return g.InputTokens }),
		gaugeRowTokens("tokens_out", cur.OutputTokens, d, func(g *Gauges) int64 { return g.OutputTokens }),
		gaugeRowCost("est_cost_usd", cur.EstimatedCostUSD, d),
	}
	f.add(fmt.Sprintf("  %-18s %12s %12s %16s", "", "total", "Δ prev", dailyHdr))
	for _, r := range rows {
		f.add(fmt.Sprintf("  %-18s %12s %12s %16s", r.label, r.cur, r.prev, r.daily))
	}
}

func gaugeRowInt(label string, cur int, d Diffs, get func(*Gauges) int) progressRow {
	row := progressRow{label: label, cur: fmt.Sprintf("%d", cur), prev: "-", daily: "-"}
	if d.Prev != nil {
		row.prev = fmtDelta(int64(cur - get(&d.Prev.Gauges)))
	}
	if d.Daily != nil {
		row.daily = fmtDelta(int64(cur - get(&d.Daily.Gauges)))
	}
	return row
}

func gaugeRowTokens(label string, cur int64, d Diffs, get func(*Gauges) int64) progressRow {
	row := progressRow{label: label, cur: fmtTokens(cur), prev: "-", daily: "-"}
	if d.Prev != nil {
		row.prev = fmtDeltaTokens(cur - get(&d.Prev.Gauges))
	}
	if d.Daily != nil {
		row.daily = fmtDeltaTokens(cur - get(&d.Daily.Gauges))
	}
	return row
}

func gaugeRowCost(label string, cur float64, d Diffs) progressRow {
	row := progressRow{label: label, cur: fmt.Sprintf("%.4f", cur), prev: "-", daily: "-"}
	if d.Prev != nil {
		row.prev = fmt.Sprintf("%+.4f", cur-d.Prev.EstimatedCostUSD)
	}
	if d.Daily != nil {
		row.daily = fmt.Sprintf("%+.4f", cur-d.Daily.EstimatedCostUSD)
	}
	return row
}

func renderUsage(f *frame, s *Snapshot) {
	u := s.Metrics.Usage
	f.section("USAGE (cost tracking)")
	if s.Metrics.Err != "" {
		f.addc(ansiYellow, "  "+s.Metrics.Err)
		return
	}
	if u == nil {
		f.addc(ansiDim, "  (disabled or never collected)")
		return
	}
	hdr := "  collected " + u.CollectedAt
	if u.Partial {
		hdr += " [partial — totals are a lower bound]"
	}
	f.addc(ansiDim, hdr)
	f.add(fmt.Sprintf("  %-14s %-12s %10s %10s %10s %10s %10s", "AGENT", "RUNTIME", "IN", "OUT", "CACHE_R", "CACHE_W", "COST"))
	for _, id := range sortedKeys(u.Agents) {
		a := u.Agents[id]
		if a == nil {
			continue
		}
		if !a.TokensKnown {
			f.add(fmt.Sprintf("  %-14s %-12s %10s", id, a.Runtime, "unknown (no local usage record)"))
			continue
		}
		cost := "-"
		if a.EstimatedCostUSD != nil {
			cost = fmt.Sprintf("$%.4f", *a.EstimatedCostUSD)
		}
		f.add(fmt.Sprintf("  %-14s %-12s %10s %10s %10s %10s %10s",
			id, a.Runtime,
			fmtTokens(a.Totals.InputTokens),
			fmtTokens(a.Totals.OutputTokens),
			fmtTokens(a.Totals.CacheReadInputTokens),
			fmtTokens(a.Totals.CacheCreationInputTokens),
			cost))
	}
	if top := topCommandCosts(u, 3); top != "" {
		f.add("  top commands: " + top)
	}
}

func renderSelfImprovement(f *frame, s *Snapshot) {
	f.section(fmt.Sprintf("SELF-IMPROVEMENT · learnings %d · skill candidates: %d pending / %d approved / %d rejected",
		s.Learnings.Total, s.SkillCandidates.Pending, s.SkillCandidates.Approved, s.SkillCandidates.Rejected))
	if s.Learnings.Err != "" {
		f.addc(ansiYellow, "  learnings: "+s.Learnings.Err)
	}
	for _, l := range s.Learnings.Latest {
		f.add(fmt.Sprintf("  learn %s [%s]: %s", shortTime(l.CreatedAt), l.SourceWorker, oneLine(l.Content)))
	}
	if s.SkillCandidates.Err != "" {
		f.addc(ansiYellow, "  skill candidates: "+s.SkillCandidates.Err)
	}
	for _, c := range s.SkillCandidates.PendingRows {
		f.add(fmt.Sprintf("  cand %s x%d: %s", c.ID, c.Occurrences, oneLine(c.Content)))
	}
}

func renderResults(f *frame, s *Snapshot) {
	f.section("RECENT RESULTS")
	if s.Results.Err != "" {
		f.addc(ansiYellow, "  "+s.Results.Err)
		return
	}
	if len(s.Results.Rows) == 0 {
		f.addc(ansiDim, "  (none)")
		return
	}
	for _, r := range s.Results.Rows {
		id := r.TaskID
		if id == "" {
			id = r.CommandID
		}
		sgr := ""
		switch r.Status {
		case "failed", "dead_letter":
			sgr = ansiRed
		case "cancelled":
			sgr = ansiYellow
		}
		f.addc(sgr, fmt.Sprintf("  %s %-9s %-9s %-24s %s",
			shortTime(r.CreatedAt), r.Reporter, r.Status, id, oneLine(r.Summary)))
	}
}

// --- formatting helpers ---

// padBetween left-aligns a and right-aligns b within width (best effort).
func padBetween(a, b string, width int) string {
	gap := width - len([]rune(a)) - len([]rune(b))
	if gap < 1 {
		gap = 1
	}
	return a + strings.Repeat(" ", gap) + b
}

func fmtAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func fmtDelta(v int64) string {
	if v == 0 {
		return "0"
	}
	return fmt.Sprintf("%+d", v)
}

func fmtTokens(v int64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(v)/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fK", float64(v)/1_000)
	default:
		return fmt.Sprintf("%d", v)
	}
}

func fmtDeltaTokens(v int64) string {
	if v == 0 {
		return "0"
	}
	sign := "+"
	if v < 0 {
		sign = "-"
		v = -v
	}
	return sign + fmtTokens(v)
}

// shortTime reduces an RFC3339 timestamp to HH:MM:SS for row prefixes.
func shortTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		if len(rfc3339) >= 8 {
			return rfc3339[:8]
		}
		return rfc3339
	}
	return t.Format("15:04:05")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}

// isTerminalControl reports whether r can alter terminal state or line
// layout: C0 controls (including ESC, BEL, CR, LF, TAB, BS), DEL, and C1
// controls (0x80–0x9F — single-byte CSI/OSC introducers on many terminals).
func isTerminalControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// sanitizeControls strips terminal control characters from a rendered line.
// Whitespace controls (LF/CR/TAB) become a single space to preserve word
// boundaries; every other control rune — most importantly ESC and the C1
// range — is dropped, so an injected sequence like OSC 52 loses its
// introducer and renders as inert printable text.
func sanitizeControls(s string) string {
	if strings.IndexFunc(s, isTerminalControl) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case isTerminalControl(r):
			// dropped: never forward control bytes to the terminal
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// topCommandCosts formats the n most expensive commands from the usage
// section, e.g. "cmd_a $1.2000 · cmd_b $0.4400".
func topCommandCosts(u *model.UsageMetrics, n int) string {
	type kv struct {
		id   string
		cost float64
	}
	var items []kv
	for id, c := range u.Commands {
		if c == nil || c.EstimatedCostUSD == nil {
			continue
		}
		items = append(items, kv{id, *c.EstimatedCostUSD})
	}
	if len(items) == 0 {
		return ""
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].cost != items[j].cost {
			return items[i].cost > items[j].cost
		}
		return items[i].id < items[j].id
	})
	if len(items) > n {
		items = items[:n]
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s $%.4f", it.id, it.cost))
	}
	return strings.Join(parts, " · ")
}
