package formation

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
)

// testDownConfig returns a Config with a unique project name to prevent
// tmux prefix matching from accidentally targeting production sessions.
// RunDown sets the session name to "maestro-<project>", so using a unique
// name like "maestro-test-down-12345" avoids collisions with real sessions
// like "maestro-maestro_v2".
func testDownConfig(t *testing.T) model.Config {
	t.Helper()
	name := fmt.Sprintf("test-down-%d-%d", os.Getpid(), time.Now().UnixNano())
	return model.Config{
		Project: model.ProjectConfig{Name: name},
	}
}

// useTestSessionForDown saves the current tmux session name and restores it
// on cleanup, preventing RunDown from polluting the global session name.
func useTestSessionForDown(t *testing.T) {
	t.Helper()
	origName := tmux.GetSessionName()
	t.Cleanup(func() {
		tmux.SetSessionName(origName)
	})
}

func TestRunDown_DaemonNotRunning(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	useTestSessionForDown(t)

	// No socket file → daemon not running
	err := RunDown(maestroDir, testDownConfig(t))
	if err != nil {
		t.Fatalf("expected no error when daemon not running, got %v", err)
	}
}

func TestRunDown_SocketExistsButNoListener(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	useTestSessionForDown(t)

	// Create a fake socket file (not a real socket, just a regular file)
	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	if err := os.WriteFile(socketPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	// RunDown should handle connection error gracefully (no error returned)
	err := RunDown(maestroDir, testDownConfig(t))
	if err != nil {
		t.Fatalf("expected graceful handling when socket exists but no listener, got %v", err)
	}
}

func TestRunDown_UDSError_CleansStalePID(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	useTestSessionForDown(t)

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

	err := RunDown(maestroDir, testDownConfig(t))
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
	useTestSessionForDown(t)

	// No socket, but a stale PID file with matching lock
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	os.WriteFile(pidPath, []byte("99999999"), 0644)
	os.WriteFile(lockPath, []byte("99999999\n"), 0644)

	err := RunDown(maestroDir, testDownConfig(t))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// PID file should be cleaned up
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected daemon.pid to be removed by cleanupStalePID")
	}
}
