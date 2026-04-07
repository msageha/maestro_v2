package worktree

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ensureWithinProjectRoot returns an error if target does not resolve to a
// path strictly inside root (or equal to root). Both root and target are
// resolved through filepath.EvalSymlinks so that symlink-based escapes are
// detected. Used as a tripwire before destructive filesystem operations
// (e.g. `git clean -fd`, `git checkout -- .`, `git reset --hard`) to ensure
// those operations cannot affect files outside the project tree.
func ensureWithinProjectRoot(root, target string) error {
	if root == "" {
		return fmt.Errorf("project root is empty")
	}
	if target == "" {
		return fmt.Errorf("target path is empty")
	}

	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve project root %q: %w", root, err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", target, err)
	}

	absRoot, err := filepath.Abs(resolvedRoot)
	if err != nil {
		return fmt.Errorf("abs project root: %w", err)
	}
	absTarget, err := filepath.Abs(resolvedTarget)
	if err != nil {
		return fmt.Errorf("abs target: %w", err)
	}

	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return fmt.Errorf("path %q outside project root %q: %w", target, root, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes project root %q (rel=%s)", target, root, rel)
	}
	return nil
}
