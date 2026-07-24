package hud

import (
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

func fixtureRenderOptions(color bool) RenderOptions {
	return RenderOptions{
		Width:    120,
		Color:    color,
		Now:      fixtureTime,
		Version:  "2.5.0",
		Project:  "demo",
		Dir:      "/tmp/demo/.maestro",
		Interval: 2 * time.Second,
	}
}

func TestRender_FullFixtureFrame(t *testing.T) {
	dir := newFixtureMaestroDir(t)
	s := Collect(dir, fixtureTime)

	prev := &Record{TS: fixtureTime.Add(-2 * time.Second).Format(time.RFC3339),
		Gauges: Gauges{TasksCompleted: 8, InputTokens: 1_100_000, EstimatedCostUSD: 4.0}}
	daily := &Record{TS: fixtureTime.Add(-25 * time.Hour).Format(time.RFC3339),
		Gauges: Gauges{TasksCompleted: 1, InputTokens: 100_000, EstimatedCostUSD: 0.5}}

	out := Render(s, Diffs{Prev: prev, Daily: daily}, fixtureRenderOptions(false))

	for _, want := range []string{
		"MAESTRO HUD 2.5.0 · demo",
		"DAEMON     running (heartbeat 3s ago)",
		"QUEUES (pending/in_progress)",
		"worker1 1/1",
		"COMMANDS (1 active / 1 total)",
		"cmd_1",
		"sealed",
		"phases 1/2 (implement)",
		"tasks 1ok/1run/1pend/1fail",
		"integ=merged",
		"ATTENTION",
		"signals 1 · dead_letters 1 · quarantine 0 · budget alerts 1",
		"[merge_conflict] cmd_1 ph_2 worker1 attempts=3",
		"BUDGET: agent worker1 exceeded per-agent budget",
		"PROGRESS (snapshot diff)",
		"tasks_completed",
		"+1", // 9 - 8 since prev
		"+8", // 9 - 1 since daily baseline
		"USAGE (cost tracking)",
		"[partial — totals are a lower bound]",
		"worker1        claude-code",
		"$4.2100",
		"unknown (no local usage record)",
		"top commands: cmd_1 $2.1000",
		"SELF-IMPROVEMENT · learnings 2 · skill candidates: 1 pending / 0 approved / 1 rejected",
		"verify runs at project root",
		"cand cand_1 x3: grep before edit",
		"RECENT RESULTS",
		"failed",
		"lint failed",
		"read-only · poll 2s · quit: q+Enter or Ctrl-C",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame missing %q\n--- frame ---\n%s", want, out)
		}
	}
}

func TestRender_NoColorHasNoEscapes(t *testing.T) {
	dir := newFixtureMaestroDir(t)
	s := Collect(dir, fixtureTime)

	out := Render(s, Diffs{}, fixtureRenderOptions(false))
	if strings.Contains(out, "\x1b") {
		t.Error("Color=false frame must not contain ANSI escapes (NO_COLOR contract)")
	}

	colored := Render(s, Diffs{}, fixtureRenderOptions(true))
	if !strings.Contains(colored, "\x1b[") {
		t.Error("Color=true frame should contain ANSI SGR sequences")
	}
}

// TestRender_StripsExternalControlSequences pins the sanitize boundary:
// worker-controlled strings (summaries, learnings, skill candidates, signal
// errors, IDs) must not smuggle terminal control bytes — OSC 52 clipboard
// writes, CSI, C1 introducers, CR, BEL — into the operator's terminal.
func TestRender_StripsExternalControlSequences(t *testing.T) {
	osc52 := "\x1b]52;c;bWFsaWNpb3Vz\x07" // OSC 52: clipboard write
	s := &Snapshot{
		CollectedAt: fixtureTime,
		Signals: SignalsSection{Total: 1, Rows: []SignalRow{{
			Kind:      "merge_conflict",
			CommandID: "cmd_\x1b[2J\x1b[31mred",
			PhaseID:   "ph_1",
			Attempts:  2,
			LastError: "boom" + osc52 + "\rovertype",
		}}},
		Learnings: LearningsSection{Total: 1, Latest: []model.Learning{{
			Content:      "learned\x9b31mthing", // C1 CSI (U+009B)
			CreatedAt:    "2026-07-24T11:30:00Z",
			SourceWorker: "worker\x071",
		}}},
		SkillCandidates: SkillCandidatesSection{Pending: 1, PendingRows: []model.SkillCandidate{{
			ID:          "cand_1",
			Content:     "grep" + osc52 + " before edit",
			Occurrences: 3,
		}}},
		Results: ResultsSection{Rows: []ResultRow{{
			Reporter:  "worker1",
			TaskID:    "task_1",
			Status:    "completed",
			Summary:   "done\x1b[9999;9999H\x08\x7fclean",
			CreatedAt: "2026-07-24T11:40:00Z",
		}}},
	}

	plain := Render(s, Diffs{}, fixtureRenderOptions(false))
	for _, bad := range []string{"\x1b", "\x9b", "\x07", "\r", "\x08", "\x7f"} {
		if strings.Contains(plain, bad) {
			t.Errorf("Color=false frame leaks control byte %q:\n%q", bad, plain)
		}
	}
	// Printable payload text must survive sanitization.
	for _, want := range []string{"boom", "overtype", "learned", "before edit", "clean"} {
		if !strings.Contains(plain, want) {
			t.Errorf("frame lost printable content %q", want)
		}
	}

	// With color on, the only escapes are the renderer's own SGR sequences;
	// injected OSC/CSI must still be gone.
	colored := Render(s, Diffs{}, fixtureRenderOptions(true))
	for _, bad := range []string{"\x1b]52", "\x1b[2J", "\x9b", "\x07", "\r"} {
		if strings.Contains(colored, bad) {
			t.Errorf("Color=true frame leaks injected sequence %q", bad)
		}
	}
	if !strings.Contains(colored, "\x1b[") {
		t.Error("Color=true frame should still contain the renderer's own SGR sequences")
	}
}

