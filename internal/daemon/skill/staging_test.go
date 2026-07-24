package skill

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func testCandidate() model.SkillCandidate {
	return model.SkillCandidate{
		ID:          "skc_1_abc",
		Content:     "go test のキャッシュ無効化\n\n1. `go test ./... -count=1` で実行する\n2. 失敗したパッケージのみ `-run` で再実行する",
		Occurrences: 3,
		CommandIDs:  []string{"cmd_1", "cmd_2", "cmd_3"},
		Status:      "pending",
	}
}

func TestDeriveDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"first line", "short title\nrest of body", "short title"},
		{"heading stripped", "## Heading Title\nbody", "Heading Title"},
		{"skips empty lines", "\n\n  \nactual line", "actual line"},
		{"empty falls back", "   \n  ", "skill-factory が完了タスクの反復パターンから生成した skill 草稿"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveDescription(tt.content); got != tt.want {
				t.Errorf("DeriveDescription(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestDeriveDescription_LongLineCapped(t *testing.T) {
	t.Parallel()
	got := DeriveDescription(strings.Repeat("あ", 500))
	if runes := []rune(got); len(runes) != maxDerivedDescriptionRunes {
		t.Errorf("expected %d runes, got %d", maxDerivedDescriptionRunes, len(runes))
	}
	if len(got) > maxDescriptionBytes {
		t.Errorf("derived description exceeds anatomy byte cap: %d bytes", len(got))
	}
}

func TestBuildStagedSkillMarkdown_FrontmatterAndGrounding(t *testing.T) {
	t.Parallel()
	md, err := BuildStagedSkillMarkdown("go-test-cache-invalidation", "", testCandidate())
	if err != nil {
		t.Fatalf("BuildStagedSkillMarkdown: %v", err)
	}
	meta, body, err := parseFrontmatter(md)
	if err != nil {
		t.Fatalf("generated markdown has invalid frontmatter: %v", err)
	}
	if meta.Name != "go-test-cache-invalidation" {
		t.Errorf("name = %q", meta.Name)
	}
	if meta.Description == "" || meta.Version == "" || len(meta.Tags) == 0 || meta.Priority == nil {
		t.Errorf("frontmatter incomplete: %+v", meta)
	}
	for _, want := range []string{"cmd_1", "cmd_2", "cmd_3", "occurrences: 3", "skc_1_abc", "go test ./... -count=1"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing grounding element %q", want)
		}
	}
}

func TestBuildStagedSkillMarkdown_DescriptionYAMLSafe(t *testing.T) {
	t.Parallel()
	// A description containing YAML metacharacters must not break parsing.
	md, err := BuildStagedSkillMarkdown("tricky-name", `desc: with "quotes" and: colons`, testCandidate())
	if err != nil {
		t.Fatalf("BuildStagedSkillMarkdown: %v", err)
	}
	meta, _, err := parseFrontmatter(md)
	if err != nil {
		t.Fatalf("frontmatter with metacharacters failed to parse: %v", err)
	}
	if !strings.Contains(meta.Description, "quotes") {
		t.Errorf("description lost content: %q", meta.Description)
	}
}

func TestStageCandidate_SuccessPassesAnatomyValidator(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "skill_staging")

	staged, err := StageCandidate(root, "go-test-cache-invalidation", "", testCandidate())
	if err != nil {
		t.Fatalf("StageCandidate: %v", err)
	}
	if staged.Path != filepath.Join(root, "go-test-cache-invalidation", "SKILL.md") {
		t.Errorf("unexpected staged path: %s", staged.Path)
	}
	if _, err := os.Stat(staged.Path); err != nil {
		t.Fatalf("staged file missing: %v", err)
	}
	// Hard rules must pass; only advisory warnings are allowed.
	issues, err := ValidateSkillTree(os.DirFS(root), ".")
	if err != nil {
		t.Fatalf("ValidateSkillTree: %v", err)
	}
	for _, issue := range issues {
		if issue.Severity == SeverityError {
			t.Errorf("staged draft has hard-rule violation: %s", issue)
		}
	}
	for _, w := range staged.Warnings {
		if w.Severity != SeverityWarning {
			t.Errorf("Warnings must only carry advisory findings, got %s", w)
		}
	}
}

