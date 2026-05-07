package daemon

import (
	"bytes"
	errorspkg "errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
	"github.com/msageha/maestro_v2/internal/testutil"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// --- checkCommandTasksTerminal tests ---

// TestCheckCommandTasksTerminal exercises the (allTerminal, hasFailed)
// outputs of checkCommandTasksTerminal across the matrix of task statuses.
func TestCheckCommandTasksTerminal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		queues          map[string][]model.Task
		commandID       string
		wantAllTerminal bool
		wantHasFailed   bool
		ignoreHasFailed bool // some legacy cases only assert allTerminal
	}{
		{
			name: "all completed",
			queues: map[string][]model.Task{
				"worker1": {
					{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
					{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
				},
			},
			commandID:       "cmd1",
			wantAllTerminal: true,
			wantHasFailed:   false,
		},
		{
			name: "has failed",
			queues: map[string][]model.Task{
				"worker1": {
					{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
					{ID: "t2", CommandID: "cmd1", Status: model.StatusFailed},
				},
			},
			commandID:       "cmd1",
			wantAllTerminal: true,
			wantHasFailed:   true,
		},
		{
			name: "has dead_letter",
			queues: map[string][]model.Task{
				"worker1": {
					{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
					{ID: "t2", CommandID: "cmd1", Status: model.StatusDeadLetter},
				},
			},
			commandID:       "cmd1",
			wantAllTerminal: true,
			wantHasFailed:   true,
		},
		{
			name: "in_progress blocks terminal",
			queues: map[string][]model.Task{
				"worker1": {
					{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
					{ID: "t2", CommandID: "cmd1", Status: model.StatusInProgress},
				},
			},
			commandID:       "cmd1",
			wantAllTerminal: false,
			ignoreHasFailed: true,
		},
		{
			name: "no tasks for command",
			queues: map[string][]model.Task{
				"worker1": {
					{ID: "t1", CommandID: "other_cmd", Status: model.StatusCompleted},
				},
			},
			commandID:       "cmd1",
			wantAllTerminal: false,
			ignoreHasFailed: true,
		},
		{
			name: "mixed commands — only cmd1 evaluated",
			queues: map[string][]model.Task{
				"worker1": {
					{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
					{ID: "t2", CommandID: "cmd2", Status: model.StatusInProgress},
				},
				"worker2": {
					{ID: "t3", CommandID: "cmd1", Status: model.StatusCancelled},
				},
			},
			commandID:       "cmd1",
			wantAllTerminal: true,
			wantHasFailed:   false,
		},
		{
			name: "across workers — pending blocks terminal",
			queues: map[string][]model.Task{
				"worker1": {
					{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
				},
				"worker2": {
					{ID: "t2", CommandID: "cmd1", Status: model.StatusPending},
				},
			},
			commandID:       "cmd1",
			wantAllTerminal: false,
			ignoreHasFailed: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qh := newMinimalQueueHandler(t)
			tqs := makeTaskQueues(tc.queues)
			allTerminal, hasFailed := qh.checkCommandTasksTerminal(tc.commandID, tqs)
			if allTerminal != tc.wantAllTerminal {
				t.Errorf("allTerminal = %v, want %v", allTerminal, tc.wantAllTerminal)
			}
			if !tc.ignoreHasFailed && hasFailed != tc.wantHasFailed {
				t.Errorf("hasFailed = %v, want %v", hasFailed, tc.wantHasFailed)
			}
		})
	}
}

// --- collectWorktreePublishAndCleanup tests ---

func TestCollectWorktreePublish_MergedIntegration(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnSuccess: true,
	})

	// Set up worktree state with "merged" integration
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)

	// Set up command state (no phases)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 1 {
		t.Fatalf("expected 1 publish item, got %d", len(publishes))
	}
	if publishes[0].CommandID != "cmd1" {
		t.Errorf("publish CommandID = %q, want cmd1", publishes[0].CommandID)
	}
	// No cleanup yet (cleanup happens after publish success in Phase B)
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipAlreadyPublished(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnSuccess: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusPublished)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items for already published, got %d", len(publishes))
	}
	// Should collect cleanup since it's published but not cleaned
	if len(cleanups) != 1 {
		t.Fatalf("expected 1 cleanup item, got %d", len(cleanups))
	}
	if cleanups[0].Reason != "success" {
		t.Errorf("cleanup reason = %q, want success", cleanups[0].Reason)
	}
}

func TestCollectWorktreePublish_SkipOnFailedTasks(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnFailure: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusFailed,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusFailed},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when tasks failed, got %d", len(publishes))
	}
	if len(cleanups) != 1 {
		t.Fatalf("expected 1 cleanup item for failed command, got %d", len(cleanups))
	}
	if cleanups[0].Reason != "failure" {
		t.Errorf("cleanup reason = %q, want failure", cleanups[0].Reason)
	}
}

// TestCollectWorktreePublish_IntegrationFailedEmitsSynthetic pins Bug-L:
// when a command's worktree integration has been marked Failed (typically
// by fast_track_cleanup), the publish gate must emit a synthetic planner
// result so R3PlannerQueue can walk the queue command terminal and the
// Orchestrator gets notified. Without this, Continuous Mode never advances
// iteration and the operator sees a stuck "in_progress" command despite
// no work being possible.
func TestCollectWorktreePublish_IntegrationFailedEmitsSynthetic(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnFailure: true,
	})

	// Integration Failed simulates a fast_track_cleanup that already
	// nuked the integration; no phases or tasks need to be in a specific
	// state — the gate decision keys off integration.Status alone for
	// this branch.
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusFailed)
	writeCommandStateWithPlanStatus(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, []model.Phase{
		{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted},
	}, model.PlanStatusSealed)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Fatalf("expected 0 publishes for failed integration, got %d", len(publishes))
	}
	if len(cleanups) != 1 || cleanups[0].Reason != "failure" {
		t.Fatalf("expected 1 failure cleanup, got %+v", cleanups)
	}

	resultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("expected synthetic planner result on integration_failed, got read error: %v", err)
	}
	var rf model.CommandResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("parse planner.yaml: %v", err)
	}
	count := 0
	for _, r := range rf.Results {
		if r.CommandID == "cmd1" && r.Status == model.StatusFailed {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 synthetic_failure for cmd1, got %d", count)
	}

	// Idempotency check: second invocation should not duplicate.
	_, _ = qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	data2, _ := os.ReadFile(resultPath)
	var rf2 model.CommandResultFile
	if err := yamlv3.Unmarshal(data2, &rf2); err != nil {
		t.Fatalf("parse planner.yaml after idempotent retry: %v", err)
	}
	count2 := 0
	for _, r := range rf2.Results {
		if r.CommandID == "cmd1" && r.Status == model.StatusFailed {
			count2++
		}
	}
	if count2 != 1 {
		t.Errorf("synthetic_failure must be idempotent; got %d after second call", count2)
	}
}

// TestSyntheticPlannerResult_PartialIntegrationMergeReported pins the
// post-2026-05-06 P1 v5 fix: when a command failed at the publish gate
// but worker commits have already rolled onto the integration branch
// (data-loss avoidance), the synthetic planner result's summary must
// surface that partial integration so the Orchestrator does not see a
// generic "failed" with `integration_status=merged` and conclude that
// the main branch was published.
func TestSyntheticPlannerResult_PartialIntegrationMergeReported(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		PlanStatus:    model.PlanStatusFailed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				"task_x": model.StatusCompleted,
				"task_y": model.StatusFailed,
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state); err != nil {
		t.Fatalf("write state: %v", err)
	}
	// Worktree state shows the integration branch was rolled forward
	// (worker_committed / worker_merged ran), but the publish gate
	// blocked the main publish because the phase failed.
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)

	if !qh.writeSyntheticFailedPlannerResult("cmd1", "phase_failed_publish_blocked") {
		t.Fatalf("expected synthetic write to succeed")
	}
	data, err := os.ReadFile(filepath.Join(maestroDir, "results", "planner.yaml"))
	if err != nil {
		t.Fatalf("read planner.yaml: %v", err)
	}
	var rf model.CommandResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("parse planner.yaml: %v", err)
	}
	if len(rf.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(rf.Results))
	}
	r := rf.Results[0]
	if !strings.Contains(r.Summary, "integration_state=merged") {
		t.Errorf("summary should report integration_state=merged, got %q", r.Summary)
	}
	if !strings.Contains(r.Summary, "main publish skipped") {
		t.Errorf("summary should clarify that main publish was skipped, got %q", r.Summary)
	}
}

