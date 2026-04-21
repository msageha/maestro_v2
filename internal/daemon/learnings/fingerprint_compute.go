package learnings

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Error message normalization: strip volatile fragments so that semantically
// identical errors produce the same fingerprint regardless of
// timestamps, PIDs, tmpfile paths, hex addresses, line numbers, etc.
var (
	reHex       = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	reDigits    = regexp.MustCompile(`\b\d+\b`)
	reTmpPath   = regexp.MustCompile(`/(?:tmp|private/tmp|var/folders)/[^\s:]+`)
	reRFC3339   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	reWhitespace = regexp.MustCompile(`\s+`)
)

// NormalizeError reduces an error message to a stable canonical form by
// masking volatile substrings (timestamps, hex addresses, numeric IDs,
// temporary paths, excess whitespace). Two errors that differ only in
// these volatile fields hash to the same fingerprint.
func NormalizeError(msg string) string {
	if msg == "" {
		return ""
	}
	s := strings.TrimSpace(msg)
	s = reRFC3339.ReplaceAllString(s, "<time>")
	s = reTmpPath.ReplaceAllString(s, "<tmppath>")
	s = reHex.ReplaceAllString(s, "<hex>")
	s = reDigits.ReplaceAllString(s, "<n>")
	s = reWhitespace.ReplaceAllString(s, " ")
	return strings.ToLower(s)
}

// ComputeErrorFingerprint returns (fingerprint, category) for a raw error
// message. The fingerprint is a 16-hex-character SHA-256 prefix over the
// normalized form. Category is a coarse-grained classification useful as
// a secondary key; it maps from heuristic keyword detection.
//
// Returns empty strings when msg is empty so callers can skip DB writes.
func ComputeErrorFingerprint(msg string) (fingerprint, category string) {
	norm := NormalizeError(msg)
	if norm == "" {
		return "", ""
	}
	sum := sha256.Sum256([]byte(norm))
	fingerprint = hex.EncodeToString(sum[:8]) // 16 hex chars
	category = classifyError(norm)
	return fingerprint, category
}

// classifyError assigns a coarse category based on common keywords. This
// is intentionally simple; the real signal for learning is the fingerprint.
func classifyError(normalized string) string {
	switch {
	case strings.Contains(normalized, "timeout") || strings.Contains(normalized, "deadline exceeded"):
		return "timeout"
	case strings.Contains(normalized, "permission denied") || strings.Contains(normalized, "forbidden"):
		return "permission"
	case strings.Contains(normalized, "not found") || strings.Contains(normalized, "no such"):
		return "not_found"
	case strings.Contains(normalized, "syntax") || strings.Contains(normalized, "parse"):
		return "syntax"
	case strings.Contains(normalized, "conflict") || strings.Contains(normalized, "merge"):
		return "conflict"
	case strings.Contains(normalized, "network") || strings.Contains(normalized, "connection"):
		return "network"
	case strings.Contains(normalized, "test") && (strings.Contains(normalized, "fail") || strings.Contains(normalized, "failed")):
		return "test_failure"
	case strings.Contains(normalized, "compile") || strings.Contains(normalized, "build"):
		return "build"
	default:
		return "generic"
	}
}
