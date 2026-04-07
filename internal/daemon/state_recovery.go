package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

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

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".bak") {
			continue
		}
		path := filepath.Join(stateDir, name)
		if err := parseYAMLFile(path); err == nil {
			continue
		} else {
			logger.logf(LogLevelWarn, "state_recovery yaml_corrupt path=%s error=%v", path, err)
		}

		bakPath := path + ".bak"
		if _, statErr := os.Stat(bakPath); statErr != nil {
			logger.logf(LogLevelWarn, "state_recovery no_backup path=%s", bakPath)
			continue
		}
		if err := parseYAMLFile(bakPath); err != nil {
			logger.logf(LogLevelWarn, "state_recovery bak_corrupt path=%s error=%v", bakPath, err)
			continue
		}

		bakContent, err := os.ReadFile(bakPath)
		if err != nil {
			logger.logf(LogLevelWarn, "state_recovery bak_read_failed path=%s error=%v", bakPath, err)
			continue
		}
		if err := yamlutil.AtomicWriteRaw(path, bakContent); err != nil {
			logger.logf(LogLevelWarn, "state_recovery restore_failed path=%s error=%v", path, err)
			continue
		}
		logger.logf(LogLevelInfo, "state_recovery restored path=%s from=%s", path, bakPath)
	}
}

func parseYAMLFile(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var v any
	if err := yamlv3.Unmarshal(content, &v); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}
