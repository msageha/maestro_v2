package main

import (
	"errors"
	"strings"
	"testing"
)

func withStdoutTerminal(t *testing.T, isTTY bool) {
	t.Helper()
	orig := isStdoutTerminal
	isStdoutTerminal = func() bool { return isTTY }
	t.Cleanup(func() { isStdoutTerminal = orig })
}

func TestRunHUD_HelpExitsZero(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			if err := runHUD([]string{arg}); !errors.Is(err, errHelpRequested) {
				t.Errorf("runHUD(%s) = %v, want errHelpRequested", arg, err)
			}
		})
	}
}

func TestRunHUD_RequiresMaestroDir(t *testing.T) {
	dir := t.TempDir() // no .maestro anywhere above
	t.Chdir(dir)
	t.Setenv("MAESTRO_DIR", "")

	err := runHUD([]string{"--once"})
	if err == nil {
		t.Fatal("expected error without .maestro directory")
	}
	if !strings.Contains(err.Error(), ".maestro") {
		t.Errorf("error should mention .maestro, got: %v", err)
	}
}

func TestRunHUD_InvalidIntervalRejected(t *testing.T) {
	withMaestroDir(t)
	err := runHUD([]string{"--interval", "0", "--once"})
	if err == nil {
		t.Fatal("expected error for --interval 0")
	}
	if !strings.Contains(err.Error(), "interval") {
		t.Errorf("error should mention interval, got: %v", err)
	}
}

func TestRunHUD_UnknownFlagRejected(t *testing.T) {
	withMaestroDir(t)
	if err := runHUD([]string{"--bogus"}); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestRunHUD_NonTTYWithoutOnceFails(t *testing.T) {
	withMaestroDir(t)
	withStdoutTerminal(t, false)

	err := runHUD(nil)
	if err == nil {
		t.Fatal("expected error when stdout is not a terminal")
	}
	if !strings.Contains(err.Error(), "--once") {
		t.Errorf("error should guide to --once, got: %v", err)
	}
}

func TestClampHUDWidth(t *testing.T) {
	cases := []struct{ in, want int }{
		{10, 60},
		{60, 60},
		{100, 100},
		{9999, 240},
	}
	for _, tc := range cases {
		if got := clampHUDWidth(tc.in); got != tc.want {
			t.Errorf("clampHUDWidth(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestResolveHUDWidth(t *testing.T) {
	t.Setenv("COLUMNS", "")
	if got := resolveHUDWidth(0); got != 100 {
		t.Errorf("default width = %d, want 100", got)
	}
	if got := resolveHUDWidth(80); got != 80 {
		t.Errorf("flag width = %d, want 80", got)
	}
	t.Setenv("COLUMNS", "132")
	if got := resolveHUDWidth(0); got != 132 {
		t.Errorf("COLUMNS width = %d, want 132", got)
	}
	t.Setenv("COLUMNS", "not-a-number")
	if got := resolveHUDWidth(0); got != 100 {
		t.Errorf("bad COLUMNS should fall back to 100, got %d", got)
	}
}
