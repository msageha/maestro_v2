package hud

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/status"
	maestroyaml "github.com/msageha/maestro_v2/internal/yaml"
)

// Display caps. The HUD is a fixed-height dashboard, not a pager: each
// section shows the most relevant head and states the full count.
const (
	maxCommandRows        = 10
	maxSignalRows         = 5
	maxLearningRows       = 3
	maxSkillCandidateRows = 5
	maxResultRows         = 8
)

// Collect reads every observable section of maestroDir. It never returns an
// error: per-section failures are recorded in the section's Err field so a
// partially readable (or entirely absent) .maestro/ still renders.
func Collect(maestroDir string, now time.Time) *Snapshot {
	s := &Snapshot{CollectedAt: now}
	s.Metrics = collectMetrics(maestroDir)
	s.Queues = collectQueues(maestroDir)
	s.Commands = collectCommands(maestroDir)
	s.Signals = collectSignals(maestroDir)
	s.Attention = collectAttention(maestroDir)
	s.Learnings = collectLearnings(maestroDir)
	s.SkillCandidates = collectSkillCandidates(maestroDir)
	s.Results = collectResults(maestroDir)
	return s
}

// unavailable renders a read failure as a short operator-facing reason.
func unavailable(err error) string {
	if os.IsNotExist(err) {
		return "unavailable (not found)"
	}
	return fmt.Sprintf("unavailable (%v)", err)
}

// readYAML loads a YAML file with the same size guard and billion-laughs
// protection as the other read-only observers (see internal/status).
func readYAML(path string, out any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", filepath.Base(path))
	}
	if info.Size() > int64(model.DefaultMaxYAMLFileBytes) {
		return fmt.Errorf("file too large (%d bytes)", info.Size())
	}
	data, err := os.ReadFile(path) //nolint:gosec // controlled .maestro/ path
	if err != nil {
		return err
	}
	return maestroyaml.SafeUnmarshal(data, out)
}

func collectMetrics(maestroDir string) MetricsSection {
	var m model.Metrics
	if err := readYAML(filepath.Join(maestroDir, "state", "metrics.yaml"), &m); err != nil {
		return MetricsSection{Err: unavailable(err)}
	}
	out := MetricsSection{Counters: m.Counters, Usage: m.Usage}
	if m.DaemonHeartbeat != nil {
		out.DaemonHeartbeat = *m.DaemonHeartbeat
	}
	if m.UpdatedAt != nil {
		out.UpdatedAt = *m.UpdatedAt
	}
	return out
}

func collectQueues(maestroDir string) QueuesSection {
	rows := status.CollectQueueCounts(maestroDir)
	if rows == nil {
		return QueuesSection{Err: "unavailable (queue/ not readable)"}
	}
	return QueuesSection{Rows: rows}
}

func collectCommands(maestroDir string) CommandsSection {
	dir := filepath.Join(maestroDir, "state", "commands")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return CommandsSection{Err: unavailable(err)}
	}

	var out CommandsSection
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		var cs model.CommandState
		if err := readYAML(filepath.Join(dir, e.Name()), &cs); err != nil {
			continue
		}
		out.TotalCommands++
		row := commandRow(&cs)
		row.Integration = integrationStatus(maestroDir, cs.CommandID)
		if !row.terminal {
			out.ActiveCount++
		}
		out.Rows = append(out.Rows, row)
	}

	// Active commands first, then most recently updated.
	sort.Slice(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		if a.terminal != b.terminal {
			return !a.terminal
		}
		return a.UpdatedAt > b.UpdatedAt
	})
	if len(out.Rows) > maxCommandRows {
		out.Rows = out.Rows[:maxCommandRows]
	}
	return out
}

func commandRow(cs *model.CommandState) CommandRow {
	row := CommandRow{
		CommandID:  cs.CommandID,
		PlanStatus: string(cs.PlanStatus),
		UpdatedAt:  cs.UpdatedAt,
		terminal:   model.IsPlanTerminal(cs.PlanStatus),
		Tasks:      bucketTasks(cs),
	}
	row.PhasesTotal = len(cs.Phases)
	for i := range cs.Phases {
		p := &cs.Phases[i]
		if p.Status == model.PhaseStatusCompleted {
			row.PhasesDone++
		}
		if row.ActivePhase == "" {
			switch p.Status {
			case model.PhaseStatusActive, model.PhaseStatusFilling, model.PhaseStatusAwaitingFill:
				row.ActivePhase = p.Name
			}
		}
	}
	return row
}

// bucketTasks folds task_states into display buckets. Only current tasks
// (required + optional) are counted, mirroring the dashboard rule that
// keeps retry-history entries out of the totals.
func bucketTasks(cs *model.CommandState) TaskCounts {
	current := make(map[string]struct{}, len(cs.RequiredTaskIDs)+len(cs.OptionalTaskIDs))
	for _, id := range cs.RequiredTaskIDs {
		current[id] = struct{}{}
	}
	for _, id := range cs.OptionalTaskIDs {
		current[id] = struct{}{}
	}

	var c TaskCounts
	c.Total = len(current)
	for id := range current {
		st, ok := cs.TaskStates[id]
		if !ok {
			c.Pending++
			continue
		}
		switch st {
		case model.StatusCompleted:
			c.Completed++
		case model.StatusFailed, model.StatusDeadLetter, model.StatusAborted:
			c.Failed++
		case model.StatusInProgress, model.StatusDispatched, model.StatusRunning,
			model.StatusVerifyPending, model.StatusRepairPending:
			c.InFlight++
		case model.StatusPending, model.StatusPlanned, model.StatusReady:
			c.Pending++
		case model.StatusPausedForReplan, model.StatusPausedForHuman:
			c.Paused++
		case model.StatusCancelled:
			c.Cancelled++
		}
	}
	return c
}

