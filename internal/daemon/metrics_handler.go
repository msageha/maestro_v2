package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/dashboard"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// ScanCounters tracks cumulative counters during a single PeriodicScan cycle.
type ScanCounters struct {
	CommandsDispatched    int
	TasksDispatched       int
	TasksCompleted        int
	TasksFailed           int
	TasksCancelled        int
	DeadLetters           int
	ReconciliationRepairs int
	NotificationRetries   int
	SignalDeliveries      int
	SignalRetries         int
	LeaseRenewals         int
	LeaseExtensions       int
	LeaseReleases         int
}

// MetricsHandler generates metrics and dashboard files.
type MetricsHandler struct {
	maestroDir string
	config     model.Config
	dl         *DaemonLogger
	logger     *log.Logger
	logLevel   LogLevel
	clock      Clock
}

// NewMetricsHandler creates a new MetricsHandler.
func NewMetricsHandler(maestroDir string, cfg model.Config, logger *log.Logger, logLevel LogLevel) *MetricsHandler {
	return &MetricsHandler{
		maestroDir: maestroDir,
		config:     cfg,
		dl:         NewDaemonLoggerFromLegacy("metrics", logger, logLevel),
		logger:     logger,
		logLevel:   logLevel,
		clock:      RealClock{},
	}
}

// UpdateMetrics loads existing metrics, merges scan counters, and writes state/metrics.yaml.
func (mh *MetricsHandler) UpdateMetrics(
	cq model.CommandQueue,
	taskQueues map[string]*taskQueueEntry,
	nq model.NotificationQueue,
	scanStart time.Time,
	scanDuration time.Duration,
	counters *ScanCounters,
) error {
	metricsPath := filepath.Join(mh.maestroDir, "state", "metrics.yaml")
	if err := os.MkdirAll(filepath.Dir(metricsPath), 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	var metrics model.Metrics
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read metrics: %w", err)
		}
		metrics.SchemaVersion = 1
		metrics.FileType = "state_metrics"
	} else {
		if err := yamlv3.Unmarshal(data, &metrics); err != nil {
			return fmt.Errorf("parse metrics: %w", err)
		}
	}

	// Compute queue depth
	metrics.QueueDepth = mh.computeQueueDepth(cq, taskQueues, nq)

	// --- Counter update strategy (B-004 documentation) ---
	//
	// MetricsHandler uses two distinct counter update patterns. Both are safe
	// because UpdateMetrics is called exclusively from the daemon's single-writer
	// goroutine (PeriodicScan). No mutex or atomic operations are needed.
	//
	// Pattern 1: RE-COMPUTED counters (overwrite each scan)
	//   TasksCompleted and TasksFailed are re-counted from result files every
	//   scan cycle. This ensures accuracy even after queue archival or process
	//   restart, since result files are the persistent source of truth.
	//
	// Pattern 2: INCREMENTAL counters (additive across scans)
	//   CommandsDispatched, TasksDispatched, etc. are accumulated from per-scan
	//   deltas. These track transient events that are not persisted elsewhere.
	//   Values survive daemon restarts via the metrics.yaml file (loaded at scan
	//   start, incremented, then written back).
	//
	// Mixing these patterns is intentional and correct: re-computed counters
	// self-heal on restart, while incremental counters preserve cumulative
	// history. The single-writer goroutine confinement guarantees no data races.

	// Pattern 1: Re-compute completed/failed from result files.
	// Result files are the authoritative source: both normal result_write and
	// dead-letter post-processing write to results/*.yaml, so archived queue
	// entries don't cause counter regression.
	resultFiles := mh.loadAllResultFiles()
	resultCompleted := 0
	resultFailed := 0
	for _, rf := range resultFiles {
		// Skip non-task result files (e.g. planner.yaml which is result_command)
		if rf.FileType != "result_task" {
			continue
		}
		for _, r := range rf.Results {
			switch r.Status {
			case model.StatusCompleted:
				resultCompleted++
			case model.StatusFailed:
				resultFailed++
			}
		}
	}
	metrics.Counters.TasksCompleted = resultCompleted
	metrics.Counters.TasksFailed = resultFailed

	// Pattern 2: Merge incremental counters (additive across scans).
	metrics.Counters.CommandsDispatched += counters.CommandsDispatched
	metrics.Counters.TasksDispatched += counters.TasksDispatched
	metrics.Counters.TasksCancelled += counters.TasksCancelled
	metrics.Counters.DeadLetters += counters.DeadLetters
	metrics.Counters.ReconciliationRepairs += counters.ReconciliationRepairs
	metrics.Counters.NotificationRetries += counters.NotificationRetries
	metrics.Counters.LeaseRenewals += counters.LeaseRenewals
	metrics.Counters.LeaseExtensions += counters.LeaseExtensions
	metrics.Counters.LeaseReleases += counters.LeaseReleases

	// Update heartbeat and timestamp
	heartbeat := scanStart.UTC().Format(time.RFC3339)
	metrics.DaemonHeartbeat = &heartbeat
	now := mh.clock.Now().UTC().Format(time.RFC3339)
	metrics.UpdatedAt = &now

	return yamlutil.AtomicWrite(metricsPath, metrics)
}

