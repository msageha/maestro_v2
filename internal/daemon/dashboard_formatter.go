package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/msageha/maestro_v2/internal/events"
)

// DashboardStats represents aggregated statistics for the dashboard
type DashboardStats struct {
	TotalTasks      int
	CompletedTasks  int
	FailedTasks     int
	InProgressTasks int
	PendingTasks    int
	TaskSuccessRate float64
	ErrorCount      int
	WarningCount    int
	LastUpdated     time.Time
}

// DashboardEvent represents a filtered event for display
type DashboardEvent struct {
	Timestamp   time.Time
	EventType   string
	TaskID      string
	AgentID     string
	Status      string
	Summary     string
	IsError     bool
	IsWarning   bool
}

// DashboardData contains all data needed to render the dashboard
type DashboardData struct {
	Stats           DashboardStats
	RecentEvents    []DashboardEvent
	RecentErrors    []DashboardEvent
	RecentWarnings  []DashboardEvent
	QueueStatus     map[string]QueueInfo
	AgentStatus     map[string]AgentInfo
	FormationStatus string
	DaemonStatus    string
	LastUpdated     time.Time
}

// QueueInfo represents queue depth information
type QueueInfo struct {
	Name       string
	Pending    int
	InProgress int
}

// AgentInfo represents agent status information
type AgentInfo struct {
	ID           string
	Status       string
	CurrentTask  string
	LastActivity time.Time
}

// DashboardFormatter formats JSONL logs into human-readable dashboard
type DashboardFormatter struct {
	logPath     string
	maestroDir  string
	maxEvents   int
	maxErrors   int
	maxWarnings int
}

// NewDashboardFormatter creates a new dashboard formatter
func NewDashboardFormatter(maestroDir string) *DashboardFormatter {
	return &DashboardFormatter{
		logPath:     filepath.Join(maestroDir, "logs", "maestro.jsonl"),
		maestroDir:  maestroDir,
		maxEvents:   20,
		maxErrors:   10,
		maxWarnings: 10,
	}
}

