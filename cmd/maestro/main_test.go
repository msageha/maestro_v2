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

// TestExitCodes_NoCollision pins the numeric values of every CLI exit code
// constant. Callers (worker shell wrappers, ops scripts, downstream
// tooling) branch on `$?`, so accidentally renumbering one of these is a
// silent break. Update this test deliberately when the contract changes.
func TestExitCodes_NoCollision(t *testing.T) {
	t.Parallel()
	codes := map[string]int{
		"ExitCodeRetryable":          ExitCodeRetryable,          // 2
		"ExitCodeSubmitUncertain":    ExitCodeSubmitUncertain,    // 3
		"ExitCodeFencingEpoch":       ExitCodeFencingEpoch,       // 10
		"ExitCodeMaxRuntimeExceeded": ExitCodeMaxRuntimeExceeded, // 11
		"ExitCodeFencingStatus":      ExitCodeFencingStatus,      // 12
	}
	want := map[string]int{
		"ExitCodeRetryable":          2,
		"ExitCodeSubmitUncertain":    3,
		"ExitCodeFencingEpoch":       10,
		"ExitCodeMaxRuntimeExceeded": 11,
		"ExitCodeFencingStatus":      12,
	}
	seen := make(map[int]string)
	for name, code := range codes {
		if w, ok := want[name]; ok && code != w {
			t.Errorf("%s = %d, want %d (renumbering this is a downstream break)", name, code, w)
		}
		if existing, dup := seen[code]; dup {
			t.Errorf("exit code %d is shared by %s and %s — collisions confuse `$?` branches",
				code, existing, name)
		}
		seen[code] = name
	}
}

func TestCLIError_ErrorsAs(t *testing.T) {
	var wrapped error = &CLIError{Code: ExitCodeRetryable, Msg: "wrapped"}
	var ce *CLIError
	if !errors.As(wrapped, &ce) {
		t.Fatal("errors.As failed to extract CLIError")
	}
	if ce.Code != ExitCodeRetryable {
		t.Errorf("Code = %d, want %d", ce.Code, ExitCodeRetryable)
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
	t.Chdir(leaf)

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

	t.Chdir(root)

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

	t.Chdir(root)

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

func TestFindMaestroDir_UsesEnvOverCwd(t *testing.T) {
	cwdRoot := t.TempDir()
	cwdMaestroPath := filepath.Join(cwdRoot, ".maestro")
	if err := os.MkdirAll(cwdMaestroPath, 0o755); err != nil {
		t.Fatal(err)
	}
	envRoot := t.TempDir()
	envMaestroPath := filepath.Join(envRoot, ".maestro")
	if err := os.MkdirAll(envMaestroPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(cwdRoot)
	t.Setenv(maestroDirEnv, envMaestroPath)

	got, err := findMaestroDir()
	if err != nil {
		t.Fatalf("findMaestroDir() returned error: %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(envMaestroPath)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("findMaestroDir() = %q, want %q", gotResolved, wantResolved)
	}
}

func TestFindMaestroDir_EnvMissingReturnsError(t *testing.T) {
	t.Setenv(maestroDirEnv, filepath.Join(t.TempDir(), ".maestro"))

	got, err := findMaestroDir()
	if err == nil {
		t.Fatal("expected error for missing MAESTRO_DIR")
	}
	if got != "" {
		t.Errorf("findMaestroDir() = %q, want empty string on error", got)
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	t.Parallel()
	code := newCLIApp().run([]string{"nonexistent-command"})
	if code != 1 {
		t.Errorf("run() = %d, want 1 for unknown command", code)
	}
}

func TestRun_NoArgs(t *testing.T) {
	t.Parallel()
	code := newCLIApp().run(nil)
	if code != 1 {
		t.Errorf("run() = %d, want 1 for no args", code)
	}
}

func TestRun_Version(t *testing.T) {
	t.Parallel()
	code := newCLIApp().run([]string{"version"})
	if code != 0 {
		t.Errorf("run() = %d, want 0 for version", code)
	}
}

func TestRun_Help(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			t.Parallel()
			code := newCLIApp().run([]string{arg})
			if code != 0 {
				t.Errorf("run(%s) = %d, want 0", arg, code)
			}
		})
	}
}

// TestNormalizeProcessEnvironment_PromotesDumbTERM verifies that TERM=dumb
// (or unset) is promoted to a real terminfo entry, while operator overrides
// win — explicit non-empty, non-"dumb" values are preserved.
func TestNormalizeProcessEnvironment_PromotesDumbTERM(t *testing.T) {
	cases := []struct {
		name      string
		set       bool
		input     string
		wantTerm  string
		wantUnset bool
	}{
		{name: "dumb_promoted", set: true, input: "dumb", wantTerm: "xterm-256color"},
		{name: "unset_promoted", set: false, wantTerm: "xterm-256color"},
		{name: "explicit_value_preserved", set: true, input: "screen-256color", wantTerm: "screen-256color"},
		{name: "explicit_xterm_preserved", set: true, input: "xterm-color", wantTerm: "xterm-color"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("TERM", tc.input)
			} else {
				// t.Setenv only sets; manually unset for the unset case.
				orig, was := os.LookupEnv("TERM")
				_ = os.Unsetenv("TERM")
				t.Cleanup(func() {
					if was {
						_ = os.Setenv("TERM", orig)
					}
				})
			}

			normalizeProcessEnvironment()

			if got := os.Getenv("TERM"); got != tc.wantTerm {
				t.Errorf("TERM = %q, want %q", got, tc.wantTerm)
			}
		})
	}
}
