// Package skill provides functions for reading and formatting SKILL.md files
// from .maestro/skills/<role>/<name>/SKILL.md and .maestro/skills/share/<name>/SKILL.md
// for injection into task content.
package skill

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// Metadata holds the parsed YAML frontmatter of a SKILL.md file.
type Metadata struct {
	ID          string   `yaml:"-"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Version     string   `yaml:"version"`
	Tags        []string `yaml:"tags"`
	Priority    *int     `yaml:"priority"`
	Mode        string   `yaml:"mode"`
}

// EffectivePriority returns the priority value, defaulting to 100 if unset.
func (m Metadata) EffectivePriority() int {
	if m.Priority != nil {
		return *m.Priority
	}
	return model.DefaultPriority
}

// Content combines metadata with the body text of a skill file.
type Content struct {
	Metadata
	Body string
}

// FormatSkillSection formats multiple skills for injection into task content.
// Skills are included in their original order. If the total body exceeds
// maxBodyChars (measured in runes), lower-priority skills are dropped first.
// A maxBodyChars of 0 or negative means no limit.
func FormatSkillSection(skills []Content, maxBodyChars int) string {
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

	var retained []Content
	for i, s := range skills {
		if included[i] {
			retained = append(retained, s)
		}
	}

	return renderSkills(retained)
}

func renderSkills(skills []Content) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n--- BEGIN SKILLS GUIDANCE (SYSTEM-GENERATED) ---\n")
	for _, s := range skills {
		displayName := s.Name
		if displayName == "" {
			displayName = s.ID
		}
		fmt.Fprintf(&sb, "スキル: %s\n%s\n", displayName, s.Body)
	}
	sb.WriteString("--- END SKILLS GUIDANCE ---\n")
	return sb.String()
}

func totalRuneCount(skills []Content, included []bool) int {
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
func parseFrontmatter(content string) (Metadata, string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	// Enlarge buffer to handle long lines (1 MiB).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// Check for opening delimiter.
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return Metadata{}, "", fmt.Errorf("reading skill file: %w", err)
		}
		return Metadata{}, "", nil
	}
	firstLine := strings.TrimSpace(scanner.Text())
	if firstLine != "---" {
		// No frontmatter — entire content is body.
		return Metadata{}, content, nil
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
		return Metadata{}, "", fmt.Errorf("reading skill file: %w", err)
	}

	if !closed {
		return Metadata{}, "", fmt.Errorf("unclosed frontmatter delimiter")
	}

	var meta Metadata
	if len(fmLines) > 0 {
		fmData := strings.Join(fmLines, "\n")
		if err := yamlv3.Unmarshal([]byte(fmData), &meta); err != nil {
			return Metadata{}, "", fmt.Errorf("invalid YAML in frontmatter: %w", err)
		}
	}

	// Remaining lines form the body.
	var bodyLines []string
	for scanner.Scan() {
		bodyLines = append(bodyLines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return Metadata{}, "", fmt.Errorf("reading skill file: %w", err)
	}
	body := strings.Join(bodyLines, "\n")

	return meta, body, nil
}

// roleScopes returns the scope subdirectories searched for a role, in
// precedence order: the role-specific scope first, then the shared scope.
// An empty role searches only the shared scope, and "share" is not doubled.
func roleScopes(role string) []string {
	if role == "" || role == "share" {
		return []string{"share"}
	}
	return []string{role, "share"}
}

// ResolveSearchDirs builds the precedence-ordered skill search directory list
// from the configured skills.extra_dirs and the bundled skills directory.
// Relative extra_dirs entries are resolved against projectRoot. Entries that
// are empty, missing, or not directories are skipped with a WARN rather than
// an error so a stale config never stops the daemon. Earlier entries take
// precedence; the bundled directory is always last (lowest precedence).
func ResolveSearchDirs(extraDirs []string, projectRoot, bundledDir string, logger *slog.Logger) []string {
	if logger == nil {
		logger = slog.Default()
	}
	dirs := make([]string, 0, len(extraDirs)+1)
	seen := make(map[string]struct{}, len(extraDirs)+1)
	for _, entry := range extraDirs {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			logger.Warn("skills.extra_dirs: skipping empty entry")
			continue
		}
		path := trimmed
		if !filepath.IsAbs(path) {
			path = filepath.Join(projectRoot, path)
		}
		path = filepath.Clean(path)
		if _, dup := seen[path]; dup {
			logger.Warn("skills.extra_dirs: skipping duplicate entry", "path", path)
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			logger.Warn("skills.extra_dirs: skipping missing directory", "path", path, "error", err)
			continue
		}
		if !info.IsDir() {
			logger.Warn("skills.extra_dirs: skipping non-directory entry", "path", path)
			continue
		}
		seen[path] = struct{}{}
		dirs = append(dirs, path)
	}
	bundled := filepath.Clean(bundledDir)
	if _, dup := seen[bundled]; !dup {
		dirs = append(dirs, bundled)
	}
	return dirs
}

// skillPathCandidate is a concrete SKILL.md location considered during
// resolution, tagged with the scope ("<role>" or "share") it was found under.
type skillPathCandidate struct {
	path  string
	scope string
}

// warnShadowedCandidates emits a WARN for each same-scope candidate that also
// exists on disk but lost the precedence race to winnerPath. Cross-scope
// duplicates (a role-specific skill shadowing a shared one) are the documented
// fallback design, not a conflict, and are not reported.
func warnShadowedCandidates(logger *slog.Logger, skillName, scope, winnerPath string, rest []skillPathCandidate) {
	var shadowed []string
	for _, c := range rest {
		if c.scope != scope {
			continue
		}
		if _, err := os.Stat(c.path); err == nil {
			shadowed = append(shadowed, c.path)
		}
	}
	if len(shadowed) > 0 {
		logger.Warn("skill name conflict across skill source directories: highest-precedence copy wins",
			"skill", skillName, "scope", scope, "winner", winnerPath, "shadowed", shadowed)
	}
}

// ReadSkillWithRole reads a skill file from a single skills directory.
// It is a convenience wrapper around ReadSkillWithRoleDirs; see that function
// for the resolution rules.
func ReadSkillWithRole(skillsDir, skillName, role string) (Content, error) {
	return ReadSkillWithRoleDirs([]string{skillsDir}, skillName, role, nil)
}

// ReadSkillWithRoleDirs reads a skill file from an ordered list of skill
// source directories using role-based fallback:
//  1. <dir>/<role>/<skillName>/SKILL.md for each dir in order (directory name match)
//  2. <dir>/share/<skillName>/SKILL.md for each dir in order (shared directory name match)
//  3. Scan the same directories for a skill whose frontmatter "name" field
//     matches skillName (name-based fallback).
//
// The name-based fallback (step 3) allows callers to reference skills by
// their frontmatter name, which is what "maestro skill list" displays.
// Earlier directories take precedence within each scope, so project-supplied
// extra_dirs shadow the bundled catalog. When the same skill exists in more
// than one source directory at the same scope, the highest-precedence copy
// wins and the shadowed copies are reported with a WARN (never silently).
// Returns the first match found, or an error if none exist.
func ReadSkillWithRoleDirs(skillsDirs []string, skillName, role string, logger *slog.Logger) (Content, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if !validate.IsValidIdentifier(skillName) {
		return Content{}, fmt.Errorf("invalid skill name: %q", skillName)
	}

	// Fast path: try exact directory name match, role scope across all
	// source directories before the shared scope.
	var candidates []skillPathCandidate
	for _, scope := range roleScopes(role) {
		for _, dir := range skillsDirs {
			candidates = append(candidates, skillPathCandidate{
				path:  filepath.Join(dir, scope, skillName, "SKILL.md"),
				scope: scope,
			})
		}
	}

	for i, c := range candidates {
		data, err := os.ReadFile(c.path) //nolint:gosec // path is constructed from controlled skills directories
		if err != nil {
			continue
		}

		meta, body, err := parseFrontmatter(string(data))
		if err != nil {
			return Content{}, fmt.Errorf("parse skill %q frontmatter: %w", skillName, err)
		}

		warnShadowedCandidates(logger, skillName, c.scope, c.path, candidates[i+1:])

		meta.ID = skillName
		if meta.Name == "" {
			meta.Name = skillName
		}

		return Content{
			Metadata: meta,
			Body:     body,
		}, nil
	}

	// Slow path: scan directories and match by frontmatter name.
	if sc, err := readSkillByFrontmatterName(skillsDirs, skillName, role, logger); err == nil {
		return sc, nil
	}

	return Content{}, fmt.Errorf("read skill %q: %w", skillName, os.ErrNotExist)
}

// readSkillByFrontmatterName scans role-specific and shared directories across
// all source directories for a skill whose frontmatter "name" field matches
// the given skillName. Directories are scanned in precedence order (role scope
// across all sources first, then shared), so the first match is deterministic.
func readSkillByFrontmatterName(skillsDirs []string, skillName, role string, logger *slog.Logger) (Content, error) {
	if logger == nil {
		logger = slog.Default()
	}
	scopes := roleScopes(role)
	dirs := make([]string, 0, len(scopes)*len(skillsDirs))
	for _, scope := range scopes {
		for _, base := range skillsDirs {
			dirs = append(dirs, filepath.Join(base, scope))
		}
	}

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(dir, e.Name(), "SKILL.md")
			data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled skills directory
			if err != nil {
				continue
			}
			meta, body, err := parseFrontmatter(string(data))
			if err != nil {
				logger.Warn("readSkillByFrontmatterName: failed to parse frontmatter",
					"path", path, "skill", e.Name(), "error", err)
				continue
			}
			if meta.Name == skillName {
				meta.ID = e.Name()
				return Content{
					Metadata: meta,
					Body:     body,
				}, nil
			}
		}
	}

	return Content{}, fmt.Errorf("read skill %q: %w", skillName, os.ErrNotExist)
}

// ListSkillsWithRole lists skill metadata from a single skills directory.
// It is a convenience wrapper around ListSkillsWithRoleDirs; see that function
// for the listing rules.
func ListSkillsWithRole(skillsDir, role string, logger *slog.Logger) ([]Metadata, error) {
	return ListSkillsWithRoleDirs([]string{skillsDir}, role, logger)
}

// ListSkillsWithRoleDirs lists skill metadata for a given role scope across an
// ordered list of skill source directories. Shared skills (skills/share/) are
// NOT included unless role is "share". This is because shared skills are
// auto-injected at dispatch time and listing them alongside role-specific
// skills would be misleading.
//
// Earlier directories take precedence: when the same skill name exists in
// more than one source directory, the highest-precedence copy is listed and
// the shadowed copies are reported with a WARN (never silently). Parse errors
// are logged as warnings and the skill is skipped; a broken higher-precedence
// copy still blocks lower-precedence copies so that the listing matches what
// ReadSkillWithRoleDirs would resolve.
func ListSkillsWithRoleDirs(skillsDirs []string, role string, logger *slog.Logger) ([]Metadata, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if role == "" {
		return nil, nil
	}
	seen := make(map[string]string) // skill ID -> winning SKILL.md path
	var skills []Metadata

	// Scan only the role-specific scope in each source directory.
	// When role is "share", this naturally scans <dir>/share/.
	for _, base := range skillsDirs {
		dir := filepath.Join(base, role)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read skills directory %s: %w", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			path := filepath.Join(dir, name, "SKILL.md")
			if winner, ok := seen[name]; ok {
				if _, statErr := os.Stat(path); statErr == nil {
					logger.Warn("skill name conflict across skill source directories: highest-precedence copy wins",
						"skill", name, "scope", role, "winner", winner, "shadowed", path)
				}
				continue
			}
			// Read SKILL.md directly from the current directory instead of
			// calling ReadSkillWithRoleDirs which redundantly searches all fallback paths.
			data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled skills directory
			if err != nil {
				logger.Warn("ListSkillsWithRoleDirs: failed to read SKILL.md", "path", path, "error", err)
				continue
			}
			meta, _, err := parseFrontmatter(string(data))
			if err != nil {
				logger.Warn("ListSkillsWithRoleDirs: failed to parse frontmatter", "path", path, "skill", name, "error", err)
				// Mark as seen so a lower-precedence copy cannot silently win:
				// resolution via ReadSkillWithRoleDirs fails on the broken
				// higher-precedence copy, and the listing must match.
				seen[name] = path
				continue
			}
			meta.ID = name
			if meta.Name == "" {
				meta.Name = name
			}
			seen[name] = path
			skills = append(skills, meta)
		}
	}

	return skills, nil
}

// ReadAllSkillsForRole reads all skill contents from a single skills
// directory. It is a convenience wrapper around ReadAllSkillsForRoleDirs; see
// that function for the merge rules.
func ReadAllSkillsForRole(skillsDir, role string, logger *slog.Logger) ([]Content, error) {
	return ReadAllSkillsForRoleDirs([]string{skillsDir}, role, logger)
}

// ReadAllSkillsForRoleDirs reads all skill contents for a given role across an
// ordered list of skill source directories. It scans <dir>/<role>/ across all
// sources first, then <dir>/share/, returning the full Content (metadata +
// body) for each skill found. When duplicates exist, the role-specific version
// takes priority over shared, and within a scope earlier source directories
// take priority. Same-scope duplicates across source directories are
// precedence conflicts and are reported with a WARN (never silently);
// role-over-share shadowing is the documented fallback design and is not
// reported. Parse errors are logged as warnings and the skill is skipped.
func ReadAllSkillsForRoleDirs(skillsDirs []string, role string, logger *slog.Logger) ([]Content, error) {
	if logger == nil {
		logger = slog.Default()
	}
	type claim struct {
		path  string
		scope string
	}
	seen := make(map[string]claim)
	var skills []Content

	for _, scope := range roleScopes(role) {
		for _, base := range skillsDirs {
			dir := filepath.Join(base, scope)
			entries, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("read skills directory %s: %w", dir, err)
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				path := filepath.Join(dir, name, "SKILL.md")
				if prev, ok := seen[name]; ok {
					if prev.scope == scope {
						if _, statErr := os.Stat(path); statErr == nil {
							logger.Warn("skill name conflict across skill source directories: highest-precedence copy wins",
								"skill", name, "scope", scope, "winner", prev.path, "shadowed", path)
						}
					}
					continue
				}
				data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled skills directory
				if err != nil {
					logger.Warn("ReadAllSkillsForRoleDirs: failed to read SKILL.md", "path", path, "error", err)
					continue
				}
				meta, body, err := parseFrontmatter(string(data))
				if err != nil {
					logger.Warn("ReadAllSkillsForRoleDirs: failed to parse frontmatter", "path", path, "skill", name, "error", err)
					// Mark as seen even on parse failure to prevent shared fallback
					// for this skill name. This matches ReadSkillWithRoleDirs behavior
					// where a broken role-specific file does not fall through to share.
					seen[name] = claim{path: path, scope: scope}
					continue
				}
				meta.ID = name
				if meta.Name == "" {
					meta.Name = name
				}
				seen[name] = claim{path: path, scope: scope}
				skills = append(skills, Content{Metadata: meta, Body: body})
			}
		}
	}

	return skills, nil
}
