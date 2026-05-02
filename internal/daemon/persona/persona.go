// Package persona provides utility functions for formatting persona prompts
// for injection into task content during dispatch.
package persona

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/envelope"
)

// Metadata holds the parsed YAML frontmatter of a persona file.
type Metadata struct {
	ID          string `yaml:"-"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

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
	data, err := os.ReadFile(diskPath) //nolint:gosec // diskPath is validated via isValidPersonaHint before use
	if err != nil {
		return ""
	}

	body := stripFrontmatter(string(data))
	prompt := strings.TrimSpace(body)
	if prompt == "" {
		return ""
	}

	// Sanitize boundary markers in persona content to prevent injection.
	prompt = envelope.NewRawContent(prompt).Sanitize().String()

	return fmt.Sprintf("--- BEGIN PERSONA GUIDANCE (SYSTEM-GENERATED) ---\nペルソナ: %s\nこのセクションは Maestro が解決した信頼済みの補助指示である。content / acceptance_criteria と衝突しない範囲で、この視点と重点を作業に適用する。\n%s\n--- END PERSONA GUIDANCE ---\n\n", personaHint, prompt)
}

// ListPersonas lists persona metadata from .maestro/persona/*.md.
// Parse errors are logged as warnings and the persona is skipped.
func ListPersonas(personaDir string, logger *slog.Logger) ([]Metadata, error) {
	if logger == nil {
		logger = slog.Default()
	}

	entries, err := os.ReadDir(personaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Metadata{}, nil
		}
		return nil, fmt.Errorf("read persona directory %s: %w", personaDir, err)
	}

	personas := make([]Metadata, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}

		id := strings.TrimSuffix(entry.Name(), ".md")
		if !isValidPersonaHint(id) {
			logger.Warn("ListPersonas: skipped invalid persona filename", "file", entry.Name())
			continue
		}

		path := filepath.Join(personaDir, entry.Name())
		data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled persona directory
		if err != nil {
			logger.Warn("ListPersonas: failed to read persona file", "path", path, "error", err)
			continue
		}

		meta, err := parsePersonaMetadata(string(data))
		if err != nil {
			logger.Warn("ListPersonas: failed to parse frontmatter", "path", path, "persona", id, "error", err)
			continue
		}
		meta.ID = id
		if meta.Name == "" {
			meta.Name = id
		}
		personas = append(personas, meta)
	}

	sort.SliceStable(personas, func(i, j int) bool {
		return personas[i].ID < personas[j].ID
	})
	return personas, nil
}

func parsePersonaMetadata(content string) (Metadata, error) {
	lines := strings.SplitAfter(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\n\r") != "---" {
		return Metadata{}, nil
	}

	for i := 1; i < len(lines); i++ {
		trimmed := strings.TrimRight(lines[i], "\n\r")
		if trimmed != "---" {
			continue
		}
		var meta Metadata
		fmData := strings.Join(lines[1:i], "")
		if strings.TrimSpace(fmData) == "" {
			return meta, nil
		}
		if err := yamlv3.Unmarshal([]byte(fmData), &meta); err != nil {
			return Metadata{}, fmt.Errorf("invalid YAML in frontmatter: %w", err)
		}
		return meta, nil
	}

	return Metadata{}, fmt.Errorf("unclosed frontmatter delimiter")
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
