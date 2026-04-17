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

// =============================================================================
// computeEpochFloor boundary value tests
// =============================================================================

func TestComputeEpochFloor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		epochs     []int
		want       int
		warnSubstr string // if non-empty, expect this substring in logger output
	}{
		// Empty input
		{"empty_input", nil, 0, ""},
		{"empty_slice", []int{}, 0, ""},

		// epoch = 0 (unset/default — should be ignored)
		{"all_zeros", []int{0, 0, 0}, 0, ""},
		{"zero_and_valid", []int{0, 5, 0}, 5, ""},
		{"single_zero", []int{0}, 0, ""},

		// epoch = 1 (minimum valid)
		{"single_one", []int{1}, 1, ""},
		{"one_and_zero", []int{0, 1}, 1, ""},
		{"one_is_max", []int{1, 1, 1}, 1, ""},

		// Normal positive values
		{"single_positive", []int{5}, 5, ""},
		{"multiple_positive", []int{3, 7, 5}, 7, ""},
		{"large_value", []int{1, 999999999}, 999999999, ""},

		// epoch = max int (near overflow boundary)
		{"max_int", []int{1, int(^uint(0) >> 1)}, int(^uint(0) >> 1), ""},

		// Negative values (invalid — should warn and treat as 1)
		{"single_negative", []int{-1}, 1, "invalid_epoch"},
		{"negative_with_valid", []int{-5, 10}, 10, "invalid_epoch"},
		{"all_negative", []int{-1, -2, -3}, 1, "invalid_epoch"},
		{"negative_and_zero", []int{-1, 0}, 1, "invalid_epoch"},

		// Mixed edge cases
		{"zero_negative_valid", []int{0, -1, 3}, 3, "invalid_epoch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger := &capturedLogger{}
			got := computeEpochFloor(tc.epochs, logger)
			require.Equal(t, tc.want, got, "computeEpochFloor(%v)", tc.epochs)
			if tc.warnSubstr != "" {
				require.Contains(t, logger.joined(), tc.warnSubstr)
			}
		})
	}
}

// TestComputeEpochFloor_ConcurrentRecovery verifies that computeEpochFloor is
// deterministic: calling it with the same inputs always produces the same floor,
// simulating concurrent recovery where multiple goroutines might compute the
// floor from the same snapshot of files.
func TestComputeEpochFloor_ConcurrentRecovery(t *testing.T) {
	t.Parallel()
	epochs := []int{3, 7, 0, 5, -1, 10, 0}

	const goroutines = 20
	results := make(chan int, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			logger := &capturedLogger{}
			results <- computeEpochFloor(epochs, logger)
		}()
	}
	for i := 0; i < goroutines; i++ {
		got := <-results
		require.Equal(t, 10, got, "concurrent call should produce deterministic result")
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
