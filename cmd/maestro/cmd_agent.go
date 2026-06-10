package main

import (
	"errors"
	"fmt"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// mapAgentExecError maps an agent.Executor result into the CLI error
// surface used by `maestro agent exec`. Returns nil when the result has
// no error to surface. Extracted from runAgentExec so the
// ErrSubmitConfirmUncertain → ExitCodeSubmitUncertain mapping can be
// unit-tested without standing up a tmux session.
//
// The three branches model three distinct outcomes:
//   - ErrSubmitConfirmUncertain: message was put on the wire but the
//     visual confirmation probe exhausted. The deliverer marks this
//     non-retryable to avoid double-submit; the CLI surfaces it with a
//     dedicated exit code (ExitCodeSubmitUncertain) so scripts can tell
//     it apart from a hard failure (1) or a retryable transport
//     error (2).
//   - result.Retryable: transient transport failure that the caller
//     can safely retry.
//   - default: everything else — treated as a generic CLI error.
func mapAgentExecError(result agent.ExecResult, agentID string) error {
	if result.Error == nil {
		return nil
	}
	if errors.Is(result.Error, agent.ErrSubmitConfirmUncertain) {
		return &CLIError{
			Code: ExitCodeSubmitUncertain,
			Msg: fmt.Sprintf(
				"maestro agent exec: %v (message was sent to %s; visual confirmation timed out — inspect the agent pane before re-running, as a re-exec can produce a duplicate submit)",
				result.Error, agentID),
		}
	}
	if result.Retryable {
		return &CLIError{Code: ExitCodeRetryable, Msg: fmt.Sprintf("maestro agent exec: %v", result.Error)}
	}
	return fmt.Errorf("maestro agent exec: %w", result.Error)
}

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

// guardAgentExecCaller rejects `maestro agent exec` invocations that
// originate from managed agent panes (orchestrator / planner / worker).
// The command pastes a message directly into a tmux pane via
// agent.Executor, bypassing the daemon's UDS dispatch path entirely —
// no lease check, no fencing, no dispatch_id dedupe. From an operator
// terminal that is an intentional debug affordance, but from an agent
// it breaks the "agents communicate only via the daemon" invariant and
// allows one agent to impersonate another. Workers are additionally
// blocked at L1 (workerDisallowedTools) and L2 (worker_policy_hook.sh);
// this CLI-layer check is the daemon-side leg of that multi-layer
// defense, mirroring the operator-only guard in cmd_plan_ops.go.
func guardAgentExecCaller(resolveRole func() (string, error)) error {
	role, err := resolveRole()
	if err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro agent exec: cannot resolve caller role: %v", err)}
	}
	if role != uds.RoleCLI {
		return &CLIError{
			Code: 1,
			Msg: fmt.Sprintf(
				"maestro agent exec: restricted to operator (CLI) role; caller role %q is not permitted — agents must communicate via the daemon dispatch path",
				role),
		}
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

	if err := guardAgentExecCaller(uds.ResolveCallerRole); err != nil {
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

	if err := mapAgentExecError(result, agentID); err != nil {
		return err
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
