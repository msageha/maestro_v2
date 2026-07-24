package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

// StagingDirName is the directory under <maestro_dir>/state/ that holds
// approved-but-not-promoted skill drafts. Staging is the terminal write
// target of the daemon-side approve flow: promotion into a live skill
// library (skills.extra_dirs or templates/skills/) is a human git operation.
const StagingDirName = "skill_staging"

// StagingRoot returns the staging directory for the given maestro dir.
func StagingRoot(maestroDir string) string {
	return filepath.Join(maestroDir, "state", StagingDirName)
}

// ErrStagedSkillExists is returned by StageCandidate when the staging
// directory for the requested skill name already exists and belongs to a
// different candidate (its manifest names another candidate ID, or it has no
// manifest at all).
var ErrStagedSkillExists = errors.New("staged skill already exists")

// CandidateManifestName is the marker file written inside every staged skill
// directory recording the ID of the candidate the draft was generated from.
// It lets a re-run approve distinguish an orphan draft left by an approve
// that crashed between staging and the candidate-state write (resumable:
// same candidate ID, candidate still pending) from a draft belonging to a
// different candidate (duplicate). It deliberately lives next to SKILL.md —
// not in the frontmatter — so the skill-anatomy validator and promotion copy
// are unaffected.
const CandidateManifestName = ".candidate_id"

// StagedCandidateID reads the candidate manifest of an existing staged skill
// directory and returns the recorded candidate ID.
func StagedCandidateID(skillDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(skillDir, CandidateManifestName)) //nolint:gosec // skillDir is under the daemon-owned staging root
	if err != nil {
		return "", fmt.Errorf("read staged candidate manifest: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// StagingValidationError reports that the generated SKILL.md failed the
// skill-anatomy hard rules. The staged directory has already been cleaned up
// when this error is returned.
type StagingValidationError struct {
	Issues []ValidationIssue
}

func (e *StagingValidationError) Error() string {
	msgs := make([]string, 0, len(e.Issues))
	for _, i := range e.Issues {
		msgs = append(msgs, i.String())
	}
	return fmt.Sprintf("staged skill failed anatomy validation: %s", strings.Join(msgs, "; "))
}

// StagedSkill describes a successfully staged skill draft.
type StagedSkill struct {
	Name string
	// Path is the absolute path of the staged SKILL.md.
	Path string
	// Warnings are advisory anatomy findings (severity=warning). Hard-rule
	// violations never reach here — they abort staging via
	// StagingValidationError.
	Warnings []ValidationIssue
}

// stagedFrontmatter is the frontmatter emitted into a staged SKILL.md. It is
// marshalled with yaml.v3 (never string-concatenated) so operator-supplied
// descriptions cannot break the YAML structure.
type stagedFrontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Version     string   `yaml:"version"`
	Tags        []string `yaml:"tags"`
	Priority    int      `yaml:"priority"`
}

// maxDerivedDescriptionRunes bounds the auto-derived description length for
// readability; the anatomy byte cap (maxDescriptionBytes) is enforced on top.
const maxDerivedDescriptionRunes = 200

// DeriveDescription builds a frontmatter description from candidate content:
// the first non-empty line, stripped of markdown heading markers and length
// capped. Returns a generic fallback only when the content yields nothing.
func DeriveDescription(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "# "))
		if line == "" {
			continue
		}
		return capDescription(line)
	}
	return "skill-factory が完了タスクの反復パターンから生成した skill 草稿"
}

