package skill_test

import (
	"testing"
	"testing/fstest"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/templates"
)

// TestBundledSkillsPassAnatomy is the CI gate (issue #11): every bundled
// skill under templates/skills/ must satisfy the hard skill-anatomy rules.
// Recommended-section gaps are warnings and are logged, not failed.
func TestBundledSkillsPassAnatomy(t *testing.T) {
	issues, err := skill.ValidateSkillTree(templates.FS, "skills")
	if err != nil {
		t.Fatalf("ValidateSkillTree: %v", err)
	}

	var errors, warnings int
	for _, is := range issues {
		switch is.Severity {
		case skill.SeverityError:
			errors++
			t.Errorf("skill anatomy violation: %s", is)
		case skill.SeverityWarning:
			warnings++
			t.Logf("skill anatomy advisory: %s", is)
		}
	}
	if errors == 0 {
		t.Logf("bundled skills pass all hard anatomy rules (%d advisory warnings)", warnings)
	}
}

// TestValidateSkillTree_CatchesHardViolations pins each hard rule against a
// synthetic in-memory tree so the CI gate itself is verified — not just the
// bundled skills that happen to already pass.
func TestValidateSkillTree_CatchesHardViolations(t *testing.T) {
	good := "---\nname: good-skill\ndescription: a valid skill\nversion: \"1.0.0\"\ntags: [worker]\npriority: 20\n---\n\n## 手順\n\n本文。\n"

	fsys := fstest.MapFS{
		"skills/worker/good-skill/SKILL.md": {Data: []byte(good)},
		// name != dir
		"skills/worker/mismatch/SKILL.md": {Data: []byte("---\nname: wrong-name\ndescription: d\nversion: \"1.0.0\"\ntags: [worker]\npriority: 20\n---\n\nbody\n")},
		// missing description + no tags + no priority
		"skills/worker/bare/SKILL.md": {Data: []byte("---\nname: bare\nversion: \"1.0.0\"\n---\n\nbody\n")},
		// cross-ref to a non-existent skill
		"skills/planner/refs-ghost/SKILL.md": {Data: []byte("---\nname: refs-ghost\ndescription: d\nversion: \"1.0.0\"\ntags: [planner]\npriority: 20\n---\n\nexample:\n  skill_refs: [\"does-not-exist\"]\n")},
		// cross-ref to an existing skill (must NOT flag)
		"skills/planner/refs-ok/SKILL.md": {Data: []byte("---\nname: refs-ok\ndescription: d\nversion: \"1.0.0\"\ntags: [planner]\npriority: 20\n---\n\nexample:\n  skill_refs: [\"good-skill\"]\n")},
	}

	issues, err := skill.ValidateSkillTree(fsys, "skills")
	if err != nil {
		t.Fatalf("ValidateSkillTree: %v", err)
	}

	// Index errors by "dir/rule" for assertions.
	got := make(map[string]bool)
	for _, is := range issues {
		if is.Severity == skill.SeverityError {
			got[is.SkillDir+"/"+is.Rule] = true
		}
	}

	wantErrors := []string{
		"worker/mismatch/name_matches_dir",
		"worker/bare/description_required",
		"worker/bare/tags_required",
		"worker/bare/priority_required",
		"planner/refs-ghost/cross_ref_exists",
	}
	for _, w := range wantErrors {
		if !got[w] {
			t.Errorf("expected error %q, not found in %v", w, keys(got))
		}
	}

	// The valid cross-ref must not produce a cross_ref error.
	if got["planner/refs-ok/cross_ref_exists"] {
		t.Error("valid cross-ref to good-skill must not be flagged")
	}
}

// TestValidateSkillTree_DuplicateNames pins name uniqueness across roles.
func TestValidateSkillTree_DuplicateNames(t *testing.T) {
	body := "---\nname: dup\ndescription: d\nversion: \"1.0.0\"\ntags: [x]\npriority: 20\n---\n\nbody\n"
	fsys := fstest.MapFS{
		"skills/worker/dup/SKILL.md": {Data: []byte(body)},
		"skills/share/dup/SKILL.md":  {Data: []byte(body)},
	}
	issues, err := skill.ValidateSkillTree(fsys, "skills")
	if err != nil {
		t.Fatalf("ValidateSkillTree: %v", err)
	}
	found := false
	for _, is := range issues {
		if is.Rule == "name_unique" && is.Severity == skill.SeverityError {
			found = true
		}
	}
	if !found {
		t.Error("duplicate skill name across roles must produce a name_unique error")
	}
}

// TestValidateSkillTree_RecommendedSectionsWarn pins that the recommended
// sections are advisory: their absence warns, their presence clears it.
func TestValidateSkillTree_RecommendedSectionsWarn(t *testing.T) {
	without := "---\nname: plain\ndescription: d\nversion: \"1.0.0\"\ntags: [x]\npriority: 20\n---\n\n## 手順\n本文のみ。\n"
	with := "---\nname: full\ndescription: d\nversion: \"1.0.0\"\ntags: [x]\npriority: 20\n---\n\n## 手順\n本文。\n\n## よくある言い訳\n...\n\n## 危険信号\n...\n\n## 検証\n...\n"
	fsys := fstest.MapFS{
		"skills/worker/plain/SKILL.md": {Data: []byte(without)},
		"skills/worker/full/SKILL.md":  {Data: []byte(with)},
	}
	issues, err := skill.ValidateSkillTree(fsys, "skills")
	if err != nil {
		t.Fatalf("ValidateSkillTree: %v", err)
	}
	var plainWarned, fullWarned bool
	for _, is := range issues {
		if is.Rule != "recommended_sections" {
			continue
		}
		if is.Severity != skill.SeverityWarning {
			t.Errorf("recommended_sections must be a warning, got %s", is.Severity)
		}
		switch is.SkillDir {
		case "worker/plain":
			plainWarned = true
		case "worker/full":
			fullWarned = true
		}
	}
	if !plainWarned {
		t.Error("skill lacking recommended sections must warn")
	}
	if fullWarned {
		t.Error("skill with all recommended sections must not warn")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
