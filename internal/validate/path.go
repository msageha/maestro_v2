package validate

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// idPattern allows IDs that start and end with alphanumeric characters,
// with alphanumeric, dot, underscore, or hyphen in between. Length 1–128.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,126}[A-Za-z0-9])?$`)

// projectNamePattern allows tmux-safe project names: alphanumeric start,
// then alphanumeric, underscore, or hyphen. No dots or colons (which are
// special in tmux target syntax). Length 1–64.
var projectNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidateID checks that an ID string (command_id, worker_id, task_id, etc.)
// contains only safe characters for use in file paths.
func ValidateID(id string) error {
	if id == "" {
		return fmt.Errorf("validate: ID must not be empty")
	}
	if strings.ContainsRune(id, 0) {
		return fmt.Errorf("validate: ID contains null byte")
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("validate: invalid ID %q: must contain only alphanumeric, dot, underscore, or hyphen, and start/end with alphanumeric", id)
	}
	return nil
}

// ValidateProjectName checks that a project name is safe for use as a tmux
// session name. Dots and colons are excluded because tmux uses them in target
// syntax (session:window.pane).
func ValidateProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("validate: project name must not be empty")
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("validate: project name contains null byte")
	}
	if !projectNamePattern.MatchString(name) {
		return fmt.Errorf("validate: invalid project name %q: must contain only alphanumeric, underscore, or hyphen, start with alphanumeric, max 64 chars", name)
	}
	return nil
}

// SafePath validates that joining base and elem produces a path that stays
// within the base directory. It performs lexical checks first, then attempts
// to resolve symlinks to detect symlink-based escapes. If symlink resolution
// fails (e.g., the target does not exist yet), the lexically-checked path is
// returned. For strongest protection, combine with os.OpenRoot on Go 1.24+.
func SafePath(base, elem string) (string, error) {
	if elem == "" {
		return "", fmt.Errorf("validate: path element must not be empty")
	}
	if strings.ContainsRune(elem, 0) {
		return "", fmt.Errorf("validate: path element contains null byte")
	}
	if !filepath.IsLocal(elem) {
		return "", fmt.Errorf("validate: path element %q is not a local path", elem)
	}

	cleaned := filepath.Clean(elem)
	joined := filepath.Join(base, cleaned)

	// Double-check the result is under base (lexical)
	rel, err := filepath.Rel(base, joined)
	if err != nil {
		return "", fmt.Errorf("validate: cannot compute relative path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("validate: path %q escapes base directory", elem)
	}

	// Resolve symlinks to detect symlink-based escapes.
	// Resolve base first so the containment check uses real paths.
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("validate: cannot resolve base directory: %w", err)
	}

	// Try to resolve the full path. If it fails (e.g. leaf doesn't exist),
	// resolve the deepest existing ancestor to catch symlink escapes in
	// intermediate path components.
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		// Walk up from joined to find the deepest existing ancestor and resolve it.
		ancestor := joined
		for {
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				return "", fmt.Errorf("validate: cannot resolve any ancestor of path %q", elem)
			}
			ancestor = parent
			resolvedAncestor, evalErr := filepath.EvalSymlinks(ancestor)
			if evalErr != nil {
				continue
			}
			// Check that this ancestor is still under the resolved base.
			ancRel, relErr := filepath.Rel(resolvedBase, resolvedAncestor)
			if relErr != nil {
				return "", fmt.Errorf("validate: cannot compute relative path after symlink resolution: %w", relErr)
			}
			if ancRel == ".." || strings.HasPrefix(ancRel, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("validate: path %q escapes base directory via symlink", elem)
			}
			// Deepest existing ancestor is within base; the lexical path is acceptable.
			return joined, nil
		}
	}

	// Re-verify the resolved path is under the resolved base.
	rel, err = filepath.Rel(resolvedBase, resolved)
	if err != nil {
		return "", fmt.Errorf("validate: cannot compute relative path after symlink resolution: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("validate: path %q escapes base directory via symlink", elem)
	}

	return resolved, nil
}

// ValidateFilePath checks that a file path is safe: non-empty, no null bytes,
// no directory traversal (".."), and cleaned. Returns the cleaned path.
func ValidateFilePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("validate: file path must not be empty")
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("validate: file path contains null byte")
	}
	cleaned := filepath.Clean(path)

	// Reject paths containing ".." components to prevent directory traversal.
	for _, part := range strings.Split(cleaned, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("validate: file path %q contains directory traversal", path)
		}
	}
	return cleaned, nil
}
