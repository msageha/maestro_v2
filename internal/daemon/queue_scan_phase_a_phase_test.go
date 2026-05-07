package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestPhaseTransitionPriority(t *testing.T) {
	tests := []struct {
		status   model.PhaseStatus
		expected int
	}{
		{model.PhaseStatusFailed, 0},
		{model.PhaseStatusCancelled, 1},
		{model.PhaseStatusTimedOut, 2},
		{model.PhaseStatusCompleted, 3},
		{model.PhaseStatusAwaitingFill, 4},
		{model.PhaseStatus("unknown"), 5},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			got := phaseTransitionPriority(tt.status)
			if got != tt.expected {
				t.Errorf("phaseTransitionPriority(%q) = %d, want %d", tt.status, got, tt.expected)
			}
		})
	}
}

func TestSortPhaseTransitions(t *testing.T) {
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted, Reason: "all done"},
		{PhaseID: "p2", NewStatus: model.PhaseStatusFailed, Reason: "task failed"},
		{PhaseID: "p3", NewStatus: model.PhaseStatusAwaitingFill, Reason: "needs fill"},
		{PhaseID: "p4", NewStatus: model.PhaseStatusTimedOut, Reason: "deadline"},
		{PhaseID: "p5", NewStatus: model.PhaseStatusCancelled, Reason: "user cancel"},
	}

	sortPhaseTransitions(transitions)

	expectedOrder := []string{"p2", "p5", "p4", "p1", "p3"}
	for i, id := range expectedOrder {
		if transitions[i].PhaseID != id {
			t.Errorf("sorted[%d].PhaseID = %s, want %s", i, transitions[i].PhaseID, id)
		}
	}
}

func TestSortPhaseTransitions_StableOrder(t *testing.T) {
	// Two transitions with the same priority should preserve original order.
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted, Reason: "first"},
		{PhaseID: "p2", NewStatus: model.PhaseStatusCompleted, Reason: "second"},
		{PhaseID: "p3", NewStatus: model.PhaseStatusFailed, Reason: "third"},
	}

	sortPhaseTransitions(transitions)

	// Failed first, then two completed in original order.
	expected := []string{"p3", "p1", "p2"}
	for i, id := range expected {
		if transitions[i].PhaseID != id {
			t.Errorf("sorted[%d].PhaseID = %s, want %s", i, transitions[i].PhaseID, id)
		}
	}
}

func TestDeduplicatePhaseTransitions(t *testing.T) {
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusFailed, Reason: "task failed"},
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted, Reason: "all done"},
		{PhaseID: "p2", NewStatus: model.PhaseStatusTimedOut, Reason: "deadline"},
	}

	result := deduplicatePhaseTransitions(transitions)

	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2", len(result))
	}
	if result[0].PhaseID != "p1" || result[0].NewStatus != model.PhaseStatusFailed {
		t.Errorf("result[0] = {%s, %s}, want {p1, failed}", result[0].PhaseID, result[0].NewStatus)
	}
	if result[1].PhaseID != "p2" || result[1].NewStatus != model.PhaseStatusTimedOut {
		t.Errorf("result[1] = {%s, %s}, want {p2, timed_out}", result[1].PhaseID, result[1].NewStatus)
	}
}

func TestSortAndDeduplicatePhaseTransitions(t *testing.T) {
	// End-to-end: sort then dedup should keep highest priority per phase.
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted, Reason: "all done"},
		{PhaseID: "p1", NewStatus: model.PhaseStatusFailed, Reason: "task failed"},
		{PhaseID: "p2", NewStatus: model.PhaseStatusAwaitingFill, Reason: "needs fill"},
		{PhaseID: "p2", NewStatus: model.PhaseStatusTimedOut, Reason: "deadline"},
		{PhaseID: "p3", NewStatus: model.PhaseStatusCancelled, Reason: "user cancel"},
	}

	sortPhaseTransitions(transitions)
	result := deduplicatePhaseTransitions(transitions)

	if len(result) != 3 {
		t.Fatalf("len(result) = %d, want 3", len(result))
	}

	// p1 should be failed (priority 0), not completed (priority 3)
	if result[0].PhaseID != "p1" || result[0].NewStatus != model.PhaseStatusFailed {
		t.Errorf("result[0] = {%s, %s}, want {p1, failed}", result[0].PhaseID, result[0].NewStatus)
	}
	// p3 should be cancelled (priority 1)
	if result[1].PhaseID != "p3" || result[1].NewStatus != model.PhaseStatusCancelled {
		t.Errorf("result[1] = {%s, %s}, want {p3, cancelled}", result[1].PhaseID, result[1].NewStatus)
	}
	// p2 should be timed_out (priority 2), not awaiting_fill (priority 4)
	if result[2].PhaseID != "p2" || result[2].NewStatus != model.PhaseStatusTimedOut {
		t.Errorf("result[2] = {%s, %s}, want {p2, timed_out}", result[2].PhaseID, result[2].NewStatus)
	}
}

