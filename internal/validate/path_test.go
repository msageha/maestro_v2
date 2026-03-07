package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateID(t *testing.T) {
	valid := []string{
		"a",
		"abc",
		"worker1",
		"worker10",
		"cmd_1772512842_98221ce6",
		"task_1772512842_98221ce6",
		"phase_1772512842_c3d4e5f6",
		"my-worker",
		"my_worker",
		"my.worker",
		"A1",
		"a1b2c3",
	}
	for _, id := range valid {
		if err := ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q) returned error: %v", id, err)
		}
	}
}

func TestValidateID_Invalid(t *testing.T) {
	invalid := []struct {
		id   string
		desc string
	}{
		{"", "empty string"},
		{".", "single dot"},
		{"..", "double dot"},
		{"../etc/passwd", "path traversal"},
		{"/absolute", "absolute path"},
		{".hidden", "leading dot"},
		{"trailing.", "trailing dot"},
		{"trailing-", "trailing hyphen"},
		{"-leading", "leading hyphen"},
		{"_leading", "leading underscore"},
		{"has space", "contains space"},
		{"has/slash", "contains slash"},
		{"has\\backslash", "contains backslash"},
		{"has:colon", "contains colon"},
		{"has\x00null", "contains null byte"},
		{"café", "non-ASCII characters"},
		{"日本語", "unicode characters"},
		{"a\nb", "contains newline"},
		{"a\tb", "contains tab"},
	}
	for _, tc := range invalid {
		if err := ValidateID(tc.id); err == nil {
			t.Errorf("ValidateID(%q) [%s] should have returned error", tc.id, tc.desc)
		}
	}
}

func TestValidateProjectName(t *testing.T) {
	valid := []string{
		"a",
		"myproject",
		"my-project",
		"my_project",
		"Project1",
		"abc123",
	}
	for _, name := range valid {
		if err := ValidateProjectName(name); err != nil {
			t.Errorf("ValidateProjectName(%q) returned error: %v", name, err)
		}
	}
}

func TestValidateProjectName_Invalid(t *testing.T) {
	invalid := []struct {
		name string
		desc string
	}{
		{"", "empty string"},
		{".", "single dot"},
		{"..", "double dot"},
		{"my.project", "contains dot (tmux special)"},
		{"my:project", "contains colon (tmux special)"},
		{"-leading", "leading hyphen"},
		{"_leading", "leading underscore"},
		{"has space", "contains space"},
		{"has/slash", "contains slash"},
		{"\x00null", "contains null byte"},
		{"café", "non-ASCII characters"},
	}
	for _, tc := range invalid {
		if err := ValidateProjectName(tc.name); err == nil {
			t.Errorf("ValidateProjectName(%q) [%s] should have returned error", tc.name, tc.desc)
		}
	}
}

func TestSafePath(t *testing.T) {
	tests := []struct {
		base    string
		elem    string
		wantErr bool
		desc    string
	}{
		{"/base", "file.txt", false, "simple file"},
		{"/base", "sub/file.txt", false, "subdirectory file"},
		{"/base", "a/b/c", false, "nested subdirectory"},
		{"/base", "..", true, "parent traversal"},
		{"/base", "../etc/passwd", true, "path traversal"},
		{"/base", "sub/../../etc", true, "nested traversal"},
		{"/base", "", true, "empty element"},
		{"/base", "/absolute", true, "absolute path"},
		{"/base", ".", false, "current directory"},
		{"/base", "file\x00.txt", true, "null byte"},
	}
	for _, tc := range tests {
		result, err := SafePath(tc.base, tc.elem)
		if tc.wantErr {
			if err == nil {
				t.Errorf("SafePath(%q, %q) [%s] should have returned error, got %q", tc.base, tc.elem, tc.desc, result)
			}
		} else {
			if err != nil {
				t.Errorf("SafePath(%q, %q) [%s] returned error: %v", tc.base, tc.elem, tc.desc, err)
			}
		}
	}
}

