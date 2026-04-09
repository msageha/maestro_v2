package reviewer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordResultAndGetModelStats(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tracker, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker: %v", err)
	}

	result := ReviewResult{
		ReviewerModel: "opus",
		TaskID:        "task_1",
		CommandID:     "cmd_1",
		FindingIDs:    []string{"f1", "f2", "f3"},
	}
	if err := tracker.RecordResult(result, []string{"f1", "f3"}); err != nil {
		t.Fatalf("RecordResult: %v", err)
	}

	stats := tracker.GetModelStats("opus")
	if stats.Model != "opus" {
		t.Errorf("Model: got %q, want %q", stats.Model, "opus")
	}
	if stats.TotalFindings != 3 {
		t.Errorf("TotalFindings: got %d, want 3", stats.TotalFindings)
	}
	if stats.AdoptedFindings != 2 {
		t.Errorf("AdoptedFindings: got %d, want 2", stats.AdoptedFindings)
	}
	if stats.ReviewCount != 1 {
		t.Errorf("ReviewCount: got %d, want 1", stats.ReviewCount)
	}
	wantRate := 2.0 / 3.0
	if stats.AdoptionRate < wantRate-0.001 || stats.AdoptionRate > wantRate+0.001 {
		t.Errorf("AdoptionRate: got %f, want ~%f", stats.AdoptionRate, wantRate)
	}
}

func TestMultipleModelsStatsSeparation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tracker, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker: %v", err)
	}

	// Record for opus
	if err := tracker.RecordResult(ReviewResult{
		ReviewerModel: "opus",
		TaskID:        "t1",
		CommandID:     "c1",
		FindingIDs:    []string{"f1", "f2"},
	}, []string{"f1"}); err != nil {
		t.Fatal(err)
	}

	// Record for sonnet
	if err := tracker.RecordResult(ReviewResult{
		ReviewerModel: "sonnet",
		TaskID:        "t2",
		CommandID:     "c1",
		FindingIDs:    []string{"f3", "f4", "f5"},
	}, []string{"f3", "f4", "f5"}); err != nil {
		t.Fatal(err)
	}

	opusStats := tracker.GetModelStats("opus")
	if opusStats.TotalFindings != 2 || opusStats.AdoptedFindings != 1 {
		t.Errorf("opus: total=%d adopted=%d, want 2/1", opusStats.TotalFindings, opusStats.AdoptedFindings)
	}

	sonnetStats := tracker.GetModelStats("sonnet")
	if sonnetStats.TotalFindings != 3 || sonnetStats.AdoptedFindings != 3 {
		t.Errorf("sonnet: total=%d adopted=%d, want 3/3", sonnetStats.TotalFindings, sonnetStats.AdoptedFindings)
	}
	if sonnetStats.AdoptionRate != 1.0 {
		t.Errorf("sonnet AdoptionRate: got %f, want 1.0", sonnetStats.AdoptionRate)
	}
}

func TestAdoptionRateZeroFindings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tracker, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker: %v", err)
	}

	// Record with zero findings
	if err := tracker.RecordResult(ReviewResult{
		ReviewerModel: "haiku",
		TaskID:        "t1",
		CommandID:     "c1",
		FindingIDs:    []string{},
	}, []string{}); err != nil {
		t.Fatal(err)
	}

	stats := tracker.GetModelStats("haiku")
	if stats.AdoptionRate != 0 {
		t.Errorf("AdoptionRate with zero findings: got %f, want 0", stats.AdoptionRate)
	}
	if stats.ReviewCount != 1 {
		t.Errorf("ReviewCount: got %d, want 1", stats.ReviewCount)
	}
}

func TestAdoptionRateAllAdopted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tracker, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker: %v", err)
	}

	if err := tracker.RecordResult(ReviewResult{
		ReviewerModel: "opus",
		TaskID:        "t1",
		CommandID:     "c1",
		FindingIDs:    []string{"f1", "f2", "f3"},
	}, []string{"f1", "f2", "f3"}); err != nil {
		t.Fatal(err)
	}

	stats := tracker.GetModelStats("opus")
	if stats.AdoptionRate != 1.0 {
		t.Errorf("AdoptionRate: got %f, want 1.0", stats.AdoptionRate)
	}
}

func TestAdoptionRatePartialAdopted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tracker, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker: %v", err)
	}

	if err := tracker.RecordResult(ReviewResult{
		ReviewerModel: "sonnet",
		TaskID:        "t1",
		CommandID:     "c1",
		FindingIDs:    []string{"f1", "f2", "f3", "f4"},
	}, []string{"f2", "f4"}); err != nil {
		t.Fatal(err)
	}

	stats := tracker.GetModelStats("sonnet")
	if stats.AdoptionRate != 0.5 {
		t.Errorf("AdoptionRate: got %f, want 0.5", stats.AdoptionRate)
	}
}

func TestJSONLPersistenceAndReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create tracker and record data
	tracker1, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker: %v", err)
	}

	if err := tracker1.RecordResult(ReviewResult{
		ReviewerModel: "opus",
		TaskID:        "t1",
		CommandID:     "c1",
		FindingIDs:    []string{"f1", "f2"},
	}, []string{"f1"}); err != nil {
		t.Fatal(err)
	}

	if err := tracker1.RecordResult(ReviewResult{
		ReviewerModel: "sonnet",
		TaskID:        "t2",
		CommandID:     "c1",
		FindingIDs:    []string{"f3"},
	}, []string{}); err != nil {
		t.Fatal(err)
	}

	// Verify JSONL file exists
	jsonlPath := filepath.Join(dir, "reviewer_usefulness.jsonl")
	if _, err := os.Stat(jsonlPath); err != nil {
		t.Fatalf("JSONL file should exist: %v", err)
	}

	// Create a new tracker from the same directory — should reload
	tracker2, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker reload: %v", err)
	}

	opusStats := tracker2.GetModelStats("opus")
	if opusStats.TotalFindings != 2 || opusStats.AdoptedFindings != 1 {
		t.Errorf("reloaded opus: total=%d adopted=%d, want 2/1", opusStats.TotalFindings, opusStats.AdoptedFindings)
	}

	sonnetStats := tracker2.GetModelStats("sonnet")
	if sonnetStats.TotalFindings != 1 || sonnetStats.AdoptedFindings != 0 {
		t.Errorf("reloaded sonnet: total=%d adopted=%d, want 1/0", sonnetStats.TotalFindings, sonnetStats.AdoptedFindings)
	}
}

func TestGetAllModelStats(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tracker, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker: %v", err)
	}

	if err := tracker.RecordResult(ReviewResult{
		ReviewerModel: "opus",
		TaskID:        "t1",
		CommandID:     "c1",
		FindingIDs:    []string{"f1"},
	}, []string{"f1"}); err != nil {
		t.Fatal(err)
	}

	if err := tracker.RecordResult(ReviewResult{
		ReviewerModel: "sonnet",
		TaskID:        "t2",
		CommandID:     "c1",
		FindingIDs:    []string{"f2", "f3"},
	}, []string{"f2"}); err != nil {
		t.Fatal(err)
	}

	allStats := tracker.GetAllModelStats()
	if len(allStats) != 2 {
		t.Fatalf("GetAllModelStats: got %d models, want 2", len(allStats))
	}

	statsByModel := make(map[string]ModelStats)
	for _, s := range allStats {
		statsByModel[s.Model] = s
	}

	opus := statsByModel["opus"]
	if opus.TotalFindings != 1 || opus.AdoptedFindings != 1 || opus.AdoptionRate != 1.0 {
		t.Errorf("opus: total=%d adopted=%d rate=%f", opus.TotalFindings, opus.AdoptedFindings, opus.AdoptionRate)
	}

	sonnet := statsByModel["sonnet"]
	if sonnet.TotalFindings != 2 || sonnet.AdoptedFindings != 1 || sonnet.AdoptionRate != 0.5 {
		t.Errorf("sonnet: total=%d adopted=%d rate=%f", sonnet.TotalFindings, sonnet.AdoptedFindings, sonnet.AdoptionRate)
	}
}

func TestGetModelStatsUnknownModel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tracker, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker: %v", err)
	}

	stats := tracker.GetModelStats("nonexistent")
	if stats.Model != "nonexistent" {
		t.Errorf("Model: got %q, want %q", stats.Model, "nonexistent")
	}
	if stats.TotalFindings != 0 || stats.AdoptedFindings != 0 || stats.ReviewCount != 0 || stats.AdoptionRate != 0 {
		t.Errorf("unexpected non-zero stats for unknown model: %+v", stats)
	}
}

func TestNewUsefulnessTrackerNoExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tracker, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker: %v", err)
	}

	allStats := tracker.GetAllModelStats()
	if len(allStats) != 0 {
		t.Errorf("expected empty stats, got %d", len(allStats))
	}
}

func TestAdoptedFindingIDsNotInResult(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tracker, err := NewUsefulnessTracker(dir)
	if err != nil {
		t.Fatalf("NewUsefulnessTracker: %v", err)
	}

	// adoptedFindingIDs contains IDs not present in FindingIDs — should not inflate count
	if err := tracker.RecordResult(ReviewResult{
		ReviewerModel: "opus",
		TaskID:        "t1",
		CommandID:     "c1",
		FindingIDs:    []string{"f1", "f2"},
	}, []string{"f1", "f99"}); err != nil {
		t.Fatal(err)
	}

	stats := tracker.GetModelStats("opus")
	if stats.AdoptedFindings != 1 {
		t.Errorf("AdoptedFindings: got %d, want 1 (f99 should not count)", stats.AdoptedFindings)
	}
}