func TestDeduplicatePhaseTransitions_Empty(t *testing.T) {
	result := deduplicatePhaseTransitions(nil)
	if len(result) != 0 {
		t.Errorf("len(result) = %d, want 0", len(result))
	}
}

func TestSortPhaseTransitions_Empty(t *testing.T) {
	// Should not panic on empty slice.
	sortPhaseTransitions(nil)
	sortPhaseTransitions([]PhaseTransitionResult{})
}

func TestDeduplicatePhaseTransitions_NoDuplicates(t *testing.T) {
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusFailed},
		{PhaseID: "p2", NewStatus: model.PhaseStatusCompleted},
		{PhaseID: "p3", NewStatus: model.PhaseStatusTimedOut},
	}

	result := deduplicatePhaseTransitions(transitions)

	if len(result) != 3 {
		t.Fatalf("len(result) = %d, want 3", len(result))
	}
	for i, tr := range transitions {
		if result[i].PhaseID != tr.PhaseID {
			t.Errorf("result[%d].PhaseID = %s, want %s", i, result[i].PhaseID, tr.PhaseID)
		}
	}
}

// TestDeduplicatePhaseTransitions_SingleElement verifies single element passes through.
func TestDeduplicatePhaseTransitions_SingleElement(t *testing.T) {
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted, Reason: "done"},
	}
	result := deduplicatePhaseTransitions(transitions)
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if result[0].PhaseID != "p1" {
		t.Errorf("result[0].PhaseID = %s, want p1", result[0].PhaseID)
	}
}

// TestPhaseTransitionPriority_AllStatusesCovered ensures all known statuses are ordered.
func TestPhaseTransitionPriority_AllStatusesCovered(t *testing.T) {
	// Failed < Cancelled < TimedOut < Completed < AwaitingFill < default
	statuses := []model.PhaseStatus{
		model.PhaseStatusFailed,
		model.PhaseStatusCancelled,
		model.PhaseStatusTimedOut,
		model.PhaseStatusCompleted,
		model.PhaseStatusAwaitingFill,
	}
	for i := 1; i < len(statuses); i++ {
		prev := phaseTransitionPriority(statuses[i-1])
		curr := phaseTransitionPriority(statuses[i])
		if prev >= curr {
			t.Errorf("priority(%s)=%d should be < priority(%s)=%d",
				statuses[i-1], prev, statuses[i], curr)
		}
	}
}

// TestSortPhaseTransitions_SingleElement verifies no panic on single element.
func TestSortPhaseTransitions_SingleElement(t *testing.T) {
	transitions := []PhaseTransitionResult{
		{PhaseID: "p1", NewStatus: model.PhaseStatusCompleted},
	}
	sortPhaseTransitions(transitions)
	if transitions[0].PhaseID != "p1" {
		t.Errorf("PhaseID = %s, want p1", transitions[0].PhaseID)
	}
}

// TestStepPhaseTransitions_SkipsNonInProgress verifies that stepPhaseTransitions
// skips commands that are not in_progress.
func TestStepPhaseTransitions_SkipsNonInProgress(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{ID: "p1", Name: "phase1", Status: model.PhaseStatusPending, DependsOn: nil},
	})
	qh.SetStateReader(reader)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusPending},
					{ID: "cmd2", Status: model.StatusCompleted},
					{ID: "cmd3", Status: model.StatusFailed},
				},
			},
		},
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: buildSignalIndex(nil),
	}

	qh.stepPhaseTransitions(&s)

	// No transitions should be applied for non-in_progress commands
	transitions := reader.getTransitions()
	if len(transitions) != 0 {
		t.Errorf("expected 0 transitions for non-in_progress commands, got %d", len(transitions))
	}
}

