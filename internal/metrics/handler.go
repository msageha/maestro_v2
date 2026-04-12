package metrics

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// Handler generates and persists metrics to state/metrics.yaml.
type Handler struct {
	core.LogMixin
	maestroDir string
	config     model.Config
	clock      core.Clock
}

// NewHandler creates a new Handler.
func NewHandler(maestroDir string, cfg model.Config, logger *log.Logger, logLevel core.LogLevel) *Handler {
	return &Handler{
		LogMixin:   core.LogMixin{DL: core.NewDaemonLoggerFromLegacy("metrics", logger, logLevel)},
		maestroDir: maestroDir,
		config:     cfg,
		clock:      core.RealClock{},
	}
}

// UpdateMetrics loads existing metrics, merges scan counters, and writes state/metrics.yaml.
func (h *Handler) UpdateMetrics(
	cq model.CommandQueue,
	taskSnapshots []TaskQueueSnapshot,
	nq model.NotificationQueue,
	scanStart time.Time,
	_ time.Duration,
	counters *ScanCounters,
	gauges Gauges,
) error {
	metricsPath := filepath.Join(h.maestroDir, "state", "metrics.yaml")
	if err := os.MkdirAll(filepath.Dir(metricsPath), 0755); err != nil { //nolint:gosec // 0755 is appropriate for a state directory
		return fmt.Errorf("create state dir: %w", err)
	}

	var metrics model.Metrics
	data, err := os.ReadFile(metricsPath) //nolint:gosec // metricsPath is constructed from a controlled application state directory
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
	metrics.QueueDepth = h.computeQueueDepth(cq, taskSnapshots, nq)

	// --- Counter update strategy (B-004 documentation) ---
	//
	// Handler uses two distinct counter update patterns. Both are safe
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
	resultFiles := h.loadAllResultFiles()
	resultCompleted := 0
	resultFailed := 0
	for _, rf := range resultFiles {
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
	metrics.Counters.SignalDeliveries += counters.SignalDeliveries
	metrics.Counters.SignalRetries += counters.SignalRetries
	metrics.Counters.SignalDeadLetters += counters.SignalDeadLetters
	metrics.Counters.LeaseRenewals += counters.LeaseRenewals
	metrics.Counters.LeaseExtensions += counters.LeaseExtensions
	metrics.Counters.LeaseReleases += counters.LeaseReleases

	// Snapshot gauges (overwrite each scan).
	metrics.WorktreeCommandsStalled = gauges.WorktreeCommandsStalled
	metrics.BakFilesCount = gauges.BakFilesCount

	// Update heartbeat and timestamp
	heartbeat := scanStart.UTC().Format(time.RFC3339)
	metrics.DaemonHeartbeat = &heartbeat
	now := h.clock.Now().UTC().Format(time.RFC3339)
	metrics.UpdatedAt = &now

	return yamlutil.AtomicWrite(metricsPath, metrics)
}

// loadAllResultFiles loads all results/worker{N}.yaml and results/planner.yaml files.
func (h *Handler) loadAllResultFiles() map[string]*model.TaskResultFile {
	resultsDir := filepath.Join(h.maestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			h.Log(core.LogLevelWarn, "read results dir: %v", err)
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
		data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
		if err != nil {
			h.Log(core.LogLevelWarn, "read result file %s: %v", name, err)
			continue
		}

		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			h.Log(core.LogLevelWarn, "parse result file %s: %v", name, err)
			continue
		}

		wID := strings.TrimSuffix(name, ".yaml")
		result[wID] = &rf
	}
	return result
}

// computeQueueDepth counts pending entries in each queue.
func (h *Handler) computeQueueDepth(
	cq model.CommandQueue,
	taskSnapshots []TaskQueueSnapshot,
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

	for _, snap := range taskSnapshots {
		if snap.WorkerID == "" {
			continue
		}
		count := 0
		for _, task := range snap.Tasks {
			if task.Status == model.StatusPending {
				count++
			}
		}
		depth.Workers[snap.WorkerID] = count
	}

	return depth
}

