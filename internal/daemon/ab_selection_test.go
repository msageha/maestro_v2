package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// stubABOps records calls; selection outcomes are injected per test.
type stubABOps struct {
	intakeErr error
	commitErr error
}

func (s *stubABOps) CommitCandidateChanges(string, string) error { return s.commitErr }
func (s *stubABOps) RunCandidateSelection(context.Context, string, string, []worktree.ABSelectionInput, []string) (*worktree.ABSelectionOutcome, error) {
	return &worktree.ABSelectionOutcome{}, nil
}
func (s *stubABOps) IntakeWinner(string, string, string, string) error { return s.intakeErr }
func (s *stubABOps) RemoveCandidateWorktree(_ string, taskID string) error {
	_ = taskID
	return nil
}

func abTestQueueHandler(t *testing.T) (*QueueHandler, string) {
	t.Helper()
	maestroDir := testutil.SetupDir(t)
	cfg := model.Config{}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	return qh, maestroDir
}

func writeABState(t *testing.T, maestroDir, commandID string) {
	t.Helper()
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs:  []string{"task_canon"},
			TaskDependencies: map[string][]string{"task_canon": {}, "task_shadow": {}},
			TaskStates: map[string]model.Status{
				"task_canon":  model.StatusCompleted,
				"task_shadow": model.StatusCompleted,
			},
			CancelledReasons: map[string]string{},
		},
		RetryTracking: model.RetryTracking{RetryLineage: map[string]string{}},
		PhaseTracking: model.PhaseTracking{Phases: []model.Phase{{
			PhaseID: "p1", Name: "phase-1", Status: model.PhaseStatusActive,
			TaskIDs: []string{"task_canon"},
		}}},
		CandidateGroups: map[string]*model.CandidateGroup{
			"abg_x": {
				Status:          model.ABGroupSelecting,
				CanonicalTaskID: "task_canon",
				Candidates: []model.ABCandidate{
					{TaskID: "task_canon", WorkerID: "worker1", Model: "opus", BloomLevel: 5, Branch: "b1"},
					{TaskID: "task_shadow", WorkerID: "worker3", Model: "codex", BloomLevel: 5, Branch: "b2"},
				},
				CreatedAt: "2026-06-12T00:00:00Z",
				UpdatedAt: "2026-06-12T00:00:00Z",
			},
		},
		CreatedAt: "2026-06-12T00:00:00Z",
		UpdatedAt: "2026-06-12T00:00:00Z",
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", commandID+".yaml"), state); err != nil {
		t.Fatal(err)
	}
}

func readABState(t *testing.T, maestroDir, commandID string) *model.CommandState {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "commands", commandID+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cs model.CommandState
	if err := yamlv3.Unmarshal(data, &cs); err != nil {
		t.Fatal(err)
	}
	return &cs
}

func TestResolveABGroup_CanonicalWin(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab1")

	qh.resolveABGroup("cmd_ab1", "abg_x", "task_canon", false, "", map[string]string{"winner": "task_canon"})

	cs := readABState(t, maestroDir, "cmd_ab1")
	g := cs.CandidateGroups["abg_x"]
	if g.Status != model.ABGroupResolved || g.WinnerTaskID != "task_canon" {
		t.Errorf("group = %+v, want resolved canonical winner", g)
	}
	if cs.TaskStates["task_canon"] != model.StatusCompleted {
		t.Errorf("canonical state = %s, want completed", cs.TaskStates["task_canon"])
	}
	if cs.TaskStates["task_shadow"] != model.StatusCancelled ||
		cs.CancelledReasons["task_shadow"] != "superseded_by_ab_loser" {
		t.Errorf("shadow must be cancelled/superseded, got %s / %q",
			cs.TaskStates["task_shadow"], cs.CancelledReasons["task_shadow"])
	}
	if cs.ABBarrierActive("task_canon") {
		t.Error("barrier must lift after resolution")
	}
}

