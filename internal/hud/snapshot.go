package hud

import (
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/status"
)

// Snapshot is one poll of the observable `.maestro/` state. Every section
// is collected independently (hermes-hud style graceful degradation): a
// section that could not be read carries a non-empty Err and renders as
// "unavailable" without affecting its siblings.
type Snapshot struct {
	CollectedAt time.Time

	Metrics         MetricsSection
	Queues          QueuesSection
	Commands        CommandsSection
	Signals         SignalsSection
	Attention       AttentionSection
	Learnings       LearningsSection
	SkillCandidates SkillCandidatesSection
	Results         ResultsSection
}

// MetricsSection mirrors state/metrics.yaml: daemon heartbeat, cumulative
// counters, and the opt-in per-agent / per-command usage (token + cost)
// block.
type MetricsSection struct {
	// DaemonHeartbeat is the raw RFC3339 heartbeat written by the daemon,
	// empty when unknown. Liveness is derived at render time from its age.
	DaemonHeartbeat string
	UpdatedAt       string
	Counters        model.MetricsCounters
	// Usage is nil when cost tracking is disabled or has never collected.
	Usage *model.UsageMetrics
	Err   string
}

// QueuesSection lists per-queue pending/in_progress depths, shared with
// `maestro status` via status.CollectQueueCounts.
type QueuesSection struct {
	Rows []status.QueueCount
	Err  string
}

// TaskCounts buckets a command's task_states the same way the dashboard
// does (extended lifecycle states folded into operator-facing buckets).
type TaskCounts struct {
	Total     int
	Completed int
	Failed    int // failed + dead_letter + aborted
	InFlight  int // in_progress / dispatched / running / verify_pending / repair_pending
	Pending   int // pending / planned / ready
	Paused    int // paused_for_replan / paused_for_human
	Cancelled int
}

// CommandRow is the per-command board line derived from
// state/commands/{id}.yaml joined with state/worktrees/{id}.yaml.
type CommandRow struct {
	CommandID   string
	PlanStatus  string
	PhasesTotal int
	PhasesDone  int
	// ActivePhase is the name of the first non-terminal phase
	// (active/filling/awaiting_fill), empty when none.
	ActivePhase string
	Tasks       TaskCounts
	// Integration is the integration branch status from the worktree state
	// file, empty when that file is missing.
	Integration string
	UpdatedAt   string
	terminal    bool // plan status terminal; used for sort only
}

// CommandsSection is the task/phase board. TotalCommands counts every state
// file even when Rows is capped for display.
type CommandsSection struct {
	Rows          []CommandRow
	TotalCommands int
	ActiveCount   int
	Err           string
}

// SignalRow is one pending planner signal (operator-attention queue).
type SignalRow struct {
	Kind      string
	CommandID string
	PhaseID   string
	WorkerID  string
	Attempts  int
	LastError string
}

// SignalsSection summarises queue/planner_signals.yaml.
type SignalsSection struct {
	Total int
	Rows  []SignalRow
	Err   string
}

// AttentionSection counts dead-lettered and quarantined files. Missing
// directories count as zero (both are created lazily).
type AttentionSection struct {
	DeadLetterFiles int
	QuarantineFiles int
	Err             string
}

// LearningsSection shows the C-5 self-improvement trail (display only —
// detection lives in the daemon).
type LearningsSection struct {
	Total  int
	Latest []model.Learning // most recent first, capped
	Err    string
}

// SkillCandidatesSection shows skill-factory candidate signals
// (display only).
type SkillCandidatesSection struct {
	Pending  int
	Approved int
	Rejected int
	// PendingRows lists pending candidates, highest occurrences first,
	// capped.
	PendingRows []model.SkillCandidate
	Err         string
}

// ResultRow is one recent result entry from results/*.yaml. Reporter is the
// queue file base name (worker1, planner, ...).
type ResultRow struct {
	Reporter  string
	TaskID    string
	CommandID string
	Status    string
	Summary   string
	CreatedAt string
}

// ResultsSection lists the most recent results across all reporters.
type ResultsSection struct {
	Rows []ResultRow
	Err  string
}
