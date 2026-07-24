package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenJaccard_IdenticalAndDisjoint(t *testing.T) {
	t.Parallel()
	if got := TokenJaccard("run go test with race flag", "run go test with race flag"); got != 1.0 {
		t.Errorf("identical strings: expected 1.0, got %f", got)
	}
	if got := TokenJaccard("alpha beta gamma", "delta epsilon zeta"); got != 0.0 {
		t.Errorf("disjoint strings: expected 0.0, got %f", got)
	}
	if got := TokenJaccard("", ""); got != 0.0 {
		t.Errorf("empty strings: expected 0.0, got %f", got)
	}
}

func TestTokenJaccard_CaseAndWhitespaceInsensitive(t *testing.T) {
	t.Parallel()
	if got := TokenJaccard("Run Go Test", "run   go\ntest"); got != 1.0 {
		t.Errorf("case/whitespace variants: expected 1.0, got %f", got)
	}
}

func TestTokenJaccard_JapaneseBigrams(t *testing.T) {
	t.Parallel()
	same := TokenJaccard("テスト実行前にキャッシュを無効化する", "テスト実行前にキャッシュを無効化する")
	if same != 1.0 {
		t.Errorf("identical Japanese: expected 1.0, got %f", same)
	}
	similar := TokenJaccard("テスト実行前にキャッシュを無効化する", "テスト実行の前にキャッシュを無効化すること")
	if similar <= 0.5 {
		t.Errorf("near-identical Japanese: expected > 0.5, got %f", similar)
	}
	different := TokenJaccard("テスト実行前にキャッシュを無効化する", "認証トークンの有効期限を延長した")
	if different >= similar {
		t.Errorf("unrelated Japanese (%f) should score below near-identical (%f)", different, similar)
	}
}

func TestTokenizeForSimilarity_LoneCJKUnigram(t *testing.T) {
	t.Parallel()
	tokens := tokenizeForSimilarity("a 語 b")
	if _, ok := tokens["語"]; !ok {
		t.Errorf("expected lone CJK char kept as unigram, got %v", tokens)
	}
	if _, ok := tokens["a"]; !ok {
		t.Errorf("expected ascii token 'a', got %v", tokens)
	}
}

func writeLibrarySkill(t *testing.T, base, role, name, description, body string) {
	t.Helper()
	dir := filepath.Join(base, role, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\nversion: \"1.0\"\ntags: [test]\npriority: 100\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListAllSkills_AllRolesAndPrecedence(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	bundled := t.TempDir()
	writeLibrarySkill(t, extra, "worker", "dup-skill", "extra copy", "extra body")
	writeLibrarySkill(t, bundled, "worker", "dup-skill", "bundled copy", "bundled body")
	writeLibrarySkill(t, bundled, "share", "shared-skill", "a shared skill", "shared body")
	writeLibrarySkill(t, bundled, "planner", "plan-skill", "a planner skill", "planner body")

	got := ListAllSkills([]string{extra, bundled}, nil)
	if len(got) != 3 {
		t.Fatalf("expected 3 skills (dup deduped), got %d: %+v", len(got), got)
	}
	byRef := make(map[string]LibrarySkill)
	for _, s := range got {
		byRef[s.Ref()] = s
	}
	if s, ok := byRef["worker/dup-skill"]; !ok || s.Description != "extra copy" {
		t.Errorf("expected extra-dir copy to win for worker/dup-skill, got %+v", s)
	}
	for _, ref := range []string{"share/shared-skill", "planner/plan-skill"} {
		if _, ok := byRef[ref]; !ok {
			t.Errorf("expected skill %s in listing", ref)
		}
	}
}

func TestListAllSkills_MissingDirAndBrokenSkill(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	writeLibrarySkill(t, base, "worker", "good-skill", "fine", "body")
	brokenDir := filepath.Join(base, "worker", "broken-skill")
	if err := os.MkdirAll(brokenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "SKILL.md"), []byte("---\nunclosed"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ListAllSkills([]string{filepath.Join(base, "does-not-exist"), base}, nil)
	if len(got) != 1 || got[0].Ref() != "worker/good-skill" {
		t.Fatalf("expected only worker/good-skill, got %+v", got)
	}
}

func TestFindSimilarSkills_MatchesAndOrders(t *testing.T) {
	t.Parallel()
	library := []LibrarySkill{
		{Content: Content{Metadata: Metadata{ID: "cache-invalidation", Name: "cache-invalidation", Description: "go test cache invalidation with count flag"}, Body: "always run go test with -count=1 to invalidate the test cache"}, Role: "worker"},
		{Content: Content{Metadata: Metadata{ID: "unrelated", Name: "unrelated", Description: "terraform state locking"}, Body: "use dynamodb table for terraform state locks"}, Role: "share"},
	}
	got := FindSimilarSkills("run go test with -count=1 to invalidate the go test cache", library, 0.5)
	if len(got) != 1 || got[0] != "worker/cache-invalidation" {
		t.Fatalf("expected [worker/cache-invalidation], got %v", got)
	}
	if got := FindSimilarSkills("completely different topic about css layout", library, 0.5); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", got)
	}
}
