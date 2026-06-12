package plan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// abFanoutFixture simulates the post-submit state: a sealed command with one
// canonical pending task on worker1 (claude runtime), plus a codex worker3.
func abFanoutFixture(t *testing.T) (maestroDir string, opts SubmitOptions, res *SubmitResult) {
	t.Helper()
	maestroDir = setupMaestroDir(t)
	commandID := "cmd_ab_fanout"
	taskID := "task_ab_canon01"

	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{taskID},
			TaskDependencies:  map[string][]string{taskID: {}},
			TaskStates:        map[string]model.Status{taskID: model.StatusPending},
			CancelledReasons:  map[string]string{},
			AppliedResultIDs:  map[string]string{},
		},
		RetryTracking: model.RetryTracking{RetryLineage: map[string]string{}},
		CreatedAt:     "2026-06-12T00:00:00Z",
		UpdatedAt:     "2026-06-12T00:00:00Z",
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", commandID+".yaml"), state); err != nil {
		t.Fatal(err)
	}

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{{
			ID:            taskID,
			CommandID:     commandID,
			Purpose:       "implement feature",
			Content:       "do it",
			BloomLevel:    5,
			Status:        model.StatusPending,
			ExpectedPaths: []string{"."},
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.Worktree.Enabled = true
	cfg.ABTest.Enabled = ptr.Bool(true)
	cfg.Agents.Workers.Count = 3
	cfg.Agents.Workers.DefaultModel = "sonnet"
	cfg.Agents.Workers.Models = map[string]string{
		"worker1": "opus",
		"worker2": "sonnet",
		"worker3": "codex",
	}

	opts = SubmitOptions{
		CommandID:  commandID,
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	}
	res = &SubmitResult{
		CommandID: commandID,
		Tasks: []SubmitTaskResult{
			{Name: "canon", TaskID: taskID, Worker: "worker1", Model: "opus"},
		},
	}
	return maestroDir, opts, res
}

func loadQueue(t *testing.T, maestroDir, workerID string) model.TaskQueue {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(maestroDir, "queue", workerID+".yaml"))
	if err != nil {
		t.Fatalf("read %s queue: %v", workerID, err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("parse %s queue: %v", workerID, err)
	}
	return tq
}

func TestMaybeCreateABCandidates_CreatesShadowAndGroup(t *testing.T) {
	maestroDir, opts, res := abFanoutFixture(t)
	sm := NewStateManager(maestroDir, opts.LockMap)

	warnings := maybeCreateABCandidates(opts, sm, res, nil)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	// Shadow row lands on the codex worker (worker3) with the group ID.
	shadowQ := loadQueue(t, maestroDir, "worker3")
	if len(shadowQ.Tasks) != 1 {
		t.Fatalf("shadow queue rows = %d, want 1", len(shadowQ.Tasks))
	}
	shadow := shadowQ.Tasks[0]
	if shadow.ABGroupID == "" || shadow.Status != model.StatusPending || shadow.BloomLevel != 5 {
		t.Errorf("shadow row malformed: %+v", shadow)
	}

	// Canonical row tagged with the same group.
	canonQ := loadQueue(t, maestroDir, "worker1")
	if canonQ.Tasks[0].ABGroupID != shadow.ABGroupID {
		t.Errorf("canonical ABGroupID = %q, want %q", canonQ.Tasks[0].ABGroupID, shadow.ABGroupID)
	}

	// State: shadow registered in TaskStates/deps but NOT in required IDs;
	// group recorded as racing with persisted model/bloom.
	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.TaskStates[shadow.ID]; !ok {
		t.Error("shadow must be registered in TaskStates")
	}
	for _, id := range state.RequiredTaskIDs {
		if id == shadow.ID {
			t.Error("shadow must NOT be in RequiredTaskIDs")
		}
	}
	g, ok := state.CandidateGroups[shadow.ABGroupID]
	if !ok {
		t.Fatalf("candidate group %q missing", shadow.ABGroupID)
	}
	if g.Status != model.ABGroupRacing || g.CanonicalTaskID != "task_ab_canon01" {
		t.Errorf("group malformed: %+v", g)
	}
	if c := g.CandidateByTask(shadow.ID); c == nil || c.Model != "codex" || c.BloomLevel != 5 {
		t.Errorf("shadow candidate metadata malformed: %+v", c)
	}
	if !state.ABBarrierActive("task_ab_canon01") {
		t.Error("barrier must be active for the canonical task")
	}

	// Idempotent: second pass creates nothing new.
	warnings = maybeCreateABCandidates(opts, sm, res, nil)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings on second pass: %v", warnings)
	}
	if q := loadQueue(t, maestroDir, "worker3"); len(q.Tasks) != 1 {
		t.Errorf("second pass duplicated the shadow: %d rows", len(q.Tasks))
	}
}

