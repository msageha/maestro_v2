package status

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetQueueDepths_EmptyQueues(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	os.Mkdir(queueDir, 0755)

	// Write empty queue files
	os.WriteFile(filepath.Join(queueDir, "planner.yaml"),
		[]byte("schema_version: 1\nfile_type: \"queue_command\"\ncommands: []\n"), 0644)
	os.WriteFile(filepath.Join(queueDir, "worker1.yaml"),
		[]byte("schema_version: 1\nfile_type: \"queue_task\"\ntasks: []\n"), 0644)

	queues := CollectQueueCounts(dir)
	if len(queues) != 2 {
		t.Fatalf("expected 2 queues, got %d", len(queues))
	}

	for _, q := range queues {
		if q.Pending != 0 || q.InProgress != 0 {
			t.Errorf("queue %s: expected 0/0, got %d/%d", q.Name, q.Pending, q.InProgress)
		}
	}
}

func TestGetQueueDepths_WithEntries(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	os.Mkdir(queueDir, 0755)

	content := `schema_version: 1
file_type: "queue_task"
tasks:
  - id: "task_0000000001_aaaaaaaa"
    status: "pending"
  - id: "task_0000000002_bbbbbbbb"
    status: "in_progress"
  - id: "task_0000000003_cccccccc"
    status: "pending"
  - id: "task_0000000004_dddddddd"
    status: "completed"
`
	os.WriteFile(filepath.Join(queueDir, "worker1.yaml"), []byte(content), 0644)

	queues := CollectQueueCounts(dir)
	if len(queues) != 1 {
		t.Fatalf("expected 1 queue, got %d", len(queues))
	}

	q := queues[0]
	if q.Name != "worker1" {
		t.Errorf("name: got %q", q.Name)
	}
	if q.Pending != 2 {
		t.Errorf("pending: got %d, want 2", q.Pending)
	}
	if q.InProgress != 1 {
		t.Errorf("in_progress: got %d, want 1", q.InProgress)
	}
}

func TestGetQueueDepths_NoQueueDir(t *testing.T) {
	dir := t.TempDir()
	queues := CollectQueueCounts(dir)
	if queues != nil {
		t.Errorf("expected nil for missing queue dir, got %v", queues)
	}
}

func TestGetQueueDepths_SkipsInvalidSchema(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	os.Mkdir(queueDir, 0755)

	// Valid file
	os.WriteFile(filepath.Join(queueDir, "planner.yaml"),
		[]byte("schema_version: 1\nfile_type: \"queue_command\"\ncommands: []\n"), 0644)

	// Invalid schema (missing file_type)
	os.WriteFile(filepath.Join(queueDir, "bad.yaml"),
		[]byte("schema_version: 1\ntasks: []\n"), 0644)

	// Invalid YAML
	os.WriteFile(filepath.Join(queueDir, "corrupt.yaml"),
		[]byte(":::invalid yaml:::"), 0644)

	queues := CollectQueueCounts(dir)
	// Only the valid file should be processed
	if len(queues) != 1 {
		t.Fatalf("expected 1 queue (valid only), got %d", len(queues))
	}
	if queues[0].Name != "planner" {
		t.Errorf("expected planner, got %q", queues[0].Name)
	}
}

// aliasBombYAML returns a YAML document whose alias nesting exceeds
// maestroyaml.MaxAliasDepth. Each level references the previous one, so a
// naive yaml.Unmarshal would expand it exponentially (billion laughs).
func aliasBombYAML(header string) string {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("a0: &a0 [\"x\",\"x\"]\n")
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&b, "a%d: &a%d [*a%d,*a%d]\n", i, i, i-1, i-1)
	}
	return b.String()
}

// TestGetQueueDepths_SkipsAliasBomb pins P-F6: queue-file reads go through
// SafeUnmarshal, so an anchor/alias bomb is rejected instead of expanded.
func TestGetQueueDepths_SkipsAliasBomb(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	os.Mkdir(queueDir, 0755)

	os.WriteFile(filepath.Join(queueDir, "planner.yaml"),
		[]byte("schema_version: 1\nfile_type: \"queue_command\"\ncommands: []\n"), 0644)
	os.WriteFile(filepath.Join(queueDir, "bomb.yaml"),
		[]byte(aliasBombYAML("schema_version: 1\nfile_type: \"queue_task\"\ntasks: []\n")), 0644)

	queues := CollectQueueCounts(dir)
	if len(queues) != 1 {
		t.Fatalf("expected 1 queue (bomb skipped), got %d", len(queues))
	}
	if queues[0].Name != "planner" {
		t.Errorf("expected planner, got %q", queues[0].Name)
	}
}

// TestGetSignalsStatus_SkipsAliasBomb pins P-F6 for the planner_signals path.
func TestGetSignalsStatus_SkipsAliasBomb(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	os.Mkdir(queueDir, 0755)

	os.WriteFile(filepath.Join(queueDir, "planner_signals.yaml"),
		[]byte(aliasBombYAML("schema_version: 1\nfile_type: \"planner_signals\"\nsignals: []\n")), 0644)

	if got := getSignalsStatus(dir); got != nil {
		t.Fatalf("expected nil for alias-bomb planner_signals, got %+v", got)
	}
}

