package formation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

func TestRunDown_DaemonNotRunning(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	// No socket file → daemon not running
	err := RunDown(maestroDir, model.Config{})
	if err != nil {
		t.Fatalf("expected no error when daemon not running, got %v", err)
	}
}

func TestRunDown_SocketExistsButNoListener(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	// Create a fake socket file (not a real socket, just a regular file)
	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	if err := os.WriteFile(socketPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	// RunDown should handle connection error gracefully (no error returned)
	err := RunDown(maestroDir, model.Config{})
	if err != nil {
		t.Fatalf("expected graceful handling when socket exists but no listener, got %v", err)
	}
}

func TestRunDown_UDSError_CleansStalePID(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	// Create a fake socket (triggers UDS path, but connection will fail)
	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	if err := os.WriteFile(socketPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	// Write a PID file with a dead PID + matching lock file
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	os.WriteFile(pidPath, []byte("99999999"), 0644)
	os.WriteFile(lockPath, []byte("99999999\n"), 0644)

	err := RunDown(maestroDir, model.Config{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// PID file should be cleaned up after UDS error
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected daemon.pid to be removed after UDS error path")
	}

	// Socket should be cleaned up
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Error("expected socket to be removed after UDS error path")
	}
}

func TestRunDown_DaemonNotRunning_CleansStalePID(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	// No socket, but a stale PID file with matching lock
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	os.WriteFile(pidPath, []byte("99999999"), 0644)
	os.WriteFile(lockPath, []byte("99999999\n"), 0644)

	err := RunDown(maestroDir, model.Config{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// PID file should be cleaned up
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected daemon.pid to be removed by cleanupStalePID")
	}
}
