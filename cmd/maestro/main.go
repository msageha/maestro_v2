package main

import (
	"fmt"
	"os"
)

const version = "2.0.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "daemon":
		runDaemon(os.Args[2:])
	case "setup":
		runSetup(os.Args[2:])
	case "up":
		runUp(os.Args[2:])
	case "down":
		runDown(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "queue":
		runQueue(os.Args[2:])
	case "result":
		runResult(os.Args[2:])
	case "plan":
		runPlan(os.Args[2:])
	case "agent":
		runAgent(os.Args[2:])
	case "worker":
		runWorker(os.Args[2:])
	case "notify":
		runNotify(os.Args[2:])
	case "dashboard":
		runDashboard(os.Args[2:])
	case "version":
		fmt.Printf("maestro %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func runQueue(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro queue <write> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "write":
		runQueueWrite(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown queue subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro queue write <target> [options]")
		os.Exit(1)
	}
}

func runResult(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro result <write> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "write":
		runResultWrite(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown result subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro result write <reporter> [options]")
		os.Exit(1)
	}
}

func runPlan(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro plan <submit|complete|add-retry-task|request-cancel|rebuild> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "submit":
		runPlanSubmit(args[1:])
	case "complete":
		runPlanComplete(args[1:])
	case "add-retry-task":
		runPlanAddRetryTask(args[1:])
	case "request-cancel":
		runPlanRequestCancel(args[1:])
	case "rebuild":
		runPlanRebuild(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown plan subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro plan <submit|complete|add-retry-task|request-cancel|rebuild> [options]")
		os.Exit(1)
	}
}

func runAgent(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro agent <launch|exec> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "launch":
		runAgentLaunch(args[1:])
	case "exec":
		runAgentExec(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown agent subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro agent <launch|exec> [options]")
		os.Exit(1)
	}
}

func runWorker(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro worker <standby> [options]")
		os.Exit(1)
	}
	switch args[0] {
	case "standby":
		runWorkerStandby(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown worker subcommand: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: maestro worker standby [options]")
		os.Exit(1)
	}
}

// Stub functions — implementations will be added in later phases.

func runDaemon(_ []string) {
	fmt.Fprintln(os.Stderr, "daemon: not yet implemented")
	os.Exit(1)
}

func runSetup(_ []string) {
	fmt.Fprintln(os.Stderr, "setup: not yet implemented")
	os.Exit(1)
}

func runUp(_ []string) {
	fmt.Fprintln(os.Stderr, "up: not yet implemented")
	os.Exit(1)
}

func runDown(_ []string) {
	fmt.Fprintln(os.Stderr, "down: not yet implemented")
	os.Exit(1)
}

func runStatus(_ []string) {
	fmt.Fprintln(os.Stderr, "status: not yet implemented")
	os.Exit(1)
}

func runQueueWrite(_ []string) {
	fmt.Fprintln(os.Stderr, "queue write: not yet implemented")
	os.Exit(1)
}

func runResultWrite(_ []string) {
	fmt.Fprintln(os.Stderr, "result write: not yet implemented")
	os.Exit(1)
}

func runPlanSubmit(_ []string) {
	fmt.Fprintln(os.Stderr, "plan submit: not yet implemented")
	os.Exit(1)
}

func runPlanComplete(_ []string) {
	fmt.Fprintln(os.Stderr, "plan complete: not yet implemented")
	os.Exit(1)
}

func runPlanAddRetryTask(_ []string) {
	fmt.Fprintln(os.Stderr, "plan add-retry-task: not yet implemented")
	os.Exit(1)
}

func runPlanRequestCancel(_ []string) {
	fmt.Fprintln(os.Stderr, "plan request-cancel: not yet implemented")
	os.Exit(1)
}

func runPlanRebuild(_ []string) {
	fmt.Fprintln(os.Stderr, "plan rebuild: not yet implemented")
	os.Exit(1)
}

func runAgentLaunch(_ []string) {
	fmt.Fprintln(os.Stderr, "agent launch: not yet implemented")
	os.Exit(1)
}

func runAgentExec(_ []string) {
	fmt.Fprintln(os.Stderr, "agent exec: not yet implemented")
	os.Exit(1)
}

func runWorkerStandby(_ []string) {
	fmt.Fprintln(os.Stderr, "worker standby: not yet implemented")
	os.Exit(1)
}

func runNotify(_ []string) {
	fmt.Fprintln(os.Stderr, "notify: not yet implemented")
	os.Exit(1)
}

func runDashboard(_ []string) {
	fmt.Fprintln(os.Stderr, "dashboard: not yet implemented")
	os.Exit(1)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `maestro %s — Multi-agent orchestration system

Usage: maestro <command> [options]

Formation:
  setup <dir>       Initialize .maestro/ directory
  up [flags]        Start formation (daemon + tmux + agents)
  down              Graceful shutdown
  status [--json]   Show formation status

Agent Commands (CLI → Daemon):
  queue write <target> [options]   Write to queue
  result write <reporter> [options] Write result
  plan submit [options]            Submit task plan
  plan complete [options]          Report command completion
  plan add-retry-task [options]    Replace failed task
  plan request-cancel [options]    Request cancellation
  plan rebuild [options]           Rebuild state from results

Internal:
  daemon            Run daemon process
  agent launch      Launch agent in tmux pane
  agent exec        Send message to agent

Utilities:
  worker standby    Show idle workers
  notify <title> <msg>  macOS notification
  dashboard         Regenerate dashboard.md
  version           Show version
  help              Show this help

`, version)
}
