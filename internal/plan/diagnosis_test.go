package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

func TestDiagnosePhase_RepairHotspots(t *testing.T) {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)

	phase := model.Phase{PhaseID: "phase-1", TaskIDs: []string{"t1", "t2", "t3"}}
	tasks := []model.Task{
		{
			ID: "t1", Purpose: "TaskA", Status: model.StatusCompleted,
			Attempts: 4, ExecutionRetries: 3,
			LastError: ptr.String("compilation error"), ExpectedPaths: []string{"a.go"},
			CreatedAt: ts, UpdatedAt: ts,
		},
		{
			ID: "t2", Purpose: "TaskB", Status: model.StatusCompleted,
			Attempts:  1,
			CreatedAt: ts, UpdatedAt: ts,
		},
		{
			ID: "t3", Purpose: "TaskC", Status: model.StatusFailed,
			Attempts: 3, ExecutionRetries: 2,
			LastError: ptr.String("test failure"),
			CreatedAt: ts, UpdatedAt: ts,
		},
	}
	results := []model.TaskResult{
		{TaskID: "t1", FilesChanged: []string{"a.go", "b.go"}},
	}

	diag := DiagnosePhase(phase, tasks, results)

	if diag.TotalTasks != 3 {
		t.Errorf("TotalTasks = %d, want 3", diag.TotalTasks)
	}
	if diag.CompletedTasks != 2 {
		t.Errorf("CompletedTasks = %d, want 2", diag.CompletedTasks)
	}
	if diag.FailedTasks != 1 {
		t.Errorf("FailedTasks = %d, want 1", diag.FailedTasks)
	}

	if len(diag.RepairHotspots) != 2 {
		t.Fatalf("RepairHotspots count = %d, want 2", len(diag.RepairHotspots))
	}

	hs0 := diag.RepairHotspots[0]
	if hs0.TaskID != "t1" {
		t.Errorf("Hotspot[0].TaskID = %s, want t1", hs0.TaskID)
	}
	if hs0.RepairCount != 3 {
		t.Errorf("Hotspot[0].RepairCount = %d, want 3", hs0.RepairCount)
	}
	if hs0.LastError != "compilation error" {
		t.Errorf("Hotspot[0].LastError = %q, want %q", hs0.LastError, "compilation error")
	}
	// Should use FilesChanged from result over ExpectedPaths
	if len(hs0.FilePaths) != 2 || hs0.FilePaths[0] != "a.go" {
		t.Errorf("Hotspot[0].FilePaths = %v, want [a.go b.go]", hs0.FilePaths)
	}

	hs1 := diag.RepairHotspots[1]
	if hs1.TaskID != "t3" || hs1.RepairCount != 2 {
		t.Errorf("Hotspot[1] = %s/%d, want t3/2", hs1.TaskID, hs1.RepairCount)
	}
}

func TestDiagnosePhase_RepairCountFallback(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)

	phase := model.Phase{PhaseID: "phase-1"}
	tasks := []model.Task{
		{
			ID: "t1", Purpose: "TaskX", Status: model.StatusCompleted,
			Attempts: 4, ExecutionRetries: 0, // No ExecutionRetries, fall back to Attempts-1
			CreatedAt: now, UpdatedAt: now,
		},
	}

	diag := DiagnosePhase(phase, tasks, nil)

	if len(diag.RepairHotspots) != 1 {
		t.Fatalf("RepairHotspots count = %d, want 1", len(diag.RepairHotspots))
	}
	if diag.RepairHotspots[0].RepairCount != 3 {
		t.Errorf("RepairCount = %d, want 3 (Attempts-1 fallback)", diag.RepairHotspots[0].RepairCount)
	}
}

