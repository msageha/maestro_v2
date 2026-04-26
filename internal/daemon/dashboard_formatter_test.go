package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDashboardFormatter_WorktreeWarningsVisible is the 2026-04 audit Bug 3
// regression. Dashboard must surface (a) integration statuses that block
// recovery (conflict, partial_merge, failed, publish_failed, quarantined) and
// (b) commit_failed_workers entries — previously both vanished into a "0
// warnings" summary.
func TestDashboardFormatter_WorktreeWarningsVisible(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	for _, sub := range []string{"logs", "state/worktrees"} {
		require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, sub), 0755))
	}
	fixTestDirPerms(t, tmpDir)

	// Seed a worktree state with one conflict + commit_failed_workers.
	wtState := []byte(`schema_version: 1
file_type: state_worktree
command_id: cmd_stuck
integration:
  command_id: cmd_stuck
  branch: maestro/cmd_stuck/integration
  base_sha: abc123
  status: conflict
  merge_failure_count: 2
  created_at: "2026-01-01T00:00:00Z"
  updated_at: "2026-01-01T00:00:00Z"
workers: []
commit_failed_workers:
  - worker2
created_at: "2026-01-01T00:00:00Z"
updated_at: "2026-01-01T00:00:00Z"
`)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "state", "worktrees", "cmd_stuck.yaml"), wtState, 0644))

	f := NewDashboardFormatter(tmpDir)
	// Call via the live-data entrypoint with empty queues — the worktree
	// section must still render.
	var (
		cq model.CommandQueue
		nq model.NotificationQueue
	)
	require.NoError(t, f.UpdateDashboardFileWithQueues(cq, map[string]*taskQueueEntry{}, nq))

	out, err := os.ReadFile(filepath.Join(tmpDir, "dashboard.md"))
	require.NoError(t, err)
	content := string(out)

	assert.Contains(t, content, "## Worktree Integration Status")
	assert.Contains(t, content, "cmd_stuck")
	assert.Contains(t, content, "conflict")
	assert.Contains(t, content, "worker2", "commit_failed_workers must be listed")
	assert.Contains(t, content, "worktree-integration warning",
		"operators need an explicit warning banner so the 'Tasks: all completed, 0 warnings' line is no longer misleading")
}

func TestDashboardFormatter_FormatDashboard(t *testing.T) {
	t.Parallel()
	// Create temp directory
	tmpDir := t.TempDir()
	logsDir := filepath.Join(tmpDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0755))
	fixTestDirPerms(t, tmpDir)

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

func TestDashboardFormatter_VerifySkipVisible(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "logs"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "state"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "state", "verify_status.yaml"), []byte(`
schema_version: 1
file_type: verify_status
mode: skipped
reason: verify.enabled=false with MAESTRO_ALLOW_VERIFY_SKIP=1
`), 0o644))
	fixTestDirPerms(t, tmpDir)

	output, err := NewDashboardFormatter(tmpDir).FormatDashboard()
	require.NoError(t, err)
	assert.Contains(t, output, "| Verify    | skipped (verify.enabled=false with MAESTRO_ALLOW_VERIFY_SKIP=1) |")
}

func TestDashboardFormatter_ParseLogFile(t *testing.T) {
	t.Parallel()
	// Create temp directory
	tmpDir := t.TempDir()
	logsDir := filepath.Join(tmpDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0755))
	fixTestDirPerms(t, tmpDir)

	// Create sample JSONL log file
	logPath := filepath.Join(logsDir, "maestro.jsonl")
	createSampleLogFile(t, logPath)

	// Create state files for task statistics
	createSampleStateFiles(t, tmpDir)

	// Create formatter
	formatter := NewDashboardFormatter(tmpDir)

	// Collect dashboard data
	data, err := formatter.collectDashboardData()
	require.NoError(t, err)
	assert.NotNil(t, data)

	// Verify task statistics from state files
	assert.Equal(t, 2, data.Stats.TotalTasks)      // task_001 and task_002
	assert.Equal(t, 1, data.Stats.CompletedTasks)  // task_001
	assert.Equal(t, 0, data.Stats.FailedTasks)     // none currently failed
	assert.Equal(t, 1, data.Stats.InProgressTasks) // task_002
	assert.Equal(t, 1, data.Stats.ErrorCount)      // task_failed log event
	assert.Equal(t, 2, data.Stats.WarningCount)    // lease_warning and task_retry

	// Verify events were collected from logs
	assert.NotEmpty(t, data.RecentEvents)
	assert.NotEmpty(t, data.RecentErrors)
	assert.NotEmpty(t, data.RecentWarnings)

	// Verify agent status from logs
	assert.NotEmpty(t, data.AgentStatus)
}

