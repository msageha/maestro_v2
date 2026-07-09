package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		[]string{"test ! -f marker_b.txt"}, nil, nil)
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
		inputs, []string{"true"}, nil, nil)
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
		inputs, []string{"false"}, nil, nil)
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

	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_nov", inputs, nil, nil, nil)
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
		[]string{"test ! -f marker_b.txt"}, nil, nil)
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if !outcome.SoleCandidateFailed || outcome.Degraded || outcome.WinnerTaskID != "" {
		t.Errorf("outcome = %+v, want SoleCandidateFailed only", outcome)
	}

	// The same sole candidate with a passing verifier wins normally.
	outcome, err = wm.RunCandidateSelection(context.Background(), commandID, "abg_sole",
		[]ABSelectionInput{inputs[1]},
		[]string{"true"}, nil, nil)
	if err != nil {
		t.Fatalf("RunCandidateSelection (passing): %v", err)
	}
	if outcome.SoleCandidateFailed || outcome.WinnerTaskID != inputs[1].TaskID {
		t.Errorf("outcome = %+v, want sole winner %s", outcome, inputs[1].TaskID)
	}
}

// --- PR2: Stage 2 metrics tiebreak + flake guard ---

func TestStage2Tiebreak(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b candidateMetrics
		want int
		via  string
	}{
		{"deviation wins exactly", candidateMetrics{Deviations: 1}, candidateMetrics{Deviations: 0}, 1, "expected_paths_deviation"},
		{"lines within 10% tie -> files", candidateMetrics{Lines: 100, Files: 20}, candidateMetrics{Lines: 95, Files: 10}, 1, "files_changed"},
		{"lines beyond 10% decides", candidateMetrics{Lines: 100}, candidateMetrics{Lines: 60}, 1, "diff_lines"},
		{"all tie -> canonical first", candidateMetrics{Lines: 10, Files: 2}, candidateMetrics{Lines: 10, Files: 2}, 0, "tie_canonical_first"},
		{"zero metrics tie", candidateMetrics{}, candidateMetrics{}, 0, "tie_canonical_first"},
		{"files within margin tie", candidateMetrics{Files: 10}, candidateMetrics{Files: 10}, 0, "tie_canonical_first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := map[string]string{}
			got := stage2Tiebreak(tc.a, tc.b, ev)
			if got != tc.want || ev["stage2_decision"] != tc.via {
				t.Errorf("tiebreak = %d via %q, want %d via %q", got, ev["stage2_decision"], tc.want, tc.via)
			}
		})
	}
}

func TestParseNumstat(t *testing.T) {
	t.Parallel()
	out := "3\t1\tsrc/a.go\n-\t-\tassets/logo.png\n5\t0\tdocs/readme.md\n2\t2\tgo.sum\n"
	m := parseNumstat(out, []string{"src"})
	// go.sum is a dependency manifest (always allowed); docs/readme.md and
	// the binary asset are outside "src".
	if m.Files != 4 || m.Lines != 3+1+5+0+2+2 || m.Binaries != 1 || m.Deviations != 2 {
		t.Errorf("metrics = %+v, want files=4 lines=13 binaries=1 deviations=2", m)
	}
	// Empty expected paths: deviations are not counted.
	if m := parseNumstat(out, nil); m.Deviations != 0 {
		t.Errorf("deviations with unknown scope = %d, want 0", m.Deviations)
	}
}

