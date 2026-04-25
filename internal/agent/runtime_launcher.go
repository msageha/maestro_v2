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
	Model  string // Model name (e.g. "opus", "sonnet")
	Prompt string // System prompt; used by runtimes that accept inline prompts (e.g. gemini -p)
}

// RuntimeLauncher resolves runtime-specific commands for multi-runtime support.
// Runtime selection is done via model name (ParseRuntimeFromModel); no config is required.
type RuntimeLauncher struct {
	runtimes       map[string]RuntimeDef
	defaultRuntime string
}

// NewRuntimeLauncher constructs a RuntimeLauncher with all built-in runtimes registered.
// Runtime availability is fixed; selection is controlled by the model field value, not config.
func NewRuntimeLauncher() *RuntimeLauncher {
	rl := &RuntimeLauncher{
		runtimes:       make(map[string]RuntimeDef),
		defaultRuntime: model.DefaultRuntime(),
	}

	// claude-code: Anthropic Claude Code CLI (default runtime).
	rl.runtimes[model.RuntimeClaudeCode] = RuntimeDef{
		Command: "claude",
	}

	// codex: OpenAI Codex CLI (Rust-based interactive TUI).
	// Launched without the "exec" subcommand — "exec" is non-interactive (headless).
	// The interactive TUI mode is the top-level "codex" command.
	//
	// --ask-for-approval never: auto-approve every tool call without interactive prompts.
	//   Short form: -a never.
	//   This replaces --full-auto, which is actually an alias for
	//   "-a on-request --sandbox workspace-write". That alias:
	//     (a) sets the sandbox to workspace-write (blocks all UDS connections), and
	//     (b) uses "on-request" approval (can still prompt in some cases).
	//   We need "never" (no prompts at all) and a sandbox that allows UDS.
	//   NOTE: --approval-policy is NOT a valid codex flag; the correct flag name is
	//   --ask-for-approval (or its short form -a).
	//
	// --sandbox danger-full-access: disable sandbox restrictions entirely. This is
	//   required for Maestro because:
	//     1. workspace-write (and by extension --full-auto) blocks all Unix domain
	//        socket connections, preventing maestro commands inside the codex session
	//        from reaching .maestro/daemon.sock. Every `maestro queue write` /
	//        `maestro status` / `maestro plan add-task` call would fail or prompt.
	//     2. Codex agents need full filesystem access to perform code changes,
	//        git operations, and worktree manipulation — the same level already
	//        granted to claude-code via --dangerously-skip-permissions.
	//
	// Requires OPENAI_API_KEY (or OPENAI_BASE_URL + compatible key) environment variable.
	rl.runtimes[model.RuntimeCodex] = RuntimeDef{
		Command: "codex",
		Args:    []string{"--ask-for-approval", "never", "--sandbox", "danger-full-access"},
	}

	// gemini: Google Gemini CLI. Requires GEMINI_API_KEY environment variable.
	// -p (prompt) is appended dynamically via RuntimeLaunchOptions.Prompt.
	rl.runtimes[model.RuntimeGemini] = RuntimeDef{
		Command: "gemini",
		Args:    []string{"--approval-mode=yolo", "-s"},
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
func buildRuntimeArgs(runtime string, def RuntimeDef, opts RuntimeLaunchOptions) []string {
	args := make([]string, len(def.Args))
	copy(args, def.Args)

	switch runtime {
	case model.RuntimeGemini:
		if opts.Model != "" {
			args = append(args, "--model", opts.Model)
		}
		if opts.Prompt != "" {
			args = append(args, "-p", opts.Prompt)
		}
	case model.RuntimeCodex:
		if opts.Model != "" {
			args = append(args, "--model", opts.Model)
		}
		// codex accepts the initial prompt as a trailing positional argument.
		// This is the only CLI mechanism for passing system-level instructions;
		// codex has no --system-prompt flag. The prompt is treated as the first
		// user message that seeds the session context.
		if opts.Prompt != "" {
			args = append(args, opts.Prompt)
		}
	default:
		// claude-code and future runtimes: --model only.
		// claude-code handles system prompts via buildLaunchArgs (--system-prompt /
		// --append-system-prompt), not via RuntimeLaunchOptions.Prompt.
		if opts.Model != "" {
			args = append(args, "--model", opts.Model)
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
