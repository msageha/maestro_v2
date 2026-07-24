package hud

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixtureTime is the "now" used across fixture-based tests.
var fixtureTime = time.Date(2026, 7, 24, 12, 0, 5, 0, time.UTC)

// writeFixtureFile writes content under dir, creating parents.
func writeFixtureFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newFixtureMaestroDir builds a representative .maestro directory covering
// every HUD section.
func newFixtureMaestroDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".maestro")

	writeFixtureFile(t, dir, "queue/worker1.yaml", `schema_version: 1
file_type: queue_task
tasks:
  - id: task_1
    status: pending
  - id: task_2
    status: in_progress
`)
	writeFixtureFile(t, dir, "queue/planner.yaml", `schema_version: 1
file_type: queue_command
commands:
  - id: cmd_1
    status: in_progress
`)
	writeFixtureFile(t, dir, "queue/planner_signals.yaml", `schema_version: 1
file_type: planner_signal_queue
signals:
  - kind: merge_conflict
    command_id: cmd_1
    phase_id: ph_2
    worker_id: worker1
    attempts: 3
    last_error: "merge failed on internal/a.go"
    created_at: "2026-07-24T11:59:00Z"
    updated_at: "2026-07-24T11:59:30Z"
`)

	writeFixtureFile(t, dir, "state/metrics.yaml", `schema_version: 1
file_type: state_metrics
queue_depth:
  planner: 0
  orchestrator: 0
counters:
  commands_dispatched: 3
  tasks_dispatched: 12
  tasks_completed: 9
  tasks_failed: 1
  tasks_cancelled: 0
  dead_letters: 0
daemon_heartbeat: "2026-07-24T12:00:02Z"
updated_at: "2026-07-24T12:00:02Z"
usage:
  source: "claude-code session files (~/.claude/projects)"
  partial: true
  collected_at: "2026-07-24T12:00:00Z"
  agents:
    worker1:
      runtime: claude-code
      tokens_known: true
      totals:
        input_tokens: 1200000
        output_tokens: 34000
        cache_read_input_tokens: 9800000
        cache_creation_input_tokens: 450000
      estimated_cost_usd: 4.21
    worker2:
      runtime: codex
      tokens_known: false
  commands:
    cmd_1:
      totals:
        input_tokens: 800000
        output_tokens: 20000
      estimated_cost_usd: 2.10
  budget_alerts:
    - "agent worker1 exceeded per-agent budget"
`)

	writeFixtureFile(t, dir, "state/commands/cmd_1.yaml", `schema_version: 1
file_type: state_command
command_id: cmd_1
plan_version: 1
plan_status: sealed
required_task_ids: [task_1, task_2, task_3]
optional_task_ids: [task_4]
task_states:
  task_1: completed
  task_2: running
  task_3: failed
phases:
  - phase_id: ph_1
    name: design
    type: concrete
    status: completed
  - phase_id: ph_2
    name: implement
    type: concrete
    status: active
created_at: "2026-07-24T11:00:00Z"
updated_at: "2026-07-24T11:59:59Z"
`)
	writeFixtureFile(t, dir, "state/worktrees/cmd_1.yaml", `schema_version: 1
file_type: state_worktree
command_id: cmd_1
integration:
  command_id: cmd_1
  branch: maestro/integration/cmd_1
  status: merged
workers: []
created_at: "2026-07-24T11:00:00Z"
updated_at: "2026-07-24T11:59:00Z"
`)

	writeFixtureFile(t, dir, "state/learnings.yaml", `schema_version: 1
file_type: state_learnings
learnings:
  - result_id: res_1
    command_id: cmd_1
    content: "use table-driven tests"
    created_at: "2026-07-24T11:30:00Z"
    source_worker: worker1
  - result_id: res_2
    command_id: cmd_1
    content: "verify runs at project root"
    created_at: "2026-07-24T11:45:00Z"
    source_worker: worker1
`)
	writeFixtureFile(t, dir, "state/skill_candidates.yaml", `schema_version: 1
file_type: state_skill_candidates
candidates:
  - id: cand_1
    content: "grep before edit"
    occurrences: 3
    status: pending
    created_at: "2026-07-24T10:00:00Z"
    updated_at: "2026-07-24T11:00:00Z"
  - id: cand_2
    content: "old approach"
    occurrences: 1
    status: rejected
    created_at: "2026-07-24T10:00:00Z"
    updated_at: "2026-07-24T11:00:00Z"
`)

	writeFixtureFile(t, dir, "results/worker1.yaml", `schema_version: 1
file_type: result_task
results:
  - id: res_1
    task_id: task_1
    command_id: cmd_1
    status: completed
    summary: "implemented the collector"
    created_at: "2026-07-24T11:40:00Z"
  - id: res_2
    task_id: task_3
    command_id: cmd_1
    status: failed
    summary: "lint failed"
    created_at: "2026-07-24T11:50:00Z"
`)

	writeFixtureFile(t, dir, "dead_letters/worker1.20260724.yaml", "dead: letter\n")

	return dir
}

