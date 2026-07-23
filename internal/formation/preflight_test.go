package formation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

func TestPreflightEnvironment_HappyPath(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}

	maestroDir, err := os.MkdirTemp("", "m-preflight-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(maestroDir) })

	if err := preflightEnvironment(maestroDir); err != nil {
		if errors.Is(err, uds.ErrUnixSocketUnavailable) {
			t.Skipf("unix domain sockets unavailable in this environment: %v", err)
		}
		t.Fatalf("preflightEnvironment returned error: %v", err)
	}
}

func TestPreflightEnvironment_DoesNotRemoveRealSocketPath(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}

	maestroDir, err := os.MkdirTemp("", "m-preflight-real-socket-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(maestroDir) })

	socketPath, err := uds.SocketPath(maestroDir)
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	const marker = "live daemon placeholder"
	if err := os.WriteFile(socketPath, []byte(marker), 0o600); err != nil {
		t.Fatalf("write dummy socket path file: %v", err)
	}

	if err := preflightEnvironment(maestroDir); err != nil {
		if errors.Is(err, uds.ErrUnixSocketUnavailable) {
			t.Skipf("unix domain sockets unavailable in this environment: %v", err)
		}
		t.Fatalf("preflightEnvironment returned error: %v", err)
	}
	got, err := os.ReadFile(socketPath)
	if err != nil {
		t.Fatalf("real socket path was removed or became unreadable: %v", err)
	}
	if string(got) != marker {
		t.Fatalf("real socket path content changed: got %q, want %q", got, marker)
	}
}

func TestPreflightProbeSocketPathFallsBackWhenSiblingTooLong(t *testing.T) {
	t.Parallel()

	realSocketPath := filepath.Join(
		string(os.PathSeparator),
		strings.Repeat("a", uds.MaxUnixSocketPathLen()),
		uds.DefaultSocketName,
	)
	probePath, err := preflightProbeSocketPath(realSocketPath)
	if err != nil {
		t.Fatalf("preflightProbeSocketPath: %v", err)
	}
	if filepath.Dir(probePath) != filepath.Clean(os.TempDir()) {
		t.Fatalf("probe dir = %q, want os.TempDir %q (path %q)", filepath.Dir(probePath), filepath.Clean(os.TempDir()), probePath)
	}
	if len(probePath) > uds.MaxUnixSocketPathLen() {
		t.Fatalf("fallback probe path too long: %d > %d (%s)", len(probePath), uds.MaxUnixSocketPathLen(), probePath)
	}
}

func TestClassifyPreflightProbeErr_SandboxUnavailable(t *testing.T) {
	t.Parallel()

	probeErr := &uds.UnixSocketUnavailableError{
		Path: filepath.Join(t.TempDir(), "daemon.sock"),
		Err:  os.ErrPermission,
	}
	err := classifyPreflightProbeErr(probeErr)
	if err == nil {
		t.Fatal("expected classified preflight error")
	}
	if !errors.Is(err, ErrPreflightFailed) {
		t.Fatalf("expected ErrPreflightFailed, got %v", err)
	}
	if !errors.Is(err, ErrSandboxedLaunch) {
		t.Fatalf("expected ErrSandboxedLaunch, got %v", err)
	}
	if !errors.Is(err, uds.ErrUnixSocketUnavailable) {
		t.Fatalf("expected underlying unix-socket unavailable error, got %v", err)
	}
	if !strings.Contains(err.Error(), "normal shell outside the sandbox") {
		t.Fatalf("expected remediation message, got %v", err)
	}
}

func TestClassifyPreflightProbeErr_GenericErrorIsNotSandbox(t *testing.T) {
	t.Parallel()

	err := classifyPreflightProbeErr(fmt.Errorf("path validation failed"))
	if err == nil {
		t.Fatal("expected classified preflight error")
	}
	if !errors.Is(err, ErrPreflightFailed) {
		t.Fatalf("expected ErrPreflightFailed, got %v", err)
	}
	if errors.Is(err, ErrSandboxedLaunch) {
		t.Fatalf("generic probe error must not be classified as sandbox: %v", err)
	}
}

func TestClassifyPreflightProbeErr_PathTooLongIsNotSandbox(t *testing.T) {
	t.Parallel()

	probeErr := uds.ProbeUnixSocket("/tmp/" + strings.Repeat("a", 200) + ".sock")
	if probeErr == nil {
		t.Fatal("expected probe error for too-long socket path")
	}

	err := classifyPreflightProbeErr(probeErr)
	if err == nil {
		t.Fatal("expected classified preflight error")
	}
	if !errors.Is(err, ErrPreflightFailed) {
		t.Fatalf("expected ErrPreflightFailed, got %v", err)
	}
	if errors.Is(err, ErrSandboxedLaunch) {
		t.Fatalf("path-too-long probe error must not be classified as sandbox: %v", err)
	}
	if !strings.Contains(err.Error(), "socket path too long") {
		t.Fatalf("expected socket path length message, got %v", err)
	}
}

