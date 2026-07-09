package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/msageha/maestro_v2/internal/uds"
)

// --- C-F3: BACKPRESSURE must exit 2 on every daemon-RPC path ---

func TestBackpressureExitCode_AllDaemonPaths(t *testing.T) {
	backpressureClient := &mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return errorResponse(uds.ErrCodeBackpressure, "server at capacity"), nil
		},
	}

	cases := []struct {
		name string
		run  func(app *cliApp) error
	}{
		{"plan submit (sendPlanCommand)", func(app *cliApp) error {
			return app.sendPlanCommand("plan submit", ".maestro", map[string]any{"operation": "submit"}, planCommandTimeout)
		}},
		{"plan request-cancel", func(app *cliApp) error {
			return app.runPlanRequestCancel([]string{"--command-id", "cmd_1"})
		}},
		{"result write", func(app *cliApp) error {
			return app.runResultWrite([]string{"worker1",
				"--task-id", "t1", "--command-id", "cmd_1", "--lease-epoch", "1",
				"--status", "completed",
				"--summary", "Verified backpressure maps to the retryable exit code"})
		}},
		{"task heartbeat", func(app *cliApp) error {
			return app.runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "w1", "--epoch", "1"})
		}},
		{"queue write", func(app *cliApp) error {
			return app.runQueueWrite([]string{"planner", "--type", "command", "--content", "do the thing"}, io.Discard)
		}},
		{"skill approve", func(app *cliApp) error {
			return app.runSkillApprove([]string{"cand_1"})
		}},
		{"skill reject", func(app *cliApp) error {
			return app.runSkillReject([]string{"cand_1"})
		}},
		{"dashboard", func(app *cliApp) error {
			return app.runDashboard(nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withMaestroDir(t)
			err := tc.run(newTestApp(backpressureClient))
			assertRetryableExit(t, err)
		})
	}

	t.Run("verify write", func(t *testing.T) {
		withMaestroDir(t)
		cfgPath := filepath.Join(t.TempDir(), "verify.yaml")
		if err := os.WriteFile(cfgPath, []byte("verify:\n  build:\n    - go test ./...\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := newTestApp(backpressureClient).runVerifyWrite([]string{
			"--command-id", "cmd_1", "--config-file", cfgPath})
		assertRetryableExit(t, err)
	})
}

func assertRetryableExit(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error for BACKPRESSURE response")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if ce.Code != ExitCodeRetryable {
		t.Errorf("exit code = %d, want %d (ExitCodeRetryable) for BACKPRESSURE", ce.Code, ExitCodeRetryable)
	}
}

// --- C-F1: relative --tasks-file must be sent to the daemon as absolute ---

