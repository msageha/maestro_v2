// Package persona provides utility functions for formatting persona prompts
// for injection into task content during dispatch.
package persona

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// FormatPersonaSectionWithFS formats a persona prompt for injection into task content,
// resolving file-backed personas from the provided filesystem.
// When a persona has File set, the file is read from fsys and its content
// (with YAML frontmatter stripped) is used as the prompt.
// If file reading fails, it falls back to the Prompt field.
// maestroDir, if non-empty, is checked first for persona files (e.g. .maestro/persona/),
// allowing user customization with fallback to the embedded template FS.
func FormatPersonaSectionWithFS(fsys fs.FS, personas map[string]model.PersonaConfig, personaHint, maestroDir string) (string, bool) {
	if personaHint == "" {
		return "", true
	}

	p, ok := personas[personaHint]
	if !ok {
		return "", false
	}

	prompt := resolvePrompt(fsys, p, maestroDir)
	if prompt == "" {
		return "", true
	}

	return fmt.Sprintf("---\nペルソナ: %s\n%s\n---\n\n", personaHint, prompt), true
}

// resolvePrompt returns the effective prompt text for a persona config.
// If File is set, it first tries reading from maestroDir (the .maestro directory),
// then falls back to the embedded template FS, and finally to the Prompt field.
func resolvePrompt(fsys fs.FS, p model.PersonaConfig, maestroDir string) string {
	if file := strings.TrimSpace(p.File); file != "" {
		if !fs.ValidPath(file) || !strings.HasPrefix(file, "persona/") {
			return strings.TrimSpace(p.Prompt)
		}

		// Try .maestro/ directory first (user customization)
		if maestroDir != "" {
			diskPath := filepath.Join(maestroDir, filepath.FromSlash(file))
			if data, err := os.ReadFile(diskPath); err == nil {
				body := stripFrontmatter(string(data))
				if trimmed := strings.TrimSpace(body); trimmed != "" {
					return trimmed
				}
			}
		}

		// Fallback to embedded template FS
		if fsys != nil {
			data, err := fs.ReadFile(fsys, file)
			if err == nil {
				body := stripFrontmatter(string(data))
				if trimmed := strings.TrimSpace(body); trimmed != "" {
					return trimmed
				}
			}
		}
		// Fall back to inline Prompt on file read failure or empty body
	}
	return strings.TrimSpace(p.Prompt)
}

// stripFrontmatter removes YAML frontmatter (delimited by --- on its own line)
// from the beginning of content. If no valid frontmatter is found, returns
// the original content unchanged.
func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}
	// Split into lines and find the closing --- (exact match on its own line)
	lines := strings.SplitAfter(content, "\n")
	// Skip the first line (opening ---)
	for i := 1; i < len(lines); i++ {
		trimmed := strings.TrimRight(lines[i], "\n\r")
		if trimmed == "---" {
			// Return everything after the closing fence
			return strings.Join(lines[i+1:], "")
		}
	}
	// No valid closing fence found
	return content
}
