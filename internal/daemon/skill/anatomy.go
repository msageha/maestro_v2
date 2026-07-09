package skill

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Severity classifies a skill-anatomy validation issue.
type Severity string

const (
	// SeverityError marks a hard-rule violation. CI fails on any error.
	SeverityError Severity = "error"
	// SeverityWarning marks a recommended-convention gap. Reported but does
	// not fail CI.
	SeverityWarning Severity = "warning"
)

// maxDescriptionBytes is the frontmatter description length cap. Mirrors the
// addyosmani/agent-skills 1024-char ceiling; kept in bytes to bound the
// injected token cost regardless of script.
const maxDescriptionBytes = 1024

// ValidationIssue is a single finding from ValidateSkillTree.
type ValidationIssue struct {
	// SkillDir is the skill's directory path relative to the tree root
	// (e.g. "worker/tdd-red-green-refactor"), or the file path for
	// tree-level issues.
	SkillDir string
	Severity Severity
	// Rule is a stable token identifying which check fired.
	Rule string
	// Message is a human-readable explanation.
	Message string
}

func (i ValidationIssue) String() string {
	return fmt.Sprintf("[%s] %s (%s): %s", i.Severity, i.SkillDir, i.Rule, i.Message)
}

// recommendedSectionExemptSkills exempts a skill (by "<role>/<name>" key) from
// the recommended-section warnings, with a reason kept HERE rather than in the
// skill frontmatter so an author cannot self-exempt (issue #11). Exemption
// applies only to the advisory recommended-section check; it never suppresses
// a hard-rule error.
var recommendedSectionExemptSkills = map[string]string{
	// (none yet — existing skills predate the recommended-section convention
	//  and are migrated opportunistically; add entries here only with a
	//  documented reason, e.g. a skill whose guidance is a pure reference
	//  table with no procedural steps to rationalize.)
}

// recommendedSectionMarkers maps each recommended section to the substrings
// (case-insensitive) that count as "present". Both English and Japanese
// spellings are accepted so the check does not force a heading language.
var recommendedSectionMarkers = map[string][]string{
	"rationalizations": {"rationalization", "言い訳", "自己正当化"},
	"red_flags":        {"red flag", "危険信号", "レッドフラグ"},
	"verification":     {"verification", "検証", "完了チェック", "完了条件"},
}

// skillRefsRe extracts the referenced skill names from a `skill_refs: [...]`
// line in a skill body. It matches the array payload; individual quoted names
// are pulled out by refNameRe.
var skillRefsRe = regexp.MustCompile(`(?m)skill_refs:\s*\[([^\]]*)\]`)

// refNameRe pulls a quoted identifier out of a skill_refs array payload.
var refNameRe = regexp.MustCompile(`["']([a-z0-9][a-z0-9-]*)["']`)

