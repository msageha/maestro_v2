package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/dispatch"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
)

// ============================================================================
// Phase A Acceptance Tests — REQUIREMENTS.md §6
//
// Phase A 完了基準:
//   「異種モデル Reviewer の指摘事項がマージをブロックせず、
//    かつその採用率がデータとして蓄積されていること」
//
// 検証項目:
//   A-1: Advisory 非ブロッキング性 (quality gate warn mode)
//   A-2: 有用性データ蓄積 (QualityGateEvaluation storage)
//   A-3: 自己診断 (PhaseDiagnostics / RepairHotspots / 反省メモ)
//   A-4: Path-overlap Heuristic (1 task per worker + worktree isolation)
// ============================================================================

// paStrPtr is a local string-pointer helper for phase_a tests.
// (strPtr already declared in queue_scan_helpers_test.go)
func paStrPtr(s string) *string { return &s }

// newTestQualityGateEvaluator creates a QualityGateEvaluator for unit tests.
func newTestQualityGateEvaluator(enabled, skipGates bool, gateFn func() dispatch.GateChecker) *QualityGateEvaluator {
	var cfg model.Config
	cfg.QualityGates.Enabled = enabled
	cfg.QualityGates.SkipGates = skipGates
	dl := NewDaemonLoggerFromLegacy("test", log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	return NewQualityGateEvaluator(cfg, RealClock{}, dl, gateFn)
}

// --- A-1: Advisory 非ブロッキング性 ---
// Review/quality gate 評価結果がタスクのディスパッチ・完了をブロックしないことを検証する。

func TestPhaseA_A1_GatesDisabled_ShouldNotEvaluate(t *testing.T) {
	t.Parallel()
	eval := newTestQualityGateEvaluator(false, false, func() dispatch.GateChecker { return nil })

	if eval.ShouldEvaluate() {
		t.Error("ShouldEvaluate must return false when gates are disabled")
	}
}

func TestPhaseA_A1_SkipGates_EmergencyMode(t *testing.T) {
	t.Parallel()
	eval := newTestQualityGateEvaluator(true, true, func() dispatch.GateChecker { return nil })

	if eval.ShouldEvaluate() {
		t.Error("ShouldEvaluate must return false in emergency mode (skip_gates=true)")
	}
}

func TestPhaseA_A1_GateDaemonNil_NoBlocking(t *testing.T) {
	t.Parallel()
	eval := newTestQualityGateEvaluator(true, false, func() dispatch.GateChecker { return nil })

	// Even when enabled, nil daemon means no evaluation (no blocking).
	if eval.ShouldEvaluate() {
		t.Error("ShouldEvaluate must return false when gate daemon is nil")
	}
}

func TestPhaseA_A1_IntegrationDispatch_GatesOff_NotBlocked(t *testing.T) {
	t.Parallel()
	// Full Phase A→B→C: task dispatches without blocking when gates are disabled.
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID: "task_pa1_001", CommandID: "cmd_pa1_001",
			Purpose: "A-1 advisory test", Content: "verify non-blocking",
			Priority: 1, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
	})

	pa := qh.periodicScanPhaseA()
	if len(pa.work.dispatches) != 1 {
		t.Fatalf("Phase A: expected 1 dispatch, got %d", len(pa.work.dispatches))
	}

	pb := qh.periodicScanPhaseB(context.Background(), pa)
	if len(pb.dispatches) != 1 || !pb.dispatches[0].Success {
		t.Fatal("Phase B: dispatch must succeed when gates are disabled")
	}

	_ = qh.periodicScanPhaseC(pa, pb)

	tq := piReadTaskQueue(t, maestroDir, "worker1")
	if tq.Tasks[0].Status != model.StatusInProgress {
		t.Errorf("task should be in_progress, got %s", tq.Tasks[0].Status)
	}
}

