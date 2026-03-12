package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
)

// gitTimeout returns the configured git command timeout as a time.Duration.
func (wm *Manager) gitTimeout() time.Duration {
	return time.Duration(wm.config.EffectiveGitTimeout()) * time.Second
}

// gitExec is the shared git execution helper. All git operations go through
// this method to ensure consistent timeout and error handling.
// dir specifies the working directory; if empty, projectRoot is used.
// Returns (stdout, combinedOutput, error). Callers that need only the exit
// status use gitRun/gitRunInDir; callers that need stdout use gitOutput/gitOutputInDir.
func (wm *Manager) gitExecCombined(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wm.gitTimeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Dir = wm.projectRoot
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			dirLabel := wm.projectRoot
			if dir != "" {
				dirLabel = dir
			}
			return output, fmt.Errorf("git %s (in %s): timeout after %s: %w",
				strings.Join(args, " "), dirLabel, wm.gitTimeout(), ctx.Err())
		}
		return output, err
	}
	return output, nil
}

func (wm *Manager) gitExecOutput(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wm.gitTimeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Dir = wm.projectRoot
	}
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			dirLabel := wm.projectRoot
			if dir != "" {
				dirLabel = dir
			}
			return nil, fmt.Errorf("git %s (in %s): timeout after %s: %w",
				strings.Join(args, " "), dirLabel, wm.gitTimeout(), ctx.Err())
		}
		return nil, err
	}
	return output, nil
}

// gitRun executes a git command in the project root.
func (wm *Manager) gitRun(args ...string) error {
	output, err := wm.gitExecCombined("", args...)
	if err != nil {
		return fmt.Errorf("git %s: %w\noutput: %s", strings.Join(args, " "), err, string(output))
	}
	return nil
}

// gitOutput executes a git command and returns stdout.
func (wm *Manager) gitOutput(args ...string) (string, error) {
	output, err := wm.gitExecOutput("", args...)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(output), nil
}

// gitRunInDir executes a git command in a specific directory.
func (wm *Manager) gitRunInDir(dir string, args ...string) error {
	output, err := wm.gitExecCombined(dir, args...)
	if err != nil {
		return fmt.Errorf("git -C %s %s: %w\noutput: %s", dir, strings.Join(args, " "), err, string(output))
	}
	return nil
}

// gitOutputInDir executes a git command in a specific directory and returns stdout.
func (wm *Manager) gitOutputInDir(dir string, args ...string) (string, error) {
	output, err := wm.gitExecOutput(dir, args...)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git -C %s %s: %w\nstderr: %s", dir, strings.Join(args, " "), err, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git -C %s %s: %w", dir, strings.Join(args, " "), err)
	}
	return string(output), nil
}

// listGitWorktreesUnlocked returns paths of all git worktrees via `git worktree list --porcelain`.
// Caller must hold wm.mu.
func (wm *Manager) listGitWorktreesUnlocked() ([]string, error) {
	output, err := wm.gitOutput("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeListPorcelain(output), nil
}

// parseWorktreeListPorcelain extracts worktree paths from `git worktree list --porcelain` output.
func parseWorktreeListPorcelain(output string) []string {
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			paths = append(paths, strings.TrimPrefix(line, "worktree "))
		}
	}
	return paths
}

// sensitiveFilePatterns lists file name patterns that should never be staged
// automatically, even if they are not covered by .gitignore.
var sensitiveFilePatterns = []string{
	".env",
	".env.*",
	"*.key",
	"*.pem",
	"*.secret",
	"*.p12",
	"*.pfx",
	"credentials.*",
}

// isSensitiveFile returns true if the filename matches a sensitive pattern
// that should not be staged automatically.
func isSensitiveFile(name string) bool {
	base := filepath.Base(name)
	for _, pattern := range sensitiveFilePatterns {
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}
	return false
}

// stageNewFiles stages untracked files that pass both .gitignore and the
// sensitive-file safety filter. Files matching sensitive patterns are logged
// but not staged. Uses NUL-separated output for safe filename handling.
func (wm *Manager) stageNewFiles(dir string) error {
	// List untracked files respecting .gitignore (NUL-separated for safety)
	output, err := wm.gitOutputInDir(dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return fmt.Errorf("list untracked files: %w", err)
	}

	var toStage []string
	for _, name := range strings.Split(output, "\x00") {
		if name == "" {
			continue
		}
		if isSensitiveFile(name) {
			wm.log(core.LogLevelWarn, "skip_sensitive_file path=%s dir=%s", name, dir)
			continue
		}
		toStage = append(toStage, name)
	}

	if len(toStage) == 0 {
		return nil
	}

	args := append([]string{"add", "--"}, toStage...)
	if err := wm.gitRunInDir(dir, args...); err != nil {
		return fmt.Errorf("git add new files: %w", err)
	}
	return nil
}

// unstageSensitiveFiles checks the staged file list and unstages any files
// matching sensitive patterns. This prevents accidentally committing sensitive
// tracked files that were staged by git add -u.
func (wm *Manager) unstageSensitiveFiles(dir string) error {
	output, err := wm.gitOutputInDir(dir, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return fmt.Errorf("list staged files: %w", err)
	}

	var toUnstage []string
	for _, name := range strings.Split(output, "\x00") {
		if name == "" {
			continue
		}
		if isSensitiveFile(name) {
			wm.log(core.LogLevelWarn, "unstage_sensitive_tracked_file path=%s dir=%s", name, dir)
			toUnstage = append(toUnstage, name)
		}
	}

	if len(toUnstage) == 0 {
		return nil
	}

	args := append([]string{"reset", "HEAD", "--"}, toUnstage...)
	if err := wm.gitRunInDir(dir, args...); err != nil {
		return fmt.Errorf("unstage sensitive files: %w", err)
	}
	return nil
}

