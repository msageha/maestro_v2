package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/search"
)

// RecordCommandRoot ensures the given commandID has a root node in the
// MCTS search tree. Idempotent — safe to call every dispatch cycle. Safe
// on nil receiver or when SearchTree is unconfigured (no-op).
//
// Returns true when a new root was added (caller may emit an observability
// log in that case), false otherwise.
func (m *PhaseCManager) RecordCommandRoot(commandID string) bool {
	if m == nil || m.SearchTree == nil || commandID == "" {
		return false
	}
	m.searchMu.Lock()
	defer m.searchMu.Unlock()
	if m.commandRoots[commandID] {
		return false
	}
	if _, exists := m.SearchTree.GetNode(commandID); exists {
		m.commandRoots[commandID] = true
		return false
	}
	m.SearchTree.AddRoot(commandID)
	m.commandRoots[commandID] = true
	return true
}

// RecordTaskExpansion links a task node under its command root and samples
// a widen/deepen decision for the dispatch. Returns the sampled decision so
// callers can log it; returns empty Decision when SearchSampler/SearchTree
// are unavailable (no-op). Expansion failures (e.g., maxBranching exceeded)
// are tolerated — the decision is still sampled so the sampler observes
// balanced exploration signals.
func (m *PhaseCManager) RecordTaskExpansion(commandID, taskID string) (search.Decision, error) {
	if m == nil || taskID == "" {
		return "", nil
	}
	var decision search.Decision
	if m.SearchSampler != nil {
		decision = m.SearchSampler.Sample()
	}

	var expandErr error
	if m.SearchTree != nil && commandID != "" {
		// Expand may fail if the task is already expanded under the command
		// (duplicate) or if branching/depth limits are exceeded. Neither is
		// fatal — the sampler decision is still informative for observability.
		if _, exists := m.SearchTree.GetNode(taskID); !exists {
			if _, err := m.SearchTree.Expand(commandID, []string{taskID}); err != nil {
				expandErr = err
			}
		}
	}

	if decision != "" {
		m.searchMu.Lock()
		m.taskDecisions[taskID] = decision
		m.searchMu.Unlock()
	}
	return decision, expandErr
}

// ObserveTaskOutcome backpropagates a reward up the tree from the task
// node and feeds the Thompson Sampler the matching widen/deepen reward.
// The consumed decision is removed from the internal map so memory stays
// bounded. Safe on nil receiver.
//
// reward is the numeric signal for MCTS backpropagation (0.0 for failure,
// 1.0 for success); success is the boolean used to reward the sampler's
// original decision (success==true increments the decision's alpha/beta).
func (m *PhaseCManager) ObserveTaskOutcome(taskID string, reward float64, success bool) {
	if m == nil || taskID == "" {
		return
	}
	if m.SearchTree != nil {
		if _, exists := m.SearchTree.GetNode(taskID); exists {
			m.SearchTree.Backpropagate(taskID, reward)
		}
	}

	m.searchMu.Lock()
	decision, ok := m.taskDecisions[taskID]
	if ok {
		delete(m.taskDecisions, taskID)
	}
	m.searchMu.Unlock()

	if ok && m.SearchSampler != nil && decision != "" {
		m.SearchSampler.Update(decision, success)
	}
}