func TestResolveABGroup_ShadowWin_Supersedes(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab2")

	qh.resolveABGroup("cmd_ab2", "abg_x", "task_shadow", false, "", nil)

	cs := readABState(t, maestroDir, "cmd_ab2")
	g := cs.CandidateGroups["abg_x"]
	if g.Status != model.ABGroupResolved || g.WinnerTaskID != "task_shadow" {
		t.Errorf("group = %+v, want resolved shadow winner", g)
	}
	// Membership replaced: shadow in required, canonical out.
	foundShadow, foundCanon := false, false
	for _, id := range cs.RequiredTaskIDs {
		if id == "task_shadow" {
			foundShadow = true
		}
		if id == "task_canon" {
			foundCanon = true
		}
	}
	if !foundShadow || foundCanon {
		t.Errorf("RequiredTaskIDs = %v, want shadow in / canonical out", cs.RequiredTaskIDs)
	}
	if cs.RetryLineage["task_shadow"] != "task_canon" {
		t.Errorf("lineage = %v, want task_shadow→task_canon", cs.RetryLineage)
	}
	// Regression (codex finding #1): WireRetryTaskIntoState resets the
	// successor to planned; the resolver must restore the winner's real
	// completed status or notify/phase completion deadlock forever.
	if cs.TaskStates["task_shadow"] != model.StatusCompleted {
		t.Errorf("winner state = %s, want completed (planned would deadlock notify)", cs.TaskStates["task_shadow"])
	}
	if cs.TaskStates["task_canon"] != model.StatusCancelled ||
		cs.CancelledReasons["task_canon"] != "superseded_by_ab_winner:task_shadow" {
		t.Errorf("canonical must be superseded, got %s / %q",
			cs.TaskStates["task_canon"], cs.CancelledReasons["task_canon"])
	}
	// Phase must now reference the shadow.
	inPhase := false
	for _, id := range cs.Phases[0].TaskIDs {
		if id == "task_shadow" {
			inPhase = true
		}
	}
	if !inPhase {
		t.Errorf("phase TaskIDs = %v, want task_shadow wired in", cs.Phases[0].TaskIDs)
	}
}

func TestResolveABGroup_Degraded(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab3")

	qh.resolveABGroup("cmd_ab3", "abg_x", "", true, "intake conflict",
		map[string]string{"degraded": "intake_conflict"}, "superseded_by_retry:task_repair01")

	cs := readABState(t, maestroDir, "cmd_ab3")
	g := cs.CandidateGroups["abg_x"]
	if g.Status != model.ABGroupDegraded {
		t.Errorf("group status = %s, want degraded", g.Status)
	}
	// With a repair successor enqueued, the canonical is superseded (silent
	// ack); its stale "completed" result entry must never reach the Planner.
	if cs.TaskStates["task_canon"] != model.StatusCancelled ||
		cs.CancelledReasons["task_canon"] != "superseded_by_retry:task_repair01" {
		t.Errorf("canonical = %s / %q, want cancelled superseded_by_retry",
			cs.TaskStates["task_canon"], cs.CancelledReasons["task_canon"])
	}
	if cs.TaskStates["task_shadow"] != model.StatusCancelled ||
		cs.CancelledReasons["task_shadow"] != "superseded_by_ab_degraded" {
		t.Errorf("shadow must be cancelled, got %s / %q",
			cs.TaskStates["task_shadow"], cs.CancelledReasons["task_shadow"])
	}
	// Idempotent: re-resolving a terminal group is a no-op.
	qh.resolveABGroup("cmd_ab3", "abg_x", "task_canon", false, "", nil)
	cs = readABState(t, maestroDir, "cmd_ab3")
	if cs.CandidateGroups["abg_x"].Status != model.ABGroupDegraded {
		t.Error("re-resolution must not change a terminal group")
	}
}

