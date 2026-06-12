package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// A/B candidate-exclusive worktrees (docs/design/ab_candidate_selection.md §3).
//
// Candidates never touch worker branches: each candidate runs in its own
// task-scoped worktree on its own branch cut from the integration HEAD. The
// selection pipeline later merges only the winner's branch into the
// canonical worker's branch, so loser isolation is structural — there is
// nothing to revert.

// maxCandidateDiffBytes caps ComputeCandidateDiff output so a runaway
// candidate (vendored deps, generated assets) cannot blow up selection
// evidence or the LLM judge prompt.
const maxCandidateDiffBytes = 512 * 1024

// candidateWorktreePath returns the conventional path for a candidate
// worktree. Lives beside worker worktrees under the command directory so
// command-level cleanup naturally sweeps it.
func (wm *Manager) candidateWorktreePath(commandID, taskID string) string {
	return filepath.Join(wm.projectRoot, wm.config.EffectivePathPrefix(), commandID, "candidate-"+taskID)
}

// candidateBranch returns the conventional branch name for a candidate.
// Delegates to model.ABCandidateBranch — the plan-side fan-out records the
// same name in the CandidateGroup, so the format must have a single owner.
func candidateBranch(commandID, taskID string) string {
	return model.ABCandidateBranch(commandID, taskID)
}

// findCandidate returns the candidate entry for taskID, or nil.
func findCandidate(state *model.WorktreeCommandState, taskID string) *model.CandidateWorktree {
	for i := range state.Candidates {
		if state.Candidates[i].TaskID == taskID {
			return &state.Candidates[i]
		}
	}
	return nil
}

// EnsureCandidateWorktree lazily creates the candidate-exclusive worktree +
// branch for an A/B candidate task. Idempotent: an existing entry returns
// its recorded path/branch. When the command has no worktree state yet
// (both candidates may dispatch before any regular worker task), the
// integration branch/worktree is created first via the shared
// createIntegrationStateUnlocked sequence.
func (wm *Manager) EnsureCandidateWorktree(commandID, taskID string) (path, branch string, err error) {
	if err := validateIDs(commandID, taskID); err != nil {
		return "", "", err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	now := wm.clock.Now().UTC().Format(time.RFC3339)

	state, err := wm.loadState(commandID)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("load worktree state: %w", err)
		}
		// No state yet — create integration first (shared critical sequence).
		newState, rollbackIntegration, cErr := wm.createIntegrationStateUnlocked(commandID, now)
		if cErr != nil {
			return "", "", cErr
		}
		if err := wm.addCandidateWorktreeUnlocked(newState, commandID, taskID, newState.Integration.BaseSHA, now); err != nil {
			if rbErr := rollbackIntegration(); rbErr != nil {
				return "", "", errors.Join(err, fmt.Errorf("rollback also failed: %w", rbErr))
			}
			return "", "", err
		}
		if err := wm.saveState(commandID, newState); err != nil {
			var rollbackErrs []error
			if rbErr := wm.removeCandidateArtifactsUnlocked(commandID, taskID); rbErr != nil {
				rollbackErrs = append(rollbackErrs, rbErr)
			}
			if rbErr := rollbackIntegration(); rbErr != nil {
				rollbackErrs = append(rollbackErrs, rbErr)
			}
			statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
			if rbErr := os.Remove(statePath); rbErr != nil && !os.IsNotExist(rbErr) {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("remove state file: %w", rbErr))
			}
			origErr := fmt.Errorf("save worktree state: %w", err)
			if len(rollbackErrs) > 0 {
				return "", "", errors.Join(origErr, fmt.Errorf("rollback also failed: %w", errors.Join(rollbackErrs...)))
			}
			return "", "", origErr
		}
		c := findCandidate(newState, taskID)
		return c.Path, c.Branch, nil
	}

	// State exists — idempotency check.
	if c := findCandidate(state, taskID); c != nil {
		return c.Path, c.Branch, nil
	}

	origCandidates := make([]model.CandidateWorktree, len(state.Candidates))
	copy(origCandidates, state.Candidates)
	origUpdatedAt := state.UpdatedAt

	// Candidates branch off the CURRENT integration HEAD so they see all
	// previously merged phase work (same rule as late-phase worker adds).
	baseSHA := state.Integration.BaseSHA
	if head, revErr := wm.gitOutput("rev-parse", state.Integration.Branch); revErr == nil {
		head = strings.TrimSpace(head)
		if validateSHA(head) == nil {
			baseSHA = head
		}
	}
	if err := validateSHA(baseSHA); err != nil {
		return "", "", fmt.Errorf("base SHA for %s: %w", commandID, err)
	}

	if err := wm.addCandidateWorktreeUnlocked(state, commandID, taskID, baseSHA, now); err != nil {
		return "", "", err
	}

	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		var rollbackErrs []error
		if rbErr := wm.removeCandidateArtifactsUnlocked(commandID, taskID); rbErr != nil {
			rollbackErrs = append(rollbackErrs, rbErr)
		}
		state.Candidates = origCandidates
		state.UpdatedAt = origUpdatedAt
		if rbErr := wm.saveState(commandID, state); rbErr != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("restore state: %w", rbErr))
		}
		origErr := fmt.Errorf("save worktree state: %w", err)
		if len(rollbackErrs) > 0 {
			return "", "", errors.Join(origErr, fmt.Errorf("rollback also failed: %w", errors.Join(rollbackErrs...)))
		}
		return "", "", origErr
	}

	c := findCandidate(state, taskID)
	return c.Path, c.Branch, nil
}