// TestStepPhaseTransitions_NoStateReader verifies that stepPhaseTransitions
// is a no-op when no state reader is configured.
func TestStepPhaseTransitions_NoStateReader(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)
	// No state reader set

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
				},
			},
		},
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: buildSignalIndex(nil),
	}

	// Must not panic
	qh.stepPhaseTransitions(&s)

	if len(s.signals.Data.Signals) != 0 {
		t.Errorf("expected 0 signals with no state reader, got %d", len(s.signals.Data.Signals))
	}
}

// TestStepPhaseTransitions_DefersCompletedUntilWorktreeMerge verifies G1:
// a PhaseStatusCompleted transition is deferred (not applied, no
// phase_diagnosis emitted) when the command has worktrees but the phase's
// merge has not yet been recorded via MarkPhaseMerged. This prevents the
// Planner from receiving "phase done" before a potential merge_conflict
// signal for the same phase.
func TestStepPhaseTransitions_DefersCompletedUntilWorktreeMerge(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	// Worktree state exists (HasWorktrees=true) but MergedPhases is empty.
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerging)

	// Override state reader with phaseIntegrationStateReader so we can drive
	// phase/task state directly without writing a full state_command yaml.
	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:              "p1",
			Name:            "phase1",
			Status:          model.PhaseStatusActive,
			RequiredTaskIDs: []string{"t1"},
		},
	})
	reader.setTaskState("cmd1", "t1", model.StatusCompleted)
	qh.SetStateReader(reader)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
				},
			},
		},
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: buildSignalIndex(nil),
	}

	qh.stepPhaseTransitions(&s)

	// Completed transition must be deferred until MarkPhaseMerged runs.
	transitions := reader.getTransitions()
	if len(transitions) != 0 {
		t.Errorf("expected 0 transitions while worktree merge pending, got %d: %+v",
			len(transitions), transitions)
	}
	// No phase_diagnosis signal should be emitted either.
	if len(s.signals.Data.Signals) != 0 {
		t.Errorf("expected 0 signals while worktree merge pending, got %d: %+v",
			len(s.signals.Data.Signals), s.signals.Data.Signals)
	}
}

// TestStepPhaseTransitions_AppliesCompletedAfterWorktreeMerge verifies G1:
// once MarkPhaseMerged has recorded the phase, the deferred Completed
// transition is applied on the next scan.
func TestStepPhaseTransitions_AppliesCompletedAfterWorktreeMerge(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	// Simulate Phase C having recorded the phase merge.
	if err := qh.worktreeManager.MarkPhaseMerged("cmd1", "p1"); err != nil {
		t.Fatalf("MarkPhaseMerged: %v", err)
	}

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:              "p1",
			Name:            "phase1",
			Status:          model.PhaseStatusActive,
			RequiredTaskIDs: []string{"t1"},
		},
	})
	reader.setTaskState("cmd1", "t1", model.StatusCompleted)
	qh.SetStateReader(reader)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
				},
			},
		},
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: buildSignalIndex(nil),
	}

	qh.stepPhaseTransitions(&s)

	transitions := reader.getTransitions()
	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition after merge recorded, got %d: %+v",
			len(transitions), transitions)
	}
	if transitions[0].NewStatus != model.PhaseStatusCompleted {
		t.Errorf("transition.NewStatus = %s, want %s",
			transitions[0].NewStatus, model.PhaseStatusCompleted)
	}
}

// TestIsPhaseMergeRecorded_NoWorktreeManager verifies the gate is permissive
// (returns true) when no worktree manager is configured — non-worktree
// commands must not be blocked by the gate.
func TestIsPhaseMergeRecorded_NoWorktreeManager(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)
	if !qh.isPhaseMergeRecorded("cmd1", "p1") {
		t.Error("expected isPhaseMergeRecorded=true when worktreeManager is nil")
	}
}

// TestIsPhaseMergeRecorded_NoWorktreesForCommand verifies the gate is
// permissive when the command has no worktrees registered.
func TestIsPhaseMergeRecorded_NoWorktreesForCommand(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	if !qh.isPhaseMergeRecorded("cmd-no-worktrees", "p1") {
		t.Error("expected isPhaseMergeRecorded=true for command with no worktrees")
	}
}