func TestMaybeCreateABCandidates_Gates(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		maestroDir, opts, res := abFanoutFixture(t)
		opts.Config.ABTest.Enabled = ptr.Bool(false)
		sm := NewStateManager(maestroDir, opts.LockMap)
		if w := maybeCreateABCandidates(opts, sm, res, nil); len(w) != 0 {
			t.Errorf("disabled A/B must be silent, got %v", w)
		}
		if q := loadQueue(t, maestroDir, "worker1"); q.Tasks[0].ABGroupID != "" {
			t.Error("canonical must be untouched when disabled")
		}
	})

	t.Run("below min bloom", func(t *testing.T) {
		maestroDir, opts, res := abFanoutFixture(t)
		opts.Config.ABTest.MinBloomLevel = ptr.Int(6)
		sm := NewStateManager(maestroDir, opts.LockMap)
		if w := maybeCreateABCandidates(opts, sm, res, nil); len(w) != 0 {
			t.Errorf("below-threshold task must be silently skipped, got %v", w)
		}
		if q := loadQueue(t, maestroDir, "worker1"); q.Tasks[0].ABGroupID != "" {
			t.Error("canonical must be untouched below min bloom")
		}
	})

	t.Run("pinned task", func(t *testing.T) {
		maestroDir, opts, res := abFanoutFixture(t)
		sm := NewStateManager(maestroDir, opts.LockMap)
		if w := maybeCreateABCandidates(opts, sm, res, map[string]bool{"canon": true}); len(w) != 0 {
			t.Errorf("pinned task must be silently skipped, got %v", w)
		}
		if q := loadQueue(t, maestroDir, "worker1"); q.Tasks[0].ABGroupID != "" {
			t.Error("canonical must be untouched for pinned tasks")
		}
	})

	t.Run("no other runtime", func(t *testing.T) {
		maestroDir, opts, res := abFanoutFixture(t)
		opts.Config.Agents.Workers.Models = map[string]string{
			"worker1": "opus", "worker2": "sonnet", "worker3": "haiku",
		}
		sm := NewStateManager(maestroDir, opts.LockMap)
		w := maybeCreateABCandidates(opts, sm, res, nil)
		if len(w) != 1 {
			t.Fatalf("expected one warning for missing runtime, got %v", w)
		}
	})
}

func TestPickShadowWorker(t *testing.T) {
	states := []WorkerState{
		{WorkerID: "worker1", Model: "opus", PendingCount: 1},
		{WorkerID: "worker2", Model: "codex", PendingCount: 3},
		{WorkerID: "worker3", Model: "codex", PendingCount: 1},
		{WorkerID: "worker4", Model: "gemini", PendingCount: 0},
	}
	// canonical = claude runtime → other runtimes: codex/gemini; least loaded = worker4.
	id, mdl, ok := pickShadowWorker(states, "worker1", "opus", 10)
	if !ok || id != "worker4" || mdl != "gemini" {
		t.Errorf("pickShadowWorker = (%q,%q,%v), want (worker4, gemini, true)", id, mdl, ok)
	}
	// canonical = codex → other runtimes are claude (worker1, pending 1)
	// and gemini (worker4, pending 0); least loaded wins.
	id, _, ok = pickShadowWorker(states, "worker3", "codex", 10)
	if !ok || id != "worker4" {
		t.Errorf("pickShadowWorker (codex canonical) = (%q,%v), want worker4", id, ok)
	}
	// capacity respected.
	_, _, ok = pickShadowWorker(states[:2], "worker1", "opus", 3)
	if ok {
		t.Error("expected no shadow worker when only candidate is at capacity")
	}
}

// --- 2026-06-12 audit regression tests ---

func abGroupState(status model.ABGroupStatus) *model.CommandState {
	return &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_ab_guard",
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{"task_canon"},
			TaskStates: map[string]model.Status{
				"task_canon":  model.StatusCompleted,
				"task_shadow": model.StatusCompleted,
			},
			CancelledReasons: map[string]string{},
		},
		RetryTracking: model.RetryTracking{RetryLineage: map[string]string{}},
		CandidateGroups: map[string]*model.CandidateGroup{
			"abg_x": {
				Status:          status,
				CanonicalTaskID: "task_canon",
				Candidates: []model.ABCandidate{
					{TaskID: "task_canon", WorkerID: "worker1"},
					{TaskID: "task_shadow", WorkerID: "worker3"},
				},
				CreatedAt: "2026-06-12T00:00:00Z",
				UpdatedAt: "2026-06-12T00:00:00Z",
			},
		},
		CreatedAt: "2026-06-12T00:00:00Z",
		UpdatedAt: "2026-06-12T00:00:00Z",
	}
}

