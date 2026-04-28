package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

// TestPersistReviewResult_AppendsAndReplaces pins the audit-trail file
// shape introduced 2026-04-28 to address the operator-visible
// "finding 本文の保存先も見つけにくい" gap surfaced in the conflict-recovery
// E2E run. The advisory review pipeline never gates completion, so the
// only path to recover full Finding text post hoc is this YAML mirror at
// .maestro/state/reviews/<task_id>.yaml. Behaviour pinned:
//   - first result for a task creates the file and seeds schema_version /
//     file_type so older readers can detect the format
//   - re-running the same reviewer overwrites that reviewer's slot rather
//     than appending a duplicate (otherwise repeated retries would let the
//     file grow unboundedly)
//   - a different reviewer model appends a new entry, preserving the
//     earlier verdict for cross-reviewer comparison
//   - a malformed pre-existing file is reset to a fresh log instead of
//     blocking the new write — the file is observability, not consensus.
func TestPersistReviewResult_AppendsAndReplaces(t *testing.T) {
	maestroDir := t.TempDir()
	rc := &ReviewCoordinator{
		maestroDir: maestroDir,
		log:        func(LogLevel, string, ...any) {},
	}

	taskID := "task_test_review"
	commandID := "cmd_test_review"

	first := model.ReviewResult{
		RequestID:     "review-" + taskID + "-1",
		ReviewerModel: "claude-sonnet-4-6",
		Status:        model.ReviewStatusCompleted,
		IsAdvisory:    true,
		CreatedAt:     time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC),
		Findings: []model.ReviewFinding{
			{Severity: model.ReviewSeverityWarning, FilePath: "foo.go", Line: 3, Message: "shadowed var"},
		},
	}

	path := rc.persistReviewResult(taskID, commandID, first)
	if path == "" {
		t.Fatal("persistReviewResult returned empty path")
	}
	wantPath := filepath.Join(maestroDir, "state", "reviews", taskID+".yaml")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}

	got := loadTaskReviewLog(t, path)
	if got.SchemaVersion != 1 || got.FileType != "task_review_log" {
		t.Errorf("schema/file_type = (%d,%q), want (1,task_review_log)", got.SchemaVersion, got.FileType)
	}
	if got.TaskID != taskID || got.CommandID != commandID {
		t.Errorf("task/command = (%q,%q), want (%q,%q)", got.TaskID, got.CommandID, taskID, commandID)
	}
	if len(got.Results) != 1 || got.Results[0].ReviewerModel != "claude-sonnet-4-6" {
		t.Fatalf("first write: results = %+v, want one entry for claude-sonnet-4-6", got.Results)
	}
	if len(got.Results[0].Findings) != 1 || got.Results[0].Findings[0].Message != "shadowed var" {
		t.Errorf("finding text not preserved: %+v", got.Results[0].Findings)
	}

	// Re-running the same reviewer with a different verdict must replace,
	// not append, otherwise retry sweeps would grow the file unboundedly.
	updated := first
	updated.Findings = []model.ReviewFinding{
		{Severity: model.ReviewSeverityError, FilePath: "foo.go", Line: 3, Message: "use after free"},
	}
	rc.persistReviewResult(taskID, commandID, updated)
	got = loadTaskReviewLog(t, path)
	if len(got.Results) != 1 {
		t.Fatalf("after same-reviewer replay: len(results) = %d, want 1", len(got.Results))
	}
	if got.Results[0].Findings[0].Message != "use after free" {
		t.Errorf("replay did not replace findings: got %+v", got.Results[0].Findings)
	}

	// A different reviewer model is a separate verdict and must append.
	other := first
	other.RequestID = "review-" + taskID + "-2"
	other.ReviewerModel = "codex-1"
	other.Findings = []model.ReviewFinding{
		{Severity: model.ReviewSeverityInfo, FilePath: "foo.go", Line: 9, Message: "consider Go 1.22 range-over-int"},
	}
	rc.persistReviewResult(taskID, commandID, other)
	got = loadTaskReviewLog(t, path)
	if len(got.Results) != 2 {
		t.Fatalf("after different-reviewer write: len(results) = %d, want 2", len(got.Results))
	}
	models := map[string]bool{}
	for _, r := range got.Results {
		models[r.ReviewerModel] = true
	}
	if !models["claude-sonnet-4-6"] || !models["codex-1"] {
		t.Errorf("results lost a reviewer: %+v", models)
	}
}

func TestPersistReviewResult_RecoversFromCorruptFile(t *testing.T) {
	maestroDir := t.TempDir()
	rc := &ReviewCoordinator{
		maestroDir: maestroDir,
		log:        func(LogLevel, string, ...any) {},
	}

	taskID := "task_corrupt"
	path := rc.taskReviewLogPath(taskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pre-seed with garbage to simulate a half-flushed write or operator
	// edit. A refusal here would lose today's audit data on top of
	// yesterday's corruption.
	if err := os.WriteFile(path, []byte("::: not yaml :::\n"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	result := model.ReviewResult{
		ReviewerModel: "claude-sonnet-4-6",
		Status:        model.ReviewStatusCompleted,
		IsAdvisory:    true,
	}
	rc.persistReviewResult(taskID, "cmd_corrupt", result)

	got := loadTaskReviewLog(t, path)
	if got.SchemaVersion != 1 || len(got.Results) != 1 {
		t.Errorf("recovery write produced unexpected log: %+v", got)
	}
}

func loadTaskReviewLog(t *testing.T, path string) TaskReviewLog {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var log TaskReviewLog
	if err := yamlv3.Unmarshal(data, &log); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return log
}