// TestRender_StripsControlSequencesEndToEnd covers the full path: a result
// summary written by a (possibly compromised) worker into results/*.yaml
// must reach the terminal with its escape bytes removed.
func TestRender_StripsControlSequencesEndToEnd(t *testing.T) {
	dir := newFixtureMaestroDir(t)
	writeFixtureFile(t, dir, "results/worker2.yaml", `schema_version: 1
file_type: result_task
results:
  - id: res_evil
    task_id: task_evil
    command_id: cmd_1
    status: completed
    summary: "pwn\x1b]52;c;bWFsaWNpb3Vz\x07\x1b[31mred\rovertype"
    created_at: "2026-07-24T11:55:00Z"
`)
	s := Collect(dir, fixtureTime)
	out := Render(s, Diffs{}, fixtureRenderOptions(false))
	for _, bad := range []string{"\x1b", "\x9b", "\x07", "\r"} {
		if strings.Contains(out, bad) {
			t.Errorf("frame leaks control byte %q from worker-written summary", bad)
		}
	}
	if !strings.Contains(out, "pwn") {
		t.Error("sanitized summary text should still render")
	}
}

func TestRender_WidthTruncatesEveryLine(t *testing.T) {
	dir := newFixtureMaestroDir(t)
	s := Collect(dir, fixtureTime)

	opt := fixtureRenderOptions(false)
	opt.Width = 40
	out := Render(s, Diffs{}, opt)
	for i, line := range strings.Split(out, "\n") {
		if n := len([]rune(line)); n > 40 {
			t.Errorf("line %d exceeds width: %d runes: %q", i, n, line)
		}
	}
}

func TestRender_UnavailableSectionsStillRender(t *testing.T) {
	s := Collect(t.TempDir(), fixtureTime) // empty dir: most sections unavailable
	out := Render(s, Diffs{}, fixtureRenderOptions(false))

	if !strings.Contains(out, "unavailable") {
		t.Errorf("frame should mark unreadable sections as unavailable:\n%s", out)
	}
	if !strings.Contains(out, "MAESTRO HUD") {
		t.Error("header must render even with nothing readable")
	}
	if !strings.Contains(out, "DAEMON     unavailable") {
		t.Errorf("daemon line should surface the metrics read failure:\n%s", out)
	}
}

func TestRender_EmptyDiffsAreExplained(t *testing.T) {
	dir := newFixtureMaestroDir(t) // metrics readable, but no history anchors
	s := Collect(dir, fixtureTime)
	out := Render(s, Diffs{}, fixtureRenderOptions(false))
	if !strings.Contains(out, "(no history yet") {
		t.Errorf("empty diffs should be explained:\n%s", out)
	}
}

func TestDaemonState_Thresholds(t *testing.T) {
	now := fixtureTime
	cases := []struct {
		name string
		hb   string
		want string
	}{
		{"fresh", now.Add(-3 * time.Second).Format(time.RFC3339), "running"},
		{"stale", now.Add(-2 * time.Minute).Format(time.RFC3339), "stale"},
		{"gone", now.Add(-30 * time.Minute).Format(time.RFC3339), "stopped?"},
		{"missing", "", "unknown"},
		{"garbage", "not-a-time", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Snapshot{Metrics: MetricsSection{DaemonHeartbeat: tc.hb}}
			got, _ := daemonState(s, now)
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("daemonState(%q) = %q, want prefix %q", tc.hb, got, tc.want)
			}
		})
	}
}

func TestFormattingHelpers(t *testing.T) {
	if got := fmtTokens(1_234_567); got != "1.2M" {
		t.Errorf("fmtTokens = %q", got)
	}
	if got := fmtTokens(4_500); got != "4.5K" {
		t.Errorf("fmtTokens = %q", got)
	}
	if got := fmtDeltaTokens(-1_500); got != "-1.5K" {
		t.Errorf("fmtDeltaTokens = %q", got)
	}
	if got := fmtDelta(0); got != "0" {
		t.Errorf("fmtDelta(0) = %q", got)
	}
	if got := fmtDelta(3); got != "+3" {
		t.Errorf("fmtDelta(3) = %q", got)
	}
	if got := truncateRunes("あいうえお", 3); got != "あい…" {
		t.Errorf("truncateRunes = %q", got)
	}
	if got := fmtAge(75 * time.Second); got != "1m15s" {
		t.Errorf("fmtAge = %q", got)
	}
}
