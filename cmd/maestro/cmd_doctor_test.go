package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// fakeDoctorPreflight returns a RuntimePreflight whose collaborators never
// touch the host PATH, env, or home directory, and never fork a process —
// keeping the suite runnable inside sandboxes that deny external spawns.
func fakeDoctorPreflight() *agent.RuntimePreflight {
	return &agent.RuntimePreflight{
		LookPath: func(file string) (string, error) { return "/fake/bin/" + file, nil },
		RunCommand: func(_ context.Context, _ int, name string, _ ...string) ([]byte, error) {
			return []byte(name + " 9.9.9\n"), nil
		},
		Getenv:         func(string) string { return "" },
		UserHomeDir:    func() (string, error) { return "", errors.New("no home in fake") },
		Timeout:        time.Second,
		MaxOutputBytes: 1024,
	}
}

func doctorTestConfig() model.Config {
	cfg := model.Config{}
	cfg.Agents.Orchestrator.Model = "opus"
	cfg.Agents.Planner.Model = "sonnet"
	cfg.Agents.Workers.Count = 2
	cfg.Agents.Workers.DefaultModel = "sonnet"
	cfg.Agents.Workers.Models = map[string]string{"worker2": "gemini"}
	return cfg
}

func TestRunDoctorReport_AllGreenExitsZero(t *testing.T) {
	t.Parallel()
	rp := fakeDoctorPreflight()
	rp.Getenv = func(key string) string {
		switch key {
		case "ANTHROPIC_API_KEY", "GEMINI_API_KEY":
			return "x"
		}
		return ""
	}

	var out bytes.Buffer
	if err := runDoctorReport(doctorTestConfig(), rp, &out, false); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"tmux: ok (/fake/bin/tmux)",
		"runtime claude-code (agents: orchestrator, planner, worker1)",
		"runtime gemini (agents: worker2)",
		"claude 9.9.9",
		"gemini 9.9.9",
		"All checks passed.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
}

// TestRunDoctorReport_MissingBinaryFails pins the exit-code contract: a
// missing runtime binary is the certain failure that must map to a non-zero
// exit, with version/auth reported as skipped rather than as extra noise.
func TestRunDoctorReport_MissingBinaryFails(t *testing.T) {
	t.Parallel()
	rp := fakeDoctorPreflight()
	rp.LookPath = func(file string) (string, error) {
		if file == "gemini" {
			return "", fmt.Errorf("exec: %q: executable file not found in $PATH", file)
		}
		return "/fake/bin/" + file, nil
	}

	var out bytes.Buffer
	err := runDoctorReport(doctorTestConfig(), rp, &out, false)
	if err == nil {
		t.Fatalf("expected failure exit, output:\n%s", out.String())
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if ce.Code != 1 {
		t.Errorf("exit code = %d, want 1", ce.Code)
	}
	text := out.String()
	for _, want := range []string{
		"binary : FAIL",
		"version: skipped (binary not found)",
		"auth   : skipped (binary not found)",
		"1 problem(s)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
}

// TestRunDoctorReport_WarningsDoNotFail pins the best-effort contract:
// unknown auth and a failed version probe stay warnings (exit 0).
func TestRunDoctorReport_WarningsDoNotFail(t *testing.T) {
	t.Parallel()
	rp := fakeDoctorPreflight()
	rp.RunCommand = func(_ context.Context, _ int, _ string, _ ...string) ([]byte, error) {
		return []byte("boom"), errors.New("exit status 1")
	}

	var out bytes.Buffer
	if err := runDoctorReport(doctorTestConfig(), rp, &out, false); err != nil {
		t.Fatalf("warnings must not fail doctor: %v\noutput:\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{"version: warn", "auth   : unknown", "No hard failures"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
}

func TestRunDoctorReport_JSONOutput(t *testing.T) {
	t.Parallel()
	rp := fakeDoctorPreflight()
	rp.LookPath = func(file string) (string, error) {
		if file == "gemini" {
			return "", errors.New("not found")
		}
		return "/fake/bin/" + file, nil
	}

	var out bytes.Buffer
	err := runDoctorReport(doctorTestConfig(), rp, &out, true)
	if err == nil {
		t.Fatal("expected failure exit for missing binary")
	}

	var report doctorReport
	if decodeErr := json.Unmarshal(out.Bytes(), &report); decodeErr != nil {
		t.Fatalf("JSON output must be parseable: %v\noutput:\n%s", decodeErr, out.String())
	}
	if report.OK {
		t.Error("report.OK = true, want false with a missing binary")
	}
	if report.Tmux.Path != "/fake/bin/tmux" {
		t.Errorf("tmux path = %q", report.Tmux.Path)
	}
	if len(report.Runtimes) != 2 {
		t.Fatalf("runtimes = %d, want 2 (claude-code, gemini): %+v", len(report.Runtimes), report.Runtimes)
	}
	var gemini *agent.RuntimeCheckResult
	for i := range report.Runtimes {
		if report.Runtimes[i].Runtime == model.RuntimeGemini {
			gemini = &report.Runtimes[i]
		}
	}
	if gemini == nil {
		t.Fatalf("gemini runtime missing from report: %+v", report.Runtimes)
	}
	if gemini.Status != agent.RuntimeCheckFail {
		t.Errorf("gemini status = %q, want fail", gemini.Status)
	}
	if gemini.BinaryError == "" {
		t.Error("gemini binary_error must be populated")
	}
}

func TestRunDoctor_RejectsNegativeTimeout(t *testing.T) {
	err := runDoctor([]string{"--probe-timeout-sec", "-1"})
	if err == nil {
		t.Fatal("expected usage error for negative timeout")
	}
	if !strings.Contains(err.Error(), "--probe-timeout-sec") {
		t.Errorf("error should mention the flag, got: %v", err)
	}
}

func TestRunDoctor_UnknownFlagRejected(t *testing.T) {
	if err := runDoctor([]string{"--bogus"}); err == nil {
		t.Fatal("expected parse error for unknown flag")
	}
}