// TestSyntheticPlannerResult_PopulatesTaskStatsFromState pins the
// post-2026-05-06 P1 v5 fix: synthetic planner results must surface the
// actual TaskStats and per-task statuses derived from the command state
// file, instead of an empty `tasks: []` payload that misleads the
// Orchestrator into thinking no work was performed.
func TestSyntheticPlannerResult_PopulatesTaskStatsFromState(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	// Seed state: 3 tasks, mixed outcomes — 2 completed, 1 failed.
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		PlanStatus:    model.PlanStatusFailed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				"task_aa": model.StatusCompleted,
				"task_bb": model.StatusCompleted,
				"task_cc": model.StatusFailed,
			},
			CancelledReasons: map[string]string{
				"task_cc": "blocked_pane_timeout: worker prompt for .claude/verify.sh edit",
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "commands", "cmd1.yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	wrote := qh.writeSyntheticFailedPlannerResult("cmd1", "phase_failed_publish_blocked")
	if !wrote {
		t.Fatalf("expected wrote=true for first synthetic emission")
	}

	resultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read planner.yaml: %v", err)
	}
	var rf model.CommandResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("parse planner.yaml: %v", err)
	}
	if len(rf.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(rf.Results))
	}
	r := rf.Results[0]
	if r.CommandID != "cmd1" || r.Status != model.StatusFailed {
		t.Fatalf("unexpected result header: id=%s status=%s", r.CommandID, r.Status)
	}
	// TaskStats must reflect the state file (not zero).
	if r.TaskStats.Total != 3 || r.TaskStats.Completed != 2 || r.TaskStats.Failed != 1 {
		t.Errorf("task_stats: got %+v, want total=3 completed=2 failed=1", r.TaskStats)
	}
	if len(r.Tasks) != 3 {
		t.Fatalf("expected 3 tasks in result, got %d", len(r.Tasks))
	}
	taskStatuses := map[string]model.Status{}
	for _, ct := range r.Tasks {
		taskStatuses[ct.TaskID] = ct.Status
	}
	for id, want := range map[string]model.Status{
		"task_aa": model.StatusCompleted,
		"task_bb": model.StatusCompleted,
		"task_cc": model.StatusFailed,
	} {
		if got := taskStatuses[id]; got != want {
			t.Errorf("task %s status: got %s, want %s", id, got, want)
		}
	}
	// Failed task summary must include the cancellation reason from state.
	for _, ct := range r.Tasks {
		if ct.TaskID == "task_cc" && ct.Summary == "" {
			t.Errorf("failed task %s should carry the state-recorded reason in summary, got empty", ct.TaskID)
		}
	}
	// Top-level summary must surface the failed task ID for quick triage.
	if !strings.Contains(r.Summary, "task_cc") {
		t.Errorf("summary should mention the failed task id, got %q", r.Summary)
	}
	if !strings.Contains(r.Summary, "phase_failed_publish_blocked") {
		t.Errorf("summary should retain the synthetic reason, got %q", r.Summary)
	}
}

// TestCollectWorktreePublish_RecoveryAfterFailedPhasePublishes verifies the
// race-safe publish gate: when a verify-repair (or planner-driven retry)
// replaces a failed task with a successor that completes, the publish
// gate honors the live derivation — not the stale persisted plan_status.
// With retry_lineage threaded through DeriveStatus the predecessor's
// superseded-cancelled status no longer counts as a failure, the dead
// phase fragment is recognised as recovered, and the command
// publishes normally rather than being poisoned by synthetic_failure.
func TestCollectWorktreePublish_RecoveryAfterFailedPhasePublishes(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnSuccess: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	// Realistic shape after a verify-repair recovery: predecessor t1 was
	// cancelled with superseded_by_verify_repair, t1_recover replaced it in
	// RequiredTaskIDs (so the live state has only t1_recover required), and
	// retry_lineage records the supersession. plan_status is still sealed —
	// R4PlanStatus has not yet propagated the recovery to disk; the publish
	// gate must derive completion directly.
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs: []string{"t1_recover"},
			TaskStates: map[string]model.Status{
				"t1":         model.StatusCancelled,
				"t1_recover": model.StatusCompleted,
			},
			CancelledReasons: map[string]string{
				"t1": "superseded_by_verify_repair: repair_task=t1_recover reason=verify_failed",
			},
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: map[string]string{
				"t1_recover": "t1",
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusFailed},
				{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusCompleted},
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "commands", "cmd1.yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write command state: %v", err)
	}

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCancelled},
			{ID: "t1_recover", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, _ := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 1 {
		t.Fatalf("expected publish to proceed when retry_lineage successor completed despite stale failed phase, got %d publishes", len(publishes))
	}

	// Verify the synthetic_failure result was NOT written: the plan succeeded.
	resultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	if _, err := os.Stat(resultPath); err == nil {
		var rf model.CommandResultFile
		data, _ := os.ReadFile(resultPath)
		if err := yamlv3.Unmarshal(data, &rf); err == nil {
			for _, r := range rf.Results {
				if r.CommandID == "cmd1" && r.Status == model.StatusFailed {
					t.Errorf("synthetic_failure should NOT be written when retry_lineage successor completed; got %+v", r)
				}
			}
		}
	}
}

// TestCollectWorktreePublish_FailedPlanWritesSynthetic verifies the inverse:
// when plan_status is genuinely failed (not recovered), the synthetic_failure
// write fires so R3/R4 can walk the queue + state to terminal.
func TestCollectWorktreePublish_FailedPlanWritesSynthetic(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnFailure: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandStateWithPlanStatus(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusFailed,
	}, []model.Phase{
		{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusFailed},
	}, model.PlanStatusFailed)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusFailed},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Fatalf("expected 0 publishes for genuinely failed plan, got %d", len(publishes))
	}
	if len(cleanups) != 1 || cleanups[0].Reason != "failure" {
		t.Fatalf("expected 1 failure cleanup, got %+v", cleanups)
	}

	// Verify the synthetic_failure result IS written exactly once.
	resultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("expected results/planner.yaml to be written: %v", err)
	}
	var rf model.CommandResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("parse planner.yaml: %v", err)
	}
	syntheticCount := 0
	for _, r := range rf.Results {
		if r.CommandID == "cmd1" && r.Status == model.StatusFailed {
			syntheticCount++
		}
	}
	if syntheticCount != 1 {
		t.Errorf("expected exactly 1 synthetic_failure for cmd1, got %d", syntheticCount)
	}

	// Second invocation should be a no-op (idempotent) — synthetic stays at 1.
	_, _ = qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	data2, _ := os.ReadFile(resultPath)
	var rf2 model.CommandResultFile
	_ = yamlv3.Unmarshal(data2, &rf2)
	syntheticCount2 := 0
	for _, r := range rf2.Results {
		if r.CommandID == "cmd1" && r.Status == model.StatusFailed {
			syntheticCount2++
		}
	}
	if syntheticCount2 != 1 {
		t.Errorf("synthetic_failure must be idempotent across scans; got %d after second call", syntheticCount2)
	}
}

func TestCollectWorktreePublish_NoCleanupOnFailureDisabled(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnFailure: false, // disabled
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusFailed,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusFailed},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items when CleanupOnFailure=false, got %d", len(cleanups))
	}
}

// TestCollectWorktreePublish_QuarantinedNoPublishNoCleanup verifies that
// quarantined integrations produce no publish and no cleanup items, and that
// CleanupTempPublishBranch is called (best-effort) to clean up any leaked
// _publish branch without removing worktrees.
func TestCollectWorktreePublish_QuarantinedNoPublishNoCleanup(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnFailure: false,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusQuarantined)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items for quarantined status, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 full cleanup items for quarantined status, got %d", len(cleanups))
	}
}

// TestCollectWorktreePublish_QuarantinedWithCleanupOnFailure verifies that
// quarantined integrations do NOT trigger full worktree cleanup even when
// cleanup_on_failure is true (quarantine preserves worktrees for inspection).
func TestCollectWorktreePublish_QuarantinedWithCleanupOnFailure(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnFailure: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusQuarantined)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items for quarantined status, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 full cleanup items for quarantined status (worktrees preserved), got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipNotReady(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	// Integration still in "created" — not ready for publish
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items for created status, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

// TestCollectWorktreePublish_BlockedByCommitFailedWorkers verifies that publish
// is blocked when worktree state still records workers whose auto-commit failed,
// even if integration status reached Merged via the workers that did commit.
func TestCollectWorktreePublish_BlockedByCommitFailedWorkers(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnSuccess: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)

	// Re-load and inject CommitFailedWorkers, then re-write.
	statePath := filepath.Join(maestroDir, "state", "worktrees", "cmd1.yaml")
	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("load worktree state: %v", err)
	}
	state.CommitFailedWorkers = []string{"worker2"}
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("rewrite worktree state: %v", err)
	}

	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when CommitFailedWorkers is non-empty, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipConflict(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusConflict)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items for conflict status, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_SkipNonTerminalPhases(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	// Command has phases, one still active
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, []model.Phase{
		{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted},
		{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusActive},
	})

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when phases not terminal, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_AllPhasesTerminal(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, []model.Phase{
		{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted},
		{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusCompleted},
	})

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, _ := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 1 {
		t.Fatalf("expected 1 publish item when all phases terminal, got %d", len(publishes))
	}
}

func TestCollectWorktreePublish_SkipNonTerminalTasks(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusInProgress,
	}, nil)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusInProgress},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when tasks not terminal, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items, got %d", len(cleanups))
	}
}

func TestCollectWorktreePublish_PhaseErrorFailsClosed(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	// No command state file → stateReader.GetCommandPhases will return error

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when phase check errors, got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items when phase check errors, got %d", len(cleanups))
	}
}

// --- Test helpers ---

