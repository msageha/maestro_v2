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
	cmd := NewCommand("maestro agent launch", "maestro agent launch")
	if err := cmd.Parse(args); err != nil {
		return err
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
	cmd := NewCommand("maestro agent exec", "maestro agent exec --agent-id <id> [--mode <mode>] [--message <msg>]")
	var agentID, message string
	mode := "deliver"
	cmd.RequiredString(&agentID, "agent-id", "")
	cmd.StringVar(&message, "message", "", "")
	cmd.StringVar(&mode, "mode", "deliver", "")
	cmd.Var(&modeSetter{target: &mode, val: "with_clear"}, "with-clear", "")
	cmd.Var(&modeSetter{target: &mode, val: "interrupt"}, "interrupt", "")
	cmd.Var(&modeSetter{target: &mode, val: "is_busy"}, "is-busy", "")
	cmd.Var(&modeSetter{target: &mode, val: "clear"}, "clear", "")

	if err := cmd.Parse(args); err != nil {
		return err
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
	if err := validate.ProjectName(cfg.Project.Name); err != nil {
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