// TestFinalizeABWinner_IntakeConflict_EnqueuesRepair is the regression test
// for codex finding #4 + the unwired-call finding: an intake conflict must
// route through degradeABGroupWithRepair — repair successor enqueued, the
// canonical superseded (silent ack; no stale "completed" notify), group
// degraded.
func TestFinalizeABWinner_IntakeConflict_EnqueuesRepair(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab4")

	// Canonical queue row (source for the repair-task content) + minimal DOA.
	doa := model.DefaultDefinitionOfAbort()
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{{
			ID: "task_canon", CommandID: "cmd_ab4", Status: model.StatusCompleted,
			Purpose: "p", Content: "c", AcceptanceCriteria: "a",
			ExpectedPaths: []string{"."}, DefinitionOfAbort: &doa, BloomLevel: 5,
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq); err != nil {
		t.Fatal(err)
	}

	ops := &stubABOps{intakeErr: errIntakeConflictTest}
	cs := readABState(t, maestroDir, "cmd_ab4")
	g := cs.CandidateGroups["abg_x"]
	item := abGroupWorkItem{CommandID: "cmd_ab4", GroupID: "abg_x",
		QueueStatuses: map[string]model.Status{"task_canon": model.StatusCompleted, "task_shadow": model.StatusCompleted}}

	qh.finalizeABWinner(ops, item, g,
		g.CandidateByTask("task_canon"), g.OtherCandidate("task_canon"),
		map[string]string{})

	after := readABState(t, maestroDir, "cmd_ab4")
	if after.CandidateGroups["abg_x"].Status != model.ABGroupDegraded {
		t.Fatalf("group status = %s, want degraded", after.CandidateGroups["abg_x"].Status)
	}
	// Canonical superseded by the repair successor (silent ack path).
	reason := after.CancelledReasons["task_canon"]
	if after.TaskStates["task_canon"] != model.StatusCancelled ||
		len(reason) == 0 || reason[:len("superseded_by_retry:")] != "superseded_by_retry:" {
		t.Errorf("canonical = %s / %q, want cancelled superseded_by_retry:<id>",
			after.TaskStates["task_canon"], reason)
	}
	repairID := reason[len("superseded_by_retry:"):]
	// Repair task registered in state as planned, non-A/B, and queued.
	if after.TaskStates[repairID] != model.StatusPlanned {
		t.Errorf("repair task state = %s, want planned", after.TaskStates[repairID])
	}
	if after.ABBarrierActive(repairID) {
		t.Error("repair task must not be an A/B candidate")
	}
	q := readWorkerQueue(t, maestroDir, "worker1")
	found := false
	for _, task := range q.Tasks {
		if task.ID == repairID {
			found = true
			if task.ABGroupID != "" {
				t.Error("repair queue row must have ab_group_id cleared")
			}
		}
	}
	if !found {
		t.Errorf("repair task %s not found in worker1 queue", repairID)
	}
}

var errIntakeConflictTest = errors.New("intake merge conflicted (worker=worker1): test")

func readWorkerQueue(t *testing.T, maestroDir, workerID string) model.TaskQueue {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(maestroDir, "queue", workerID+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatal(err)
	}
	return tq
}

// --- 2026-06-12 audit regression tests ---

// abQueues builds a taskQueues snapshot from workerID → rows.
func abQueues(maestroDir string, rows map[string][]model.Task) map[string]*taskQueueEntry {
	out := map[string]*taskQueueEntry{}
	for workerID, tasks := range rows {
		path := filepath.Join(maestroDir, "queue", workerID+".yaml")
		out[path] = &taskQueueEntry{
			Path:  path,
			Queue: model.TaskQueue{SchemaVersion: 1, FileType: "queue_task", Tasks: tasks},
		}
	}
	return out
}

func writeWorkerQueue(t *testing.T, maestroDir, workerID string, tasks []model.Task) {
	t.Helper()
	tq := model.TaskQueue{SchemaVersion: 1, FileType: "queue_task", Tasks: tasks}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", workerID+".yaml"), tq); err != nil {
		t.Fatal(err)
	}
}

func mutateABState(t *testing.T, maestroDir, commandID string, fn func(*model.CommandState)) {
	t.Helper()
	cs := readABState(t, maestroDir, commandID)
	fn(cs)
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", commandID+".yaml"), cs); err != nil {
		t.Fatal(err)
	}
}

// Audit #1: a dead-lettered candidate (queue row deleted, state mirrored to
// failed) must not wedge the group — the effective-status fallback converges
// to a walkover by the surviving finisher.
func TestProcessABGroup_DeadLetteredRowConvergesViaState(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab_dl")
	mutateABState(t, maestroDir, "cmd_ab_dl", func(cs *model.CommandState) {
		cs.CandidateGroups["abg_x"].Status = model.ABGroupRacing
		cs.TaskStates["task_canon"] = model.StatusFailed // dead-letter post-process
	})
	// Only the shadow row survives in the queues.
	shadowRow := model.Task{ID: "task_shadow", CommandID: "cmd_ab_dl", ABGroupID: "abg_x",
		Status: model.StatusCompleted, ExpectedPaths: []string{"."}}
	writeWorkerQueue(t, maestroDir, "worker3", []model.Task{shadowRow})

	var work deferredWork
	qh.collectABGroupWork(abQueues(maestroDir, map[string][]model.Task{"worker3": {shadowRow}}), &work)
	if len(work.abGroups) != 1 {
		t.Fatalf("collected %d group items, want 1 (state-driven discovery)", len(work.abGroups))
	}
	qh.processABGroup(context.Background(), &stubABOps{}, work.abGroups[0])

	cs := readABState(t, maestroDir, "cmd_ab_dl")
	g := cs.CandidateGroups["abg_x"]
	if g.Status != model.ABGroupResolved || g.WinnerTaskID != "task_shadow" {
		t.Fatalf("group = %+v, want resolved with shadow walkover", g)
	}
	if cs.TaskStates["task_shadow"] != model.StatusCompleted {
		t.Errorf("winner status = %s, want completed", cs.TaskStates["task_shadow"])
	}
}