// FormatDashboard reads JSONL logs and formats them for dashboard display
func (f *DashboardFormatter) FormatDashboard() (string, error) {
	data, err := f.collectDashboardData()
	if err != nil {
		return "", fmt.Errorf("failed to collect dashboard data: %w", err)
	}

	// Render using template
	tmpl, err := f.getDashboardTemplate()
	if err != nil {
		return "", fmt.Errorf("failed to get template: %w", err)
	}

	var output strings.Builder
	if err := tmpl.Execute(&output, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return output.String(), nil
}

// collectDashboardData reads logs and aggregates dashboard data
func (f *DashboardFormatter) collectDashboardData() (*DashboardData, error) {
	data := &DashboardData{
		Stats:           DashboardStats{LastUpdated: time.Now()},
		RecentEvents:    make([]DashboardEvent, 0),
		RecentErrors:    make([]DashboardEvent, 0),
		RecentWarnings:  make([]DashboardEvent, 0),
		QueueStatus:     make(map[string]QueueInfo),
		AgentStatus:     make(map[string]AgentInfo),
		FormationStatus: "Active",
		DaemonStatus:    "Running",
		LastUpdated:     time.Now(),
	}

	// Read and parse JSONL log file
	if err := f.parseLogFile(data); err != nil {
		// If log file doesn't exist, return empty data
		if os.IsNotExist(err) {
			return data, nil
		}
		return nil, err
	}

	// Calculate statistics
	f.calculateStats(data)

	// Sort events by timestamp (most recent first)
	f.sortEvents(data)

	// Limit events to max counts
	f.limitEvents(data)

	return data, nil
}

// parseLogFile reads and parses the JSONL log file
func (f *DashboardFormatter) parseLogFile(data *DashboardData) error {
	file, err := os.Open(f.logPath)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	taskStatus := make(map[string]string)

	for scanner.Scan() {
		var entry events.LogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue // Skip malformed entries
		}

		// Extract event information
		event := f.extractEvent(entry)

		// Filter task-related events
		if f.isTaskRelated(entry.EventType) {
			data.RecentEvents = append(data.RecentEvents, event)

			// Track task status (include retries)
			if event.TaskID != "" {
				if event.Status != "" {
					taskStatus[event.TaskID] = event.Status
				} else if strings.Contains(entry.EventType, "retry") {
					// Count retry as a task (task_002 is retrying)
					taskStatus[event.TaskID] = "in_progress"
				}
			}
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

	// Count task statuses
	for _, status := range taskStatus {
		data.Stats.TotalTasks++
		switch status {
		case "completed":
			data.Stats.CompletedTasks++
		case "failed":
			data.Stats.FailedTasks++
		case "in_progress":
			data.Stats.InProgressTasks++
		case "pending":
			data.Stats.PendingTasks++
		}
	}

	return scanner.Err()
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
	} else if err, ok := entry.Details["error"].(string); ok {
		event.Summary = err
	}

	// Determine if error or warning
	event.IsError = f.isErrorEvent(entry.EventType)
	event.IsWarning = f.isWarningEvent(entry.EventType)

	return event
}

// isTaskRelated checks if an event type is task-related
func (f *DashboardFormatter) isTaskRelated(eventType string) bool {
	taskEvents := []string{
		"task_created",
		"task_started",
		"task_completed",
		"task_failed",
		"task_dispatched",
		"task_retry",
		"result_written",
		"command_started",
		"command_completed",
		"command_failed",
	}

	for _, te := range taskEvents {
		if strings.Contains(strings.ToLower(eventType), te) {
			return true
		}
	}
	return false
}

// isErrorEvent checks if an event type indicates an error
func (f *DashboardFormatter) isErrorEvent(eventType string) bool {
	return strings.Contains(strings.ToLower(eventType), "error") ||
		strings.Contains(strings.ToLower(eventType), "failed")
}

// isWarningEvent checks if an event type indicates a warning
func (f *DashboardFormatter) isWarningEvent(eventType string) bool {
	return strings.Contains(strings.ToLower(eventType), "warn") ||
		strings.Contains(strings.ToLower(eventType), "timeout") ||
		strings.Contains(strings.ToLower(eventType), "retry")
}

// calculateStats calculates aggregate statistics
func (f *DashboardFormatter) calculateStats(data *DashboardData) {
	if data.Stats.TotalTasks > 0 {
		data.Stats.TaskSuccessRate = float64(data.Stats.CompletedTasks) / float64(data.Stats.TotalTasks) * 100
	}

	// Read queue depths from filesystem
	f.updateQueueStatus(data)
}

// updateQueueStatus reads current queue depths
func (f *DashboardFormatter) updateQueueStatus(data *DashboardData) {
	queueDir := filepath.Join(f.maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
			queueName := strings.TrimSuffix(entry.Name(), ".yaml")
			info := QueueInfo{Name: queueName}

			// This is simplified - in reality we'd parse the queue file
			// to count pending and in-progress tasks
			data.QueueStatus[queueName] = info
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

// getDashboardTemplate returns the dashboard template
func (f *DashboardFormatter) getDashboardTemplate() (*template.Template, error) {
	tmplText := `# Maestro Dashboard

> Auto-generated at {{ .LastUpdated.Format "2006-01-02 15:04:05 MST" }}. Do not edit manually.

## System Status

| Component | Status |
|-----------|--------|
| Daemon    | {{ .DaemonStatus }} |
| Formation | {{ .FormationStatus }} |

## Task Statistics

| Metric | Value |
|--------|-------|
| Total Tasks | {{ .Stats.TotalTasks }} |
| Completed | {{ .Stats.CompletedTasks }} ({{ printf "%.1f" .Stats.TaskSuccessRate }}%) |
| Failed | {{ .Stats.FailedTasks }} |
| In Progress | {{ .Stats.InProgressTasks }} |
| Pending | {{ .Stats.PendingTasks }} |
| Errors | {{ .Stats.ErrorCount }} |
| Warnings | {{ .Stats.WarningCount }} |

## Queue Status

| Queue | Pending | In Progress |
|-------|---------|-------------|
{{ range .QueueStatus -}}
| {{ .Name }} | {{ .Pending }} | {{ .InProgress }} |
{{ else -}}
| _No queues_ | - | - |
{{ end }}

## Agent Status

| Agent ID | Status | Current Task | Last Activity |
|----------|--------|--------------|---------------|
{{ range .AgentStatus -}}
| {{ .ID }} | {{ .Status }} | {{ if .CurrentTask }}{{ .CurrentTask }}{{ else }}-{{ end }} | {{ .LastActivity.Format "15:04:05" }} |
{{ else -}}
| _No agents_ | - | - | - |
{{ end }}

## Recent Errors (Last {{ len .RecentErrors }})

{{ if .RecentErrors -}}
| Time | Agent | Task | Error |
|------|-------|------|-------|
{{ range .RecentErrors -}}
| {{ .Timestamp.Format "15:04:05" }} | {{ if .AgentID }}{{ .AgentID }}{{ else }}-{{ end }} | {{ if .TaskID }}{{ .TaskID }}{{ else }}-{{ end }} | {{ .Summary }} |
{{ end -}}
{{ else -}}
_No recent errors._
{{ end }}

## Recent Warnings (Last {{ len .RecentWarnings }})

{{ if .RecentWarnings -}}
| Time | Agent | Task | Warning |
|------|-------|------|---------|
{{ range .RecentWarnings -}}
| {{ .Timestamp.Format "15:04:05" }} | {{ if .AgentID }}{{ .AgentID }}{{ else }}-{{ end }} | {{ if .TaskID }}{{ .TaskID }}{{ else }}-{{ end }} | {{ .Summary }} |
{{ end -}}
{{ else -}}
_No recent warnings._
{{ end }}

## Recent Activity (Last {{ len .RecentEvents }})

{{ if .RecentEvents -}}
| Time | Event | Agent | Task | Status | Details |
|------|-------|-------|------|--------|---------|
{{ range .RecentEvents -}}
| {{ .Timestamp.Format "15:04:05" }} | {{ .EventType }} | {{ if .AgentID }}{{ .AgentID }}{{ else }}-{{ end }} | {{ if .TaskID }}{{ .TaskID }}{{ else }}-{{ end }} | {{ if .Status }}{{ .Status }}{{ else }}-{{ end }} | {{ if .Summary }}{{ .Summary }}{{ else }}-{{ end }} |
{{ end -}}
{{ else -}}
_No recent activity._
{{ end }}

---
_Last updated: {{ .LastUpdated.Format "2006-01-02 15:04:05 MST" }}_
`

	return template.New("dashboard").Parse(tmplText)
}

// WriteDashboard writes the formatted dashboard to dashboard.md
func (f *DashboardFormatter) WriteDashboard(output io.Writer) error {
	formatted, err := f.FormatDashboard()
	if err != nil {
		return err
	}

	_, err = output.Write([]byte(formatted))
	return err
}

// UpdateDashboardFile updates the dashboard.md file
func (f *DashboardFormatter) UpdateDashboardFile() error {
	dashboardPath := filepath.Join(f.maestroDir, "dashboard.md")

	// Create temporary file
	tmpPath := dashboardPath + ".tmp"
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpPath)
	defer tmpFile.Close()

	// Write formatted dashboard
	if err := f.WriteDashboard(tmpFile); err != nil {
		return fmt.Errorf("failed to write dashboard: %w", err)
	}

	// Sync to disk
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	// Close before rename
	tmpFile.Close()

	// Atomic replace
	if err := os.Rename(tmpPath, dashboardPath); err != nil {
		return fmt.Errorf("failed to replace dashboard file: %w", err)
	}

	return nil
}