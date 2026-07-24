package hud

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRun_OnceRendersSingleFrameWithoutWrites(t *testing.T) {
	dir := newFixtureMaestroDir(t)
	histPath := DefaultHistoryPath(dir)

	var out bytes.Buffer
	err := Run(context.Background(), Options{
		MaestroDir:  dir,
		Out:         &out,
		Once:        true,
		HistoryPath: histPath,
		Interval:    2 * time.Second,
		Width:       120,
		Version:     "test",
		Now:         func() time.Time { return fixtureTime },
	})
	if err != nil {
		t.Fatalf("Run(once): %v", err)
	}

	frame := out.String()
	if !strings.Contains(frame, "MAESTRO HUD") || !strings.Contains(frame, "cmd_1") {
		t.Errorf("once frame incomplete:\n%s", frame)
	}
	if strings.Contains(frame, enterAltScreen) || strings.Contains(frame, hideCursor) {
		t.Error("once mode must not switch terminal modes")
	}
	if _, err := os.Stat(histPath); !os.IsNotExist(err) {
		t.Error("once mode must not create the history file (fully read-only)")
	}
}

func TestRun_LoopEntersAndRestoresAltScreen(t *testing.T) {
	dir := newFixtureMaestroDir(t)
	histPath := DefaultHistoryPath(dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // exit after the first frame

	var out bytes.Buffer
	err := Run(ctx, Options{
		MaestroDir:  dir,
		Out:         &out,
		HistoryPath: histPath,
		Interval:    time.Second,
		Width:       120,
		Version:     "test",
		Now:         func() time.Time { return fixtureTime },
	})
	if err != nil {
		t.Fatalf("Run(loop): %v", err)
	}

	s := out.String()
	if !strings.HasPrefix(s, enterAltScreen+hideCursor) {
		t.Error("loop must enter the alternate screen and hide the cursor first")
	}
	if !strings.HasSuffix(s, showCursor+leaveAltScreen) {
		t.Error("loop must restore cursor and leave the alternate screen on exit")
	}
	if !strings.Contains(s, "MAESTRO HUD") {
		t.Error("loop must have rendered at least one frame")
	}
	if _, err := os.Stat(histPath); err != nil {
		t.Errorf("loop mode should persist the history trail: %v", err)
	}
}

func TestRun_QuitKeyStopsLoop(t *testing.T) {
	dir := newFixtureMaestroDir(t)

	in, inW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			MaestroDir: dir,
			Out:        io.Discard,
			Input:      in,
			Interval:   time.Second,
			Width:      100,
			Now:        func() time.Time { return fixtureTime },
		})
	}()

	if _, err := inW.Write([]byte("q\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after the quit key")
	}
	_ = inW.Close()
}

func TestFormatIntervalFlag(t *testing.T) {
	if _, err := FormatIntervalFlag(0); err == nil {
		t.Error("zero interval must be rejected")
	}
	if _, err := FormatIntervalFlag(-3); err == nil {
		t.Error("negative interval must be rejected")
	}
	if d, err := FormatIntervalFlag(2); err != nil || d != 2*time.Second {
		t.Errorf("interval 2 = %v, %v", d, err)
	}
	if d, _ := FormatIntervalFlag(120); d != MaxInterval {
		t.Errorf("interval 120 should clamp to %v, got %v", MaxInterval, d)
	}
}

func TestRun_DaemonAbsentStillRenders(t *testing.T) {
	// A bare directory (daemon never ran, no .maestro content) must still
	// produce a frame — the HUD waits instead of failing.
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		MaestroDir: t.TempDir(),
		Out:        &out,
		Once:       true,
		Width:      100,
		Now:        func() time.Time { return fixtureTime },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "unavailable") {
		t.Errorf("frame should degrade to unavailable sections:\n%s", out.String())
	}
}