// scanPhaseStateReader implements StateReader for scan phase tests.
type scanPhaseStateReader struct {
	maestroDir string
}

func (r *scanPhaseStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return "", err
	}
	s, ok := state.TaskStates[taskID]
	if !ok {
		return "", ErrStateNotFound
	}
	return s, nil
}

func (r *scanPhaseStateReader) GetEffectiveTaskStatus(commandID, taskID string) (model.Status, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return "", err
	}
	if _, ok := state.TaskStates[taskID]; !ok {
		return "", ErrStateNotFound
	}
	return plan.EffectiveStatus(taskID, state.TaskStates, state.RetryLineage), nil
}

func (r *scanPhaseStateReader) GetEffectiveTaskStatusForCompletion(commandID, taskID string) (model.Status, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return "", err
	}
	if _, ok := state.TaskStates[taskID]; !ok {
		return "", ErrStateNotFound
	}
	return plan.EffectiveStatusForCompletion(taskID, state), nil
}

func (r *scanPhaseStateReader) GetCommandPhases(commandID string) ([]PhaseInfo, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return nil, err
	}
	// Build name→ID lookup so DependsOnPhases (names) can be resolved into
	// the PhaseInfo.DependsOn (IDs) form the production reader uses.
	phaseNameToID := make(map[string]string, len(state.Phases))
	for _, p := range state.Phases {
		phaseNameToID[p.Name] = p.PhaseID
	}
	var phases []PhaseInfo
	for _, p := range state.Phases {
		var depIDs []string
		for _, depName := range p.DependsOnPhases {
			if id, ok := phaseNameToID[depName]; ok {
				depIDs = append(depIDs, id)
			}
		}
		phases = append(phases, PhaseInfo{
			ID:        p.PhaseID,
			Name:      p.Name,
			Status:    p.Status,
			DependsOn: depIDs,
		})
	}
	return phases, nil
}

func (r *scanPhaseStateReader) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	return nil, nil
}

func (r *scanPhaseStateReader) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	return nil
}

func (r *scanPhaseStateReader) SetPhaseCancelledReason(commandID, phaseID string, reason *string) error {
	return nil
}

func (r *scanPhaseStateReader) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	// Persist the update so phantom-task force-fail tests can verify the
	// resulting state. Reads use the same loadState helper, so the cycle
	// stays consistent.
	state, err := r.loadState(commandID)
	if err != nil {
		return err
	}
	if state.TaskStates == nil {
		state.TaskStates = map[string]model.Status{}
	}
	state.TaskStates[taskID] = newStatus
	if cancelledReason != "" {
		if state.CancelledReasons == nil {
			state.CancelledReasons = map[string]string{}
		}
		state.CancelledReasons[taskID] = cancelledReason
	}
	path := filepath.Join(r.maestroDir, "state", "commands", commandID+".yaml")
	data, err := yamlv3.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (r *scanPhaseStateReader) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	return false, false, nil
}

func (r *scanPhaseStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	return false, nil
}

func (r *scanPhaseStateReader) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	return &model.CircuitBreakerState{}, nil
}

func (r *scanPhaseStateReader) HasNonTerminalTaskState(commandID string) (bool, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return false, err
	}
	for _, status := range state.TaskStates {
		if !model.IsTerminal(status) {
			return true, nil
		}
	}
	return false, nil
}

func (r *scanPhaseStateReader) GetNonTerminalTaskStates(commandID string) (map[string]model.Status, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]model.Status, len(state.TaskStates))
	for taskID, status := range state.TaskStates {
		if !model.IsTerminal(status) {
			out[taskID] = status
		}
	}
	return out, nil
}

func (r *scanPhaseStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	return nil
}

func (r *scanPhaseStateReader) MarkAwaitingFillStallNotified(string, string, string) error {
	return nil
}

func (r *scanPhaseStateReader) MarkCircuitBreakerProgress(string) error { return nil }

func (r *scanPhaseStateReader) loadState(commandID string) (*model.CommandState, error) {
	path := filepath.Join(r.maestroDir, "state", "commands", commandID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrStateNotFound
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func setupScanPhaseTestDir(t *testing.T) string {
	t.Helper()
	return testutil.SetupDir(t)
}

func newMinimalQueueHandler(t *testing.T) *QueueHandler {
	t.Helper()
	maestroDir := setupScanPhaseTestDir(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
	}
	return NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
}

func newScanPhaseTestQueueHandler(t *testing.T, maestroDir string, wtConfig model.WorktreeConfig) *QueueHandler {
	t.Helper()
	cfg := model.Config{
		Agents:   model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher:  model.WatcherConfig{DispatchLeaseSec: 300},
		Worktree: wtConfig,
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	// Wire state reader
	reader := &scanPhaseStateReader{maestroDir: maestroDir}
	qh.SetStateReader(reader)

	// Wire worktree manager with minimal config (only needs to read state files)
	wm := NewWorktreeManager(maestroDir, wtConfig, log.New(&bytes.Buffer{}, "", 0), LogLevelError)
	qh.SetWorktreeManager(wm)

	return qh
}

func makeTaskQueues(workerTasks map[string][]model.Task) map[string]*taskQueueEntry {
	tqs := make(map[string]*taskQueueEntry)
	for workerID, tasks := range workerTasks {
		path := "/fake/" + workerID + ".yaml"
		tqs[path] = &taskQueueEntry{
			Queue: model.TaskQueue{
				SchemaVersion: 1,
				FileType:      "queue_task",
				Tasks:         tasks,
			},
			Path: path,
		}
	}
	return tqs
}

func writeWorktreeState(t *testing.T, maestroDir, commandID string, integrationStatus model.IntegrationStatus) {
	t.Helper()
	state := model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID: commandID,
			Branch:    "maestro/" + commandID + "/integration",
			BaseSHA:   "abc123",
			Status:    integrationStatus,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		Workers: []model.WorktreeState{
			{
				CommandID: commandID,
				WorkerID:  "worker1",
				Path:      "/fake/worktree/worker1",
				Branch:    "maestro/" + commandID + "/worker1",
				BaseSHA:   "abc123",
				Status:    model.WorktreeStatusActive,
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	path := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write worktree state: %v", err)
	}
}

func markCommitFailedWorkerForTest(t *testing.T, maestroDir, commandID, workerID, updatedAt string) {
	t.Helper()
	path := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	var state model.WorktreeCommandState
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read worktree state: %v", err)
	}
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse worktree state: %v", err)
	}
	state.CommitFailedWorkers = []string{workerID}
	state.UpdatedAt = updatedAt
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write worktree state: %v", err)
	}
}

func writeCommandState(t *testing.T, maestroDir, commandID string, taskStates map[string]model.Status, phases []model.Phase) {
	t.Helper()
	writeCommandStateWithPlanStatus(t, maestroDir, commandID, taskStates, phases, model.PlanStatusSealed)
}

// writeCommandStateWithPlanStatus is the variant used by tests that need to
// drive the plan_status reconciliation paths (e.g. recovery after a failed
// phase has been superseded by an add-task and plan_status flipped to
// completed).
func writeCommandStateWithPlanStatus(t *testing.T, maestroDir, commandID string, taskStates map[string]model.Status, phases []model.Phase, planStatus model.PlanStatus) {
	t.Helper()
	var requiredIDs []string
	for id := range taskStates {
		requiredIDs = append(requiredIDs, id)
	}
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    planStatus,
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs: requiredIDs,
			TaskStates:      taskStates,
		},
		PhaseTracking: model.PhaseTracking{
			Phases: phases,
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	path := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write command state: %v", err)
	}
}

// --- collectWorktreePhaseMerges phase 0 件 fallback tests ---

// noPhasesFallbackFixture seeds: worktree state (Integration=integrationStatus,
// 1 worker), command state with phases=nil, and a task queue with the given
// task statuses. Returns the qh + task queue map ready for the helper call.
func noPhasesFallbackFixture(
	t *testing.T,
	integrationStatus model.IntegrationStatus,
	taskStates map[string]model.Status,
	mergedPhases map[string]string,
) (*QueueHandler, map[string]*taskQueueEntry) {
	t.Helper()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})
	writeWorktreeState(t, maestroDir, "cmd1", integrationStatus)
	if mergedPhases != nil {
		statePath := filepath.Join(maestroDir, "state", "worktrees", "cmd1.yaml")
		state, err := qh.worktreeManager.GetCommandState("cmd1")
		if err != nil {
			t.Fatalf("get worktree state: %v", err)
		}
		state.MergedPhases = mergedPhases
		if err := yamlutil.AtomicWrite(statePath, state); err != nil {
			t.Fatalf("rewrite worktree state: %v", err)
		}
	}
	writeCommandState(t, maestroDir, "cmd1", taskStates, nil)

	tasks := make([]model.Task, 0, len(taskStates))
	for id, st := range taskStates {
		tasks = append(tasks, model.Task{ID: id, CommandID: "cmd1", Status: st})
	}
	tqs := makeTaskQueues(map[string][]model.Task{"worker1": tasks})
	return qh, tqs
}

