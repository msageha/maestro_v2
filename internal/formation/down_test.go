package formation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/uds"
)

func TestRunDown_DaemonNotRunning(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	// No socket file → daemon not running
	err := RunDown(maestroDir)
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
	err := RunDown(maestroDir)
	if err != nil {
		t.Fatalf("expected graceful handling when socket exists but no listener, got %v", err)
	}
}