// Audit #1 (both rows gone): state-driven discovery must emit a work item
// even when no queue row references the group.
func TestCollectABGroupWork_StateDrivenDiscovery_NoRows(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab_norow")

	var work deferredWork
	qh.collectABGroupWork(map[string]*taskQueueEntry{}, &work)
	if len(work.abGroups) != 1 || work.abGroups[0].GroupID != "abg_x" {
		t.Fatalf("work items = %+v, want one for abg_x", work.abGroups)
	}
	if len(work.abGroups[0].QueueStatuses) != 0 {
		t.Errorf("QueueStatuses = %v, want empty (no rows)", work.abGroups[0].QueueStatuses)
	}
}

// Audit #5 (legacy artifact): tagged rows whose group is absent from a
// readable state are repaired — known-to-state rows get the tag stripped,
// unknown shadow remnants are cancelled too.
func TestProcessABGroup_OrphanTaggedRows_Repaired(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab_orphan")
	mutateABState(t, maestroDir, "cmd_ab_orphan", func(cs *model.CommandState) {
		cs.CandidateGroups = nil // group never registered (legacy crash)
		cs.TaskStates["task_canon"] = model.StatusPending
		delete(cs.TaskStates, "task_shadow")
	})
	canonRow := model.Task{ID: "task_canon", CommandID: "cmd_ab_orphan", ABGroupID: "abg_lost",
		Status: model.StatusPending, ExpectedPaths: []string{"."}}
	shadowRow := model.Task{ID: "task_shadow", CommandID: "cmd_ab_orphan", ABGroupID: "abg_lost",
		Status: model.StatusPending, ExpectedPaths: []string{"."}}
	writeWorkerQueue(t, maestroDir, "worker1", []model.Task{canonRow})
	writeWorkerQueue(t, maestroDir, "worker3", []model.Task{shadowRow})

	var work deferredWork
	qh.collectABGroupWork(abQueues(maestroDir, map[string][]model.Task{
		"worker1": {canonRow}, "worker3": {shadowRow},
	}), &work)
	if len(work.abGroups) != 1 {
		t.Fatalf("collected %d items, want 1 orphan item", len(work.abGroups))
	}
	qh.processABGroup(context.Background(), &stubABOps{}, work.abGroups[0])

	q1 := readWorkerQueue(t, maestroDir, "worker1")
	if q1.Tasks[0].ABGroupID != "" || q1.Tasks[0].Status != model.StatusPending {
		t.Errorf("canonical row = %+v, want untagged pending", q1.Tasks[0])
	}
	q3 := readWorkerQueue(t, maestroDir, "worker3")
	if q3.Tasks[0].ABGroupID != "" || q3.Tasks[0].Status != model.StatusCancelled {
		t.Errorf("shadow remnant = %+v, want untagged cancelled", q3.Tasks[0])
	}
}

// Audit #5 (state-first order): a group whose canonical row never received
// its tag degrades immediately; the canonical keeps its own lifecycle.
func TestProcessABGroup_FanoutIncomplete_UntaggedCanonical(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab_untag")
	mutateABState(t, maestroDir, "cmd_ab_untag", func(cs *model.CommandState) {
		cs.CandidateGroups["abg_x"].Status = model.ABGroupRacing
		cs.TaskStates["task_canon"] = model.StatusPending
		cs.TaskStates["task_shadow"] = model.StatusPending
	})
	canonRow := model.Task{ID: "task_canon", CommandID: "cmd_ab_untag",
		Status: model.StatusPending, ExpectedPaths: []string{"."}} // NO tag
	writeWorkerQueue(t, maestroDir, "worker1", []model.Task{canonRow})

	var work deferredWork
	qh.collectABGroupWork(abQueues(maestroDir, map[string][]model.Task{"worker1": {canonRow}}), &work)
	if len(work.abGroups) != 1 {
		t.Fatalf("collected %d items, want 1", len(work.abGroups))
	}
	qh.processABGroup(context.Background(), &stubABOps{}, work.abGroups[0])

	cs := readABState(t, maestroDir, "cmd_ab_untag")
	g := cs.CandidateGroups["abg_x"]
	if g.Status != model.ABGroupDegraded {
		t.Fatalf("group status = %s, want degraded (fanout_incomplete)", g.Status)
	}
	if cs.TaskStates["task_canon"] != model.StatusPending {
		t.Errorf("canonical must keep its own lifecycle, got %s", cs.TaskStates["task_canon"])
	}
	if cs.TaskStates["task_shadow"] != model.StatusCancelled {
		t.Errorf("shadow must be superseded, got %s", cs.TaskStates["task_shadow"])
	}
}