// TestCollectWorktreePhaseMerges_ActiveAllTasksCompleted verifies that a phase
// whose status is still Active (because stepPhaseTransitions deferred the
// Completed transition on the merge gate) but whose required tasks are all
// completed is still picked up for merging. Without this, merge collection
// and stepPhaseTransitions deadlock: merge is skipped because the phase is
// not "completed", and the phase never becomes completed because the merge is
// not recorded.
func TestCollectWorktreePhaseMerges_ActiveAllTasksCompleted(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:              "p1",
			Name:            "phase1",
			Status:          model.PhaseStatusActive,
			RequiredTaskIDs: []string{"t1"},
		},
	})
	reader.setTaskState("cmd1", "t1", model.StatusCompleted)
	qh.SetStateReader(reader)

	// Task queue contents don't drive eligibility now (state reader is the
	// source of truth), but we still need a non-empty map for the iteration.
	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted}},
	})

	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 1 {
		t.Fatalf("expected 1 merge item for deferred-completed phase, got %d", len(items))
	}
	if items[0].PhaseID != "p1" {
		t.Errorf("items[0].PhaseID = %q, want p1", items[0].PhaseID)
	}
}

// TestCollectWorktreePhaseMerges_ActiveTaskNotCompleted verifies that a phase
// with status Active but at least one required task still in-progress is NOT
// eligible for merging.
func TestCollectWorktreePhaseMerges_ActiveTaskNotCompleted(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:              "p1",
			Name:            "phase1",
			Status:          model.PhaseStatusActive,
			RequiredTaskIDs: []string{"t1", "t2"},
		},
	})
	reader.setTaskState("cmd1", "t1", model.StatusCompleted)
	reader.setTaskState("cmd1", "t2", model.StatusInProgress)
	qh.SetStateReader(reader)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			{ID: "t2", CommandID: "cmd1", Status: model.StatusInProgress},
		},
	})

	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 0 {
		t.Errorf("expected 0 merge items when a task is still in-progress, got %d: %+v",
			len(items), items)
	}
}

func TestCollectWorktreePhaseMerges_SuppressesCommitFailedWorkerUntilRecoveryCompletes(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusFailed)
	markCommitFailedWorkerForTest(t, maestroDir, "cmd1", "worker1", "2026-01-01T00:10:00Z")

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:              "p1",
			Name:            "phase1",
			Status:          model.PhaseStatusActive,
			RequiredTaskIDs: []string{"t1"},
		},
	})
	reader.setTaskState("cmd1", "t1", model.StatusCompleted)
	qh.SetStateReader(reader)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {{
			ID:        "t1",
			CommandID: "cmd1",
			Status:    model.StatusCompleted,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:05:00Z",
		}},
	})

	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 0 {
		t.Fatalf("expected no merge retry before recovery task completes, got %d: %+v", len(items), items)
	}

	tqs = makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{
				ID:        "t1",
				CommandID: "cmd1",
				Status:    model.StatusCompleted,
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:05:00Z",
			},
			{
				ID:        "t_recovery",
				CommandID: "cmd1",
				Status:    model.StatusCompleted,
				CreatedAt: "2026-01-01T00:11:00Z",
				UpdatedAt: "2026-01-01T00:11:30Z",
			},
		},
	})

	items = qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 1 {
		t.Fatalf("expected merge retry after recovery task completes, got %d: %+v", len(items), items)
	}
	if got := items[0].WorkerIDs; len(got) != 1 || got[0] != "worker1" {
		t.Fatalf("WorkerIDs = %v, want [worker1]", got)
	}
}

// TestCollectWorktreePhaseMerges_FailedPhaseStillMergedWhenAllTerminal pins
// the policy that a phase whose status ended in PhaseStatusFailed is STILL
// collected for merging, as long as every task has reached a terminal
// effective status. The earlier "failed phase = no merge" gate produced a
// hard deadlock (Report 2026-05-04): a phase with 6 completed tasks +
// 2 failed left worker worktrees at WorktreeStatusActive forever; the
// publish/cleanup gate then deferred indefinitely on uncommitted worker
// output and the daemon required operator intervention. Forward progress
// requires the merge to happen so workers advance to Committed/Integrated;
// publish-to-main is independently blocked when plan_status is failed.
func TestCollectWorktreePhaseMerges_FailedPhaseStillMergedWhenAllTerminal(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:              "p1",
			Name:            "phase1",
			Status:          model.PhaseStatusFailed,
			RequiredTaskIDs: []string{"t1", "t2"},
			TaskIDs:         []string{"t1", "t2"},
		},
	})
	// One completed task + one failed task. Both terminal → phase is
	// eligible for merge collection. The merge itself is per-worker;
	// the worker(s) holding the completed task's output are committed
	// and merged so cleanup can proceed.
	reader.setTaskState("cmd1", "t1", model.StatusCompleted)
	reader.setTaskState("cmd1", "t2", model.StatusFailed)
	qh.SetStateReader(reader)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted}},
		"worker2": {{ID: "t2", CommandID: "cmd1", Status: model.StatusFailed}},
	})

	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 1 {
		t.Fatalf("expected 1 merge item for failed-but-terminal phase, got %d: %+v", len(items), items)
	}
	if items[0].PhaseID != "p1" {
		t.Errorf("PhaseID = %q, want %q", items[0].PhaseID, "p1")
	}
}