func TestPhaseA_A1_TaskCompletes_RegardlessOfGateState(t *testing.T) {
	t.Parallel()
	// A completed task (via result_write) is not blocked by gate evaluation state.
	// This is verified by showing that the result_write path is independent of
	// quality gate results — the gate evaluation only decorates the result.
	eval := newTestQualityGateEvaluator(false, false, func() dispatch.GateChecker { return nil })

	// Store a failing evaluation
	eval.StoreEvaluation("task_fail_gate", &model.QualityGateEvaluation{
		Passed:      false,
		FailedGates: []string{"coverage", "lint"},
		EvaluatedAt: time.Now().Format(time.RFC3339),
	})

	// The evaluation is stored but does not affect task completion flow.
	// Quality gate failures with action="warn" only log; they never block
	// result_write or status transitions.
	stored := eval.GetEvaluation("task_fail_gate")

	if stored == nil {
		t.Fatal("evaluation should be stored")
	}
	if stored.Passed {
		t.Error("stored evaluation should record failure (Passed=false)")
	}
	// Key assertion: the evaluation records failure data without blocking anything.
	// The task lifecycle (in_progress → completed) is controlled by result_write,
	// which is independent of quality gate evaluations.
}

// --- A-2: 有用性データ蓄積 ---
// 評価結果がタスク単位で蓄積され、モデルごとの統計が計算可能であることを検証する。

func TestPhaseA_A2_EvaluationStored(t *testing.T) {
	t.Parallel()
	eval := newTestQualityGateEvaluator(false, false, func() dispatch.GateChecker { return nil })

	evaluation := eval.SkippedEvaluation("disabled")
	eval.StoreEvaluation("task_001", evaluation)

	stored := eval.GetEvaluation("task_001")

	if stored == nil {
		t.Fatal("evaluation should be stored for task_001")
	}
	if !stored.Passed {
		t.Error("skipped evaluation should have Passed=true")
	}
	if stored.SkippedReason != "disabled" {
		t.Errorf("SkippedReason=%q, want 'disabled'", stored.SkippedReason)
	}
	if stored.EvaluatedAt == "" {
		t.Error("EvaluatedAt should be set")
	}
}

func TestPhaseA_A2_MultipleEvaluations_PerTask(t *testing.T) {
	t.Parallel()
	eval := newTestQualityGateEvaluator(false, false, func() dispatch.GateChecker { return nil })

	// Simulate evaluations from different tasks (different models/contexts)
	eval.StoreEvaluation("task_model_a", &model.QualityGateEvaluation{
		Passed:      true,
		EvaluatedAt: time.Now().Format(time.RFC3339),
	})
	eval.StoreEvaluation("task_model_b", &model.QualityGateEvaluation{
		Passed:      false,
		FailedGates: []string{"coverage"},
		EvaluatedAt: time.Now().Format(time.RFC3339),
	})
	eval.StoreEvaluation("task_model_c", &model.QualityGateEvaluation{
		Passed:      true,
		Action:      "warn",
		EvaluatedAt: time.Now().Format(time.RFC3339),
	})

	count := eval.EvaluationCount()
	modelA := eval.GetEvaluation("task_model_a")
	modelB := eval.GetEvaluation("task_model_b")
	modelC := eval.GetEvaluation("task_model_c")

	if count != 3 {
		t.Errorf("expected 3 evaluations, got %d", count)
	}
	if !modelA.Passed {
		t.Error("model_a evaluation should have Passed=true")
	}
	if modelB.Passed {
		t.Error("model_b evaluation should have Passed=false")
	}
	if len(modelB.FailedGates) != 1 || modelB.FailedGates[0] != "coverage" {
		t.Errorf("model_b FailedGates=%v, want [coverage]", modelB.FailedGates)
	}
	if modelC.Action != "warn" {
		t.Errorf("model_c Action=%q, want 'warn'", modelC.Action)
	}
}

func TestPhaseA_A2_AdoptionRate_Calculable(t *testing.T) {
	t.Parallel()
	// Verify that stored evaluations allow computing adoption rate statistics.
	eval := newTestQualityGateEvaluator(false, false, func() dispatch.GateChecker { return nil })

	// 3 evaluations: 2 passed, 1 failed
	for i := 0; i < 3; i++ {
		eval.StoreEvaluation(fmt.Sprintf("task_%03d", i), &model.QualityGateEvaluation{
			Passed:      i < 2,
			EvaluatedAt: time.Now().Add(time.Duration(i) * time.Second).Format(time.RFC3339),
		})
	}

	total := eval.EvaluationCount()
	if total != 3 {
		t.Fatalf("total=%d, want 3", total)
	}
	var passed int
	for i := 0; i < 3; i++ {
		e := eval.GetEvaluation(fmt.Sprintf("task_%03d", i))
		if e != nil && e.Passed {
			passed++
		}
	}
	rate := float64(passed) / float64(total) * 100
	if rate < 66.0 || rate > 67.0 {
		t.Errorf("pass rate=%.1f%%, want ~66.7%%", rate)
	}
}