// capDescription truncates s to the derived-rune cap and the anatomy byte cap.
func capDescription(s string) string {
	if runes := []rune(s); len(runes) > maxDerivedDescriptionRunes {
		s = string(runes[:maxDerivedDescriptionRunes])
	}
	for len(s) > maxDescriptionBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// BuildStagedSkillMarkdown renders the complete SKILL.md draft for a
// candidate: full maestro frontmatter (name/description/version/tags/priority)
// plus a body that keeps the reported pattern verbatim and grounds it in the
// command IDs it was observed under.
func BuildStagedSkillMarkdown(name, description string, cand model.SkillCandidate) (string, error) {
	if strings.TrimSpace(description) == "" {
		description = DeriveDescription(cand.Content)
	}
	fm := stagedFrontmatter{
		Name:        name,
		Description: capDescription(description),
		Version:     "0.1.0",
		Tags:        []string{"skill-factory", "draft"},
		Priority:    model.DefaultPriority,
	}
	fmBytes, err := yamlv3.Marshal(fm)
	if err != nil {
		return "", fmt.Errorf("marshal staged skill frontmatter: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(fmBytes)
	sb.WriteString("---\n\n")
	fmt.Fprintf(&sb, "# %s\n\n", name)
	fmt.Fprintf(&sb, "> skill-factory が候補 %s から生成した草稿。昇格前に人間がレビュー・編集する。\n\n", cand.ID)
	sb.WriteString("## パターン（Worker 報告の原文）\n\n")
	sb.WriteString(strings.TrimSpace(cand.Content))
	sb.WriteString("\n\n")
	sb.WriteString("## 実例への接地（grounding）\n\n")
	sb.WriteString("このパターンは以下の完了 command の実タスク軌跡で観測された。昇格前に該当 command の results / audit を参照して手順の正しさを確認すること。\n\n")
	fmt.Fprintf(&sb, "- occurrences: %d\n", cand.Occurrences)
	fmt.Fprintf(&sb, "- command_ids: %s\n\n", strings.Join(cand.CommandIDs, ", "))
	sb.WriteString("## 検証（昇格前チェックリスト）\n\n")
	sb.WriteString("- [ ] 手順が上記 command の実タスク軌跡と一致することを確認した\n")
	sb.WriteString("- [ ] 既存 skill と重複しないことを確認した（`maestro skill list` / similar_skills 参照）\n")
	sb.WriteString("- [ ] 「ベストプラクティスに従う」式の generic な記述を具体手順に書き換えた\n")
	return sb.String(), nil
}

// StageCandidate writes the generated SKILL.md (plus the candidate manifest,
// see CandidateManifestName) into <stagingRoot>/<name>/ and runs the
// skill-anatomy validator on it. Hard-rule failures remove the staged
// directory and return a *StagingValidationError; advisory warnings are
// returned on the StagedSkill for the operator to review.
//
// An existing staged directory for the same name is handled by manifest:
// when it records this same candidate's ID the previous approve crashed
// before the candidate state was persisted, so the draft is regenerated in
// place (resume); any other directory returns ErrStagedSkillExists untouched.
// The caller is responsible for only staging pending candidates.
func StageCandidate(stagingRoot, name, description string, cand model.SkillCandidate) (StagedSkill, error) {
	markdown, err := BuildStagedSkillMarkdown(name, description, cand)
	if err != nil {
		return StagedSkill{}, err
	}

	if err := os.MkdirAll(stagingRoot, 0o750); err != nil {
		return StagedSkill{}, fmt.Errorf("create staging root: %w", err)
	}
	skillDir := filepath.Join(stagingRoot, name)

	// Build the draft in a temp dir and move it into place with a single
	// rename so skillDir either fully exists (manifest + SKILL.md) or not at
	// all: a crash mid-staging can never leave a manifest-less directory that
	// would wedge subsequent approves. The "." prefix cannot collide with a
	// real skill dir (names start with [a-z0-9]) and keeps the temp dir out
	// of the anatomy validator's SKILL.md walk; a stale temp dir left by a
	// crash is removed here on the next attempt.
	tmpDir := filepath.Join(stagingRoot, ".staging-"+name)
	if err := os.RemoveAll(tmpDir); err != nil {
		return StagedSkill{}, fmt.Errorf("clean stale staging temp dir: %w", err)
	}
	if err := os.Mkdir(tmpDir, 0o750); err != nil {
		return StagedSkill{}, fmt.Errorf("create staging temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }() // no-op once the rename succeeds
	if err := os.WriteFile(filepath.Join(tmpDir, CandidateManifestName), []byte(cand.ID+"\n"), 0o600); err != nil {
		return StagedSkill{}, fmt.Errorf("write staged candidate manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "SKILL.md"), []byte(markdown), 0o600); err != nil {
		return StagedSkill{}, fmt.Errorf("write staged SKILL.md: %w", err)
	}

	switch _, statErr := os.Lstat(skillDir); {
	case statErr == nil:
		stagedID, manifestErr := StagedCandidateID(skillDir)
		if manifestErr != nil || stagedID != cand.ID {
			return StagedSkill{}, fmt.Errorf("%w: %s", ErrStagedSkillExists, skillDir)
		}
		// Orphan draft from this same candidate: replace it (resume).
		if err := os.RemoveAll(skillDir); err != nil {
			return StagedSkill{}, fmt.Errorf("replace orphan staged draft: %w", err)
		}
	case !os.IsNotExist(statErr):
		return StagedSkill{}, fmt.Errorf("stat staged skill directory: %w", statErr)
	}
	if err := os.Rename(tmpDir, skillDir); err != nil {
		return StagedSkill{}, fmt.Errorf("move staged skill into place: %w", err)
	}

	cleanup := func() {
		_ = os.RemoveAll(skillDir)
	}

	skillPath := filepath.Join(skillDir, "SKILL.md")

	issues, err := ValidateSkillTree(os.DirFS(stagingRoot), ".")
	if err != nil {
		cleanup()
		return StagedSkill{}, fmt.Errorf("validate staged skill: %w", err)
	}

	var errorsFound, warnings []ValidationIssue
	for _, issue := range issues {
		if issue.SkillDir != name {
			continue // findings for other, previously staged drafts
		}
		if issue.Severity == SeverityError {
			errorsFound = append(errorsFound, issue)
		} else {
			warnings = append(warnings, issue)
		}
	}
	if len(errorsFound) > 0 {
		cleanup()
		return StagedSkill{}, &StagingValidationError{Issues: errorsFound}
	}

	return StagedSkill{Name: name, Path: skillPath, Warnings: warnings}, nil
}