func TestValidateFilePath(t *testing.T) {
	// Use a temp dir as cwd to make relative path tests deterministic.
	// Resolve symlinks (macOS: /var -> /private/var) to match filepath.Abs behavior.
	tmpDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	cwd := resolved
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	sep := string(filepath.Separator)

	tests := []struct {
		name    string
		input   string
		want    func(string) string // returns expected path given cwd
		wantErr string              // substring of expected error; empty means success
	}{
		// --- Error cases ---
		{
			name:    "empty string",
			input:   "",
			wantErr: "must not be empty",
		},
		{
			name:    "null byte at start",
			input:   "\x00x",
			wantErr: "contains null byte",
		},
		{
			name:    "null byte in middle",
			input:   "a\x00b",
			wantErr: "contains null byte",
		},
		{
			name:    "null byte at end",
			input:   "x\x00",
			wantErr: "contains null byte",
		},

		// --- Success: relative paths converted to absolute ---
		{
			name:  "simple relative file",
			input: "file.txt",
			want:  func(cwd string) string { return filepath.Join(cwd, "file.txt") },
		},
		{
			name:  "dot-slash relative",
			input: "." + sep + "file.txt",
			want:  func(cwd string) string { return filepath.Join(cwd, "file.txt") },
		},
		{
			name:  "current directory dot",
			input: ".",
			want:  func(cwd string) string { return cwd },
		},

		// --- Success: already absolute ---
		{
			name:  "already absolute path",
			input: filepath.Join(cwd, "dir", "file.txt"),
			want:  func(_ string) string { return filepath.Join(cwd, "dir", "file.txt") },
		},

		// --- Success: filepath.Clean applied (redundant separators, dot segments) ---
		{
			name:  "redundant separators cleaned",
			input: "dir" + sep + sep + "sub" + sep + sep + "file.txt",
			want:  func(cwd string) string { return filepath.Join(cwd, "dir", "sub", "file.txt") },
		},
		{
			name:  "dot segment removed",
			input: "dir" + sep + "." + sep + "file.txt",
			want:  func(cwd string) string { return filepath.Join(cwd, "dir", "file.txt") },
		},
		{
			name:  "parent segment inside path normalized",
			input: "a" + sep + ".." + sep + "file.txt",
			want:  func(cwd string) string { return filepath.Join(cwd, "file.txt") },
		},

		// --- Security boundary: traversal inputs are normalized, not rejected ---
		// ValidateFilePath is a normalization function; traversal prevention
		// is handled by SafePath. These tests document that behavior.
		{
			name:  "traversal normalized not rejected",
			input: ".." + sep + "secret.txt",
			want:  func(cwd string) string { return filepath.Join(filepath.Dir(cwd), "secret.txt") },
		},
		{
			name:  "deep traversal normalized",
			input: ".." + sep + ".." + sep + "etc" + sep + "passwd",
			want: func(cwd string) string {
				parent := filepath.Dir(filepath.Dir(cwd))
				return filepath.Join(parent, "etc", "passwd")
			},
		},
		{
			name:  "parent directory",
			input: "..",
			want:  func(cwd string) string { return filepath.Dir(cwd) },
		},

		// --- Edge cases ---
		{
			name:  "path with spaces",
			input: "dir with spaces" + sep + "file name.txt",
			want:  func(cwd string) string { return filepath.Join(cwd, "dir with spaces", "file name.txt") },
		},
		{
			name:  "unicode path",
			input: "日本語" + sep + "ファイル.txt",
			want:  func(cwd string) string { return filepath.Join(cwd, "日本語", "ファイル.txt") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateFilePath(tt.input)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (result: %q)", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// All returned paths must be absolute
			if !filepath.IsAbs(got) {
				t.Fatalf("result is not absolute: %q", got)
			}

			want := tt.want(cwd)
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}