// TestCollectWorktreePhaseMerges_FailedPhasePartialTasksNotMerged: a phase
// whose status is failed but whose tasks are still in flight (not all
// terminal) must NOT be collected. The merge gate still requires task
// terminality so a half-complete phase does not get force-merged while
// a successor task may still produce output.
func TestCollectWorktreePhaseMerges_FailedPhasePartialTasksNotMerged(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:              "p1",
			Name:            "phase1",
			Status:          model.PhaseStatusFailed,
			RequiredTaskIDs: []string{"t1", "t2"},
			TaskIDs:         []string{"t1", "t2"},
		},
	})
	reader.setTaskState("cmd1", "t1", model.StatusCompleted)
	reader.setTaskState("cmd1", "t2", model.StatusInProgress) // not yet terminal
	qh.SetStateReader(reader)

	tqs := makeTaskQueues(map[string][]model.Task{
		"worker1": {{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted}},
		"worker2": {{ID: "t2", CommandID: "cmd1", Status: model.StatusInProgress}},
	})

	items := qh.collectWorktreePhaseMerges("cmd1", tqs)
	if len(items) != 0 {
		t.Errorf("expected 0 merge items for partial-terminal phase, got %d: %+v", len(items), items)
	}
}

// TestCheckPhaseTransitions_MergeGateDefersPendingActivation verifies that a
// pending phase whose dependency's merge has not yet been recorded does NOT
// activate in the same scan cycle. Without the gate, pass 1 reflected the
// deferred Completed into phaseMap and pass 2 activated the dependent phase —
// causing verification to run on worker worktrees instead of integration.
func TestCheckPhaseTransitions_MergeGateDefersPendingActivation(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})
	// Worktree state exists so the gate is active, and MergedPhases is empty.
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerging)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:              "p1",
			Name:            "setup",
			Status:          model.PhaseStatusActive,
			RequiredTaskIDs: []string{"t1"},
		},
		{
			ID:        "p2",
			Name:      "parallel-edits",
			Status:    model.PhaseStatusPending,
			DependsOn: []string{"p1"},
		},
	})
	reader.setTaskState("cmd1", "t1", model.StatusCompleted)
	qh.SetStateReader(reader)

	// CheckPhaseTransitions should NOT emit an AwaitingFill transition for p2
	// while p1's merge is pending. The Completed transition for p1 may be
	// present in the returned slice (for deferral logging) but must not have
	// been reflected into phaseMap — which is verified indirectly by
	// confirming no AwaitingFill appears for p2.
	transitions, err := qh.dependencyResolver.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("CheckPhaseTransitions: %v", err)
	}
	for _, tr := range transitions {
		if tr.PhaseID == "p2" && tr.NewStatus == model.PhaseStatusAwaitingFill {
			t.Errorf("p2 should not activate while p1 merge pending, got transition: %+v", tr)
		}
	}
}

