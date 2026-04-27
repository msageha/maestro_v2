package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const validQueueYAML = "schema_version: 1\nfile_type: queue_task\ntasks:\n  - id: task_1\n    lease_epoch: 1\n"
const validQueueYAMLBak = "schema_version: 1\nfile_type: queue_task\ntasks:\n  - id: task_1\n    lease_epoch: 1\n"
const corruptQueueYAML = "schema_version: 1\n  : : ::: not yaml\n\t- ["

func TestRecoverQueueDir_ValidYAMLUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "worker1.yaml")
	writeFile(t, path, validQueueYAML)

	logger := &capturedLogger{}
	recoverQueueDir(dir, logger)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, validQueueYAML, string(got))
	require.Empty(t, logger.lines, "no warnings expected: %s", logger.joined())
}

func TestRecoverQueueDir_CorruptYAMLRestoredFromBak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "worker1.yaml")
	bak := path + ".bak"
	writeFile(t, path, corruptQueueYAML)
	writeFile(t, bak, validQueueYAMLBak)

	logger := &capturedLogger{}
	recoverQueueDir(dir, logger)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, validQueueYAMLBak, string(got), "yaml should be restored from .bak")
	if !strings.Contains(logger.joined(), "queue_recovery yaml_corrupt") {
		t.Errorf("expected queue_recovery yaml_corrupt warning, got: %s", logger.joined())
	}
	if !strings.Contains(logger.joined(), "queue_recovery restored") {
		t.Errorf("expected queue_recovery restored info log, got: %s", logger.joined())
	}
}

func TestRecoverQueueDir_NoBackupLeavesCorruptInPlace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "worker1.yaml")
	writeFile(t, path, corruptQueueYAML)

	logger := &capturedLogger{}
	recoverQueueDir(dir, logger)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, corruptQueueYAML, string(got), "without .bak the corrupt file is left untouched")
	if !strings.Contains(logger.joined(), "queue_recovery no_backup") {
		t.Errorf("expected queue_recovery no_backup warning, got: %s", logger.joined())
	}
}

func TestRecoverQueueDir_EpochFloorClampsRestoredEpoch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Healthy file establishes a higher floor (epoch=5).
	healthy := filepath.Join(dir, "worker1.yaml")
	writeFile(t, healthy, "schema_version: 1\nfile_type: queue_task\ntasks:\n  - id: task_a\n    lease_epoch: 5\n")
	// Corrupt file plus a stale .bak with epoch=2 — must be clamped to 5.
	corrupt := filepath.Join(dir, "worker2.yaml")
	bak := corrupt + ".bak"
	writeFile(t, corrupt, corruptQueueYAML)
	writeFile(t, bak, "schema_version: 1\nfile_type: queue_task\ntasks:\n  - id: task_b\n    lease_epoch: 2\n")

	logger := &capturedLogger{}
	recoverQueueDir(dir, logger)

	got, err := os.ReadFile(corrupt)
	require.NoError(t, err)
	if !strings.Contains(string(got), "lease_epoch: 5") {
		t.Errorf("expected restored payload to have lease_epoch clamped to 5, got: %s", string(got))
	}
}

func TestRecoverQueueDir_MissingDirNoOp(t *testing.T) {
	t.Parallel()
	logger := &capturedLogger{}
	recoverQueueDir(filepath.Join(t.TempDir(), "nonexistent"), logger)
	require.Empty(t, logger.lines, "missing dir should not warn")
}