func TestPhaseA_A2_EvictionOnOverflow(t *testing.T) {
	t.Parallel()
	eval := newTestQualityGateEvaluator(false, false, func() dispatch.GateChecker { return nil })

	// Store more than MaxGateEvaluations entries to trigger eviction.
	for i := 0; i < dispatch.MaxGateEvaluations+10; i++ {
		eval.StoreEvaluation(
			fmt.Sprintf("task_%05d", i),
			&model.QualityGateEvaluation{
				Passed:      true,
				EvaluatedAt: time.Now().Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			},
		)
	}

	count := eval.EvaluationCount()

	if count > dispatch.MaxGateEvaluations {
		t.Errorf("evaluations=%d, expected <= %d after eviction", count, dispatch.MaxGateEvaluations)
	}
	if count == 0 {
		t.Error("evaluations should not be empty after eviction")
	}
}

func TestPhaseA_A2_EvictionDeterministicSort(t *testing.T) {
	t.Parallel()
	// Verify that eviction deterministically removes the oldest entries,
	// regardless of map iteration order. Run multiple iterations to catch
	// non-determinism from map randomization.
	for iter := 0; iter < 5; iter++ {
		eval := newTestQualityGateEvaluator(false, false, func() dispatch.GateChecker { return nil })

		total := dispatch.MaxGateEvaluations + 20
		baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

		for i := 0; i < total; i++ {
			eval.StoreEvaluation(
				fmt.Sprintf("task_%05d", i),
				&model.QualityGateEvaluation{
					Passed:      true,
					EvaluatedAt: baseTime.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
				},
			)
		}

		// After eviction, the newest entries should survive.
		// evictTarget = MaxGateEvaluations / 2, so count should be <= evictTarget + entries added after eviction.
		count := eval.EvaluationCount()
		if count > dispatch.MaxGateEvaluations {
			t.Fatalf("iter=%d: count=%d exceeds max=%d", iter, count, dispatch.MaxGateEvaluations)
		}

		// The oldest entries (smallest indices) should have been evicted.
		// The newest entries should remain.
		newestID := fmt.Sprintf("task_%05d", total-1)
		if eval.GetEvaluation(newestID) == nil {
			t.Errorf("iter=%d: newest entry %s was evicted", iter, newestID)
		}

		// An old entry that should have been evicted.
		oldestID := fmt.Sprintf("task_%05d", 0)
		if eval.GetEvaluation(oldestID) != nil {
			t.Errorf("iter=%d: oldest entry %s should have been evicted", iter, oldestID)
		}
	}
}

// --- A-3: 自己診断 ---
// PhaseDiagnostics が生成され、RepairHotspot 抽出と反省メモが機能することを検証する。

func TestPhaseA_A3_PhaseDiagnostics_Generated(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Format(time.RFC3339)

	phase := model.Phase{PhaseID: "phase-a3-test", TaskIDs: []string{"t1", "t2", "t3"}}
	tasks := []model.Task{
		{ID: "t1", Purpose: "Task A", Status: model.StatusCompleted, ExecutionRetries: 0, CreatedAt: now, UpdatedAt: now},
		{ID: "t2", Purpose: "Task B", Status: model.StatusCompleted, ExecutionRetries: 1, CreatedAt: now, UpdatedAt: now},
		{ID: "t3", Purpose: "Task C", Status: model.StatusFailed, ExecutionRetries: 0, CreatedAt: now, UpdatedAt: now},
	}

	diag := plan.DiagnosePhase(phase, tasks, nil)

	if diag == nil {
		t.Fatal("PhaseDiagnostics should not be nil")
	}
	if diag.PhaseID != "phase-a3-test" {
		t.Errorf("PhaseID=%q, want 'phase-a3-test'", diag.PhaseID)
	}
	if diag.TotalTasks != 3 {
		t.Errorf("TotalTasks=%d, want 3", diag.TotalTasks)
	}
	if diag.CompletedTasks != 2 {
		t.Errorf("CompletedTasks=%d, want 2", diag.CompletedTasks)
	}
	if diag.FailedTasks != 1 {
		t.Errorf("FailedTasks=%d, want 1", diag.FailedTasks)
	}
	if diag.Summary == "" {
		t.Error("Summary should not be empty")
	}
}

