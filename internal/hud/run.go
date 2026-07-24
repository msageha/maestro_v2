package hud

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// Terminal control sequences (stdlib-only TUI). The alternate screen keeps
// the operator's scrollback intact — mandatory inside tmux — and is always
// restored on exit, including SIGINT/SIGTERM (the caller cancels ctx via
// signal.NotifyContext).
const (
	enterAltScreen = "\x1b[?1049h"
	leaveAltScreen = "\x1b[?1049l"
	hideCursor     = "\x1b[?25l"
	showCursor     = "\x1b[?25h"
	cursorHome     = "\x1b[H"
	eraseLineRest  = "\x1b[K"
	eraseBelow     = "\x1b[J"
)

// Interval bounds. Polling below 1s hammers the state dir for no operator
// benefit; above 10s the HUD stops feeling live.
const (
	MinInterval     = 1 * time.Second
	MaxInterval     = 10 * time.Second
	DefaultInterval = 2 * time.Second
)

// DailyBaselineAge is the history-baseline age for the "Δ 24h" column.
const DailyBaselineAge = 24 * time.Hour

// Options configures a HUD run.
type Options struct {
	// MaestroDir is the resolved .maestro directory (read-only source).
	MaestroDir string
	// Out receives frames. The interactive loop assumes it is a terminal.
	Out io.Writer
	// Input, when non-nil, is watched for "q"/"quit" lines to exit the
	// loop (stdin in production; nil in tests and --once mode).
	Input io.Reader
	// Interval is the poll period, clamped to [MinInterval, MaxInterval].
	Interval time.Duration
	// Width is the render width in columns (0 = default 100).
	Width int
	// Color enables ANSI SGR colors (caller resolves NO_COLOR / --no-color).
	Color bool
	// Once renders exactly one frame to Out and returns without touching
	// the terminal modes and without writing history (fully read-only).
	Once bool
	// HistoryPath is the snapshot-diff JSONL file; empty keeps the trail
	// in memory only.
	HistoryPath string
	// Version is shown in the header.
	Version string
	// now is injectable for tests; nil means time.Now.
	Now func() time.Time
}

func (o *Options) normalize() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Interval < MinInterval {
		o.Interval = DefaultInterval
	}
	if o.Interval > MaxInterval {
		o.Interval = MaxInterval
	}
	if o.Width <= 0 {
		o.Width = 100
	}
}

// Run executes the HUD until ctx is cancelled (or immediately in Once
// mode). It never returns an error for unreadable .maestro content — only
// for terminal-write failures.
func Run(ctx context.Context, opts Options) error {
	opts.normalize()

	if opts.Once {
		// Once mode is fully read-only: the trail is consulted for diffs
		// (LoadHistory never writes) but no sample is appended.
		frame, _ := buildFrame(opts, NewCollector(), loadHistoryBestEffort(opts.HistoryPath, opts.Now()), "")
		_, err := io.WriteString(opts.Out, frame+"\n")
		return err
	}

	now := opts.Now()
	hist := loadHistoryBestEffort(opts.HistoryPath, now)
	histNote := ""
	if opts.HistoryPath != "" && hist.path == "" {
		histNote = "history: unavailable (diffs are in-memory only)"
	}

	if _, err := io.WriteString(opts.Out, enterAltScreen+hideCursor); err != nil {
		return err
	}
	defer func() { _, _ = io.WriteString(opts.Out, showCursor+leaveAltScreen) }()

	quit := watchQuitKeys(opts.Input)
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()

	// One collector for the whole loop: its per-file cache keeps steady-state
	// polls at stat-only cost for unchanged command state files.
	collector := NewCollector()
	for {
		frame, note := buildFrame(opts, collector, hist, histNote)
		if err := writeFrame(opts.Out, frame); err != nil {
			return err
		}
		if note != "" {
			histNote = note
		}
		select {
		case <-ctx.Done():
			return nil
		case <-quit:
			return nil
		case <-ticker.C:
		}
	}
}

