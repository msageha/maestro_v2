//revive:disable-next-line:var-naming // internal/metrics is intentionally named to mirror runtime/metrics' purpose; no import collision in this codebase
package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/clock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// Clock is an alias for clock.Clock kept here so that existing
// metrics.NewHandler / metrics.Clock call sites continue to compile. New code
// should depend on internal/clock directly. F-038.
type Clock = clock.Clock

// Logger provides warning-level logging for the metrics handler.
type Logger interface {
	Warnf(format string, args ...any)
}

// resultFileEntry holds cached result file data keyed by modtime + size.
// Size participates in the staleness check because filesystems with coarse
// timestamp granularity can give two successive atomic renames the same
// modtime; size catches most of those rewrites.
type resultFileEntry struct {
	modTime time.Time
	size    int64
	file    *model.TaskResultFile
}

// Handler generates and persists metrics to state/metrics.yaml.
type Handler struct {
	logger      Logger
	maestroDir  string
	clock       Clock
	resultCache map[string]*resultFileEntry
	// usageCollector feeds the opt-in cost/token usage section. Lazily
	// initialized to the claude-code session-file collector on the first
	// scan with cost_tracking.enabled, unless explicitly set (or cleared)
	// via SetUsageCollector.
	usageCollector    UsageCollector
	usageCollectorSet bool
	// usageLastCollectedAt is when the last usage collection pass ran.
	// Collection re-stats every retained session transcript, so it is
	// throttled to cost_tracking.collect_interval_sec instead of running
	// on every scan tick; the zero value forces the first scan to collect.
	usageLastCollectedAt time.Time
}

// NewHandler creates a new Handler.
func NewHandler(maestroDir string, logger Logger, clock Clock) *Handler {
	return &Handler{
		logger:      logger,
		maestroDir:  maestroDir,
		clock:       clock,
		resultCache: make(map[string]*resultFileEntry),
	}
}

// UpdateMetrics loads existing metrics, merges scan counters, and writes state/metrics.yaml.
func (h *Handler) UpdateMetrics(
	cq model.CommandQueue,
	taskSnapshots []TaskQueueSnapshot,
	nq model.NotificationQueue,
	scanStart time.Time,
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
		// SafeUnmarshal enforces anchor/alias limits (billion-laughs
		// defence) on the state file before the full decode.
		if err := yamlutil.SafeUnmarshal(data, &metrics); err != nil {
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
	metrics.Counters.SignalInlineRetrySuccesses += counters.SignalInlineRetrySuccesses
	metrics.Counters.LeaseRenewals += counters.LeaseRenewals
	metrics.Counters.LeaseExtensions += counters.LeaseExtensions
	metrics.Counters.LeaseReleases += counters.LeaseReleases

	// Snapshot gauges (overwrite each scan).
	metrics.WorktreeCommandsStalled = gauges.WorktreeCommandsStalled
	metrics.BakFilesCount = gauges.BakFilesCount

	// Opt-in cost/token usage (issue #32): recomputed from runtime session
	// files each scan; best-effort, never fails the metrics write.
	h.updateUsage(&metrics)

	// Update heartbeat and timestamp
	heartbeat := scanStart.UTC().Format(time.RFC3339)
	metrics.DaemonHeartbeat = &heartbeat
	now := h.clock.Now().UTC().Format(time.RFC3339)
	metrics.UpdatedAt = &now

	return yamlutil.AtomicWrite(metricsPath, metrics)
}

// loadAllResultFiles loads all results/worker{N}.yaml and results/planner.yaml files.
// It uses modtime-based caching to skip re-reading files that have not changed since the last call.
func (h *Handler) loadAllResultFiles() map[string]*model.TaskResultFile {
	resultsDir := filepath.Join(h.maestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			h.logger.Warnf("read results dir: %v", err)
		}
		return nil
	}

	currentFiles := make(map[string]struct{})
	result := make(map[string]*model.TaskResultFile)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		currentFiles[name] = struct{}{}

		info, err := entry.Info()
		if err != nil {
			h.logger.Warnf("stat result file %s: %v", name, err)
			continue
		}
		modTime := info.ModTime()

		// Use cached entry if modtime and size are unchanged.
		if cached, ok := h.resultCache[name]; ok && cached.modTime.Equal(modTime) && cached.size == info.Size() {
			wID := strings.TrimSuffix(name, ".yaml")
			result[wID] = cached.file
			continue
		}

		// Cache miss or stale: read and parse.
		path := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
		if err != nil {
			h.logger.Warnf("read result file %s: %v", name, err)
			continue
		}

		var rf model.TaskResultFile
		if err := yamlutil.SafeUnmarshal(data, &rf); err != nil {
			h.logger.Warnf("parse result file %s: %v", name, err)
			continue
		}

		h.resultCache[name] = &resultFileEntry{modTime: modTime, size: info.Size(), file: &rf}
		wID := strings.TrimSuffix(name, ".yaml")
		result[wID] = &rf
	}

	// Evict cache entries for deleted files.
	for name := range h.resultCache {
		if _, exists := currentFiles[name]; !exists {
			delete(h.resultCache, name)
		}
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
		// WorkerID may be empty for task queues that have not yet been assigned
		// to a specific worker (e.g., planner task queues). These are excluded
		// from per-worker depth counts.
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
