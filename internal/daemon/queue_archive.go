package daemon

import (
	"path/filepath"
	"strings"
)

// safeStr dereferences a string pointer, returning "" for nil.
func safeStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// workerIDFromPath extracts the worker ID from a queue file path.
// e.g., "/path/to/queue/worker1.yaml" -> "worker1"
func workerIDFromPath(path string) string {
	base := filepath.Base(path) // "worker1.yaml"
	if !strings.HasPrefix(base, "worker") || !strings.HasSuffix(base, ".yaml") {
		return ""
	}
	return strings.TrimSuffix(base, ".yaml") // "worker1"
}
