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
	Prompt string // Ignored for claude-code; prompts are passed by buildLaunchArgs.
}

// RuntimeLauncher resolves the supported managed-agent runtime command.
// Managed roles currently support claude-code only; non-claude runtimes are
// rejected by config validation and again by Launch as defense in depth.
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
	if runtime == model.RuntimeClaudeCode && opts.Model != "" {
		args = append(args, "--model", opts.Model)
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
