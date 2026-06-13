package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/pathutil"
)

// A/B candidate selection engine (docs/design/ab_candidate_selection.md §5).
//
// Implemented stages: Stage 0 (verifier baseline health) + the candidate
// suite (PR1), the flake guard and the Stage 2 metric tiebreak (PR2). The
// integration worktree is borrowed sequentially under wm.mu with a durable
// ABSelection marker so a daemon crash mid-selection is restored by startup
// Reconcile. Cross-tests (PR3) and the LLM judge (PR4) are later PRs.

// ErrSelectionBusy signals that the integration worktree is not idle
// (in-flight merge / publish / quarantine); the caller defers to the next
// scan.
var ErrSelectionBusy = errors.New("integration worktree busy; selection deferred")

// selectionCmdTimeout bounds a single verify command run during selection.
const selectionCmdTimeout = 10 * time.Minute

// ABSelectionInput describes one candidate to evaluate.
type ABSelectionInput struct {
	TaskID string
	Branch string
	// ExpectedPaths is the logical task's declared scope, shared by both
	// candidates (the caller applies a surviving row's paths to both).
	// Empty = unknown — the Stage 2 deviation metric then treats every
	// change as in-scope and decision falls to the later metrics.
	ExpectedPaths []string
}

// ABSelectionOutcome is the machine-decided result of a selection run.
type ABSelectionOutcome struct {
	// WinnerTaskID is empty when Degraded or SoleCandidateFailed is true.
	WinnerTaskID string
	// Degraded means selection could not pick mechanically; the caller
	// resolves the group as degraded (canonical walkover).
	Degraded bool
	// SoleCandidateFailed: single-candidate mode (walkover verification)
	// where the verifier is HEALTHY (Stage 0 passed) but the sole finisher
	// merge-conflicted or failed the suite. The caller degrades with a
	// repair re-execution instead of intaking verified-bad work.
	SoleCandidateFailed bool
	Reason              string
	// Evidence holds per-stage results for selection_evidence.
	Evidence map[string]string
}

