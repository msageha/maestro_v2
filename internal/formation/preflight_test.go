package formation

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
