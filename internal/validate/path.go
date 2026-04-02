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