func TestRunPlanSubmit_RelativeTasksFileSentAsAbsolute(t *testing.T) {
	withMaestroDir(t)
	var captured map[string]any
	app := newTestApp(&mockUDSClient{
		sendCommandContextFunc: func(_ context.Context, _ string, params any) (*uds.Response, error) {
			raw, _ := json.Marshal(params)
			if err := json.Unmarshal(raw, &captured); err != nil {
				t.Fatalf("unmarshal params: %v", err)
			}
			return successResponse(nil), nil
		},
	})

	if err := app.runPlanSubmit([]string{"--command-id", "cmd_1", "--tasks-file", "tasks.yaml"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, ok := captured["data"].(map[string]any)
	if !ok {
		t.Fatalf("params missing data map: %v", captured)
	}
	got, ok := data["tasks_file"].(string)
	if !ok {
		t.Fatalf("data missing tasks_file: %v", data)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("tasks_file %q was not absolutised; the daemon resolves relative paths against its own CWD", got)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(wd, "tasks.yaml"); got != want {
		t.Errorf("tasks_file = %q, want %q", got, want)
	}
}

// --- C-F11: stdin default must not block when stdin is a terminal ---

func withTerminalStdin(t *testing.T) {
	t.Helper()
	orig := isStdinTerminal
	isStdinTerminal = func() bool { return true }
	t.Cleanup(func() { isStdinTerminal = orig })
}

func TestRunPlanSubmit_StdinTerminalFailsFast(t *testing.T) {
	withMaestroDir(t)
	withTerminalStdin(t)

	err := newCLIApp().runPlanSubmit([]string{"--command-id", "cmd_1"})
	if err == nil {
		t.Fatal("expected error when stdin is a terminal and no --tasks-file given")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "--tasks-file") {
		t.Errorf("error should guide to --tasks-file, got: %s", ce.Msg)
	}
}

func TestReadVerifyConfigInput_StdinTerminalFailsFast(t *testing.T) {
	withTerminalStdin(t)

	_, err := readVerifyConfigInput("-")
	if err == nil {
		t.Fatal("expected error when stdin is a terminal")
	}
	if !strings.Contains(err.Error(), "--config-file") {
		t.Errorf("error should guide to --config-file, got: %v", err)
	}
}

// --- C-F2: inline stdin payloads must respect the UDS frame cap ---

func TestRunPlanSubmit_StdinExceedsInlineLimit(t *testing.T) {
	withMaestroDir(t)
	oversized := strings.Repeat("x", maxInlineUDSPayloadBytes+1)
	withStdin(t, oversized, func() {
		err := newCLIApp().runPlanSubmit([]string{"--command-id", "cmd_1"})
		if err == nil {
			t.Fatal("expected error for stdin over the inline limit")
		}
		msg := err.Error()
		if !strings.Contains(msg, "inline limit") || !strings.Contains(msg, "--tasks-file") {
			t.Errorf("error should mention the inline limit and guide to --tasks-file, got: %s", msg)
		}
	})
}

func TestReadVerifyConfigInput_StdinExceedsInlineLimit(t *testing.T) {
	oversized := strings.Repeat("x", maxInlineUDSPayloadBytes+1)
	withStdin(t, oversized, func() {
		_, err := readVerifyConfigInput("-")
		if err == nil {
			t.Fatal("expected error for stdin over the inline limit")
		}
		if !strings.Contains(err.Error(), "inline limit") {
			t.Errorf("error should mention the inline limit, got: %v", err)
		}
	})
}

// --- C-F16: signal cancellation must not be reported as a timeout ---

func TestPlanSendError_Classification(t *testing.T) {
	t.Parallel()
	sendErr := errors.New("read response: use of closed network connection")

	timedOut := planSendError("plan submit", context.DeadlineExceeded, sendErr, planCommandTimeout)
	if !strings.Contains(timedOut.Error(), "timed out") {
		t.Errorf("deadline expiry should report a timeout, got: %v", timedOut)
	}

	interrupted := planSendError("plan submit", context.Canceled, sendErr, planCommandTimeout)
	if strings.Contains(interrupted.Error(), "timed out") {
		t.Errorf("signal cancellation must not be reported as a timeout, got: %v", interrupted)
	}
	if !strings.Contains(interrupted.Error(), "interrupt") {
		t.Errorf("signal cancellation should mention the interrupt, got: %v", interrupted)
	}

	plain := planSendError("plan submit", nil, sendErr, planCommandTimeout)
	if strings.Contains(plain.Error(), "timed out") || strings.Contains(plain.Error(), "interrupt") {
		t.Errorf("plain transport error must stay unclassified, got: %v", plain)
	}
	if !errors.Is(plain, sendErr) {
		t.Error("planSendError must wrap the original error")
	}
}

// --- C-F9: daemon-down hint must not tell agents to run `maestro daemon` ---

func TestRewriteDaemonStartHint(t *testing.T) {
	t.Parallel()
	orig := fmt.Errorf("failed to connect to daemon at /tmp/x.sock: %w\n%s",
		syscall.ECONNREFUSED, udsDaemonStartHint)

	got := rewriteDaemonStartHint(orig)
	msg := got.Error()
	if strings.Contains(msg, udsDaemonStartHint) {
		t.Errorf("foreground-daemon hint must be rewritten, got: %s", msg)
	}
	if !strings.Contains(msg, "maestro up") {
		t.Errorf("rewritten hint should point at maestro up, got: %s", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("original cause must be preserved, got: %s", msg)
	}
	if !errors.Is(got, syscall.ECONNREFUSED) {
		t.Error("rewritten error must preserve the wrapped error chain")
	}

	passthrough := errors.New("some other failure")
	if rewriteDaemonStartHint(passthrough) != passthrough {
		t.Error("errors without the hint must pass through unchanged")
	}
	if rewriteDaemonStartHint(nil) != nil {
		t.Error("nil must pass through as nil")
	}
}

func TestNewDaemonClient_RewritesDaemonStartHint(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return nil, fmt.Errorf("failed to connect to daemon at /tmp/x.sock: connection refused\n%s", udsDaemonStartHint)
		},
	})

	_, err := app.newDaemonClient(".maestro").SendCommand("queue_write", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), udsDaemonStartHint) {
		t.Errorf("newDaemonClient must rewrite the daemon-start hint, got: %v", err)
	}
	if !strings.Contains(err.Error(), "maestro up") {
		t.Errorf("rewritten hint should point at maestro up, got: %v", err)
	}
}

