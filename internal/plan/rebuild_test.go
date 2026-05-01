package plan

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

func setupRebuildDir(t *testing.T) (string, *lock.MutexMap) {
	t.Helper()
	dir := t.TempDir()
	lm := lock.NewMutexMap()

	// Create required directories
	for _, sub := range []string{"state/commands", "results"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, lm
}

func writeRebuildState(t *testing.T, dir string, state *model.CommandState) {
	t.Helper()
	path := filepath.Join(dir, "state", "commands", state.CommandID+".yaml")
	data, err := yamlv3.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func writeWorkerResult(t *testing.T, dir string, filename string, rf model.TaskResultFile) {
	t.Helper()
	path := filepath.Join(dir, "results", filename)
	data, err := yamlv3.Marshal(rf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func loadState(t *testing.T, dir string, commandID string) *model.CommandState {
	t.Helper()
	path := filepath.Join(dir, "state", "commands", commandID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return &state
}

func TestRebuild_NilLockMap(t *testing.T) {
	err := Rebuild(RebuildOptions{
		CommandID:  "cmd_test",
		MaestroDir: t.TempDir(),
		LockMap:    nil,
	})
	if err == nil {
		t.Fatal("expected error for nil LockMap")
	}
}

func TestRebuild_NoResults(t *testing.T) {
	dir, lm := setupRebuildDir(t)

	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_test_1",
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{"task1": model.StatusPending},
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	writeRebuildState(t, dir, state)

	err := Rebuild(RebuildOptions{
		CommandID:  "cmd_test_1",
		MaestroDir: dir,
		LockMap:    lm,
	})
	if err != nil {
		t.Fatalf("Rebuild error: %v", err)
	}

	updated := loadState(t, dir, "cmd_test_1")
	if updated.LastReconciledAt == nil {
		t.Fatal("LastReconciledAt should be set after rebuild")
	}
	// Task state should remain pending (no results)
	if updated.TaskStates["task1"] != model.StatusPending {
		t.Errorf("task1 status = %s, want pending", updated.TaskStates["task1"])
	}
}

func TestRebuild_AppliesLatestResult(t *testing.T) {
	dir, lm := setupRebuildDir(t)

	now := time.Now().UTC()
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_test_2",
		TaskTracking: model.TaskTracking{
			TaskStates:       map[string]model.Status{"task1": model.StatusPending},
			AppliedResultIDs: make(map[string]string),
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	writeRebuildState(t, dir, state)

	// Write two results for the same task — the later one should win.
	writeWorkerResult(t, dir, "worker1.yaml", model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_old",
				TaskID:    "task1",
				CommandID: "cmd_test_2",
				Status:    model.StatusFailed,
				CreatedAt: now.Add(-time.Minute).Format(time.RFC3339),
			},
		},
	})
	writeWorkerResult(t, dir, "worker2.yaml", model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_new",
				TaskID:    "task1",
				CommandID: "cmd_test_2",
				Status:    model.StatusCompleted,
				CreatedAt: now.Format(time.RFC3339),
			},
		},
	})

	err := Rebuild(RebuildOptions{
		CommandID:  "cmd_test_2",
		MaestroDir: dir,
		LockMap:    lm,
	})
	if err != nil {
		t.Fatalf("Rebuild error: %v", err)
	}

	updated := loadState(t, dir, "cmd_test_2")
	if updated.TaskStates["task1"] != model.StatusCompleted {
		t.Errorf("task1 status = %s, want completed", updated.TaskStates["task1"])
	}
	if updated.AppliedResultIDs["task1"] != "res_new" {
		t.Errorf("applied result = %s, want res_new", updated.AppliedResultIDs["task1"])
	}
}

func TestRebuild_IgnoresUnknownTasks(t *testing.T) {
	dir, lm := setupRebuildDir(t)

	now := time.Now().UTC()
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_test_3",
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{"task1": model.StatusPending},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	writeRebuildState(t, dir, state)

	writeWorkerResult(t, dir, "worker1.yaml", model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_unknown",
				TaskID:    "unknown_task",
				CommandID: "cmd_test_3",
				Status:    model.StatusCompleted,
				CreatedAt: now.Format(time.RFC3339),
			},
		},
	})

	err := Rebuild(RebuildOptions{
		CommandID:  "cmd_test_3",
		MaestroDir: dir,
		LockMap:    lm,
	})
	if err != nil {
		t.Fatalf("Rebuild error: %v", err)
	}

	updated := loadState(t, dir, "cmd_test_3")
	if updated.TaskStates["task1"] != model.StatusPending {
		t.Errorf("task1 status should remain pending, got %s", updated.TaskStates["task1"])
	}
}

