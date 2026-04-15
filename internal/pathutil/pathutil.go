// Package pathutil provides path normalization and containment utilities
// shared across the daemon and scheduler packages.
package pathutil

import (
	"path"
	"strings"
)

// NormalizePath strips trailing slashes and cleans the path.
// Returns "" for empty or root-equivalent paths.
// Uses path.Clean (not filepath.Clean) because queue paths are always
// forward-slash-separated regardless of OS.
func NormalizePath(p string) string {
	cleaned := path.Clean(strings.TrimSuffix(p, "/"))
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// IsDescendant reports whether child is a descendant of dir at a "/" boundary.
func IsDescendant(child, dir string) bool {
	return strings.HasPrefix(child, dir+"/")
}
