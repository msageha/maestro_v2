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
	Enabled bool
}

// RuntimeLaunchOptions contains optional parameters for building launch commands.
type RuntimeLaunchOptions struct {
	Model  string // Model name (e.g. "opus", "sonnet")
	Prompt string // System prompt; used by runtimes that accept inline prompts (e.g. gemini -p)
}

// RuntimeLauncher resolves runtime-specific commands for multi-runtime support (C-7).
type RuntimeLauncher struct {
	runtimes       map[string]RuntimeDef
	defaultRuntime string
}

// NewRuntimeLauncher constructs a RuntimeLauncher from the config's Runtimes map.
// claude-code is always registered as a hardcoded default.
func NewRuntimeLauncher(cfg map[string]model.RuntimeConfig) *RuntimeLauncher {
	rl := &RuntimeLauncher{
		runtimes:       make(map[string]RuntimeDef),
		defaultRuntime: model.DefaultRuntime(),
	}

	// Hardcoded default: claude-code is always available.
	rl.runtimes[model.RuntimeClaudeCode] = RuntimeDef{
		Command: "claude",
		Args:    nil,
		EnvVars: nil,
		Enabled: true,
	}

	// codex: OpenAI Codex CLI.
	// Requires CODEX_API_KEY environment variable to be set.
	// Enabled by default: runtime selection via model name (e.g. model: "codex") implicitly
	// enables this runtime. Can be explicitly disabled via runtimes.codex.enabled: false.
	rl.runtimes[model.RuntimeCodex] = RuntimeDef{
		Command: "codex",
		Args:    []string{"exec", "-a", "never", "-s", "workspace-write"},
		EnvVars: nil,
		Enabled: true,
	}

	// gemini: Google Gemini CLI.
	// Requires GOOGLE_API_KEY environment variable to be set.
	// -p (prompt) is added dynamically via RuntimeLaunchOptions.Prompt in GetCommand.
	// Enabled by default: runtime selection via model name (e.g. model: "gemini" or
	// model: "gemini-2.5-pro") implicitly enables this runtime.
	rl.runtimes[model.RuntimeGemini] = RuntimeDef{
		Command: "gemini",
		Args:    []string{"--approval-mode=yolo", "-s"},
		EnvVars: nil,
		Enabled: true,
	}

	// Apply config overrides.
	for name, rc := range cfg {
		if !model.ValidateRuntime(name) {
			continue
		}
		if def, ok := rl.runtimes[name]; ok {
			def.Enabled = rc.EffectiveEnabled()
			rl.runtimes[name] = def
		}
	}

	return rl
}

// GetCommand returns the command and args for the given runtime.
// Empty runtime falls back to the default (claude-code).
// Returns an error if the runtime is disabled or unknown.
func (rl *RuntimeLauncher) GetCommand(runtime string, opts RuntimeLaunchOptions) (string, []string, error) {
	if runtime == "" {
		runtime = rl.defaultRuntime
	}

	def, ok := rl.runtimes[runtime]
	if !ok {
		return "", nil, fmt.Errorf("unknown runtime %q", runtime)
	}
	if !def.Enabled {
		return "", nil, fmt.Errorf("runtime %q is disabled", runtime)
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
// This implements C-7 requirement 5: degraded operation always falls back to claude-code.
func (rl *RuntimeLauncher) FallbackToDefault() (string, []string) {
	def := rl.runtimes[rl.defaultRuntime]
	args := make([]string, len(def.Args))
	copy(args, def.Args)
	return def.Command, args
}