func TestDiagnosePhase_BlockedTasks(t *testing.T) {
	created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	started := created.Add(120 * time.Second)

	phase := model.Phase{PhaseID: "phase-1"}
	tasks := []model.Task{
		{
			ID: "t1", Purpose: "BlockedTask", Status: model.StatusCompleted,
			Attempts: 1, BlockedBy: []string{"t0"},
			InProgressAt: ptr.String(started.Format(time.RFC3339)),
			CreatedAt:    created.Format(time.RFC3339),
			UpdatedAt:    created.Format(time.RFC3339),
		},
	}

	diag := DiagnosePhase(phase, tasks, nil)

	if len(diag.BlockedTasks) != 1 {
		t.Fatalf("BlockedTasks count = %d, want 1", len(diag.BlockedTasks))
	}

	bt := diag.BlockedTasks[0]
	if bt.TaskID != "t1" {
		t.Errorf("BlockedTask.TaskID = %s, want t1", bt.TaskID)
	}
	if bt.BlockedDurationSec != 120 {
		t.Errorf("BlockedDurationSec = %f, want 120", bt.BlockedDurationSec)
	}
	if len(bt.BlockedBy) != 1 || bt.BlockedBy[0] != "t0" {
		t.Errorf("BlockedBy = %v, want [t0]", bt.BlockedBy)
	}
}

func TestDiagnosePhase_NoBlockedWithoutInProgress(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)

	phase := model.Phase{PhaseID: "phase-1"}
	tasks := []model.Task{
		{
			ID: "t1", Purpose: "StillBlocked", Status: model.StatusReady,
			Attempts: 1, BlockedBy: []string{"t0"},
			// InProgressAt is nil — task never started
			CreatedAt: now, UpdatedAt: now,
		},
	}

	diag := DiagnosePhase(phase, tasks, nil)

	if len(diag.BlockedTasks) != 0 {
		t.Errorf("BlockedTasks count = %d, want 0 (no InProgressAt)", len(diag.BlockedTasks))
	}
}

func TestDiagnosePhase_ZeroRepairs(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)

	phase := model.Phase{PhaseID: "phase-1"}
	tasks := []model.Task{
		{ID: "t1", Purpose: "A", Status: model.StatusCompleted, Attempts: 1, CreatedAt: now, UpdatedAt: now},
		{ID: "t2", Purpose: "B", Status: model.StatusCompleted, Attempts: 1, CreatedAt: now, UpdatedAt: now},
	}

	diag := DiagnosePhase(phase, tasks, nil)

	if len(diag.RepairHotspots) != 0 {
		t.Errorf("RepairHotspots = %d, want 0", len(diag.RepairHotspots))
	}
	if diag.AvgRepairCount != 0 {
		t.Errorf("AvgRepairCount = %f, want 0", diag.AvgRepairCount)
	}
	if diag.CompletedTasks != 2 {
		t.Errorf("CompletedTasks = %d, want 2", diag.CompletedTasks)
	}
}

func TestDiagnosePhase_EmptyInput(t *testing.T) {
	phase := model.Phase{PhaseID: "phase-empty"}
	diag := DiagnosePhase(phase, nil, nil)

	if diag.TotalTasks != 0 {
		t.Errorf("TotalTasks = %d, want 0", diag.TotalTasks)
	}
	if diag.PhaseID != "phase-empty" {
		t.Errorf("PhaseID = %s, want phase-empty", diag.PhaseID)
	}
	if diag.Summary == "" {
		t.Error("Summary should not be empty for empty phase")
	}
	if diag.CompletedTasks != 0 || diag.FailedTasks != 0 {
		t.Errorf("CompletedTasks=%d FailedTasks=%d, want 0/0", diag.CompletedTasks, diag.FailedTasks)
	}
}

func TestDiagnosePhase_AvgRepairCount(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)

	phase := model.Phase{PhaseID: "phase-1"}
	tasks := []model.Task{
		{ID: "t1", Purpose: "A", Status: model.StatusCompleted, ExecutionRetries: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "t2", Purpose: "B", Status: model.StatusCompleted, ExecutionRetries: 1, CreatedAt: now, UpdatedAt: now},
	}

	diag := DiagnosePhase(phase, tasks, nil)

	// (3+1)/2 = 2.0
	if diag.AvgRepairCount != 2.0 {
		t.Errorf("AvgRepairCount = %f, want 2.0", diag.AvgRepairCount)
	}
}