func TestRebuild_IgnoresOtherCommands(t *testing.T) {
	dir, lm := setupRebuildDir(t)

	now := time.Now().UTC()
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_test_4",
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{"task1": model.StatusPending},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	writeRebuildState(t, dir, state)

	// Result belongs to a different command
	writeWorkerResult(t, dir, "worker1.yaml", model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_other",
				TaskID:    "task1",
				CommandID: "cmd_other",
				Status:    model.StatusCompleted,
				CreatedAt: now.Format(time.RFC3339),
			},
		},
	})

	err := Rebuild(RebuildOptions{
		CommandID:  "cmd_test_4",
		MaestroDir: dir,
		LockMap:    lm,
	})
	if err != nil {
		t.Fatalf("Rebuild error: %v", err)
	}

	updated := loadState(t, dir, "cmd_test_4")
	if updated.TaskStates["task1"] != model.StatusPending {
		t.Errorf("task1 should remain pending when result is for another command, got %s", updated.TaskStates["task1"])
	}
}

func TestRebuild_DeterministicTieBreak(t *testing.T) {
	dir, lm := setupRebuildDir(t)

	sameTime := time.Now().UTC().Format(time.RFC3339)
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_test_5",
		TaskTracking: model.TaskTracking{
			TaskStates:       map[string]model.Status{"task1": model.StatusPending},
			AppliedResultIDs: make(map[string]string),
		},
		CreatedAt: sameTime,
		UpdatedAt: sameTime,
	}
	writeRebuildState(t, dir, state)

	// Two results with identical timestamps — higher ID should win.
	writeWorkerResult(t, dir, "worker1.yaml", model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_aaa",
				TaskID:    "task1",
				CommandID: "cmd_test_5",
				Status:    model.StatusFailed,
				CreatedAt: sameTime,
			},
			{
				ID:        "res_zzz",
				TaskID:    "task1",
				CommandID: "cmd_test_5",
				Status:    model.StatusCompleted,
				CreatedAt: sameTime,
			},
		},
	})

	err := Rebuild(RebuildOptions{
		CommandID:  "cmd_test_5",
		MaestroDir: dir,
		LockMap:    lm,
	})
	if err != nil {
		t.Fatalf("Rebuild error: %v", err)
	}

	updated := loadState(t, dir, "cmd_test_5")
	// res_zzz > res_aaa lexicographically, so completed should win.
	if updated.AppliedResultIDs["task1"] != "res_zzz" {
		t.Errorf("applied result = %s, want res_zzz (deterministic tie-break by ID)", updated.AppliedResultIDs["task1"])
	}
}

func TestRebuild_PrunesStaleAppliedResultIDs(t *testing.T) {
	dir, lm := setupRebuildDir(t)

	now := time.Now().UTC().Format(time.RFC3339)
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_test_stale",
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{"task_live": model.StatusPending},
			AppliedResultIDs: map[string]string{
				"task_live":    "res_live_old",
				"task_deleted": "res_ghost", // task no longer in TaskStates
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	writeRebuildState(t, dir, state)

	writeWorkerResult(t, dir, "worker1.yaml", model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_live_new",
				TaskID:    "task_live",
				CommandID: "cmd_test_stale",
				Status:    model.StatusCompleted,
				CreatedAt: now,
			},
		},
	})

	if err := Rebuild(RebuildOptions{
		CommandID:  "cmd_test_stale",
		MaestroDir: dir,
		LockMap:    lm,
	}); err != nil {
		t.Fatalf("Rebuild error: %v", err)
	}

	updated := loadState(t, dir, "cmd_test_stale")
	if _, exists := updated.AppliedResultIDs["task_deleted"]; exists {
		t.Errorf("stale applied_result_id for task_deleted should be pruned, got %q",
			updated.AppliedResultIDs["task_deleted"])
	}
	if updated.AppliedResultIDs["task_live"] != "res_live_new" {
		t.Errorf("task_live applied result = %s, want res_live_new",
			updated.AppliedResultIDs["task_live"])
	}
	if updated.TaskStates["task_live"] != model.StatusCompleted {
		t.Errorf("task_live status = %s, want completed", updated.TaskStates["task_live"])
	}
}

