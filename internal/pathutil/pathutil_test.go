package pathutil

import "testing"

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
