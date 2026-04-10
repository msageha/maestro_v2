package plan

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// HotspotInfo represents a task with frequent repair cycles.
type HotspotInfo struct {
	TaskID      string
	TaskName    string
	RepairCount int
	LastError   string
	FilePaths   []string
}

// BlockInfo represents a task that was blocked waiting on dependencies.
type BlockInfo struct {
	TaskID             string
	TaskName           string
	BlockedDurationSec float64
	BlockedBy          []string
}

// PhaseDiagnostics contains diagnostic summary for a completed phase.
type PhaseDiagnostics struct {
	PhaseID        string
	TotalTasks     int
	CompletedTasks int
	FailedTasks    int
	AvgRepairCount float64
	RepairHotspots []HotspotInfo
	BlockedTasks   []BlockInfo
	Summary        string
}

// repairCount returns the effective repair count for a task.
// Uses ExecutionRetries if available, otherwise falls back to Attempts-1.
func repairCount(t *model.Task) int {
	if t.ExecutionRetries > 0 {
		return t.ExecutionRetries
	}
	if t.Attempts > 1 {
		return t.Attempts - 1
	}
	return 0
}

// DiagnosePhase analyzes completed phase tasks and produces diagnostics
// including repair hotspots and blocked task information.
func DiagnosePhase(phase model.Phase, tasks []model.Task, results []model.TaskResult) *PhaseDiagnostics {
	diag := &PhaseDiagnostics{
		PhaseID:    phase.PhaseID,
		TotalTasks: len(tasks),
	}

	if len(tasks) == 0 {
		diag.Summary = "フェーズにタスクなし"
		return diag
	}

	resultByTask := make(map[string]*model.TaskResult, len(results))
	for i := range results {
		resultByTask[results[i].TaskID] = &results[i]
	}

	totalRepairs := 0

	for i := range tasks {
		t := &tasks[i]

		switch t.Status {
		case model.StatusCompleted:
			diag.CompletedTasks++
		case model.StatusFailed, model.StatusDeadLetter, model.StatusAborted:
			diag.FailedTasks++
		}

		rc := repairCount(t)
		totalRepairs += rc

		if rc >= 2 {
			hs := HotspotInfo{
				TaskID:      t.ID,
				TaskName:    t.Purpose,
				RepairCount: rc,
			}
			if t.LastError != nil {
				hs.LastError = *t.LastError
			}
			if r, ok := resultByTask[t.ID]; ok && len(r.FilesChanged) > 0 {
				hs.FilePaths = r.FilesChanged
			} else if len(t.ExpectedPaths) > 0 {
				hs.FilePaths = t.ExpectedPaths
			}
			diag.RepairHotspots = append(diag.RepairHotspots, hs)
		}

		if len(t.BlockedBy) > 0 && t.InProgressAt != nil {
			created, err1 := time.Parse(time.RFC3339, t.CreatedAt)
			started, err2 := time.Parse(time.RFC3339, *t.InProgressAt)
			if err1 == nil && err2 == nil {
				dur := started.Sub(created).Seconds()
				if dur > 0 {
					diag.BlockedTasks = append(diag.BlockedTasks, BlockInfo{
						TaskID:             t.ID,
						TaskName:           t.Purpose,
						BlockedDurationSec: math.Round(dur*100) / 100,
						BlockedBy:          t.BlockedBy,
					})
				}
			}
		}
	}

	diag.AvgRepairCount = float64(totalRepairs) / float64(len(tasks))
	diag.Summary = buildSummaryText(diag)

	return diag
}

// FormatDiagnosisPrompt formats diagnostics as a structured reflection note
// for inclusion in the next phase's Planner prompt.
func FormatDiagnosisPrompt(diag *PhaseDiagnostics) string {
	if diag == nil {
		return ""
	}

	successRate := float64(0)
	if diag.TotalTasks > 0 {
		successRate = float64(diag.CompletedTasks) / float64(diag.TotalTasks) * 100
	}

	if len(diag.RepairHotspots) == 0 && len(diag.BlockedTasks) == 0 {
		return fmt.Sprintf("## 前フェーズの反省\n問題なし（成功率: %.0f%%, 平均Repair: %.1f回）",
			successRate, diag.AvgRepairCount)
	}

	var b strings.Builder
	b.WriteString("## 前フェーズの反省\n")

	if len(diag.RepairHotspots) > 0 {
		b.WriteString("### Repair 多発箇所\n")
		for _, hs := range diag.RepairHotspots {
			errSummary := hs.LastError
			if errSummary == "" {
				errSummary = "エラー詳細なし"
			}
			fmt.Fprintf(&b, "- %s (%d回): %s\n", hs.TaskName, hs.RepairCount, errSummary)
			if len(hs.FilePaths) > 0 {
				fmt.Fprintf(&b, "  ファイル: %s\n", strings.Join(hs.FilePaths, ", "))
			}
		}
	}

	if len(diag.BlockedTasks) > 0 {
		b.WriteString("### ブロックタスク\n")
		for _, bt := range diag.BlockedTasks {
			fmt.Fprintf(&b, "- %s: %.0f秒待機 (blocked_by: %s)\n",
				bt.TaskName, bt.BlockedDurationSec, strings.Join(bt.BlockedBy, ", "))
		}
	}

	fmt.Fprintf(&b, "### 統計\n- 成功率: %.0f%%, 平均Repair: %.1f回", successRate, diag.AvgRepairCount)

	return b.String()
}

func buildSummaryText(diag *PhaseDiagnostics) string {
	parts := []string{
		fmt.Sprintf("タスク: %d (完了: %d, 失敗: %d)", diag.TotalTasks, diag.CompletedTasks, diag.FailedTasks),
		fmt.Sprintf("平均Repair: %.1f回", diag.AvgRepairCount),
	}
	if len(diag.RepairHotspots) > 0 {
		parts = append(parts, fmt.Sprintf("Repairホットスポット: %d件", len(diag.RepairHotspots)))
	}
	if len(diag.BlockedTasks) > 0 {
		parts = append(parts, fmt.Sprintf("ブロックタスク: %d件", len(diag.BlockedTasks)))
	}
	return strings.Join(parts, ", ")
}
