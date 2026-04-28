package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
)

// metricsLogAdapter adapts a core.DaemonLogger to the metrics.Logger interface.
type metricsLogAdapter struct {
	dl *core.DaemonLogger
}

func (a *metricsLogAdapter) Warnf(format string, args ...any) {
	a.dl.Logf(core.LogLevelWarn, format, args...)
}

// newMetricsHandler creates a metrics.Handler bridging daemon logging to the metrics.Logger interface.
func newMetricsHandler(maestroDir string, logger *log.Logger, logLevel core.LogLevel, clock metrics.Clock) *metrics.Handler {
	dl := core.NewDaemonLoggerFromLegacy("metrics", logger, logLevel)
	return metrics.NewHandler(maestroDir, &metricsLogAdapter{dl: dl}, clock)
}

// taskQueuesToSnapshots converts daemon-internal taskQueueEntry map to
// a slice of metrics.TaskQueueSnapshot for the metrics package.
func taskQueuesToSnapshots(taskQueues map[string]*taskQueueEntry) []metrics.TaskQueueSnapshot {
	snapshots := make([]metrics.TaskQueueSnapshot, 0, len(taskQueues))
	for path, tq := range taskQueues {
		wID := workerIDFromPath(path)
		snapshots = append(snapshots, metrics.TaskQueueSnapshot{
			WorkerID: wID,
			Tasks:    tq.Queue.Tasks,
		})
	}
	return snapshots
}

// updateDashboard delegates dashboard generation to DashboardFormatter.
// SIER-002: This eliminates the dual dashboard generation that previously existed
// between metrics collection (now in internal/metrics/) and DashboardFormatter (log-based).
func (qh *QueueHandler) updateDashboard(
	cq model.CommandQueue,
	taskQueues map[string]*taskQueueEntry,
	nq model.NotificationQueue,
) error {
	formatter := NewDashboardFormatter(qh.maestroDir)
	return formatter.UpdateDashboardFileWithQueues(cq, taskQueues, nq)
}

// atomicWriteText writes raw text to a file using temp+rename for atomicity.
func atomicWriteText(path string, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".maestro-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	// tmpName and path both live under the controlled maestroDir layout
	// (CreateTemp keyed by Dir(path)). gosec G703 (path traversal via
	// taint) cannot apply here because no user-controlled fragment is
	// concatenated into either path.
	return os.Rename(tmpName, path) //nolint:gosec // both paths are derived from a controlled maestroDir
}
