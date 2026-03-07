// Package personas provides utility functions for resolving persona hints
// and formatting persona prompts for injection into task content.
package personas

import (
	"fmt"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// ResolvePersonaSection looks up the persona_hint in the config's Personas map
// and returns a formatted section to append to task content.
// Returns ("", false) if persona_hint is empty, the persona is not found,
// or the Personas map is nil/empty.
func ResolvePersonaSection(personaHint string, personas map[string]model.PersonaConfig) (string, bool) {
	if personaHint == "" {
		return "", false
	}
	if len(personas) == 0 {
		return "", false
	}
	p, ok := personas[personaHint]
	if !ok {
		return "", false
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return "", false
	}
	return FormatPersonaSection(p.Prompt), true
}

// FormatPersonaSection formats a persona prompt for injection into task content.
func FormatPersonaSection(prompt string) string {
	var sb strings.Builder
	sb.WriteString("\n\n---\n")
	fmt.Fprintf(&sb, "ペルソナ指示:\n%s\n", prompt)
	return sb.String()
}