func TestCollectWorktreePhaseMerges_NoPhasesFallback(t *testing.T) {
	t.Parallel()
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusCreated, map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, nil)

	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 1 {
		t.Fatalf("expected 1 implicit merge item, got %d", len(items))
	}
	if items[0].PhaseID != "__implicit_phase" {
		t.Errorf("PhaseID = %q, want __implicit_phase", items[0].PhaseID)
	}
	if items[0].CommandID != "cmd1" {
		t.Errorf("CommandID = %q, want cmd1", items[0].CommandID)
	}
	if len(items[0].WorkerIDs) != 1 || items[0].WorkerIDs[0] != "worker1" {
		t.Errorf("WorkerIDs = %v, want [worker1]", items[0].WorkerIDs)
	}
}

func TestCollectWorktreePhaseMerges_NoPhasesSkipsOnFailure(t *testing.T) {
	t.Parallel()
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusCreated, map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusFailed,
	}, nil)
	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if items != nil {
		t.Errorf("expected nil items when a task failed, got %v", items)
	}
}

func TestCollectWorktreePhaseMerges_NoPhasesIncrementalWhenSomeTerminal(t *testing.T) {
	t.Parallel()
	// Implicit-phase merge runs incrementally: a single completed task is
	// enough to start collecting merge work, so a worker whose dependent
	// task completed early can hand off its output to integration before
	// the rest of the command finishes.
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusCreated, map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusInProgress,
	}, nil)
	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 1 {
		t.Fatalf("expected 1 incremental merge item when at least one task completed, got %d", len(items))
	}
	if items[0].PhaseID != "__implicit_phase" {
		t.Errorf("PhaseID = %q, want __implicit_phase", items[0].PhaseID)
	}
}

func TestCollectWorktreePhaseMerges_NoPhasesAllowsRecollectAfterMerged(t *testing.T) {
	t.Parallel()
	// integration_status=merged is not a hard skip for implicit-phase
	// commands: new completions keep flowing in while the command runs
	// and each one needs the merge collector to pick it up.
	// MergeToIntegration's per-worker idempotency keeps duplicate scans
	// cheap.
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusMerged, map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)
	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 1 {
		t.Fatalf("expected merge item to be re-collected after merged status (incremental merge), got %d", len(items))
	}
}

func TestCollectImplicitWorktreeMerge_PartialMerge(t *testing.T) {
	t.Parallel()
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusPartialMerge, map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, nil)
	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 1 {
		t.Fatalf("expected 1 implicit merge item for partial_merge, got %d", len(items))
	}
	if items[0].PhaseID != "__implicit_phase" {
		t.Errorf("PhaseID = %q, want __implicit_phase", items[0].PhaseID)
	}
}

func TestCollectImplicitWorktreeMerge_Failed(t *testing.T) {
	t.Parallel()
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusFailed, map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCompleted,
	}, nil)
	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 1 {
		t.Fatalf("expected 1 implicit merge item for failed, got %d", len(items))
	}
	if items[0].PhaseID != "__implicit_phase" {
		t.Errorf("PhaseID = %q, want __implicit_phase", items[0].PhaseID)
	}
}

func TestCollectImplicitWorktreeMerge_AlreadyMerged_IncrementalReentry(t *testing.T) {
	t.Parallel()
	// __implicit_phase membership in MergedPhases is not a gate.
	// Incremental merge expects to re-enter on every completed task; the
	// actual idempotency lives inside MergeToIntegration where up-to-date
	// worker branches are skipped without spending git ops.
	qh, tqs := noPhasesFallbackFixture(t, model.IntegrationStatusPartialMerge, map[string]model.Status{
		"t1": model.StatusCompleted,
	}, map[string]string{"__implicit_phase": "2026-01-01T00:00:00Z"})
	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 1 {
		t.Fatalf("expected re-entry merge item even after __implicit_phase recorded as merged, got %d", len(items))
	}
}

