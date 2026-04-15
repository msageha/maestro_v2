package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// EventClass represents the classification of an event using bitmask flags.
type EventClass int

const (
	// EventClassTask classifies events related to task lifecycle changes.
	EventClassTask EventClass = 1 << iota // 1
	// EventClassError classifies events that represent errors or failures.
	EventClassError // 2
	// EventClassWarning classifies events that represent warnings or non-fatal issues.
	EventClassWarning // 4
)

// eventClassification maps known event types to their classification bitmask.
var eventClassification = map[string]EventClass{
	"task_created":      EventClassTask,
	"task_started":      EventClassTask,
	"task_completed":    EventClassTask,
	"task_failed":       EventClassTask | EventClassError,
	"task_dispatched":   EventClassTask,
	"task_retry":        EventClassTask | EventClassWarning,
	"result_written":    EventClassTask,
	"command_started":   EventClassTask,
	"command_completed": EventClassTask,
	"command_failed":    EventClassTask | EventClassError,
	"command_error":     EventClassError,
	"lease_warning":     EventClassWarning,
	"lease_timeout":     EventClassWarning,
}

// classifyEvent returns the EventClass bitmask for the given event type.
// Known event types are looked up in O(1) via the map. Unknown event types
// fall back to substring matching to preserve backward compatibility.
func classifyEvent(eventType string) EventClass {
	if class, ok := eventClassification[eventType]; ok {
		return class
	}
	// Fallback for unknown event types
	lower := strings.ToLower(eventType)
	var class EventClass
	if strings.Contains(lower, "error") || strings.Contains(lower, "failed") {
		class |= EventClassError
	}
	if strings.Contains(lower, "warn") || strings.Contains(lower, "timeout") || strings.Contains(lower, "retry") {
		class |= EventClassWarning
	}
	return class
}

// isTaskRelated checks if an event type is task-related
func (f *DashboardFormatter) isTaskRelated(eventType string) bool {
	return classifyEvent(eventType)&EventClassTask != 0
}

// isErrorEvent checks if an event type indicates an error
func (f *DashboardFormatter) isErrorEvent(eventType string) bool {
	return classifyEvent(eventType)&EventClassError != 0
}

// isWarningEvent checks if an event type indicates a warning
func (f *DashboardFormatter) isWarningEvent(eventType string) bool {
	return classifyEvent(eventType)&EventClassWarning != 0
}

// collectDashboardData reads logs and aggregates dashboard data
func (f *DashboardFormatter) collectDashboardData() (*DashboardData, error) {
	data := &DashboardData{
		Stats:           DashboardStats{LastUpdated: f.clock.Now()},
		RecentEvents:    make([]DashboardEvent, 0),
		RecentErrors:    make([]DashboardEvent, 0),
		RecentWarnings:  make([]DashboardEvent, 0),
		QueueStatus:     make(map[string]QueueInfo),
		AgentStatus:     make(map[string]AgentInfo),
		FormationStatus: "Active",
		DaemonStatus:    "Running",
		LastUpdated:     f.clock.Now(),
	}

	// Read queue depths from filesystem (independent of log file)
	f.updateQueueStatus(data)

	// Collect task statistics from state files (accurate, not log-windowed)
	f.collectTaskStatsFromState(data)

	// Read and parse JSONL log file (events, errors, warnings, agent status only)
	if err := f.parseLogFile(data); err != nil {
		// If log file doesn't exist, return data with state-based stats
		if os.IsNotExist(err) {
			f.calculateStats(data)
			return data, nil
		}
		// Return partial data with stale marker
		data.IsStale = true
		data.StaleReason = err.Error()
		f.calculateStats(data)
		return data, err
	}

	// Calculate derived statistics (success rate)
	f.calculateStats(data)

	// Sort events by timestamp (most recent first)
	f.sortEvents(data)

	// Limit events to max counts
	f.limitEvents(data)

	return data, nil
}

// parseLogFile reads and parses the JSONL log file.
// To avoid full-scanning large logs, only the tail portion (last maxTailBytes)
// is parsed. Statistics are therefore windowed over recent events, not full history.
func (f *DashboardFormatter) parseLogFile(data *DashboardData) error {
	file, err := os.Open(f.logPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			f.dl.Logf(LogLevelWarn, "close %s: %v", f.logPath, cerr)
		}
	}()

	// Tail-read optimization: only read the last portion of the file
	// to avoid O(n) full scans as log files grow.
	const maxTailBytes int64 = 512 * 1024 // 512KB
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > maxTailBytes {
		if _, err := file.Seek(-maxTailBytes, io.SeekEnd); err != nil {
			return err
		}
	}

	scanner := bufio.NewScanner(file)
	// Expand scanner buffer from default 64KB to 1MB to handle long log lines
	const maxScannerBuffer = 1024 * 1024 // 1MB
	scanner.Buffer(make([]byte, 0, maxScannerBuffer), maxScannerBuffer)

	// If we seeked into the middle of the file, discard the first partial line
	if info.Size() > maxTailBytes {
		scanner.Scan() // discard partial line at seek boundary
	}

	for scanner.Scan() {
		// Copy scanner bytes: the underlying buffer is reused across Scan calls.
		line := append([]byte(nil), scanner.Bytes()...)
		f.parseLogEntry(data, line)
	}

	return scanner.Err()
}

