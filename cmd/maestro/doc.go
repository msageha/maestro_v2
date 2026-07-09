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
//   - plan: Submits task plans, reports completion, adds retry tasks, injects
//     tasks into active plans (add-task), requests cancellation, and rebuilds
//     state from results.
//   - agent: Launches and communicates with agents in tmux panes.
//   - worker: Queries idle worker status.
//   - task: Manages task heartbeats.
//   - skill: Lists, approves, and rejects skill candidates.
//   - dashboard: Regenerates the dashboard.md status file.
//   - resolve-conflict: Operator command for clearing a worker from
//     commit_failed_workers when the publish-to-base step is blocked by a
//     stale commit failure. NOT for in-phase merge_conflict signals — those
//     are addressed by `plan add-task --worker-id <id>`, which dispatches a
//     conflict resolution task and lets the daemon auto-run resume_merge.
//   - daemon: Runs the background daemon process (internal).
package main