// TestCollectImplicitWorktreeMerge_FiltersConflictResolvingWorkers verifies
// that the Phase A collector yields to the resume-merge pipeline when the
// integration is in a recovery state and there are workers in
// Conflict/Resolving. Those workers are owned by ResumeMerge /
// AutoRecoverAfterResolution; running Phase B's auto-commit + auto-merge in
// parallel would re-attempt unrelated workers' merges (pinning fresh
// merge_conflict signals to the wrong phase) and risk promoting a
// pre-resolution worker branch to integrated. The gate added in
// collectWorktreePhaseMerges suppresses the entire merge collection in this
// state — Phase B simply does nothing for the command until the recovery
// pipeline drives the integration back to a non-recovery status.
func TestCollectImplicitWorktreeMerge_FiltersConflictResolvingWorkers(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	// Seed a worktree state with three workers: Active, Conflict, Resolving.
	state := model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     "cmd1",
		Integration: model.IntegrationState{
			CommandID: "cmd1",
			Branch:    "maestro/cmd1/integration",
			BaseSHA:   "abc123",
			Status:    model.IntegrationStatusPartialMerge,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		Workers: []model.WorktreeState{
			{CommandID: "cmd1", WorkerID: "worker1", Path: "/fake/w1", Branch: "maestro/cmd1/worker1", BaseSHA: "abc123", Status: model.WorktreeStatusActive, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
			{CommandID: "cmd1", WorkerID: "worker2", Path: "/fake/w2", Branch: "maestro/cmd1/worker2", BaseSHA: "abc123", Status: model.WorktreeStatusResolving, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
			{CommandID: "cmd1", WorkerID: "worker3", Path: "/fake/w3", Branch: "maestro/cmd1/worker3", BaseSHA: "abc123", Status: model.WorktreeStatusConflict, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "worktrees", "cmd1.yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write worktree state: %v", err)
	}
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)
	tqs := makeTaskQueues(map[string][]model.Task{"worker1": {{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted}}})

	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if items != nil {
		t.Fatalf("expected nil items while integration is in partial_merge with conflict/resolving workers (resume-merge pipeline owns recovery), got %v", items)
	}
}

// TestStepWorktreeStallDetection_NoPhasesFastPath verifies the case 5
// fast-path: phase 0 件 + Integration.Status==created でタイムアウト経過後に
// stall シグナルが発火する。タイムアウト前には発火しない。
func TestStepWorktreeStallDetection_NoPhasesFastPath(t *testing.T) {
	t.Parallel()
	// Use a timestamp beyond the stall timeout so the signal fires.
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	qh, s := stallTestSetup(t, past, model.IntegrationStatusCreated, false)

	// Overwrite command state with no phases.
	writeCommandState(t, qh.maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	qh.stepWorktreeStallDetection(s)

	if len(s.signals.Data.Signals) != 1 {
		t.Fatalf("expected 1 fast-path stall signal, got %d", len(s.signals.Data.Signals))
	}
	sig := s.signals.Data.Signals[0]
	if sig.Reason != "integration_stalled_no_phases:created" {
		t.Errorf("reason = %q, want integration_stalled_no_phases:created", sig.Reason)
	}
	if sig.Kind != "worktree_stalled" || sig.CommandID != "cmd1" {
		t.Errorf("unexpected signal: %+v", sig)
	}
	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !state.Integration.StallSignaled {
		t.Errorf("StallSignaled flag was not persisted")
	}
}

// --- classifyCommitError unit tests ---
//
// With the policy/sensitive-file gates removed from the orchestrator,
// classifyCommitError no longer differentiates structured failure
// classes — it surfaces a generic class plus the underlying message.
// The remaining test pins that behaviour.

func TestClassifyCommitError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"generic", errorspkg.New("boom"), "generic:boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyCommitError(tc.err)
			if got != tc.want {
				t.Errorf("classifyCommitError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// (Removed: TestPhaseBC_CommitFailure_FlowTable.
// The orchestrator no longer enforces commit policies — sensitive-file lists,
// max_files, expected_paths and message regex have all moved to the worker
// environment. There is therefore no path through periodicScanPhaseB that
// rejects a worker's dirty changes for policy reasons; the equivalent
// failure modes are exercised by the worktree-package tests for genuine
// git-level errors.)

// TestPeriodicScanPhaseC_MergeConflictSignalStructuredFields verifies that
// MVP-1 structured conflict fields (BaseRef/OursRef/TheirsRef/Files) are
// propagated from MergeConflict into the emitted PlannerSignal, while the
// legacy free-form Message field continues to embed the same values for
// backward compatibility with CSV-style consumers.
func TestPeriodicScanPhaseC_MergeConflictSignalStructuredFields(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: false}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	commandID := "cmd_mc_struct"
	pa := phaseAResult{scanStart: time.Now()}
	pb := phaseBResult{
		worktreeMerges: []worktreeMergeResult{{
			Item: worktreeMergeItem{
				CommandID: commandID,
				PhaseID:   "phase1",
				WorkerIDs: []string{"worker1"},
			},
			Conflicts: []model.MergeConflict{{
				WorkerID:      "worker1",
				ConflictFiles: []string{"a.go", "b.go"},
				BaseRef:       "base_sha",
				OursRef:       "ours_sha",
				TheirsRef:     "theirs_sha",
			}},
		}},
	}

	qh.periodicScanPhaseC(pa, pb)

	signalQueue, _, _ := qh.queueStore.LoadPlannerSignalQueue()
	var found *model.PlannerSignal
	for i := range signalQueue.Signals {
		s := &signalQueue.Signals[i]
		if s.Kind == "merge_conflict" && s.CommandID == commandID {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("merge_conflict signal not found: %+v", signalQueue.Signals)
	}
	if found.ConflictBaseRef != "base_sha" || found.ConflictOursRef != "ours_sha" || found.ConflictTheirsRef != "theirs_sha" {
		t.Errorf("structured refs mismatch: base=%q ours=%q theirs=%q",
			found.ConflictBaseRef, found.ConflictOursRef, found.ConflictTheirsRef)
	}
	if len(found.ConflictFiles) != 2 || found.ConflictFiles[0] != "a.go" || found.ConflictFiles[1] != "b.go" {
		t.Errorf("ConflictFiles mismatch: %v", found.ConflictFiles)
	}
	if found.WorkerID != "worker1" {
		t.Errorf("WorkerID = %q, want worker1", found.WorkerID)
	}
	// Backward-compat: legacy CSV-style Message must still embed refs and files.
	if !strings.Contains(found.Message, "base:base_sha") ||
		!strings.Contains(found.Message, "ours:ours_sha") ||
		!strings.Contains(found.Message, "theirs:theirs_sha") ||
		!strings.Contains(found.Message, "a.go, b.go") {
		t.Errorf("legacy Message missing structured fields: %q", found.Message)
	}
}

// TestPlannerSignal_StructuredConflictFields_RoundTrip verifies that the new
// MVP-1 fields survive a YAML marshal/unmarshal round trip and that absent
// values stay omitted (preserving on-disk compatibility for non-conflict
// signal kinds).
func TestPlannerSignal_StructuredConflictFields_RoundTrip(t *testing.T) {
	t.Parallel()
	orig := model.PlannerSignal{
		Kind:              "merge_conflict",
		CommandID:         "cmd1",
		PhaseID:           "phase1",
		WorkerID:          "worker1",
		Message:           "[maestro] kind:merge_conflict ...",
		ConflictBaseRef:   "base_sha",
		ConflictOursRef:   "ours_sha",
		ConflictTheirsRef: "theirs_sha",
		ConflictFiles:     []string{"a.go", "b.go"},
		CreatedAt:         "2026-01-01T00:00:00Z",
		UpdatedAt:         "2026-01-01T00:00:00Z",
	}
	data, err := yamlv3.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got model.PlannerSignal
	if err := yamlv3.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ConflictBaseRef != orig.ConflictBaseRef ||
		got.ConflictOursRef != orig.ConflictOursRef ||
		got.ConflictTheirsRef != orig.ConflictTheirsRef ||
		len(got.ConflictFiles) != 2 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Non-conflict signals: empty fields must be omitted from YAML output.
	plain := model.PlannerSignal{
		Kind:      "commit_failed",
		CommandID: "cmd1",
		PhaseID:   "phase1",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	data2, err := yamlv3.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	out := string(data2)
	for _, key := range []string{"conflict_base_ref", "conflict_ours_ref", "conflict_theirs_ref", "conflict_files"} {
		if strings.Contains(out, key) {
			t.Errorf("expected %q to be omitted from non-conflict signal yaml: %s", key, out)
		}
	}
}

// TestStepPlannerSignalsDeferred_ImplicitPhaseNotOrphaned verifies that signals
// with __implicit_phase bypass the phase existence check and are retained as
// long as the command itself exists.
func TestStepPlannerSignalsDeferred_ImplicitPhaseNotOrphaned(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	// Create command state without any phases (simulates no-phase command)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	now := "2026-01-01T00:00:00Z"
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "merge_conflict",
				CommandID: "cmd1",
				PhaseID:   "__implicit_phase",
				Message:   "conflict in implicit phase",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}

	dirty := false
	work := &deferredWork{}
	qh.stepPlannerSignalsDeferred(&sq, &dirty, work, model.CommandQueue{})

	// Signal must be retained (not orphaned)
	if len(sq.Signals) != 1 {
		t.Fatalf("expected 1 signal retained, got %d", len(sq.Signals))
	}
	if sq.Signals[0].PhaseID != "__implicit_phase" {
		t.Errorf("PhaseID = %q, want __implicit_phase", sq.Signals[0].PhaseID)
	}
	// Signal must be deferred for delivery
	if len(work.signals) != 1 {
		t.Fatalf("expected 1 deferred signal, got %d", len(work.signals))
	}
}

// TestPeriodicScanPhaseC_ConflictDispatchSucceedsOnFirstCycle verifies that
// when a merge_conflict signal is created in the same Phase C cycle, the C1
// opportunistic dispatch succeeds because signals are pre-flushed to disk
// before DispatchConflictResolution reads them via YAMLSignalStore.
func TestPeriodicScanPhaseC_ConflictDispatchSucceedsOnFirstCycle(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: true}
	lockMap := lock.NewMutexMap()
	cfg := model.Config{
		Agents:   model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher:  model.WatcherConfig{DispatchLeaseSec: 300},
		Worktree: wtCfg,
	}
	qh := NewQueueHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	reader := &scanPhaseStateReader{maestroDir: maestroDir}
	qh.SetStateReader(reader)

	wm := NewWorktreeManager(maestroDir, wtCfg, log.New(&bytes.Buffer{}, "", 0), LogLevelError)
	wm.SetSignalStore(NewYAMLSignalStore(maestroDir, lockMap))
	qh.SetWorktreeManager(wm)

	commandID := "cmd_c1_dispatch"
	workerID := "worker1"
	phaseID := "phase1"

	// Write worktree state with worker in conflict status so
	// DispatchConflictResolution can transition it to resolving.
	state := model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID: commandID,
			Branch:    "maestro/" + commandID + "/integration",
			BaseSHA:   "abc123",
			Status:    model.IntegrationStatusConflict,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		Workers: []model.WorktreeState{
			{
				CommandID: commandID,
				WorkerID:  workerID,
				Path:      "/fake/worktree/" + workerID,
				Branch:    "maestro/" + commandID + "/" + workerID,
				BaseSHA:   "abc123",
				Status:    model.WorktreeStatusConflict,
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write worktree state: %v", err)
	}

	// Simulate Phase B merge result that detects a conflict.
	pa := phaseAResult{scanStart: time.Now()}
	pb := phaseBResult{
		worktreeMerges: []worktreeMergeResult{{
			Item: worktreeMergeItem{
				CommandID: commandID,
				PhaseID:   phaseID,
				WorkerIDs: []string{workerID},
			},
			Conflicts: []model.MergeConflict{{
				WorkerID:      workerID,
				ConflictFiles: []string{"a.go"},
				BaseRef:       "base_sha",
				OursRef:       "ours_sha",
				TheirsRef:     "theirs_sha",
			}},
		}},
	}

	qh.periodicScanPhaseC(pa, pb)

	// Verify: signal should exist on disk with ResolutionState = "dispatched"
	// (C1 dispatch succeeded on the same cycle the signal was created).
	signalQueue, _, _ := qh.queueStore.LoadPlannerSignalQueue()
	var found *model.PlannerSignal
	for i := range signalQueue.Signals {
		s := &signalQueue.Signals[i]
		if s.Kind == "merge_conflict" && s.CommandID == commandID {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("merge_conflict signal not found in queue")
	}
	if found.ResolutionState != "dispatched" {
		t.Errorf("ResolutionState = %q, want %q (C1 dispatch should succeed on first cycle)",
			found.ResolutionState, "dispatched")
	}

	// Verify: worker should have transitioned from conflict to resolving.
	wtState, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("load worktree state: %v", err)
	}
	for _, ws := range wtState.Workers {
		if ws.WorkerID == workerID {
			if ws.Status != model.WorktreeStatusResolving {
				t.Errorf("worker status = %q, want %q", ws.Status, model.WorktreeStatusResolving)
			}
			return
		}
	}
	t.Fatalf("worker %s not found in worktree state", workerID)
}

// TestPeriodicScanPhaseC_PublishCompletedSignal verifies that a successful
// worktree publish emits an *informational* publish_completed signal to the
// Planner. The signal itself does not instruct `plan complete` (Bug B
// double-fire fix): the Planner is the sole caller of `plan complete` via
// its envelope instruction, and the daemon's deferredPlanCompleter
// auto-finalises any deferred intent after publish succeeds. The signal
// remains useful as a post-publish trigger for `--run-on-main` verification.
func TestPeriodicScanPhaseC_PublishCompletedSignal(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: false}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	commandID := "cmd_pub_ok"
	pa := phaseAResult{scanStart: time.Now()}
	pb := phaseBResult{
		worktreePublishes: []worktreePublishResult{{
			Item:  worktreePublishItem{CommandID: commandID, PublishMessage: "test publish"},
			Error: nil, // success
		}},
	}

	qh.periodicScanPhaseC(pa, pb)

	signalQueue, _, _ := qh.queueStore.LoadPlannerSignalQueue()
	var found *model.PlannerSignal
	for i := range signalQueue.Signals {
		s := &signalQueue.Signals[i]
		if s.Kind == "publish_completed" && s.CommandID == commandID {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("publish_completed signal not found; signals: %+v", signalQueue.Signals)
	}
	if !strings.Contains(found.Message, "kind:publish_completed") {
		t.Errorf("Message missing kind tag: %q", found.Message)
	}
	if !strings.Contains(found.Message, "command_id:"+commandID) {
		t.Errorf("Message missing command_id: %q", found.Message)
	}
	// Informational signal must confirm publish success but must NOT direct
	// the Planner to call `plan complete` (double-fire guard).
	if !strings.Contains(found.Message, "successfully published") {
		t.Errorf("Message should confirm publish success: %q", found.Message)
	}
	if strings.Contains(found.Message, "Call `maestro plan complete` to finalise") {
		t.Errorf("Message must not instruct Planner to call plan complete (Bug B regression): %q", found.Message)
	}
}

// TestPeriodicScanPhaseC_PublishCompletedEmittedAfterDeferredFinalize asserts
// the symmetry contract: when the deferred plan-complete intent is
// finalised inside Phase C, the daemon emits publish_completed so the
// Planner sees parity with the non-deferred path. The signal is tagged
// Reason="deferred_complete_finalized" to bypass the Phase A dispatch-
// time stale filter (exercised by
// TestStepPlannerSignalsDeferred_PublishCompletedRetainedAfterDeferredFinalize).
func TestPeriodicScanPhaseC_PublishCompletedEmittedAfterDeferredFinalize(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: false}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	commandID := "cmd_deferred_finalize"

	// Wire a deferredPlanCompleter that simulates a real Complete() flushing
	// a deferred intent: it flips the queue to terminal AND returns
	// (true, nil) to signal "deferred finalisation occurred".
	cqPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_planner",
		Commands: []model.Command{
			{ID: commandID, Status: model.StatusInProgress},
		},
	}
	if err := yamlutil.AtomicWrite(cqPath, cq); err != nil {
		t.Fatalf("write command queue: %v", err)
	}
	qh.deferredPlanCompleter = func(cmdID string) (bool, error) {
		terminalCQ := model.CommandQueue{
			SchemaVersion: 1,
			FileType:      "queue_planner",
			Commands: []model.Command{
				{ID: cmdID, Status: model.StatusCompleted},
			},
		}
		if err := yamlutil.AtomicWrite(cqPath, terminalCQ); err != nil {
			t.Fatalf("simulate deferred plan complete: %v", err)
		}
		return true, nil
	}

	pa := phaseAResult{scanStart: time.Now()}
	pb := phaseBResult{
		worktreePublishes: []worktreePublishResult{{
			Item:  worktreePublishItem{CommandID: commandID, PublishMessage: "test publish"},
			Error: nil,
		}},
	}

	qh.periodicScanPhaseC(pa, pb)

	signalQueue, _, _ := qh.queueStore.LoadPlannerSignalQueue()
	var found *model.PlannerSignal
	for i := range signalQueue.Signals {
		s := &signalQueue.Signals[i]
		if s.Kind == "publish_completed" && s.CommandID == commandID {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("publish_completed signal must be emitted after deferred finalisation; signals: %+v", signalQueue.Signals)
	}
	if found.Reason != "deferred_complete_finalized" {
		t.Errorf("Reason = %q, want %q (so Phase A dispatch keeps the signal)", found.Reason, "deferred_complete_finalized")
	}
}

// TestPeriodicScanPhaseC_PublishFailedNoSignal verifies that a failed worktree
// publish does NOT emit any signal (neither publish_failed nor publish_completed).
// The Daemon handles publish retries automatically via recordPublishFailure /
// backoff, and R8 (NotifyPublishQuarantined) escalates to the Planner when
// retries are exhausted. Emitting publish_failed would cause the Planner to
// attempt plan_submit / add_retry_task which fails because the Worker task
// already completed successfully.
func TestPeriodicScanPhaseC_PublishFailedNoSignal(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: false}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	commandID := "cmd_pub_fail"
	pa := phaseAResult{scanStart: time.Now()}
	pb := phaseBResult{
		worktreePublishes: []worktreePublishResult{{
			Item:  worktreePublishItem{CommandID: commandID, PublishMessage: "test publish"},
			Error: fmt.Errorf("push rejected"),
		}},
	}

	qh.periodicScanPhaseC(pa, pb)

	signalQueue, _, _ := qh.queueStore.LoadPlannerSignalQueue()
	for _, s := range signalQueue.Signals {
		if s.CommandID != commandID {
			continue
		}
		if s.Kind == "publish_failed" {
			t.Errorf("publish_failed signal should NOT be emitted (daemon handles retry); got: %+v", s)
		}
		if s.Kind == "publish_completed" {
			t.Errorf("publish_completed signal should NOT be emitted on failure; got: %+v", s)
		}
	}
}

// TestPeriodicScanPhaseC_PublishCompletedSuppressedWhenTerminal verifies that
// the publish_completed signal is NOT emitted when the command is already in a
// terminal status in the command queue. This prevents the Planner from issuing
// a redundant second plan complete call.
func TestPeriodicScanPhaseC_PublishCompletedSuppressedWhenTerminal(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: false}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	commandID := "cmd_already_done"

	// Pre-populate the command queue with a terminal (completed) command so
	// that Phase C sees the command as already finished.
	cqPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_planner",
		Commands: []model.Command{
			{ID: commandID, Status: model.StatusCompleted},
		},
	}
	if err := yamlutil.AtomicWrite(cqPath, cq); err != nil {
		t.Fatalf("write command queue: %v", err)
	}

	pa := phaseAResult{scanStart: time.Now()}
	pb := phaseBResult{
		worktreePublishes: []worktreePublishResult{{
			Item:  worktreePublishItem{CommandID: commandID, PublishMessage: "test publish"},
			Error: nil, // success
		}},
	}

	qh.periodicScanPhaseC(pa, pb)

	signalQueue, _, _ := qh.queueStore.LoadPlannerSignalQueue()
	for _, s := range signalQueue.Signals {
		if s.Kind == "publish_completed" && s.CommandID == commandID {
			t.Errorf("publish_completed signal should be suppressed for terminal command; got: %+v", s)
		}
	}
}

// TestPeriodicScanPhaseC_PublishCompletedSuppressedWhenTerminalOnDisk verifies
// that a publish_completed signal is suppressed even when the in-memory command
// queue snapshot (loaded at the start of Phase C) is stale, as long as the
// command is terminal on disk. This exercises the race where plan complete is
// called concurrently during Phase B and the on-disk queue is updated after
// Phase C's initial load.
func TestPeriodicScanPhaseC_PublishCompletedSuppressedWhenTerminalOnDisk(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: false}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	commandID := "cmd_race"

	// Start with a NON-terminal command in the queue on disk (simulating the
	// state at Phase C's initial queue load).
	cqPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_planner",
		Commands: []model.Command{
			{ID: commandID, Status: model.StatusInProgress},
		},
	}
	if err := yamlutil.AtomicWrite(cqPath, cq); err != nil {
		t.Fatalf("write command queue: %v", err)
	}

	// Wire a deferredPlanCompleter that simulates an external plan complete:
	// it writes the terminal status to the queue file on disk (as plan.Complete
	// would) and returns (false, nil) — no deferred intent.
	qh.deferredPlanCompleter = func(cmdID string) (bool, error) {
		terminalCQ := model.CommandQueue{
			SchemaVersion: 1,
			FileType:      "queue_planner",
			Commands: []model.Command{
				{ID: cmdID, Status: model.StatusCompleted},
			},
		}
		if err := yamlutil.AtomicWrite(cqPath, terminalCQ); err != nil {
			t.Fatalf("simulate plan complete: %v", err)
		}
		return false, nil
	}

	pa := phaseAResult{scanStart: time.Now()}
	pb := phaseBResult{
		worktreePublishes: []worktreePublishResult{{
			Item:  worktreePublishItem{CommandID: commandID, PublishMessage: "test publish"},
			Error: nil,
		}},
	}

	qh.periodicScanPhaseC(pa, pb)

	// The publish_completed signal must be suppressed because the disk reload
	// inside applyPublishResultSignals detects the terminal status.
	signalQueue, _, _ := qh.queueStore.LoadPlannerSignalQueue()
	for _, s := range signalQueue.Signals {
		if s.Kind == "publish_completed" && s.CommandID == commandID {
			t.Errorf("publish_completed signal should be suppressed for terminal command on disk; got: %+v", s)
		}
	}
}

