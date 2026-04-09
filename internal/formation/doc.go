// Package formation manages the lifecycle of the maestro agent formation:
// daemon process, tmux session, and agent panes (orchestrator, planner,
// workers).
//
// # Main components
//
//   - RunUp: Starts the formation by creating a tmux session with hardened
//     options, launching the daemon as a background process, and spawning
//     agent panes for orchestrator, planner, and configured workers.
//   - RunDown: Gracefully shuts down the formation by stopping the daemon
//     via UDS, killing the tmux session, and cleaning up transient state
//     while preserving quarantine data for forensics.
//   - createFormation: Creates the tmux session with per-window
//     remain-on-exit settings and rollback-on-failure semantics.
//   - startDaemon / stopDaemon: Manages the daemon process lifecycle
//     including UDS-based graceful shutdown, PID file validation, and
//     SIGTERM-to-SIGKILL escalation.
//   - processManager: Interface abstracting OS-level process operations
//     (alive check, signal sending, start time query) for testability.
//   - StartupState: Manages formation startup state persistence, including
//     reset of transient state files while preserving quarantine entries.
//   - TmuxServerOptions: Applies tmux server and session hardening options
//     to prevent accidental session destruction.
package formation