func TestCheckDaemon_NotRunning(t *testing.T) {
	// Non-existent socket should report not running
	status := checkDaemon("/tmp/nonexistent-maestro-test.sock")
	if status.Running {
		t.Error("expected daemon not running")
	}
}

func TestGetSignalsStatus_NoFile(t *testing.T) {
	dir := t.TempDir()
	if got := getSignalsStatus(dir); got != nil {
		t.Errorf("missing planner_signals.yaml must yield nil, got %+v", got)
	}
}

func TestGetSignalsStatus_EmptyQueue(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	if err := os.Mkdir(queueDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := `schema_version: 1
file_type: "planner_signal_queue"
signals: []
`
	if err := os.WriteFile(filepath.Join(queueDir, "planner_signals.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got := getSignalsStatus(dir)
	if got == nil {
		t.Fatal("expected non-nil status for empty queue, got nil")
	}
	if got.Total != 0 {
		t.Errorf("Total: got %d, want 0", got.Total)
	}
	if got.MaxAttempts != 0 || got.LastAttemptAt != "" || got.LastError != "" {
		t.Errorf("empty queue must yield zero-valued fields, got %+v", got)
	}
}

func TestGetSignalsStatus_AggregatesAttemptsAndPicksMostRecent(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	if err := os.Mkdir(queueDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Two signals: one with higher Attempts but older LastAttemptAt; one
	// with smaller Attempts but more recent LastAttemptAt + a LastError.
	// LastAttemptAt picks the most recent timestamp; LastError must follow
	// that same entry, not the entry with the highest Attempts.
	content := `schema_version: 1
file_type: "planner_signal_queue"
signals:
  - kind: "merge_conflict"
    command_id: "cmd1"
    phase_id: "ph1"
    message: "stale"
    attempts: 9
    last_attempt_at: "2026-04-27T10:00:00Z"
    next_attempt_at: "2026-04-27T10:05:00Z"
    last_error: "older failure"
    created_at: "2026-04-27T09:00:00Z"
    updated_at: "2026-04-27T10:00:00Z"
  - kind: "commit_failed"
    command_id: "cmd2"
    phase_id: "ph2"
    message: "recent"
    attempts: 3
    last_attempt_at: "2026-04-28T10:00:00Z"
    next_attempt_at: "2026-04-28T10:05:00Z"
    last_error: "tmux load-buffer timeout"
    created_at: "2026-04-28T09:00:00Z"
    updated_at: "2026-04-28T10:00:00Z"
`
	if err := os.WriteFile(filepath.Join(queueDir, "planner_signals.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got := getSignalsStatus(dir)
	if got == nil {
		t.Fatal("expected non-nil status, got nil")
	}
	if got.Total != 2 {
		t.Errorf("Total: got %d, want 2", got.Total)
	}
	if got.MaxAttempts != 9 {
		t.Errorf("MaxAttempts: got %d, want 9", got.MaxAttempts)
	}
	if got.LastAttemptAt != "2026-04-28T10:00:00Z" {
		t.Errorf("LastAttemptAt: got %q, want most-recent timestamp", got.LastAttemptAt)
	}
	if got.LastError != "tmux load-buffer timeout" {
		t.Errorf("LastError: got %q, want LastError of most-recent entry", got.LastError)
	}
	if !got.Degraded {
		t.Errorf("Degraded: must be true when MaxAttempts (9) >= threshold (%d)", signalDegradationAttemptsThreshold)
	}
}

// TestGetSignalsStatus_LowAttemptsNotDegraded pins the negative side of the
// degraded flag: a signal with Attempts well below the threshold (e.g. one
// retry after a transient hiccup) must not flip status to degraded, so the
// flag remains a real "tmux delivery is stuck" indicator instead of noise
// every operator sees.
func TestGetSignalsStatus_LowAttemptsNotDegraded(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	if err := os.Mkdir(queueDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := `schema_version: 1
file_type: "planner_signal_queue"
signals:
  - kind: "merge_conflict"
    command_id: "cmd1"
    phase_id: "ph1"
    message: "ok"
    attempts: 2
    last_attempt_at: "2026-04-28T10:00:00Z"
    next_attempt_at: "2026-04-28T10:05:00Z"
    created_at: "2026-04-28T09:00:00Z"
    updated_at: "2026-04-28T10:00:00Z"
`
	if err := os.WriteFile(filepath.Join(queueDir, "planner_signals.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got := getSignalsStatus(dir)
	if got == nil {
		t.Fatal("expected non-nil status, got nil")
	}
	if got.Degraded {
		t.Errorf("Degraded: must be false when MaxAttempts (2) < threshold (%d)", signalDegradationAttemptsThreshold)
	}
}

// TestAnnotateStaleBusyAgents_FlagsBusyWithoutQueueWork verifies that an
// agent reporting @status=busy with no queue file showing an in-progress
// task is rewritten to "busy (stale)" so the inconsistency is immediately
// observable in `maestro status`.
func TestAnnotateStaleBusyAgents_FlagsBusyWithoutQueueWork(t *testing.T) {
	agents := []agentStatus{
		{ID: "worker1", Role: "worker", Status: "busy"},
		{ID: "worker4", Role: "worker", Status: "busy"},
		{ID: "planner", Role: "planner", Status: "idle"},
	}
	queues := []QueueCount{
		{Name: "worker1", InProgress: 1, Pending: 0},
		{Name: "worker4", InProgress: 0, Pending: 0},
	}
	out := annotateStaleBusyAgents(agents, queues)
	statusByID := map[string]string{}
	for _, a := range out {
		statusByID[a.ID] = a.Status
	}
	if statusByID["worker1"] != "busy" {
		t.Errorf("worker1 must stay busy when queue has in-progress, got %q", statusByID["worker1"])
	}
	if statusByID["worker4"] != "busy (stale)" {
		t.Errorf("worker4 must flip to busy (stale) when queue has no in-progress, got %q", statusByID["worker4"])
	}
	if statusByID["planner"] != "idle" {
		t.Errorf("planner status (idle) must not be touched by the annotation, got %q", statusByID["planner"])
	}
}

// TestAnnotateStaleBusyAgents_SkipsWhenQueueFileAbsent verifies that
// when no queue file backs the busy claim we leave the row alone — the
// annotation should only fire when a queue file actually contradicts
// the busy state, not when the queue file is missing entirely (which
// could mean "not yet observed" rather than "stale busy").
func TestAnnotateStaleBusyAgents_SkipsWhenQueueFileAbsent(t *testing.T) {
	agents := []agentStatus{
		{ID: "worker99", Role: "worker", Status: "busy"},
	}
	out := annotateStaleBusyAgents(agents, nil)
	if out[0].Status != "busy" {
		t.Errorf("absent queue file must not trigger stale annotation, got %q", out[0].Status)
	}
}

// TestAnnotateStaleBusyAgents_PlannerAndOrchestratorMapByRole confirms
// the role-based mapping for planner/orchestrator (their queue files
// are not named after a worker pattern).
func TestAnnotateStaleBusyAgents_PlannerAndOrchestratorMapByRole(t *testing.T) {
	agents := []agentStatus{
		{ID: "planner", Role: "planner", Status: "busy"},
		{ID: "orchestrator", Role: "orchestrator", Status: "busy"},
	}
	queues := []QueueCount{
		{Name: "planner", InProgress: 0},
		{Name: "orchestrator", InProgress: 1},
	}
	out := annotateStaleBusyAgents(agents, queues)
	statusByID := map[string]string{}
	for _, a := range out {
		statusByID[a.ID] = a.Status
	}
	if statusByID["planner"] != "busy (stale)" {
		t.Errorf("planner with empty command queue must flip to stale, got %q", statusByID["planner"])
	}
	if statusByID["orchestrator"] != "busy" {
		t.Errorf("orchestrator with non-empty notifications must stay busy, got %q", statusByID["orchestrator"])
	}
}

func TestPrintStatus_DoesNotPanic(t *testing.T) {
	// Verify printing works without panicking for all cases
	s := formationStatus{
		Daemon: daemonStatus{Running: false},
	}
	printStatus(s)

	s.Daemon.Running = true
	s.Agents = []agentStatus{
		{ID: "orchestrator", Role: "orchestrator", Model: "opus", Status: "idle"},
	}
	s.Queues = []QueueCount{
		{Name: "planner", Pending: 3, InProgress: 1},
	}
	printStatus(s)
}

// TestParseAgentStatusLines_EvictedNotDead pins the 2026-04-28 retest2
// follow-up: when the daemon respawns a worker pane to project root for
// worktree cleanup, it sets @agent_state="evicted" so `maestro status`
// can distinguish "intentionally between turns" from "crashed back to
// shell". Before this fix every successful command ended with the
// worker rows showing "dead" in the dashboard until the next dispatch.
func TestParseAgentStatusLines_EvictedNotDead(t *testing.T) {
	isShell := func(cmd string) bool { return cmd == "zsh" || cmd == "bash" }

	cases := []struct {
		name       string
		line       string
		wantStatus string
	}{
		{
			name:       "shell_with_evicted_state_renders_idle_evicted",
			line:       "worker1\tworker\tsonnet\tidle\tzsh\tclaude_code\tevicted",
			wantStatus: "idle (evicted)",
		},
		{
			name:       "shell_without_evicted_state_renders_dead",
			line:       "worker1\tworker\tsonnet\tidle\tzsh\tclaude_code\t",
			wantStatus: "dead",
		},
		{
			name:       "claude_running_keeps_status_field_value",
			line:       "worker1\tworker\tsonnet\tbusy\tnode\tclaude_code\tevicted",
			wantStatus: "busy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAgentStatusLines([]string{tc.line}, isShell)
			if len(got) != 1 {
				t.Fatalf("expected 1 row, got %d", len(got))
			}
			if got[0].Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got[0].Status, tc.wantStatus)
			}
		})
	}
}
