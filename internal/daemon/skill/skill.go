// Package skill provides functions for reading and formatting SKILL.md files
// from .maestro/skills/<name>/SKILL.md for injection into task content.
package skill

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	yamlv3 "gopkg.in/yaml.v3"
)

const defaultPriority = 100

// SkillMetadata holds the parsed YAML frontmatter of a SKILL.md file.
type SkillMetadata struct {
	ID          string   `yaml:"-"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Version     string   `yaml:"version"`
	AppliesTo   []string `yaml:"applies_to"`
	Tags        []string `yaml:"tags"`
	Priority    *int     `yaml:"priority"`
	Mode        string   `yaml:"mode"`
}

// EffectivePriority returns the priority value, defaulting to 100 if unset.
func (m SkillMetadata) EffectivePriority() int {
	if m.Priority != nil {
		return *m.Priority
	}
	return defaultPriority
}

// SkillContent combines metadata with the body text of a skill file.
type SkillContent struct {
	SkillMetadata
	Body string
}

// ReadSkill reads .maestro/skills/<skillName>/SKILL.md, parses YAML frontmatter,
// and returns the skill content. Returns an error if the file does not exist
// or if the frontmatter is malformed.
func ReadSkill(skillsDir, skillName string) (SkillContent, error) {
	if !isValidIdentifier(skillName) {
		return SkillContent{}, fmt.Errorf("invalid skill name: %q", skillName)
	}

	path := filepath.Join(skillsDir, skillName, "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillContent{}, fmt.Errorf("read skill %q: %w", skillName, err)
	}

	meta, body, err := parseFrontmatter(string(data))
	if err != nil {
		return SkillContent{}, fmt.Errorf("parse skill %q frontmatter: %w", skillName, err)
	}

	meta.ID = skillName
	if meta.Name == "" {
		meta.Name = skillName
	}

	return SkillContent{
		SkillMetadata: meta,
		Body:          body,
	}, nil
}

// ListSkills lists all skill metadata from the given skills directory.
// Each subdirectory containing a SKILL.md is treated as a skill.
// Skills with parse errors are skipped.
func ListSkills(skillsDir string) ([]SkillMetadata, error) {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills directory: %w", err)
	}

	var skills []SkillMetadata
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		sc, err := ReadSkill(skillsDir, name)
		if err != nil {
			// Skip skills with errors (file missing, bad frontmatter, etc.)
			continue
		}
		skills = append(skills, sc.SkillMetadata)
	}

	return skills, nil
}

// FormatSkillSection formats multiple skills for injection into task content.
// Skills are included in their original order. If the total body exceeds
// maxBodyChars (measured in runes), lower-priority skills are dropped first.
// A maxBodyChars of 0 or negative means no limit.
func FormatSkillSection(skills []SkillContent, maxBodyChars int) string {
	if len(skills) == 0 {
		return ""
	}

	if maxBodyChars <= 0 {
		return renderSkills(skills)
	}

	// Build an index sorted by priority (higher number = lower priority = drop first).
	type indexed struct {
		origIdx  int
		priority int
	}
	indices := make([]indexed, len(skills))
	for i, s := range skills {
		indices[i] = indexed{origIdx: i, priority: s.EffectivePriority()}
	}
	// Sort by priority descending so we drop lowest-priority (highest number) first.
	sort.SliceStable(indices, func(i, j int) bool {
		return indices[i].priority > indices[j].priority
	})

	// Start with all skills included, drop from the end of the sorted list
	// (lowest priority) until we fit within budget.
	included := make([]bool, len(skills))
	for i := range included {
		included[i] = true
	}

	for _, idx := range indices {
		total := totalRuneCount(skills, included)
		if total <= maxBodyChars {
			break
		}
		included[idx.origIdx] = false
	}

	var retained []SkillContent
	for i, s := range skills {
		if included[i] {
			retained = append(retained, s)
		}
	}

	return renderSkills(retained)
}

func renderSkills(skills []SkillContent) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, s := range skills {
		displayName := s.Name
		if displayName == "" {
			displayName = s.ID
		}
		fmt.Fprintf(&sb, "\n\n---\nスキル: %s\n%s\n", displayName, s.Body)
	}
	return sb.String()
}

func totalRuneCount(skills []SkillContent, included []bool) int {
	total := 0
	for i, s := range skills {
		if !included[i] {
			continue
		}
		// Account for the wrapper text as well.
		displayName := s.Name
		if displayName == "" {
			displayName = s.ID
		}
		wrapper := fmt.Sprintf("\n\n---\nスキル: %s\n%s\n", displayName, s.Body)
		total += utf8.RuneCountInString(wrapper)
	}
	return total
}

// parseFrontmatter splits a document into YAML frontmatter and body.
// If no frontmatter delimiter is found, the entire content is treated as body
// with empty metadata. If an opening `---` is found without a closing one,
// or if the YAML is invalid, an error is returned.
//
// The closing delimiter must be exactly `---` at the start of the line
// (leading/trailing whitespace is trimmed). Indented `---` inside YAML
// block scalars could be misdetected; keep frontmatter simple key-value.
func parseFrontmatter(content string) (SkillMetadata, string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	// Enlarge buffer to handle long lines (1 MiB).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// Check for opening delimiter.
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return SkillMetadata{}, "", fmt.Errorf("reading skill file: %w", err)
		}
		return SkillMetadata{}, "", nil
	}
	firstLine := strings.TrimSpace(scanner.Text())
	if firstLine != "---" {
		// No frontmatter — entire content is body.
		return SkillMetadata{}, content, nil
	}

	// Collect frontmatter lines until closing `---`.
	var fmLines []string
	closed := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		fmLines = append(fmLines, line)
	}
	if err := scanner.Err(); err != nil {
		return SkillMetadata{}, "", fmt.Errorf("reading skill file: %w", err)
	}

	if !closed {
		return SkillMetadata{}, "", fmt.Errorf("unclosed frontmatter delimiter")
	}

	var meta SkillMetadata
	if len(fmLines) > 0 {
		fmData := strings.Join(fmLines, "\n")
		if err := yamlv3.Unmarshal([]byte(fmData), &meta); err != nil {
			return SkillMetadata{}, "", fmt.Errorf("invalid YAML in frontmatter: %w", err)
		}
	}

	// Remaining lines form the body.
	var bodyLines []string
	for scanner.Scan() {
		bodyLines = append(bodyLines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return SkillMetadata{}, "", fmt.Errorf("reading skill file: %w", err)
	}
	body := strings.Join(bodyLines, "\n")

	return meta, body, nil
}

// isValidIdentifier checks that a skill name is a safe directory name.
func isValidIdentifier(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == '\x00' {
			return false
		}
	}
	return true
}
