package worktree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	wm, store, cmd, phase, worker := setupResolverTest(t, "good")
	err := wm.DispatchConflictResolution(cmd, phase, worker, "bad")
	if !errors.Is(err, ErrConflictGenerationMismatch) {
		t.Fatalf("expected ErrConflictGenerationMismatch, got %v", err)
	}
	state, _ := wm.loadState(cmd)
	if got := wm.findWorker(state, worker).Status; got != model.WorktreeStatusConflict {
		t.Errorf("worker status leaked to %s on CAS mismatch", got)
	}
	if s := store.get(cmd, phase, worker); s.ResolutionState == "dispatched" {
		t.Errorf("signal must not be marked dispatched on CAS mismatch")
	}
}

// resolverPrep brings worker to resolving and writes a non-conflicted file.
func resolverPrep(t *testing.T, wm *Manager, cmd, phase, worker, gen string) {
	t.Helper()
	if err := wm.DispatchConflictResolution(cmd, phase, worker, gen); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

func writeIntFile(t *testing.T, wm *Manager, cmd, rel, content string) string {
	t.Helper()
	full := filepath.Join(wm.integrationWorktreePath(cmd), rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestCommitResolvedConflict_OK(t *testing.T) {
	gen := "g1"
	wm, _, cmd, phase, worker := setupResolverTest(t, gen)
	resolverPrep(t, wm, cmd, phase, worker, gen)

	writeIntFile(t, wm, cmd, "resolved.txt", "clean content\n")

	if err := wm.CommitResolvedConflict(cmd, phase, worker, gen, []string{"resolved.txt"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	state, _ := wm.loadState(cmd)
	if got := wm.findWorker(state, worker).Status; got != model.WorktreeStatusIntegrated {
		t.Errorf("worker status = %s, want integrated", got)
	}
}

func TestCommitResolvedConflict_LsFilesUNonEmpty(t *testing.T) {
	gen := "g2"
	wm, _, cmd, phase, worker := setupResolverTest(t, gen)
	resolverPrep(t, wm, cmd, phase, worker, gen)

	// Inject an unmerged (stage>0) index entry via `git update-index --index-info`
	// by writing the line to a temp file and feeding it through a shell pipe.
	intPath := wm.integrationWorktreePath(cmd)
	writeIntFile(t, wm, cmd, "seed.txt", "seed\n")
	blob, err := wm.gitOutputInDir(intPath, "hash-object", "-w", "seed.txt")
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	blob = strings.TrimSpace(blob)

	// Use os/exec directly so we can pipe stdin to update-index --index-info.
	c := exec.Command("git", "-C", intPath, "update-index", "--index-info")
	c.Stdin = strings.NewReader("100644 " + blob + " 1\tconflicted.txt\n")
	if out, err := c.CombinedOutput(); err != nil {
		t.Skipf("cannot inject unmerged entry portably: %v\n%s", err, out)
	}
	out, _ := wm.gitOutputInDir(intPath, "ls-files", "-u")
	if strings.TrimSpace(out) == "" {
		t.Skip("unmerged entry injection produced empty ls-files -u; skipping")
	}

	err = wm.CommitResolvedConflict(cmd, phase, worker, gen, []string{"seed.txt"})
	if !errors.Is(err, ErrResolverPreconditionFailed) {
		t.Fatalf("expected precondition failure, got %v", err)
	}
}

func TestCommitResolvedConflict_FileSubsetViolation(t *testing.T) {
	gen := "g3"
	wm, _, cmd, phase, worker := setupResolverTest(t, gen)
	resolverPrep(t, wm, cmd, phase, worker, gen)

	// Two dirty files, but only one is allowed.
	writeIntFile(t, wm, cmd, "a.txt", "A\n")
	writeIntFile(t, wm, cmd, "b.txt", "B\n")

	err := wm.CommitResolvedConflict(cmd, phase, worker, gen, []string{"a.txt"})
	if !errors.Is(err, ErrResolverPreconditionFailed) {
		t.Fatalf("expected precondition failure, got %v", err)
	}
}

func TestCommitResolvedConflict_ConflictMarkerRemains(t *testing.T) {
	gen := "g4"
	wm, _, cmd, phase, worker := setupResolverTest(t, gen)
	resolverPrep(t, wm, cmd, phase, worker, gen)

	writeIntFile(t, wm, cmd, "x.txt", "ok\n<<<<<<< HEAD\nfoo\n=======\nbar\n>>>>>>> theirs\n")

	err := wm.CommitResolvedConflict(cmd, phase, worker, gen, []string{"x.txt"})
	if !errors.Is(err, ErrResolverPreconditionFailed) {
		t.Fatalf("expected precondition failure, got %v", err)
	}
}

func TestCommitResolvedConflict_AttemptLimitRevertsAndIncrements(t *testing.T) {
	gen := "g5"
	wm, _, cmd, phase, worker := setupResolverTest(t, gen)
	resolverPrep(t, wm, cmd, phase, worker, gen)

	// Always-failing commit: dirty file is not in allowed list.
	writeIntFile(t, wm, cmd, "x.txt", "x\n")

	// Attempt 1: failure, worker still resolving.
	if err := wm.CommitResolvedConflict(cmd, phase, worker, gen, []string{"unrelated.txt"}); err == nil {
		t.Fatal("attempt 1 should fail")
	}
	state, _ := wm.loadState(cmd)
	if got := wm.findWorker(state, worker).Status; got != model.WorktreeStatusResolving {
		t.Errorf("after attempt 1: status = %s, want resolving", got)
	}

	// Attempt 2: failure, hits maxResolveAttempts → revert to conflict.
	if err := wm.CommitResolvedConflict(cmd, phase, worker, gen, []string{"unrelated.txt"}); err == nil {
		t.Fatal("attempt 2 should fail")
	}
	state, _ = wm.loadState(cmd)
	if got := wm.findWorker(state, worker).Status; got != model.WorktreeStatusConflict {
		t.Errorf("after attempt-limit: status = %s, want conflict", got)
	}
	if state.Integration.MergeFailureCount != 1 {
		t.Errorf("MergeFailureCount = %d, want 1", state.Integration.MergeFailureCount)
	}
}

func TestDiscardResolverEdits(t *testing.T) {
	gen := "g6"
	wm, _, cmd, _, worker := setupResolverTest(t, gen)

	// Dirty the integration worktree.
	writeIntFile(t, wm, cmd, "scratch.txt", "junk\n")
	if err := wm.DiscardResolverEdits(cmd, worker); err != nil {
		t.Fatalf("discard: %v", err)
	}
	intPath := wm.integrationWorktreePath(cmd)
	if _, err := os.Stat(filepath.Join(intPath, "scratch.txt")); !os.IsNotExist(err) {
		t.Errorf("scratch.txt should be removed by discard, stat err=%v", err)
	}
}
