// Package daemon implements the long-running maestro background process that
// arbitrates queue/state/result file mutations on behalf of the CLI.
//
// # Lock ordering
//
// All in-process locks are acquired through lock.MutexMap. To prevent
// deadlocks every code path MUST acquire locks in the canonical order
// documented here. The lock package's debug order checker (build tag
// "lockorder") enforces the coarse three-level scheme; the additional
// sub-ordering inside the state:* namespace is enforced by code review and
// the comments at each call site.
//
// Canonical lock order (acquire low → high; release in reverse):
//
//	level 1   queue:{worker}              — internal/daemon/queue_*.go
//	level 2a  state:{commandID}           — per-command CommandState file
//	level 2b  state:learnings             — global state/learnings.yaml
//	level 2c  state:skill_candidates      — global state/skill_candidates.yaml
//	level 2d  state:continuous            — global state/continuous.yaml
//	level 3   result:{worker}             — per-worker results file
//
// The four state:* sub-locks (2a–2d) are siblings under the same coarse
// level-2 bucket, but the order shown above is the only permitted nesting
// order. Concretely this means:
//
//   - state:{commandID} may be acquired without holding any other state:*
//     lock. While it is held, none of state:learnings, state:skill_candidates
//     or state:continuous may be acquired (the H5 audit finding).
//   - state:learnings, state:skill_candidates and state:continuous are
//     "leaf" locks: they MUST be acquired in isolation (no other state:*
//     lock held). Today every call site obeys this — the writers in
//     result_write_handler.go acquire each leaf lock only after Phase B has
//     released state:{commandID}, and skill_handler.go / continuous_handler.go
//     never reach for state:{commandID} at all.
//   - When two leaf locks must be taken together (no current call site does
//     this), they MUST be acquired in the alphabetical order shown above:
//     learnings → skill_candidates → continuous.
//
// Rationale: collapsing every state:* key into a single coarse level keeps
// the runtime checker simple, but it cannot detect a future regression where
// one goroutine acquires state:{commandID} → state:learnings while another
// acquires state:learnings → state:{commandID}. Documenting the rule here,
// together with the explicit "// lock order: see doc.go" comments at each
// state-namespace acquisition site, gives reviewers a single source of truth
// to point at.
//
// New code that introduces another state:* sub-namespace MUST update this
// comment, assign the new key a position in the ordering, and add a
// reference comment at every acquisition site.
package daemon
