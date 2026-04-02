// Package persona provides utility functions for formatting persona prompts
// for injection into task content during dispatch.
package persona

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FormatPersonaSection formats a persona prompt for injection into task content.
// The persona prompt is read from {maestroDir}/persona/{personaHint}.md.
func FormatPersonaSection(personaHint, maestroDir string) string {
	if personaHint == "" || maestroDir == "" {
		return ""
	}

	// Reject path traversal and unsafe characters in persona hint.
	if !isValidPersonaHint(personaHint) {
		return ""
	}

	diskPath := filepath.Join(maestroDir, "persona", personaHint+".md")
	data, err := os.ReadFile(diskPath)
	if err != nil {
		return ""
	}

	body := stripFrontmatter(string(data))
	prompt := strings.TrimSpace(body)
	if prompt == "" {
		return ""
	}

	return fmt.Sprintf("---\nペルソナ: %s\n%s\n---\n\n", personaHint, prompt)
}

// isValidPersonaHint checks that a persona hint is a safe identifier
// (no path traversal or special characters).
func isValidPersonaHint(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == '\x00' {
			return false
		}
	}
	return true
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
