package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDashboardFormatter_FormatDashboard(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	logsDir := filepath.Join(tmpDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0755))

	// Create sample JSONL log file
	logPath := filepath.Join(logsDir, "maestro.jsonl")
	createSampleLogFile(t, logPath)

	// Create formatter
	formatter := NewDashboardFormatter(tmpDir)

	// Format dashboard
	output, err := formatter.FormatDashboard()
	require.NoError(t, err)
	assert.NotEmpty(t, output)

	// Verify output contains expected sections
	assert.Contains(t, output, "# Maestro Dashboard")
	assert.Contains(t, output, "## System Status")
	assert.Contains(t, output, "## Task Statistics")
	assert.Contains(t, output, "## Queue Status")
	assert.Contains(t, output, "## Agent Status")
	assert.Contains(t, output, "## Recent Errors")
	assert.Contains(t, output, "## Recent Warnings")
	assert.Contains(t, output, "## Recent Activity")
}

func TestDashboardFormatter_ParseLogFile(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	logsDir := filepath.Join(tmpDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0755))

	// Create sample JSONL log file
	logPath := filepath.Join(logsDir, "maestro.jsonl")
	createSampleLogFile(t, logPath)

	// Create formatter
	formatter := NewDashboardFormatter(tmpDir)

	// Collect dashboard data
	data, err := formatter.collectDashboardData()
	require.NoError(t, err)
	assert.NotNil(t, data)

	// Verify statistics - task_003 only has warning but no status, so it doesn't count as a task
	// task_002 gets counted once with failed status, then updated to in_progress when retried
	assert.Equal(t, 2, data.Stats.TotalTasks) // task_001 and task_002
	assert.Equal(t, 1, data.Stats.CompletedTasks) // task_001
	assert.Equal(t, 0, data.Stats.FailedTasks) // task_002 was failed but is now in_progress due to retry
	assert.Equal(t, 1, data.Stats.InProgressTasks) // task_002 after retry
	assert.Equal(t, 1, data.Stats.ErrorCount) // task_failed
	assert.Equal(t, 2, data.Stats.WarningCount) // lease_warning and task_retry

	// Verify events were collected
	assert.NotEmpty(t, data.RecentEvents)
	assert.NotEmpty(t, data.RecentErrors)
	assert.NotEmpty(t, data.RecentWarnings)

	// Verify agent status
	assert.NotEmpty(t, data.AgentStatus)
}

func TestDashboardFormatter_EventFiltering(t *testing.T) {
	formatter := &DashboardFormatter{}

	testCases := []struct {
		name         string
		eventType    string
		isTask       bool
		isError      bool
		isWarning    bool
	}{
		{"task_created", "task_created", true, false, false},
		{"task_completed", "task_completed", true, false, false},
		{"task_failed", "task_failed", true, true, false},
		{"command_error", "command_error", false, true, false},
		{"lease_warning", "lease_warning", false, false, true},
		{"task_retry", "task_retry", true, false, true},
		{"random_event", "random_event", false, false, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.isTask, formatter.isTaskRelated(tc.eventType))
			assert.Equal(t, tc.isError, formatter.isErrorEvent(tc.eventType))
			assert.Equal(t, tc.isWarning, formatter.isWarningEvent(tc.eventType))
		})
	}
}

func TestDashboardFormatter_ExtractEvent(t *testing.T) {
	formatter := &DashboardFormatter{}

	entry := events.LogEntry{
		Timestamp: time.Now(),
		EventType: "task_completed",
		TaskID:    "task_123",
		AgentID:   "worker1",
		Details: map[string]interface{}{
			"status":  "completed",
			"summary": "Task completed successfully",
		},
	}

	event := formatter.extractEvent(entry)

	assert.Equal(t, entry.Timestamp, event.Timestamp)
	assert.Equal(t, "task_completed", event.EventType)
	assert.Equal(t, "task_123", event.TaskID)
	assert.Equal(t, "worker1", event.AgentID)
	assert.Equal(t, "completed", event.Status)
	assert.Equal(t, "Task completed successfully", event.Summary)
	assert.False(t, event.IsError)
	assert.False(t, event.IsWarning)
}

func TestDashboardFormatter_LimitEvents(t *testing.T) {
	formatter := &DashboardFormatter{
		maxEvents:   5,
		maxErrors:   3,
		maxWarnings: 3,
	}

	data := &DashboardData{
		RecentEvents:   make([]DashboardEvent, 10),
		RecentErrors:   make([]DashboardEvent, 10),
		RecentWarnings: make([]DashboardEvent, 10),
	}

	formatter.limitEvents(data)

	assert.Len(t, data.RecentEvents, 5)
	assert.Len(t, data.RecentErrors, 3)
	assert.Len(t, data.RecentWarnings, 3)
}

func TestDashboardFormatter_WriteDashboard(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	logsDir := filepath.Join(tmpDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0755))

	// Create sample JSONL log file
	logPath := filepath.Join(logsDir, "maestro.jsonl")
	createSampleLogFile(t, logPath)

	// Create formatter
	formatter := NewDashboardFormatter(tmpDir)

	// Write to buffer
	var buf bytes.Buffer
	err := formatter.WriteDashboard(&buf)
	require.NoError(t, err)

	// Verify output
	output := buf.String()
	assert.NotEmpty(t, output)
	assert.Contains(t, output, "# Maestro Dashboard")
}