// Audit #5: a shadow whose row was never enqueued is cancelled in state so
// the race converges instead of hanging until the timeout.
func TestProcessABGroup_ShadowNeverEnqueued_StateCancelled(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab_noshadow")
	mutateABState(t, maestroDir, "cmd_ab_noshadow", func(cs *model.CommandState) {
		cs.CandidateGroups["abg_x"].Status = model.ABGroupRacing
		cs.TaskStates["task_canon"] = model.StatusInProgress
		cs.TaskStates["task_shadow"] = model.StatusPending
	})
	canonRow := model.Task{ID: "task_canon", CommandID: "cmd_ab_noshadow", ABGroupID: "abg_x",
		Status: model.StatusInProgress, ExpectedPaths: []string{"."}}
	writeWorkerQueue(t, maestroDir, "worker1", []model.Task{canonRow})

	var work deferredWork
	qh.collectABGroupWork(abQueues(maestroDir, map[string][]model.Task{"worker1": {canonRow}}), &work)
	qh.processABGroup(context.Background(), &stubABOps{}, work.abGroups[0])

	cs := readABState(t, maestroDir, "cmd_ab_noshadow")
	if cs.TaskStates["task_shadow"] != model.StatusCancelled ||
		cs.CancelledReasons["task_shadow"] != "ab_fanout_incomplete" {
		t.Errorf("shadow = %s / %q, want cancelled ab_fanout_incomplete",
			cs.TaskStates["task_shadow"], cs.CancelledReasons["task_shadow"])
	}
}

// selectionTrackingABOps fails the test if selection runs.
type selectionTrackingABOps struct {
	stubABOps
	selectionCalls int
	intaken        []string
}

func (s *selectionTrackingABOps) RunCandidateSelection(context.Context, string, string, []worktree.ABSelectionInput, []string) (*worktree.ABSelectionOutcome, error) {
	s.selectionCalls++
	return &worktree.ABSelectionOutcome{WinnerTaskID: "task_canon"}, nil
}

func (s *selectionTrackingABOps) IntakeWinner(_ string, _ string, _ string, taskID string) error {
	s.intaken = append(s.intaken, taskID)
	return nil
}

// Audit #10: a durable pending winner from a crashed attempt is re-finalized
// verbatim — selection must NOT re-run (a flake could intake a second branch).
func TestProcessABGroup_PendingWinnerReplayedWithoutReselection(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab_pw")
	mutateABState(t, maestroDir, "cmd_ab_pw", func(cs *model.CommandState) {
		cs.CandidateGroups["abg_x"].PendingWinnerTaskID = "task_shadow"
	})
	rows := map[string][]model.Task{
		"worker1": {{ID: "task_canon", CommandID: "cmd_ab_pw", ABGroupID: "abg_x",
			Status: model.StatusCompleted, ExpectedPaths: []string{"."}}},
		"worker3": {{ID: "task_shadow", CommandID: "cmd_ab_pw", ABGroupID: "abg_x",
			Status: model.StatusCompleted, ExpectedPaths: []string{"."}}},
	}
	writeWorkerQueue(t, maestroDir, "worker1", rows["worker1"])
	writeWorkerQueue(t, maestroDir, "worker3", rows["worker3"])

	ops := &selectionTrackingABOps{}
	var work deferredWork
	qh.collectABGroupWork(abQueues(maestroDir, rows), &work)
	qh.processABGroup(context.Background(), ops, work.abGroups[0])

	if ops.selectionCalls != 0 {
		t.Errorf("selection ran %d times, want 0 (pending winner replay)", ops.selectionCalls)
	}
	if len(ops.intaken) != 1 || ops.intaken[0] != "task_shadow" {
		t.Errorf("intaken = %v, want [task_shadow]", ops.intaken)
	}
	cs := readABState(t, maestroDir, "cmd_ab_pw")
	g := cs.CandidateGroups["abg_x"]
	if g.Status != model.ABGroupResolved || g.WinnerTaskID != "task_shadow" || g.PendingWinnerTaskID != "" {
		t.Errorf("group = %+v, want resolved shadow with pending cleared", g)
	}
}

