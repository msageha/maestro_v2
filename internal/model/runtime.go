package model

import "strings"

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

// ParseRuntimeFromModel infers the agent runtime and effective model from a
// raw model string. This lets callers select a non-claude-code runtime simply
// by writing its name (or a gemini-prefixed model) in model/default_model/models
// config fields — no separate runtimes: section required.
//
// Rules:
//   - "codex"       → (RuntimeCodex, "")         — codex CLI, runtime picks its default model
//   - "gemini"      → (RuntimeGemini, "")         — gemini CLI, runtime picks its default model
//   - "gemini-*"    → (RuntimeGemini, modelName)  — gemini CLI with an explicit model override
//   - anything else → (RuntimeClaudeCode, modelName) — claude CLI (existing behavior)
func ParseRuntimeFromModel(modelName string) (runtime, effectiveModel string) {
	switch {
	case modelName == RuntimeCodex:
		return RuntimeCodex, ""
	case modelName == RuntimeGemini:
		return RuntimeGemini, ""
	case strings.HasPrefix(modelName, "gemini-"):
		return RuntimeGemini, modelName
	default:
		return RuntimeClaudeCode, modelName
	}
}