// Stage 2 deviation: equal suite scores, candidate B touches a file outside
// the expected scope — A must win even when B is listed first.
func TestRunCandidateSelection_Stage2DeviationDecides(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_ab_stage2"

	mk := func(task string, files map[string]string) ABSelectionInput {
		path, branch, err := wm.EnsureCandidateWorktree(commandID, task)
		if err != nil {
			t.Fatalf("EnsureCandidateWorktree(%s): %v", task, err)
		}
		for name, content := range files {
			full := filepath.Join(path, name)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := wm.CommitCandidateChanges(commandID, task); err != nil {
			t.Fatalf("CommitCandidateChanges(%s): %v", task, err)
		}
		return ABSelectionInput{TaskID: task, Branch: branch, ExpectedPaths: []string{"scope"}}
	}
	inScope := mk("task_s2_in", map[string]string{"scope/feature.txt": "x\n"})
	outScope := mk("task_s2_out", map[string]string{"scope/feature2.txt": "x\n", "stray/extra.txt": "y\n"})

	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_s2",
		[]ABSelectionInput{outScope, inScope}, []string{"true"}, nil, nil)
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if outcome.WinnerTaskID != "task_s2_in" {
		t.Errorf("winner = %s (evidence %v), want in-scope candidate", outcome.WinnerTaskID, outcome.Evidence)
	}
	if outcome.Evidence["stage2_decision"] != "expected_paths_deviation" {
		t.Errorf("stage2_decision = %q, want expected_paths_deviation", outcome.Evidence["stage2_decision"])
	}
}

// Flake guard: candidate B fails once (sticky flag outside the worktree
// makes the verifier pass afterwards) — the rerun recovers it and Stage 2
// settles the tie instead of the flaky first run deciding.
func TestRunCandidateSelection_FlakeRerunRecovers(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)
	flag := filepath.Join(t.TempDir(), "flake_seen")
	// Baseline + candidate A: marker_b.txt absent -> first branch passes.
	// Candidate B first run: flag absent -> create it, fail. Rerun: passes.
	cmd := "test ! -f marker_b.txt || test -f " + flag + " || { touch " + flag + "; exit 1; }"

	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_flake",
		[]ABSelectionInput{inputs[0], inputs[1]}, []string{cmd}, nil, nil)
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if outcome.Evidence["flake_rerun_"+inputs[1].TaskID] != "recovered" {
		t.Fatalf("flake rerun evidence = %v, want recovered", outcome.Evidence)
	}
	// Recovered -> tie -> Stage 2 (equal metrics) -> first input wins.
	if outcome.WinnerTaskID != inputs[0].TaskID ||
		outcome.Evidence["stage2_decision"] == "" {
		t.Errorf("winner = %s evidence = %v, want first input via stage2", outcome.WinnerTaskID, outcome.Evidence)
	}
}

// Flake guard: a deterministic failure stays failing after the rerun and
// the all-pass candidate wins on score.
func TestRunCandidateSelection_FlakeRerunStillFailing(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)
	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_flake2",
		[]ABSelectionInput{inputs[1], inputs[0]}, // failing candidate listed first
		[]string{"test ! -f marker_b.txt"}, nil, nil)
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if outcome.Evidence["flake_rerun_"+inputs[1].TaskID] != "still_failing" {
		t.Errorf("flake rerun evidence = %v, want still_failing", outcome.Evidence)
	}
	if outcome.WinnerTaskID != inputs[0].TaskID {
		t.Errorf("winner = %s, want the all-pass candidate", outcome.WinnerTaskID)
	}
	if outcome.Evidence["stage2_decision"] != "" {
		t.Errorf("stage2 must not run on a score win, got %q", outcome.Evidence["stage2_decision"])
	}
}

// --- PR3: cross-test matrix ---