func TestDashboardFormatter_UpdateDashboardFile(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	logsDir := filepath.Join(tmpDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0755))

	// Create sample JSONL log file
	logPath := filepath.Join(logsDir, "maestro.jsonl")
	createSampleLogFile(t, logPath)

	// Create formatter
	formatter := NewDashboardFormatter(tmpDir)

	// Update dashboard file
	err := formatter.UpdateDashboardFile()
	require.NoError(t, err)

	// Verify file was created
	dashboardPath := filepath.Join(tmpDir, "dashboard.md")
	assert.FileExists(t, dashboardPath)

	// Read and verify content
	content, err := os.ReadFile(dashboardPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "# Maestro Dashboard")
}

func TestDashboardFormatter_EmptyLogFile(t *testing.T) {
	// Create temp directory without log file
	tmpDir := t.TempDir()

	// Create formatter
	formatter := NewDashboardFormatter(tmpDir)

	// Format dashboard - should not error
	output, err := formatter.FormatDashboard()
	require.NoError(t, err)
	assert.NotEmpty(t, output)

	// Should contain empty sections
	assert.Contains(t, output, "_No recent errors._")
	assert.Contains(t, output, "_No recent warnings._")
	assert.Contains(t, output, "_No recent activity._")
}

func TestDashboardFormatter_CalculateStats(t *testing.T) {
	formatter := &DashboardFormatter{}

	data := &DashboardData{
		Stats: DashboardStats{
			TotalTasks:     10,
			CompletedTasks: 7,
			FailedTasks:    2,
			InProgressTasks: 1,
		},
	}

	formatter.calculateStats(data)

	// Verify success rate calculation
	assert.Equal(t, 70.0, data.Stats.TaskSuccessRate)
}

// Helper function to create a sample JSONL log file
func createSampleLogFile(t *testing.T, logPath string) {
	file, err := os.Create(logPath)
	require.NoError(t, err)
	defer file.Close()

	// Write sample log entries
	entries := []events.LogEntry{
		{
			Timestamp: time.Now().Add(-5 * time.Minute),
			EventType: "task_created",
			TaskID:    "task_001",
			AgentID:   "worker1",
			Details: map[string]interface{}{
				"status": "pending",
			},
		},
		{
			Timestamp: time.Now().Add(-4 * time.Minute),
			EventType: "task_started",
			TaskID:    "task_001",
			AgentID:   "worker1",
			Details: map[string]interface{}{
				"status": "in_progress",
			},
		},
		{
			Timestamp: time.Now().Add(-3 * time.Minute),
			EventType: "task_completed",
			TaskID:    "task_001",
			AgentID:   "worker1",
			Details: map[string]interface{}{
				"status":  "completed",
				"summary": "Successfully processed data",
			},
		},
		{
			Timestamp: time.Now().Add(-2 * time.Minute),
			EventType: "task_failed",
			TaskID:    "task_002",
			AgentID:   "worker2",
			Details: map[string]interface{}{
				"status": "failed",
				"error":  "Connection timeout",
			},
		},
		{
			Timestamp: time.Now().Add(-1 * time.Minute),
			EventType: "lease_warning",
			TaskID:    "task_003",
			Details: map[string]interface{}{
				"message": "Lease expiring soon",
			},
		},
		{
			Timestamp: time.Now(),
			EventType: "task_retry",
			TaskID:    "task_002",
			AgentID:   "worker3",
			Details: map[string]interface{}{
				"message": "Retrying after failure",
			},
		},
	}

	encoder := json.NewEncoder(file)
	for _, entry := range entries {
		require.NoError(t, encoder.Encode(entry))
	}
}

// TestDashboardFormatter_SortEvents tests event sorting functionality
func TestDashboardFormatter_SortEvents(t *testing.T) {
	formatter := &DashboardFormatter{}

	now := time.Now()
	data := &DashboardData{
		RecentEvents: []DashboardEvent{
			{Timestamp: now.Add(-3 * time.Minute)},
			{Timestamp: now},
			{Timestamp: now.Add(-1 * time.Minute)},
		},
	}

	formatter.sortEvents(data)

	// Verify events are sorted most recent first
	assert.True(t, data.RecentEvents[0].Timestamp.After(data.RecentEvents[1].Timestamp))
	assert.True(t, data.RecentEvents[1].Timestamp.After(data.RecentEvents[2].Timestamp))
}

// TestDashboardFormatter_Template tests template rendering
func TestDashboardFormatter_Template(t *testing.T) {
	formatter := &DashboardFormatter{}

	tmpl, err := formatter.getDashboardTemplate()
	require.NoError(t, err)

	data := &DashboardData{
		DaemonStatus:    "Running",
		FormationStatus: "Active",
		Stats: DashboardStats{
			TotalTasks:      10,
			CompletedTasks:  7,
			FailedTasks:     2,
			InProgressTasks: 1,
			TaskSuccessRate: 70.0,
		},
		LastUpdated: time.Now(),
	}

	var buf strings.Builder
	err = tmpl.Execute(&buf, data)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "Daemon    | Running")
	assert.Contains(t, output, "Total Tasks | 10")
	assert.Contains(t, output, "Completed | 7 (70.0%)")
}