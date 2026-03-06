package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  CLIError
		want string
	}{
		{"with msg", CLIError{Code: 1, Msg: "something failed"}, "something failed"},
		{"empty msg", CLIError{Code: 1}, "cli error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCLIError_ExitCode(t *testing.T) {
	tests := []struct {
		name string
		code int
		want int
	}{
		{"non-zero", 2, 2},
		{"zero defaults to 1", 0, 1},
		{"one", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce := &CLIError{Code: tt.code}
			if got := ce.ExitCode(); got != tt.want {
				t.Errorf("ExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCLIError_ErrorsAs(t *testing.T) {
	var wrapped error = &CLIError{Code: 2, Msg: "wrapped"}
	var ce *CLIError
	if !errors.As(wrapped, &ce) {
		t.Fatal("errors.As failed to extract CLIError")
	}
	if ce.Code != 2 {
		t.Errorf("Code = %d, want 2", ce.Code)
	}
}

func TestStringSliceFlag_Set(t *testing.T) {
	var s stringSliceFlag
	if err := s.Set("a"); err != nil {
		t.Fatalf("Set(a): %v", err)
	}
	if err := s.Set("b"); err != nil {
		t.Fatalf("Set(b): %v", err)
	}
	if len(s) != 2 || s[0] != "a" || s[1] != "b" {
		t.Errorf("got %v, want [a b]", s)
	}
}

func TestStringSliceFlag_String(t *testing.T) {
	tests := []struct {
		name string
		val  *stringSliceFlag
		want string
	}{
		{"nil", nil, ""},
		{"empty", &stringSliceFlag{}, ""},
		{"single", &stringSliceFlag{"a"}, "a"},
		{"multiple", &stringSliceFlag{"a", "b", "c"}, "a,b,c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.val.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestModeSetter(t *testing.T) {
	var mode string
	ms := &modeSetter{target: &mode, val: "interrupt"}

	if !ms.IsBoolFlag() {
		t.Error("IsBoolFlag() should return true")
	}

	if err := ms.Set("true"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if mode != "interrupt" {
		t.Errorf("mode = %q, want %q", mode, "interrupt")
	}
}

func TestModeSetter_LastWins(t *testing.T) {
	var mode string
	fs := newFlagSet("test")
	fs.Var(&modeSetter{target: &mode, val: "interrupt"}, "interrupt", "")
	fs.Var(&modeSetter{target: &mode, val: "clear"}, "clear", "")

	if err := fs.Parse([]string{"--interrupt", "--clear"}); err != nil {
		t.Fatal(err)
	}
	if mode != "clear" {
		t.Errorf("mode = %q, want %q (last flag wins)", mode, "clear")
	}
}

func TestNewFlagSet(t *testing.T) {
	fs := newFlagSet("test")

	// Should use ContinueOnError so errors are returned, not panicked
	err := fs.Parse([]string{"--nonexistent-flag"})
	if err == nil {
		t.Error("expected error for unknown flag, got nil")
	}
}

func TestFindMaestroDir_Found(t *testing.T) {
	root := t.TempDir()
	maestroPath := filepath.Join(root, ".maestro")
	if err := os.MkdirAll(maestroPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create nested directory structure
	leaf := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}

	// Change to leaf and find .maestro in ancestor
	orig, _ := os.Getwd()
	defer func() { _ = os.Chdir(orig) }()
	if err := os.Chdir(leaf); err != nil {
		t.Fatal(err)
	}

	got, err := findMaestroDir()
	if err != nil {
		t.Fatalf("findMaestroDir() returned error: %v", err)
	}
	// Resolve symlinks for reliable comparison (macOS /tmp -> /private/tmp)
	wantResolved, _ := filepath.EvalSymlinks(maestroPath)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("findMaestroDir() = %q, want %q", gotResolved, wantResolved)
	}
}

func TestFindMaestroDir_NotFound(t *testing.T) {
	root := t.TempDir()
	// No .maestro directory anywhere

	orig, _ := os.Getwd()
	defer func() { _ = os.Chdir(orig) }()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	got, err := findMaestroDir()
	if err != nil {
		t.Fatalf("findMaestroDir() returned error: %v", err)
	}
	if got != "" {
		t.Errorf("findMaestroDir() = %q, want empty string", got)
	}
}

func TestFindMaestroDir_InCurrentDir(t *testing.T) {
	root := t.TempDir()
	maestroPath := filepath.Join(root, ".maestro")
	if err := os.MkdirAll(maestroPath, 0o755); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	defer func() { _ = os.Chdir(orig) }()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	got, err := findMaestroDir()
	if err != nil {
		t.Fatalf("findMaestroDir() returned error: %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(maestroPath)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("findMaestroDir() = %q, want %q", gotResolved, wantResolved)
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"maestro", "nonexistent-command"}
	code := run()
	if code != 1 {
		t.Errorf("run() = %d, want 1 for unknown command", code)
	}
}

func TestRun_NoArgs(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"maestro"}
	code := run()
	if code != 1 {
		t.Errorf("run() = %d, want 1 for no args", code)
	}
}

func TestRun_Version(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"maestro", "version"}
	code := run()
	if code != 0 {
		t.Errorf("run() = %d, want 0 for version", code)
	}
}

func TestRun_Help(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			os.Args = []string{"maestro", arg}
			code := run()
			if code != 0 {
				t.Errorf("run(%s) = %d, want 0", arg, code)
			}
		})
	}
}
