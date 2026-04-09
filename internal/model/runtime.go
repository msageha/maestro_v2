package model

// Runtime name constants.
const (
	RuntimeClaudeCode = "claude-code"
	RuntimeCodex      = "codex"
	RuntimeGemini     = "gemini"
)

// ValidateRuntime reports whether s is a known runtime name.
func ValidateRuntime(s string) bool {
	switch s {
	case RuntimeClaudeCode, RuntimeCodex, RuntimeGemini:
		return true
	}
	return false
}

// DefaultRuntime returns the default runtime name.
func DefaultRuntime() string {
	return RuntimeClaudeCode
}

// RuntimeDefinition describes how to invoke a runtime.
type RuntimeDefinition struct {
	Name    string            `yaml:"name" json:"name"`
	Command string            `yaml:"command" json:"command"`
	Args    []string          `yaml:"args,omitempty" json:"args,omitempty"`
	EnvVars map[string]string `yaml:"env_vars,omitempty" json:"env_vars,omitempty"`
}