// crossFixture: candidate A ships impl_a.txt plus a test file asserting it;
// candidate B ships only impl_b.txt (no tests). Suites pass for both; the
// cross layer must catch that B cannot satisfy A's test.
func crossFixture(t *testing.T) (wm *Manager, commandID string, withTests, noTests ABSelectionInput) {
	t.Helper()
	projectRoot := testutil.InitTestGitRepo(t)
	wm = newTestWorktreeManager(t, projectRoot)
	commandID = "cmd_ab_cross"

	mk := func(task string, files map[string]string) ABSelectionInput {
		path, branch, err := wm.EnsureCandidateWorktree(commandID, task)
		if err != nil {
			t.Fatalf("EnsureCandidateWorktree(%s): %v", task, err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := wm.CommitCandidateChanges(commandID, task); err != nil {
			t.Fatalf("CommitCandidateChanges(%s): %v", task, err)
		}
		return ABSelectionInput{TaskID: task, Branch: branch}
	}
	withTests = mk("task_cr_a", map[string]string{
		"impl_a.txt":    "a\n",
		"check.test.sh": "test -f impl_a.txt\n", // matches *.test.*
	})
	noTests = mk("task_cr_b", map[string]string{"impl_b.txt": "b\n"})
	return wm, commandID, withTests, noTests
}

// verifyCmdRunTests runs every overlaid *.test.sh (none on the baseline).
const verifyCmdRunTests = `for f in *.test.sh; do [ -e "$f" ] || continue; sh "$f" || exit 1; done`

func TestRunCandidateSelection_CrossTestDecides(t *testing.T) {
	t.Parallel()
	wm, commandID, withTests, noTests := crossFixture(t)

	// B listed FIRST: without the cross layer the tie would hand B the win.
	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_cross",
		[]ABSelectionInput{noTests, withTests}, []string{verifyCmdRunTests},
		[]string{"*.test.*"}, nil)
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if outcome.WinnerTaskID != withTests.TaskID {
		t.Errorf("winner = %s (evidence %v), want the candidate that satisfies its peer's tests", outcome.WinnerTaskID, outcome.Evidence)
	}
	// B was run against A's test and failed it.
	if got := outcome.Evidence["cross_"+noTests.TaskID]; got != "0/1" {
		t.Errorf("cross score for no-tests candidate = %q, want 0/1", got)
	}
	// A had no opponent tests: neutral.
	if got := outcome.Evidence["cross_"+withTests.TaskID]; got != "neutral_no_tests" {
		t.Errorf("cross evidence for tests candidate = %q, want neutral_no_tests", got)
	}
	if outcome.Evidence["stage2_decision"] != "" {
		t.Errorf("stage2 must not run when cross decides, got %q", outcome.Evidence["stage2_decision"])
	}

	// Overlay cleanup: the integration worktree must not retain A's test.
	intPath := wm.integrationWorktreePath(commandID)
	if _, err := os.Stat(filepath.Join(intPath, "check.test.sh")); err == nil {
		t.Error("overlaid test file must be cleaned by restore")
	}
}

func TestRunCandidateSelection_CrossSharedPathExcluded(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_ab_crossx"
	mk := func(task, content string) ABSelectionInput {
		path, branch, err := wm.EnsureCandidateWorktree(commandID, task)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "shared.test.sh"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := wm.CommitCandidateChanges(commandID, task); err != nil {
			t.Fatal(err)
		}
		return ABSelectionInput{TaskID: task, Branch: branch}
	}
	a := mk("task_cx_a", "true\n")
	b := mk("task_cx_b", "false\n")

	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_crossx",
		[]ABSelectionInput{a, b}, []string{"true"}, []string{"*.test.*"}, nil)
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if outcome.Evidence["cross_excluded"] != "1" {
		t.Errorf("cross_excluded = %q, want 1", outcome.Evidence["cross_excluded"])
	}
	// Both neutral after the exclusion -> full tie -> stage2 -> canonical first.
	if outcome.WinnerTaskID != a.TaskID || outcome.Evidence["stage2_decision"] == "" {
		t.Errorf("winner = %s evidence = %v, want first input via stage2", outcome.WinnerTaskID, outcome.Evidence)
	}
}

// --- PR4: Stage 3 cross-LLM judge ---

type stubJudge struct {
	responses map[string]string // judge model -> raw output
	calls     []string
}

func (s *stubJudge) Invoke(_ context.Context, model, _, _ string) (string, error) {
	s.calls = append(s.calls, model)
	out, ok := s.responses[model]
	if !ok {
		return "", fmt.Errorf("no stub response for %s", model)
	}
	return out, nil
}