func integrationStatus(maestroDir, commandID string) string {
	if commandID == "" {
		return ""
	}
	var ws model.WorktreeCommandState
	path := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	if err := readYAML(path, &ws); err != nil {
		return ""
	}
	return string(ws.Integration.Status)
}

func collectSignals(maestroDir string) SignalsSection {
	path := filepath.Join(maestroDir, "queue", "planner_signals.yaml")
	var sq model.PlannerSignalQueue
	if err := readYAML(path, &sq); err != nil {
		if os.IsNotExist(err) {
			return SignalsSection{} // no signal queue yet: healthy, not an error
		}
		return SignalsSection{Err: unavailable(err)}
	}
	out := SignalsSection{Total: len(sq.Signals)}
	rows := make([]SignalRow, 0, len(sq.Signals))
	for i := range sq.Signals {
		sg := &sq.Signals[i]
		row := SignalRow{
			Kind:      sg.Kind,
			CommandID: sg.CommandID,
			PhaseID:   sg.PhaseID,
			WorkerID:  sg.WorkerID,
			Attempts:  sg.Attempts,
		}
		if sg.LastError != nil {
			row.LastError = *sg.LastError
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Attempts > rows[j].Attempts })
	if len(rows) > maxSignalRows {
		rows = rows[:maxSignalRows]
	}
	out.Rows = rows
	return out
}

func collectAttention(maestroDir string) AttentionSection {
	var out AttentionSection
	out.DeadLetterFiles = countDirEntries(filepath.Join(maestroDir, "dead_letters"))
	out.QuarantineFiles = countDirEntries(filepath.Join(maestroDir, "quarantine"))
	return out
}

// countDirEntries returns the number of regular files in dir; a missing dir
// counts as zero because both dead_letters/ and quarantine/ are created
// lazily on first use.
func countDirEntries(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

func collectLearnings(maestroDir string) LearningsSection {
	path := filepath.Join(maestroDir, "state", "learnings.yaml")
	var lf model.LearningsFile
	if err := readYAML(path, &lf); err != nil {
		if os.IsNotExist(err) {
			return LearningsSection{}
		}
		return LearningsSection{Err: unavailable(err)}
	}
	out := LearningsSection{Total: len(lf.Learnings)}
	// Entries are appended chronologically; show the newest first.
	for i := len(lf.Learnings) - 1; i >= 0 && len(out.Latest) < maxLearningRows; i-- {
		out.Latest = append(out.Latest, lf.Learnings[i])
	}
	return out
}

func collectSkillCandidates(maestroDir string) SkillCandidatesSection {
	path := filepath.Join(maestroDir, "state", "skill_candidates.yaml")
	var sf model.SkillCandidatesFile
	if err := readYAML(path, &sf); err != nil {
		if os.IsNotExist(err) {
			return SkillCandidatesSection{}
		}
		return SkillCandidatesSection{Err: unavailable(err)}
	}
	var out SkillCandidatesSection
	for i := range sf.Candidates {
		c := sf.Candidates[i]
		switch c.Status {
		case "approved":
			out.Approved++
		case "rejected":
			out.Rejected++
		default:
			out.Pending++
			out.PendingRows = append(out.PendingRows, c)
		}
	}
	sort.Slice(out.PendingRows, func(i, j int) bool {
		return out.PendingRows[i].Occurrences > out.PendingRows[j].Occurrences
	})
	if len(out.PendingRows) > maxSkillCandidateRows {
		out.PendingRows = out.PendingRows[:maxSkillCandidateRows]
	}
	return out
}

// genericResultFile decodes the fields shared by result_task and
// result_command files, so one reader covers worker*.yaml and planner.yaml.
type genericResultFile struct {
	Results []struct {
		ID        string `yaml:"id"`
		TaskID    string `yaml:"task_id"`
		CommandID string `yaml:"command_id"`
		Status    string `yaml:"status"`
		Summary   string `yaml:"summary"`
		CreatedAt string `yaml:"created_at"`
	} `yaml:"results"`
}

func collectResults(maestroDir string) ResultsSection {
	dir := filepath.Join(maestroDir, "results")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ResultsSection{Err: unavailable(err)}
	}
	var rows []ResultRow
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		var rf genericResultFile
		if err := readYAML(filepath.Join(dir, e.Name()), &rf); err != nil {
			continue
		}
		reporter := strings.TrimSuffix(e.Name(), ".yaml")
		for _, r := range rf.Results {
			rows = append(rows, ResultRow{
				Reporter:  reporter,
				TaskID:    r.TaskID,
				CommandID: r.CommandID,
				Status:    r.Status,
				Summary:   r.Summary,
				CreatedAt: r.CreatedAt,
			})
		}
	}
	// RFC3339 timestamps sort lexicographically; newest first.
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt > rows[j].CreatedAt })
	if len(rows) > maxResultRows {
		rows = rows[:maxResultRows]
	}
	return ResultsSection{Rows: rows}
}
