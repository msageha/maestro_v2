package daemon

import (
	"bytes"
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/events"
	yamlv3 "gopkg.in/yaml.v3"

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
		VerifyStatus:    f.readVerifyStatus(),
		LastUpdated:     f.clock.Now(),
	}

	// Read queue depths from filesystem (independent of log file)
	f.updateQueueStatus(data)

	// A/B candidate races (state-driven, byte-filtered like the daemon's
	// own discovery; unresolved races plus the most recent resolutions)
	f.collectABRaces(data)

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

// Dashboard verify status strings rendered by readVerifyStatus.
const (
	verifyStatusEnabled  = "enabled"
	verifyStatusUnknown  = "unknown"
	verifyStatusDisabled = "disabled (verify.enabled=false in config.yaml)"
)

// readVerifyStatus reflects the configured verify runner state directly
// from config.yaml. The earlier indirection via `state/verify_status.yaml`
// existed to surface a deprecated emergency env opt-out (`MAESTRO_ALLOW_VERIFY_SKIP`);
// that gate is gone, and `verify.enabled` is now the single source of
// truth. Returns one of verifyStatus{Enabled,Unknown,Disabled} — only
// states that can be explained from config alone.
func (f *DashboardFormatter) readVerifyStatus() string {
	cfg, err := model.LoadConfig(f.maestroDir)
	if err != nil {
		return verifyStatusUnknown
	}
	if cfg.Verify.EffectiveEnabled() {
		return verifyStatusEnabled
	}
	return verifyStatusDisabled
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
	// to avoid O(n) full scans as log files grow. Use a dynamic buffer
	// size based on file size: minimum 64KB, maximum 2MB.
	info, err := file.Stat()
	if err != nil {
		return err
	}
	maxTailBytes := dynamicTailBytes(info.Size())
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
		// scanner.Bytes() is valid until the next Scan call; parseLogEntry
		// processes the slice synchronously (json.Unmarshal does not retain
		// a reference), so no copy is needed.
		f.parseLogEntry(data, scanner.Bytes())
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
		case model.StatusInProgress,
			// §2.1 extended lifecycle: dispatched/running/verify_pending/repair_pending
			// are all "in flight" from the operator perspective. Without this branch
			// the dashboard's InProgress counter silently dropped to zero between a
			// task's lease acquisition and verify completion, hiding stuck work.
			model.StatusDispatched,
			model.StatusRunning,
			model.StatusVerifyPending,
			model.StatusRepairPending:
			data.Stats.InProgressTasks++
		case model.StatusPending,
			// planned/ready precede dispatch; they belong to the pending bucket
			// until the daemon hands the task to a worker.
			model.StatusPlanned,
			model.StatusReady:
			data.Stats.PendingTasks++
		case model.StatusCancelled:
			data.Stats.CancelledTasks++
		}
	}
}

// updateQueueStatus reads current queue depths by parsing queue YAML files.
func (f *DashboardFormatter) updateQueueStatus(data *DashboardData) {
	queueDir := queueDirPath(f.maestroDir)
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

// countQueueStatuses is a parameterized helper that counts pending/in_progress
// entries in a queue YAML file. listKey is the top-level YAML key containing the
// entry list (e.g. "notifications", "commands", "tasks").
func countQueueStatuses(fileData []byte, listKey string, info *QueueInfo) {
	// Generic structure: map with a single list key containing status entries.
	var raw map[string][]struct {
		Status string `yaml:"status"`
	}
	if err := yaml.Unmarshal(fileData, &raw); err != nil {
		return
	}
	for _, entry := range raw[listKey] {
		switch entry.Status {
		case "pending":
			info.Pending++
		case "in_progress":
			info.InProgress++
		}
	}
}

// countNotificationStatuses counts pending/in_progress notifications in an orchestrator queue file.
func countNotificationStatuses(fileData []byte, info *QueueInfo) {
	countQueueStatuses(fileData, "notifications", info)
}

// countCommandStatuses counts pending/in_progress commands in a planner queue file.
func countCommandStatuses(fileData []byte, info *QueueInfo) {
	countQueueStatuses(fileData, "commands", info)
}

// countTaskStatuses counts pending/in_progress tasks in a worker queue file.
func countTaskStatuses(fileData []byte, info *QueueInfo) {
	countQueueStatuses(fileData, "tasks", info)
}

// dynamicTailBytes returns the number of bytes to tail-read from a log file
// based on its total size. Minimum 64KB, maximum 2MB.
func dynamicTailBytes(fileSize int64) int64 {
	const minTail int64 = 64 * 1024   // 64KB
	const maxTail int64 = 2048 * 1024 // 2MB

	// Use ~25% of file size, clamped to [minTail, maxTail].
	tail := fileSize / 4
	if tail < minTail {
		return minTail
	}
	if tail > maxTail {
		return maxTail
	}
	return tail
}

// sortEventsByTimestampDesc sorts a DashboardEvent slice by timestamp descending (most recent first).
func sortEventsByTimestampDesc(events []DashboardEvent) {
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp.After(events[j].Timestamp)
	})
}

