package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// leaseEpochRe matches "lease_epoch: <number>" in YAML content.
var leaseEpochRe = regexp.MustCompile(`(?m)^(\s*lease_epoch:\s*)(\d+)(.*)$`)

// stateLogger is the minimal logging interface used by recoverStateFiles so
// the routine can be exercised from tests without a full Daemon.
type stateLogger interface {
	logf(level LogLevel, format string, args ...any)
}

// daemonStateLogger adapts *Daemon to stateLogger.
type daemonStateLogger struct{ d *Daemon }

func (l daemonStateLogger) logf(level LogLevel, format string, args ...any) {
	l.d.log(level, format, args...)
}

// recoverStateFiles validates every command state YAML under
// <maestroDir>/state/commands and attempts to restore corrupted files from
// their sibling .bak. Recovery failures are logged as warnings; daemon
// startup is never aborted by this routine.
func (d *Daemon) recoverStateFiles() {
	stateDir := filepath.Join(d.maestroDir, "state", "commands")
	recoverStateDir(stateDir, daemonStateLogger{d: d})
}

func recoverStateDir(stateDir string, logger stateLogger) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		// Directory may not exist yet on a fresh install; that is fine.
		if !os.IsNotExist(err) {
			logger.logf(LogLevelWarn, "state_recovery readdir failed dir=%s error=%v", stateDir, err)
		}
		return
	}

	// ORC-3: Collect max lease_epoch from all valid YAML files before recovery.
	// This prevents .bak restoration from rolling back epoch values, which could
	// cause fencing violations (stale epoch accepted as current).
	epochFloor := collectMaxLeaseEpoch(stateDir, entries, logger)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".bak") {
			continue
		}
		path := filepath.Join(stateDir, name)
		parseErr := parseYAMLFile(path)
		if parseErr == nil {
			continue
		}
		logger.logf(LogLevelWarn, "state_recovery yaml_corrupt path=%s error=%v", path, parseErr)

		bakPath := path + ".bak"
		if _, statErr := os.Stat(bakPath); statErr != nil {
			logger.logf(LogLevelWarn, "state_recovery no_backup path=%s", bakPath)
			continue
		}
		if err := parseYAMLFile(bakPath); err != nil {
			logger.logf(LogLevelWarn, "state_recovery bak_corrupt path=%s error=%v", bakPath, err)
			continue
		}

		bakContent, err := os.ReadFile(bakPath) //nolint:gosec // bakPath is constructed from a controlled application state directory
		if err != nil {
			logger.logf(LogLevelWarn, "state_recovery bak_read_failed path=%s error=%v", bakPath, err)
			continue
		}

		// ORC-3: Clamp lease_epoch values in restored content to the floor.
		bakContent = clampLeaseEpoch(bakContent, epochFloor, logger, path)

		if err := yamlutil.AtomicWriteRaw(path, bakContent); err != nil {
			logger.logf(LogLevelWarn, "state_recovery restore_failed path=%s error=%v", path, err)
			continue
		}
		logger.logf(LogLevelInfo, "state_recovery restored path=%s from=%s", path, bakPath)
	}
}

// collectMaxLeaseEpoch scans all valid YAML files in the directory and returns
// the maximum lease_epoch value found. Returns 0 if none found.
func collectMaxLeaseEpoch(stateDir string, entries []os.DirEntry, logger stateLogger) int {
	maxEpoch := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".bak") {
			continue
		}
		path := filepath.Join(stateDir, name)
		content, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled directory
		if err != nil {
			continue
		}
		for _, epoch := range extractLeaseEpochs(content) {
			if epoch > maxEpoch {
				maxEpoch = epoch
			}
		}
	}
	return maxEpoch
}

// extractLeaseEpochs returns all lease_epoch integer values found in YAML content.
func extractLeaseEpochs(content []byte) []int {
	matches := leaseEpochRe.FindAllSubmatch(content, -1)
	epochs := make([]int, 0, len(matches))
	for _, m := range matches {
		if v, err := strconv.Atoi(string(m[2])); err == nil {
			epochs = append(epochs, v)
		}
	}
	return epochs
}

// clampLeaseEpoch replaces any lease_epoch value below floor with floor in the
// raw YAML content, preserving formatting. Returns the (possibly modified) content.
func clampLeaseEpoch(content []byte, floor int, logger stateLogger, path string) []byte {
	if floor <= 0 {
		return content
	}
	return leaseEpochRe.ReplaceAllFunc(content, func(match []byte) []byte {
		parts := leaseEpochRe.FindSubmatch(match)
		v, err := strconv.Atoi(string(parts[2]))
		if err != nil {
			return match
		}
		if v < floor {
			logger.logf(LogLevelWarn, "state_recovery epoch_clamped path=%s old_epoch=%d floor=%d", path, v, floor)
			return []byte(string(parts[1]) + strconv.Itoa(floor) + string(parts[3]))
		}
		return match
	})
}

func parseYAMLFile(path string) error {
	content, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var v any
	if err := yamlv3.Unmarshal(content, &v); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}
