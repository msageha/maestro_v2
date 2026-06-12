package model

import "fmt"

// A/B candidate selection (cross-runtime best-of-2) state model.
// Design: docs/design/ab_candidate_selection.md.
//
// A CandidateGroup races two executions of the same logical task — the
// canonical candidate (registered in phase/required tracking) and a shadow
// candidate of a different runtime. Both candidates ARE registered in
// TaskStates / TaskDependencies (the result_write pipeline requires it);
// only the canonical task is a member of phase.TaskIDs / RequiredTaskIDs /
// OptionalTaskIDs. The group is the SSOT for the non-canonical candidate's
// lifecycle, and the pre-selection barrier (§4.5) consults it.

// ABCandidateBranch returns the conventional candidate-exclusive branch
// name. Defined in model (not worktree) because the plan-side fan-out
// records it in the CandidateGroup at creation time and the daemon-side
// worktree manager must produce the identical name.
func ABCandidateBranch(commandID, taskID string) string {
	return fmt.Sprintf("maestro/%s/candidate/%s", commandID, taskID)
}

// ABGroupStatus is the lifecycle of a candidate group.
type ABGroupStatus string

const (
	// ABGroupRacing — candidates dispatched, not all terminal yet.
	ABGroupRacing ABGroupStatus = "racing"
	// ABGroupSelecting — all candidates terminal; selection pipeline running.
	ABGroupSelecting ABGroupStatus = "selecting"
	// ABGroupResolved — winner chosen and intake completed.
	ABGroupResolved ABGroupStatus = "resolved"
	// ABGroupDegraded — A/B aborted; canonical continues on the normal
	// single-candidate pipeline (walkover, selection failure, intake
	// conflict, timeout...).
	ABGroupDegraded ABGroupStatus = "degraded"
)

// IsUnresolved reports whether the group still holds the pre-selection
// barrier (notify deferral, phase exclusion, review/reward skip).
func (s ABGroupStatus) IsUnresolved() bool {
	return s == ABGroupRacing || s == ABGroupSelecting
}

// ABCandidate describes one candidate execution within a group. Model and
// BloomLevel are persisted at dispatch time so the post-selection bandit
// reward does not depend on in-memory dispatch records (which a daemon
// restart would lose).
type ABCandidate struct {
	TaskID     string `yaml:"task_id"`
	WorkerID   string `yaml:"worker_id"`
	Model      string `yaml:"model"`
	BloomLevel int    `yaml:"bloom_level"`
	// Branch is the candidate-exclusive branch (maestro/{cmd}/candidate/{task}).
	Branch string `yaml:"branch"`
}

// CandidateGroup tracks one A/B race. Keyed in CommandState.CandidateGroups
// by group ID.
type CandidateGroup struct {
	Status          ABGroupStatus `yaml:"status"`
	CanonicalTaskID string        `yaml:"canonical_task_id"`
	// WinnerTaskID is set when Status becomes resolved. For degraded groups
	// it records the surviving canonical task.
	WinnerTaskID string        `yaml:"winner_task_id,omitempty"`
	Candidates   []ABCandidate `yaml:"candidates"`
	// SelectionEvidence records how the race was decided (stage outcomes,
	// degradation reasons). Audit + future learned-router training data.
	SelectionEvidence map[string]string `yaml:"selection_evidence,omitempty"`
	CreatedAt         string            `yaml:"created_at"`
	UpdatedAt         string            `yaml:"updated_at"`
}

// CandidateByTask returns the candidate entry for taskID, or nil.
func (g *CandidateGroup) CandidateByTask(taskID string) *ABCandidate {
	if g == nil {
		return nil
	}
	for i := range g.Candidates {
		if g.Candidates[i].TaskID == taskID {
			return &g.Candidates[i]
		}
	}
	return nil
}

// OtherCandidate returns the candidate that is NOT taskID (two-candidate
// groups), or nil when not found or the group has an unexpected shape.
func (g *CandidateGroup) OtherCandidate(taskID string) *ABCandidate {
	if g == nil {
		return nil
	}
	for i := range g.Candidates {
		if g.Candidates[i].TaskID != taskID {
			return &g.Candidates[i]
		}
	}
	return nil
}

// FindCandidateGroupByTask scans the command state for the group containing
// taskID as a candidate. Returns ("", nil) when the task is not an A/B
// candidate. Linear scan: the number of concurrent groups per command is
// small (bounded by worker pairs).
func (cs *CommandState) FindCandidateGroupByTask(taskID string) (string, *CandidateGroup) {
	if cs == nil || len(cs.CandidateGroups) == 0 {
		return "", nil
	}
	for id, g := range cs.CandidateGroups {
		if g.CandidateByTask(taskID) != nil {
			return id, g
		}
	}
	return "", nil
}

// ABBarrierActive reports whether taskID is an A/B candidate whose group is
// still unresolved — i.e. the pre-selection barrier applies: defer Planner
// notify, exclude from phase progression / merge collection / advisory
// review / bandit rewards. This is the single helper the barrier call sites
// share (design §4.5).
func (cs *CommandState) ABBarrierActive(taskID string) bool {
	_, g := cs.FindCandidateGroupByTask(taskID)
	return g != nil && g.Status.IsUnresolved()
}