// --- C-F13: rune-safe truncation / oversized path rejection ---

func TestTruncateEntries_RuneBoundary(t *testing.T) {
	t.Parallel()
	entries := stringSliceFlag{strings.Repeat("あ", 10)} // 30 bytes
	truncateEntries("--learnings", entries, 8)          // not a rune boundary (8 % 3 != 0)
	if len(entries[0]) > 8 {
		t.Errorf("entry length = %d, want <= 8", len(entries[0]))
	}
	if !utf8.ValidString(entries[0]) {
		t.Errorf("truncated entry is invalid UTF-8: %q", entries[0])
	}
	if want := strings.Repeat("あ", 2); entries[0] != want {
		t.Errorf("entry = %q, want %q", entries[0], want)
	}
}

func TestTruncateAtRuneBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"under limit untouched", "abc", 8, "abc"},
		{"ascii cut exact", "abcdefghij", 4, "abcd"},
		{"multibyte cut at boundary", "あいう", 7, "あい"},
		{"multibyte cut mid-rune", "あいう", 8, "あい"},
		{"zero max", "あ", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateAtRuneBoundary(tc.in, tc.maxBytes); got != tc.want {
				t.Errorf("truncateAtRuneBoundary(%q, %d) = %q, want %q", tc.in, tc.maxBytes, got, tc.want)
			}
		})
	}
}

func TestRejectOversizedEntries(t *testing.T) {
	t.Parallel()
	if err := rejectOversizedEntries("--files-changed", stringSliceFlag{"a.go", "b.go"}, 16); err != nil {
		t.Fatalf("normal entries must pass: %v", err)
	}
	err := rejectOversizedEntries("--files-changed", stringSliceFlag{"a.go", strings.Repeat("x", 17)}, 16)
	if err == nil {
		t.Fatal("expected rejection for oversized entry")
	}
	if !strings.Contains(err.Error(), "entry 1") {
		t.Errorf("error should identify the offending entry index, got: %v", err)
	}
}

// --- C-F5: flag in positional-argument slot must name the missing argument ---

func TestRunQueueWrite_FlagInTargetPosition(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"--type", "command", "--content", "x"}, io.Discard)
	if err == nil {
		t.Fatal("expected error when target is missing")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "missing target") {
		t.Errorf("expected 'missing target' guidance, got: %s", ce.Msg)
	}
}

func TestRunResultWrite_FlagInReporterPosition(t *testing.T) {
	err := newCLIApp().runResultWrite([]string{"--task-id", "t1", "--command-id", "c1",
		"--lease-epoch", "1", "--status", "completed"})
	if err == nil {
		t.Fatal("expected error when reporter is missing")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "missing reporter") {
		t.Errorf("expected 'missing reporter' guidance, got: %s", ce.Msg)
	}
}

func TestRunSkillApproveReject_FlagInCandidatePosition(t *testing.T) {
	app := newCLIApp()

	err := app.runSkillApprove([]string{"--name", "my-skill"})
	if err == nil {
		t.Fatal("expected error when candidate-id is missing")
	}
	if !strings.Contains(err.Error(), "missing candidate-id") {
		t.Errorf("approve: expected 'missing candidate-id' guidance, got: %v", err)
	}

	err = app.runSkillReject([]string{"--unknown"})
	if err == nil {
		t.Fatal("expected error when candidate-id is missing")
	}
	if !strings.Contains(err.Error(), "missing candidate-id") {
		t.Errorf("reject: expected 'missing candidate-id' guidance, got: %v", err)
	}
}

// --- C-F6: usage must not advertise the always-rejected --type task ---

func TestRunQueueWrite_UsageOmitsTaskType(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "bogus"}, io.Discard)
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "<command|message|notification|cancel-request>") {
		t.Errorf("usage should list the supported types, got: %s", ce.Msg)
	}
	if strings.Contains(ce.Msg, "|task|") || strings.Contains(ce.Msg, "task|") {
		t.Errorf("usage must not advertise the rejected task type, got: %s", ce.Msg)
	}
}

// --- C-F7: explicit -h/--help must exit 0 ---