// TestCheckPhaseTransitions_MergeGateAllowsActivationAfterMerge verifies that
// once the merge is recorded, the pending phase does activate normally.
func TestCheckPhaseTransitions_MergeGateAllowsActivationAfterMerge(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	if err := qh.worktreeManager.MarkPhaseMerged("cmd1", "p1"); err != nil {
		t.Fatalf("MarkPhaseMerged: %v", err)
	}

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:              "p1",
			Name:            "setup",
			Status:          model.PhaseStatusActive,
			RequiredTaskIDs: []string{"t1"},
		},
		{
			ID:        "p2",
			Name:      "parallel-edits",
			Status:    model.PhaseStatusPending,
			DependsOn: []string{"p1"},
		},
	})
	reader.setTaskState("cmd1", "t1", model.StatusCompleted)
	qh.SetStateReader(reader)

	transitions, err := qh.dependencyResolver.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("CheckPhaseTransitions: %v", err)
	}

	// Both transitions should appear: p1 → Completed, p2 → AwaitingFill.
	var sawCompleted, sawAwaitingFill bool
	for _, tr := range transitions {
		if tr.PhaseID == "p1" && tr.NewStatus == model.PhaseStatusCompleted {
			sawCompleted = true
		}
		if tr.PhaseID == "p2" && tr.NewStatus == model.PhaseStatusAwaitingFill {
			sawAwaitingFill = true
		}
	}
	if !sawCompleted {
		t.Errorf("expected p1 Completed transition after merge recorded, got %+v", transitions)
	}
	if !sawAwaitingFill {
		t.Errorf("expected p2 AwaitingFill transition after merge recorded, got %+v", transitions)
	}
}

// TestStepAwaitingFillWatchdog_FiresOnceAfterThreshold asserts that a
// phase at awaiting_fill longer than the configured stall window emits
// a single `awaiting_fill_stall` planner signal and records
// AwaitingFillStallNotifiedAt so subsequent scans do not refire. The
// original `awaiting_fill` signal is one-shot at phase entry; without
// the watchdog a stalled Planner would wait up to 3 hours
// (R6 fill_deadline) before any retry.
func TestStepAwaitingFillWatchdog_FiresOnceAfterThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)
	// Threshold = 5 minutes (default); set explicitly so the test
	// pins the gate independently of DefaultAwaitingFillStallNotifyMinutes.
	notifyMin := 5
	qh.config.Maestro.AwaitingFillStallNotifyMinutes = &notifyMin

	now := qh.clock.Now()
	enteredAt := now.Add(-10 * time.Minute).UTC().Format(time.RFC3339)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:                "p_fixes",
			Name:              "fixes",
			Status:            model.PhaseStatusAwaitingFill,
			AwaitingFillSince: &enteredAt,
		},
	})
	qh.SetStateReader(reader)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
				},
			},
		},
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: buildSignalIndex(nil),
	}

	qh.stepAwaitingFillWatchdog(&s)

	if got := len(s.signals.Data.Signals); got != 1 {
		t.Fatalf("expected 1 awaiting_fill_stall signal, got %d", got)
	}
	sig := s.signals.Data.Signals[0]
	if sig.Kind != "awaiting_fill_stall" {
		t.Errorf("signal kind = %q, want awaiting_fill_stall", sig.Kind)
	}
	if sig.CommandID != "cmd1" || sig.PhaseID != "p_fixes" {
		t.Errorf("signal command/phase = %q/%q, want cmd1/p_fixes", sig.CommandID, sig.PhaseID)
	}

	// Marker should now be set on the in-memory phase, gating future scans.
	phases, _ := reader.GetCommandPhases("cmd1")
	if phases[0].AwaitingFillStallNotifiedAt == nil {
		t.Error("expected AwaitingFillStallNotifiedAt to be set after watchdog fires")
	}

	// Re-running with the marker set must not emit a second signal even
	// though the elapsed window still exceeds the threshold.
	s.signalIndex = buildSignalIndex(s.signals.Data.Signals)
	qh.stepAwaitingFillWatchdog(&s)
	if got := len(s.signals.Data.Signals); got != 1 {
		t.Errorf("expected watchdog to be one-shot per window, got %d signals", got)
	}
}

// TestStepAwaitingFillWatchdog_SkipsBeforeThreshold guards the conservative
// gate: a phase that just entered awaiting_fill within the threshold window
// must not fire — Planner is still expected to be working on the plan.
func TestStepAwaitingFillWatchdog_SkipsBeforeThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)
	notifyMin := 5
	qh.config.Maestro.AwaitingFillStallNotifyMinutes = &notifyMin

	now := qh.clock.Now()
	// Only 1 minute elapsed — well within the 5-minute window.
	enteredAt := now.Add(-1 * time.Minute).UTC().Format(time.RFC3339)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:                "p_fixes",
			Name:              "fixes",
			Status:            model.PhaseStatusAwaitingFill,
			AwaitingFillSince: &enteredAt,
		},
	})
	qh.SetStateReader(reader)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
				},
			},
		},
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: buildSignalIndex(nil),
	}

	qh.stepAwaitingFillWatchdog(&s)
	if got := len(s.signals.Data.Signals); got != 0 {
		t.Errorf("expected no signal before threshold, got %d", got)
	}
}