func TestStageCandidate_DuplicateNameFromOtherCandidate(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "skill_staging")
	if _, err := StageCandidate(root, "dup-name", "", testCandidate()); err != nil {
		t.Fatalf("first StageCandidate: %v", err)
	}
	other := testCandidate()
	other.ID = "skc_2_other"
	_, err := StageCandidate(root, "dup-name", "", other)
	if !errors.Is(err, ErrStagedSkillExists) {
		t.Fatalf("expected ErrStagedSkillExists for a different candidate, got %v", err)
	}
}

func TestStageCandidate_WritesCandidateManifest(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "skill_staging")
	cand := testCandidate()
	if _, err := StageCandidate(root, "with-manifest", "", cand); err != nil {
		t.Fatalf("StageCandidate: %v", err)
	}
	id, err := StagedCandidateID(filepath.Join(root, "with-manifest"))
	if err != nil {
		t.Fatalf("StagedCandidateID: %v", err)
	}
	if id != cand.ID {
		t.Errorf("manifest candidate ID = %q, want %q", id, cand.ID)
	}
}

func TestStageCandidate_ResumesOwnOrphanDraft(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "skill_staging")
	cand := testCandidate()
	// First staging succeeded but the approve "crashed" before the candidate
	// state write: the staged dir (with manifest) is left behind. Re-running
	// StageCandidate for the same candidate must resume, not wedge.
	if _, err := StageCandidate(root, "resume-name", "", cand); err != nil {
		t.Fatalf("first StageCandidate: %v", err)
	}
	staged, err := StageCandidate(root, "resume-name", "", cand)
	if err != nil {
		t.Fatalf("resume StageCandidate: %v", err)
	}
	if _, err := os.Stat(staged.Path); err != nil {
		t.Fatalf("resumed staged file missing: %v", err)
	}
	id, err := StagedCandidateID(filepath.Join(root, "resume-name"))
	if err != nil || id != cand.ID {
		t.Errorf("resumed manifest = (%q, %v), want (%q, nil)", id, err, cand.ID)
	}
}

func TestStageCandidate_ManifestlessDirRejected(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "skill_staging")
	// A directory without a candidate manifest (operator-created, or from a
	// pre-manifest version) must be treated as a foreign draft.
	dir := filepath.Join(root, "manual-draft")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("manual"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := StageCandidate(root, "manual-draft", "", testCandidate())
	if !errors.Is(err, ErrStagedSkillExists) {
		t.Fatalf("expected ErrStagedSkillExists for manifest-less dir, got %v", err)
	}
	// The manual draft must be left untouched.
	data, readErr := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if readErr != nil || string(data) != "manual" {
		t.Errorf("manual draft modified: (%q, %v)", data, readErr)
	}
}

func TestStageCandidate_StaleTempDirCleaned(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "skill_staging")
	// A crash between temp-dir creation and rename leaves .staging-<name>
	// behind; the next attempt must clean it and still succeed.
	stale := filepath.Join(root, ".staging-fresh-name")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "SKILL.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := StageCandidate(root, "fresh-name", "", testCandidate()); err != nil {
		t.Fatalf("StageCandidate with stale temp dir: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temp dir not cleaned (stat err: %v)", err)
	}
}

func TestStageCandidate_ValidationFailureCleansUp(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "skill_staging")
	cand := testCandidate()
	// A skill_refs reference to an unknown skill violates the
	// cross_ref_exists hard rule inside the staged tree.
	cand.Content = "pattern that references skill_refs: [\"no-such-skill\"] in its steps"

	_, err := StageCandidate(root, "bad-cross-ref", "x", cand)
	var valErr *StagingValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected StagingValidationError, got %v", err)
	}
	if len(valErr.Issues) == 0 || valErr.Issues[0].Rule != "cross_ref_exists" {
		t.Errorf("expected cross_ref_exists issue, got %+v", valErr.Issues)
	}
	if _, statErr := os.Stat(filepath.Join(root, "bad-cross-ref")); !os.IsNotExist(statErr) {
		t.Errorf("expected staged dir cleaned up after validation failure, stat err: %v", statErr)
	}
}

func TestStageCandidate_IgnoresOtherStagedDrafts(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "skill_staging")
	// Pre-existing broken draft must not block staging a new, valid one.
	brokenDir := filepath.Join(root, "broken-draft")
	if err := os.MkdirAll(brokenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "SKILL.md"), []byte("---\nname: mismatch\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := StageCandidate(root, "fresh-draft", "", testCandidate()); err != nil {
		t.Fatalf("StageCandidate with unrelated broken draft present: %v", err)
	}
}