func TestDiagnosePhase_DeadLetterAndAbortedCountAsFailed(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)

	phase := model.Phase{PhaseID: "phase-1"}
	tasks := []model.Task{
		{ID: "t1", Purpose: "A", Status: model.StatusDeadLetter, CreatedAt: now, UpdatedAt: now},
		{ID: "t2", Purpose: "B", Status: model.StatusAborted, CreatedAt: now, UpdatedAt: now},
		{ID: "t3", Purpose: "C", Status: model.StatusCancelled, CreatedAt: now, UpdatedAt: now},
	}

	diag := DiagnosePhase(phase, tasks, nil)

	if diag.FailedTasks != 2 {
		t.Errorf("FailedTasks = %d, want 2 (dead_letter + aborted)", diag.FailedTasks)
	}
	if diag.CompletedTasks != 0 {
		t.Errorf("CompletedTasks = %d, want 0", diag.CompletedTasks)
	}
}

func TestFormatDiagnosisPrompt_WithIssues(t *testing.T) {
	diag := &PhaseDiagnostics{
		PhaseID:        "phase-1",
		TotalTasks:     5,
		CompletedTasks: 3,
		FailedTasks:    2,
		AvgRepairCount: 1.5,
		RepairHotspots: []HotspotInfo{
			{TaskID: "t1", TaskName: "TaskA", RepairCount: 3, LastError: "compile error", FilePaths: []string{"a.go"}},
		},
		BlockedTasks: []BlockInfo{
			{TaskID: "t2", TaskName: "TaskB", BlockedDurationSec: 120, BlockedBy: []string{"TaskC"}},
		},
	}

	result := FormatDiagnosisPrompt(diag)

	expected := []string{
		"## 前フェーズの反省",
		"### Repair 多発箇所",
		"TaskA (3回): compile error",
		"ファイル: a.go",
		"### ブロックタスク",
		"TaskB: 120秒待機 (blocked_by: TaskC)",
		"### 統計",
		"成功率: 60%",
		"平均Repair: 1.5回",
	}
	for _, s := range expected {
		if !strings.Contains(result, s) {
			t.Errorf("missing %q in output:\n%s", s, result)
		}
	}
}

func TestFormatDiagnosisPrompt_NoErrorDetail(t *testing.T) {
	diag := &PhaseDiagnostics{
		TotalTasks:     1,
		CompletedTasks: 1,
		RepairHotspots: []HotspotInfo{
			{TaskID: "t1", TaskName: "TaskX", RepairCount: 2, LastError: ""},
		},
	}

	result := FormatDiagnosisPrompt(diag)

	if !strings.Contains(result, "エラー詳細なし") {
		t.Errorf("expected fallback error text, got:\n%s", result)
	}
}

func TestFormatDiagnosisPrompt_NoIssues(t *testing.T) {
	diag := &PhaseDiagnostics{
		PhaseID:        "phase-1",
		TotalTasks:     3,
		CompletedTasks: 3,
		AvgRepairCount: 0,
	}

	result := FormatDiagnosisPrompt(diag)

	if !strings.Contains(result, "問題なし") {
		t.Errorf("expected '問題なし' for clean phase, got:\n%s", result)
	}
	if !strings.Contains(result, "成功率: 100%") {
		t.Errorf("expected '成功率: 100%%', got:\n%s", result)
	}
}

func TestFormatDiagnosisPrompt_Nil(t *testing.T) {
	result := FormatDiagnosisPrompt(nil)
	if result != "" {
		t.Errorf("expected empty string for nil, got: %s", result)
	}
}

func TestFormatDiagnosisPrompt_MultipleBlockedBy(t *testing.T) {
	diag := &PhaseDiagnostics{
		TotalTasks:     2,
		CompletedTasks: 2,
		BlockedTasks: []BlockInfo{
			{TaskID: "t1", TaskName: "TaskX", BlockedDurationSec: 60, BlockedBy: []string{"t0", "t2"}},
		},
	}

	result := FormatDiagnosisPrompt(diag)

	if !strings.Contains(result, "blocked_by: t0, t2") {
		t.Errorf("expected multiple blocked_by, got:\n%s", result)
	}
}
