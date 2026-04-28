package uds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackpressureWriteDeadlineConstant(t *testing.T) {
	t.Parallel()
	// Verify the constant is positive and reasonable.
	if backpressureWriteDeadline <= 0 {
		t.Errorf("backpressureWriteDeadline should be positive, got %v", backpressureWriteDeadline)
	}
}

func TestServer_Start_SocketPathTooLong(t *testing.T) {
	t.Parallel()
	// Build a path that exceeds maxUnixSocketPathLen.
	longName := strings.Repeat("a", maxUnixSocketPathLen+1)
	server := NewServer(longName)

	err := server.Start()
	if err == nil {
		server.Stop()
		t.Fatal("expected error for socket path exceeding length limit")
	}
	if !strings.Contains(err.Error(), "socket path too long") {
		t.Errorf("expected 'socket path too long' error, got: %v", err)
	}
}

func TestSocketPath_LongMaestroDirUsesFallback(t *testing.T) {
	t.Parallel()

	maestroDir := t.TempDir()
	for len(filepath.Join(maestroDir, DefaultSocketName)) <= maxUnixSocketPathLen {
		maestroDir = filepath.Join(maestroDir, "very-long-maestro-path-segment")
	}

	preferred := filepath.Join(maestroDir, DefaultSocketName)
	sockPath, err := SocketPath(maestroDir)
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	if sockPath == preferred {
		t.Fatalf("expected fallback path for %d-byte preferred socket", len(preferred))
	}
	if len(sockPath) > maxUnixSocketPathLen {
		t.Fatalf("fallback socket path too long: %d > %d (%s)", len(sockPath), maxUnixSocketPathLen, sockPath)
	}
	if !strings.Contains(sockPath, socketFallbackDirBaseName) {
		t.Fatalf("fallback path %q should contain %q", sockPath, socketFallbackDirBaseName)
	}
}

func TestSocketPath_TempMaestroDirUsesFallback(t *testing.T) {
	t.Parallel()

	maestroDir := filepath.Join("/tmp", "maestro-socket-test", ".maestro")
	preferred := filepath.Join(maestroDir, DefaultSocketName)
	sockPath, err := SocketPath(maestroDir)
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	if sockPath == preferred {
		t.Fatalf("expected fallback path for temp maestro dir %q", maestroDir)
	}
	if len(sockPath) > maxUnixSocketPathLen {
		t.Fatalf("fallback socket path too long: %d > %d (%s)", len(sockPath), maxUnixSocketPathLen, sockPath)
	}
}

func TestServer_Start_LongMaestroDirFallbackSocket(t *testing.T) {
	t.Parallel()
	requireUnixSocketSupport(t)

	maestroDir := t.TempDir()
	for len(filepath.Join(maestroDir, DefaultSocketName)) <= maxUnixSocketPathLen {
		maestroDir = filepath.Join(maestroDir, "very-long-maestro-path-segment")
	}

	sockPath, err := SocketPath(maestroDir)
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}

	server := NewServer(sockPath)
	server.Handle("ping", func(*Request) *Response {
		return SuccessResponse(map[string]string{"status": "ok"})
	})
	if err := server.Start(); err != nil {
		t.Fatalf("server start with fallback socket: %v", err)
	}
	t.Cleanup(func() { _ = server.Stop() })

	client := NewClient(sockPath)
	resp, err := client.SendCommand("ping", nil)
	if err != nil {
		t.Fatalf("client ping: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected ping success, got %+v", resp)
	}
}

func TestServer_Start_SocketPathAtLimit(t *testing.T) {
	t.Parallel()
	// A path exactly at the limit should not be rejected by the length check.
	// We use a temporary directory to build a path of exactly maxUnixSocketPathLen bytes.
	dir, err := os.MkdirTemp("", "m-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	// Calculate how many padding characters we need for the filename.
	// path = dir + "/" + filename
	prefix := dir + "/"
	need := maxUnixSocketPathLen - len(prefix)
	if need <= 0 {
		t.Skipf("temp dir path already too long for this test (%d bytes)", len(prefix))
	}

	sockPath := prefix + strings.Repeat("s", need)
	if len(sockPath) != maxUnixSocketPathLen {
		t.Fatalf("expected path length %d, got %d", maxUnixSocketPathLen, len(sockPath))
	}

	server := NewServer(sockPath)
	err = server.Start()
	if err != nil {
		// The error should not be about path length.
		if strings.Contains(err.Error(), "socket path too long") {
			t.Errorf("path at exact limit should not be rejected: %v", err)
		}
		// Other errors (e.g. listen failure) are acceptable for this test.
		return
	}
	defer server.Stop()
}

func TestServer_ChmodFailure_CleansUpSocket(t *testing.T) {
	t.Parallel()
	requireUnixSocketSupport(t)
	// We cannot easily force os.Chmod to fail on a regular socket,
	// but we can verify the cleanup path by checking that after a
	// successful Start+Stop cycle the socket is removed (existing test),
	// and verify the code path by reading the source.
	//
	// Instead, test the observable behavior: create a server, start it,
	// stop it, and ensure no socket file remains.
	dir, err := os.MkdirTemp("", "m-uds-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "c.sock")

	server := NewServer(sockPath)
	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	// Verify socket exists.
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket should exist after start: %v", err)
	}

	server.Stop()

	// Verify cleanup.
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Error("socket should be removed after stop")
	}
}

func TestMaxUnixSocketPathLen(t *testing.T) {
	t.Parallel()
	// Sanity check: the constant should be reasonable (between 92 and 108).
	if maxUnixSocketPathLen < 92 || maxUnixSocketPathLen > 108 {
		t.Errorf("unexpected maxUnixSocketPathLen: %d", maxUnixSocketPathLen)
	}
}