// judgeFixture: full Stage 1 + Stage 2 tie (lines 10 vs 9 is inside the 10%
// margin; deviations/files equal) so Stage 3 decides.
func judgeFixture(t *testing.T) (wm *Manager, commandID string, inputs []ABSelectionInput) {
	t.Helper()
	projectRoot := testutil.InitTestGitRepo(t)
	wm = newTestWorktreeManager(t, projectRoot)
	commandID = "cmd_ab_judge"
	mk := func(task, content string) ABSelectionInput {
		path, branch, err := wm.EnsureCandidateWorktree(commandID, task)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, task+".txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := wm.CommitCandidateChanges(commandID, task); err != nil {
			t.Fatal(err)
		}
		return ABSelectionInput{TaskID: task, Branch: branch, TaskPurpose: "p", AcceptanceCriteria: "a"}
	}
	a := mk("task_j_a", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n")
	b := mk("task_j_b", "1\n2\n3\n4\n5\n6\n7\n8\n9\n")
	return wm, commandID, []ABSelectionInput{a, b}
}

func TestRunCandidateSelection_Stage3JudgeAgreement(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := judgeFixture(t)
	judge := &stubJudge{responses: map[string]string{
		"judge-claude": `The better one is {"winner":"B"} for clarity.`,
		"judge-codex":  `{"winner": "b"}`,
	}}
	wm.SetABJudge(judge)

	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_j1",
		inputs, []string{"true"}, nil, []string{"judge-claude", "judge-codex"})
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if outcome.WinnerTaskID != inputs[1].TaskID {
		t.Errorf("winner = %s (evidence %v), want B via judge agreement", outcome.WinnerTaskID, outcome.Evidence)
	}
	if outcome.Evidence["stage3_decision"] != "agree:B" {
		t.Errorf("stage3_decision = %q, want agree:B", outcome.Evidence["stage3_decision"])
	}
	if len(judge.calls) != 2 {
		t.Errorf("judge calls = %v, want both judges", judge.calls)
	}
}

func TestRunCandidateSelection_Stage3DisagreementFallsToMargin(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := judgeFixture(t)
	judge := &stubJudge{responses: map[string]string{
		"judge-claude": `{"winner":"A"}`,
		"judge-codex":  `{"winner":"B"}`,
	}}
	wm.SetABJudge(judge)

	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_j2",
		inputs, []string{"true"}, nil, []string{"judge-claude", "judge-codex"})
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	// B has 9 lines vs A's 10 — within the Stage 2 margin but B is the
	// margin-free "barely better" side.
	if outcome.WinnerTaskID != inputs[1].TaskID ||
		outcome.Evidence["stage3_decision"] != "disagree_margin:B" {
		t.Errorf("winner = %s decision = %q, want B via disagree_margin", outcome.WinnerTaskID, outcome.Evidence["stage3_decision"])
	}
}

func TestRunCandidateSelection_Stage3JudgeFailureFallsToCanonical(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := judgeFixture(t)
	judge := &stubJudge{responses: map[string]string{
		"judge-claude": `no json here`,
	}}
	wm.SetABJudge(judge)

	outcome, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_j3",
		inputs, []string{"true"}, nil, []string{"judge-claude", "judge-codex"})
	if err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}
	if outcome.WinnerTaskID != inputs[0].TaskID ||
		outcome.Evidence["stage3_decision"] != "judge_failed_canonical" {
		t.Errorf("winner = %s decision = %q, want canonical via judge_failed", outcome.WinnerTaskID, outcome.Evidence["stage3_decision"])
	}
}

func TestParseJudgeVote(t *testing.T) {
	t.Parallel()
	if v, err := parseJudgeVote(`prefix {"winner":"A","reason":"x"} suffix`); err != nil || v != 0 {
		t.Errorf("vote = %d err = %v, want A", v, err)
	}
	if _, err := parseJudgeVote(`{"winner":"C"}`); err == nil {
		t.Error("invalid winner must fail")
	}
	if _, err := parseJudgeVote(`plain text`); err == nil {
		t.Error("no JSON must fail")
	}
}

// --- W-G3: wm.mu released during external processes; integration lock keeps
// the worktree reserved ---

// waitForFile polls until path exists or the timeout elapses.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear within %s", path, timeout)
}

