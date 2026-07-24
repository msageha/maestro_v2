package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/msageha/maestro_v2/internal/hud"
)

// runHUD starts the read-only TUI observation HUD (issue #29). It reads
// `.maestro/` (state/queue/results/dead_letters/quarantine) and renders a
// live board; the only file it ever writes is its own snapshot-diff trail
// (state/hud_history.jsonl), and `--once` / `--no-history` suppress even
// that. It carries no control-plane operation and works with or without a
// running daemon.
func runHUD(args []string) error {
	cmd := NewCommand("maestro hud",
		"maestro hud [--interval <sec>] [--width <cols>] [--once] [--no-color] [--no-history]")
	var (
		intervalSec int
		width       int
		once        bool
		noColor     bool
		noHistory   bool
	)
	cmd.IntVar(&intervalSec, "interval", 2, "Poll interval in seconds (clamped to 1-10)")
	cmd.IntVar(&width, "width", 0, "Render width in columns (default: $COLUMNS or 100)")
	cmd.BoolVar(&once, "once", false, "Render a single frame to stdout and exit (no history write)")
	cmd.BoolVar(&noColor, "no-color", false, "Disable ANSI colors (NO_COLOR env is also honored)")
	cmd.BoolVar(&noHistory, "no-history", false, "Do not persist the snapshot-diff trail (in-memory diffs only)")
	if err := cmd.Parse(args); err != nil {
		return err
	}

	maestroDir, err := requireMaestroDir("hud")
	if err != nil {
		return err
	}

	interval, err := hud.FormatIntervalFlag(intervalSec)
	if err != nil {
		return cmd.UsageErrorf("%v", err)
	}

	if !once && !isStdoutTerminal() {
		return cmd.Errorf("stdout is not a terminal; use --once for a single non-interactive frame")
	}

	historyPath := ""
	if !noHistory {
		historyPath = hud.DefaultHistoryPath(maestroDir)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := hud.Options{
		MaestroDir:  maestroDir,
		Out:         os.Stdout,
		Input:       os.Stdin,
		Interval:    interval,
		Width:       resolveHUDWidth(width),
		Color:       !noColor && os.Getenv("NO_COLOR") == "",
		Once:        once,
		HistoryPath: historyPath,
		Version:     version,
	}
	if err := hud.Run(ctx, opts); err != nil {
		return fmt.Errorf("maestro hud: %w", err)
	}
	return nil
}

// resolveHUDWidth picks the render width: explicit flag > $COLUMNS (tmux
// and most shells export it) > conservative default. No ioctl / external
// terminal library is used by design (stdlib-only constraint).
func resolveHUDWidth(flagWidth int) int {
	if flagWidth > 0 {
		return clampHUDWidth(flagWidth)
	}
	if cols, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && cols > 0 {
		return clampHUDWidth(cols)
	}
	return 100
}

// clampHUDWidth bounds the width so degenerate values (COLUMNS=2, a typo'd
// --width 9999) still produce a readable frame.
func clampHUDWidth(w int) int {
	const minWidth, maxWidth = 60, 240
	if w < minWidth {
		return minWidth
	}
	if w > maxWidth {
		return maxWidth
	}
	return w
}

// isStdoutTerminal reports whether stdout is an interactive terminal.
// Declared as a variable so tests can force the non-tty branch.
var isStdoutTerminal = func() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