// RunCandidateSelection evaluates candidates on the integration worktree:
//
//	Stage 0: run verifyCmds on the untouched integration HEAD (baseline).
//	         Baseline failure → verifier broken → degraded outcome (the
//	         caller falls back to the canonical candidate).
//	Stage 1: per candidate — merge --no-commit its branch, run verifyCmds
//	         (candidate suite), then overlay the OPPONENT's added/modified
//	         test files (crossPatterns matched, both-touched paths
//	         excluded) and run verifyCmds again (cross-test matrix), then
//	         abort+restore. Score is lexicographic (suite pass count,
//	         cross pass count); a merge conflict scores -1 (below any run)
//	         and neutralizes the cross layer for both sides.
//	Stage 2: deterministic metric tiebreak on a full Stage 1 tie.
//
// Remaining ties go to the FIRST input — callers pass the canonical
// candidate first, making "canonical wins ties" deterministic. The
// integration worktree is always restored to its pre-selection SHA,
// marker-guarded for crash recovery.
func (wm *Manager) RunCandidateSelection(ctx context.Context, commandID, groupID string, candidates []ABSelectionInput, verifyCmds []string, crossPatterns []string) (*ABSelectionOutcome, error) {
	if err := validateIDs(commandID); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("selection requires at least one candidate")
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return nil, fmt.Errorf("load worktree state: %w", err)
	}
	intPath := wm.integrationWorktreePath(commandID)

	// Idle gate: never interleave with merge / publish / quarantine.
	switch state.Integration.Status {
	case model.IntegrationStatusPublishing, model.IntegrationStatusQuarantined:
		return nil, ErrSelectionBusy
	}
	if _, err := os.Stat(intPath); err != nil {
		return nil, fmt.Errorf("integration worktree missing: %w", err)
	}
	if _, err := wm.gitOutputInDir(intPath, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		return nil, ErrSelectionBusy // in-flight merge owns the worktree
	}
	if state.ABSelection != nil && state.ABSelection.GroupID != groupID {
		return nil, ErrSelectionBusy // another group's stale marker; Reconcile clears it
	}

	preSHA, err := wm.gitOutput("rev-parse", state.Integration.Branch)
	if err != nil {
		return nil, fmt.Errorf("resolve integration HEAD: %w", err)
	}
	preSHA = strings.TrimSpace(preSHA)
	if err := validateSHA(preSHA); err != nil {
		return nil, fmt.Errorf("integration HEAD: %w", err)
	}

	// Durable marker BEFORE the first mutation (crash → Reconcile restore).
	now := wm.clock.Now().UTC().Format(time.RFC3339)
	state.ABSelection = &model.ABSelectionMarker{GroupID: groupID, PreSHA: preSHA, StartedAt: now}
	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		return nil, fmt.Errorf("persist selection marker: %w", err)
	}

	restore := func() {
		if err := wm.gitRunInDir(intPath, "merge", "--abort"); err == nil {
			wm.Log(core.LogLevelDebug, "ab_selection_merge_aborted command=%s", commandID)
		}
		if err := wm.gitRunInDir(intPath, "reset", "--hard", preSHA); err != nil {
			wm.Log(core.LogLevelError, "ab_selection_restore_reset_failed command=%s error=%v", commandID, err)
		}
		if err := wm.gitRunInDir(intPath, "clean", "-fd"); err != nil {
			wm.Log(core.LogLevelWarn, "ab_selection_restore_clean_failed command=%s error=%v", commandID, err)
		}
	}
	clearMarker := func() {
		st, err := wm.loadState(commandID)
		if err != nil {
			wm.Log(core.LogLevelWarn, "ab_selection_marker_clear_load_failed command=%s error=%v", commandID, err)
			return
		}
		st.ABSelection = nil
		st.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
		if err := wm.saveState(commandID, st); err != nil {
			wm.Log(core.LogLevelWarn, "ab_selection_marker_clear_save_failed command=%s error=%v", commandID, err)
		}
	}
	defer func() {
		restore()
		clearMarker()
	}()

	evidence := map[string]string{"pre_sha": preSHA}
	outcome := &ABSelectionOutcome{Evidence: evidence}

	// Stage 0: baseline health of the verifier itself.
	if len(verifyCmds) == 0 {
		evidence["stage0"] = "no_verifier"
		outcome.Degraded = true
		outcome.Reason = "no verify commands configured; no mechanical signal (PR1)"
		return outcome, nil
	}
	basePass, baseFailCmd := wm.runSelectionCmds(ctx, intPath, verifyCmds)
	if ctx.Err() != nil {
		// Shutdown mid-selection is RETRYABLE, not a verdict: command
		// failures caused by the dying context must not masquerade as a
		// broken verifier and permanently lock in a canonical walkover.
		// The durable marker + selecting status resume the run next scan.
		return nil, fmt.Errorf("selection interrupted: %w", ctx.Err())
	}
	evidence["stage0_pass"] = fmt.Sprintf("%d/%d", basePass, len(verifyCmds))
	if basePass < len(verifyCmds) {
		evidence["stage0"] = "verifier_broken"
		evidence["stage0_first_fail"] = baseFailCmd
		outcome.Degraded = true
		outcome.Reason = "verifier failed on the untouched baseline (candidates are not at fault)"
		return outcome, nil
	}
	evidence["stage0"] = "pass"

	// Cross-test matrix preparation (design §5 Stage 1): extract each
	// candidate's added/modified test files so the opponent can be run
	// against them. Paths BOTH candidates touched are excluded (no clear
	// ownership). Extraction failures degrade to a neutral cross layer.
	total := len(verifyCmds)
	overlays := make([][]string, len(candidates))      // files overlaid ONTO candidate i (opponent's tests)
	opponentBranch := make([]string, len(candidates))
	if len(candidates) == 2 && total > 0 {
		sets := make([][]string, 2)
		extractFailed := false
		for i, c := range candidates {
			files, err := wm.candidateTestFiles(state, c.TaskID, c.Branch, crossPatterns)
			if err != nil {
				wm.Log(core.LogLevelWarn, "ab_selection_cross_extract_failed command=%s task=%s error=%v",
					commandID, c.TaskID, err)
				evidence["cross_extract_"+c.TaskID] = "failed"
				extractFailed = true
			}
			sets[i] = files
		}
		if extractFailed {
			// An asymmetric extraction failure must not decide the race:
			// one side would face the opponent's tests while the other gets
			// a free pass. Neutralize the whole cross layer.
			evidence["cross"] = "neutral_extract_failed"
			sets[0], sets[1] = nil, nil
		}
		touched := func(files []string) map[string]bool {
			m := make(map[string]bool, len(files))
			for _, f := range files {
				m[f] = true
			}
			return m
		}
		in0, in1 := touched(sets[0]), touched(sets[1])
		excluded := 0
		filter := func(files []string, other map[string]bool) []string {
			var out []string
			for _, f := range files {
				if other[f] {
					excluded++
					continue
				}
				out = append(out, f)
			}
			return out
		}
		overlays[0] = filter(sets[1], in0)
		overlays[1] = filter(sets[0], in1)
		if excluded > 0 {
			// Each shared path is counted once per direction; report pairs.
			evidence["cross_excluded"] = strconv.Itoa(excluded / 2)
		}
		opponentBranch[0], opponentBranch[1] = candidates[1].Branch, candidates[0].Branch
	}

	// Stage 1: candidate suite + cross-test per candidate. Metrics for the
	// Stage 2 tiebreak are collected while the candidate is merged (no
	// re-merge needed later).
	runCandidate := func(c ABSelectionInput, oppBranch string, overlay []string) candidateRun {
		r := candidateRun{score: -1, crossScore: total} // cross neutral by default
		if err := wm.gitRunInDir(intPath, "merge", "--no-ff", "--no-commit", c.Branch); err != nil {
			wm.Log(core.LogLevelInfo, "ab_selection_candidate_merge_conflict command=%s task=%s error=%v",
				commandID, c.TaskID, err)
			evidence["suite_"+c.TaskID] = "merge_conflict"
			restore()
			return r
		}
		r.merged = true
		r.metrics = wm.collectCandidateMetrics(intPath, c.ExpectedPaths)
		pass, failCmd := wm.runSelectionCmds(ctx, intPath, verifyCmds)
		r.score = pass
		r.firstFail = failCmd
		evidence["suite_"+c.TaskID] = fmt.Sprintf("%d/%d", pass, total)
		if failCmd != "" {
			evidence["suite_"+c.TaskID+"_first_fail"] = failCmd
		}
		evidence["metrics_"+c.TaskID] = r.metrics.String()

		// Cross-test: the candidate's implementation against the OPPONENT's
		// test expectations. A compile/run failure here is signal, not
		// error — the candidate cannot satisfy its peer's tests.
		switch {
		case ctx.Err() != nil:
			// fall through to restore; the caller surfaces the retryable error
		case len(overlay) == 0:
			evidence["cross_"+c.TaskID] = "neutral_no_tests"
		default:
			applied := wm.overlayOpponentTests(intPath, oppBranch, overlay)
			evidence["cross_overlay_"+c.TaskID] = strconv.Itoa(applied)
			if applied == 0 {
				evidence["cross_"+c.TaskID] = "neutral_no_overlay"
			} else {
				crossPass, crossFail := wm.runSelectionCmds(ctx, intPath, verifyCmds)
				r.crossScore = crossPass
				evidence["cross_"+c.TaskID] = fmt.Sprintf("%d/%d", crossPass, total)
				if crossFail != "" {
					evidence["cross_"+c.TaskID+"_first_fail"] = crossFail
				}
			}
		}
		restore() // back to preSHA before the next candidate
		return r
	}

	runs := make([]candidateRun, len(candidates))
	for i, c := range candidates {
		runs[i] = runCandidate(c, opponentBranch[i], overlays[i])
		if ctx.Err() != nil {
			return nil, fmt.Errorf("selection interrupted: %w", ctx.Err()) // retryable (see Stage 0)
		}
	}

	// A candidate that cannot even integrate loses on the suite score; its
	// tests must not punish the viable candidate — neutralize the cross
	// layer for both sides.
	if len(candidates) == 2 && (!runs[0].merged || !runs[1].merged) {
		runs[0].crossScore, runs[1].crossScore = total, total
		evidence["cross"] = "neutral_conflict"
	}

	// Flake guard: both candidates merged, one all-pass and one failing —
	// re-run the failing suite ONCE before deciding. A recovered rerun
	// makes the race a tie (Stage 2 decides); instability is recorded.
	if len(candidates) == 2 && runs[0].merged && runs[1].merged && total > 0 {
		failing := -1
		switch {
		case runs[0].score == total && runs[1].score >= 0 && runs[1].score < total:
			failing = 1
		case runs[1].score == total && runs[0].score >= 0 && runs[0].score < total:
			failing = 0
		}
		if failing >= 0 {
			taskID := candidates[failing].TaskID
			evidence["flake_rerun_"+taskID+"_initial_fail"] = runs[failing].firstFail
			wm.Log(core.LogLevelInfo, "ab_selection_flake_rerun command=%s task=%s first_fail=%q",
				commandID, taskID, runs[failing].firstFail)
			rerun := runCandidate(candidates[failing], opponentBranch[failing], overlays[failing])
			if ctx.Err() != nil {
				return nil, fmt.Errorf("selection interrupted: %w", ctx.Err())
			}
			if rerun.merged && rerun.score == total {
				evidence["flake_rerun_"+taskID] = "recovered"
				runs[failing] = rerun
			} else {
				evidence["flake_rerun_"+taskID] = "still_failing"
			}
		}
	}

	// Lexicographic Stage 1 score: (candidate suite, cross-test matrix).
	bestIdx := -1
	bestScore := -2
	bestCross := -1
	for i := range runs {
		if runs[i].score > bestScore ||
			(runs[i].score == bestScore && runs[i].crossScore > bestCross) {
			bestScore = runs[i].score
			bestCross = runs[i].crossScore
			bestIdx = i
		}
	}

	if len(candidates) == 1 && bestScore < total {
		// The verifier is healthy (Stage 0 passed) and the SOLE finisher
		// demonstrably fails: intaking it would ship verified-bad work.
		outcome.SoleCandidateFailed = true
		outcome.Reason = "sole finisher failed verification against a healthy baseline"
		return outcome, nil
	}
	if bestIdx < 0 || bestScore < 0 {
		outcome.Degraded = true
		outcome.Reason = "no candidate could be evaluated (all merge-conflicted against integration)"
		return outcome, nil
	}

	// Stage 2: deterministic metric tiebreak when the full Stage 1 score
	// (suite + cross matrix) cannot discriminate. Falls back to the FIRST
	// input (canonical) when every metric ties within margin.
	if len(candidates) == 2 && runs[0].score == runs[1].score &&
		runs[0].crossScore == runs[1].crossScore {
		if len(candidates[0].ExpectedPaths) == 0 {
			evidence["metrics_expected_paths"] = "missing"
		}
		bestIdx = stage2Tiebreak(runs[0].metrics, runs[1].metrics, evidence)
	}

	outcome.WinnerTaskID = candidates[bestIdx].TaskID
	evidence["winner"] = outcome.WinnerTaskID
	evidence["winner_score"] = fmt.Sprintf("suite:%d/%d,cross:%d/%d",
		runs[bestIdx].score, total, runs[bestIdx].crossScore, total)
	return outcome, nil
}