func TestCommandBuilder_HelpRequestedSentinel(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			cmd := NewCommand("maestro test", "maestro test --name <val>")
			var name string
			cmd.RequiredString(&name, "name", "the name")
			err := cmd.Parse([]string{arg})
			if !errors.Is(err, errHelpRequested) {
				t.Errorf("Parse(%s) = %v, want errHelpRequested", arg, err)
			}
		})
	}
}

func TestRun_SubcommandHelpExitsZero(t *testing.T) {
	cases := [][]string{
		{"plan", "submit", "-h"},
		{"plan", "submit", "--help"},
		{"result", "write", "-h"},
		{"queue", "write", "-h"},
		{"task", "heartbeat", "--help"},
		{"up", "-h"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if code := newCLIApp().run(args); code != 0 {
				t.Errorf("run(%v) = %d, want 0 for explicit help", args, code)
			}
		})
	}
}

func TestRun_ParseErrorStillExitsNonZero(t *testing.T) {
	if code := newCLIApp().run([]string{"plan", "submit", "--nonexistent-flag"}); code == 0 {
		t.Error("parse errors must keep a non-zero exit code")
	}
}

// --- C-F4: --boost=false / --continuous=false must count as explicit ---

func TestParseUpFlags_ExplicitFalseDetected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want upFlagState
	}{
		{"no flags", nil, upFlagState{}},
		{"boost on", []string{"--boost"}, upFlagState{boost: true, boostSet: true}},
		{"boost explicit false", []string{"--boost=false"}, upFlagState{boost: false, boostSet: true}},
		{"continuous on", []string{"--continuous"}, upFlagState{continuous: true, continuousSet: true}},
		{"continuous explicit false", []string{"--continuous=false"}, upFlagState{continuous: false, continuousSet: true}},
		{"mixed", []string{"--boost=false", "--continuous", "-d", "-f"},
			upFlagState{boost: false, boostSet: true, continuous: true, continuousSet: true, detach: true, force: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseUpFlags(tc.args)
			if err != nil {
				t.Fatalf("parseUpFlags(%v): %v", tc.args, err)
			}
			if *got != tc.want {
				t.Errorf("parseUpFlags(%v) = %+v, want %+v", tc.args, *got, tc.want)
			}
		})
	}
}

// --- C-F24: heartbeat accepts --lease-epoch as an alias for --epoch ---

func TestRunTaskHeartbeat_LeaseEpochAlias(t *testing.T) {
	withMaestroDir(t)
	var captured map[string]any
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(_ string, params any) (*uds.Response, error) {
			captured = params.(map[string]any)
			return successResponse(nil), nil
		},
	})

	err := app.runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "w1", "--lease-epoch", "5"})
	if err != nil {
		t.Fatalf("unexpected error: --lease-epoch must be accepted as an alias: %v", err)
	}
	if captured["epoch"] != 5 {
		t.Errorf("epoch = %v, want 5", captured["epoch"])
	}
}

// --- C-F12: modeSetter must honour explicit =false ---

func TestModeSetter_ExplicitFalseIsNoop(t *testing.T) {
	var mode string
	fs := newFlagSet("test")
	fs.Var(&modeSetter{target: &mode, val: "interrupt"}, "interrupt", "")

	if err := fs.Parse([]string{"--interrupt=false"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if mode != "" {
		t.Errorf("mode = %q, want empty (explicit =false must not select the mode)", mode)
	}
}

func TestModeSetter_InvalidBoolRejected(t *testing.T) {
	var mode string
	fs := newFlagSet("test")
	fs.Var(&modeSetter{target: &mode, val: "interrupt"}, "interrupt", "")

	if err := fs.Parse([]string{"--interrupt=bogus"}); err == nil {
		t.Error("expected error for non-boolean value")
	}
}

// --- C-F25: success with no payload must print a JSON object, not null ---

func TestPrintJSONResponse_EmptyDataPrintsObject(t *testing.T) {
	for _, data := range []json.RawMessage{nil, json.RawMessage("null")} {
		oldStdout := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w

		printErr := printJSONResponse(data, "plan submit")

		_ = w.Close()
		os.Stdout = oldStdout
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(r); err != nil {
			t.Fatal(err)
		}

		if printErr != nil {
			t.Fatalf("unexpected error: %v", printErr)
		}
		if got := strings.TrimSpace(buf.String()); got != "{}" {
			t.Errorf("output for %q = %q, want {}", string(data), got)
		}
	}
}
