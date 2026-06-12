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
	removed   []string
	intakeErr error
}

func (s *stubABOps) CommitCandidateChanges(string, string) error { return nil }
func (s *stubABOps) RunCandidateSelection(context.Context, string, string, []worktree.ABSelectionInput, []string) (*worktree.ABSelectionOutcome, error) {
	return &worktree.ABSelectionOutcome{}, nil
}
func (s *stubABOps) IntakeWinner(string, string, string, string) error { return s.intakeErr }
func (s *stubABOps) RemoveCandidateWorktree(_ string, taskID string) error {
	s.removed = append(s.removed, taskID)
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
	ops := &stubABOps{}

	qh.resolveABGroup("cmd_ab1", "abg_x", "task_canon", false, "", map[string]string{"winner": "task_canon"}, ops)

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
	ops := &stubABOps{}

	qh.resolveABGroup("cmd_ab2", "abg_x", "task_shadow", false, "", nil, ops)

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
	ops := &stubABOps{}

	qh.resolveABGroup("cmd_ab3", "abg_x", "", true, "intake conflict",
		map[string]string{"degraded": "intake_conflict"}, ops, "superseded_by_retry:task_repair01")

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
	if len(ops.removed) != 1 || ops.removed[0] != "task_shadow" {
		t.Errorf("loser worktree removal = %v, want [task_shadow]", ops.removed)
	}

	// Idempotent: re-resolving a terminal group is a no-op.
	qh.resolveABGroup("cmd_ab3", "abg_x", "task_canon", false, "", nil, ops)
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

	qh.finalizeABWinner(context.Background(), ops, item, g,
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
