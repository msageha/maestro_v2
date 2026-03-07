package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// recoverPanic catches panics in goroutines to prevent the daemon from crashing.
// It logs the panic with a full stack trace and initiates a graceful shutdown.
func (d *Daemon) recoverPanic(goroutine string) {
	if r := recover(); r != nil {
		d.log(core.LogLevelError, "panic in %s: %v\n%s", goroutine, r, debug.Stack())
		go d.Shutdown()
	}
}

// validateLearningsFile checks the learnings file on daemon startup.
// If the file is corrupt, it uses the quarantine/recovery flow.
func (d *Daemon) validateLearningsFile() {
	learningsPath := filepath.Join(d.maestroDir, "state", "learnings.yaml")
	data, err := os.ReadFile(learningsPath)
	if os.IsNotExist(err) {
		return // No file yet — will be created on first write
	}
	if err != nil {
		d.log(core.LogLevelWarn, "learnings_startup_read error=%v", err)
		return
	}

	var lf struct {
		SchemaVersion int    `yaml:"schema_version"`
		FileType      string `yaml:"file_type"`
	}
	if err := yamlv3.Unmarshal(data, &lf); err != nil || lf.FileType != "state_learnings" {
		d.log(core.LogLevelWarn, "learnings_startup_corrupt, recovering")
		if recErr := yamlutil.RecoverCorruptedFile(d.maestroDir, learningsPath, "state_learnings"); recErr != nil {
			d.log(core.LogLevelError, "learnings_startup_recovery_failed: %v", recErr)
		}
	}
}

func (d *Daemon) log(level core.LogLevel, format string, args ...any) {
	if level < d.logLevel {
		return
	}
	levelStr := "INFO"
	switch level {
	case core.LogLevelDebug:
		levelStr = "DEBUG"
	case core.LogLevelWarn:
		levelStr = "WARN"
	case core.LogLevelError:
		levelStr = "ERROR"
	}
	msg := fmt.Sprintf(format, args...)
	d.logger.Printf("%s %s daemon: %s", d.clock.Now().Format(time.RFC3339), levelStr, msg)
}
