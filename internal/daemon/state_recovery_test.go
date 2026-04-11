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