// TestStepAwaitingFillWatchdog_SuppressesFireWhenPlannerActive pins the
// post-2026-05-06 P2 fix: when the Planner pane shows cross-scan
// activity (spinner verb, hash delta), the watchdog must skip the fire
// so a Planner that ran `plan submit --dry-run` and is composing the
// real submit a few seconds later is NOT flagged as a stall (Report
// 2026-05-06 P2 — false WARN noise).
func TestStepAwaitingFillWatchdog_SuppressesFireWhenPlannerActive(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)
	notifyMin := 5
	qh.config.Maestro.AwaitingFillStallNotifyMinutes = &notifyMin

	// Spoof a Planner pane that visibly oscillates content (cross-scan
	// hash delta) so paneactivity returns VerdictActive on the second
	// observation.
	qh.paneFinder = func(string) (string, error) { return "session:0.0", nil }
	frame := 0
	qh.paneCapture = func(string) (string, error) {
		frame++
		return fmt.Sprintf("Cogitating for %ds (frame %d)", frame*10, frame), nil
	}
	// Prime the tracker with a baseline frame ~2 minutes ago so the next
	// observation crosses minPrevAge and the cross-scan delta is treated
	// as Active.
	qh.paneActivity.RecordObservation("planner", "Cogitating for 0s (frame 0)",
		qh.clock.Now().Add(-2*time.Minute).UTC())

	now := qh.clock.Now()
	enteredAt := now.Add(-10 * time.Minute).UTC().Format(time.RFC3339) // past 5-min threshold

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:                "p_fixes",
			Name:              "fixes",
			Status:            model.PhaseStatusAwaitingFill,
			AwaitingFillSince: &enteredAt,
		},
	})
	qh.SetStateReader(reader)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{ID: "cmd1", Status: model.StatusInProgress}},
			},
		},
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: buildSignalIndex(nil),
	}

	qh.stepAwaitingFillWatchdog(&s)
	if got := len(s.signals.Data.Signals); got != 0 {
		t.Errorf("expected no fire when Planner pane is VerdictActive, got %d signals", got)
	}
}

// TestStepAwaitingFillWatchdog_DoesNotEscalateOnLegitimateThinking pins
// post-2026-05-06 round-2 P1: when the Planner pane keeps reporting
// VerdictActive (Opus xhigh-effort thinking) and elapsed is within the
// 25-min active backstop budget, the watchdog must NOT force-escalate.
// Tested at elapsed=18min — the false-fire window from the workspace
// benchmark report.
func TestStepAwaitingFillWatchdog_DoesNotEscalateOnLegitimateThinking(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)
	notifyMin := 5
	qh.config.Maestro.AwaitingFillStallNotifyMinutes = &notifyMin

	qh.paneFinder = func(string) (string, error) { return "session:0.0", nil }
	frame := 0
	qh.paneCapture = func(string) (string, error) {
		frame++
		return fmt.Sprintf("Cogitating for %ds (frame %d)", frame*10, frame), nil
	}
	qh.paneActivity.RecordObservation("planner", "Cogitating for 0s (frame 0)",
		qh.clock.Now().Add(-2*time.Minute).UTC())

	now := qh.clock.Now()
	enteredAt := now.Add(-18 * time.Minute).UTC().Format(time.RFC3339)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:                "p_fixes",
			Name:              "fixes",
			Status:            model.PhaseStatusAwaitingFill,
			AwaitingFillSince: &enteredAt,
		},
	})
	qh.SetStateReader(reader)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{ID: "cmd1", Status: model.StatusInProgress}},
			},
		},
		signals:       fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex:   buildSignalIndex(nil),
		notifications: fileState[model.NotificationQueue]{Data: model.NotificationQueue{}},
	}

	qh.stepAwaitingFillWatchdog(&s)

	for _, n := range s.notifications.Data.Notifications {
		if n.Type == model.NotificationTypeCommandFailed && n.CommandID == "cmd1" {
			t.Errorf("legitimate 18min thinking with VerdictActive should NOT escalate; got command_failed notification: %+v", n)
		}
	}
}