// candidateRun captures one candidate's Stage 1 evaluation.
type candidateRun struct {
	score     int
	crossScore int
	firstFail string
	merged    bool
	metrics   candidateMetrics
}

// candidateTestFiles extracts the test files (basename-matched against
// patterns) a candidate ADDED or MODIFIED relative to its recorded base.
// Renames are not detected (no -M): they surface as A+D, and only A/M rows
// are considered.
func (wm *Manager) candidateTestFiles(state *model.WorktreeCommandState, taskID, branch string, patterns []string) ([]string, error) {
	c := findCandidate(state, taskID)
	if c == nil || c.BaseSHA == "" {
		return nil, fmt.Errorf("candidate worktree entry missing (task=%s)", taskID)
	}
	// --no-renames pins the A+D decomposition regardless of the user's
	// diff.renames git config (the matcher only consumes A/M rows).
	out, err := wm.gitOutput("diff", "--no-renames", "--name-status", "-z", c.BaseSHA+".."+branch)
	if err != nil {
		return nil, fmt.Errorf("diff --name-status: %w", err)
	}
	var files []string
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status, p := fields[i], fields[i+1]
		if (status != "A" && status != "M") || p == "" {
			continue
		}
		base := path.Base(filepath.ToSlash(p))
		for _, pat := range patterns {
			if ok, _ := filepath.Match(pat, base); ok {
				files = append(files, filepath.ToSlash(p))
				break
			}
		}
	}
	return files, nil
}

