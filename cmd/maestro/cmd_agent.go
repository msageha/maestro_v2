package main

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
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
	cmd.RequiredString(&agentID, "agent-id", "Target agent ID")
	cmd.StringVar(&message, "message", "", "Message to send to the agent")
	cmd.StringVar(&mode, "mode", "deliver", "Delivery mode (deliver|with_clear|interrupt|is_busy|clear)")
	cmd.Var(&modeSetter{target: &mode, val: "with_clear"}, "with-clear", "Set mode to with_clear (clear then deliver)")
	cmd.Var(&modeSetter{target: &mode, val: "interrupt"}, "interrupt", "Set mode to interrupt")
	cmd.Var(&modeSetter{target: &mode, val: "is_busy"}, "is-busy", "Set mode to is_busy (check if agent is busy)")
	cmd.Var(&modeSetter{target: &mode, val: "clear"}, "clear", "Set mode to clear")

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
	if err := setupTmuxSession("agent exec", maestroDir, cfg); err != nil {
		return err
	}

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
			return &CLIError{Code: ExitCodeRetryable, Msg: fmt.Sprintf("maestro agent exec: %v", result.Error)}
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
