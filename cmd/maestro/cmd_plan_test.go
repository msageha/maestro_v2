package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

func TestRunPlan_NoSubcommand(t *testing.T) {
	err := newCLIApp().runPlan(nil)
	if err == nil {
		t.Fatal("expected error for missing subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunPlan_UnknownSubcommand(t *testing.T) {
	err := newCLIApp().runPlan([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunPlanSubmit_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"missing command-id", []string{}, true},
		{"unknown flag", []string{"--unknown"}, true},
		{"unexpected arg", []string{"--command-id", "c1", "extra"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCLIApp().runPlanSubmit(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				var ce *CLIError
				if !errors.As(err, &ce) {
					t.Fatalf("expected CLIError, got %T: %v", err, err)
				}
			}
		})
	}
}

func TestRunPlanUnquarantine_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing command-id", []string{}},
		{"invalid command-id", []string{"--command-id", "../bad"}},
		{"unexpected arg", []string{"--command-id", "cmd_1", "extra"}},
		{"unknown flag", []string{"--unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCLIApp().runPlanUnquarantine(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
		})
	}
}

// TestRunPlanUnquarantine_RejectedForPlanner verifies that the CLI layer
// rejects plan unquarantine invocations originating from the Planner role
// before the request ever reaches the daemon. This is a multi-layer defense
// that complements the daemon-side caller_role check in
// internal/daemon/plan_handler.go handlePlan.
func TestRunPlanUnquarantine_RejectedForPlanner(t *testing.T) {
	t.Setenv(uds.CallerRoleEnv, uds.RolePlanner)

	// Use a mock client so a successful regression (missing guard) would
	// surface as "daemon was called" rather than a socket error.
	called := false
	app := newTestApp(&mockUDSClient{
		sendCommandContextFunc: func(_ context.Context, _ string, _ any) (*uds.Response, error) {
			called = true
			return successResponse(nil), nil
		},
	})
	withMaestroDir(t)

	err := app.runPlanUnquarantine([]string{"--command-id", "cmd_1"})
	if err == nil {
		t.Fatal("expected error when invoked with Planner role")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if ce.ExitCode() == 0 {
		t.Errorf("expected non-zero exit code, got %d", ce.ExitCode())
	}
	if !containsStr(ce.Msg, "planner") && !containsStr(ce.Msg, "Planner") {
		t.Errorf("expected rejection message to mention Planner, got: %s", ce.Msg)
	}
	if !containsStr(ce.Msg, "multi-layer defense") {
		t.Errorf("expected rejection message to mention 'multi-layer defense', got: %s", ce.Msg)
	}
	if called {
		t.Error("daemon must not be contacted when CLI-layer guard rejects Planner")
	}
}

// TestRunPlanUnquarantine_OperatorRoleReachesDaemon is a regression guard:
// with the default/operator caller role (MAESTRO_AGENT_ROLE unset) the
// unquarantine request must still be forwarded to the daemon.
func TestRunPlanUnquarantine_OperatorRoleReachesDaemon(t *testing.T) {
	// Explicitly clear any inherited role so the caller is treated as
	// direct CLI / operator.
	t.Setenv(uds.CallerRoleEnv, "")
	t.Setenv("TMUX_PANE", "")
	withMaestroDir(t)

	called := false
	app := newTestApp(&mockUDSClient{
		sendCommandContextFunc: func(_ context.Context, cmd string, _ any) (*uds.Response, error) {
			called = true
			if cmd != "plan" {
				t.Errorf("expected cmd=plan, got %q", cmd)
			}
			return successResponse(nil), nil
		},
	})

	if err := app.runPlanUnquarantine([]string{"--command-id", "cmd_1"}); err != nil {
		t.Fatalf("unexpected error for operator role: %v", err)
	}
	if !called {
		t.Fatal("expected daemon SendCommandContext to be called for operator role")
	}
}

// TestRunPlanUnquarantine_OrchestratorRoleReachesDaemon ensures the CLI
// guard is narrowly scoped to the Planner role and does not inadvertently
// block other recovery-eligible roles (orchestrator, cli).
func TestRunPlanUnquarantine_OrchestratorRoleReachesDaemon(t *testing.T) {
	t.Setenv(uds.CallerRoleEnv, uds.RoleOrchestrator)
	withMaestroDir(t)

	called := false
	app := newTestApp(&mockUDSClient{
		sendCommandContextFunc: func(_ context.Context, _ string, _ any) (*uds.Response, error) {
			called = true
			return successResponse(nil), nil
		},
	})

	if err := app.runPlanUnquarantine([]string{"--command-id", "cmd_1"}); err != nil {
		t.Fatalf("unexpected error for orchestrator role: %v", err)
	}
	if !called {
		t.Fatal("expected daemon to be called for orchestrator role")
	}
}

func TestRunPlanResumeMerge_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing command-id", []string{}},
		{"invalid command-id", []string{"--command-id", "../bad"}},
		{"unexpected arg", []string{"--command-id", "cmd_1", "extra"}},
		{"unknown flag", []string{"--unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCLIApp().runPlanResumeMerge(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
		})
	}
}

