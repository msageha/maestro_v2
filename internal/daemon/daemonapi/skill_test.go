package daemonapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// newTestSkillHandler builds a Skill handler over a temp .maestro dir with
// the given pending candidates and returns (handler, maestroDir).
func newTestSkillHandler(t *testing.T, candidates []model.SkillCandidate, extraDirs []string) (*Skill, string) {
	t.Helper()
	projectRoot := t.TempDir()
	maestroDir := filepath.Join(projectRoot, ".maestro")
	if err := os.MkdirAll(filepath.Join(maestroDir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if candidates != nil {
		if err := skill.WriteCandidates(filepath.Join(maestroDir, "state", "skill_candidates.yaml"), candidates); err != nil {
			t.Fatal(err)
		}
	}
	h := NewSkill(maestroDir, func() []string { return extraDirs }, lock.NewMutexMap(), nil, nil)
	return h, maestroDir
}

func approveRequest(t *testing.T, params SkillApproveParams) *uds.Request {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return &uds.Request{Command: "skill_approve", Params: raw}
}

func decodeApproveResult(t *testing.T, resp *uds.Response) SkillApproveResult {
	t.Helper()
	var result SkillApproveResult
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("unmarshal approve result: %v", err)
	}
	return result
}

func pendingCandidate(id, content string, occurrences int) model.SkillCandidate {
	cmds := make([]string, 0, occurrences)
	for i := 0; i < occurrences; i++ {
		cmds = append(cmds, "cmd_"+string(rune('a'+i)))
	}
	return model.SkillCandidate{
		ID:          id,
		Content:     content,
		Occurrences: occurrences,
		CommandIDs:  cmds,
		Status:      "pending",
	}
}

func writeExistingSkill(t *testing.T, base, role, name, description, body string) {
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

func TestHandleApprove_StagesDraftWithoutTouchingLiveLibrary(t *testing.T) {
	t.Parallel()
	cand := pendingCandidate("skc_1", "go test cache invalidation\n\n1. run go test with -count=1\n2. rerun failures only", 2)
	h, maestroDir := newTestSkillHandler(t, []model.SkillCandidate{cand}, nil)

	resp := h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_1", SkillName: "go-test-cache-invalidation"}))
	if !resp.Success {
		t.Fatalf("expected success, got %+v", resp.Error)
	}
	result := decodeApproveResult(t, resp)

	// Draft lands in staging, with a project-root-relative path.
	wantRel := filepath.Join(".maestro", "state", "skill_staging", "go-test-cache-invalidation", "SKILL.md")
	if result.StagedPath != wantRel {
		t.Errorf("staged_path = %q, want %q", result.StagedPath, wantRel)
	}
	stagedAbs := filepath.Join(filepath.Dir(maestroDir), result.StagedPath)
	data, err := os.ReadFile(stagedAbs)
	if err != nil {
		t.Fatalf("staged draft missing: %v", err)
	}
	for _, want := range []string{"name: go-test-cache-invalidation", "version:", "tags:", "priority:", "cmd_a", "cmd_b"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("staged draft missing %q", want)
		}
	}
	if result.PromotionHint == "" {
		t.Error("expected a promotion hint for the operator")
	}

	// The live library must remain untouched.
	if _, err := os.Stat(filepath.Join(maestroDir, "skills")); !os.IsNotExist(err) {
		t.Errorf("live library was written during approve (stat err: %v)", err)
	}

	// Candidate is marked approved with staging metadata.
	candidates, err := skill.ReadCandidates(filepath.Join(maestroDir, "state", "skill_candidates.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if candidates[0].Status != "approved" || candidates[0].SkillName != "go-test-cache-invalidation" || candidates[0].StagedPath != wantRel {
		t.Errorf("candidate not updated with staging metadata: %+v", candidates[0])
	}
}

func TestHandleApprove_OccurrenceGate(t *testing.T) {
	t.Parallel()
	cand := pendingCandidate("skc_1", "single observation pattern with several concrete steps", 1)
	h, maestroDir := newTestSkillHandler(t, []model.SkillCandidate{cand}, nil)

	resp := h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_1", SkillName: "single-shot"}))
	if resp.Success {
		t.Fatal("expected occurrences<2 to be rejected without force")
	}
	if !strings.Contains(resp.Error.Message, "occurrences=1") {
		t.Errorf("expected occurrences gate message, got %q", resp.Error.Message)
	}

	// Force bypasses the value gate.
	resp = h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_1", SkillName: "single-shot", Force: true}))
	if !resp.Success {
		t.Fatalf("expected force to bypass occurrence gate, got %+v", resp.Error)
	}
	if _, err := os.Stat(filepath.Join(maestroDir, "state", "skill_staging", "single-shot", "SKILL.md")); err != nil {
		t.Errorf("forced approve did not stage: %v", err)
	}
}

func TestHandleApprove_NameCollisionWithLibrary(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	writeExistingSkill(t, extra, "worker", "taken-name", "an existing skill", "existing body about something else entirely unrelated topic")
	cand := pendingCandidate("skc_1", "brand new pattern with fresh unmatched vocabulary tokens", 2)
	h, _ := newTestSkillHandler(t, []model.SkillCandidate{cand}, []string{extra})

	resp := h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_1", SkillName: "taken-name", Force: true}))
	if resp.Success {
		t.Fatal("expected name collision to be rejected even with force")
	}
	if resp.Error.Code != uds.ErrCodeDuplicate || !strings.Contains(resp.Error.Message, "worker/taken-name") {
		t.Errorf("expected duplicate error referencing worker/taken-name, got %+v", resp.Error)
	}
}

func TestHandleApprove_SimilarSkillDedupGate(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	writeExistingSkill(t, extra, "worker", "cache-skill", "go test cache invalidation",
		"always run go test with -count=1 to invalidate the test cache before reporting results")
	cand := pendingCandidate("skc_1", "always run go test with -count=1 to invalidate the test cache before reporting", 2)
	h, _ := newTestSkillHandler(t, []model.SkillCandidate{cand}, []string{extra})

	resp := h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_1", SkillName: "new-cache-skill"}))
	if resp.Success {
		t.Fatal("expected similar existing skill to block approve without force")
	}
	if resp.Error.Code != uds.ErrCodeDuplicate || !strings.Contains(resp.Error.Message, "worker/cache-skill") {
		t.Errorf("expected dedup error referencing worker/cache-skill, got %+v", resp.Error)
	}

	// Force overrides the dedup gate; the similar skill is still reported.
	resp = h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_1", SkillName: "new-cache-skill", Force: true}))
	if !resp.Success {
		t.Fatalf("expected force to bypass dedup gate, got %+v", resp.Error)
	}
	result := decodeApproveResult(t, resp)
	if len(result.SimilarSkills) == 0 || result.SimilarSkills[0] != "worker/cache-skill" {
		t.Errorf("expected similar skills surfaced in result, got %v", result.SimilarSkills)
	}
}

func TestHandleApprove_StagedDuplicate(t *testing.T) {
	t.Parallel()
	cands := []model.SkillCandidate{
		pendingCandidate("skc_1", "pattern one with concrete steps and details", 2),
		pendingCandidate("skc_2", "a completely different second pattern about database migrations", 2),
	}
	h, _ := newTestSkillHandler(t, cands, nil)

	if resp := h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_1", SkillName: "same-name"})); !resp.Success {
		t.Fatalf("first approve failed: %+v", resp.Error)
	}
	resp := h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_2", SkillName: "same-name"}))
	if resp.Success {
		t.Fatal("expected staged-name duplicate to be rejected")
	}
	if resp.Error.Code != uds.ErrCodeDuplicate {
		t.Errorf("expected DUPLICATE code, got %+v", resp.Error)
	}
}

func TestHandleApprove_RetryAfterCandidateWriteFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: cannot make directories read-only")
	}
	cand := pendingCandidate("skc_1", "pattern one with concrete steps and details", 2)
	h, maestroDir := newTestSkillHandler(t, []model.SkillCandidate{cand}, nil)

	stateDir := filepath.Join(maestroDir, "state")
	stagingRoot := filepath.Join(stateDir, "skill_staging")
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make state/ read-only so WriteCandidates (atomic temp-file write inside
	// state/) fails while staging (inside the pre-created, still writable
	// skill_staging/) succeeds.
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(stateDir, 0o755); err != nil {
			t.Errorf("restore state dir permissions: %v", err)
		}
	})

	resp := h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_1", SkillName: "retry-skill"}))
	if resp.Success {
		t.Fatal("expected approve to fail when the candidate state cannot be written")
	}
	if resp.Error.Code != uds.ErrCodeInternal {
		t.Errorf("expected INTERNAL error code, got %+v", resp.Error)
	}
	// The staged draft must be rolled back; a leftover draft would wedge every
	// retry with a DUPLICATE error while the candidate is still pending.
	if _, err := os.Stat(filepath.Join(stagingRoot, "retry-skill")); !os.IsNotExist(err) {
		t.Fatalf("staged draft not rolled back after candidate write failure (stat err: %v)", err)
	}

	// Once the transient failure clears, the same approve must succeed.
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	resp = h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_1", SkillName: "retry-skill"}))
	if !resp.Success {
		t.Fatalf("retry after transient write failure should succeed, got %+v", resp.Error)
	}
	result := decodeApproveResult(t, resp)
	if _, err := os.Stat(filepath.Join(filepath.Dir(maestroDir), result.StagedPath)); err != nil {
		t.Errorf("staged draft missing after successful retry: %v", err)
	}
	candidates, err := skill.ReadCandidates(filepath.Join(stateDir, "skill_candidates.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if candidates[0].Status != "approved" {
		t.Errorf("candidate status = %q, want approved", candidates[0].Status)
	}
}