// UpdateDashboard delegates dashboard generation to DashboardFormatter,
// which reads JSONL logs and combines with live queue data to produce a unified
// human-readable dashboard.
// SIER-002: This eliminates the dual dashboard generation that previously existed
// between MetricsHandler (queue-based) and DashboardFormatter (log-based).
func (mh *MetricsHandler) UpdateDashboard(
	cq model.CommandQueue,
	taskQueues map[string]*taskQueueEntry,
	nq model.NotificationQueue,
) error {
	formatter := NewDashboardFormatter(mh.maestroDir)
	// Convert internal taskQueueEntry to dashboard.TaskQueueEntry
	dashTQ := make(map[string]*dashboard.TaskQueueEntry, len(taskQueues))
	for k, v := range taskQueues {
		dashTQ[k] = &dashboard.TaskQueueEntry{Queue: v.Queue, Path: v.Path}
	}
	return formatter.UpdateDashboardFileWithQueues(cq, dashTQ, nq, workerIDFromPath, atomicWriteText)
}

// loadAllResultFiles loads all results/worker{N}.yaml and results/planner.yaml files.
func (mh *MetricsHandler) loadAllResultFiles() map[string]*model.TaskResultFile {
	resultsDir := filepath.Join(mh.maestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			mh.log(LogLevelWarn, "read results dir: %v", err)
		}
		return nil
	}

	result := make(map[string]*model.TaskResultFile)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}

		path := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			mh.log(LogLevelWarn, "read result file %s: %v", name, err)
			continue
		}

		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			mh.log(LogLevelWarn, "parse result file %s: %v", name, err)
			continue
		}

		wID := strings.TrimSuffix(name, ".yaml")
		result[wID] = &rf
	}
	return result
}

// computeQueueDepth counts pending entries in each queue.
func (mh *MetricsHandler) computeQueueDepth(
	cq model.CommandQueue,
	taskQueues map[string]*taskQueueEntry,
	nq model.NotificationQueue,
) model.QueueDepth {
	depth := model.QueueDepth{
		Workers: make(map[string]int),
	}

	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusPending {
			depth.Planner++
		}
	}

	for _, ntf := range nq.Notifications {
		if ntf.Status == model.StatusPending {
			depth.Orchestrator++
		}
	}

	for path, tq := range taskQueues {
		wID := workerIDFromPath(path)
		if wID == "" {
			continue
		}
		count := 0
		for _, task := range tq.Queue.Tasks {
			if task.Status == model.StatusPending {
				count++
			}
		}
		depth.Workers[wID] = count
	}

	return depth
}

// atomicWriteText writes raw text to a file using temp+rename for atomicity.
func atomicWriteText(path string, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".maestro-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.WriteString(content); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	return os.Rename(tmpName, path)
}

func (mh *MetricsHandler) log(level LogLevel, format string, args ...any) {
	mh.dl.Logf(level, format, args...)
}