// truncateEvents returns events truncated to limit length. If len(events) <= limit, returns events unchanged.
func truncateEvents(events []DashboardEvent, limit int) []DashboardEvent {
	if len(events) > limit {
		return events[:limit]
	}
	return events
}

// sortEvents sorts all event lists by timestamp (most recent first)
func (f *DashboardFormatter) sortEvents(data *DashboardData) {
	sortEventsByTimestampDesc(data.RecentEvents)
	sortEventsByTimestampDesc(data.RecentErrors)
	sortEventsByTimestampDesc(data.RecentWarnings)
}

// limitEvents limits event lists to maximum counts
func (f *DashboardFormatter) limitEvents(data *DashboardData) {
	data.RecentEvents = truncateEvents(data.RecentEvents, f.maxEvents)
	data.RecentErrors = truncateEvents(data.RecentErrors, f.maxErrors)
	data.RecentWarnings = truncateEvents(data.RecentWarnings, f.maxWarnings)
}

// collectABRaces lists A/B candidate groups from command state files. Uses
// the same cheap byte pre-filter as the daemon's state-driven discovery.
func (f *DashboardFormatter) collectABRaces(data *DashboardData) {
	dir := filepath.Join(f.maestroDir, "state", "commands")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	const maxRaces = 10
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // controlled state dir
		if err != nil || !bytes.Contains(raw, []byte("candidate_groups")) {
			continue
		}
		var cs model.CommandState
		if err := yamlv3.Unmarshal(raw, &cs); err != nil {
			continue
		}
		for gid, g := range cs.CandidateGroups {
			if g == nil {
				continue
			}
			info := ABRaceInfo{CommandID: cs.CommandID, GroupID: gid, Status: string(g.Status)}
			switch {
			case g.Status.IsUnresolved():
				var models []string
				for _, c := range g.Candidates {
					models = append(models, c.Model)
				}
				info.Detail = strings.Join(models, " vs ")
			case g.WinnerTaskID != "":
				winner := g.CandidateByTask(g.WinnerTaskID)
				detail := "winner: "
				if winner != nil {
					detail += winner.Model
				} else {
					detail += g.WinnerTaskID
				}
				for _, k := range []string{"stage3_decision", "stage2_decision", "walkover"} {
					if v := g.SelectionEvidence[k]; v != "" {
						detail += " (" + k + "=" + v + ")"
						break
					}
				}
				info.Detail = detail
			default:
				info.Detail = "degraded: " + g.SelectionEvidence["degraded_reason"]
			}
			info.updatedAt = g.UpdatedAt
			info.unresolved = g.Status.IsUnresolved()
			data.ABRaces = append(data.ABRaces, info)
		}
	}
	// Unresolved races first, then most recently updated.
	sort.Slice(data.ABRaces, func(i, j int) bool {
		a, b := data.ABRaces[i], data.ABRaces[j]
		if a.unresolved != b.unresolved {
			return a.unresolved
		}
		return a.updatedAt > b.updatedAt
	})
	if len(data.ABRaces) > maxRaces {
		data.ABRaces = data.ABRaces[:maxRaces]
	}
}
