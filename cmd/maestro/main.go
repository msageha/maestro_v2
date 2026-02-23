package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/notify"
	"github.com/msageha/maestro_v2/internal/plan"
	"github.com/msageha/maestro_v2/internal/setup"
	"github.com/msageha/maestro_v2/internal/status"
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
	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	d, err := daemon.New(maestroDir, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create daemon: %v\n", err)
		os.Exit(1)
	}

	// Wire Phase 6 state reader for dependency resolution
	lockMap := lock.NewMutexMap()
	sm := plan.NewStateManager(maestroDir, lockMap)
	reader := plan.NewPlanStateReader(sm)
	d.SetStateReader(reader)

	if err := d.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		os.Exit(1)
	}
}

func runSetup(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: maestro setup <project_dir>")
		os.Exit(1)
	}
	if err := setup.Run(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		os.Exit(1)
	}
	absDir, _ := filepath.Abs(args[0])
	fmt.Printf("Initialized .maestro/ in %s\n", absDir)
}

func runUp(_ []string) {
	fmt.Fprintln(os.Stderr, "up: not yet implemented")
	os.Exit(1)
}

func runDown(_ []string) {
	fmt.Fprintln(os.Stderr, "down: not yet implemented")
	os.Exit(1)
}

func runStatus(args []string) {
	jsonOutput := false
	for _, a := range args {
		switch a {
		case "--json":
			jsonOutput = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\nusage: maestro status [--json]\n", a)
			os.Exit(1)
		}
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	if err := status.Run(maestroDir, jsonOutput); err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		os.Exit(1)
	}
}

func runQueueWrite(_ []string) {
	fmt.Fprintln(os.Stderr, "queue write: not yet implemented")
	os.Exit(1)
}

func runResultWrite(_ []string) {
	fmt.Fprintln(os.Stderr, "result write: not yet implemented")
	os.Exit(1)
}

func runPlanSubmit(args []string) {
	var commandID, tasksFile, phaseName string
	dryRun := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--command-id":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--command-id requires a value")
				os.Exit(1)
			}
			i++
			commandID = args[i]
		case "--tasks-file":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--tasks-file requires a value")
				os.Exit(1)
			}
			i++
			tasksFile = args[i]
		case "--phase":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--phase requires a value")
				os.Exit(1)
			}
			i++
			phaseName = args[i]
		case "--dry-run":
			dryRun = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			fmt.Fprintln(os.Stderr, "usage: maestro plan submit --command-id <id> [--tasks-file <path>] [--phase <name>] [--dry-run]")
			os.Exit(1)
		}
	}

	if commandID == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro plan submit --command-id <id> [--tasks-file <path>] [--phase <name>] [--dry-run]")
		os.Exit(1)
	}

	if tasksFile == "" {
		tasksFile = "-" // default to stdin
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	result, err := plan.Submit(plan.SubmitOptions{
		CommandID:  commandID,
		TasksFile:  tasksFile,
		PhaseName:  phaseName,
		DryRun:     dryRun,
		MaestroDir: maestroDir,
		Config:     cfg,
	})
	if err != nil {
		if verrs, ok := err.(*plan.ValidationErrors); ok {
			fmt.Fprint(os.Stderr, verrs.FormatStderr())
		} else {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(out))
}

func runPlanComplete(args []string) {
	var commandID, summary string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--command-id":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--command-id requires a value")
				os.Exit(1)
			}
			i++
			commandID = args[i]
		case "--summary":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--summary requires a value")
				os.Exit(1)
			}
			i++
			summary = args[i]
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			fmt.Fprintln(os.Stderr, "usage: maestro plan complete --command-id <id> --summary <text>")
			os.Exit(1)
		}
	}

	if commandID == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro plan complete --command-id <id> --summary <text>")
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	result, err := plan.Complete(plan.CompleteOptions{
		CommandID:  commandID,
		Summary:    summary,
		MaestroDir: maestroDir,
		Config:     cfg,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(out))
}

