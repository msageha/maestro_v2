package model

import "strings"

// Runtime name constants.
const (
	RuntimeClaudeCode = "claude-code"
	RuntimeCodex      = "codex"
	RuntimeGemini     = "gemini"
)

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
//   - "codex"       → (RuntimeCodex, "")          — codex CLI, runtime picks its default model
//   - "codex-*"     → (RuntimeCodex, modelName)   — codex CLI with an explicit model override
//     (e.g. "codex-5" for ChatGPT account users; symmetric with the gemini-*
//     handling so misconfigured names do not silently route to claude and
//     fail with an opaque "model not found" error)
//   - "gemini"      → (RuntimeGemini, "")         — gemini CLI, runtime picks its default model
//   - "gemini-*"    → (RuntimeGemini, modelName)  — gemini CLI with an explicit model override
//   - anything else → (RuntimeClaudeCode, modelName) — claude CLI (existing behavior)
func ParseRuntimeFromModel(modelName string) (runtime, effectiveModel string) {
	switch {
	case modelName == RuntimeCodex:
		return RuntimeCodex, ""
	case strings.HasPrefix(modelName, "codex-"):
		return RuntimeCodex, modelName
	case modelName == RuntimeGemini:
		return RuntimeGemini, ""
	case strings.HasPrefix(modelName, "gemini-"):
		return RuntimeGemini, modelName
	default:
		return RuntimeClaudeCode, modelName
	}
}

// Family returns the canonical family alias ("sonnet" / "opus" / "haiku")
// for a given model identifier. Short aliases pass through unchanged; full
// Claude model IDs such as "claude-opus-4-7" or "claude-haiku-4-5-20251001"
// are mapped to their family. Unknown names (codex / gemini / custom) are
// returned unchanged so they still compare equal to themselves.
//
// This is the canonical comparison granularity across the daemon and plan
// packages: worker eligibility matches by family, and the bandit's model
// arms and reward attribution are keyed by family so a config spelled with
// full IDs and a reward path that resolves an alias land on the same arm.
func Family(name string) string {
	switch name {
	case "", "sonnet", "opus", "haiku":
		return name
	}
	// Full Claude model IDs have the form "claude-<family>-<version>[-<suffix>]".
	if rest, ok := strings.CutPrefix(name, "claude-"); ok {
		family := rest
		if i := strings.IndexByte(rest, '-'); i >= 0 {
			family = rest[:i]
		}
		switch family {
		case "sonnet", "opus", "haiku":
			return family
		}
	}
	return name
}