// parseLogEntry processes a single scanner line and updates dashboard data.
func (f *DashboardFormatter) parseLogEntry(data *DashboardData, line []byte) {
	var entry events.LogEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		f.dl.Logf(LogLevelDebug, "parse_log_entry_skip error=%v", err)
		return
	}

	// Extract event information
	event := f.extractEvent(entry)

	// Filter task-related events
	if f.isTaskRelated(entry.EventType) {
		data.RecentEvents = append(data.RecentEvents, event)
	}

	// Collect errors and warnings
	if event.IsError {
		data.RecentErrors = append(data.RecentErrors, event)
		data.Stats.ErrorCount++
	}
	if event.IsWarning {
		data.RecentWarnings = append(data.RecentWarnings, event)
		data.Stats.WarningCount++
	}

	// Update agent status
	if event.AgentID != "" {
		agent := data.AgentStatus[event.AgentID]
		agent.ID = event.AgentID
		agent.LastActivity = event.Timestamp
		if event.TaskID != "" {
			agent.CurrentTask = event.TaskID
		}
		if event.Status != "" {
			agent.Status = event.Status
		}
		data.AgentStatus[event.AgentID] = agent
	}
}

// extractEvent extracts displayable event from log entry
func (f *DashboardFormatter) extractEvent(entry events.LogEntry) DashboardEvent {
	event := DashboardEvent{
		Timestamp: entry.Timestamp,
		EventType: entry.EventType,
		TaskID:    entry.TaskID,
		AgentID:   entry.AgentID,
	}

	// Extract status and summary from details
	if status, ok := entry.Details["status"].(string); ok {
		event.Status = status
	}
	if summary, ok := entry.Details["summary"].(string); ok {
		event.Summary = summary
	} else if msg, ok := entry.Details["message"].(string); ok {
		event.Summary = msg
	} else if errMsg, ok := entry.Details["error"].(string); ok {
		event.Summary = errMsg
	}

	// Determine if error or warning
	event.IsError = f.isErrorEvent(entry.EventType)
	event.IsWarning = f.isWarningEvent(entry.EventType)

	return event
}

// calculateStats calculates aggregate statistics.
// Queue status is populated separately in collectDashboardData.
func (f *DashboardFormatter) calculateStats(data *DashboardData) {
	if data.Stats.TotalTasks > 0 {
		data.Stats.TaskSuccessRate = float64(data.Stats.CompletedTasks) / float64(data.Stats.TotalTasks) * 100
	}
}

// commandStateSnapshot holds the parsed subset of a command state file needed
// for dashboard task statistics.
type commandStateSnapshot struct {
	RequiredTaskIDs []string                `yaml:"required_task_ids"`
	OptionalTaskIDs []string                `yaml:"optional_task_ids"`
	TaskStates      map[string]model.Status `yaml:"task_states"`
}

// collectTaskStatsFromState reads command state files and aggregates task statistics.
// Only tasks listed in required_task_ids and optional_task_ids are counted to avoid
// double-counting retry history entries in task_states.
func (f *DashboardFormatter) collectTaskStatsFromState(data *DashboardData) {
	stateDir := filepath.Join(f.maestroDir, "state", "commands")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		filePath := filepath.Join(stateDir, entry.Name())
		fileData, err := os.ReadFile(filePath) //nolint:gosec // filePath is constructed from a controlled application state directory
		if err != nil {
			continue
		}

		var cs commandStateSnapshot
		if err := yaml.Unmarshal(fileData, &cs); err != nil {
			continue
		}

		aggregateCommandTaskStats(data, &cs)
	}
}

