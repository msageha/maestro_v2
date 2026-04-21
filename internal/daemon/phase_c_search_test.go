package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/search"
)

// newPhaseCWithSearch builds a minimal PhaseCManager wired with a SearchTree
// and Sampler so the C-4 integration helpers can be exercised end-to-end.
func newPhaseCWithSearch(maxDepth, maxBranching int) *PhaseCManager {
	return &PhaseCManager{
		SearchTree:    search.NewTree(maxDepth, maxBranching, 0.2),
		SearchSampler: search.NewSampler(1.0, 1.0),
		commandRoots:  make(map[string]bool),
		taskDecisions: make(map[string]search.Decision),
	}
}

func TestPhaseCManager_RecordCommandRoot_Idempotent(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithSearch(3, 4)

	if !m.RecordCommandRoot("cmd-1") {
		t.Fatalf("first call should add root")
	}
	if m.RecordCommandRoot("cmd-1") {
		t.Fatalf("second call should be a no-op")
	}
	if _, ok := m.SearchTree.GetNode("cmd-1"); !ok {
		t.Fatalf("root node cmd-1 not found in tree")
	}
}

func TestPhaseCManager_RecordCommandRoot_NilSafe(t *testing.T) {
	t.Parallel()
	var m *PhaseCManager
	if m.RecordCommandRoot("cmd-1") {
		t.Fatalf("nil receiver must return false")
	}
	m2 := &PhaseCManager{} // no SearchTree configured
	if m2.RecordCommandRoot("cmd-1") {
		t.Fatalf("unconfigured manager must return false")
	}
}

func TestPhaseCManager_RecordTaskExpansion_ExpandsAndSamples(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithSearch(3, 4)
	m.RecordCommandRoot("cmd-1")

	decision, err := m.RecordTaskExpansion("cmd-1", "task-1")
	if err != nil {
		t.Fatalf("unexpected expand error: %v", err)
	}
	if decision != search.DecisionWiden && decision != search.DecisionDeepen {
		t.Fatalf("expected valid decision, got %q", decision)
	}

	// Task node should exist under the command root.
	node, ok := m.SearchTree.GetNode("task-1")
	if !ok {
		t.Fatalf("task-1 node not registered in search tree")
	}
	if node.ParentID != "cmd-1" {
		t.Fatalf("task parent = %q, want cmd-1", node.ParentID)
	}

	// Decision must be stored for later consumption.
	m.searchMu.Lock()
	stored, ok := m.taskDecisions["task-1"]
	m.searchMu.Unlock()
	if !ok {
		t.Fatalf("expected decision to be persisted for task-1")
	}
	if stored != decision {
		t.Fatalf("persisted decision %q != returned %q", stored, decision)
	}
}

func TestPhaseCManager_RecordTaskExpansion_DuplicateTolerated(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithSearch(3, 4)
	m.RecordCommandRoot("cmd-1")

	if _, err := m.RecordTaskExpansion("cmd-1", "task-1"); err != nil {
		t.Fatalf("first expand: %v", err)
	}
	// Second call should not attempt Expand again (node already exists) and
	// should not surface an error.
	if _, err := m.RecordTaskExpansion("cmd-1", "task-1"); err != nil {
		t.Fatalf("duplicate expand should be silent, got %v", err)
	}
}

func TestPhaseCManager_ObserveTaskOutcome_BackpropagatesAndUpdates(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithSearch(3, 4)
	m.RecordCommandRoot("cmd-1")

	// Force a known decision so we can assert sampler update afterwards.
	m.searchMu.Lock()
	m.taskDecisions["task-1"] = search.DecisionWiden
	m.searchMu.Unlock()
	if _, err := m.SearchTree.Expand("cmd-1", []string{"task-1"}); err != nil {
		t.Fatalf("prep expand: %v", err)
	}

	alpha0 := m.SearchSampler.Alpha()
	m.ObserveTaskOutcome("task-1", 1.0, true)

	// Sampler alpha should have incremented after a successful widen decision.
	if m.SearchSampler.Alpha() <= alpha0 {
		t.Fatalf("alpha did not increment: before=%.1f after=%.1f",
			alpha0, m.SearchSampler.Alpha())
	}

	// Tree should have visits recorded on task-1 AND root cmd-1.
	task, _ := m.SearchTree.GetNode("task-1")
	if task.Visits != 1 || task.TotalReward != 1.0 {
		t.Fatalf("task visits/reward = %d/%.1f, want 1/1.0", task.Visits, task.TotalReward)
	}
	cmd, _ := m.SearchTree.GetNode("cmd-1")
	if cmd.Visits != 1 || cmd.TotalReward != 1.0 {
		t.Fatalf("cmd visits/reward = %d/%.1f, want 1/1.0", cmd.Visits, cmd.TotalReward)
	}

	// Decision map should be drained after consumption.
	m.searchMu.Lock()
	_, present := m.taskDecisions["task-1"]
	m.searchMu.Unlock()
	if present {
		t.Fatalf("task decision should be cleared after ObserveTaskOutcome")
	}
}

func TestPhaseCManager_ObserveTaskOutcome_FailureDoesNotUpdateSampler(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithSearch(3, 4)
	m.RecordCommandRoot("cmd-1")
	m.searchMu.Lock()
	m.taskDecisions["task-f"] = search.DecisionDeepen
	m.searchMu.Unlock()
	if _, err := m.SearchTree.Expand("cmd-1", []string{"task-f"}); err != nil {
		t.Fatalf("prep expand: %v", err)
	}

	beta0 := m.SearchSampler.Beta()
	m.ObserveTaskOutcome("task-f", 0.0, false)

	// Standard Thompson Sampling: failures do not update alpha/beta.
	if m.SearchSampler.Beta() != beta0 {
		t.Fatalf("beta changed on failure: before=%.1f after=%.1f",
			beta0, m.SearchSampler.Beta())
	}

	// But tree visits are still incremented (even for reward 0.0).
	task, _ := m.SearchTree.GetNode("task-f")
	if task.Visits != 1 {
		t.Fatalf("visits = %d, want 1", task.Visits)
	}
}

func TestPhaseCManager_ObserveTaskOutcome_UnknownTaskNoOp(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithSearch(3, 4)
	// No Expand — just verify that an unknown taskID does not panic or
	// corrupt sampler state.
	alpha0, beta0 := m.SearchSampler.Alpha(), m.SearchSampler.Beta()
	m.ObserveTaskOutcome("ghost", 1.0, true)
	if m.SearchSampler.Alpha() != alpha0 || m.SearchSampler.Beta() != beta0 {
		t.Fatalf("sampler must not change when no decision exists")
	}
}
