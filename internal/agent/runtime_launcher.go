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

	// codex: OpenAI Codex CLI. Requires CODEX_API_KEY environment variable.
	rl.runtimes[model.RuntimeCodex] = RuntimeDef{
		Command: "codex",
		Args:    []string{"exec", "-a", "never", "-s", "workspace-write"},
	}

	// gemini: Google Gemini CLI. Requires GOOGLE_API_KEY environment variable.
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
	default:
		// claude-code, codex, and future runtimes use --model.
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