func TestCollect_FullFixture(t *testing.T) {
	dir := newFixtureMaestroDir(t)
	s := Collect(dir, fixtureTime)

	// Metrics
	if s.Metrics.Err != "" {
		t.Fatalf("metrics err: %s", s.Metrics.Err)
	}
	if s.Metrics.DaemonHeartbeat != "2026-07-24T12:00:02Z" {
		t.Errorf("heartbeat = %q", s.Metrics.DaemonHeartbeat)
	}
	if s.Metrics.Counters.TasksCompleted != 9 {
		t.Errorf("tasks_completed = %d, want 9", s.Metrics.Counters.TasksCompleted)
	}
	if s.Metrics.Usage == nil {
		t.Fatal("usage section missing")
	}
	if !s.Metrics.Usage.Partial {
		t.Error("usage.partial should be true")
	}
	if got := len(s.Metrics.Usage.BudgetAlerts); got != 1 {
		t.Errorf("budget alerts = %d, want 1", got)
	}

	// Queues
	if s.Queues.Err != "" {
		t.Fatalf("queues err: %s", s.Queues.Err)
	}
	byName := map[string][2]int{}
	for _, q := range s.Queues.Rows {
		byName[q.Name] = [2]int{q.Pending, q.InProgress}
	}
	if byName["worker1"] != [2]int{1, 1} {
		t.Errorf("worker1 queue = %v, want {1 1}", byName["worker1"])
	}
	if byName["planner"] != [2]int{0, 1} {
		t.Errorf("planner queue = %v, want {0 1}", byName["planner"])
	}

	// Commands
	if s.Commands.TotalCommands != 1 || len(s.Commands.Rows) != 1 {
		t.Fatalf("commands = %d rows / %d total, want 1/1", len(s.Commands.Rows), s.Commands.TotalCommands)
	}
	row := s.Commands.Rows[0]
	if row.CommandID != "cmd_1" || row.PlanStatus != "sealed" {
		t.Errorf("row = %+v", row)
	}
	if row.PhasesTotal != 2 || row.PhasesDone != 1 || row.ActivePhase != "implement" {
		t.Errorf("phases = %d/%d active=%q, want 1/2 implement", row.PhasesDone, row.PhasesTotal, row.ActivePhase)
	}
	want := TaskCounts{Total: 4, Completed: 1, Failed: 1, InFlight: 1, Pending: 1}
	if row.Tasks != want {
		t.Errorf("task counts = %+v, want %+v", row.Tasks, want)
	}
	if row.Integration != "merged" {
		t.Errorf("integration = %q, want merged", row.Integration)
	}
	if s.Commands.ActiveCount != 1 {
		t.Errorf("active commands = %d, want 1", s.Commands.ActiveCount)
	}

	// Signals
	if s.Signals.Total != 1 || len(s.Signals.Rows) != 1 {
		t.Fatalf("signals = %d/%d rows", s.Signals.Total, len(s.Signals.Rows))
	}
	sg := s.Signals.Rows[0]
	if sg.Kind != "merge_conflict" || sg.Attempts != 3 || sg.LastError == "" {
		t.Errorf("signal row = %+v", sg)
	}

	// Attention
	if s.Attention.DeadLetterFiles != 1 {
		t.Errorf("dead letters = %d, want 1", s.Attention.DeadLetterFiles)
	}
	if s.Attention.QuarantineFiles != 0 {
		t.Errorf("quarantine = %d, want 0", s.Attention.QuarantineFiles)
	}

	// Learnings: newest first
	if s.Learnings.Total != 2 || len(s.Learnings.Latest) != 2 {
		t.Fatalf("learnings = %d total / %d latest", s.Learnings.Total, len(s.Learnings.Latest))
	}
	if s.Learnings.Latest[0].ResultID != "res_2" {
		t.Errorf("latest learning = %s, want res_2 (newest first)", s.Learnings.Latest[0].ResultID)
	}

	// Skill candidates
	if s.SkillCandidates.Pending != 1 || s.SkillCandidates.Rejected != 1 || s.SkillCandidates.Approved != 0 {
		t.Errorf("skill candidates = %+v", s.SkillCandidates)
	}
	if len(s.SkillCandidates.PendingRows) != 1 || s.SkillCandidates.PendingRows[0].ID != "cand_1" {
		t.Errorf("pending rows = %+v", s.SkillCandidates.PendingRows)
	}

	// Results: newest first
	if len(s.Results.Rows) != 2 {
		t.Fatalf("results = %d rows, want 2", len(s.Results.Rows))
	}
	if s.Results.Rows[0].TaskID != "task_3" || s.Results.Rows[0].Status != "failed" {
		t.Errorf("newest result = %+v, want task_3 failed", s.Results.Rows[0])
	}
	if s.Results.Rows[0].Reporter != "worker1" {
		t.Errorf("reporter = %q", s.Results.Rows[0].Reporter)
	}
}

