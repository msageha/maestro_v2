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

	// codex: placeholder command pattern.
	rl.runtimes[model.RuntimeCodex] = RuntimeDef{
		Command: "codex",
		Args:    nil,
		EnvVars: nil,
		Enabled: false,
	}

	// gemini: placeholder command pattern.
	rl.runtimes[model.RuntimeGemini] = RuntimeDef{
		Command: "gemini",
		Args:    nil,
		EnvVars: nil,
		Enabled: false,
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
func (rl *RuntimeLauncher) GetCommand(runtime, runtimeModel string) (string, []string, error) {
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

	args := make([]string, len(def.Args))
	copy(args, def.Args)

	if runtimeModel != "" {
		args = append(args, "--model", runtimeModel)
	}

	return def.Command, args, nil
}

// FallbackToDefault returns the command and args for the default runtime (claude-code).
// This implements C-7 requirement 5: degraded operation always falls back to claude-code.
func (rl *RuntimeLauncher) FallbackToDefault() (string, []string) {
	def := rl.runtimes[rl.defaultRuntime]
	args := make([]string, len(def.Args))
	copy(args, def.Args)
	return def.Command, args
}
