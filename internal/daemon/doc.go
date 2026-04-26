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
// All queue:* keys are level-1 peers. Within this level a fixed sub-ordering
// MUST be followed whenever multiple queue locks are accessed in the same
// code path (even if acquired and released sequentially):
//
//	queue:planner → queue:{worker1} → … → queue:{workerN} → queue:orchestrator → queue:planner_signals
//
// Worker queues are ordered lexicographically by worker ID when more than one
// must be accessed.
//
// Sub-ordering enforcement points:
//
//   - FlushQueues (queue_store.go): iterates queues in the canonical order
//     above. Each queue:* key is acquired and released in its own
//     closure+defer block, so they are never held simultaneously.
//   - PeriodicScan Phase A (queue_scan_collect.go): reads all queues under
//     scanMu.Lock; individual queue locks are not held during collection.
//   - PeriodicScan Phase C (queue_scan_apply.go): writes back dirty queues;
//     acquires queue locks in the canonical order.
//   - Write handlers (queue_write_command.go, queue_write_task.go, etc.):
//     acquire a single queue lock per handler invocation. Concurrent handlers
//     respect the sub-ordering via scanMu.RLock + per-target lockMap key.
//
// queue:planner_signals (added in Phase 1) is positioned last in the sub-
// ordering. Signal delivery (queue_write_signal.go) acquires only this lock,
// and Phase A holds scanMu.Lock (not individual queue locks), so no deadlock
// with the planner_signals position is possible.
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
//     result_write_*.go acquire each leaf lock only after Phase B has
//     released state:{commandID}, and daemonapi.Skill / continuous_handler.go
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
//
// # Signal deduplication key ordering
//
// PlannerSignal deduplication uses a composite key (signalKey) with fields
// evaluated in the following order for equality:
//
//	CommandID → PhaseID → Kind → WorkerID → ConflictGeneration
//
// Two signals are considered duplicates if and only if all five fields match.
// WorkerID scopes per-worker signals (commit_failed, merge_conflict, etc.)
// so that distinct workers retain separate entries. Phase-level signals leave
// WorkerID empty. ConflictGeneration ensures a re-detected merge conflict
// against a different integration HEAD registers as a fresh signal rather than
// being suppressed by a stale entry.
//
// See signalKey and signalDedupKey in queue_dispatch.go for the implementation.
package daemon
