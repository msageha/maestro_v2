package dashboard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	yaml "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// Stats represents aggregated statistics for the dashboard
type Stats struct {
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

// Event represents a filtered event for display
type Event struct {
	Timestamp time.Time
	EventType string
	TaskID    string
	AgentID   string
	Status    string
	Summary   string
	IsError   bool
	IsWarning bool
}

// Data contains all data needed to render the dashboard
type Data struct {
	Stats           Stats
	RecentEvents    []Event
	RecentErrors    []Event
	RecentWarnings  []Event
	QueueStatus     map[string]QueueInfo
	AgentStatus     map[string]AgentInfo
	ActiveCommands  []ActiveCommandInfo
	WorkerSummaries []WorkerSummary
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

// Formatter formats JSONL logs into human-readable dashboard
type Formatter struct {
	logPath     string
	maestroDir  string
	maxEvents   int
	maxErrors   int
	maxWarnings int
	clock       core.Clock

	tmplOnce sync.Once
	tmplVal  *template.Template
	tmplErr  error
}

// NewFormatter creates a new dashboard formatter
func NewFormatter(maestroDir string) *Formatter {
	return &Formatter{
		logPath:     filepath.Join(maestroDir, "logs", "maestro.jsonl"),
		maestroDir:  maestroDir,
		maxEvents:   20,
		maxErrors:   10,
		maxWarnings: 10,
		clock:       core.RealClock{},
	}
}

// FormatDashboard reads JSONL logs and formats them for dashboard display
func (f *Formatter) FormatDashboard() (string, error) {
	data, err := f.collectDashboardData()
	if err != nil {
		return "", fmt.Errorf("failed to collect dashboard data: %w", err)
	}

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
func (f *Formatter) collectDashboardData() (*Data, error) {
	data := &Data{
		Stats:           Stats{LastUpdated: f.clock.Now()},
		RecentEvents:    make([]Event, 0),
		RecentErrors:    make([]Event, 0),
		RecentWarnings:  make([]Event, 0),
		QueueStatus:     make(map[string]QueueInfo),
		AgentStatus:     make(map[string]AgentInfo),
		FormationStatus: "Active",
		DaemonStatus:    "Running",
		LastUpdated:     f.clock.Now(),
	}

	f.updateQueueStatus(data)

	if err := f.parseLogFile(data); err != nil {
		if os.IsNotExist(err) {
			return data, nil
		}
		return nil, err
	}

	f.calculateStats(data)
	f.sortEvents(data)
	f.limitEvents(data)

	return data, nil
}

// parseLogFile reads and parses the JSONL log file.
func (f *Formatter) parseLogFile(data *Data) error {
	file, err := os.Open(f.logPath)
	if err != nil {
		return err
	}
	defer file.Close()

	const maxTailBytes int64 = 512 * 1024
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
	const maxScannerBuffer = 1024 * 1024
	scanner.Buffer(make([]byte, 0, maxScannerBuffer), maxScannerBuffer)

	if info.Size() > maxTailBytes {
		scanner.Scan() // discard partial line
	}

	taskStatus := make(map[string]string)

	for scanner.Scan() {
		var entry events.LogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}

		event := f.extractEvent(entry)

		if f.isTaskRelated(entry.EventType) {
			data.RecentEvents = append(data.RecentEvents, event)

			if event.TaskID != "" {
				if event.Status != "" {
					taskStatus[event.TaskID] = event.Status
				} else if strings.Contains(entry.EventType, "retry") {
					taskStatus[event.TaskID] = "in_progress"
				}
			}
		}

		if event.IsError {
			data.RecentErrors = append(data.RecentErrors, event)
			data.Stats.ErrorCount++
		}
		if event.IsWarning {
			data.RecentWarnings = append(data.RecentWarnings, event)
			data.Stats.WarningCount++
		}

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

func (f *Formatter) extractEvent(entry events.LogEntry) Event {
	event := Event{
		Timestamp: entry.Timestamp,
		EventType: entry.EventType,
		TaskID:    entry.TaskID,
		AgentID:   entry.AgentID,
	}

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

	event.IsError = f.isErrorEvent(entry.EventType)
	event.IsWarning = f.isWarningEvent(entry.EventType)

	return event
}

func (f *Formatter) isTaskRelated(eventType string) bool {
	taskEvents := []string{
		"task_created", "task_started", "task_completed", "task_failed",
		"task_dispatched", "task_retry", "result_written",
		"command_started", "command_completed", "command_failed",
	}
	for _, te := range taskEvents {
		if strings.Contains(strings.ToLower(eventType), te) {
			return true
		}
	}
	return false
}

func (f *Formatter) isErrorEvent(eventType string) bool {
	return strings.Contains(strings.ToLower(eventType), "error") ||
		strings.Contains(strings.ToLower(eventType), "failed")
}

func (f *Formatter) isWarningEvent(eventType string) bool {
	return strings.Contains(strings.ToLower(eventType), "warn") ||
		strings.Contains(strings.ToLower(eventType), "timeout") ||
		strings.Contains(strings.ToLower(eventType), "retry")
}

func (f *Formatter) calculateStats(data *Data) {
	if data.Stats.TotalTasks > 0 {
		data.Stats.TaskSuccessRate = float64(data.Stats.CompletedTasks) / float64(data.Stats.TotalTasks) * 100
	}
}

func (f *Formatter) updateQueueStatus(data *Data) {
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
		fileData, err := os.ReadFile(queuePath)
		if err != nil {
			data.QueueStatus[queueName] = info
			continue
		}

		switch {
		case queueName == "orchestrator":
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
		case queueName == "planner":
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
		case strings.HasPrefix(queueName, "worker"):
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
		default:
			continue
		}

		data.QueueStatus[queueName] = info
	}
}

func (f *Formatter) sortEvents(data *Data) {
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

func (f *Formatter) limitEvents(data *Data) {
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

func (f *Formatter) getDashboardTemplate() (*template.Template, error) {
	f.tmplOnce.Do(func() {
		const tmplText = `# Maestro Dashboard

> Auto-generated at {{ .LastUpdated.Format "2006-01-02 15:04:05 MST" }}. Do not edit manually.

## System Status

| Component | Status |
|-----------|--------|
| Daemon    | {{ .DaemonStatus }} |
| Formation | {{ .FormationStatus }} |

## Queue Status

| Queue | Pending | In Progress |
|-------|---------|-------------|
{{ range .QueueStatus -}}
| {{ .Name }} | {{ .Pending }} | {{ .InProgress }} |
{{ else -}}
| _No queues_ | - | - |
{{ end }}

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
		f.tmplVal, f.tmplErr = template.New("dashboard").Parse(tmplText)
	})
	return f.tmplVal, f.tmplErr
}

// TaskQueueEntry is the interface-compatible struct for dashboard queue rendering.
type TaskQueueEntry struct {
	Queue model.TaskQueue
	Path  string
}

// UpdateDashboardFileWithQueues generates the dashboard using both JSONL logs
// and live queue data.
func (f *Formatter) UpdateDashboardFileWithQueues(
	cq model.CommandQueue,
	taskQueues map[string]*TaskQueueEntry,
	nq model.NotificationQueue,
	workerIDFromPath func(string) string,
	atomicWriteTextFn func(string, string) error,
) error {
	var sb strings.Builder
	sb.WriteString("# Maestro Dashboard\n\n")
	fmt.Fprintf(&sb, "Updated: %s\n\n", f.clock.Now().UTC().Format(time.RFC3339))

	sb.WriteString("## Queue Depth\n\n")
	sb.WriteString("| Queue | Pending |\n")
	sb.WriteString("|-------|--------:|\n")

	plannerPending := 0
	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusPending {
			plannerPending++
		}
	}
	fmt.Fprintf(&sb, "| planner | %d |\n", plannerPending)

	orchPending := 0
	for _, ntf := range nq.Notifications {
		if ntf.Status == model.StatusPending {
			orchPending++
		}
	}
	fmt.Fprintf(&sb, "| orchestrator | %d |\n", orchPending)

	workerKeys := make([]string, 0, len(taskQueues))
	for path := range taskQueues {
		wID := workerIDFromPath(path)
		if wID != "" {
			workerKeys = append(workerKeys, wID)
		}
	}
	sort.Strings(workerKeys)

	workerPending := make(map[string]int)
	workerInProg := make(map[string]int)
	for path, tq := range taskQueues {
		wID := workerIDFromPath(path)
		if wID == "" {
			continue
		}
		for _, task := range tq.Queue.Tasks {
			switch task.Status {
			case model.StatusPending:
				workerPending[wID]++
			case model.StatusInProgress:
				workerInProg[wID]++
			}
		}
	}
	for _, wID := range workerKeys {
		fmt.Fprintf(&sb, "| %s | %d |\n", wID, workerPending[wID])
	}

	sb.WriteString("\n## Active Commands\n\n")
	activeCount := 0
	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusInProgress {
			fmt.Fprintf(&sb, "- `%s` (priority=%d, attempts=%d)\n", cmd.ID, cmd.Priority, cmd.Attempts)
			activeCount++
		}
	}
	if activeCount == 0 {
		sb.WriteString("_No active commands_\n")
	}

	sb.WriteString("\n## Worker Tasks\n\n")
	for _, wID := range workerKeys {
		fmt.Fprintf(&sb, "- **%s**: %d pending, %d in_progress\n", wID, workerPending[wID], workerInProg[wID])
	}

	data, _ := f.collectDashboardData()
	if data != nil && (data.Stats.TotalTasks > 0 || data.Stats.ErrorCount > 0) {
		sb.WriteString("\n## Recent Activity\n\n")
		fmt.Fprintf(&sb, "Tasks: %d total, %d completed, %d failed\n",
			data.Stats.TotalTasks, data.Stats.CompletedTasks, data.Stats.FailedTasks)
		fmt.Fprintf(&sb, "Errors: %d, Warnings: %d\n", data.Stats.ErrorCount, data.Stats.WarningCount)
	}

	dashboardPath := filepath.Join(f.maestroDir, "dashboard.md")
	return atomicWriteTextFn(dashboardPath, sb.String())
}

// ActiveCommandInfo holds info about an in-progress command for dashboard display.
type ActiveCommandInfo struct {
	ID       string
	Priority int
	Attempts int
}

// WorkerSummary holds per-worker task counts for dashboard display.
type WorkerSummary struct {
	ID         string
	Pending    int
	InProgress int
}

// WriteDashboard writes the formatted dashboard to dashboard.md
func (f *Formatter) WriteDashboard(output io.Writer) error {
	formatted, err := f.FormatDashboard()
	if err != nil {
		return err
	}
	_, err = output.Write([]byte(formatted))
	return err
}

// UpdateDashboardFile updates the dashboard.md file using atomic write.
func (f *Formatter) UpdateDashboardFile(atomicWriteTextFn func(string, string) error) error {
	formatted, err := f.FormatDashboard()
	if err != nil {
		return fmt.Errorf("failed to format dashboard: %w", err)
	}

	dashboardPath := filepath.Join(f.maestroDir, "dashboard.md")
	return atomicWriteTextFn(dashboardPath, formatted)
}