func TestDashboardFormatter_EventFiltering(t *testing.T) {
	t.Parallel()
	formatter := &DashboardFormatter{}

	testCases := []struct {
		name      string
		eventType string
		isTask    bool
		isError   bool
		isWarning bool
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
			t.Parallel()
			assert.Equal(t, tc.isTask, formatter.isTaskRelated(tc.eventType))
			assert.Equal(t, tc.isError, formatter.isErrorEvent(tc.eventType))
			assert.Equal(t, tc.isWarning, formatter.isWarningEvent(tc.eventType))
		})
	}
}

func TestDashboardFormatter_ExtractEvent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestDashboardFormatter_UpdateDashboardFile(t *testing.T) {
	t.Parallel()
	// Create temp directory
	tmpDir := t.TempDir()
	logsDir := filepath.Join(tmpDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0755))
	fixTestDirPerms(t, tmpDir)

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
	t.Parallel()
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
	t.Parallel()
	formatter := &DashboardFormatter{}

	data := &DashboardData{
		Stats: DashboardStats{
			TotalTasks:      10,
			CompletedTasks:  7,
			FailedTasks:     2,
			InProgressTasks: 1,
		},
	}

	formatter.calculateStats(data)

	// Verify success rate calculation
	assert.Equal(t, 70.0, data.Stats.TaskSuccessRate)
}

// Helper function to create a sample JSONL log file
func createSampleLogFile(t *testing.T, logPath string) {
	t.Helper()
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

// createSampleStateFiles creates command state YAML files for testing.
func createSampleStateFiles(t *testing.T, maestroDir string) {
	t.Helper()
	stateDir := filepath.Join(maestroDir, "state", "commands")
	require.NoError(t, os.MkdirAll(stateDir, 0755))
	os.Chmod(stateDir, 0755)

	// Command with task_001 (completed) and task_002 (in_progress after retry).
	// task_002_orig is a retry ancestor kept in task_states but NOT in required_task_ids,
	// so it must not be double-counted.
	stateYAML := `schema_version: 1
file_type: state_command
command_id: cmd_001
plan_status: sealed
expected_task_count: 2
required_task_ids:
  - task_001
  - task_002
task_states:
  task_001: completed
  task_002: in_progress
  task_002_orig: failed
`
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, "cmd_001.yaml"), []byte(stateYAML), 0644))
}

// TestDashboardFormatter_SortEvents tests event sorting functionality
func TestDashboardFormatter_SortEvents(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestDashboardFormatter_CalculateStats_EdgeCases(t *testing.T) {
	t.Parallel()
	formatter := &DashboardFormatter{}

	testCases := []struct {
		name         string
		total        int
		completed    int
		failed       int
		expectedRate float64
	}{
		{
			name:         "zero total tasks",
			total:        0,
			completed:    0,
			failed:       0,
			expectedRate: 0,
		},
		{
			name:         "all completed",
			total:        10,
			completed:    10,
			failed:       0,
			expectedRate: 100.0,
		},
		{
			name:         "all failed",
			total:        10,
			completed:    0,
			failed:       10,
			expectedRate: 0,
		},
		{
			name:         "mixed with large numbers",
			total:        10000,
			completed:    7777,
			failed:       2223,
			expectedRate: 77.77,
		},
		{
			name:         "single task completed",
			total:        1,
			completed:    1,
			failed:       0,
			expectedRate: 100.0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := &DashboardData{
				Stats: DashboardStats{
					TotalTasks:     tc.total,
					CompletedTasks: tc.completed,
					FailedTasks:    tc.failed,
				},
			}

			formatter.calculateStats(data)

			assert.InDelta(t, tc.expectedRate, data.Stats.TaskSuccessRate, 0.01,
				"success rate mismatch for %s", tc.name)
		})
	}
}