func runPlanAddRetryTask(args []string) {
	var commandID, retryOf, purpose, content, acceptanceCriteria string
	var bloomLevel int
	var blockedBy []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--command-id":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--command-id requires a value")
				os.Exit(1)
			}
			i++
			commandID = args[i]
		case "--retry-of":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--retry-of requires a value")
				os.Exit(1)
			}
			i++
			retryOf = args[i]
		case "--purpose":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--purpose requires a value")
				os.Exit(1)
			}
			i++
			purpose = args[i]
		case "--content":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--content requires a value")
				os.Exit(1)
			}
			i++
			content = args[i]
		case "--acceptance-criteria":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--acceptance-criteria requires a value")
				os.Exit(1)
			}
			i++
			acceptanceCriteria = args[i]
		case "--bloom-level":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--bloom-level requires a value")
				os.Exit(1)
			}
			i++
			fmt.Sscanf(args[i], "%d", &bloomLevel)
		case "--blocked-by":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--blocked-by requires a value")
				os.Exit(1)
			}
			i++
			blockedBy = append(blockedBy, args[i])
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			fmt.Fprintln(os.Stderr, "usage: maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]...")
			os.Exit(1)
		}
	}

	if commandID == "" || retryOf == "" || purpose == "" || content == "" || acceptanceCriteria == "" || bloomLevel == 0 {
		fmt.Fprintln(os.Stderr, "usage: maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]...")
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	result, err := plan.AddRetryTask(plan.RetryOptions{
		CommandID:          commandID,
		RetryOf:            retryOf,
		Purpose:            purpose,
		Content:            content,
		AcceptanceCriteria: acceptanceCriteria,
		BlockedBy:          blockedBy,
		BloomLevel:         bloomLevel,
		MaestroDir:         maestroDir,
		Config:             cfg,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(out))
}

func runPlanRequestCancel(args []string) {
	var commandID, requestedBy, reason string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--command-id":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--command-id requires a value")
				os.Exit(1)
			}
			i++
			commandID = args[i]
		case "--requested-by":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--requested-by requires a value")
				os.Exit(1)
			}
			i++
			requestedBy = args[i]
		case "--reason":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--reason requires a value")
				os.Exit(1)
			}
			i++
			reason = args[i]
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			fmt.Fprintln(os.Stderr, "usage: maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]")
			os.Exit(1)
		}
	}

	if commandID == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]")
		os.Exit(1)
	}

	if requestedBy == "" {
		requestedBy = "cli"
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	err := plan.RequestCancel(plan.RequestCancelOptions{
		CommandID:   commandID,
		RequestedBy: requestedBy,
		Reason:      reason,
		MaestroDir:  maestroDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	fmt.Printf("cancel requested for command %s\n", commandID)
}

func runPlanRebuild(args []string) {
	var commandID string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--command-id":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--command-id requires a value")
				os.Exit(1)
			}
			i++
			commandID = args[i]
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			fmt.Fprintln(os.Stderr, "usage: maestro plan rebuild --command-id <id>")
			os.Exit(1)
		}
	}

	if commandID == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro plan rebuild --command-id <id>")
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	err := plan.Rebuild(plan.RebuildOptions{
		CommandID:  commandID,
		MaestroDir: maestroDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	fmt.Printf("state rebuilt for command %s\n", commandID)
}

func runAgentLaunch(_ []string) {
	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}
	if err := agent.Launch(maestroDir); err != nil {
		fmt.Fprintf(os.Stderr, "agent launch: %v\n", err)
		os.Exit(1)
	}
}

func runAgentExec(args []string) {
	var agentID, message, mode string
	mode = "deliver"

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--agent-id":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--agent-id requires a value")
				os.Exit(1)
			}
			i++
			agentID = args[i]
		case "--message":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--message requires a value")
				os.Exit(1)
			}
			i++
			message = args[i]
		case "--mode":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--mode requires a value")
				os.Exit(1)
			}
			i++
			mode = args[i]
		case "--with-clear":
			mode = "with_clear"
		case "--interrupt":
			mode = "interrupt"
		case "--is-busy":
			mode = "is_busy"
		case "--clear":
			mode = "clear"
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\nusage: maestro agent exec --agent-id <id> [--mode <mode>] [--message <msg>]\n", args[i])
			os.Exit(1)
		}
	}

	if agentID == "" {
		fmt.Fprintln(os.Stderr, "usage: maestro agent exec --agent-id <id> [--mode <mode>] [--message <msg>]")
		os.Exit(1)
	}

	maestroDir := findMaestroDir()
	if maestroDir == "" {
		fmt.Fprintln(os.Stderr, "error: .maestro/ directory not found. Run 'maestro setup <dir>' first.")
		os.Exit(1)
	}

	cfg, err := loadConfig(maestroDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	exec, err := agent.NewExecutor(maestroDir, cfg.Watcher, cfg.Logging.Level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create executor: %v\n", err)
		os.Exit(1)
	}
	defer exec.Close()

	result := exec.Execute(agent.ExecRequest{
		AgentID: agentID,
		Message: message,
		Mode:    agent.ExecMode(mode),
	})

	if result.Error != nil {
		fmt.Fprintf(os.Stderr, "agent exec: %v\n", result.Error)
		if result.Retryable {
			os.Exit(2)
		}
		os.Exit(1)
	}

	if mode == "is_busy" {
		if result.Success {
			fmt.Println("busy")
			os.Exit(0)
		}
		fmt.Println("idle")
		os.Exit(1)
	}
}

func runWorkerStandby(_ []string) {
	fmt.Fprintln(os.Stderr, "worker standby: not yet implemented")
	os.Exit(1)
}

func runNotify(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: maestro notify <title> <message>")
		os.Exit(1)
	}
	if err := notify.Send(args[0], args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "notify: %v\n", err)
		os.Exit(1)
	}
}

func runDashboard(_ []string) {
	fmt.Fprintln(os.Stderr, "dashboard: not yet implemented")
	os.Exit(1)
}

// findMaestroDir searches for .maestro/ in the current directory and ancestors.
func findMaestroDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ".maestro")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func loadConfig(maestroDir string) (model.Config, error) {
	data, err := os.ReadFile(filepath.Join(maestroDir, "config.yaml"))
	if err != nil {
		return model.Config{}, fmt.Errorf("read config.yaml: %w", err)
	}
	var cfg model.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return model.Config{}, fmt.Errorf("parse config.yaml: %w", err)
	}
	return cfg, nil
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
