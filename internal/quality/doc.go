// Package quality implements a configurable quality gate evaluation engine for
// the maestro system. Quality gates are rule-based checkpoints that validate
// task and command data at defined lifecycle stages.
//
// # Main components
//
//   - Engine: Central evaluation engine that compiles gate definitions into
//     optimized rule trees, evaluates them against input data, and caches
//     results with singleflight deduplication. Supports hot-reload via
//     configuration checksum comparison.
//   - Loader: Loads and validates gate configuration from YAML files in the
//     quality_gates directory, enforcing file permission checks for safety.
//   - GateType: Defines evaluation points in the task lifecycle — pre_task,
//     post_task, phase_transition, and command_validation.
//   - RuleEvaluator: Interface for condition evaluation. Built-in evaluators
//     include field_validation (with operators like exists, equals, contains,
//     matches_regex, in_range), logical combinators (and, or, not), and
//     script execution.
//   - Script evaluator: Executes user-defined shell scripts in a sandboxed
//     environment (bash --restricted) with dangerous command pattern detection,
//     length limits, and execution timeouts.
//   - resultCache: Thread-safe LRU cache with TTL expiration for evaluation
//     results, preventing redundant re-evaluation of identical inputs.
package quality
