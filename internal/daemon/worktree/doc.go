// Package worktree manages the git worktree lifecycle for Worker isolation in
// the maestro daemon. Each Worker operates in a dedicated git worktree so that
// concurrent file edits do not interfere with each other or the project root.
//
// # Main components
//
//   - Manager: Central manager that serializes all git operations through a
//     Single-Writer mutex. Handles worktree creation, branch setup,
//     integration merging, cleanup, and state persistence.
//   - MergeToIntegration: Merges worker branches into a shared integration
//     branch in deterministic order. Detects merge conflicts and transitions
//     through failure/quarantine states after repeated unrecoverable errors.
//   - CleanupCommand / CleanupGC: Removes worktrees and branches for
//     completed commands. GC prunes orphaned worktrees whose state files no
//     longer exist.
//   - Recover / Unquarantine / ResumeMerge: Operator-recovery entry points
//     for restoring quarantined integrations or resuming after manual conflict
//     resolution.
//   - Resolver: Conflict resolution pipeline with generation-based CAS tokens
//     to prevent stale resolver attempts from colliding with re-detected
//     conflicts.
//   - PathGuard: Validates that filesystem targets remain strictly inside the
//     project root via symlink-resolved path checks, preventing destructive
//     operations from escaping the project tree.
//   - GitOperations: Low-level git command execution with transient error
//     classification (lock contention vs permanent errors) for retry decisions.
//   - StateStore: Persistence layer for per-command worktree state files
//     (state/worktrees/{cmd}.yaml).
package worktree
