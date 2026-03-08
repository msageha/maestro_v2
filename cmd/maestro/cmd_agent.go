package main

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runAgent dispatches agent subcommands (launch, exec).
func runAgent(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro agent: missing subcommand\nusage: maestro agent <launch|exec> [options]"}
	}
	switch args[0] {
	case "launch":
		return runAgentLaunch(args[1:])
	case "exec":
		return runAgentExec(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro agent: unknown subcommand: %s\nusage: maestro agent <launch|exec> [options]", args[0])}
	}
}

// runAgentLaunch starts an agent process inside a tmux pane.
func runAgentLaunch(args []string) error {
	fs := newFlagSet("maestro agent launch")
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro agent launch: %v\nusage: maestro agent launch", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro agent launch: unexpected argument: %s\nusage: maestro agent launch", fs.Arg(0))}
	}

	maestroDir, err := requireMaestroDir("agent launch")
	if err != nil {
		return err
	}
	if err := agent.Launch(maestroDir); err != nil {
		return fmt.Errorf("maestro agent launch: %w", err)
	}
	return nil
}

// runAgentExec sends a message to a running agent via the executor.
func runAgentExec(args []string) error {
	fs := newFlagSet("maestro agent exec")
	var agentID, message string
	mode := "deliver"
	fs.StringVar(&agentID, "agent-id", "", "")
	fs.StringVar(&message, "message", "", "")
	fs.StringVar(&mode, "mode", "deliver", "")
	fs.Var(&modeSetter{target: &mode, val: "with_clear"}, "with-clear", "")
	fs.Var(&modeSetter{target: &mode, val: "interrupt"}, "interrupt", "")
	fs.Var(&modeSetter{target: &mode, val: "is_busy"}, "is-busy", "")
	fs.Var(&modeSetter{target: &mode, val: "clear"}, "clear", "")

	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro agent exec: %v\nusage: maestro agent exec --agent-id <id> [--mode <mode>] [--message <msg>]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro agent exec: unexpected argument: %s\nusage: maestro agent exec --agent-id <id> [--mode <mode>] [--message <msg>]", fs.Arg(0))}
	}

	if agentID == "" {
		return &CLIError{Code: 1, Msg: "maestro agent exec: --agent-id is required\nusage: maestro agent exec --agent-id <id> [--mode <mode>] [--message <msg>]"}
	}

	maestroDir, err := requireMaestroDir("agent exec")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return fmt.Errorf("maestro agent exec: load config: %w", err)
	}
	// HIGH-16: Validate project name before use in tmux session name
	if err := validate.ValidateProjectName(cfg.Project.Name); err != nil {
		return fmt.Errorf("maestro agent exec: invalid project name: %w", err)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	exec, err := agent.NewExecutor(maestroDir, cfg.Watcher, cfg.Logging.Level)
	if err != nil {
		return fmt.Errorf("maestro agent exec: create executor: %w", err)
	}
	defer func() { _ = exec.Close() }()

	result := exec.Execute(agent.ExecRequest{
		AgentID: agentID,
		Message: message,
		Mode:    agent.ExecMode(mode),
	})

	if result.Error != nil {
		if result.Retryable {
			return &CLIError{Code: 2, Msg: fmt.Sprintf("maestro agent exec: %v", result.Error)}
		}
		return fmt.Errorf("maestro agent exec: %w", result.Error)
	}

	if mode == "is_busy" {
		if result.Success {
			fmt.Println("busy")
			return nil
		}
		fmt.Println("idle")
		return &CLIError{Code: 1, Silent: true}
	}
	return nil
}
