// Package persona provides utility functions for formatting persona prompts
// for injection into task content during dispatch.
package persona

import (
	"fmt"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// FormatPersonaSection formats a persona prompt for injection into task content.
// Returns the formatted section and true if the persona was found,
// empty string and true if personaHint is empty (no injection needed),
// or empty string and false if the persona was not found in the config.
func FormatPersonaSection(personas map[string]model.PersonaConfig, personaHint string) (string, bool) {
	if personaHint == "" {
		return "", true
	}

	p, ok := personas[personaHint]
	if !ok {
		return "", false
	}

	prompt := strings.TrimSpace(p.Prompt)
	if prompt == "" {
		return "", true
	}

	return fmt.Sprintf("---\nペルソナ: %s\n%s\n---\n\n", personaHint, prompt), true
}