// --- preflightRuntimes ---

// fakeRuntimePreflight returns an agent.RuntimePreflight whose collaborators
// never touch the host PATH, env, or home directory, so these tests stay
// hermetic (and sandbox-safe: no external process is forked).
func fakeRuntimePreflight(lookPath func(string) (string, error)) *agent.RuntimePreflight {
	return &agent.RuntimePreflight{
		LookPath: lookPath,
		RunCommand: func(_ context.Context, _ int, _ string, _ ...string) ([]byte, error) {
			return nil, errors.New("preflightRuntimes must not fork a probe process")
		},
		Getenv:         func(string) string { return "" },
		UserHomeDir:    func() (string, error) { return "", errors.New("no home") },
		Timeout:        time.Second,
		MaxOutputBytes: 1024,
	}
}

func runtimeTestConfig() model.Config {
	cfg := model.Config{}
	cfg.Agents.Orchestrator.Model = "opus"
	cfg.Agents.Planner.Model = "sonnet"
	cfg.Agents.Workers.Count = 2
	cfg.Agents.Workers.DefaultModel = "sonnet"
	cfg.Agents.Workers.Models = map[string]string{"worker2": "codex"}
	return cfg
}

// TestPreflightRuntimes_MissingBinaryFailsFast pins the hard-failure path:
// a configured runtime whose CLI is absent from PATH must abort `maestro up`
// with ErrPreflightFailed (so the CLI skips CleanupOnFailure) and a message
// naming the runtime and the agents that need it.
func TestPreflightRuntimes_MissingBinaryFailsFast(t *testing.T) {
	t.Parallel()
	rp := fakeRuntimePreflight(func(file string) (string, error) {
		if file == "codex" {
			return "", fmt.Errorf("exec: %q: executable file not found in $PATH", file)
		}
		return "/fake/bin/" + file, nil
	})

	var warn bytes.Buffer
	err := preflightRuntimes(runtimeTestConfig(), rp, &warn)
	if err == nil {
		t.Fatal("expected preflight failure for missing codex binary")
	}
	if !errors.Is(err, ErrPreflightFailed) {
		t.Fatalf("expected ErrPreflightFailed, got %v", err)
	}
	for _, want := range []string{"codex", "worker2", "maestro doctor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// TestPreflightRuntimes_AuthUnknownWarnsOnly pins the best-effort auth
// contract: undetectable credentials produce a stderr warning, never an
// error — auth stores (keychain in particular) are not reliably
// inspectable, so blocking startup on them would be a false positive.
func TestPreflightRuntimes_AuthUnknownWarnsOnly(t *testing.T) {
	t.Parallel()
	rp := fakeRuntimePreflight(func(file string) (string, error) {
		return "/fake/bin/" + file, nil
	})

	var warn bytes.Buffer
	if err := preflightRuntimes(runtimeTestConfig(), rp, &warn); err != nil {
		t.Fatalf("unknown auth must not fail preflight: %v", err)
	}
	out := warn.String()
	if !strings.Contains(out, "auth status unknown") {
		t.Errorf("expected auth warnings on the warn writer, got: %q", out)
	}
	// Both configured runtimes (claude-code, codex) should be warned about.
	for _, runtime := range []string{"claude-code", "codex"} {
		if !strings.Contains(out, runtime) {
			t.Errorf("warning should cover runtime %s, got: %q", runtime, out)
		}
	}
}

// TestPreflightRuntimes_AllGreenIsQuiet verifies the happy path stays
// silent: binaries resolve and credentials are detected via env vars.
func TestPreflightRuntimes_AllGreenIsQuiet(t *testing.T) {
	t.Parallel()
	rp := fakeRuntimePreflight(func(file string) (string, error) {
		return "/fake/bin/" + file, nil
	})
	rp.Getenv = func(key string) string {
		switch key {
		case "ANTHROPIC_API_KEY", "OPENAI_API_KEY":
			return "x"
		}
		return ""
	}

	var warn bytes.Buffer
	if err := preflightRuntimes(runtimeTestConfig(), rp, &warn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn.Len() != 0 {
		t.Errorf("expected no warnings, got: %q", warn.String())
	}
}
