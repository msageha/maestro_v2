package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
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
}

// MetricsHandler generates metrics and dashboard files.
type MetricsHandler struct {
	maestroDir string
	config     model.Config
	logger     *log.Logger
	logLevel   LogLevel
}

// NewMetricsHandler creates a new MetricsHandler.
func NewMetricsHandler(maestroDir string, cfg model.Config, logger *log.Logger, logLevel LogLevel) *MetricsHandler {
	return &MetricsHandler{
		maestroDir: maestroDir,
		config:     cfg,
		logger:     logger,
		logLevel:   logLevel,
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

	// Count completed/failed tasks from current queue snapshot for accurate metrics
	for _, tq := range taskQueues {
		for _, task := range tq.Queue.Tasks {
			switch task.Status {
			case model.StatusCompleted:
				counters.TasksCompleted++
			case model.StatusFailed:
				counters.TasksFailed++
			}
		}
	}

	// Set absolute counters from snapshot (completed/failed are snapshot-based)
	metrics.Counters.TasksCompleted = counters.TasksCompleted
	metrics.Counters.TasksFailed = counters.TasksFailed

	// Merge incremental counters (additive)
	metrics.Counters.CommandsDispatched += counters.CommandsDispatched
	metrics.Counters.TasksDispatched += counters.TasksDispatched
	metrics.Counters.TasksCancelled += counters.TasksCancelled
	metrics.Counters.DeadLetters += counters.DeadLetters
	metrics.Counters.ReconciliationRepairs += counters.ReconciliationRepairs
	metrics.Counters.NotificationRetries += counters.NotificationRetries

	// Update heartbeat and timestamp
	heartbeat := scanStart.UTC().Format(time.RFC3339)
	metrics.DaemonHeartbeat = &heartbeat
	now := time.Now().UTC().Format(time.RFC3339)
	metrics.UpdatedAt = &now

	return yamlutil.AtomicWrite(metricsPath, metrics)
}

// UpdateDashboard generates a markdown summary and writes .maestro/dashboard.md.
func (mh *MetricsHandler) UpdateDashboard(
	cq model.CommandQueue,
	taskQueues map[string]*taskQueueEntry,
	nq model.NotificationQueue,
) error {
	depth := mh.computeQueueDepth(cq, taskQueues, nq)

	var sb strings.Builder
	sb.WriteString("# Maestro Dashboard\n\n")
	sb.WriteString(fmt.Sprintf("Updated: %s\n\n", time.Now().UTC().Format(time.RFC3339)))

	// Queue Depth table
	sb.WriteString("## Queue Depth\n\n")
	sb.WriteString("| Queue | Pending |\n")
	sb.WriteString("|-------|--------:|\n")
	sb.WriteString(fmt.Sprintf("| planner | %d |\n", depth.Planner))
	sb.WriteString(fmt.Sprintf("| orchestrator | %d |\n", depth.Orchestrator))

	// Sort worker keys for deterministic output
	workerKeys := make([]string, 0, len(depth.Workers))
	for k := range depth.Workers {
		workerKeys = append(workerKeys, k)
	}
	sort.Strings(workerKeys)
	for _, w := range workerKeys {
		sb.WriteString(fmt.Sprintf("| %s | %d |\n", w, depth.Workers[w]))
	}

	// Active commands
	sb.WriteString("\n## Active Commands\n\n")
	activeCount := 0
	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusInProgress {
			sb.WriteString(fmt.Sprintf("- `%s` (priority=%d, attempts=%d)\n", cmd.ID, cmd.Priority, cmd.Attempts))
			activeCount++
		}
	}
	if activeCount == 0 {
		sb.WriteString("_No active commands_\n")
	}

	// Task summary per worker
	sb.WriteString("\n## Worker Tasks\n\n")
	workerPaths := make([]string, 0, len(taskQueues))
	for path := range taskQueues {
		workerPaths = append(workerPaths, path)
	}
	sort.Strings(workerPaths)

	for _, path := range workerPaths {
		tq := taskQueues[path]
		wID := workerIDFromPath(path)
		if wID == "" {
			continue
		}

		pending, inProg := 0, 0
		for _, task := range tq.Queue.Tasks {
			switch task.Status {
			case model.StatusPending:
				pending++
			case model.StatusInProgress:
				inProg++
			}
		}
		sb.WriteString(fmt.Sprintf("- **%s**: %d pending, %d in_progress\n", wID, pending, inProg))
	}

	dashboardPath := filepath.Join(mh.maestroDir, "dashboard.md")
	return atomicWriteText(dashboardPath, sb.String())
}

