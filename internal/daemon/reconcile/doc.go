// Package reconcile implements the periodic inconsistency detection and repair
// engine for the maestro daemon. It scans queue, state, and result files to
// detect mismatches caused by crashes, partial writes, or timing races, and
// applies deterministic repairs to restore consistency.
//
// # Architecture
//
// The engine uses the Strategy pattern: each reconciliation rule is a [Pattern]
// implementation with an Apply method that receives a per-scan [Run] context.
// The [Engine] orchestrates all patterns in sequence and collects [Repair]
// actions and [DeferredNotification] items for post-scan execution.
//
// # Reconciliation patterns
//
//   - R0 (Dispatch): Detects commands stuck in dispatch phase with no state
//     file created beyond the lease timeout. Releases the lease for retry.
//   - R0b (FillingStuck): Detects deferred phases stuck in awaiting_fill
//     state beyond the configured threshold.
//   - R1 (ResultQueue): Detects terminal results with in_progress queue
//     entries. Updates queue to terminal and clears the lease. Also
//     re-enqueues orphaned retry tasks from RetryEnqueueFailed entries.
//   - R2 (ResultState): Detects terminal worker results with non-terminal
//     task state entries. Applies the result to update task_states and
//     applied_result_ids.
//   - R3 (PlannerQueue): Detects terminal planner results with in_progress
//     planner queue entries. Updates the planner queue to terminal.
//   - R4 (PlanStatus): Detects terminal planner results with non-terminal
//     plan_status. Re-evaluates via plan.CanComplete and updates plan_status
//     or quarantines the result.
//   - R5 (Notification): Detects terminal planner results that have been
//     notified but lack an orchestrator notification entry. Re-issues the
//     notification.
//   - R6 (FillTimeout): Detects deferred phases past their fill deadline.
//     Sets the phase to timed_out, cascade-cancels downstream pending phases,
//     and defers a Planner notification.
//
// # Supporting types
//
//   - Deps: Long-lived dependencies shared by all patterns (directory paths,
//     config, lock map, logger, clock, result handler).
//   - Run: Per-scan context with directory caching and helper methods for
//     loading state and result files.
package reconcile