// TestStepPlannerSignalsDeferred_PublishCompletedStaleWhenTerminal verifies that
// a publish_completed signal is removed as stale when the command is already in
// a terminal status in the command queue. This closes the race window where plan
// complete is called between Phase C signal creation and Phase A evaluation.
func TestStepPlannerSignalsDeferred_PublishCompletedStaleWhenTerminal(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	// Create command state so orphan check passes
	writeCommandState(t, maestroDir, "cmd_done", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	now := "2026-01-01T00:00:00Z"
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "publish_completed",
				CommandID: "cmd_done",
				Message:   "integration branch published",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}

	// Command is already terminal in the queue (both in-memory and on disk
	// so the reload inside stepPlannerSignalsDeferred finds it).
	commandQueue := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_planner",
		Commands: []model.Command{
			{ID: "cmd_done", Status: model.StatusCompleted},
		},
	}
	cqPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	if err := yamlutil.AtomicWrite(cqPath, commandQueue); err != nil {
		t.Fatalf("write command queue: %v", err)
	}

	dirty := false
	work := &deferredWork{}
	qh.stepPlannerSignalsDeferred(&sq, &dirty, work, commandQueue)

	// Signal must be removed as stale
	if len(sq.Signals) != 0 {
		t.Errorf("expected 0 signals (stale removed), got %d: %+v", len(sq.Signals), sq.Signals)
	}
	if !dirty {
		t.Error("expected dirty=true after removing stale signal")
	}
	// Signal must NOT be deferred for delivery
	if len(work.signals) != 0 {
		t.Errorf("expected 0 deferred signals, got %d", len(work.signals))
	}
}