// addCandidateWorktreeUnlocked creates the candidate branch + worktree and
// appends the entry to state. Caller MUST hold wm.mu and persist state.
func (wm *Manager) addCandidateWorktreeUnlocked(state *model.WorktreeCommandState, commandID, taskID, baseSHA, now string) error {
	branch := candidateBranch(commandID, taskID)
	wtPath := wm.candidateWorktreePath(commandID, taskID)

	if err := os.MkdirAll(filepath.Dir(wtPath), 0750); err != nil {
		return fmt.Errorf("create candidate worktree parent dir: %w", err)
	}

	if err := wm.gitWorktreeAddWithUnstattableFallback(commandID,
		[]string{"worktree", "add", "-b", branch, wtPath, baseSHA}); err != nil {
		if rmErr := os.RemoveAll(wtPath); rmErr != nil {
			wm.Log(core.LogLevelWarn,
				"candidate_worktree_partial_cleanup_failed command=%s task=%s path=%s error=%v",
				commandID, taskID, wtPath, rmErr)
		}
		_ = wm.gitRun("worktree", "prune", "-v")
		_ = wm.gitRun("branch", "-D", branch)
		return fmt.Errorf("create candidate worktree for %s: %w", taskID, err)
	}

	state.Candidates = append(state.Candidates, model.CandidateWorktree{
		TaskID:    taskID,
		Path:      wtPath,
		Branch:    branch,
		BaseSHA:   baseSHA,
		CreatedAt: now,
		UpdatedAt: now,
	})

	wm.Log(core.LogLevelInfo, "candidate_worktree_created command=%s task=%s branch=%s", commandID, taskID, branch)
	return nil
}

// CommitCandidateChanges commits all changes in the candidate worktree to
// the candidate branch. A clean worktree (or only-gitignored residue) is a
// successful no-op. Unlike CommitWorkerChanges this carries no worker
// status-machine transitions — candidate lifecycle lives in the
// CandidateGroup (command state), not the worktree state.
func (wm *Manager) CommitCandidateChanges(commandID, taskID string) error {
	if err := validateIDs(commandID, taskID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return fmt.Errorf("load worktree state: %w", err)
	}
	c := findCandidate(state, taskID)
	if c == nil {
		return fmt.Errorf("candidate worktree not found (command=%s, task=%s)", commandID, taskID)
	}

	statusOut, err := wm.gitOutputInDir(c.Path, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status (candidate=%s): %w", taskID, err)
	}
	if strings.TrimSpace(statusOut) == "" {
		wm.Log(core.LogLevelDebug, "candidate_commit_skip_clean command=%s task=%s", commandID, taskID)
		return nil
	}

	// Same unstattable-file fallback discipline as worker commits: a
	// sandbox-denied file must not wedge the candidate pipeline.
	if err := wm.gitAddAllWithUnstattableFallback(c.Path, commandID, "candidate-"+taskID); err != nil {
		return fmt.Errorf("git add -A (candidate=%s, command=%s): %w", taskID, commandID, err)
	}

	stagedOut, err := wm.gitOutputInDir(c.Path, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return fmt.Errorf("git diff --cached (candidate=%s): %w", taskID, err)
	}
	if strings.TrimRight(stagedOut, "\x00") == "" {
		wm.Log(core.LogLevelDebug, "candidate_commit_skip_only_ignored command=%s task=%s", commandID, taskID)
		return nil
	}

	msg := fmt.Sprintf("ab-candidate: %s (command %s)", taskID, commandID)
	if err := wm.gitRunInDir(c.Path, "commit", "-m", msg); err != nil {
		return fmt.Errorf("git commit (candidate=%s, command=%s): %w", taskID, commandID, err)
	}

	now := wm.clock.Now().UTC().Format(time.RFC3339)
	c.UpdatedAt = now
	state.UpdatedAt = now
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	wm.Log(core.LogLevelInfo, "candidate_committed command=%s task=%s", commandID, taskID)
	return nil
}