// Audit #4: phaseless commands must not plan_complete while a candidate
// group is unresolved — both raw TaskStates can read completed mid-race.
func TestCanComplete_BlockedByUnresolvedABGroup(t *testing.T) {
	state := abGroupState(model.ABGroupRacing)
	if _, err := CanComplete(state); err == nil {
		t.Fatal("CanComplete must refuse while the A/B group is unresolved")
	} else {
		var re *retryableError
		if !errors.As(err, &re) {
			t.Fatalf("error must be retryable (daemon resolves shortly), got %v", err)
		}
	}

	state.CandidateGroups["abg_x"].Status = model.ABGroupResolved
	state.CandidateGroups["abg_x"].WinnerTaskID = "task_canon"
	if _, err := CanComplete(state); err != nil {
		t.Fatalf("resolved group must not block completion: %v", err)
	}
}

// Audit #4: the publish/cleanup gate treats an unresolved group as
// in-flight work even when every TaskStates entry is terminal.
func TestHasNonTerminalTaskState_UnresolvedABGroup(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	state := abGroupState(model.ABGroupSelecting)
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", state.CommandID+".yaml"), state); err != nil {
		t.Fatal(err)
	}
	r := NewPlanStateReader(NewStateManager(maestroDir, lock.NewMutexMap()))
	got, err := r.HasNonTerminalTaskState(state.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("unresolved A/B group must count as non-terminal work")
	}
}

// Audit #3: Planner-initiated retries are rejected for unresolved group
// members and for superseded losers; the surviving line stays retryable.
func TestValidateRetryRequest_ABGuards(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	sm := NewStateManager(maestroDir, lock.NewMutexMap())

	write := func(mut func(*model.CommandState)) {
		state := abGroupState(model.ABGroupResolved)
		state.TaskStates["task_canon"] = model.StatusFailed
		state.TaskStates["task_shadow"] = model.StatusFailed
		mut(state)
		if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", state.CommandID+".yaml"), state); err != nil {
			t.Fatal(err)
		}
	}
	opts := func(retryOf string) RetryOptions {
		doa := model.DefaultDefinitionOfAbort()
		return RetryOptions{
			CommandID: "cmd_ab_guard", RetryOf: retryOf, Purpose: "p",
			Content: "c", AcceptanceCriteria: "a", BloomLevel: 3,
			ExpectedPaths: []string{"."}, DefinitionOfAbort: &doa,
		}
	}

	write(func(cs *model.CommandState) { cs.CandidateGroups["abg_x"].Status = model.ABGroupRacing })
	if _, err := validateRetryRequest(sm, opts("task_canon")); err == nil {
		t.Error("retry of an unresolved candidate must be rejected")
	}

	write(func(cs *model.CommandState) { cs.CandidateGroups["abg_x"].WinnerTaskID = "task_canon" })
	if _, err := validateRetryRequest(sm, opts("task_shadow")); err == nil {
		t.Error("retry of a superseded loser must be rejected")
	}
	if _, err := validateRetryRequest(sm, opts("task_canon")); err != nil {
		t.Errorf("retry of the surviving winner must be allowed: %v", err)
	}
}

// Audit #5 (state-first order): a group already registered in state makes a
// re-run of the fan-out a silent no-op even when the queue tag is missing —
// completing the queue side is the recovery's job, not the fan-out's.
func TestMaybeCreateABCandidates_SkipsWhenGroupAlreadyInState(t *testing.T) {
	maestroDir, opts, res := abFanoutFixture(t)
	sm := NewStateManager(maestroDir, opts.LockMap)

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	state.CandidateGroups = map[string]*model.CandidateGroup{
		"abg_task_ab_canon01": {
			Status:          model.ABGroupRacing,
			CanonicalTaskID: "task_ab_canon01",
			Candidates: []model.ABCandidate{
				{TaskID: "task_ab_canon01", WorkerID: "worker1"},
				{TaskID: "task_prev_shadow", WorkerID: "worker3"},
			},
			CreatedAt: "2026-06-12T00:00:00Z", UpdatedAt: "2026-06-12T00:00:00Z",
		},
	}
	if err := sm.SaveState(state); err != nil {
		t.Fatal(err)
	}

	if w := maybeCreateABCandidates(opts, sm, res, nil); len(w) != 0 {
		t.Fatalf("unexpected warnings: %v", w)
	}
	if q := loadQueue(t, maestroDir, "worker1"); q.Tasks[0].ABGroupID != "" {
		t.Error("fan-out must not re-tag when the group already exists in state")
	}
	if _, err := os.Stat(filepath.Join(maestroDir, "queue", "worker3.yaml")); err == nil {
		if q := loadQueue(t, maestroDir, "worker3"); len(q.Tasks) != 0 {
			t.Error("fan-out must not enqueue a second shadow")
		}
	}
}