// TestStepPlannerSignalsDeferred_PublishCompletedRetainedAfterDeferredFinalize
// asserts the symmetry-fix counterpart on the dispatch side: a publish_completed
// signal tagged with Reason="deferred_complete_finalized" must be retained for
// delivery even when the command is terminal in the queue. The tag indicates
// that the signal was emitted intentionally from the Phase C deferred path
// after the daemon flipped the command terminal, and the Planner should
// receive it for parity with the non-deferred publish_completed path.
func TestStepPlannerSignalsDeferred_PublishCompletedRetainedAfterDeferredFinalize(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	writeCommandState(t, maestroDir, "cmd_deferred_done", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	now := "2026-01-01T00:00:00Z"
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "publish_completed",
				CommandID: "cmd_deferred_done",
				Reason:    "deferred_complete_finalized",
				Message:   "integration branch published",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}

	commandQueue := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_planner",
		Commands: []model.Command{
			{ID: "cmd_deferred_done", Status: model.StatusCompleted},
		},
	}
	cqPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	if err := yamlutil.AtomicWrite(cqPath, commandQueue); err != nil {
		t.Fatalf("write command queue: %v", err)
	}

	dirty := false
	work := &deferredWork{}
	qh.stepPlannerSignalsDeferred(&sq, &dirty, work, commandQueue)

	if len(sq.Signals) != 1 {
		t.Errorf("expected 1 retained signal (tagged signals must survive terminal-state filter), got %d: %+v",
			len(sq.Signals), sq.Signals)
	}
	// Signal must be deferred for delivery to the Planner.
	if len(work.signals) != 1 {
		t.Errorf("expected 1 deferred signal (tagged signal must reach Planner), got %d", len(work.signals))
	}
}

// TestStepPlannerSignalsDeferred_PublishFailedSuppressed verifies that a
// publish_failed signal is removed from the queue and NOT deferred for delivery.
// The Daemon handles publish retries internally; delivering publish_failed to
// the Planner would cause plan_submit / add_retry_task errors.
func TestStepPlannerSignalsDeferred_PublishFailedSuppressed(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	// Create command state so orphan check passes
	writeCommandState(t, maestroDir, "cmd_pub", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	now := "2026-01-01T00:00:00Z"
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "publish_failed",
				CommandID: "cmd_pub",
				Message:   "[maestro] kind:publish_failed command_id:cmd_pub\nerror: merge conflict",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}

	dirty := false
	work := &deferredWork{}
	qh.stepPlannerSignalsDeferred(&sq, &dirty, work, model.CommandQueue{})

	// Signal must be removed
	if len(sq.Signals) != 0 {
		t.Errorf("expected 0 signals (publish_failed suppressed), got %d: %+v", len(sq.Signals), sq.Signals)
	}
	if !dirty {
		t.Error("expected dirty=true after suppressing publish_failed signal")
	}
	// Signal must NOT be deferred for delivery
	if len(work.signals) != 0 {
		t.Errorf("expected 0 deferred signals, got %d", len(work.signals))
	}
}

// TestStepPlannerSignalsDeferred_PublishFailedSuppressedWithOtherSignalsRetained
// verifies that suppressing publish_failed does not affect other signal types.
func TestStepPlannerSignalsDeferred_PublishFailedSuppressedWithOtherSignalsRetained(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	writeCommandState(t, maestroDir, "cmd_mix", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	now := "2026-01-01T00:00:00Z"
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "publish_failed",
				CommandID: "cmd_mix",
				Message:   "publish failed",
				CreatedAt: now,
				UpdatedAt: now,
			},
			{
				Kind:      "circuit_breaker_tripped",
				CommandID: "cmd_mix",
				Message:   "progress timeout",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}

	dirty := false
	work := &deferredWork{}
	qh.stepPlannerSignalsDeferred(&sq, &dirty, work, model.CommandQueue{})

	// Only publish_failed should be removed; circuit_breaker_tripped retained
	if len(sq.Signals) != 1 {
		t.Fatalf("expected 1 signal retained, got %d: %+v", len(sq.Signals), sq.Signals)
	}
	if sq.Signals[0].Kind != "circuit_breaker_tripped" {
		t.Errorf("retained signal kind = %q, want circuit_breaker_tripped", sq.Signals[0].Kind)
	}
	// Only circuit_breaker_tripped should be deferred for delivery
	if len(work.signals) != 1 {
		t.Fatalf("expected 1 deferred signal, got %d", len(work.signals))
	}
	if work.signals[0].Kind != "circuit_breaker_tripped" {
		t.Errorf("deferred signal kind = %q, want circuit_breaker_tripped", work.signals[0].Kind)
	}
}

// TestStepPlannerSignalsDeferred_PublishCompletedRetainedWhenNonTerminal verifies
// that a publish_completed signal is retained and deferred for delivery when the
// command is not yet terminal.
func TestStepPlannerSignalsDeferred_PublishCompletedRetainedWhenNonTerminal(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	// Create command state so orphan check passes
	writeCommandState(t, maestroDir, "cmd_active", map[string]model.Status{
		"t1": model.StatusCompleted,
	}, nil)

	now := "2026-01-01T00:00:00Z"
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind:      "publish_completed",
				CommandID: "cmd_active",
				Message:   "integration branch published",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}

	// Command is NOT terminal in the queue
	commandQueue := model.CommandQueue{
		Commands: []model.Command{
			{ID: "cmd_active", Status: model.StatusInProgress},
		},
	}

	dirty := false
	work := &deferredWork{}
	qh.stepPlannerSignalsDeferred(&sq, &dirty, work, commandQueue)

	// Signal must be retained
	if len(sq.Signals) != 1 {
		t.Fatalf("expected 1 signal retained, got %d", len(sq.Signals))
	}
	// Signal must be deferred for delivery
	if len(work.signals) != 1 {
		t.Fatalf("expected 1 deferred signal, got %d", len(work.signals))
	}
	if work.signals[0].Kind != "publish_completed" {
		t.Errorf("deferred signal kind = %q, want publish_completed", work.signals[0].Kind)
	}
}

// TestPeriodicScanPhaseC_DeferredComplete verifies that when a deferred plan
// complete intent exists for a command, a successful publish triggers
// auto-completion AND emits a publish_completed signal tagged with
// Reason="deferred_complete_finalized" (symmetry with the non-deferred
// publish_completed path). The tag lets the dispatch-side stale filter
// keep the signal so the Planner sees a uniform notification regardless
// of which path finalised the command.
func TestPeriodicScanPhaseC_DeferredComplete(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: false}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	commandID := "cmd_deferred_ok"

	// Wire a deferred plan completer that records the call and returns success
	completedCommands := make(map[string]bool)
	qh.deferredPlanCompleter = func(cmdID string) (bool, error) {
		completedCommands[cmdID] = true
		return true, nil
	}

	pa := phaseAResult{scanStart: time.Now()}
	pb := phaseBResult{
		worktreePublishes: []worktreePublishResult{{
			Item:  worktreePublishItem{CommandID: commandID, PublishMessage: "test"},
			Error: nil,
		}},
	}

	qh.periodicScanPhaseC(pa, pb)

	// Deferred completer should have been called
	if !completedCommands[commandID] {
		t.Error("deferredPlanCompleter was not called for the command")
	}

	// publish_completed signal should be emitted with the deferred-finalized
	// tag so the Planner gets parity with the non-deferred path.
	signalQueue, _, _ := qh.queueStore.LoadPlannerSignalQueue()
	var found *model.PlannerSignal
	for i := range signalQueue.Signals {
		s := &signalQueue.Signals[i]
		if s.Kind == "publish_completed" && s.CommandID == commandID {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("publish_completed signal must still be emitted after deferred complete (symmetry); signals: %+v", signalQueue.Signals)
	}
	if found.Reason != "deferred_complete_finalized" {
		t.Errorf("Reason = %q, want %q so the dispatch-time stale filter retains the signal",
			found.Reason, "deferred_complete_finalized")
	}
}

