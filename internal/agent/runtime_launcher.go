package agent

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
)

// RuntimeDef describes how to invoke a specific coding agent runtime.
type RuntimeDef struct {
	Command string
	Args    []string
	EnvVars map[string]string
}

// RuntimeLaunchOptions contains optional parameters for building launch commands.
type RuntimeLaunchOptions struct {
	Model string // Model name (e.g. "opus", "sonnet", "gemini-2.5-pro")
	// Prompt is the system / role prompt assembled by buildSystemPrompt.
	// For claude-code this field is ignored: buildLaunchArgs feeds the
	// prompt via --append-system-prompt / --system-prompt directly. For
	// codex / gemini there is no equivalent system-prompt flag (verified
	// against codex 0.125.0 and gemini-cli main; both treat first-class
	// system instructions as a file-discovery feature, not a CLI flag),
	// so the prompt is injected as the runtime's "initial interactive
	// prompt" — `-i / --prompt-interactive` for gemini-cli and the
	// positional [PROMPT] for codex. That makes it a user turn rather
	// than a system turn, but it is still consumed by the agent as
	// framing before the dispatcher pastes the first task envelope, so
	// role guidance reaches the worker.
	Prompt string
}

// RuntimeLauncher resolves the supported managed-agent runtime command.
// claude-code is the default; codex and gemini are registered as worker
// runtimes (orchestrator / planner remain claude-code only — see
// launchAlternativeRuntime).
type RuntimeLauncher struct {
	runtimes       map[string]RuntimeDef
	defaultRuntime string
}

// NewRuntimeLauncher constructs a RuntimeLauncher for supported managed roles.
func NewRuntimeLauncher() *RuntimeLauncher {
	rl := &RuntimeLauncher{
		runtimes:       make(map[string]RuntimeDef),
		defaultRuntime: model.DefaultRuntime(),
	}
	rl.runtimes[model.RuntimeClaudeCode] = RuntimeDef{
		Command: "claude",
	}
	// codex (interactive REPL). `--dangerously-bypass-approvals-and-sandbox`
	// is required for two reasons:
	//
	//  1. `--sandbox workspace-write` blocks Unix-socket `connect(2)` to
	//     the daemon (.maestro/daemon.sock or its /tmp/maestro-uds-<uid>/
	//     fallback), so workers could edit files but never call
	//     `maestro result write` — the sandbox blocks IPC to anything
	//     outside the workspace allowlist.
	//
	//  2. The first-run "Do you trust the contents of this directory?"
	//     trust prompt is gated by sandbox/approval mode and would
	//     otherwise stop the worker pane on the very first launch. The
	//     bypass flag short-circuits the trust check the same way it
	//     short-circuits per-command approvals.
	//
	// codex has no .maestro/state read deny, no maestro-CLI escape-hatch
	// deny, and no PreToolUse interception. There is NO in-tree mechanical
	// defense against destructive shell commands for codex workers — that
	// responsibility sits with operator-side controls (host sandboxing,
	// repo-level shell policy). What the daemon does enforce mechanically:
	// run_on_main tasks are never assigned or dispatched to codex/gemini
	// workers (plan.AssignWorkers RequireClaudeRuntime +
	// dispatch.validateRunOnMainPreflight), so codex blast radius stays
	// inside its isolated git worktree.
	rl.runtimes[model.RuntimeCodex] = RuntimeDef{
		Command: "codex",
		Args:    []string{"--dangerously-bypass-approvals-and-sandbox"},
	}
	// gemini (interactive REPL). `--yolo` (a.k.a. `--approval-mode yolo`)
	// auto-approves every WriteFile/Edit/Shell tool call. Without it gemini
	// pauses on every tool action waiting for an interactive Allow click,
	// and the maestro worker pane has no human at the keyboard. As with
	// codex above, there is NO in-tree mechanical defense against
	// destructive shell commands for gemini workers (operator-side controls
	// own that); the daemon mechanically keeps gemini off run_on_main tasks
	// so its blast radius stays inside its isolated git worktree.
	rl.runtimes[model.RuntimeGemini] = RuntimeDef{
		Command: "gemini",
		Args:    []string{"--yolo"},
	}
	return rl
}

// GetCommand returns the command and args for the given runtime.
// Empty runtime falls back to the default (claude-code).
// Returns an error if the runtime is unknown.
func (rl *RuntimeLauncher) GetCommand(runtime string, opts RuntimeLaunchOptions) (string, []string, error) {
	if runtime == "" {
		runtime = rl.defaultRuntime
	}

	def, ok := rl.runtimes[runtime]
	if !ok {
		return "", nil, fmt.Errorf("unknown runtime %q", runtime)
	}

	args := buildRuntimeArgs(runtime, def, opts)
	return def.Command, args, nil
}

// buildRuntimeArgs constructs runtime-specific CLI arguments.
// Each runtime may interpret model/prompt options differently.
//
// Claude-code: --model selects the model. The system prompt is NOT routed
// through this helper; buildLaunchArgs handles --(append-)system-prompt
// directly so we honour the role-specific base_prompt_mode.
//
// Codex: --model selects the model. The system prompt — when supplied —
// becomes codex's positional [PROMPT], which the runtime treats as the
// first user turn. Codex has no --system-prompt / --instructions
// equivalent; the positional-prompt fallback is the documented seed for
// interactive sessions.
//
// Gemini: --model selects the model. The system prompt — when supplied —
// is passed via -i / --prompt-interactive, which seeds the interactive
// REPL with the prompt while keeping the worker pane interactive. The
// non-interactive --prompt / -p flag is intentionally NOT used because it
// drives gemini in headless mode and exits immediately, which would tear
// the worker pane down.
func buildRuntimeArgs(runtime string, def RuntimeDef, opts RuntimeLaunchOptions) []string {
	args := make([]string, len(def.Args))
	copy(args, def.Args)
	switch runtime {
	case model.RuntimeClaudeCode:
		if opts.Model != "" {
			args = append(args, "--model", opts.Model)
		}
	case model.RuntimeCodex:
		if opts.Model != "" {
			args = append(args, "--model", opts.Model)
		}
		if opts.Prompt != "" {
			args = append(args, opts.Prompt)
		}
	case model.RuntimeGemini:
		if opts.Model != "" {
			args = append(args, "--model", opts.Model)
		}
		if opts.Prompt != "" {
			args = append(args, "--prompt-interactive", opts.Prompt)
		}
	}
	return args
}

// FallbackToDefault returns the command and args for the default runtime (claude-code).
func (rl *RuntimeLauncher) FallbackToDefault() (string, []string) {
	def := rl.runtimes[rl.defaultRuntime]
	args := make([]string, len(def.Args))
	copy(args, def.Args)
	return def.Command, args
}