func TestDashboardFormatter_ExtractEvent_MissingFields(t *testing.T) {
	t.Parallel()
	formatter := &DashboardFormatter{}

	testCases := []struct {
		name            string
		entry           events.LogEntry
		expectedStatus  string
		expectedSummary string
		expectedIsError bool
		expectedIsWarn  bool
	}{
		{
			name: "empty details map",
			entry: events.LogEntry{
				Timestamp: time.Now(),
				EventType: "task_created",
				TaskID:    "task_100",
				Details:   map[string]interface{}{},
			},
			expectedStatus:  "",
			expectedSummary: "",
			expectedIsError: false,
			expectedIsWarn:  false,
		},
		{
			name: "nil details map",
			entry: events.LogEntry{
				Timestamp: time.Now(),
				EventType: "task_started",
				TaskID:    "task_200",
				Details:   nil,
			},
			expectedStatus:  "",
			expectedSummary: "",
			expectedIsError: false,
			expectedIsWarn:  false,
		},
		{
			name: "only message key",
			entry: events.LogEntry{
				Timestamp: time.Now(),
				EventType: "task_created",
				TaskID:    "task_300",
				Details: map[string]interface{}{
					"message": "Something happened",
				},
			},
			expectedStatus:  "",
			expectedSummary: "Something happened",
			expectedIsError: false,
			expectedIsWarn:  false,
		},
		{
			name: "only error key",
			entry: events.LogEntry{
				Timestamp: time.Now(),
				EventType: "task_created",
				TaskID:    "task_400",
				Details: map[string]interface{}{
					"error": "Connection refused",
				},
			},
			expectedStatus:  "",
			expectedSummary: "Connection refused",
			expectedIsError: false,
			expectedIsWarn:  false,
		},
		{
			name: "task_failed sets IsError true",
			entry: events.LogEntry{
				Timestamp: time.Now(),
				EventType: "task_failed",
				TaskID:    "task_500",
				Details: map[string]interface{}{
					"error": "Disk full",
				},
			},
			expectedStatus:  "",
			expectedSummary: "Disk full",
			expectedIsError: true,
			expectedIsWarn:  false,
		},
		{
			name: "lease_warning sets IsWarning true",
			entry: events.LogEntry{
				Timestamp: time.Now(),
				EventType: "lease_warning",
				TaskID:    "task_600",
				Details: map[string]interface{}{
					"message": "Lease expiring",
				},
			},
			expectedStatus:  "",
			expectedSummary: "Lease expiring",
			expectedIsError: false,
			expectedIsWarn:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			event := formatter.extractEvent(tc.entry)

			assert.Equal(t, tc.entry.Timestamp, event.Timestamp)
			assert.Equal(t, tc.entry.EventType, event.EventType)
			assert.Equal(t, tc.entry.TaskID, event.TaskID)
			assert.Equal(t, tc.entry.AgentID, event.AgentID)
			assert.Equal(t, tc.expectedStatus, event.Status)
			assert.Equal(t, tc.expectedSummary, event.Summary)
			assert.Equal(t, tc.expectedIsError, event.IsError)
			assert.Equal(t, tc.expectedIsWarn, event.IsWarning)
		})
	}
}

func TestDashboardFormatter_LimitEvents_BelowMax(t *testing.T) {
	t.Parallel()
	formatter := &DashboardFormatter{
		maxEvents:   10,
		maxErrors:   10,
		maxWarnings: 10,
	}

	data := &DashboardData{
		RecentEvents:   make([]DashboardEvent, 3),
		RecentErrors:   make([]DashboardEvent, 2),
		RecentWarnings: make([]DashboardEvent, 1),
	}

	formatter.limitEvents(data)

	assert.Len(t, data.RecentEvents, 3, "events should not be truncated when below max")
	assert.Len(t, data.RecentErrors, 2, "errors should not be truncated when below max")
	assert.Len(t, data.RecentWarnings, 1, "warnings should not be truncated when below max")
}

func TestDashboardFormatter_LimitEvents_ExactMax(t *testing.T) {
	t.Parallel()
	formatter := &DashboardFormatter{
		maxEvents:   5,
		maxErrors:   3,
		maxWarnings: 4,
	}

	data := &DashboardData{
		RecentEvents:   make([]DashboardEvent, 5),
		RecentErrors:   make([]DashboardEvent, 3),
		RecentWarnings: make([]DashboardEvent, 4),
	}

	formatter.limitEvents(data)

	assert.Len(t, data.RecentEvents, 5, "events should not be truncated at exactly max")
	assert.Len(t, data.RecentErrors, 3, "errors should not be truncated at exactly max")
	assert.Len(t, data.RecentWarnings, 4, "warnings should not be truncated at exactly max")
}