// overlayOpponentTests writes the opponent's test files (taken from its
// candidate branch) into the merged integration worktree. Best-effort: a
// file that cannot be materialized is skipped. The post-run restore
// (reset --hard + clean -fd) removes every overlay.
func (wm *Manager) overlayOpponentTests(intPath, opponentBranch string, files []string) int {
	applied := 0
	for _, f := range files {
		content, err := wm.gitOutput("show", opponentBranch+":"+f)
		if err != nil {
			wm.Log(core.LogLevelDebug, "ab_cross_overlay_show_failed branch=%s file=%s error=%v", opponentBranch, f, err)
			continue
		}
		full := filepath.Join(intPath, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil { //nolint:gosec // test files inside the managed worktree
			continue
		}
		applied++
	}
	return applied
}

// candidateMetrics is the Stage 2 input collected while the candidate was
// merged into the integration worktree.
type candidateMetrics struct {
	// Deviations counts changed files outside the task's expected_paths
	// (shared semantics with the verify runner via pathutil).
	Deviations int
	// Lines is added+deleted across text files (binary files excluded —
	// numstat reports no counts for them; they still count in Files).
	Lines int
	// Files is the changed-file count (text + binary).
	Files int
	// Binaries counts binary-diff files (line counts unknowable).
	Binaries int
}

func (m candidateMetrics) String() string {
	return fmt.Sprintf("deviations:%d,lines:%d,files:%d,binaries:%d", m.Deviations, m.Lines, m.Files, m.Binaries)
}

// collectCandidateMetrics reads `git diff --cached --numstat` (the staged
// merge result vs the pre-selection HEAD) inside the integration worktree.
// Best-effort: a git failure returns zero metrics, which Stage 2 treats as
// indistinguishable (decision falls through to the canonical-first rule).
func (wm *Manager) collectCandidateMetrics(intPath string, expectedPaths []string) candidateMetrics {
	out, err := wm.gitOutputInDir(intPath, "diff", "--cached", "--numstat")
	if err != nil {
		wm.Log(core.LogLevelWarn, "ab_selection_metrics_failed dir=%s error=%v", intPath, err)
		return candidateMetrics{}
	}
	return parseNumstat(out, expectedPaths)
}

// parseNumstat turns `git diff --numstat` output into candidateMetrics.
// Binary diffs report "-" for both counts: they count as Files (and
// Deviations when out of scope) but contribute no Lines. Renames appear as
// a single "{old => new}" row, counted once.
func parseNumstat(out string, expectedPaths []string) candidateMetrics {
	var m candidateMetrics
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		m.Files++
		if len(expectedPaths) > 0 && !pathutil.AllowedByExpectedPaths(fields[2], expectedPaths) {
			m.Deviations++
		}
		if fields[0] == "-" || fields[1] == "-" {
			m.Binaries++ // binary diff: line counts unknowable
			continue
		}
		added, aerr := strconv.Atoi(fields[0])
		deleted, derr := strconv.Atoi(fields[1])
		if aerr == nil && derr == nil {
			m.Lines += added + deleted
		}
	}
	return m
}