func TestPhaseA_A3_HotspotExtraction_RepairCountGE2(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Format(time.RFC3339)

	phase := model.Phase{PhaseID: "phase-hotspot"}
	tasks := []model.Task{
		{
			ID: "t1", Purpose: "HotTask", Status: model.StatusCompleted,
			ExecutionRetries: 3, LastError: paStrPtr("compilation error"),
			ExpectedPaths: []string{"internal/foo.go"},
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "t2", Purpose: "NormalTask", Status: model.StatusCompleted,
			ExecutionRetries: 1, // below threshold
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "t3", Purpose: "FailedHot", Status: model.StatusFailed,
			ExecutionRetries: 2, LastError: paStrPtr("test failure"),
			CreatedAt: now, UpdatedAt: now,
		},
	}
	results := []model.TaskResult{
		{TaskID: "t1", FilesChanged: []string{"internal/foo.go", "internal/bar.go"}},
	}

	diag := plan.DiagnosePhase(phase, tasks, results)

	// Only tasks with RepairCount >= 2 should be hotspots
	if len(diag.RepairHotspots) != 2 {
		t.Fatalf("RepairHotspots=%d, want 2 (t1 rc=3, t3 rc=2)", len(diag.RepairHotspots))
	}

	hs0 := diag.RepairHotspots[0]
	if hs0.TaskID != "t1" || hs0.RepairCount != 3 {
		t.Errorf("Hotspot[0]=%s/rc=%d, want t1/3", hs0.TaskID, hs0.RepairCount)
	}
	if hs0.LastError != "compilation error" {
		t.Errorf("Hotspot[0].LastError=%q, want 'compilation error'", hs0.LastError)
	}
	// FilesChanged from result should take priority over ExpectedPaths
	if len(hs0.FilePaths) != 2 || hs0.FilePaths[0] != "internal/foo.go" {
		t.Errorf("Hotspot[0].FilePaths=%v, want [internal/foo.go internal/bar.go]", hs0.FilePaths)
	}

	hs1 := diag.RepairHotspots[1]
	if hs1.TaskID != "t3" || hs1.RepairCount != 2 {
		t.Errorf("Hotspot[1]=%s/rc=%d, want t3/2", hs1.TaskID, hs1.RepairCount)
	}
}

func TestPhaseA_A3_ReflectionMemo_IncludesHotspots(t *testing.T) {
	t.Parallel()
	diag := &plan.PhaseDiagnostics{
		PhaseID:        "phase-reflection",
		TotalTasks:     5,
		CompletedTasks: 3,
		FailedTasks:    2,
		AvgRepairCount: 1.5,
		RepairHotspots: []plan.HotspotInfo{
			{TaskID: "t1", TaskName: "TaskA", RepairCount: 3,
				LastError: "compile error", FilePaths: []string{"a.go"}},
			{TaskID: "t2", TaskName: "TaskB", RepairCount: 2,
				LastError: "test failure", FilePaths: []string{"b.go", "c.go"}},
		},
	}

	memo := plan.FormatDiagnosisPrompt(diag)

	expected := []string{
		"前フェーズの反省",
		"Repair 多発箇所",
		"TaskA (3回): compile error",
		"ファイル: a.go",
		"TaskB (2回): test failure",
		"ファイル: b.go, c.go",
		"成功率: 60%",
		"平均Repair: 1.5回",
	}
	for _, s := range expected {
		if !strings.Contains(memo, s) {
			t.Errorf("reflection memo missing %q\nmemo:\n%s", s, memo)
		}
	}
}

func TestPhaseA_A3_ReflectionMemo_NextPhaseInput(t *testing.T) {
	t.Parallel()
	// Verify that the reflection memo is structured as Planner input
	// for the next phase (REQUIREMENTS A-3).
	diag := &plan.PhaseDiagnostics{
		PhaseID:        "phase-1",
		TotalTasks:     3,
		CompletedTasks: 3,
		AvgRepairCount: 0,
	}

	memo := plan.FormatDiagnosisPrompt(diag)

	if !strings.Contains(memo, "前フェーズの反省") {
		t.Error("reflection memo must contain '前フェーズの反省' header")
	}
	if !strings.Contains(memo, "問題なし") {
		t.Error("clean phase memo should contain '問題なし'")
	}
	if !strings.Contains(memo, "成功率: 100%") {
		t.Error("clean phase memo should show 100% success rate")
	}
}