// Audit #9: a repair successor already wired in RetryLineage must not be
// re-issued — the degrade only finishes the resolution.
func TestDegradeABGroupWithRepair_IdempotentOnExistingRepair(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab_idem")
	mutateABState(t, maestroDir, "cmd_ab_idem", func(cs *model.CommandState) {
		cs.RetryLineage["task_repair_prev"] = "task_canon"
	})
	cs := readABState(t, maestroDir, "cmd_ab_idem")
	g := cs.CandidateGroups["abg_x"]
	item := abGroupWorkItem{CommandID: "cmd_ab_idem", GroupID: "abg_x"}

	qh.degradeABGroupWithRepair(item, g, map[string]string{}, errors.New("test"))

	after := readABState(t, maestroDir, "cmd_ab_idem")
	if after.CandidateGroups["abg_x"].Status != model.ABGroupDegraded {
		t.Fatalf("group status = %s, want degraded", after.CandidateGroups["abg_x"].Status)
	}
	if after.CancelledReasons["task_canon"] != "superseded_by_retry:task_repair_prev" {
		t.Errorf("canonical reason = %q, want existing repair reused", after.CancelledReasons["task_canon"])
	}
	if _, err := os.Stat(filepath.Join(maestroDir, "queue", "worker1.yaml")); err == nil {
		q := readWorkerQueue(t, maestroDir, "worker1")
		if len(q.Tasks) != 0 {
			t.Errorf("no new repair row expected, got %d rows", len(q.Tasks))
		}
	}
}