// TestStepAwaitingFillWatchdog_PlannerActiveBackstopEscalates pins the
// wall-clock backstop: even when the Planner pane keeps reporting
// VerdictActive (spinner alive but LLM wedged), the watchdog must
// escalate once elapsed exceeds awaitingFillStallActiveBackstop so a
// "wedged-but-spinning" Planner cannot hold the command in awaiting_fill
// indefinitely (Report 2026-05-06 P-2 from review). Backstop is
// decoupled from awaitingFillStallEscalateAfter * threshold so legitimate
// 15-20min Opus xhigh-effort thinking does not false-fire while the
// VerdictActive backstop still bounds genuinely wedged spinners
// (Report 2026-05-06 round-2 P1).
func TestStepAwaitingFillWatchdog_PlannerActiveBackstopEscalates(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)
	notifyMin := 5
	qh.config.Maestro.AwaitingFillStallNotifyMinutes = &notifyMin

	qh.paneFinder = func(string) (string, error) { return "session:0.0", nil }
	frame := 0
	qh.paneCapture = func(string) (string, error) {
		frame++
		return fmt.Sprintf("Cogitating for %ds (frame %d)", frame*10, frame), nil
	}
	qh.paneActivity.RecordObservation("planner", "Cogitating for 0s (frame 0)",
		qh.clock.Now().Add(-2*time.Minute).UTC())

	now := qh.clock.Now()
	// elapsed = 30 min — past the 5×5=25 min backstop budget.
	enteredAt := now.Add(-30 * time.Minute).UTC().Format(time.RFC3339)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:                "p_fixes",
			Name:              "fixes",
			Status:            model.PhaseStatusAwaitingFill,
			AwaitingFillSince: &enteredAt,
		},
	})
	qh.SetStateReader(reader)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{ID: "cmd1", Status: model.StatusInProgress}},
			},
		},
		signals:       fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex:   buildSignalIndex(nil),
		notifications: fileState[model.NotificationQueue]{Data: model.NotificationQueue{}},
	}

	qh.stepAwaitingFillWatchdog(&s)

	// Backstop escalation calls escalateAwaitingFillStall, which appends
	// a command_failed notification. The signal queue may or may not be
	// touched depending on whether the escalation also writes a stall
	// signal first; the canonical assertion is that a command_failed
	// notification is queued.
	gotEscalation := false
	for _, n := range s.notifications.Data.Notifications {
		if n.Type == model.NotificationTypeCommandFailed && n.CommandID == "cmd1" {
			gotEscalation = true
		}
	}
	if !gotEscalation {
		t.Errorf("expected command_failed notification from backstop escalation, got notifications=%+v signals=%+v",
			s.notifications.Data.Notifications, s.signals.Data.Signals)
	}
}

// TestStepAwaitingFillWatchdog_DisabledByConfig pins the operator-disable
// path: setting AwaitingFillStallNotifyMinutes=0 must turn the watchdog
// into a no-op (no signal, no state mutation) so deployments that prefer
// to rely on R6 fill_deadline only can opt out.
func TestStepAwaitingFillWatchdog_DisabledByConfig(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)
	disabled := 0
	qh.config.Maestro.AwaitingFillStallNotifyMinutes = &disabled

	now := qh.clock.Now()
	enteredAt := now.Add(-1 * time.Hour).UTC().Format(time.RFC3339)

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{
			ID:                "p_fixes",
			Name:              "fixes",
			Status:            model.PhaseStatusAwaitingFill,
			AwaitingFillSince: &enteredAt,
		},
	})
	qh.SetStateReader(reader)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
				},
			},
		},
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: buildSignalIndex(nil),
	}

	qh.stepAwaitingFillWatchdog(&s)
	if got := len(s.signals.Data.Signals); got != 0 {
		t.Errorf("expected no signal when watchdog is disabled, got %d", got)
	}
}

// TestStepPlannerSignals_EmptySignals verifies that stepPlannerSignals is a
// no-op when no signals exist.
func TestStepPlannerSignals_EmptySignals(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)

	s := scanState{
		signals: fileState[model.PlannerSignalQueue]{
			Data: model.PlannerSignalQueue{Signals: nil},
		},
	}

	// Must not panic or modify state
	qh.stepPlannerSignals(&s)

	if s.signals.Dirty {
		t.Error("expected signals not dirty when empty")
	}
}
