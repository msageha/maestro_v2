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