// UpdateDashboardFull generates a dashboard including results/ summary (§5.11).
func (mh *MetricsHandler) UpdateDashboardFull(
	cq model.CommandQueue,
	taskQueues map[string]*taskQueueEntry,
	nq model.NotificationQueue,
	resultFiles map[string]*model.TaskResultFile,
) error {
	depth := mh.computeQueueDepth(cq, taskQueues, nq)

	var sb strings.Builder
	sb.WriteString("# Maestro Dashboard\n\n")
	sb.WriteString(fmt.Sprintf("Updated: %s\n\n", time.Now().UTC().Format(time.RFC3339)))

	// Queue Depth table
	sb.WriteString("## Queue Depth\n\n")
	sb.WriteString("| Queue | Pending |\n")
	sb.WriteString("|-------|--------:|\n")
	sb.WriteString(fmt.Sprintf("| planner | %d |\n", depth.Planner))
	sb.WriteString(fmt.Sprintf("| orchestrator | %d |\n", depth.Orchestrator))

	workerKeys := make([]string, 0, len(depth.Workers))
	for k := range depth.Workers {
		workerKeys = append(workerKeys, k)
	}
	sort.Strings(workerKeys)
	for _, w := range workerKeys {
		sb.WriteString(fmt.Sprintf("| %s | %d |\n", w, depth.Workers[w]))
	}

	// Active commands
	sb.WriteString("\n## Active Commands\n\n")
	activeCount := 0
	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusInProgress {
			sb.WriteString(fmt.Sprintf("- `%s` (priority=%d, attempts=%d)\n", cmd.ID, cmd.Priority, cmd.Attempts))
			activeCount++
		}
	}
	if activeCount == 0 {
		sb.WriteString("_No active commands_\n")
	}

	// Task summary per worker
	sb.WriteString("\n## Worker Tasks\n\n")
	workerPaths := make([]string, 0, len(taskQueues))
	for path := range taskQueues {
		workerPaths = append(workerPaths, path)
	}
	sort.Strings(workerPaths)

	for _, path := range workerPaths {
		tq := taskQueues[path]
		wID := workerIDFromPath(path)
		if wID == "" {
			continue
		}

		pending, inProg := 0, 0
		for _, task := range tq.Queue.Tasks {
			switch task.Status {
			case model.StatusPending:
				pending++
			case model.StatusInProgress:
				inProg++
			}
		}
		sb.WriteString(fmt.Sprintf("- **%s**: %d pending, %d in_progress\n", wID, pending, inProg))
	}

	// Results summary (from results/ YAML files)
	if len(resultFiles) > 0 {
		sb.WriteString("\n## Results Summary\n\n")
		sb.WriteString("| Worker | Completed | Failed | Total |\n")
		sb.WriteString("|--------|----------:|-------:|------:|\n")

		resultWorkers := make([]string, 0, len(resultFiles))
		for wID := range resultFiles {
			resultWorkers = append(resultWorkers, wID)
		}
		sort.Strings(resultWorkers)

		for _, wID := range resultWorkers {
			rf := resultFiles[wID]
			completed, failed, total := 0, 0, 0
			for _, r := range rf.Results {
				total++
				switch r.Status {
				case model.StatusCompleted:
					completed++
				case model.StatusFailed:
					failed++
				}
			}
			sb.WriteString(fmt.Sprintf("| %s | %d | %d | %d |\n", wID, completed, failed, total))
		}
	}

	dashboardPath := filepath.Join(mh.maestroDir, "dashboard.md")
	return atomicWriteText(dashboardPath, sb.String())
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
		tmp.Close()
		os.Remove(tmpName)
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
	if level < mh.logLevel {
		return
	}
	levelStr := "INFO"
	switch level {
	case LogLevelDebug:
		levelStr = "DEBUG"
	case LogLevelWarn:
		levelStr = "WARN"
	case LogLevelError:
		levelStr = "ERROR"
	}
	msg := fmt.Sprintf(format, args...)
	mh.logger.Printf("%s %s metrics: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}