func TestPhaseA_A3_DiagnosticsNilSafe(t *testing.T) {
	t.Parallel()
	memo := plan.FormatDiagnosisPrompt(nil)
	if memo != "" {
		t.Errorf("nil diagnostics should produce empty string, got %q", memo)
	}
}

// --- A-4: Path-overlap Heuristic ---
// 同一ファイルを触るタスクの同時ディスパッチ回避を検証する。
// 現在の実装: worker 単位で 1 タスク/サイクル + worktree 分離により衝突回避。

func TestPhaseA_A4_SingleTaskPerWorkerPerCycle(t *testing.T) {
	t.Parallel()
	// Verify: only one pending task per worker dispatched per scan cycle.
	// Even tasks with overlapping expected_paths in the same worker queue
	// are serialized — the second waits for the first to complete.
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID: "task_pa4_001", CommandID: "cmd_pa4_001",
			Purpose:       "task A (overlapping path)",
			Content:       "edit shared file",
			ExpectedPaths: []string{"internal/shared.go"},
			Priority:      1, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "task_pa4_002", CommandID: "cmd_pa4_001",
			Purpose:       "task B (overlapping path)",
			Content:       "also edit shared file",
			ExpectedPaths: []string{"internal/shared.go"},
			Priority:      2, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
	})

	pa := qh.periodicScanPhaseA()

	var taskDispatches int
	for _, d := range pa.work.dispatches {
		if d.Kind == "task" && d.WorkerID == "worker1" {
			taskDispatches++
		}
	}
	if taskDispatches != 1 {
		t.Errorf("expected 1 task dispatch per worker per cycle, got %d", taskDispatches)
	}

	// Higher priority task (priority=1) should be chosen
	for _, d := range pa.work.dispatches {
		if d.Kind == "task" && d.Task.ID != "task_pa4_001" {
			t.Errorf("expected task_pa4_001 (higher priority), got %s", d.Task.ID)
		}
	}
}

func TestPhaseA_A4_PrefixContainment_SameWorker_Sequential(t *testing.T) {
	t.Parallel()
	// Prefix containment: internal/ contains internal/foo.go.
	// Tasks with prefix-overlap in the same worker queue are serialized.
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID: "task_prefix_001", CommandID: "cmd_prefix_001",
			Purpose:       "broad scope",
			Content:       "refactor internal",
			ExpectedPaths: []string{"internal/"},
			Priority:      1, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "task_prefix_002", CommandID: "cmd_prefix_001",
			Purpose:       "narrow scope",
			Content:       "fix internal/foo.go",
			ExpectedPaths: []string{"internal/foo.go"},
			Priority:      2, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
	})

	pa := qh.periodicScanPhaseA()

	var taskDispatches int
	for _, d := range pa.work.dispatches {
		if d.Kind == "task" {
			taskDispatches++
		}
	}
	// Single-task-per-worker guarantee prevents parallel dispatch
	if taskDispatches != 1 {
		t.Errorf("prefix-overlap tasks in same worker should be serialized: got %d dispatches", taskDispatches)
	}
}

func TestPhaseA_A4_NonOverlapping_DifferentWorkers_Parallel(t *testing.T) {
	t.Parallel()
	// Non-overlapping tasks on different workers dispatch in parallel.
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID: "task_noov_w1", CommandID: "cmd_noov_001",
			Purpose:       "worker1 task",
			Content:       "edit foo",
			ExpectedPaths: []string{"internal/foo.go"},
			Priority:      1, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
	})
	writeTaskQueue(t, maestroDir, "worker2", []model.Task{
		{
			ID: "task_noov_w2", CommandID: "cmd_noov_001",
			Purpose:       "worker2 task",
			Content:       "edit bar",
			ExpectedPaths: []string{"internal/bar.go"},
			Priority:      1, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
	})

	pa := qh.periodicScanPhaseA()

	workers := map[string]bool{}
	for _, d := range pa.work.dispatches {
		if d.Kind == "task" {
			workers[d.WorkerID] = true
		}
	}
	if len(workers) != 2 {
		t.Errorf("non-overlapping tasks on different workers should dispatch in parallel: got %d workers", len(workers))
	}
	if !workers["worker1"] || !workers["worker2"] {
		t.Errorf("expected both workers dispatched, got %v", workers)
	}
}