// TestRebuild_FailsPhantomPlannedTasksWithoutQueueEntry pins the
// 2026-04-29 e2e regression: a retry/repair task registered in
// state.TaskStates as `planned` but never enqueued to a worker queue
// would block phase termination forever, and `plan rebuild` reported
// no-op success without surfacing the inconsistency. Rebuild must
// detect such phantoms (planned in state, absent from every worker
// queue) and force them to failed so the phase can advance.
func TestRebuild_FailsPhantomPlannedTasksWithoutQueueEntry(t *testing.T) {
	dir, lm := setupRebuildDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_phantom",
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				"t1":         model.StatusCompleted,
				"t1_retry":   model.StatusPlanned, // phantom — never enqueued
				"t2_pending": model.StatusPending, // not planned — should be left alone
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	writeRebuildState(t, dir, state)

	// Write a queue with t1 only — t1_retry and t2_pending are absent.
	queue := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{ID: "t1", CommandID: "cmd_phantom", Status: model.StatusCompleted},
		},
	}
	queueData, err := yamlv3.Marshal(queue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queue", "worker1.yaml"), queueData, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Rebuild(RebuildOptions{
		CommandID:  "cmd_phantom",
		MaestroDir: dir,
		LockMap:    lm,
	}); err != nil {
		t.Fatalf("Rebuild error: %v", err)
	}

	updated := loadState(t, dir, "cmd_phantom")
	if got := updated.TaskStates["t1_retry"]; got != model.StatusFailed {
		t.Errorf("phantom t1_retry status = %s, want failed", got)
	}
	if reason := updated.CancelledReasons["t1_retry"]; reason == "" {
		t.Errorf("phantom CancelledReasons[t1_retry] should record the reason, got empty")
	}
	if got := updated.TaskStates["t1"]; got != model.StatusCompleted {
		t.Errorf("t1 status = %s, want completed (must not be touched)", got)
	}
	// t2_pending is at status `pending`, not `planned`, so the phantom
	// detector must leave it alone — pending is owned by the dispatch
	// path and force-failing here would race that owner.
	if got := updated.TaskStates["t2_pending"]; got != model.StatusPending {
		t.Errorf("t2_pending status = %s, want pending (only `planned` should be flagged as phantom)", got)
	}
}

// TestRebuild_KeepsPlannedTaskWithMatchingQueueEntry guards against
// the false-positive direction: a `planned` task that *does* have a
// corresponding queue entry must not be force-failed (it is in the
// normal pre-dispatch window).
func TestRebuild_KeepsPlannedTaskWithMatchingQueueEntry(t *testing.T) {
	dir, lm := setupRebuildDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_pre_dispatch",
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				"t1": model.StatusPlanned,
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	writeRebuildState(t, dir, state)

	queue := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{ID: "t1", CommandID: "cmd_pre_dispatch", Status: model.StatusPending},
		},
	}
	queueData, err := yamlv3.Marshal(queue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queue", "worker1.yaml"), queueData, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Rebuild(RebuildOptions{
		CommandID:  "cmd_pre_dispatch",
		MaestroDir: dir,
		LockMap:    lm,
	}); err != nil {
		t.Fatalf("Rebuild error: %v", err)
	}

	updated := loadState(t, dir, "cmd_pre_dispatch")
	if got := updated.TaskStates["t1"]; got != model.StatusPlanned {
		t.Errorf("planned task with queue entry status = %s, want planned", got)
	}
}

func TestRebuild_NilAppliedResultIDs(t *testing.T) {
	dir, lm := setupRebuildDir(t)

	now := time.Now().UTC().Format(time.RFC3339)
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_test_6",
		TaskTracking: model.TaskTracking{
			TaskStates:       map[string]model.Status{"task1": model.StatusPending},
			AppliedResultIDs: nil, // explicitly nil
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	writeRebuildState(t, dir, state)

	writeWorkerResult(t, dir, "worker1.yaml", model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_1",
				TaskID:    "task1",
				CommandID: "cmd_test_6",
				Status:    model.StatusCompleted,
				CreatedAt: now,
			},
		},
	})

	err := Rebuild(RebuildOptions{
		CommandID:  "cmd_test_6",
		MaestroDir: dir,
		LockMap:    lm,
	})
	if err != nil {
		t.Fatalf("Rebuild error: %v", err)
	}

	updated := loadState(t, dir, "cmd_test_6")
	if updated.AppliedResultIDs == nil {
		t.Fatal("AppliedResultIDs should be initialized (not nil)")
	}
	if updated.AppliedResultIDs["task1"] != "res_1" {
		t.Errorf("applied result = %s, want res_1", updated.AppliedResultIDs["task1"])
	}
}