func TestRunPlanRetryPublish_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing command-id", []string{}},
		{"invalid command-id", []string{"--command-id", "../bad"}},
		{"unexpected arg", []string{"--command-id", "cmd_1", "extra"}},
		{"unknown flag", []string{"--unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCLIApp().runPlanRetryPublish(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
		})
	}
}

func TestRunPlanRecover_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing command-id", []string{}},
		{"invalid command-id", []string{"--command-id", "../bad"}},
		{"unexpected arg", []string{"--command-id", "cmd_1", "extra"}},
		{"unknown flag", []string{"--unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCLIApp().runPlanRecover(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
		})
	}
}

func TestRunResolveConflict_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing command-id", []string{"--phase-id", "p1", "--worker-id", "worker1"}},
		{"invalid command-id", []string{"--command-id", "../bad", "--phase-id", "p1", "--worker-id", "worker1"}},
		{"missing phase-id", []string{"--command-id", "cmd_1", "--worker-id", "worker1"}},
		{"invalid phase-id", []string{"--command-id", "cmd_1", "--phase-id", "../bad", "--worker-id", "worker1"}},
		{"missing worker-id", []string{"--command-id", "cmd_1", "--phase-id", "p1"}},
		{"invalid worker-id", []string{"--command-id", "cmd_1", "--phase-id", "p1", "--worker-id", "../bad"}},
		{"unexpected arg", []string{"--command-id", "cmd_1", "--phase-id", "p1", "--worker-id", "worker1", "extra"}},
		{"unknown flag", []string{"--unknown"}},
		{"conflicting-files without other flags", []string{"--conflicting-files", "a.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCLIApp().runResolveConflict(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
		})
	}
}

func TestRunResolveConflict_PhaseIDValidation(t *testing.T) {
	// Run in a temp directory without .maestro/ so that valid phase IDs
	// hit the "missing .maestro dir" CLIError instead of attempting a
	// daemon socket connection.
	t.Chdir(t.TempDir())

	tests := []struct {
		name        string
		phaseID     string
		wantInvalid bool // true if "invalid --phase-id" error is expected
	}{
		{"internal phase id __implicit_phase", "__implicit_phase", false},
		{"regular phase id", "phase1", false},
		{"regular phase id with dots", "phase.1.2", false},
		{"invalid phase-id with traversal", "../bad", true},
		{"invalid phase-id with special chars", "phase@!", true},
		{"invalid internal id with uppercase", "__ImplicitPhase", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{
				"--command-id", "cmd_1",
				"--phase-id", tt.phaseID,
				"--worker-id", "worker1",
			}
			err := newCLIApp().runResolveConflict(args)
			if err == nil {
				t.Fatal("expected error (at least from missing .maestro dir)")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
			hasInvalidPhase := containsStr(ce.Msg, "invalid --phase-id")
			if tt.wantInvalid && !hasInvalidPhase {
				t.Errorf("expected 'invalid --phase-id' error, got: %s", ce.Msg)
			}
			if !tt.wantInvalid && hasInvalidPhase {
				t.Errorf("did not expect 'invalid --phase-id' error, got: %s", ce.Msg)
			}
		})
	}
}

