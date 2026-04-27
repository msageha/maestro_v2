package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/msageha/maestro_v2/internal/validate"
)

// runHook dispatches internal hook helper commands.
func (a *cliApp) runHook(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro hook: missing subcommand\nusage: maestro hook <policy-check> [options]"}
	}
	switch args[0] {
	case "policy-check":
		return a.runHookPolicyCheck(args[1:], os.Stdin, os.Stdout)
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro hook: unknown subcommand: %s\nusage: maestro hook policy-check [options]", args[0])}
	}
}

type hookPolicyWireInput struct {
	ToolName  string              `json:"tool_name"`
	ToolInput hookPolicyToolInput `json:"tool_input"`
}

type hookPolicyToolInput struct {
	Command  string `json:"command,omitempty"`
	FilePath string `json:"file_path,omitempty"`
}

type hookPolicyDecisionOutput struct {
	Allow              bool                    `json:"allow"`
	Reason             string                  `json:"reason,omitempty"`
	HookSpecificOutput *hookSpecificDenyOutput `json:"hookSpecificOutput,omitempty"`
}

type hookSpecificDenyOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

func (a *cliApp) runHookPolicyCheck(args []string, in io.Reader, out io.Writer) error {
	cmd := NewCommand("maestro hook policy-check", "maestro hook policy-check [--run-on-main] [--project-root <path>]")
	var runOnMain bool
	var projectRoot string
	cmd.BoolVar(&runOnMain, "run-on-main", false, "Evaluate policy as run_on_main read-only mode")
	cmd.StringVar(&projectRoot, "project-root", "", "Project root for path confinement checks")
	if err := cmd.Parse(args); err != nil {
		return err
	}

	raw, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("maestro hook policy-check: read stdin: %w", err)
	}
	var wire hookPolicyWireInput
	if err := json.Unmarshal(raw, &wire); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro hook policy-check: invalid hook JSON: %v", err)}
	}

	decision := validate.CheckWorkerPolicy(validate.HookInput{
		ToolName:    wire.ToolName,
		Command:     wire.ToolInput.Command,
		FilePath:    wire.ToolInput.FilePath,
		RunOnMain:   runOnMain,
		ProjectRoot: projectRoot,
	})

	resp := hookPolicyDecisionOutput{Allow: decision.Allow}
	if !decision.Allow {
		resp.Reason = decision.Reason
		resp.HookSpecificOutput = &hookSpecificDenyOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: decision.Reason,
		}
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		return fmt.Errorf("maestro hook policy-check: write decision: %w", err)
	}
	return nil
}
