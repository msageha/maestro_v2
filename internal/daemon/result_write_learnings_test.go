package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// newSkillCandidatesTestAPI assembles the minimal ResultWriteAPI shape that
// writeSkillCandidates depends on (maestroDir + config + clock + lockMap +
// logFn) over a temp .maestro dir seeded with the given candidates.
func newSkillCandidatesTestAPI(t *testing.T, seed []model.SkillCandidate, logs *[]capturedLog) (*ResultWriteAPI, string) {
	t.Helper()
	maestroDir := filepath.Join(t.TempDir(), ".maestro")
	if err := os.MkdirAll(filepath.Join(maestroDir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	candidatesPath := filepath.Join(maestroDir, "state", "skill_candidates.yaml")
	if seed != nil {
		if err := skill.WriteCandidates(candidatesPath, seed); err != nil {
			t.Fatal(err)
		}
	}
	api := &ResultWriteAPI{
		apiContext: &apiContext{
			maestroDir: maestroDir,
			config:     &model.Config{},
			clock:      RealClock{},
			lockMap:    lock.NewMutexMap(),
			logFn:      captureLog(logs),
		},
	}
	return api, candidatesPath
}

// seedPendingCandidates builds n distinct pending candidates whose contents
// do not merge with each other (token overlap stays below the merge
// threshold).
func seedPendingCandidates(n int) []model.SkillCandidate {
	out := make([]model.SkillCandidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, model.SkillCandidate{
			ID:          fmt.Sprintf("skc_seed_%03d", i),
			Content:     fmt.Sprintf("distinct%03d alpha%03d beta%03d gamma%03d delta%03d", i, i, i, i, i),
			Occurrences: 1,
			CommandIDs:  []string{fmt.Sprintf("cmd_seed_%03d", i)},
			CreatedAt:   "2025-01-01T00:00:00Z",
			UpdatedAt:   "2025-01-01T00:00:00Z",
			Status:      "pending",
		})
	}
	return out
}

// TestWriteSkillCandidates_CapacityFullSkipsWithWarn asserts that when
// pending candidates alone fill the retention cap, a new registration is
// skipped with a WARN log (never silently dropped, never grown past the cap).
func TestWriteSkillCandidates_CapacityFullSkipsWithWarn(t *testing.T) {
	t.Parallel()
	var logs []capturedLog
	api, candidatesPath := newSkillCandidatesTestAPI(t, seedPendingCandidates(skill.MaxCandidates), &logs)

	err := api.writeSkillCandidates(ResultWriteParams{
		Reporter:        "worker1",
		TaskID:          "task_1",
		CommandID:       "cmd_new",
		SkillCandidates: []string{"a brand new pattern that has no room left in the ledger"},
	})
	if err != nil {
		t.Fatalf("writeSkillCandidates: %v", err)
	}

	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != skill.MaxCandidates {
		t.Errorf("candidate count = %d, want unchanged %d", len(candidates), skill.MaxCandidates)
	}
	for _, c := range candidates {
		if c.CommandIDs[0] == "cmd_new" {
			t.Errorf("new candidate was registered despite a full pending ledger: %+v", c)
		}
	}
	if !hasLogAt(logs, LogLevelWarn, "skill_candidate_skipped_capacity") {
		t.Errorf("expected WARN skill_candidate_skipped_capacity, got %+v", logs)
	}
}

// TestWriteSkillCandidates_PrunesTerminalToAdmitNew asserts that terminal
// (approved/rejected) entries are pruned oldest-first to make room for a new
// registration while pending entries survive.
func TestWriteSkillCandidates_PrunesTerminalToAdmitNew(t *testing.T) {
	t.Parallel()
	seed := seedPendingCandidates(skill.MaxCandidates)
	seed[0].Status = "rejected"
	seed[0].UpdatedAt = "2024-12-01T00:00:00Z"
	var logs []capturedLog
	api, candidatesPath := newSkillCandidatesTestAPI(t, seed, &logs)

	err := api.writeSkillCandidates(ResultWriteParams{
		Reporter:        "worker1",
		TaskID:          "task_1",
		CommandID:       "cmd_new",
		SkillCandidates: []string{"a brand new pattern that should evict the old rejected entry"},
	})
	if err != nil {
		t.Fatalf("writeSkillCandidates: %v", err)
	}

	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != skill.MaxCandidates {
		t.Fatalf("candidate count = %d, want %d", len(candidates), skill.MaxCandidates)
	}
	foundNew := false
	for _, c := range candidates {
		if c.ID == "skc_seed_000" {
			t.Errorf("terminal entry was not pruned: %+v", c)
		}
		if len(c.CommandIDs) > 0 && c.CommandIDs[0] == "cmd_new" {
			foundNew = true
		}
	}
	if !foundNew {
		t.Error("new candidate missing after terminal prune")
	}
	if hasLogAt(logs, LogLevelWarn, "skill_candidate_skipped_capacity") {
		t.Errorf("no capacity skip expected when a terminal entry can be pruned, got %+v", logs)
	}
}