func TestDashboardFormatter_FormatDashboard_WithStateFiles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	logsDir := filepath.Join(tmpDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0755))
	fixTestDirPerms(t, tmpDir)

	// Create both log file and state files
	logPath := filepath.Join(logsDir, "maestro.jsonl")
	createSampleLogFile(t, logPath)
	createSampleStateFiles(t, tmpDir)

	formatter := NewDashboardFormatter(tmpDir)

	output, err := formatter.FormatDashboard()
	require.NoError(t, err)
	assert.NotEmpty(t, output)

	// Verify specific stat values from state files (2 tasks: 1 completed, 1 in_progress)
	assert.Contains(t, output, "Total Tasks | 2")
	assert.Contains(t, output, "Completed | 1 (50.0%)")
	assert.Contains(t, output, "Failed | 0")
	assert.Contains(t, output, "In Progress | 1")

	// Verify log-based events appear with concrete details
	assert.Contains(t, output, "task_completed")
	assert.Contains(t, output, "task_failed")
	assert.Contains(t, output, "worker1")
	assert.Contains(t, output, "worker2")

	// Verify error and warning counts from log parsing
	assert.Contains(t, output, "Errors | 1")
	assert.Contains(t, output, "Warnings | 2")
}

func TestDashboardFormatter_SortEvents_SameTimestamp(t *testing.T) {
	t.Parallel()
	formatter := &DashboardFormatter{}

	now := time.Now()
	data := &DashboardData{
		RecentEvents: []DashboardEvent{
			{Timestamp: now, EventType: "a"},
			{Timestamp: now, EventType: "b"},
			{Timestamp: now, EventType: "c"},
		},
	}

	// Should not panic; all elements remain present
	assert.NotPanics(t, func() { formatter.sortEvents(data) })
	assert.Len(t, data.RecentEvents, 3)
}

func TestDashboardFormatter_SortEvents_ReverseOrder(t *testing.T) {
	t.Parallel()
	formatter := &DashboardFormatter{}

	now := time.Now()
	data := &DashboardData{
		RecentEvents: []DashboardEvent{
			{Timestamp: now.Add(-3 * time.Minute), EventType: "oldest"},
			{Timestamp: now.Add(-2 * time.Minute), EventType: "middle"},
			{Timestamp: now.Add(-1 * time.Minute), EventType: "newest"},
		},
	}

	formatter.sortEvents(data)

	assert.Equal(t, "newest", data.RecentEvents[0].EventType)
	assert.Equal(t, "middle", data.RecentEvents[1].EventType)
	assert.Equal(t, "oldest", data.RecentEvents[2].EventType)
}

func TestDashboardFormatter_SortEvents_ErrorsAndWarnings(t *testing.T) {
	t.Parallel()
	formatter := &DashboardFormatter{}

	now := time.Now()
	data := &DashboardData{
		RecentErrors: []DashboardEvent{
			{Timestamp: now.Add(-2 * time.Minute), EventType: "old_err"},
			{Timestamp: now, EventType: "new_err"},
		},
		RecentWarnings: []DashboardEvent{
			{Timestamp: now.Add(-1 * time.Minute), EventType: "old_warn"},
			{Timestamp: now, EventType: "new_warn"},
		},
	}

	formatter.sortEvents(data)

	assert.Equal(t, "new_err", data.RecentErrors[0].EventType)
	assert.Equal(t, "old_err", data.RecentErrors[1].EventType)
	assert.Equal(t, "new_warn", data.RecentWarnings[0].EventType)
	assert.Equal(t, "old_warn", data.RecentWarnings[1].EventType)
}

func TestDashboardFormatter_SortEvents_Empty(t *testing.T) {
	t.Parallel()
	formatter := &DashboardFormatter{}

	data := &DashboardData{
		RecentEvents:   []DashboardEvent{},
		RecentErrors:   []DashboardEvent{},
		RecentWarnings: []DashboardEvent{},
	}

	// Should not panic on empty slices
	assert.NotPanics(t, func() {
		formatter.sortEvents(data)
	})

	assert.Empty(t, data.RecentEvents)
	assert.Empty(t, data.RecentErrors)
	assert.Empty(t, data.RecentWarnings)
}
