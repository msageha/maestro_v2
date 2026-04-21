package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/complexity"
	"github.com/msageha/maestro_v2/internal/daemon/featuregate"
	"github.com/msageha/maestro_v2/internal/model"
)

// ComplexityInputFromTask derives a complexity.Input from a single task.
// Used when dispatching individual tasks where the full sibling-task list
// is not available (the per-command classification is cached separately).
func ComplexityInputFromTask(task model.Task) complexity.Input {
	repair := 0.0
	if task.Attempts > 1 {
		repair = float64(task.Attempts-1) * 0.2
		if repair > 1 {
			repair = 1
		}
	}
	return complexity.Input{
		FileCount:         len(task.ExpectedPaths),
		DependencyDepth:   len(task.BlockedBy),
		BloomLevel:        task.BloomLevel,
		PastRepairRate:    repair,
		ExpectedPathCount: len(task.ExpectedPaths),
	}
}

// EvaluateLevel computes the complexity profile level for the given input.
// Order of precedence:
//  1. ComplexityScorer available → use structured Score
//  2. FeatureEvaluator available → file-count heuristic via GetLevel
//  3. Fallback → LevelSimple (conservative default)
//
// Safe to call on a nil receiver (returns LevelSimple).
func (m *PhaseCManager) EvaluateLevel(input complexity.Input) featuregate.ProfileLevel {
	if m == nil {
		return featuregate.LevelSimple
	}
	if m.ComplexityScorer != nil {
		score := m.ComplexityScorer.Estimate(input)
		return complexityLevelToProfile(score.Level)
	}
	if m.FeatureEvaluator != nil {
		return m.FeatureEvaluator.GetLevel(input.FileCount, input.DependencyDepth)
	}
	return featuregate.LevelSimple
}

// IsFeatureEnabled returns true only when both the evaluator exists and
// the feature is enabled at the given level. Safe to call on nil receiver.
func (m *PhaseCManager) IsFeatureEnabled(level featuregate.ProfileLevel, feat featuregate.Feature) bool {
	if m == nil {
		return false
	}
	if m.FeatureEvaluator == nil {
		return false
	}
	return m.FeatureEvaluator.IsEnabled(level, feat)
}

// complexityLevelToProfile translates the complexity package's Level to
// the featuregate package's ProfileLevel. The string values are identical
// by convention (both packages document this alignment), but the types
// are distinct.
func complexityLevelToProfile(l complexity.Level) featuregate.ProfileLevel {
	switch l {
	case complexity.LevelSimple:
		return featuregate.LevelSimple
	case complexity.LevelStandard:
		return featuregate.LevelStandard
	case complexity.LevelComplex:
		return featuregate.LevelComplex
	case complexity.LevelCritical:
		return featuregate.LevelCritical
	default:
		return featuregate.LevelSimple
	}
}

// ComplexityInputFromCommand builds a complexity.Input from a Command and
// its associated tasks. Used at dispatch time to classify the level per
// command. The BloomLevel is the max across tasks, PastRepairRate reflects
// prior attempt counts, and ExpectedPathCount counts declared file targets.
func ComplexityInputFromCommand(cmd model.Command, tasks []model.Task) complexity.Input {
	input := complexity.Input{
		FileCount:         countDistinctPaths(tasks),
		DependencyDepth:   maxDepFanIn(tasks),
		BloomLevel:        maxBloom(tasks),
		PastRepairRate:    pastRepairRate(tasks),
		ExpectedPathCount: countExpectedPaths(tasks),
	}
	if cmd.Attempts > 1 {
		// Higher attempts → higher repair signal; scale monotonically.
		repair := float64(cmd.Attempts-1) * 0.2
		if repair > 1 {
			repair = 1
		}
		if repair > input.PastRepairRate {
			input.PastRepairRate = repair
		}
	}
	return input
}