// aggregateCommandTaskStats aggregates task statistics from a single command state
// snapshot into the dashboard data.
func aggregateCommandTaskStats(data *DashboardData, cs *commandStateSnapshot) {
	// Collect only current task IDs (required + optional), not retry history
	currentTasks := make(map[string]struct{}, len(cs.RequiredTaskIDs)+len(cs.OptionalTaskIDs))
	for _, id := range cs.RequiredTaskIDs {
		currentTasks[id] = struct{}{}
	}
	for _, id := range cs.OptionalTaskIDs {
		currentTasks[id] = struct{}{}
	}

	for taskID := range currentTasks {
		data.Stats.TotalTasks++
		status, ok := cs.TaskStates[taskID]
		if !ok {
			// Task registered but no state yet — count as pending
			data.Stats.PendingTasks++
			continue
		}
		switch status {
		case model.StatusCompleted:
			data.Stats.CompletedTasks++
		case model.StatusFailed, model.StatusDeadLetter:
			data.Stats.FailedTasks++
		case model.StatusInProgress:
			data.Stats.InProgressTasks++
		case model.StatusPending:
			data.Stats.PendingTasks++
		case model.StatusCancelled:
			data.Stats.CancelledTasks++
		}
	}
}

// updateQueueStatus reads current queue depths by parsing queue YAML files.
func (f *DashboardFormatter) updateQueueStatus(data *DashboardData) {
	queueDir := filepath.Join(f.maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		queueName := strings.TrimSuffix(entry.Name(), ".yaml")
		info := QueueInfo{Name: queueName}

		queuePath := filepath.Join(queueDir, entry.Name())
		fileData, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
		if err != nil {
			data.QueueStatus[queueName] = info
			continue
		}

		// Determine queue type and count statuses.
		// Only process known queue types; skip auxiliary files (e.g., planner_signals.yaml).
		switch {
		case queueName == "orchestrator":
			countNotificationStatuses(fileData, &info)
		case queueName == "planner":
			countCommandStatuses(fileData, &info)
		case strings.HasPrefix(queueName, "worker"):
			countTaskStatuses(fileData, &info)
		default:
			// Skip unknown queue files (e.g., planner_signals.yaml)
			continue
		}

		data.QueueStatus[queueName] = info
	}
}

// countNotificationStatuses counts pending/in_progress notifications in an orchestrator queue file.
func countNotificationStatuses(fileData []byte, info *QueueInfo) {
	var nq struct {
		Notifications []struct {
			Status string `yaml:"status"`
		} `yaml:"notifications"`
	}
	if err := yaml.Unmarshal(fileData, &nq); err == nil {
		for _, n := range nq.Notifications {
			switch n.Status {
			case "pending":
				info.Pending++
			case "in_progress":
				info.InProgress++
			}
		}
	}
}

// countCommandStatuses counts pending/in_progress commands in a planner queue file.
func countCommandStatuses(fileData []byte, info *QueueInfo) {
	var cq struct {
		Commands []struct {
			Status string `yaml:"status"`
		} `yaml:"commands"`
	}
	if err := yaml.Unmarshal(fileData, &cq); err == nil {
		for _, c := range cq.Commands {
			switch c.Status {
			case "pending":
				info.Pending++
			case "in_progress":
				info.InProgress++
			}
		}
	}
}

// countTaskStatuses counts pending/in_progress tasks in a worker queue file.
func countTaskStatuses(fileData []byte, info *QueueInfo) {
	var tq struct {
		Tasks []struct {
			Status string `yaml:"status"`
		} `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(fileData, &tq); err == nil {
		for _, t := range tq.Tasks {
			switch t.Status {
			case "pending":
				info.Pending++
			case "in_progress":
				info.InProgress++
			}
		}
	}
}

// sortEvents sorts all event lists by timestamp (most recent first)
func (f *DashboardFormatter) sortEvents(data *DashboardData) {
	sort.Slice(data.RecentEvents, func(i, j int) bool {
		return data.RecentEvents[i].Timestamp.After(data.RecentEvents[j].Timestamp)
	})
	sort.Slice(data.RecentErrors, func(i, j int) bool {
		return data.RecentErrors[i].Timestamp.After(data.RecentErrors[j].Timestamp)
	})
	sort.Slice(data.RecentWarnings, func(i, j int) bool {
		return data.RecentWarnings[i].Timestamp.After(data.RecentWarnings[j].Timestamp)
	})
}

// limitEvents limits event lists to maximum counts
func (f *DashboardFormatter) limitEvents(data *DashboardData) {
	if len(data.RecentEvents) > f.maxEvents {
		data.RecentEvents = data.RecentEvents[:f.maxEvents]
	}
	if len(data.RecentErrors) > f.maxErrors {
		data.RecentErrors = data.RecentErrors[:f.maxErrors]
	}
	if len(data.RecentWarnings) > f.maxWarnings {
		data.RecentWarnings = data.RecentWarnings[:f.maxWarnings]
	}
}
