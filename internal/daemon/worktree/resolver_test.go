package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// fakeSignalStore is an in-memory SignalStore for resolver tests.
type fakeSignalStore struct {
	mu      sync.Mutex
	signals map[string]*model.PlannerSignal // key = cmd|phase|worker
}

func newFakeSignalStore() *fakeSignalStore {
	return &fakeSignalStore{signals: map[string]*model.PlannerSignal{}}
}

func sigKey(cmd, phase, worker string) string { return cmd + "|" + phase + "|" + worker }

func (f *fakeSignalStore) put(s model.PlannerSignal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := s
	f.signals[sigKey(s.CommandID, s.PhaseID, s.WorkerID)] = &cp
}

func (f *fakeSignalStore) get(cmd, phase, worker string) *model.PlannerSignal {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.signals[sigKey(cmd, phase, worker)]; ok {
		cp := *s
		return &cp
	}
	return nil
}

func (f *fakeSignalStore) UpdateMergeConflictSignal(cmd, phase, worker string, fn func(*model.PlannerSignal) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.signals[sigKey(cmd, phase, worker)]
	if err := fn(s); err != nil {
		return err
	}
	return nil
}

// setupResolverTest builds a manager with one worker in conflict state and a
// signal store seeded with a merge_conflict signal carrying the given gen.
func setupResolverTest(t *testing.T, gen string) (*Manager, *fakeSignalStore, string, string, string) {
	t.Helper()
	dir := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, dir)
	cmdID := "cmdR"
	workerID := "workerA"
	phaseID := "phase1"

	if err := createForCommand(wm, cmdID, []string{workerID}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Move worker to conflict directly via state file: created→active→conflict.
	state, err := wm.loadState(cmdID)
	if err != nil {
		t.Fatal(err)
	}
	now := wm.clock.Now().UTC().Format("2006-01-02T15:04:05Z")
	ws := wm.findWorker(state, workerID)
	if ws == nil {
		t.Fatal("worker missing")
	}
	if err := wm.setWorkerStatus(ws, model.WorktreeStatusActive, now); err != nil {
		t.Fatal(err)
	}
	if err := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, now); err != nil {
		t.Fatal(err)
	}
	if err := wm.saveState(cmdID, state); err != nil {
		t.Fatal(err)
	}

	store := newFakeSignalStore()
	store.put(model.PlannerSignal{
		Kind:               "merge_conflict",
		CommandID:          cmdID,
		PhaseID:            phaseID,
		WorkerID:           workerID,
		ConflictGeneration: gen,
	})
	wm.SetSignalStore(store)
	return wm, store, cmdID, phaseID, workerID
}

func TestDispatchConflictResolution_OK(t *testing.T) {
	t.Parallel()
	gen := "abc123"
	wm, store, cmd, phase, worker := setupResolverTest(t, gen)

	if err := wm.DispatchConflictResolution(cmd, phase, worker, gen); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	state, _ := wm.loadState(cmd)
	if got := wm.findWorker(state, worker).Status; got != model.WorktreeStatusResolving {
		t.Errorf("worker status = %s, want resolving", got)
	}
	if s := store.get(cmd, phase, worker); s.ResolutionState != "dispatched" {
		t.Errorf("resolution_state = %q, want dispatched", s.ResolutionState)
	}
}

func TestDispatchConflictResolution_CASMismatch(t *testing.T) {
	t.Parallel()
	wm, store, cmd, phase, worker := setupResolverTest(t, "good")
	err := wm.DispatchConflictResolution(cmd, phase, worker, "bad")
	if !errors.Is(err, errConflictGenerationMismatch) {
		t.Fatalf("expected errConflictGenerationMismatch, got %v", err)
	}
	state, _ := wm.loadState(cmd)
	if got := wm.findWorker(state, worker).Status; got != model.WorktreeStatusConflict {
		t.Errorf("worker status leaked to %s on CAS mismatch", got)
	}
	if s := store.get(cmd, phase, worker); s.ResolutionState == "dispatched" {
		t.Errorf("signal must not be marked dispatched on CAS mismatch")
	}
}

func TestDiscardResolverEdits(t *testing.T) {
	t.Parallel()
	gen := "g6"
	wm, _, cmd, _, worker := setupResolverTest(t, gen)

	// Dirty the integration worktree.
	intPath := wm.integrationWorktreePath(cmd)
	scratch := filepath.Join(intPath, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("junk\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.DiscardResolverEdits(cmd, worker); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch.txt should be removed by discard, stat err=%v", err)
	}
}