func TestCollect_EmptyDirIsGraceful(t *testing.T) {
	dir := t.TempDir() // no .maestro content at all
	s := Collect(dir, fixtureTime)

	if s.Metrics.Err == "" {
		t.Error("metrics should be unavailable")
	}
	if s.Queues.Err == "" {
		t.Error("queues should be unavailable")
	}
	if s.Commands.Err == "" {
		t.Error("commands should be unavailable")
	}
	// Lazily created dirs / optional files are healthy-empty, not errors.
	if s.Signals.Err != "" || s.Signals.Total != 0 {
		t.Errorf("signals = %+v, want empty without error", s.Signals)
	}
	if s.Learnings.Err != "" || s.SkillCandidates.Err != "" {
		t.Errorf("learnings/skills should not error on missing files: %q / %q",
			s.Learnings.Err, s.SkillCandidates.Err)
	}
	if s.Attention.DeadLetterFiles != 0 || s.Attention.QuarantineFiles != 0 {
		t.Errorf("attention = %+v, want zeros", s.Attention)
	}
}

func TestCollect_CorruptFilesAreIsolated(t *testing.T) {
	dir := newFixtureMaestroDir(t)
	// Corrupt one section; the rest must stay readable.
	writeFixtureFile(t, dir, "state/metrics.yaml", ":: not yaml ::\n\t")

	s := Collect(dir, fixtureTime)
	if s.Metrics.Err == "" {
		t.Error("metrics should be unavailable after corruption")
	}
	if s.Commands.Err != "" || len(s.Commands.Rows) != 1 {
		t.Errorf("commands must survive metrics corruption: %+v", s.Commands)
	}
	if s.Queues.Err != "" {
		t.Errorf("queues must survive metrics corruption: %s", s.Queues.Err)
	}
}

func TestBucketTasks_RetryHistoryNotDoubleCounted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".maestro")
	// task_9 exists only in task_states (retry history), not in the
	// current required/optional sets — it must not be counted.
	writeFixtureFile(t, dir, "state/commands/cmd_2.yaml", `schema_version: 1
file_type: state_command
command_id: cmd_2
plan_status: completed
required_task_ids: [task_1]
task_states:
  task_1: completed
  task_9: failed
created_at: "2026-07-24T11:00:00Z"
updated_at: "2026-07-24T11:59:59Z"
`)
	s := Collect(dir, fixtureTime)
	if len(s.Commands.Rows) != 1 {
		t.Fatalf("rows = %d", len(s.Commands.Rows))
	}
	got := s.Commands.Rows[0].Tasks
	want := TaskCounts{Total: 1, Completed: 1}
	if got != want {
		t.Errorf("task counts = %+v, want %+v", got, want)
	}
	if s.Commands.ActiveCount != 0 {
		t.Errorf("completed plan must not count as active")
	}
}