// stage2Tiebreak compares two same-score candidates lexicographically:
// expected_paths deviations (exact) → diff lines (10% relative margin) →
// changed files (10% margin). Within-margin differences are ties so noisy
// hair-splitting never decides a race; a full tie keeps index 0 (the
// canonical, by input order). Returns the winning index and records the
// deciding metric in evidence.
func stage2Tiebreak(a, b candidateMetrics, evidence map[string]string) int {
	withinMargin := func(x, y int) bool {
		larger, smaller := x, y
		if larger < smaller {
			larger, smaller = smaller, larger
		}
		if larger == 0 {
			return true
		}
		return float64(larger-smaller)/float64(larger) <= 0.10
	}
	pick := func(metric string, x, y int) int {
		evidence["stage2_decision"] = metric
		if y < x {
			return 1
		}
		return 0
	}
	switch {
	case a.Deviations != b.Deviations:
		return pick("expected_paths_deviation", a.Deviations, b.Deviations)
	case !withinMargin(a.Lines, b.Lines):
		return pick("diff_lines", a.Lines, b.Lines)
	case !withinMargin(a.Files, b.Files):
		return pick("files_changed", a.Files, b.Files)
	default:
		evidence["stage2_decision"] = "tie_canonical_first"
		return 0
	}
}

