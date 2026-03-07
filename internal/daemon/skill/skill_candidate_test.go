package skill

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestReadCandidates_Normal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skill_candidates.yaml")

	want := []model.SkillCandidate{
		{ID: "sc1", Content: "always use gofmt", Occurrences: 2, CommandIDs: []string{"cmd1", "cmd2"}, Status: "pending"},
		{ID: "sc2", Content: "run tests first", Occurrences: 1, CommandIDs: []string{"cmd3"}, Status: "approved"},
	}
	if err := WriteCandidates(path, want); err != nil {
		t.Fatalf("WriteCandidates: %v", err)
	}

	got, err := ReadCandidates(path)
	if err != nil {
		t.Fatalf("ReadCandidates: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d candidates, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Content != want[i].Content || got[i].Occurrences != want[i].Occurrences || got[i].Status != want[i].Status {
			t.Errorf("candidate %d mismatch: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReadCandidates_NotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.yaml")
	got, err := ReadCandidates(path)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestWriteReadCandidates_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "candidates.yaml")

	candidates := []model.SkillCandidate{
		{ID: "c1", Content: "content1", Occurrences: 1, CommandIDs: []string{"cmd1"}, CreatedAt: "2025-01-01T00:00:00Z", UpdatedAt: "2025-01-01T00:00:00Z", Status: "pending"},
	}
	if err := WriteCandidates(path, candidates); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadCandidates(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0].Content != "content1" || got[0].Status != "pending" {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}
}

func TestAddOrUpdateCandidate_NewCandidate(t *testing.T) {
	var candidates []model.SkillCandidate
	seq := 0
	idFunc := func() (string, error) {
		seq++
		return fmt.Sprintf("id-%d", seq), nil
	}

	got, err := AddOrUpdateCandidate(candidates, "new skill content", "cmd1", "2025-01-01T00:00:00Z", idFunc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0].Occurrences != 1 {
		t.Errorf("expected Occurrences=1, got %d", got[0].Occurrences)
	}
	if got[0].Status != "pending" {
		t.Errorf("expected status 'pending', got %q", got[0].Status)
	}
	if got[0].ID != "id-1" {
		t.Errorf("expected ID 'id-1', got %q", got[0].ID)
	}
}

func TestAddOrUpdateCandidate_ExistingIncrement(t *testing.T) {
	candidates := []model.SkillCandidate{
		{ID: "sc1", Content: "reuse this", Occurrences: 1, CommandIDs: []string{"cmd1"}, Status: "pending"},
	}
	idFunc := func() (string, error) { return "unused", nil }

	got, err := AddOrUpdateCandidate(candidates, "reuse this", "cmd2", "2025-01-02T00:00:00Z", idFunc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0].Occurrences != 2 {
		t.Errorf("expected Occurrences=2, got %d", got[0].Occurrences)
	}
	if len(got[0].CommandIDs) != 2 || got[0].CommandIDs[1] != "cmd2" {
		t.Errorf("expected CommandIDs [cmd1, cmd2], got %v", got[0].CommandIDs)
	}
	if got[0].UpdatedAt != "2025-01-02T00:00:00Z" {
		t.Errorf("expected UpdatedAt updated, got %q", got[0].UpdatedAt)
	}
}

func TestAddOrUpdateCandidate_DuplicateCommandID(t *testing.T) {
	candidates := []model.SkillCandidate{
		{ID: "sc1", Content: "same content", Occurrences: 2, CommandIDs: []string{"cmd1", "cmd2"}, Status: "pending"},
	}
	idFunc := func() (string, error) { return "unused", nil }

	got, err := AddOrUpdateCandidate(candidates, "same content", "cmd1", "2025-01-03T00:00:00Z", idFunc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].Occurrences != 2 {
		t.Errorf("expected Occurrences unchanged at 2, got %d", got[0].Occurrences)
	}
	if len(got[0].CommandIDs) != 2 {
		t.Errorf("expected CommandIDs unchanged at 2, got %v", got[0].CommandIDs)
	}
}

func TestAddOrUpdateCandidate_EmptyContent(t *testing.T) {
	var candidates []model.SkillCandidate
	idFunc := func() (string, error) { return "unused", nil }

	got, err := AddOrUpdateCandidate(candidates, "  ", "cmd1", "2025-01-01T00:00:00Z", idFunc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no candidates for empty content, got %d", len(got))
	}
}

func TestAddOrUpdateCandidate_NonPendingSkipped(t *testing.T) {
	candidates := []model.SkillCandidate{
		{ID: "sc1", Content: "approved skill", Occurrences: 1, CommandIDs: []string{"cmd1"}, Status: "approved"},
	}
	idFunc := func() (string, error) { return "unused", nil }

	got, err := AddOrUpdateCandidate(candidates, "approved skill", "cmd2", "2025-01-02T00:00:00Z", idFunc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].Occurrences != 1 {
		t.Errorf("expected Occurrences unchanged for non-pending, got %d", got[0].Occurrences)
	}
	if len(got[0].CommandIDs) != 1 {
		t.Errorf("expected CommandIDs unchanged for non-pending, got %v", got[0].CommandIDs)
	}
}
