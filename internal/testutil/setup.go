// Package testutil provides shared test fixtures and helpers used across the
// maestro test suite (temp directories, git repo init, fs permission probes,
// and minimal mock implementations).
package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// standardDirs is the superset of subdirectories used by .maestro tests.
var standardDirs = []string{
	"queue", "results", "state/commands", "state/worktrees",
	"logs", "dead_letters", "quarantine", "locks",
}

// PopulateDirs creates the standard .maestro subdirectory structure under the
// given directory. Use this when the caller already owns the parent directory.
func PopulateDirs(t *testing.T, maestroDir string) {
	t.Helper()
	for _, d := range standardDirs {
		if err := os.MkdirAll(filepath.Join(maestroDir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
}

// SetupDir creates a temporary .maestro directory structure for testing.
// It returns the path to the .maestro directory.
func SetupDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	PopulateDirs(t, maestroDir)
	return maestroDir
}

// SetupDirWithQueues creates a temporary .maestro directory with empty worker
// queue files. It returns the path to the .maestro directory.
func SetupDirWithQueues(t *testing.T, workerCount int) string {
	t.Helper()
	maestroDir := SetupDir(t)

	for i := 1; i <= workerCount; i++ {
		tq := model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks:         []model.Task{},
		}
		queueFile := filepath.Join(maestroDir, "queue", fmt.Sprintf("worker%d.yaml", i))
		if err := yamlutil.AtomicWrite(queueFile, tq); err != nil {
			t.Fatalf("write worker queue %d: %v", i, err)
		}
	}

	return maestroDir
}