// ComputeCandidateDiff returns the changed file list and a size-capped
// unified diff of the candidate branch against its recorded base. Selection
// evidence + (later PRs) cross-test extraction and the LLM judge consume it.
func (wm *Manager) ComputeCandidateDiff(commandID, taskID string) (changedFiles []string, diff string, err error) {
	if err := validateIDs(commandID, taskID); err != nil {
		return nil, "", err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		return nil, "", fmt.Errorf("load worktree state: %w", err)
	}
	c := findCandidate(state, taskID)
	if c == nil {
		return nil, "", fmt.Errorf("candidate worktree not found (command=%s, task=%s)", commandID, taskID)
	}

	namesOut, err := wm.gitOutput("diff", "--name-only", c.BaseSHA+".."+c.Branch)
	if err != nil {
		return nil, "", fmt.Errorf("git diff --name-only (candidate=%s): %w", taskID, err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(namesOut), "\n") {
		if line != "" {
			changedFiles = append(changedFiles, line)
		}
	}

	diffOut, err := wm.gitOutput("diff", c.BaseSHA+".."+c.Branch)
	if err != nil {
		return nil, "", fmt.Errorf("git diff (candidate=%s): %w", taskID, err)
	}
	if len(diffOut) > maxCandidateDiffBytes {
		diffOut = diffOut[:maxCandidateDiffBytes] + "\n... [diff truncated]"
	}
	return changedFiles, diffOut, nil
}

// RemoveCandidateWorktree deletes a candidate's worktree + branch and drops
// its state entry. Used for loser cleanup and group degradation. Idempotent:
// a missing entry is a successful no-op; partial git failures are returned
// so the caller (selection resolver / GC) can retry on the next scan.
func (wm *Manager) RemoveCandidateWorktree(commandID, taskID string) error {
	if err := validateIDs(commandID, taskID); err != nil {
		return err
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()

	state, err := wm.loadState(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // command already cleaned up
		}
		return fmt.Errorf("load worktree state: %w", err)
	}
	if findCandidate(state, taskID) == nil {
		return nil // already removed
	}

	if err := wm.removeCandidateArtifactsUnlocked(commandID, taskID); err != nil {
		return err
	}

	kept := state.Candidates[:0]
	for _, c := range state.Candidates {
		if c.TaskID != taskID {
			kept = append(kept, c)
		}
	}
	state.Candidates = kept
	state.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.saveState(commandID, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	wm.Log(core.LogLevelInfo, "candidate_worktree_removed command=%s task=%s", commandID, taskID)
	return nil
}

// removeCandidateArtifactsUnlocked removes the candidate worktree and branch
// from git. Caller MUST hold wm.mu. "already gone" outcomes are tolerated.
func (wm *Manager) removeCandidateArtifactsUnlocked(commandID, taskID string) error {
	wtPath := wm.candidateWorktreePath(commandID, taskID)
	branch := candidateBranch(commandID, taskID)

	var errs []error
	if err := ensureWithinProjectRoot(wm.projectRoot, wtPath); err != nil {
		return fmt.Errorf("path guard candidate worktree: %w", err)
	}
	if err := wm.gitRun("worktree", "remove", "--force", wtPath); err != nil {
		if !strings.Contains(err.Error(), "not a working tree") &&
			!strings.Contains(err.Error(), "No such file") {
			errs = append(errs, fmt.Errorf("remove candidate worktree: %w", err))
		}
	}
	if err := wm.gitRun("branch", "-D", branch); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			errs = append(errs, fmt.Errorf("delete candidate branch %s: %w", branch, err))
		}
	}
	return errors.Join(errs...)
}
