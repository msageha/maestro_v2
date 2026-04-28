// Package model defines the data structures for Maestro's configuration,
// state, and queue entries.
//
// # Naming convention (F-049)
//
// The codebase uses three closely-related but distinct nouns. Reviewers and
// new contributors routinely conflate them, so the boundaries are pinned
// here as the authoritative reference. New types should respect these rules
// or come with an explicit justification.
//
//   - Status — a tagged enum (string under the hood) describing what an
//     entity IS at a single instant. Read by humans and consumed by
//     reconcilers as a discrete value. Examples: Status (queue tasks),
//     PlanStatus, PhaseStatus, ContinuousStatus, ReviewStatus,
//     WorktreeStatus, IntegrationStatus. The valid transitions are encoded
//     by ValidateCommandTaskQueueTransition / similar helpers.
//   - State — a struct aggregating multiple fields that together describe
//     an entity's persisted snapshot. Always serialised to YAML. Examples:
//     CommandState, WorktreeState, IntegrationState, WorktreeCommandState,
//     CircuitBreakerState, CancelState. A State usually contains exactly
//     one Status field plus history/metadata.
//   - Phase — a named segment of the command pipeline (concrete /
//     deferred). Phase is its own concept distinct from Status because
//     a single Phase progresses through several PhaseStatus values during
//     its lifetime. The Phase struct lives in state.go alongside CommandState.
//
// Linter and reviewer guidance:
//
//   - Do NOT introduce a new "FooState string" enum — call it FooStatus.
//   - Do NOT introduce a "FooStatus struct" — call it FooState.
//   - When adding a new pipeline segment that is not a Status enum value,
//     consider whether it is genuinely a new Phase or just another value
//     in PhaseStatus / Status before forking the vocabulary.
package model
