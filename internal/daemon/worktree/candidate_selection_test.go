package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// selectionFixture builds: integration + two committed candidates. Candidate
// A creates marker_a.txt, candidate B creates marker_b.txt.
func selectionFixture(t *testing.T) (wm *Manager, commandID string, inputs []ABSelectionInput) {
	t.Helper()
	projectRoot := testutil.InitTestGitRepo(t)
	wm = newTestWorktreeManager(t, projectRoot)
	commandID = "cmd_ab_select"

	for _, c := range []struct{ task, file string }{
		{"task_sel_A", "marker_a.txt"},
		{"task_sel_B", "marker_b.txt"},
	} {
		path, branch, err := wm.EnsureCandidateWorktree(commandID, c.task)
		if err != nil {
			t.Fatalf("EnsureCandidateWorktree(%s): %v", c.task, err)
		}
		if err := os.WriteFile(filepath.Join(path, c.file), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := wm.CommitCandidateChanges(commandID, c.task); err != nil {
			t.Fatalf("CommitCandidateChanges(%s): %v", c.task, err)
		}
		inputs = append(inputs, ABSelectionInput{TaskID: c.task, Branch: branch})
	}
	return wm, commandID, inputs
}

func integrationHead(t *testing.T, wm *Manager, commandID string) string {
	t.Helper()
	out, err := wm.gitOutput("rev-parse", "maestro/"+commandID+"/integration")
	if err != nil {
		t.Fatalf("rev-parse integration: %v", err)
	}
	return strings.TrimSpace(out)
}

func TestRunCandidateSelection_DiscriminatingVerifier(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)
	pre := integrationHead(t, wm, commandID)

	// Discriminating verifier: passes on the baseline (marker_b.txt does
	// not exist there), passes for candidate A, fails for candidate B which
	// introduces marker_b.txt — a "candidate B breaks the build" analogue.
	// B is listed first to prove the win comes from the score, not order.
	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_test",
		[]ABSelectionInput{inputs[1], inputs[0]},
		[]string{"test ! -f marker_b.txt"})
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if outcome.Degraded || outcome.WinnerTaskID != "task_sel_A" {
		t.Errorf("outcome = %+v, want winner task_sel_A", outcome)
	}

	// Integration restored, marker cleared.
	if got := integrationHead(t, wm, commandID); got != pre {
		t.Errorf("integration HEAD changed: %s → %s", pre, got)
	}
	state, _ := wm.loadState(commandID)
	if state.ABSelection != nil {
		t.Errorf("selection marker not cleared: %+v", state.ABSelection)
	}
	intPath := wm.integrationWorktreePath(commandID)
	if statusOut, _ := wm.gitOutputInDir(intPath, "status", "--porcelain"); strings.TrimSpace(statusOut) != "" {
		t.Errorf("integration worktree left dirty:\n%s", statusOut)
	}
}

func TestRunCandidateSelection_TieGoesToFirstInput(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)

	// Verifier passes for both → tie → first input (canonical slot) wins.
	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_tie",
		inputs, []string{"true"})
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if outcome.Degraded || outcome.WinnerTaskID != inputs[0].TaskID {
		t.Errorf("tie must go to the first (canonical) input, got %+v", outcome)
	}
}

func TestRunCandidateSelection_BrokenBaselineDegrades(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)

	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_broken",
		inputs, []string{"false"})
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if !outcome.Degraded {
		t.Errorf("baseline-failing verifier must degrade, got %+v", outcome)
	}
	if outcome.Evidence["stage0"] != "verifier_broken" {
		t.Errorf("evidence = %v, want stage0=verifier_broken", outcome.Evidence)
	}
}

func TestRunCandidateSelection_NoVerifierDegrades(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)

	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_nov", inputs, nil)
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if !outcome.Degraded || outcome.Evidence["stage0"] != "no_verifier" {
		t.Errorf("no-verifier selection must degrade with evidence, got %+v", outcome)
	}
}

func TestIntakeWinner_MergesIntoWorkerBranch(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)

	// Worker worktree does not exist yet: intake must create it lazily.
	if err := wm.IntakeWinner(commandID, "worker1", inputs[0].Branch, inputs[0].TaskID); err != nil {
		t.Fatalf("IntakeWinner: %v", err)
	}
	state, _ := wm.loadState(commandID)
	var workerPath string
	for _, ws := range state.Workers {
		if ws.WorkerID == "worker1" {
			workerPath = ws.Path
		}
	}
	if workerPath == "" {
		t.Fatal("worker worktree should have been created lazily for intake")
	}
	if _, err := os.Stat(filepath.Join(workerPath, "marker_a.txt")); err != nil {
		t.Errorf("winner content missing from worker worktree: %v", err)
	}
}

func TestReconcile_RestoresStaleABSelectionMarker(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)
	pre := integrationHead(t, wm, commandID)
	intPath := wm.integrationWorktreePath(commandID)

	// Simulate a crash mid-selection: candidate merged into integration,
	// marker present, no restore.
	if err := wm.gitRunInDir(intPath, "merge", "--no-ff", "--no-commit", inputs[0].Branch); err != nil {
		t.Fatalf("setup merge: %v", err)
	}
	st2, _ := wm.loadState(commandID)
	st2.ABSelection = &model.ABSelectionMarker{GroupID: "abg_crash", PreSHA: pre, StartedAt: "2026-06-12T00:00:00Z"}
	if err := wm.saveState(commandID, st2); err != nil {
		t.Fatalf("save marker: %v", err)
	}

	wm.Reconcile()

	if got := integrationHead(t, wm, commandID); got != pre {
		t.Errorf("Reconcile should restore integration HEAD to %s, got %s", pre, got)
	}
	st3, _ := wm.loadState(commandID)
	if st3.ABSelection != nil {
		t.Errorf("Reconcile should clear the stale marker, got %+v", st3.ABSelection)
	}
	if statusOut, _ := wm.gitOutputInDir(intPath, "status", "--porcelain"); strings.TrimSpace(statusOut) != "" {
		t.Errorf("integration left dirty after recovery:\n%s", statusOut)
	}
}

// Audit #15: single-candidate mode (walkover verification) — a healthy
// verifier failing the sole finisher must surface SoleCandidateFailed so the
// caller degrades with a repair instead of intaking verified-bad work.
func TestRunCandidateSelection_SoleCandidateFailed(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)

	// Passes on the baseline (no marker_b.txt) but fails for candidate B,
	// the sole input.
	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_sole",
		[]ABSelectionInput{inputs[1]},
		[]string{"test ! -f marker_b.txt"})
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if !outcome.SoleCandidateFailed || outcome.Degraded || outcome.WinnerTaskID != "" {
		t.Errorf("outcome = %+v, want SoleCandidateFailed only", outcome)
	}

	// The same sole candidate with a passing verifier wins normally.
	outcome, err = wm.RunCandidateSelection(context.Background(), commandID, "abg_sole",
		[]ABSelectionInput{inputs[1]},
		[]string{"true"})
	if err != nil {
		t.Fatalf("RunCandidateSelection (passing): %v", err)
	}
	if outcome.SoleCandidateFailed || outcome.WinnerTaskID != inputs[1].TaskID {
		t.Errorf("outcome = %+v, want sole winner %s", outcome, inputs[1].TaskID)
	}
}