func TestRunPlan_RecoverySubcommandsRouted(t *testing.T) {
	// Sanity: runPlan should accept the new subcommand names without
	// returning "unknown subcommand". They will fail flag parsing instead.
	for _, sub := range []string{"unquarantine", "resume-merge", "recover"} {
		err := newCLIApp().runPlan([]string{sub})
		if err == nil {
			t.Fatalf("%s: expected error", sub)
		}
		if msg := err.Error(); containsAny(msg, "unknown subcommand") {
			t.Fatalf("%s: was rejected as unknown subcommand: %v", sub, err)
		}
	}
}

func containsAny(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRunPlanComplete_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"missing command-id", []string{}, true},
		{"unknown flag", []string{"--unknown"}, true},
		{"unexpected arg", []string{"--command-id", "c1", "extra"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCLIApp().runPlanComplete(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				var ce *CLIError
				if !errors.As(err, &ce) {
					t.Fatalf("expected CLIError, got %T: %v", err, err)
				}
			}
		})
	}
}

func TestRunPlanAddRetryTask_MissingFlags(t *testing.T) {
	err := newCLIApp().runPlanAddRetryTask([]string{"--command-id", "c1"})
	if err == nil {
		t.Fatal("expected error for missing required flags")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunPlanAddRetryTask_ValidateID(t *testing.T) {
	allRequiredFlags := func(overrides map[string]string) []string {
		defaults := map[string]string{
			"--command-id":          "valid-cmd",
			"--retry-of":            "valid-task",
			"--purpose":             "test purpose",
			"--content":             "test content",
			"--acceptance-criteria": "test criteria",
			"--bloom-level":         "3",
			"--expected-paths":      "internal/example.go",
		}
		for k, v := range overrides {
			defaults[k] = v
		}
		var args []string
		for k, v := range defaults {
			args = append(args, k, v)
		}
		return args
	}

	tests := []struct {
		name    string
		args    []string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "invalid command-id with path traversal",
			args:    allRequiredFlags(map[string]string{"--command-id": "../evil"}),
			wantErr: true,
			errMsg:  "invalid --command-id",
		},
		{
			name:    "invalid retry-of with path traversal",
			args:    allRequiredFlags(map[string]string{"--retry-of": "../evil"}),
			wantErr: true,
			errMsg:  "invalid --retry-of",
		},
		{
			name:    "bloom-level too low",
			args:    allRequiredFlags(map[string]string{"--bloom-level": "0"}),
			wantErr: true,
			errMsg:  "all required flags must be set",
		},
		{
			name:    "bloom-level too high",
			args:    allRequiredFlags(map[string]string{"--bloom-level": "7"}),
			wantErr: true,
			errMsg:  "--bloom-level must be between 1 and 6",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCLIApp().runPlanAddRetryTask(tt.args)
			if !tt.wantErr {
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
			if tt.errMsg != "" {
				if got := ce.Msg; !containsStr(got, tt.errMsg) {
					t.Errorf("error message %q does not contain %q", got, tt.errMsg)
				}
			}
		})
	}
}

func TestRunPlanAddRetryTask_InvalidBlockedBy(t *testing.T) {
	args := []string{
		"--command-id", "valid-cmd",
		"--retry-of", "valid-task",
		"--purpose", "p",
		"--content", "c",
		"--acceptance-criteria", "ac",
		"--bloom-level", "3",
		"--expected-paths", "internal/example.go",
		"--blocked-by", "../evil",
	}
	err := newCLIApp().runPlanAddRetryTask(args)
	if err == nil {
		t.Fatal("expected error for invalid blocked-by")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && stringContains(s, substr))
}

// withStdin swaps os.Stdin for a pipe carrying input for the duration of
// fn. Tests that exercise readFlagInputFile via the dash form rely on
// this helper because the production code reads os.Stdin directly to
// match the `plan submit --tasks-file -` shape Planner agents already
// trust. The pipe writer runs on a goroutine so a write larger than the
// kernel pipe buffer does not deadlock.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = w.Write([]byte(input))
		_ = w.Close()
	}()
	fn()
	<-done
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestRunPlanRequestCancel_MissingCommandID(t *testing.T) {
	err := newCLIApp().runPlanRequestCancel(nil)
	if err == nil {
		t.Fatal("expected error for missing command-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunPlanSubmit_InvalidCommandID(t *testing.T) {
	err := newCLIApp().runPlanSubmit([]string{"--command-id", "../evil"})
	if err == nil {
		t.Fatal("expected error for invalid command-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !containsStr(ce.Msg, "invalid --command-id") {
		t.Errorf("expected 'invalid --command-id' in error, got: %s", ce.Msg)
	}
}

func TestRunPlanComplete_InvalidCommandID(t *testing.T) {
	err := newCLIApp().runPlanComplete([]string{"--command-id", "../evil"})
	if err == nil {
		t.Fatal("expected error for invalid command-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !containsStr(ce.Msg, "invalid --command-id") {
		t.Errorf("expected 'invalid --command-id' in error, got: %s", ce.Msg)
	}
}

func TestRunPlanComplete_SummaryTooLong(t *testing.T) {
	longSummary := make([]byte, 65537)
	for i := range longSummary {
		longSummary[i] = 'x'
	}
	err := newCLIApp().runPlanComplete([]string{"--command-id", "valid-cmd", "--summary", string(longSummary)})
	if err == nil {
		t.Fatal("expected error for oversized summary")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !containsStr(ce.Msg, "exceeds maximum size") {
		t.Errorf("expected 'exceeds maximum size' in error, got: %s", ce.Msg)
	}
}

// TestRunPlanComplete_SummaryFile pins the --summary-file flag, mirroring the
// (--content | --content-file) shape from plan add-task. It validates: file
// content is loaded, --summary and --summary-file are mutually exclusive, and
// a missing file produces a clear error rather than silently sending an empty
// summary to the daemon.
func TestRunPlanComplete_SummaryFile(t *testing.T) {
	t.Run("loads_summary_from_file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "summary.txt")
		if err := os.WriteFile(path, []byte("multi\nline\nsummary"), 0o600); err != nil {
			t.Fatalf("write summary file: %v", err)
		}
		// runPlanComplete attempts a daemon RPC; we can't fully run it
		// without a daemon. Use resolveSummaryFile directly to validate
		// the flag parsing layer (the same helper runPlanComplete invokes).
		var summary string
		if err := resolveSummaryFile(NewCommand("test", "test"), &summary, path); err != nil {
			t.Fatalf("resolveSummaryFile returned error: %v", err)
		}
		if summary != "multi\nline\nsummary" {
			t.Errorf("summary = %q, want loaded file content", summary)
		}
	})

	t.Run("rejects_summary_and_summary_file_together", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "summary.txt")
		if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
			t.Fatalf("write summary file: %v", err)
		}
		summary := "inline-value"
		err := resolveSummaryFile(NewCommand("test", "test"), &summary, path)
		if err == nil {
			t.Fatal("expected error when both --summary and --summary-file are set")
		}
		if !containsStr(err.Error(), "mutually exclusive") {
			t.Errorf("error %q does not mention mutual exclusion", err.Error())
		}
	})

	t.Run("missing_file_surfaces_read_error", func(t *testing.T) {
		var summary string
		err := resolveSummaryFile(NewCommand("test", "test"), &summary, "/non/existent/path")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
		if !containsStr(err.Error(), "read --summary-file") {
			t.Errorf("error %q does not mention read --summary-file", err.Error())
		}
	})

	t.Run("empty_summaryfile_path_is_noop", func(t *testing.T) {
		summary := "preserved"
		if err := resolveSummaryFile(NewCommand("test", "test"), &summary, ""); err != nil {
			t.Fatalf("expected no error when --summary-file is empty, got: %v", err)
		}
		if summary != "preserved" {
			t.Errorf("summary = %q, want untouched value", summary)
		}
	})

	// "-" and "/dev/stdin" must consume os.Stdin directly so Planner agents
	// piping long summaries via `--summary-file -` succeed without a
	// temp-file fallback.
	t.Run("dash_reads_stdin", func(t *testing.T) {
		withStdin(t, "piped\nsummary\n", func() {
			var summary string
			if err := resolveSummaryFile(NewCommand("test", "test"), &summary, "-"); err != nil {
				t.Fatalf("resolveSummaryFile(-): %v", err)
			}
			if summary != "piped\nsummary\n" {
				t.Errorf("summary = %q, want stdin contents", summary)
			}
		})
	})

	t.Run("dev_stdin_reads_stdin", func(t *testing.T) {
		withStdin(t, "alt-stdin", func() {
			var summary string
			if err := resolveSummaryFile(NewCommand("test", "test"), &summary, "/dev/stdin"); err != nil {
				t.Fatalf("resolveSummaryFile(/dev/stdin): %v", err)
			}
			if summary != "alt-stdin" {
				t.Errorf("summary = %q, want /dev/stdin contents", summary)
			}
		})
	})

	t.Run("stdin_overflow_is_rejected", func(t *testing.T) {
		oversized := make([]byte, model.DefaultMaxEntryContentBytes+1)
		for i := range oversized {
			oversized[i] = 'x'
		}
		withStdin(t, string(oversized), func() {
			var summary string
			err := resolveSummaryFile(NewCommand("test", "test"), &summary, "-")
			if err == nil {
				t.Fatal("expected overflow error from stdin reader")
			}
			if !containsStr(err.Error(), "exceeds maximum size") {
				t.Errorf("error = %q, want overflow message", err.Error())
			}
		})
	})
}

// TestBuildDefinitionOfAbort_RejectsNonPositive ensures the definition_of_abort
// CLI flags reject explicit zero or negative values. docs/requirements/REQUIREMENTS.md §S2-2
// makes max_repair_count / max_wall_clock_sec hard stops, so a typo such as
// `--max-repair-count 0` must surface immediately rather than silently fall
// through to the model defaults.
func TestBuildDefinitionOfAbort_RejectsNonPositive(t *testing.T) {
	cases := []struct {
		name           string
		maxRepairCount int
		maxWallClock   int
		wantErrFrag    string
	}{
		{"repair_zero", 0, dabUnset, "--max-repair-count must be a positive integer"},
		{"repair_negative", -5, dabUnset, "--max-repair-count must be a positive integer"},
		{"wallclock_zero", dabUnset, 0, "--max-wall-clock-sec must be a positive integer"},
		{"wallclock_negative", dabUnset, -10, "--max-wall-clock-sec must be a positive integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doa, err := buildDefinitionOfAbort(tc.maxRepairCount, tc.maxWallClock, nil)
			if err == nil {
				t.Fatalf("expected error, got doa=%+v", doa)
			}
			if !containsStr(err.Error(), tc.wantErrFrag) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrFrag)
			}
		})
	}
}

