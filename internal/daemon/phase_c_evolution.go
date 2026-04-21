package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/evolution"
)

// RecordTaskCompletionNovelty hashes the summary of a successfully completed
// task and checks it against prior summaries for the same command via
// EvolutionEngine.CheckNovelty. The hash is then registered as "seen" (up to
// evolutionNoveltyMaxPerCmd entries per command) so future identical outputs
// are flagged as non-novel. Safe on nil receiver or when EvolutionEngine is
// unconfigured (returns false, false — "not novel, not evaluated").
//
// Returns (isNovel, evaluated):
//   - evaluated == false  → no evolution engine; caller should skip logging.
//   - evaluated == true, isNovel == true  → first time this summary seen.
//   - evaluated == true, isNovel == false → duplicate of a previous completion.
func (m *PhaseCManager) RecordTaskCompletionNovelty(commandID, summary string) (isNovel, evaluated bool) {
	if m == nil || m.EvolutionEngine == nil || commandID == "" || summary == "" {
		return false, false
	}

	hash := evolution.HashContent(summary)

	m.evolutionMu.Lock()
	defer m.evolutionMu.Unlock()

	existing := m.commandNovelty[commandID]
	existingHashes := make([]string, 0, len(existing))
	for h := range existing {
		existingHashes = append(existingHashes, h)
	}
	novel := m.EvolutionEngine.CheckNovelty(hash, existingHashes)

	// Register the new hash so subsequent identical outputs are non-novel.
	// Respect the per-command cap to keep memory bounded.
	if existing == nil {
		existing = make(map[string]struct{})
		m.commandNovelty[commandID] = existing
	}
	if len(existing) < evolutionNoveltyMaxPerCmd {
		existing[hash] = struct{}{}
	}
	return novel, true
}

// PlanRetryMutations records a failure for the command and, once the per-
// command failure count crosses evolutionFailureRetryThreshold, invokes
// EvolutionEngine.PlanMutations to produce a mutation slot plan. parentCount
// is the number of candidate parents available for crossover (typically the
// attempt count or the number of previously completed siblings).
//
// Returns the planned slots and `planned=true` when the threshold is met;
// otherwise returns nil/false. Safe on nil receiver.
func (m *PhaseCManager) PlanRetryMutations(commandID string, parentCount int) (slots []evolution.MutationSlot, planned bool) {
	if m == nil || m.EvolutionEngine == nil || commandID == "" {
		return nil, false
	}

	m.evolutionMu.Lock()
	m.commandFailures[commandID]++
	failures := m.commandFailures[commandID]
	m.evolutionMu.Unlock()

	if failures < evolutionFailureRetryThreshold {
		return nil, false
	}

	return m.EvolutionEngine.PlanMutations(parentCount), true
}

// ResetEvolutionState clears all tracked novelty hashes and failure counts
// for the given command. Called when the command reaches a terminal state
// so memory does not grow without bound across long-lived daemons. Safe on
// nil receiver.
func (m *PhaseCManager) ResetEvolutionState(commandID string) {
	if m == nil || commandID == "" {
		return
	}
	m.evolutionMu.Lock()
	defer m.evolutionMu.Unlock()
	delete(m.commandNovelty, commandID)
	delete(m.commandFailures, commandID)
}
