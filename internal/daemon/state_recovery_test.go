package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type capturedLogger struct {
	lines []string
}

func (l *capturedLogger) logf(level LogLevel, format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf("[%d] ", level)+fmt.Sprintf(format, args...))
}

func (l *capturedLogger) joined() string { return strings.Join(l.lines, "\n") }

const validYAML = "command_id: cmd_1\nstate: planning\n"
const validYAMLAlt = "command_id: cmd_1\nstate: dispatched\n"
const corruptYAML = "command_id: cmd_1\n  : : ::: not yaml\n\t- ["

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
}

func TestRecoverStateDir_ValidYAMLUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cmd_1.yaml")
	writeFile(t, path, validYAML)

	logger := &capturedLogger{}
	recoverStateDir(dir, logger)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, validYAML, string(got))
	require.Empty(t, logger.lines, "no warnings expected: %s", logger.joined())
}

func TestRecoverStateDir_CorruptYAMLRestoredFromBak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cmd_1.yaml")
	bak := path + ".bak"
	writeFile(t, path, corruptYAML)
	writeFile(t, bak, validYAMLAlt)

	logger := &capturedLogger{}
	recoverStateDir(dir, logger)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, validYAMLAlt, string(got), "yaml should be restored from .bak")
	require.Contains(t, logger.joined(), "restored")
}

func TestRecoverStateDir_BothCorruptKeepsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cmd_1.yaml")
	bak := path + ".bak"
	writeFile(t, path, corruptYAML)
	writeFile(t, bak, corruptYAML)

	logger := &capturedLogger{}
	recoverStateDir(dir, logger) // must not panic / abort

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, corruptYAML, string(got), "corrupt yaml is left in place when bak is also corrupt")
	require.Contains(t, logger.joined(), "bak_corrupt")
}

func TestRecoverStateDir_NoBakKeepsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cmd_1.yaml")
	writeFile(t, path, corruptYAML)

	logger := &capturedLogger{}
	recoverStateDir(dir, logger)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, corruptYAML, string(got))
	require.Contains(t, logger.joined(), "no_backup")
}

func TestRecoverStateDir_MissingDirIsNoop(t *testing.T) {
	t.Parallel()
	logger := &capturedLogger{}
	recoverStateDir(filepath.Join(t.TempDir(), "does-not-exist"), logger)
	require.Empty(t, logger.lines)
}

// =============================================================================
// ORC-3: .bak restoration epoch rollback prevention
// =============================================================================

const yamlWithEpoch5 = "command_id: cmd_1\nstate: planning\nlease_epoch: 5\n"
const yamlWithEpoch3 = "command_id: cmd_1\nstate: dispatched\nlease_epoch: 3\n"
const yamlWithEpoch7 = "command_id: cmd_2\nstate: planning\nlease_epoch: 7\n"

func TestRecoverStateDir_ORC3_ClampsEpochOnRestore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// cmd_1.yaml is corrupt, its .bak has epoch 3
	path1 := filepath.Join(dir, "cmd_1.yaml")
	bak1 := path1 + ".bak"
	writeFile(t, path1, corruptYAML)
	writeFile(t, bak1, yamlWithEpoch3)

	// cmd_2.yaml is valid with epoch 7 (establishes the floor)
	path2 := filepath.Join(dir, "cmd_2.yaml")
	writeFile(t, path2, yamlWithEpoch7)

	logger := &capturedLogger{}
	recoverStateDir(dir, logger)

	// cmd_1 should be restored from .bak but epoch clamped to 7
	got, err := os.ReadFile(path1)
	require.NoError(t, err)
	require.Contains(t, string(got), "lease_epoch: 7", "epoch should be clamped to floor value")
	require.Contains(t, logger.joined(), "epoch_clamped")
}

func TestRecoverStateDir_ORC3_NoClampWhenEpochAboveFloor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// cmd_1.yaml is corrupt, its .bak has epoch 5
	path1 := filepath.Join(dir, "cmd_1.yaml")
	bak1 := path1 + ".bak"
	writeFile(t, path1, corruptYAML)
	writeFile(t, bak1, yamlWithEpoch5)

	// cmd_2.yaml is valid with epoch 3 (floor is 3, .bak epoch 5 is above floor)
	path2 := filepath.Join(dir, "cmd_2.yaml")
	writeFile(t, path2, yamlWithEpoch3)

	logger := &capturedLogger{}
	recoverStateDir(dir, logger)

	got, err := os.ReadFile(path1)
	require.NoError(t, err)
	require.Contains(t, string(got), "lease_epoch: 5", "epoch above floor should not be clamped")
	require.NotContains(t, logger.joined(), "epoch_clamped")
}

func TestRecoverStateDir_ORC3_NoClampWhenNoEpochInFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Files without lease_epoch should not be affected
	path := filepath.Join(dir, "cmd_1.yaml")
	bak := path + ".bak"
	writeFile(t, path, corruptYAML)
	writeFile(t, bak, validYAMLAlt)

	logger := &capturedLogger{}
	recoverStateDir(dir, logger)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, validYAMLAlt, string(got))
	require.NotContains(t, logger.joined(), "epoch_clamped")
}

func TestExtractLeaseEpochs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    []int
	}{
		{"no epoch", "state: planning\n", nil},
		{"single epoch", "lease_epoch: 5\n", []int{5}},
		{"multiple epochs", "lease_epoch: 3\ntask:\n  lease_epoch: 7\n", []int{3, 7}},
		{"zero epoch", "lease_epoch: 0\n", []int{0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractLeaseEpochs([]byte(tc.content))
			if len(tc.want) == 0 {
				require.Empty(t, got)
			} else {
				require.Equal(t, tc.want, got)
			}
		})
	}
}

func TestClampLeaseEpoch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		content  string
		floor    int
		expected string
		clamped  bool
	}{
		{"no clamp needed", "lease_epoch: 5\n", 3, "lease_epoch: 5\n", false},
		{"clamp to floor", "lease_epoch: 2\n", 5, "lease_epoch: 5\n", true},
		{"zero floor no-op", "lease_epoch: 2\n", 0, "lease_epoch: 2\n", false},
		{"equal no clamp", "lease_epoch: 5\n", 5, "lease_epoch: 5\n", false},
		{"multiple epochs mixed", "lease_epoch: 2\ntask:\n  lease_epoch: 8\n", 5,
			"lease_epoch: 5\ntask:\n  lease_epoch: 8\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger := &capturedLogger{}
			got := clampLeaseEpoch([]byte(tc.content), tc.floor, logger, "test.yaml")
			require.Equal(t, tc.expected, string(got))
			if tc.clamped {
				require.Contains(t, logger.joined(), "epoch_clamped")
			} else {
				require.NotContains(t, logger.joined(), "epoch_clamped")
			}
		})
	}
}