// ValidateSkillTree walks a skill tree rooted at root within fsys (each skill
// lives at <role>/<name>/SKILL.md) and returns all anatomy findings. The
// returned slice is deterministically ordered (by skill dir, then rule). A
// non-nil error is returned only for I/O failures walking the tree, not for
// validation findings — inspect the issues' Severity for those.
func ValidateSkillTree(fsys fs.FS, root string) ([]ValidationIssue, error) {
	type parsed struct {
		dir  string // "<role>/<name>"
		name string
		meta Metadata
		body string
	}
	var skills []parsed
	// nameToDir maps a frontmatter name to the first dir that declared it, so
	// duplicates across roles are caught and cross-refs resolve by name.
	nameToDir := make(map[string]string)
	var issues []ValidationIssue

	walkErr := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		dir := path.Dir(rel) // "<role>/<name>"
		dirName := path.Base(dir)

		data, readErr := fs.ReadFile(fsys, p)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", p, readErr)
		}
		meta, body, parseErr := parseFrontmatter(string(data))
		if parseErr != nil {
			issues = append(issues, ValidationIssue{
				SkillDir: dir, Severity: SeverityError, Rule: "frontmatter_parse",
				Message: parseErr.Error(),
			})
			return nil
		}

		// Hard rules.
		if meta.Name == "" {
			issues = append(issues, ValidationIssue{dir, SeverityError, "frontmatter_name_required", "frontmatter must set a non-empty name"})
		} else if meta.Name != dirName {
			issues = append(issues, ValidationIssue{dir, SeverityError, "name_matches_dir",
				fmt.Sprintf("frontmatter name %q must equal the directory name %q", meta.Name, dirName)})
		}
		if strings.TrimSpace(meta.Description) == "" {
			issues = append(issues, ValidationIssue{dir, SeverityError, "description_required", "frontmatter must set a non-empty description"})
		} else if n := len(meta.Description); n > maxDescriptionBytes {
			issues = append(issues, ValidationIssue{dir, SeverityError, "description_length",
				fmt.Sprintf("description is %d bytes, exceeds the %d-byte cap", n, maxDescriptionBytes)})
		}
		if strings.TrimSpace(meta.Version) == "" {
			issues = append(issues, ValidationIssue{dir, SeverityError, "version_required", "frontmatter must set a version"})
		}
		if len(meta.Tags) == 0 {
			issues = append(issues, ValidationIssue{dir, SeverityError, "tags_required", "frontmatter must set at least one tag"})
		}
		if meta.Priority == nil {
			issues = append(issues, ValidationIssue{dir, SeverityError, "priority_required", "frontmatter must set a priority"})
		}
		if strings.TrimSpace(body) == "" {
			issues = append(issues, ValidationIssue{dir, SeverityError, "body_required", "skill body must not be empty"})
		}

		// Duplicate name detection (a name must resolve to exactly one skill).
		if meta.Name != "" {
			if prev, ok := nameToDir[meta.Name]; ok {
				issues = append(issues, ValidationIssue{dir, SeverityError, "name_unique",
					fmt.Sprintf("skill name %q is already declared by %q", meta.Name, prev)})
			} else {
				nameToDir[meta.Name] = dir
			}
		}

		skills = append(skills, parsed{dir: dir, name: meta.Name, meta: meta, body: body})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	// Cross-reference integrity: every skill_refs entry must resolve to a
	// known skill name. Runs after the full pass so forward references
	// (a skill referencing one defined later in the walk) are accepted.
	for _, s := range skills {
		for _, ref := range extractSkillRefs(s.body) {
			if _, ok := nameToDir[ref]; !ok {
				issues = append(issues, ValidationIssue{s.dir, SeverityError, "cross_ref_exists",
					fmt.Sprintf("skill_refs references %q which is not a known skill", ref)})
			}
		}
	}

	// Recommended-section advisory (does not fail CI).
	for _, s := range skills {
		if _, exempt := recommendedSectionExemptSkills[s.dir]; exempt {
			continue
		}
		if missing := missingRecommendedSections(s.body); len(missing) > 0 {
			issues = append(issues, ValidationIssue{s.dir, SeverityWarning, "recommended_sections",
				fmt.Sprintf("missing recommended sections: %s (see docs/skill-anatomy.md)", strings.Join(missing, ", "))})
		}
	}

	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].SkillDir != issues[j].SkillDir {
			return issues[i].SkillDir < issues[j].SkillDir
		}
		return issues[i].Rule < issues[j].Rule
	})
	return issues, nil
}

// extractSkillRefs returns the distinct skill names referenced via a
// `skill_refs: [...]` construct anywhere in body.
func extractSkillRefs(body string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, m := range skillRefsRe.FindAllStringSubmatch(body, -1) {
		for _, nm := range refNameRe.FindAllStringSubmatch(m[1], -1) {
			ref := nm[1]
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			out = append(out, ref)
		}
	}
	return out
}

// missingRecommendedSections returns the recommended sections absent from body.
func missingRecommendedSections(body string) []string {
	lower := strings.ToLower(body)
	var missing []string
	// Deterministic order.
	for _, section := range []string{"rationalizations", "red_flags", "verification"} {
		found := false
		for _, marker := range recommendedSectionMarkers[section] {
			if strings.Contains(lower, strings.ToLower(marker)) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, section)
		}
	}
	return missing
}
