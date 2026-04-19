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

// internalIDPattern matches system-internal IDs with double-underscore prefix
// (e.g. "__implicit_phase"). Only lowercase letters and underscores after the prefix.
var internalIDPattern = regexp.MustCompile(`^__[a-z][a-z_]*$`)

// PhaseID validates a phase identifier. It accepts either a regular ID
// (validated by idPattern) or a system-internal ID with a "__" prefix
// (validated by internalIDPattern). This allows synthetic phase IDs like
// "__implicit_phase" used for commands without explicit phases.
func PhaseID(id string) error {
	if id == "" {
		return fmt.Errorf("validate: phase ID must not be empty")
	}
	if internalIDPattern.MatchString(id) {
		return nil
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("validate: invalid phase ID %q: must be a valid ID or an internal __-prefixed identifier", id)
	}
	return nil
}

// projectNamePattern allows tmux-safe project names: alphanumeric start,
// then alphanumeric, underscore, or hyphen. No dots or colons (which are
// special in tmux target syntax). Length 1–64.
var projectNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ID checks that an ID string (command_id, worker_id, task_id, etc.)
// contains only safe characters for use in file paths.
func ID(id string) error {
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

// ProjectName checks that a project name is safe for use as a tmux
// session name. Dots and colons are excluded because tmux uses them in target
// syntax (session:window.pane).
func ProjectName(name string) error {
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

// ContentLength checks that a text field does not exceed maxBytes in byte length.
// It uses len(value) which returns the number of bytes in the UTF-8 encoded
// Go string, not the number of Unicode characters (runes).
// Returns an error describing the field name and the limit.
func ContentLength(field, value string, maxBytes int) error {
	if len(value) > maxBytes {
		return fmt.Errorf("validate: %s exceeds maximum size of %d bytes (got %d bytes)", field, maxBytes, len(value))
	}
	return nil
}

// FilePath checks that a file path is safe: non-empty, no null bytes,
// no directory traversal (".."), and cleaned. Returns the cleaned path.
func FilePath(path string) (string, error) {
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

	// Defense in depth: for relative paths, verify via filepath.Rel that the
	// cleaned path does not escape the current directory. This catches edge
	// cases where filepath.Clean may normalise away traversal indicators on
	// certain platforms.
	if !filepath.IsAbs(cleaned) {
		rel, err := filepath.Rel(".", cleaned)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("validate: file path %q contains directory traversal", path)
		}
	}

	return cleaned, nil
}