// CommitPolicyViolation represents a single commit policy check failure.
type CommitPolicyViolation struct {
	Code    string   // machine-readable code (e.g. "max_files_exceeded")
	Message string   // human-readable description
	Files   []string // affected files (if applicable)
}

// checkCommitPolicy validates the staged changes and commit message against the
// configured CommitPolicy. Returns an empty slice if all checks pass.
// stagedNul is the NUL-separated output from `git diff --cached --name-only -z`.
func (wm *Manager) checkCommitPolicy(worktreePath, message, stagedNul string) []CommitPolicyViolation {
	policy := wm.config.CommitPolicy
	var violations []CommitPolicyViolation

	// Parse staged file list
	var stagedFiles []string
	for _, name := range strings.Split(stagedNul, "\x00") {
		if name != "" {
			stagedFiles = append(stagedFiles, name)
		}
	}

	// Check 1: Maximum files per commit (MaxFiles=0 means unlimited)
	maxFiles := policy.EffectiveMaxFiles()
	if maxFiles > 0 && len(stagedFiles) > maxFiles {
		violations = append(violations, CommitPolicyViolation{
			Code:    "max_files_exceeded",
			Message: fmt.Sprintf("staged file count %d exceeds limit %d", len(stagedFiles), maxFiles),
			Files:   stagedFiles,
		})
	}

	// Check 2: .gitignore existence
	if policy.RequireGitignore {
		gitignorePath := filepath.Join(worktreePath, ".gitignore")
		if _, err := os.Stat(gitignorePath); err != nil {
			if os.IsNotExist(err) {
				violations = append(violations, CommitPolicyViolation{
					Code:    "missing_gitignore",
					Message: ".gitignore file not found in worktree root",
				})
			} else {
				violations = append(violations, CommitPolicyViolation{
					Code:    "gitignore_check_error",
					Message: fmt.Sprintf("failed to check .gitignore: %v", err),
				})
			}
		}
	}

	// Check 3: Commit message format
	pattern := policy.MessagePattern
	if pattern != "" {
		re, err := regexp.Compile(pattern)
		if err != nil {
			violations = append(violations, CommitPolicyViolation{
				Code:    "invalid_message_pattern",
				Message: fmt.Sprintf("commit message pattern %q is invalid: %v", pattern, err),
			})
		} else if !re.MatchString(message) {
			violations = append(violations, CommitPolicyViolation{
				Code:    "message_format_invalid",
				Message: fmt.Sprintf("commit message does not match required pattern %q", pattern),
			})
		}
	}

	return violations
}

// hasUnmergedFiles checks if a directory has unmerged index entries (indicating a true merge conflict).
// Uses `git ls-files -u` which is more robust than checking exit codes for automation.
// Returns (hasConflict, error) — callers must handle probe errors separately.
func (wm *Manager) hasUnmergedFiles(dir string) (bool, error) {
	output, err := wm.gitOutputInDir(dir, "ls-files", "-u")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

// getConflictFilesInDir returns the list of conflicting files in a directory.
func (wm *Manager) getConflictFilesInDir(dir string) ([]string, error) {
	output, err := wm.gitOutputInDir(dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// recoverWorktreeAfterMerge attempts to recover a worktree when merge --abort fails.
// It performs git reset --hard to preMergeHEAD, then git clean -fd to remove untracked files,
// and verifies the worktree is clean via git status --porcelain.
// Returns nil only if the worktree is confirmed clean after recovery.
func (wm *Manager) recoverWorktreeAfterMerge(worktreePath, preMergeHEAD, commandID, workerID string) error {
	// Step 1: reset --hard is mandatory — it restores tracked content, index, and HEAD.
	if resetErr := wm.gitRunInDir(worktreePath, "reset", "--hard", preMergeHEAD); resetErr != nil {
		wm.log(core.LogLevelError, "merge_abort_recovery_reset_failed command=%s worker=%s error=%v",
			commandID, workerID, resetErr)
		return fmt.Errorf("worktree recovery failed: reset --hard: %w", resetErr)
	}
	wm.log(core.LogLevelInfo, "merge_abort_recovery_reset_ok command=%s worker=%s head=%s",
		commandID, workerID, preMergeHEAD)

	// Step 2: clean -fd to remove untracked files left by the failed merge.
	if cleanErr := wm.gitRunInDir(worktreePath, "clean", "-fd"); cleanErr != nil {
		wm.log(core.LogLevelWarn, "merge_abort_recovery_clean_failed command=%s worker=%s error=%v",
			commandID, workerID, cleanErr)
		// Not immediately fatal — check status below to confirm.
	}

	// Step 3: verify the worktree is clean.
	statusOut, statusErr := wm.gitOutputInDir(worktreePath, "status", "--porcelain")
	if statusErr != nil {
		wm.log(core.LogLevelError, "merge_abort_recovery_verify_failed command=%s worker=%s error=%v",
			commandID, workerID, statusErr)
		return fmt.Errorf("worktree recovery verification failed: %w", statusErr)
	}
	if strings.TrimSpace(statusOut) != "" {
		wm.log(core.LogLevelError, "merge_abort_recovery_still_dirty command=%s worker=%s status=%q",
			commandID, workerID, strings.TrimSpace(statusOut))
		return fmt.Errorf("worktree still dirty after recovery: %s", strings.TrimSpace(statusOut))
	}

	wm.log(core.LogLevelInfo, "merge_abort_recovery_success command=%s worker=%s", commandID, workerID)
	return nil
}