// TestBuildDefinitionOfAbort_UnsetInheritsDefaults verifies the dabUnset
// sentinel falls through to the model defaults — this is the path taken when
// the user does not pass the flag at all and is the contract the help text
// promises.
func TestBuildDefinitionOfAbort_UnsetInheritsDefaults(t *testing.T) {
	doa, err := buildDefinitionOfAbort(dabUnset, dabUnset, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doa.MaxRepairCount <= 0 || doa.MaxWallClockSec <= 0 {
		t.Errorf("defaults must be positive, got %+v", doa)
	}
}

// TestBuildDefinitionOfAbort_PositiveOverrides confirms an explicit positive
// value flows through unchanged, so legitimate Planner-driven overrides are
// not blocked by the new validation.
func TestBuildDefinitionOfAbort_PositiveOverrides(t *testing.T) {
	doa, err := buildDefinitionOfAbort(7, 1234, []string{"boom"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doa.MaxRepairCount != 7 {
		t.Errorf("MaxRepairCount = %d, want 7", doa.MaxRepairCount)
	}
	if doa.MaxWallClockSec != 1234 {
		t.Errorf("MaxWallClockSec = %d, want 1234", doa.MaxWallClockSec)
	}
}

// TestRunPlanAddTask_RejectsZeroMaxRepairCount drives the rejection through
// the actual CLI entrypoint to prove the wiring (sentinel default → validation
// → CLIError) holds end-to-end, not just at the helper level.
func TestRunPlanAddTask_RejectsZeroMaxRepairCount(t *testing.T) {
	args := []string{
		"--command-id", "valid-cmd",
		"--purpose", "p",
		"--content", "c",
		"--acceptance-criteria", "ac",
		"--bloom-level", "3",
		"--expected-paths", "internal/example.go",
		"--max-repair-count", "0",
	}
	err := newCLIApp().runPlanAddTask(args)
	if err == nil {
		t.Fatal("expected error for --max-repair-count 0")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !containsStr(ce.Msg, "--max-repair-count must be a positive integer") {
		t.Errorf("error message %q missing expected text", ce.Msg)
	}
}

// TestRunPlanAddRetryTask_RejectsNegativeWallClock mirrors the above for the
// add-retry-task path so both CLI surfaces share the safety net.
func TestRunPlanAddRetryTask_RejectsNegativeWallClock(t *testing.T) {
	args := []string{
		"--command-id", "valid-cmd",
		"--retry-of", "valid-task",
		"--purpose", "p",
		"--content", "c",
		"--acceptance-criteria", "ac",
		"--bloom-level", "3",
		"--expected-paths", "internal/example.go",
		"--max-wall-clock-sec=-2",
	}
	err := newCLIApp().runPlanAddRetryTask(args)
	if err == nil {
		t.Fatal("expected error for --max-wall-clock-sec -1")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !containsStr(ce.Msg, "--max-wall-clock-sec must be a positive integer") {
		t.Errorf("error message %q missing expected text", ce.Msg)
	}
}

func TestRunPlanAddRetryTask_ContentTooLong(t *testing.T) {
	longContent := make([]byte, 65537)
	for i := range longContent {
		longContent[i] = 'x'
	}
	err := newCLIApp().runPlanAddRetryTask([]string{
		"--command-id", "valid-cmd",
		"--retry-of", "valid-task",
		"--purpose", "p",
		"--content", string(longContent),
		"--acceptance-criteria", "ac",
		"--bloom-level", "3",
		"--expected-paths", "internal/example.go",
	})
	if err == nil {
		t.Fatal("expected error for oversized content")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !containsStr(ce.Msg, "exceeds maximum size") {
		t.Errorf("expected 'exceeds maximum size' in error, got: %s", ce.Msg)
	}
}

func TestResolveContentFile_ReadsFile(t *testing.T) {
	path := t.TempDir() + "/content.txt"
	if err := os.WriteFile(path, []byte("file content\nsecond line"), 0644); err != nil {
		t.Fatalf("write content file: %v", err)
	}
	cmd := NewCommand("maestro plan add-task", "maestro plan add-task")
	content := ""
	if err := resolveContentFile(cmd, &content, path); err != nil {
		t.Fatalf("resolveContentFile: %v", err)
	}
	if content != "file content\nsecond line" {
		t.Fatalf("content = %q", content)
	}
}

func TestResolveContentFile_RejectsMixedSources(t *testing.T) {
	path := t.TempDir() + "/content.txt"
	if err := os.WriteFile(path, []byte("file content"), 0644); err != nil {
		t.Fatalf("write content file: %v", err)
	}
	cmd := NewCommand("maestro plan add-task", "maestro plan add-task")
	content := "inline"
	err := resolveContentFile(cmd, &content, path)
	if err == nil {
		t.Fatal("expected mixed source error")
	}
	if !containsStr(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %q", err.Error())
	}
}

// TestResolveAcceptanceCriteriaFile_ReadsFile pins the
// --acceptance-criteria-file flag, which mirrors --content-file so Planner
// agents do not have to argv-quote multi-line acceptance criteria values.
func TestResolveAcceptanceCriteriaFile_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ac.txt")
	if err := os.WriteFile(path, []byte("must compile\nand pass tests"), 0o600); err != nil {
		t.Fatalf("write acceptance file: %v", err)
	}
	cmd := NewCommand("maestro plan add-task", "maestro plan add-task")
	var ac string
	if err := resolveAcceptanceCriteriaFile(cmd, &ac, path); err != nil {
		t.Fatalf("resolveAcceptanceCriteriaFile: %v", err)
	}
	if ac != "must compile\nand pass tests" {
		t.Errorf("acceptance = %q, want loaded file content", ac)
	}
}

// TestResolveAcceptanceCriteriaFile_RejectsMixedSources symmetry with
// --content-file: only one source is allowed so the value sent to the
// daemon has a single obvious origin.
func TestResolveAcceptanceCriteriaFile_RejectsMixedSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ac.txt")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatalf("write acceptance file: %v", err)
	}
	cmd := NewCommand("maestro plan add-task", "maestro plan add-task")
	ac := "inline-value"
	err := resolveAcceptanceCriteriaFile(cmd, &ac, path)
	if err == nil {
		t.Fatal("expected mixed-source error")
	}
	if !containsStr(err.Error(), "mutually exclusive") {
		t.Errorf("error %q does not mention mutual exclusion", err.Error())
	}
}

// TestResolvePurposeFile_DashReadsStdin pins the matching stdin path on
// --purpose-file. Same readFlagInputFile underneath, so a smoke test is
// enough to lock the wire-up.
func TestResolvePurposeFile_DashReadsStdin(t *testing.T) {
	withStdin(t, "purpose via pipe", func() {
		cmd := NewCommand("maestro plan add-task", "maestro plan add-task")
		var purpose string
		if err := resolvePurposeFile(cmd, &purpose, "-"); err != nil {
			t.Fatalf("resolvePurposeFile(-): %v", err)
		}
		if purpose != "purpose via pipe" {
			t.Errorf("purpose = %q, want stdin contents", purpose)
		}
	})
}

// TestResolveContentFile_DashReadsStdin pins the stdin fall-through added
// alongside resolveSummaryFile's dash support: Planner agents that pipe
// task content through `plan add-task --content-file -` would otherwise
// hit "open -: no such file or directory" the same way plan complete
// did. Both flag helpers share readFlagInputFile, so a single regression
// test per call site is enough.
func TestResolveContentFile_DashReadsStdin(t *testing.T) {
	withStdin(t, "task body via pipe", func() {
		cmd := NewCommand("maestro plan add-task", "maestro plan add-task")
		content := ""
		if err := resolveContentFile(cmd, &content, "-"); err != nil {
			t.Fatalf("resolveContentFile(-): %v", err)
		}
		if content != "task body via pipe" {
			t.Errorf("content = %q, want stdin contents", content)
		}
	})
}

func TestRunPlanRequestCancel_InvalidCommandID(t *testing.T) {
	err := newCLIApp().runPlanRequestCancel([]string{"--command-id", "../evil"})
	if err == nil {
		t.Fatal("expected error for invalid command-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !containsStr(ce.Msg, "invalid --command-id") {
		t.Errorf("expected 'invalid --command-id' in error, got: %s", ce.Msg)
	}
}

func TestRunPlanRebuild_InvalidCommandID(t *testing.T) {
	err := newCLIApp().runPlanRebuild([]string{"--command-id", "../evil"})
	if err == nil {
		t.Fatal("expected error for invalid command-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !containsStr(ce.Msg, "invalid --command-id") {
		t.Errorf("expected 'invalid --command-id' in error, got: %s", ce.Msg)
	}
}

func TestRunPlanRebuild_MissingCommandID(t *testing.T) {
	err := newCLIApp().runPlanRebuild(nil)
	if err == nil {
		t.Fatal("expected error for missing command-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestSendPlanCommand_SanitizesValidationMessage(t *testing.T) {
	withMaestroDir(t)

	app := newTestApp(&mockUDSClient{
		sendCommandContextFunc: func(_ context.Context, _ string, _ any) (*uds.Response, error) {
			return uds.ErrorResponse(uds.ErrCodeValidation, "bad input\x1b[31m injected\x1b[0m"), nil
		},
	})

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err := app.sendPlanCommand("test", ".maestro", map[string]any{"operation": "test"}, planCommandTimeout)

	w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) || !ce.Silent {
		t.Fatalf("expected silent CLIError, got: %v", err)
	}
	// ANSI escape (0x1b) should be stripped
	if bytes.ContainsRune([]byte(output), 0x1b) {
		t.Errorf("stderr should not contain ANSI escape codes, got: %q", output)
	}
	if !containsStr(output, "bad input") {
		t.Errorf("stderr should contain message text, got: %q", output)
	}
}