func countDistinctPaths(tasks []model.Task) int {
	seen := make(map[string]struct{})
	for _, t := range tasks {
		for _, p := range t.ExpectedPaths {
			seen[p] = struct{}{}
		}
	}
	return len(seen)
}

func countExpectedPaths(tasks []model.Task) int {
	n := 0
	for _, t := range tasks {
		n += len(t.ExpectedPaths)
	}
	return n
}

func maxBloom(tasks []model.Task) int {
	m := 0
	for _, t := range tasks {
		if t.BloomLevel > m {
			m = t.BloomLevel
		}
	}
	return m
}

func maxDepFanIn(tasks []model.Task) int {
	m := 0
	for _, t := range tasks {
		n := len(t.BlockedBy)
		if n > m {
			m = n
		}
	}
	return m
}

// classifyAndLogCommand computes the complexity level for a command at
// dispatch time and emits a structured log entry. Safe when phaseC is nil.
//
// The classification uses only command-level signals (attempts), since the
// full task list is not yet known at command dispatch (command → planner).
// The planner produces tasks; per-task classification happens separately in
// classifyAndLogTask.
func (qh *QueueHandler) classifyAndLogCommand(cmd *model.Command) {
	if qh == nil || qh.phaseC == nil || cmd == nil {
		return
	}
	input := ComplexityInputFromCommand(*cmd, nil)
	level := qh.phaseC.EvaluateLevel(input)
	qh.log(LogLevelInfo, "phase_c_command_classified id=%s level=%s attempts=%d",
		cmd.ID, level, cmd.Attempts)

	// C-4 search tree: ensure the command has a root so subsequent task
	// dispatches can Expand under it. Nil-safe.
	if qh.phaseC.RecordCommandRoot(cmd.ID) {
		qh.log(LogLevelDebug, "search_tree_root_added command=%s", cmd.ID)
	}
}

// classifyAndLogTask computes the complexity level for a task at dispatch
// time and emits a structured log entry. Safe when phaseC is nil.
//
// The evaluated level feeds two downstream consumers (both nil-safe):
//  1. FeatureEvaluator — gates optional features (learnings injection,
//     cross-agent review) per profile.
//  2. BanditSelector — when adaptive model selection is enabled for the
//     level, the worker model override is recorded here for observability.
func (qh *QueueHandler) classifyAndLogTask(task *model.Task, workerID string) {
	if qh == nil || qh.phaseC == nil || task == nil {
		return
	}
	input := ComplexityInputFromTask(*task)
	level := qh.phaseC.EvaluateLevel(input)
	qh.log(LogLevelInfo, "phase_c_task_classified id=%s worker=%s level=%s bloom=%d attempts=%d",
		task.ID, workerID, level, task.BloomLevel, task.Attempts)

	// C-4 search tree / Thompson sampler: lazily add the command root (in case
	// a task is dispatched without its command entering classifyAndLogCommand
	// first — e.g., on retry), expand the task under it, and sample a
	// widen/deepen decision that will be rewarded on task completion.
	qh.phaseC.RecordCommandRoot(task.CommandID)
	decision, expandErr := qh.phaseC.RecordTaskExpansion(task.CommandID, task.ID)
	if expandErr != nil {
		qh.log(LogLevelDebug, "search_tree_expand_skipped command=%s task=%s error=%v",
			task.CommandID, task.ID, expandErr)
	}
	if decision != "" {
		qh.log(LogLevelInfo, "search_sampler_decision task=%s command=%s decision=%s",
			task.ID, task.CommandID, decision)
	}
}

func pastRepairRate(tasks []model.Task) float64 {
	if len(tasks) == 0 {
		return 0
	}
	var attempts float64
	for _, t := range tasks {
		if t.Attempts > 1 {
			attempts += float64(t.Attempts - 1)
		}
	}
	rate := attempts / float64(len(tasks)*5)
	if rate > 1 {
		rate = 1
	}
	return rate
}