func TestHandleApprove_NonPendingAndMissing(t *testing.T) {
	t.Parallel()
	cand := pendingCandidate("skc_1", "some pattern content here", 2)
	cand.Status = "rejected"
	h, _ := newTestSkillHandler(t, []model.SkillCandidate{cand}, nil)

	resp := h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_1", SkillName: "x-name"}))
	if resp.Success || !strings.Contains(resp.Error.Message, "already rejected") {
		t.Errorf("expected already-rejected error, got %+v", resp)
	}

	resp = h.HandleApprove(approveRequest(t, SkillApproveParams{CandidateID: "skc_missing", SkillName: "x-name"}))
	if resp.Success || resp.Error.Code != uds.ErrCodeNotFound {
		t.Errorf("expected NOT_FOUND, got %+v", resp)
	}
}

func TestHandleReject_MarksRejected(t *testing.T) {
	t.Parallel()
	cand := pendingCandidate("skc_1", "some pattern content here", 1)
	h, maestroDir := newTestSkillHandler(t, []model.SkillCandidate{cand}, nil)

	raw, _ := json.Marshal(SkillRejectParams{CandidateID: "skc_1"})
	resp := h.HandleReject(&uds.Request{Command: "skill_reject", Params: raw})
	if !resp.Success {
		t.Fatalf("reject failed: %+v", resp.Error)
	}
	candidates, err := skill.ReadCandidates(filepath.Join(maestroDir, "state", "skill_candidates.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if candidates[0].Status != "rejected" {
		t.Errorf("expected rejected, got %q", candidates[0].Status)
	}
	if candidates[0].UpdatedAt == "" {
		t.Error("expected UpdatedAt set on reject")
	}
}