// loadHistoryBestEffort loads the trail, degrading to an in-memory history
// when the file is unreadable — a broken trail must not block observation.
func loadHistoryBestEffort(path string, now time.Time) *History {
	h, err := LoadHistory(path, now)
	if err != nil {
		return &History{}
	}
	return h
}

// buildFrame collects one snapshot, advances the history trail, and renders
// a frame. It returns the frame plus an updated history note ("" = keep the
// current one).
func buildFrame(opts Options, collector *Collector, hist *History, histNote string) (string, string) {
	now := opts.Now()
	snap := collector.Collect(opts.MaestroDir, now)

	note := ""
	// In Once mode the trail is never persisted or mutated (read-only run);
	// in loop mode append the sample before diffing so "Δ prev" compares
	// against the previous distinct sample.
	var prev, daily *Record
	if opts.Once {
		prev = hist.Latest()
		daily = hist.Baseline(now, DailyBaselineAge)
	} else {
		before := hist.Latest()
		appended, err := hist.Append(Record{TS: now.Format(time.RFC3339), Gauges: SnapshotGauges(snap)})
		if err != nil {
			note = "history: write failed (diffs may reset on restart)"
		}
		if appended {
			prev = before
		} else {
			prev = hist.Previous()
		}
		daily = hist.Baseline(now, DailyBaselineAge)
		// The freshly appended record must not be its own baseline.
		if daily != nil && appended && daily == hist.Latest() {
			daily = hist.Previous()
		}
	}

	frame := Render(snap, Diffs{Prev: prev, Daily: daily}, RenderOptions{
		Width:    opts.Width,
		Color:    opts.Color,
		Now:      now,
		Version:  opts.Version,
		Project:  filepath.Base(filepath.Dir(opts.MaestroDir)),
		Dir:      opts.MaestroDir,
		Interval: opts.Interval,
		Note:     firstNonEmpty(note, histNote),
	})
	return frame, note
}

// writeFrame repaints in place: cursor home, erase each line's tail, erase
// everything below. No full-screen clear per tick = no flicker.
func writeFrame(w io.Writer, frame string) error {
	var b strings.Builder
	b.WriteString(cursorHome)
	lines := strings.Split(frame, "\n")
	for i, line := range lines {
		b.WriteString(line)
		b.WriteString(eraseLineRest)
		if i < len(lines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString(eraseBelow)
	_, err := io.WriteString(w, b.String())
	return err
}

// watchQuitKeys returns a channel that closes when the operator types
// "q"/"quit" (line-buffered — no raw mode, no termios/ioctl dependency).
// A nil input yields a never-closing channel.
func watchQuitKeys(in io.Reader) <-chan struct{} {
	ch := make(chan struct{})
	if in == nil {
		return ch
	}
	go func() {
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			switch strings.TrimSpace(strings.ToLower(sc.Text())) {
			case "q", "quit", "exit":
				close(ch)
				return
			}
		}
	}()
	return ch
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// DefaultHistoryPath returns the HUD-owned snapshot trail location under
// the maestro state dir. The daemon neither reads nor writes this file.
func DefaultHistoryPath(maestroDir string) string {
	return filepath.Join(maestroDir, "state", "hud_history.jsonl")
}

// FormatIntervalFlag converts a --interval seconds value into a clamped
// duration, erroring on non-positive input so typos fail loudly instead of
// silently pinning to the minimum.
func FormatIntervalFlag(seconds int) (time.Duration, error) {
	if seconds <= 0 {
		return 0, fmt.Errorf("interval must be a positive number of seconds, got %d", seconds)
	}
	d := time.Duration(seconds) * time.Second
	if d < MinInterval {
		d = MinInterval
	}
	if d > MaxInterval {
		d = MaxInterval
	}
	return d, nil
}