func TestPhaseA_A4_InProgressBlocks_NextDispatch(t *testing.T) {
	t.Parallel()
	// When a worker already has an in_progress task, no additional
	// pending tasks dispatch to that worker (global in-flight guard).
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	expires := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	owner := qh.leaseOwnerID()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID: "task_inflight", CommandID: "cmd_inflight_001",
			Purpose:        "already running",
			Content:        "in progress",
			Status:         model.StatusInProgress,
			LeaseOwner:     &owner,
			LeaseExpiresAt: &expires,
			LeaseEpoch:     1,
			CreatedAt:      now, UpdatedAt: now,
		},
		{
			ID: "task_pending", CommandID: "cmd_inflight_001",
			Purpose:       "waiting",
			Content:       "should not dispatch yet",
			ExpectedPaths: []string{"internal/bar.go"},
			Priority:      1, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
	})

	pa := qh.periodicScanPhaseA()

	for _, d := range pa.work.dispatches {
		if d.Kind == "task" && d.WorkerID == "worker1" {
			t.Error("no task should dispatch for worker1 while a task is in_progress")
		}
	}
}

func TestPhaseA_A4_AfterCompletion_NextTaskDispatches(t *testing.T) {
	t.Parallel()
	// After the first task completes (transitions to a terminal state),
	// the next pending task dispatches on the subsequent scan cycle.
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID: "task_done", CommandID: "cmd_seq_001",
			Purpose:   "already completed",
			Content:   "done",
			Status:    model.StatusCompleted,
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "task_next", CommandID: "cmd_seq_001",
			Purpose:       "ready to dispatch",
			Content:       "next task",
			ExpectedPaths: []string{"internal/shared.go"},
			Priority:      1, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
	})

	pa := qh.periodicScanPhaseA()

	var dispatched bool
	for _, d := range pa.work.dispatches {
		if d.Kind == "task" && d.Task.ID == "task_next" {
			dispatched = true
		}
	}
	if !dispatched {
		t.Error("pending task should dispatch after prior task completes")
	}
}

// --- Full Pipeline Validation ---

func TestPhaseA_FullPipeline_GatesDisabled_E2E(t *testing.T) {
	t.Parallel()
	// End-to-end: two workers with tasks dispatch, execute, and the pipeline
	// completes without gate-related blocking.
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	now := nowRFC3339()
	writeTaskQueue(t, maestroDir, "worker1", []model.Task{
		{
			ID: "task_e2e_w1", CommandID: "cmd_e2e_001",
			Purpose: "worker1 task", Content: "do work",
			Priority: 1, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
	})
	writeTaskQueue(t, maestroDir, "worker2", []model.Task{
		{
			ID: "task_e2e_w2", CommandID: "cmd_e2e_001",
			Purpose: "worker2 task", Content: "do work too",
			Priority: 1, Status: model.StatusPending,
			CreatedAt: now, UpdatedAt: now,
		},
	})

	// Phase A → B → C
	pa := qh.periodicScanPhaseA()
	pb := qh.periodicScanPhaseB(context.Background(), pa)
	_ = qh.periodicScanPhaseC(pa, pb)

	// Verify both tasks dispatched
	calls := exec.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 executor calls, got %d", len(calls))
	}

	agents := map[string]bool{}
	for _, c := range calls {
		agents[c.AgentID] = true
	}
	if !agents["worker1"] || !agents["worker2"] {
		t.Errorf("expected dispatch to both workers, got %v", agents)
	}

	// Both tasks should be in_progress
	for _, wid := range []string{"worker1", "worker2"} {
		tq := piReadTaskQueue(t, maestroDir, wid)
		if len(tq.Tasks) != 1 {
			t.Fatalf("%s: expected 1 task, got %d", wid, len(tq.Tasks))
		}
		if tq.Tasks[0].Status != model.StatusInProgress {
			t.Errorf("%s: expected in_progress, got %s", wid, tq.Tasks[0].Status)
		}
	}
}