// TestPeriodicScanPhaseC_DeferredComplete_Fallback verifies that when the
// deferred plan completer returns (false, nil) — meaning no deferred intent
// exists — the daemon falls back to emitting a publish_completed signal.
func TestPeriodicScanPhaseC_DeferredComplete_Fallback(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: false}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	commandID := "cmd_no_deferred"

	// Wire a deferred plan completer that returns false (no deferred intent)
	qh.deferredPlanCompleter = func(cmdID string) (bool, error) {
		return false, nil
	}

	pa := phaseAResult{scanStart: time.Now()}
	pb := phaseBResult{
		worktreePublishes: []worktreePublishResult{{
			Item:  worktreePublishItem{CommandID: commandID, PublishMessage: "test"},
			Error: nil,
		}},
	}

	qh.periodicScanPhaseC(pa, pb)

	// publish_completed signal SHOULD be emitted as fallback
	signalQueue, _, _ := qh.queueStore.LoadPlannerSignalQueue()
	var found bool
	for _, s := range signalQueue.Signals {
		if s.Kind == "publish_completed" && s.CommandID == commandID {
			found = true
			break
		}
	}
	if !found {
		t.Error("publish_completed signal should be emitted when no deferred intent exists")
	}
}

// --- Regression tests for conflict-skipped-worker merge gate (Bug #1) ---

// TestApplyMergeResultSignals_SkipsMarkPhaseMergedOnPartialMerge verifies that
// when a merge result has no NEW conflicts and no commit failures, but the
// integration status is still PartialMerge (because a previous conflict worker
// hasn't been re-merged yet), MarkPhaseMerged is NOT called. Without this gate
// the phase merge record would race ahead of the integration branch and
// incorrectly unblock downstream phase transitions.
func TestApplyMergeResultSignals_SkipsMarkPhaseMergedOnPartialMerge(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	// Seed integration state as PartialMerge (one worker still unresolved).
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusPartialMerge)

	// Successful-looking merge result: no conflicts, no commit failures. This
	// is exactly the shape Phase B produces on a re-merge pass when the
	// remaining conflict worker is still in Resolving state.
	merges := []worktreeMergeResult{{
		Item: worktreeMergeItem{CommandID: "cmd1", PhaseID: "p1"},
	}}
	sq := &model.PlannerSignalQueue{SchemaVersion: 1, FileType: "planner_signal_queue"}
	dirty := false
	idx := buildSignalIndex(sq.Signals)

	qh.applyMergeResultSignals(merges, sq, &dirty, idx, "2026-04-22T00:00:00Z")

	// Reload state — MarkPhaseMerged must not have recorded p1.
	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if _, merged := state.MergedPhases["p1"]; merged {
		t.Errorf("phase p1 was marked merged despite integration status=partial_merge")
	}
}

// TestApplyMergeResultSignals_MarksMergedWhenIntegrationMerged verifies that
// the new gate does not regress the happy path: when integration status is
// Merged (all workers integrated), MarkPhaseMerged is called as before.
func TestApplyMergeResultSignals_MarksMergedWhenIntegrationMerged(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)

	merges := []worktreeMergeResult{{
		Item: worktreeMergeItem{CommandID: "cmd1", PhaseID: "p1"},
	}}
	sq := &model.PlannerSignalQueue{SchemaVersion: 1, FileType: "planner_signal_queue"}
	dirty := false
	idx := buildSignalIndex(sq.Signals)

	qh.applyMergeResultSignals(merges, sq, &dirty, idx, "2026-04-22T00:00:00Z")

	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if _, merged := state.MergedPhases["p1"]; !merged {
		t.Errorf("phase p1 should be marked merged when integration status=merged, got MergedPhases=%v",
			state.MergedPhases)
	}
}

// TestApplyMergeResultSignals_MarksMergedOnNoOpMergeWhenCreated verifies the
// rerun2 regression: a phase whose tasks produce no code edits (e.g. a
// research-only foundation phase) triggers a merge attempt that reports
// `no_commits_to_merge`. `determineMergeOutcome` reverts Integration.Status
// from Merging back to its pre-merge value (Created, for the first phase).
// Before the fix, applyMergeResultSignals bailed out because
// Integration.Status != Merged, leaving the phase OUT of MergedPhases and
// causing the merge collector to re-emit a fresh merge item on every
// subsequent scan. That retry loop eventually landed during the next phase's
// in-flight worker edit window and silently absorbed the later phase's dirty
// changes into the earlier phase's integration commit — exactly the
// `_integration has only alpha` corruption observed in production.
func TestApplyMergeResultSignals_MarksMergedOnNoOpMergeWhenCreated(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	// Simulate post-no-op-merge state: integration reverted to Created because
	// the phase had nothing to merge. No workers in Conflict/Resolving.
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	merges := []worktreeMergeResult{{
		Item: worktreeMergeItem{CommandID: "cmd1", PhaseID: "foundation"},
	}}
	sq := &model.PlannerSignalQueue{SchemaVersion: 1, FileType: "planner_signal_queue"}
	dirty := false
	idx := buildSignalIndex(sq.Signals)

	qh.applyMergeResultSignals(merges, sq, &dirty, idx, "2026-04-25T00:00:00Z")

	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if _, merged := state.MergedPhases["foundation"]; !merged {
		t.Errorf("phase foundation must be recorded as merged on no-op merge with integration status=created; MergedPhases=%v", state.MergedPhases)
	}
}

// TestApplyMergeResultSignals_DefersOnConflictStatus verifies the new
// status-allowlist gate still defers when the integration is in Conflict (or
// PartialMerge / Failed). A no-conflict no-op merge result on top of a broken
// integration must NOT retroactively mark the phase merged.
func TestApplyMergeResultSignals_DefersOnConflictStatus(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusConflict)

	merges := []worktreeMergeResult{{
		Item: worktreeMergeItem{CommandID: "cmd1", PhaseID: "p1"},
	}}
	sq := &model.PlannerSignalQueue{SchemaVersion: 1, FileType: "planner_signal_queue"}
	dirty := false
	idx := buildSignalIndex(sq.Signals)

	qh.applyMergeResultSignals(merges, sq, &dirty, idx, "2026-04-25T00:00:00Z")

	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if _, merged := state.MergedPhases["p1"]; merged {
		t.Errorf("phase p1 must NOT be marked merged while integration status=conflict; MergedPhases=%v", state.MergedPhases)
	}
}

// --- Regression tests for publish-skip phase-level judgment (Bug #2) ---

// TestCollectWorktreePublish_FailedPhaseBlocksPublish verifies that when a
// phased command has any phase ending non-successfully (Failed / Cancelled /
// TimedOut), publish is skipped even though all tasks are in terminal state.
// This is the "phase is the authoritative unit" contract.
func TestCollectWorktreePublish_FailedPhaseBlocksPublish(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnFailure: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusCancelled, // was failed, got cancelled by retry handler
	}, []model.Phase{
		{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusFailed},
	})

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusCancelled},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items when a phase is failed, got %d", len(publishes))
	}
	if len(cleanups) != 1 {
		t.Fatalf("expected 1 cleanup item (failure), got %d", len(cleanups))
	}
	if cleanups[0].Reason != "failure" {
		t.Errorf("cleanup reason = %q, want failure", cleanups[0].Reason)
	}
}

// TestCollectWorktreePublish_StaleFailedTaskWithCompletedPhaseStillPublishes
// verifies the Bug #2 regression scenario: a task queue retains a Failed task
// (e.g., the original verification task that the Planner later replaced via
// add_retry_task) but the phase's authoritative status is Completed — which
// means the retry ran and succeeded. Publish MUST proceed; the old behavior
// of returning early on task-level hasFailed permanently poisoned the command.
func TestCollectWorktreePublish_StaleFailedTaskWithCompletedPhaseStillPublishes(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnSuccess: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	// Phase is Completed (retry succeeded) and tasks are all terminal. The
	// Failed task in the queue is historical — it was superseded by t2_retry.
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1":       model.StatusCompleted,
		"t2":       model.StatusFailed, // leftover in state — not updated by old-style retry
		"t2_retry": model.StatusCompleted,
	}, []model.Phase{
		{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted},
	})

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusFailed},
			{ID: "t2_retry", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 1 {
		t.Fatalf("expected 1 publish item (phase completed ⇒ retry succeeded), got %d", len(publishes))
	}
	if len(cleanups) != 0 {
		t.Errorf("expected 0 cleanup items when publish is queued, got %d", len(cleanups))
	}
}

// TestCollectWorktreePublish_NoPhasesFallsBackToTaskLevelFailure preserves the
// implicit-phase behavior: commands with no phases continue to use the task
// queue's hasFailed signal (there is no phase-level authoritative source in
// that case).
func TestCollectWorktreePublish_NoPhasesFallsBackToTaskLevelFailure(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{
		Enabled:          true,
		CleanupOnFailure: true,
	})

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1", map[string]model.Status{
		"t1": model.StatusCompleted,
		"t2": model.StatusFailed,
	}, nil) // no phases

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusFailed},
		},
	})

	publishes, cleanups := qh.collectWorktreePublishAndCleanup("cmd1", "", tqs)
	if len(publishes) != 0 {
		t.Errorf("expected 0 publish items (no phases, task failed), got %d", len(publishes))
	}
	if len(cleanups) != 1 {
		t.Fatalf("expected 1 cleanup item (failure), got %d", len(cleanups))
	}
}
