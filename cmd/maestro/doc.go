// Package main is the entry point for the maestro CLI, a multi-agent
// orchestration system for coordinating parallel task execution.
//
// # Subcommands
//
//   - setup: Initializes the .maestro/ directory structure for a project.
//   - up: Starts the agent formation (daemon, tmux session, agent panes).
//   - down: Gracefully shuts down the formation.
//   - status: Displays formation and command status.
//   - queue: Reads and writes agent queue entries (CLI-to-daemon IPC).
//   - result: Reports task execution results from workers.
//   - plan: Submits task plans, reports completion, adds retry tasks, requests
//     cancellation, and rebuilds state from results.
//   - agent: Launches and communicates with agents in tmux panes.
//   - worker: Queries idle worker status.
//   - task: Manages task heartbeats.
//   - skill: Lists, approves, and rejects skill candidates.
//   - dashboard: Regenerates the dashboard.md status file.
//   - resolve-conflict: Operator command for resolving worker merge conflicts.
//   - daemon: Runs the background daemon process (internal).
package main
