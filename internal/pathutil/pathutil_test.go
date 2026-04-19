package pathutil

import (
	"strings"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		// Normal cases
		{name: "relative path", in: "a/b/c", want: "a/b/c"},
		{name: "absolute path", in: "/a/b/c", want: "/a/b/c"},
		{name: "single segment", in: "foo", want: "foo"},

		// Dot and double-dot
		{name: "path with dot", in: "a/./b", want: "a/b"},
		{name: "path with double-dot", in: "a/b/../c", want: "a/c"},
		{name: "leading double-dot", in: "../a/b", want: "../a/b"},

		// Trailing slash
		{name: "trailing slash", in: "a/b/", want: "a/b"},
		{name: "absolute trailing slash", in: "/a/b/", want: "/a/b"},
		{name: "multiple trailing slashes via clean", in: "a/b//", want: "a/b"},

		// Empty and root-equivalent
		{name: "empty string", in: "", want: ""},
		{name: "dot only", in: ".", want: ""},
		{name: "dot slash", in: "./", want: ""},

		// Double slashes
		{name: "double slashes", in: "a//b//c", want: "a/b/c"},

		// Backslash (path.Clean treats as literal, not separator)
		{name: "backslash preserved", in: `a\b\c`, want: `a\b\c`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizePath(tt.in)
			if got != tt.want {
				t.Errorf("NormalizePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsDescendant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		child string
		dir   string
		want  bool
	}{
		// Positive cases
		{name: "direct child", child: "a/b", dir: "a", want: true},
		{name: "nested descendant", child: "a/b/c/d", dir: "a", want: true},
		{name: "deep descendant", child: "a/b/c/d", dir: "a/b", want: true},
		{name: "absolute paths", child: "/usr/local/bin", dir: "/usr/local", want: true},

		// Negative cases
		{name: "same path", child: "a", dir: "a", want: false},
		{name: "not ancestor", child: "b/c", dir: "a", want: false},
		{name: "sibling", child: "ab/c", dir: "a", want: false},
		{name: "parent is longer", child: "a", dir: "a/b", want: false},
		{name: "empty child", child: "", dir: "a", want: false},
		{name: "empty dir", child: "a/b", dir: "", want: false},

		// Edge cases: prefix collision
		{name: "prefix but not boundary", child: "abc/d", dir: "ab", want: false},
		{name: "dir with trailing content", child: "a-extra/b", dir: "a", want: false},

		// Edge cases: path traversal
		{name: "traversal attempt", child: "a/../b/c", dir: "b", want: false},
		// Note: IsDescendant uses raw string prefix, not path.Clean.
		// "a/b/../../c" starts with "a/" so it returns true.
		{name: "child with dot-dot raw prefix", child: "a/b/../../c", dir: "a", want: true},

		// Edge cases: trailing slash
		{name: "dir trailing slash", child: "a/b/c", dir: "a/b/", want: false},
		{name: "child trailing slash", child: "a/b/c/", dir: "a/b", want: true},

		// Both empty
		{name: "both empty", child: "", dir: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsDescendant(tt.child, tt.dir)
			if got != tt.want {
				t.Errorf("IsDescendant(%q, %q) = %v, want %v", tt.child, tt.dir, got, tt.want)
			}
		})
	}
}

// TestNormalizePath_PathTraversal tests that NormalizePath handles path traversal
// inputs predictably. path.Clean resolves ".." components, which is the expected
// behavior for the normalize step.
func TestNormalizePath_PathTraversal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		// Classic traversal patterns — path.Clean resolves ".." lexically
		{name: "etc passwd traversal", in: "../../../etc/passwd", want: "../../../etc/passwd"},
		{name: "deep traversal", in: "a/../../../../etc/shadow", want: "../../../etc/shadow"},
		{name: "traversal from nested dir", in: "a/b/c/../../../etc/passwd", want: "etc/passwd"},
		{name: "traversal that stays within", in: "a/b/../c", want: "a/c"},

		// Absolute path traversal
		{name: "absolute traversal", in: "/a/b/../../../etc/passwd", want: "/etc/passwd"},
		{name: "absolute deep traversal", in: "/../../../../etc/passwd", want: "/etc/passwd"},

		// Dot-dot with mixed separators and redundant slashes
		{name: "double slashes with traversal", in: "a//b//../c", want: "a/c"},
		{name: "traversal with trailing slash", in: "a/../../b/", want: "../b"},

		// Null byte (should be preserved as-is — caller must reject)
		{name: "null byte in path", in: "a/b\x00c/d", want: "a/b\x00c/d"},
		{name: "null byte with traversal", in: "../\x00/etc/passwd", want: "../\x00/etc/passwd"},

		// Encoded sequences (path.Clean does not decode URL-encoding)
		{name: "percent-encoded dot-dot", in: "a/..%2f..%2fetc/passwd", want: "a/..%2f..%2fetc/passwd"},
		{name: "percent-encoded slash", in: "a%2fb%2fc", want: "a%2fb%2fc"},

		// Unicode edge cases
		{name: "unicode path segment", in: "a/日本語/b", want: "a/日本語/b"},
		{name: "fullwidth slash U+FF0F", in: "a\uFF0Fb\uFF0Fc", want: "a\uFF0Fb\uFF0Fc"},
		{name: "line separator U+2028", in: "a/\u2028/b", want: "a/\u2028/b"},
		{name: "paragraph separator U+2029", in: "a/\u2029/b", want: "a/\u2029/b"},

		// Backslash-based traversal attempts (path.Clean treats \ as literal)
		{name: "backslash traversal attempt", in: `a\..\b`, want: `a\..\b`},
		{name: "mixed separators traversal", in: `a\..\/b`, want: `a\..\/b`},

		// Very long path
		{name: "long path with traversal", in: strings.Repeat("a/", 500) + "../../../etc/passwd", want: strings.Repeat("a/", 496) + "a/etc/passwd"},

		// Only dots
		{name: "triple dot", in: "a/.../b", want: "a/.../b"},
		{name: "quadruple dot", in: "a/..../b", want: "a/..../b"},
		{name: "just double-dot", in: "..", want: ".."},
		{name: "double-dot with slash", in: "../", want: ".."},

		// Root path edge cases
		// "/" → TrimSuffix removes trailing "/" → "" → path.Clean("") = "." → returns ""
		{name: "root slash only", in: "/", want: ""},
		{name: "root with dot-dot", in: "/..", want: "/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizePath(tt.in)
			if got != tt.want {
				t.Errorf("NormalizePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestIsDescendant_PathTraversal tests that IsDescendant rejects traversal attempts
// when used with raw (un-normalized) paths and when combined with NormalizePath.
func TestIsDescendant_PathTraversal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		child string
		dir   string
		want  bool
	}{
		// Direct traversal escape — raw path does not start with dir prefix
		{name: "classic etc passwd", child: "../../../etc/passwd", dir: "project", want: false},
		{name: "traversal from root", child: "/../../etc/passwd", dir: "/home", want: false},

		// Traversal disguised with valid prefix (raw prefix match is true)
		{name: "traverse out then back raw", child: "project/../../etc/passwd", dir: "project", want: true},
		{name: "deep escape raw", child: "project/a/../../../../etc/passwd", dir: "project", want: true},

		// Null byte in child trying to escape
		{name: "null byte traversal child", child: "project/\x00/../etc/passwd", dir: "project", want: true},

		// Empty and whitespace edge cases
		{name: "whitespace dir", child: " /b", dir: " ", want: true},
		{name: "dot-dot as dir", child: "../a/b", dir: "..", want: true},

		// Prefix collision with traversal
		{name: "dir prefix collision with dot-dot", child: "projects/../secret", dir: "project", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsDescendant(tt.child, tt.dir)
			if got != tt.want {
				t.Errorf("IsDescendant(%q, %q) = %v, want %v", tt.child, tt.dir, got, tt.want)
			}
		})
	}
}

// TestContainmentWithNormalization tests the secure pattern: NormalizePath first,
// then IsDescendant. This is the expected usage for path traversal defense.
// Normalizing before checking containment prevents traversal via ".." components.
func TestContainmentWithNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		child    string
		dir      string
		wantSafe bool // expected result after normalization
	}{
		// Valid paths remain valid after normalization
		{name: "valid nested path", child: "project/src/main.go", dir: "project", wantSafe: true},
		{name: "valid deep path", child: "project/a/b/c/d.go", dir: "project", wantSafe: true},

		// Traversal attempts are neutralized by NormalizePath
		{name: "traversal neutralized", child: "project/../../etc/passwd", dir: "project", wantSafe: false},
		{name: "deep traversal neutralized", child: "project/a/../../../../etc/passwd", dir: "project", wantSafe: false},
		{name: "traverse to sibling", child: "project/../other/secret", dir: "project", wantSafe: false},
		{name: "traverse within then out", child: "project/a/b/../../c/../../etc/passwd", dir: "project", wantSafe: false},

		// Traversal that resolves back inside the dir
		{name: "traverse out and back in", child: "project/../project/file", dir: "project", wantSafe: true},
		{name: "dot-dot resolves within", child: "project/a/../b/file", dir: "project", wantSafe: true},

		// Redundant slashes + traversal
		{name: "double slash traversal", child: "project//../../etc/passwd", dir: "project", wantSafe: false},
		{name: "double slash valid", child: "project//a//b", dir: "project", wantSafe: true},

		// Dot segments
		{name: "dot segment valid", child: "project/./a/b", dir: "project", wantSafe: true},
		{name: "dot segment traversal", child: "project/./../../etc", dir: "project", wantSafe: false},

		// Edge: child equals dir after normalization
		{name: "child equals dir after norm", child: "project/a/..", dir: "project", wantSafe: false},

		// Edge: empty after normalization
		{name: "child normalizes to empty", child: ".", dir: "project", wantSafe: false},
		{name: "dir normalizes to empty", child: "project/a", dir: ".", wantSafe: false},

		// Absolute paths
		{name: "absolute traversal blocked", child: "/home/user/../../../etc/passwd", dir: "/home/user", wantSafe: false},
		{name: "absolute valid", child: "/home/user/docs/file.txt", dir: "/home/user", wantSafe: true},

		// URL-encoded traversal (not decoded by path.Clean — stays literal)
		{name: "percent-encoded stays literal", child: "project/..%2f..%2fetc/passwd", dir: "project", wantSafe: true},

		// Null byte — path.Clean does not strip null bytes, so the path is
		// preserved. Whether the result is "safe" depends on the literal prefix.
		{name: "null byte in child", child: "project/\x00/../etc", dir: "project", wantSafe: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			normalizedChild := NormalizePath(tt.child)
			normalizedDir := NormalizePath(tt.dir)
			got := IsDescendant(normalizedChild, normalizedDir)
			if got != tt.wantSafe {
				t.Errorf(
					"IsDescendant(NormalizePath(%q), NormalizePath(%q)) = IsDescendant(%q, %q) = %v, want %v",
					tt.child, tt.dir, normalizedChild, normalizedDir, got, tt.wantSafe,
				)
			}
		})
	}
}

// TestNormalizePath_EdgeCases tests additional edge cases for robustness.
func TestNormalizePath_EdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		// Whitespace handling (preserved by path.Clean)
		{name: "leading space", in: " a/b", want: " a/b"},
		{name: "trailing space", in: "a/b ", want: "a/b "},
		{name: "space-only segment", in: "a/ /b", want: "a/ /b"},
		{name: "tab in segment", in: "a/\t/b", want: "a/\t/b"},

		// Very long single segment (no slash)
		{name: "long single segment", in: strings.Repeat("x", 4096), want: strings.Repeat("x", 4096)},

		// Multiple consecutive dot-dot
		{name: "many dot-dot relative", in: "../../../../..", want: "../../../../.."},

		// Slash-only paths
		{name: "multiple slashes only", in: "///", want: "/"},

		// Mixed dots
		{name: "dot then dot-dot", in: "./a/../b", want: "b"},
		{name: "dot-dot then dot", in: "../a/./b", want: "../a/b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizePath(tt.in)
			if got != tt.want {
				t.Errorf("NormalizePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
