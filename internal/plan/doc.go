// Package plan handles task plan submission, validation, state management, and
// command lifecycle operations for the maestro orchestration system.
//
// Plans define the set of tasks (and optional phases) that compose a command.
// The package validates inputs, assigns tasks to workers, persists state to
// YAML files, and tracks task completion through to command-level terminal
// status.
//
// # Main components
//
//   - Submit: Validates and persists a plan, assigning tasks to workers via
//     round-robin and writing queue entries. Supports both flat task lists and
//     multi-phase plans with DAG-based ordering.
//   - StateManager: Manages read/write access to command state files
//     (state/commands/{id}.yaml) with file-level locking, atomic writes, and
//     automatic backup recovery for corrupted YAML.
//   - stateStore: Interface abstracting state persistence operations, allowing
//     the submit flow to be decoupled from direct filesystem I/O.
//   - PlanStateReader: Adapter that reads and writes task and command
//     status from state files, satisfying the StateReader/StateWriter interfaces.
//   - Complete: Handles command completion with WAL-based crash recovery
//     (completeIntent) to ensure idempotent multi-step completion sequences.
//   - Retry: Replaces a failed task with a new one, re-assigning to a worker
//     and cascading recovery to blocked downstream tasks.
//   - Rebuild: Reconstructs command state from worker result files, pruning
//     stale entries and applying the latest status for each task.
//   - Cancel: Validates cancellation requests against command state.
//   - ValidateTaskDAG / ValidatePhaseDAG: Uses Kahn's algorithm for
//     topological sort with DFS-based cycle detection and reporting.
//   - WorkerAssignment: Assigns tasks to workers based on Bloom taxonomy
//     levels and configured worker models.
//   - migrator: Sequential schema migration for state files from older
//     versions to the current schema version.
//   - Validation: Comprehensive field validation for task inputs including
//     name uniqueness, length limits, reserved-name checks, and DAG integrity.
package plan