// Regression (W-G3): RunCandidateSelection must not hold wm.mu while its
// external verify commands run — every other manager operation (dispatch
// path resolution, commits, GC) would otherwise stall for the whole
// multi-minute suite.
func TestRunCandidateSelection_ReleasesManagerMutexDuringVerify(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)

	flag := filepath.Join(t.TempDir(), "verify_started")
	// The first run (Stage 0 baseline) signals then sleeps; later runs pass
	// immediately so the whole selection stays fast.
	verify := fmt.Sprintf("test -f %s || { touch %s; sleep 3; }", flag, flag)

	type result struct {
		outcome *ABSelectionOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		o, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_unlock",
			inputs, []string{verify}, nil, nil)
		done <- result{o, err}
	}()

	waitForFile(t, flag, 10*time.Second)

	// The baseline verify is sleeping (~3s) with the durable marker saved;
	// wm.mu must be free for unrelated operations.
	start := time.Now()
	if _, err := wm.GetCommandState(commandID); err != nil {
		t.Fatalf("GetCommandState during selection: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("manager mutex held during external verify: GetCommandState took %s", elapsed)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("RunCandidateSelection: %v", res.err)
	}
	if res.outcome.Degraded || res.outcome.WinnerTaskID != inputs[0].TaskID {
		t.Errorf("outcome = %+v, want first input winning a passing-verifier tie", res.outcome)
	}
}

// Regression (W-G3): while wm.mu is released for external verify runs, the
// per-command integration lock must keep same-command integration mutations
// (here: MergeToIntegration) excluded. Without it the merge would land
// mid-selection and the selection's restore (reset --hard to the
// pre-selection SHA) would wipe the merge commit off the integration branch.
func TestMergeToIntegration_ExcludedDuringSelection(t *testing.T) {
	t.Parallel()
	wm, commandID, inputs := selectionFixture(t)

	// A committed worker ready to merge.
	if err := wm.EnsureWorkerWorktree(commandID, "workerm"); err != nil {
		t.Fatal(err)
	}
	wtPath, err := wm.GetWorkerPath(commandID, "workerm")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "merge_payload.txt"), []byte("m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "workerm", "payload"); err != nil {
		t.Fatal(err)
	}

	flag := filepath.Join(t.TempDir(), "verify_started")
	verify := fmt.Sprintf("test -f %s || { touch %s; sleep 3; }", flag, flag)

	done := make(chan error, 1)
	go func() {
		_, err := wm.RunCandidateSelection(context.Background(), commandID, "abg_excl",
			inputs, []string{verify}, nil, nil)
		done <- err
	}()
	waitForFile(t, flag, 10*time.Second)

	// Issued during the selection's unlock window: must block on the
	// integration lock and apply only after the selection restored preSHA.
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, []string{"workerm"}, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
	if err := <-done; err != nil {
		t.Fatalf("RunCandidateSelection: %v", err)
	}

	// The merge commit must survive on the integration branch: the
	// selection's restore ran strictly before the merge started.
	if _, err := wm.gitOutput("show", "maestro/"+commandID+"/integration:merge_payload.txt"); err != nil {
		t.Errorf("worker merge lost — selection restore wiped a concurrent merge: %v", err)
	}
	state, err := wm.loadState(commandID)
	if err != nil {
		t.Fatal(err)
	}
	if state.ABSelection != nil {
		t.Errorf("selection marker not cleared: %+v", state.ABSelection)
	}
}

// Regression (W-G9): a verify command leaving a background child that
// inherits the output pipes must not block runSelectionCmds until that
// grandchild exits — cmd.WaitDelay bounds the pipe wait after the direct
// child terminates.
func TestRunSelectionCmds_WaitDelayBoundsPipeHoldingGrandchild(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	start := time.Now()
	wm.runSelectionCmds(context.Background(), projectRoot, []string{"sleep 30 & exit 0"})
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("runSelectionCmds blocked %s on a pipe-holding grandchild; WaitDelay must bound the wait", elapsed)
	}
}
