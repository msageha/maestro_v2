package daemon

import (
	"os"
	"path/filepath"
	"strings"

	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// recoverQueueFiles validates every queue YAML under <maestroDir>/queue and
// attempts to restore corrupted files from their sibling .bak. Recovery
// failures are logged as warnings; daemon startup is never aborted.
//
// F-034: prior to this routine, a queue file truncated by a SIGKILL or
// out-of-disk situation would silently start the daemon with an empty queue,
// erasing the in-flight `lease_epoch` history of every task it referenced.
// recoverQueueFiles parallels recoverStateFiles so the same .bak rotation
// produced by yaml.AtomicWriteRaw can be used to restore the prior payload.
func (d *Daemon) recoverQueueFiles() {
	recoverQueueDir(queueDirPath(d.maestroDir), daemonStateLogger{d: d})
}

// recoverQueueDir is the queue counterpart of recoverStateDir. The
// commandID-integrity guard (F-031) does not apply because queue files are
// keyed by worker / endpoint name (planner.yaml, orchestrator.yaml,
// planner_signals.yaml, worker{N}.yaml) and aggregate many command-scoped
// entries; instead the routine relies on the ORC-3 epoch floor clamp to
// keep restored lease_epoch values monotonic.
func recoverQueueDir(queueDir string, logger stateLogger) {
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.logf(LogLevelWarn, "queue_recovery readdir failed dir=%s error=%v", queueDir, err)
		}
		return
	}

	epochFloor := collectMaxLeaseEpoch(queueDir, entries, logger)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".bak") {
			continue
		}
		path := filepath.Join(queueDir, name)
		parseErr := parseYAMLFile(path)
		if parseErr == nil {
			continue
		}
		logger.logf(LogLevelWarn, "queue_recovery yaml_corrupt path=%s error=%v", path, parseErr)

		bakPath := path + ".bak"
		if _, statErr := os.Stat(bakPath); statErr != nil {
			logger.logf(LogLevelWarn, "queue_recovery no_backup path=%s", bakPath)
			continue
		}
		if err := parseYAMLFile(bakPath); err != nil {
			logger.logf(LogLevelWarn, "queue_recovery bak_corrupt path=%s error=%v", bakPath, err)
			continue
		}

		bakContent, err := os.ReadFile(bakPath) //nolint:gosec // bakPath is constructed from a controlled application queue directory
		if err != nil {
			logger.logf(LogLevelWarn, "queue_recovery bak_read_failed path=%s error=%v", bakPath, err)
			continue
		}

		// ORC-3: clamp lease_epoch values in the restored payload so a stale
		// .bak cannot resurrect a worker epoch that current dispatch has
		// already moved past. Same routine as state recovery.
		bakContent = clampLeaseEpoch(bakContent, epochFloor, logger, path)

		if err := yamlutil.AtomicWriteRaw(path, bakContent); err != nil {
			logger.logf(LogLevelWarn, "queue_recovery restore_failed path=%s error=%v", path, err)
			continue
		}
		logger.logf(LogLevelInfo, "queue_recovery restored path=%s from=%s", path, bakPath)
	}
}