// Audit #2: daemon retries never inherit A/B candidacy.
func TestCreateRetryTask_ClearsABGroupID(t *testing.T) {
	maestroDir := testutil.SetupDir(t)
	h := NewTaskRetryHandler(maestroDir, model.Config{}, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	doa := model.DefaultDefinitionOfAbort()
	original := &model.Task{ID: "task_orig", CommandID: "cmd_x", ABGroupID: "abg_x",
		Status: model.StatusFailed, Purpose: "p", Content: "c", AcceptanceCriteria: "a",
		ExpectedPaths: []string{"."}, DefinitionOfAbort: &doa, BloomLevel: 5}
	rt, err := h.CreateRetryTask(original, "worker1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if rt.ABGroupID != "" {
		t.Errorf("retry ABGroupID = %q, want cleared", rt.ABGroupID)
	}
}

// Audit #7: candidates of the same A/B race never path-block each other;
// different races still do.
func TestFindOverlappingTask_SameABGroupExempt(t *testing.T) {
	candidate := &model.Task{ID: "task_b", ABGroupID: "abg_1", ExpectedPaths: []string{"src"}}
	inFlight := []inFlightPathEntry{{TaskID: "task_a", ExpectedPaths: []string{"src"}, ABGroupID: "abg_1"}}
	if id, _, _ := findOverlappingTask(candidate, inFlight); id != "" {
		t.Errorf("same-group candidates must not block each other, got conflict with %s", id)
	}
	inFlight[0].ABGroupID = "abg_other"
	if id, _, _ := findOverlappingTask(candidate, inFlight); id != "task_a" {
		t.Errorf("different-group overlap must still block, got %q", id)
	}
}

// Codex 差し戻し P0: pending winner の replay が commit 恒久失敗で wedge せず、
// selecting タイムアウト超過後に repair 縮退へエスケープする。
func TestProcessABGroup_PendingWinnerTimeoutEscapesToRepair(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab_pwto")
	mutateABState(t, maestroDir, "cmd_ab_pwto", func(cs *model.CommandState) {
		g := cs.CandidateGroups["abg_x"]
		g.PendingWinnerTaskID = "task_shadow"
		g.UpdatedAt = "2026-06-11T00:00:00Z" // selecting budget (1800s) を超過
	})
	doa := model.DefaultDefinitionOfAbort()
	canonRow := model.Task{ID: "task_canon", CommandID: "cmd_ab_pwto", ABGroupID: "abg_x",
		Status: model.StatusCompleted, Purpose: "p", Content: "c", AcceptanceCriteria: "a",
		ExpectedPaths: []string{"."}, DefinitionOfAbort: &doa, BloomLevel: 5}
	shadowRow := model.Task{ID: "task_shadow", CommandID: "cmd_ab_pwto", ABGroupID: "abg_x",
		Status: model.StatusCompleted, ExpectedPaths: []string{"."}}
	writeWorkerQueue(t, maestroDir, "worker1", []model.Task{canonRow})
	writeWorkerQueue(t, maestroDir, "worker3", []model.Task{shadowRow})

	ops := &stubABOps{commitErr: errors.New("candidate worktree not found")}
	var work deferredWork
	qh.collectABGroupWork(abQueues(maestroDir, map[string][]model.Task{
		"worker1": {canonRow}, "worker3": {shadowRow},
	}), &work)
	qh.processABGroup(context.Background(), ops, work.abGroups[0])

	cs := readABState(t, maestroDir, "cmd_ab_pwto")
	g := cs.CandidateGroups["abg_x"]
	if g.Status != model.ABGroupDegraded {
		t.Fatalf("group status = %s, want degraded (timeout escape)", g.Status)
	}
	reason := cs.CancelledReasons["task_canon"]
	if len(reason) < len("superseded_by_retry:") || reason[:len("superseded_by_retry:")] != "superseded_by_retry:" {
		t.Errorf("canonical reason = %q, want superseded_by_retry:<repair>", reason)
	}
}

// Codex 差し戻し P1: repair enqueue 済み (RetryLineage に非候補 successor) の
// クラッシュ再入では、pending winner より repair が優先され intake されない。
func TestProcessABGroup_RepairPriorityOverPendingWinner(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab_rp")
	mutateABState(t, maestroDir, "cmd_ab_rp", func(cs *model.CommandState) {
		cs.CandidateGroups["abg_x"].PendingWinnerTaskID = "task_shadow"
		cs.RetryLineage["task_repair_prev"] = "task_canon"
	})
	rows := map[string][]model.Task{
		"worker1": {{ID: "task_canon", CommandID: "cmd_ab_rp", ABGroupID: "abg_x",
			Status: model.StatusCompleted, ExpectedPaths: []string{"."}}},
		"worker3": {{ID: "task_shadow", CommandID: "cmd_ab_rp", ABGroupID: "abg_x",
			Status: model.StatusCompleted, ExpectedPaths: []string{"."}}},
	}
	writeWorkerQueue(t, maestroDir, "worker1", rows["worker1"])
	writeWorkerQueue(t, maestroDir, "worker3", rows["worker3"])

	ops := &selectionTrackingABOps{}
	var work deferredWork
	qh.collectABGroupWork(abQueues(maestroDir, rows), &work)
	qh.processABGroup(context.Background(), ops, work.abGroups[0])

	if len(ops.intaken) != 0 {
		t.Errorf("winner must NOT be intaken when a repair is wired, got %v", ops.intaken)
	}
	cs := readABState(t, maestroDir, "cmd_ab_rp")
	g := cs.CandidateGroups["abg_x"]
	if g.Status != model.ABGroupDegraded {
		t.Fatalf("group status = %s, want degraded (repair priority)", g.Status)
	}
	if cs.CancelledReasons["task_canon"] != "superseded_by_retry:task_repair_prev" {
		t.Errorf("canonical reason = %q, want existing repair reused", cs.CancelledReasons["task_canon"])
	}
}

// Codex P2 提案: 孤児 (group なし) の completed 行の intake が失敗した場合、
// タグ剥がしの前に repair が必ず確保される。既に successor が wired なら
// 二重発行せずタグだけ剥がす。
func TestProcessABGroup_OrphanIntakeFailure_EnqueuesRepairBeforeStrip(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_ab_oif")
	mutateABState(t, maestroDir, "cmd_ab_oif", func(cs *model.CommandState) {
		cs.CandidateGroups = nil // legacy crash: group never registered
		delete(cs.TaskStates, "task_shadow")
	})
	doa := model.DefaultDefinitionOfAbort()
	canonRow := model.Task{ID: "task_canon", CommandID: "cmd_ab_oif", ABGroupID: "abg_lost",
		Status: model.StatusCompleted, Purpose: "p", Content: "c", AcceptanceCriteria: "a",
		ExpectedPaths: []string{"."}, DefinitionOfAbort: &doa, BloomLevel: 5}
	writeWorkerQueue(t, maestroDir, "worker1", []model.Task{canonRow})

	ops := &stubABOps{intakeErr: errors.New("intake merge conflicted")}
	var work deferredWork
	qh.collectABGroupWork(abQueues(maestroDir, map[string][]model.Task{"worker1": {canonRow}}), &work)
	if len(work.abGroups) != 1 {
		t.Fatalf("collected %d items, want 1", len(work.abGroups))
	}
	qh.processABGroup(context.Background(), ops, work.abGroups[0])

	q := readWorkerQueue(t, maestroDir, "worker1")
	var orig, repair *model.Task
	for i := range q.Tasks {
		if q.Tasks[i].ID == "task_canon" {
			orig = &q.Tasks[i]
		} else {
			repair = &q.Tasks[i]
		}
	}
	if repair == nil {
		t.Fatal("repair task must be enqueued before the tag strip")
	}
	if repair.ABGroupID != "" || repair.Status != model.StatusPending {
		t.Errorf("repair row = %+v, want non-A/B pending", repair)
	}
	if orig == nil || orig.ABGroupID != "" {
		t.Errorf("original row must be untagged after repair was ensured, got %+v", orig)
	}
	cs := readABState(t, maestroDir, "cmd_ab_oif")
	if cs.RetryLineage[repair.ID] != "task_canon" {
		t.Errorf("RetryLineage[%s] = %q, want task_canon", repair.ID, cs.RetryLineage[repair.ID])
	}

	// 再入 (successor wired 済み): 二重発行されない。
	retagged := *orig
	retagged.ABGroupID = "abg_lost"
	writeWorkerQueue(t, maestroDir, "worker1", []model.Task{retagged, *repair})
	var work2 deferredWork
	qh.collectABGroupWork(abQueues(maestroDir, map[string][]model.Task{"worker1": {retagged, *repair}}), &work2)
	qh.processABGroup(context.Background(), ops, work2.abGroups[0])
	q = readWorkerQueue(t, maestroDir, "worker1")
	if len(q.Tasks) != 2 {
		t.Errorf("re-entry must not issue a second repair, got %d rows", len(q.Tasks))
	}
}

// PR2: the mtime+size cache must serve unchanged files, notice rewrites
// (AtomicWrite bumps mtime), and prune deleted entries.
func TestReadABCommandStates_Cache(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	writeABState(t, maestroDir, "cmd_cache")

	if got := qh.readABCommandStates(); len(got) != 1 || got[0].CommandID != "cmd_cache" {
		t.Fatalf("first read = %v, want one state", got)
	}
	// Cached read returns the same parsed state.
	if got := qh.readABCommandStates(); len(got) != 1 {
		t.Fatalf("cached read = %d states, want 1", len(got))
	}
	// Rewrite with the group resolved: discovery must reflect it (the item
	// is still returned — filtering happens in collectABGroupWork).
	mutateABState(t, maestroDir, "cmd_cache", func(cs *model.CommandState) {
		cs.CandidateGroups["abg_x"].Status = model.ABGroupResolved
	})
	got := qh.readABCommandStates()
	if len(got) != 1 || got[0].CandidateGroups["abg_x"].Status != model.ABGroupResolved {
		t.Fatalf("post-rewrite read = %v, want resolved group", got)
	}
	// Deletion prunes the entry.
	if err := os.Remove(filepath.Join(maestroDir, "state", "commands", "cmd_cache.yaml")); err != nil {
		t.Fatal(err)
	}
	if got := qh.readABCommandStates(); len(got) != 0 {
		t.Fatalf("post-delete read = %d states, want 0", len(got))
	}
	qh.abStateCache.mu.Lock()
	n := len(qh.abStateCache.entries)
	qh.abStateCache.mu.Unlock()
	if n != 0 {
		t.Errorf("cache entries after prune = %d, want 0", n)
	}
}

// PR2: expected_paths resolve from whichever candidate row survives so a
// one-sided row loss cannot skew the Stage 2 deviation metric.
func TestABExpectedPaths_SharedFallback(t *testing.T) {
	qh, maestroDir := abTestQueueHandler(t)
	canonical := &model.ABCandidate{TaskID: "task_canon", WorkerID: "worker1"}
	shadow := &model.ABCandidate{TaskID: "task_shadow", WorkerID: "worker3"}

	// Only the shadow row survives (canonical dead-lettered).
	writeWorkerQueue(t, maestroDir, "worker3", []model.Task{{
		ID: "task_shadow", CommandID: "cmd_ep", ExpectedPaths: []string{"src", "docs"},
	}})
	got := qh.abExpectedPaths(canonical, shadow)
	if len(got) != 2 || got[0] != "src" {
		t.Errorf("expected paths = %v, want shadow row's [src docs]", got)
	}
	// Both rows gone: nil (Stage 2 skips the deviation metric).
	if got := qh.abExpectedPaths(&model.ABCandidate{TaskID: "x", WorkerID: "worker9"}); got != nil {
		t.Errorf("expected nil for missing rows, got %v", got)
	}
}
