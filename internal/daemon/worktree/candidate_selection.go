package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// A/B candidate selection engine (docs/design/ab_candidate_selection.md §5).
//
// PR1 scope: Stage 0 (verifier baseline health) + the candidate suite. The
// integration worktree is borrowed sequentially under wm.mu with a durable
// ABSelection marker so a daemon crash mid-selection is restored by startup
// Reconcile. Cross-tests, metric tie-breaks and the LLM judge are later PRs.

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
//	Stage 1: per candidate — merge --no-commit its branch, run verifyCmds,
//	         abort+restore. Score = number of passing commands; a merge
//	         conflict against integration scores -1 (below any run).
//
// Ties (equal scores, including the no-verifier case) go to the FIRST input
// — callers pass the canonical candidate first, making "canonical wins ties"
// deterministic. The integration worktree is always restored to its
// pre-selection SHA, marker-guarded for crash recovery.
func (wm *Manager) RunCandidateSelection(ctx context.Context, commandID, groupID string, candidates []ABSelectionInput, verifyCmds []string) (*ABSelectionOutcome, error) {
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

	// Stage 1: candidate suite per candidate.
	bestIdx := -1
	bestScore := -2
	for i, c := range candidates {
		score := -1 // merge conflict / setup failure sentinel
		if err := wm.gitRunInDir(intPath, "merge", "--no-ff", "--no-commit", c.Branch); err != nil {
			wm.Log(core.LogLevelInfo, "ab_selection_candidate_merge_conflict command=%s task=%s error=%v",
				commandID, c.TaskID, err)
			evidence["suite_"+c.TaskID] = "merge_conflict"
		} else {
			pass, failCmd := wm.runSelectionCmds(ctx, intPath, verifyCmds)
			score = pass
			evidence["suite_"+c.TaskID] = fmt.Sprintf("%d/%d", pass, len(verifyCmds))
			if failCmd != "" {
				evidence["suite_"+c.TaskID+"_first_fail"] = failCmd
			}
		}
		restore() // back to preSHA before the next candidate
		if ctx.Err() != nil {
			return nil, fmt.Errorf("selection interrupted: %w", ctx.Err()) // retryable (see Stage 0)
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}

	if len(candidates) == 1 && bestScore < len(verifyCmds) {
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
	outcome.WinnerTaskID = candidates[bestIdx].TaskID
	evidence["winner"] = outcome.WinnerTaskID
	evidence["winner_score"] = fmt.Sprintf("%d/%d", bestScore, len(verifyCmds))
	return outcome, nil
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
