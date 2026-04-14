package daemon

import (
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// DashboardStats represents aggregated statistics for the dashboard
type DashboardStats struct {
	TotalTasks      int
	CompletedTasks  int
	FailedTasks     int
	InProgressTasks int
	PendingTasks    int
	CancelledTasks  int
	TaskSuccessRate float64
	ErrorCount      int
	WarningCount    int
	LastUpdated     time.Time
}

// DashboardEvent represents a filtered event for display
type DashboardEvent struct {
	Timestamp time.Time
	EventType string
	TaskID    string
	AgentID   string
	Status    string
	Summary   string
	IsError   bool
	IsWarning bool
}

// DashboardData contains all data needed to render the dashboard
type DashboardData struct {
	Stats           DashboardStats
	RecentEvents    []DashboardEvent
	RecentErrors    []DashboardEvent
	RecentWarnings  []DashboardEvent
	QueueStatus     map[string]QueueInfo
	AgentStatus     map[string]AgentInfo
	ActiveCommands  []ActiveCommandInfo
	WorkerSummaries []WorkerSummary
	FormationStatus string
	DaemonStatus    string
	LastUpdated     time.Time
	IsStale         bool
	StaleReason     string
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
	clock       Clock

	tmplOnce sync.Once
	tmplVal  *template.Template
	tmplErr  error
}

// NewDashboardFormatter creates a new dashboard formatter
func NewDashboardFormatter(maestroDir string) *DashboardFormatter {
	return &DashboardFormatter{
		logPath:     filepath.Join(maestroDir, "logs", "maestro.jsonl"),
		maestroDir:  maestroDir,
		maxEvents:   20,
		maxErrors:   10,
		maxWarnings: 10,
		clock:       RealClock{},
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

// getDashboardTemplate returns the dashboard template, caching it after the first parse.
func (f *DashboardFormatter) getDashboardTemplate() (*template.Template, error) {
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
| Cancelled | {{ .Stats.CancelledTasks }} |
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

// UpdateDashboardFileWithQueues generates the dashboard using both JSONL logs
// and live queue data. This is the unified entry point for SIER-002:
// MetricsHandler delegates here so that dashboard.md is generated from a single path.
// The output format includes queue-depth, active commands, and worker task sections
// derived from live queue data, plus log-based statistics and events.
func (f *DashboardFormatter) UpdateDashboardFileWithQueues(
	cq model.CommandQueue,
	taskQueues map[string]*taskQueueEntry,
	nq model.NotificationQueue,
) error {
	var sb strings.Builder
	sb.WriteString("# Maestro Dashboard\n\n")
	fmt.Fprintf(&sb, "Updated: %s\n\n", f.clock.Now().UTC().Format(time.RFC3339))

	// Queue Depth table (from live queue data)
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

	// Active commands
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

	// Worker tasks
	sb.WriteString("\n## Worker Tasks\n\n")
	for _, wID := range workerKeys {
		fmt.Fprintf(&sb, "- **%s**: %d pending, %d in_progress\n", wID, workerPending[wID], workerInProg[wID])
	}

	// Enrich with log-based statistics (best-effort)
	data, dataErr := f.collectDashboardData()
	if dataErr != nil {
		log.Printf("[WARN] collectDashboardData: %v (continuing with partial data)", dataErr)
	}
	if data != nil {
		if data.IsStale {
			fmt.Fprintf(&sb, "\n> ⚠ [STALE] log data unavailable: %s\n", data.StaleReason)
		}
		if data.Stats.TotalTasks > 0 || data.Stats.ErrorCount > 0 {
			sb.WriteString("\n## Recent Activity\n\n")
			fmt.Fprintf(&sb, "Tasks: %d total, %d completed, %d failed\n",
				data.Stats.TotalTasks, data.Stats.CompletedTasks, data.Stats.FailedTasks)
			fmt.Fprintf(&sb, "Errors: %d, Warnings: %d\n", data.Stats.ErrorCount, data.Stats.WarningCount)
		}
	}

	dashboardPath := filepath.Join(f.maestroDir, "dashboard.md")
	return atomicWriteText(dashboardPath, sb.String())
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

// UpdateDashboardFile updates the dashboard.md file using atomic write.
func (f *DashboardFormatter) UpdateDashboardFile() error {
	formatted, err := f.FormatDashboard()
	if err != nil {
		return fmt.Errorf("failed to format dashboard: %w", err)
	}

	dashboardPath := filepath.Join(f.maestroDir, "dashboard.md")
	return atomicWriteText(dashboardPath, formatted)
}