// runSelectionCmds executes verify commands sequentially in dir, stopping at
// nothing (all commands run so the pass count is a meaningful score).
// Returns the pass count and the first failing command ("" when all pass).
func (wm *Manager) runSelectionCmds(ctx context.Context, dir string, cmds []string) (pass int, firstFail string) {
	for _, c := range cmds {
		cctx, cancel := context.WithTimeout(ctx, selectionCmdTimeout)
		cmd := exec.CommandContext(cctx, "sh", "-c", c) //nolint:gosec // verify.yaml commands are operator/Planner-authored by design
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			if firstFail == "" {
				firstFail = c
			}
			wm.Log(core.LogLevelDebug, "ab_selection_cmd_failed dir=%s cmd=%q error=%v output_len=%d",
				dir, c, err, len(out))
			continue
		}
		pass++
	}
	return pass, firstFail
}

// IntakeWinner merges the winner's candidate branch into the worker branch
// of the worker that produced it. The worker is frozen by the dispatch gate
// for the whole race, so the merge cannot race a worker commit. A merge
// conflict (possible when the worker branch holds earlier-phase commits the
// candidate base predates) aborts and returns an error — the caller
// degrades the group (PR1; see design §6).
func (wm *Manager) IntakeWinner(commandID, workerID, candidateBranch, taskID string) error {
	if err := validateIDs(commandID, workerID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load worktree state: %w", err)
	}
	var ws *model.WorktreeState
	for i := range state.Workers {
		if state.Workers[i].WorkerID == workerID {
			ws = &state.Workers[i]
			break
		}
	}
	if ws == nil {
		// The winner's worker never created a regular worktree (candidates
		// don't trigger EnsureWorkerWorktree). Create it now so the standard
		// phase-merge pipeline has a worker branch to collect.
		baseSHA := state.Integration.BaseSHA
		if head, revErr := wm.gitOutput("rev-parse", state.Integration.Branch); revErr == nil {
			head = strings.TrimSpace(head)
			if validateSHA(head) == nil {
				baseSHA = head
			}
		}
		now := wm.clock.Now().UTC().Format(time.RFC3339)
		if err := wm.addWorkerWorktreeUnlocked(state, commandID, workerID, baseSHA, now); err != nil {
			return fmt.Errorf("create worker worktree for intake: %w", err)
		}
		state.UpdatedAt = now
		if err := wm.saveState(commandID, state); err != nil {
			return fmt.Errorf("persist worker worktree for intake: %w", err)
		}
		ws = &state.Workers[len(state.Workers)-1]
	}

	msg := fmt.Sprintf("ab-winner: %s (group intake)", taskID)
	if err := wm.gitRunInDir(ws.Path, "merge", "--no-ff", "-m", msg, candidateBranch); err != nil {
		if abortErr := wm.gitRunInDir(ws.Path, "merge", "--abort"); abortErr != nil {
			wm.Log(core.LogLevelWarn, "ab_intake_abort_failed command=%s worker=%s error=%v",
				commandID, workerID, abortErr)
		}
		return fmt.Errorf("intake merge conflicted (worker=%s): %w", workerID, err)
	}
	wm.Log(core.LogLevelInfo, "ab_winner_intake command=%s worker=%s task=%s branch=%s",
		commandID, workerID, taskID, candidateBranch)
	return nil
}

// reconcileABSelectionMarker restores the integration worktree from a stale
// in-flight selection marker (daemon crashed mid-selection). Called from
// startup Reconcile with wm.mu held.
func (wm *Manager) reconcileABSelectionMarkerUnlocked(commandID string, state *model.WorktreeCommandState) {
	m := state.ABSelection
	if m == nil {
		return
	}
	intPath := wm.integrationWorktreePath(commandID)
	wm.Log(core.LogLevelWarn,
		"ab_selection_marker_recovery command=%s group=%s pre_sha=%s (daemon crashed mid-selection; restoring integration)",
		commandID, m.GroupID, m.PreSHA)
	if _, err := os.Stat(intPath); err == nil {
		_ = wm.gitRunInDir(intPath, "merge", "--abort")
		if err := wm.gitRunInDir(intPath, "reset", "--hard", m.PreSHA); err != nil {
			wm.Log(core.LogLevelError, "ab_selection_recovery_reset_failed command=%s error=%v", commandID, err)
			return // keep the marker so the next startup retries
		}
		if err := wm.gitRunInDir(intPath, "clean", "-fd"); err != nil {
			wm.Log(core.LogLevelWarn, "ab_selection_recovery_clean_failed command=%s error=%v", commandID, err)
		}
	}
	state.ABSelection = nil
	state.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.saveState(commandID, state); err != nil {
		wm.Log(core.LogLevelWarn, "ab_selection_recovery_save_failed command=%s error=%v", commandID, err)
	}
}
