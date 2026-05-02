package uds

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIsTransientDialError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"EAGAIN", syscall.EAGAIN, true},
		{"ENOENT", syscall.ENOENT, true},
		{"EACCES", syscall.EACCES, false},
		{"EPERM", syscall.EPERM, false},
		{"EADDRINUSE", syscall.EADDRINUSE, false},
		{"ENOTDIR", syscall.ENOTDIR, false},
		{"ENOTSOCK", syscall.ENOTSOCK, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isTransientDialError(tt.err)
			if got != tt.want {
				t.Errorf("isTransientDialError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsPermanentDialError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"EACCES", syscall.EACCES, true},
		{"EPERM", syscall.EPERM, true},
		{"EADDRINUSE", syscall.EADDRINUSE, true},
		{"ENOTDIR", syscall.ENOTDIR, true},
		{"ENAMETOOLONG", syscall.ENAMETOOLONG, true},
		{"ENOTSOCK", syscall.ENOTSOCK, true},
		{"EPROTOTYPE", syscall.EPROTOTYPE, true},
		{"EAFNOSUPPORT", syscall.EAFNOSUPPORT, true},
		{"ECONNREFUSED_not_permanent", syscall.ECONNREFUSED, false},
		{"EAGAIN_not_permanent", syscall.EAGAIN, false},
		{"ENOENT_not_permanent", syscall.ENOENT, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isPermanentDialError(tt.err)
			if got != tt.want {
				t.Errorf("isPermanentDialError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestDialWithRetry_PermanentErrorFailsImmediately(t *testing.T) {
	// Dial a path whose parent is a regular file (not a directory) —
	// connect(2) returns ENOTDIR on both Linux and macOS, which our
	// permanent-error classifier treats as non-retryable. Earlier this
	// test dialled a regular file directly, which yields ENOTSOCK on
	// macOS (permanent) but ECONNREFUSED on Linux (transient), so the
	// assertion was platform-dependent and broke CI on Linux.
	dir, err := os.MkdirTemp("", "m-uds-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	parentFile := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("regular file"), 0600); err != nil {
		t.Fatalf("create file: %v", err)
	}
	filePath := filepath.Join(parentFile, "child")

	client := NewClient(filePath)
	client.SetTimeout(2 * time.Second)

	start := time.Now()
	_, err = client.SendCommand("ping", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for non-socket path")
	}
	// Permanent error should fail without retry backoff.
	// First retry backoff is 100ms, so < 100ms means no retries.
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected immediate failure for permanent error, took %v", elapsed)
	}
	if !strings.Contains(err.Error(), "cannot connect to daemon") {
		t.Errorf("expected 'cannot connect to daemon' message, got: %v", err)
	}
}

func TestDialWithRetry_PermissionDeniedFailsImmediately(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user")
	}

	dir, err := os.MkdirTemp("", "m-uds-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	restrictedDir := filepath.Join(dir, "noaccess")
	if err := os.Mkdir(restrictedDir, 0000); err != nil {
		t.Fatalf("create restricted dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(restrictedDir, 0755) })

	sockPath := filepath.Join(restrictedDir, "t.sock")
	client := NewClient(sockPath)
	client.SetTimeout(2 * time.Second)

	start := time.Now()
	_, err = client.SendCommand("ping", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for permission denied")
	}
	// Should fail immediately without retries.
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected immediate failure for permission denied, took %v", elapsed)
	}
}

func TestSend_UsesTimeoutContext(t *testing.T) {
	// Verify send() completes (with error) within the client timeout,
	// even though the socket doesn't exist (transient ENOENT exhausts retries).
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "no.sock")

	client := NewClient(sockPath)
	client.SetTimeout(2 * time.Second)

	start := time.Now()
	_, err := client.SendCommand("ping", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error when daemon not running")
	}
	// With 2s timeout, 3 retries, and backoff (100ms + 200ms), should complete
	// well within the timeout. Definitely should not hang.
	if elapsed > 5*time.Second {
		t.Errorf("send() took %v, expected it to respect timeout", elapsed)
	}
}

func TestTransientAndPermanentSetsAreDisjoint(t *testing.T) {
	t.Parallel()
	// Verify no errno is classified as both transient and permanent.
	errnos := []syscall.Errno{
		syscall.ECONNREFUSED, syscall.EAGAIN, syscall.ENOENT,
		syscall.EACCES, syscall.EPERM, syscall.EADDRINUSE,
		syscall.ENOTDIR, syscall.ENAMETOOLONG, syscall.ENOTSOCK,
		syscall.EPROTOTYPE, syscall.EAFNOSUPPORT,
	}
	for _, e := range errnos {
		if isTransientDialError(e) && isPermanentDialError(e) {
			t.Errorf("errno %v is classified as both transient and permanent", e)
		}
	}
}
