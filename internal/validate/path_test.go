package validate

import (
	"os"
	"path/filepath"
	"runtime"
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
	realBase := t.TempDir()

	tests := []struct {
		base    string
		elem    string
		wantErr bool
		desc    string
	}{
		{realBase, "file.txt", false, "simple file"},
		{realBase, "sub/file.txt", false, "subdirectory file"},
		{realBase, "a/b/c", false, "nested subdirectory"},
		{realBase, "..", true, "parent traversal"},
		{realBase, "../etc/passwd", true, "path traversal"},
		{realBase, "sub/../../etc", true, "nested traversal"},
		{realBase, "", true, "empty element"},
		{realBase, "/absolute", true, "absolute path"},
		{realBase, ".", false, "current directory"},
		{realBase, "file\x00.txt", true, "null byte"},
		{"/nonexistent-base-dir", "file.txt", true, "non-existent base (fail-closed)"},
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

func TestSafePath_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests not reliable on Windows")
	}

	// Create a temp directory structure:
	//   base/
	//   base/legit/       (real directory)
	//   outside/          (directory outside base)
	//   base/escape       -> ../outside  (symlink escaping base)
	tmp := t.TempDir()
	base := filepath.Join(tmp, "base")
	outside := filepath.Join(tmp, "outside")
	if err := os.MkdirAll(filepath.Join(base, "legit"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	// Create a file in the outside directory
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	// Create a symlink inside base that points outside
	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Fatal(err)
	}

	// Accessing "escape/secret.txt" should be detected as escaping base
	_, err := SafePath(base, "escape/secret.txt")
	if err == nil {
		t.Error("SafePath should reject symlink that escapes base directory")
	}

	// Accessing "legit" should still work
	result, err := SafePath(base, "legit")
	if err != nil {
		t.Errorf("SafePath should allow access to real subdirectory: %v", err)
	}
	if result == "" {
		t.Error("SafePath returned empty path for valid subdirectory")
	}
}

func TestSafePath_SymlinkEscapeNonExistentLeaf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests not reliable on Windows")
	}

	// Even if the leaf file doesn't exist, a symlink escape in the path
	// should be detected by resolving the deepest existing ancestor.
	//   base/
	//   outside/
	//   base/escape -> ../outside  (symlink escaping base)
	// Accessing "escape/new.txt" (non-existent leaf) should still be rejected.
	tmp := t.TempDir()
	base := filepath.Join(tmp, "base")
	outside := filepath.Join(tmp, "outside")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Fatal(err)
	}

	_, err := SafePath(base, "escape/new.txt")
	if err == nil {
		t.Error("SafePath should reject symlink escape even when leaf file does not exist")
	}
}

func TestValidateFilePath(t *testing.T) {
	valid := []string{
		"/tmp/tasks.yaml",
		"tasks.yaml",
		"sub/tasks.yaml",
		"/absolute/path/file.txt",
		"relative/path/file.txt",
	}
	for _, path := range valid {
		result, err := ValidateFilePath(path)
		if err != nil {
			t.Errorf("ValidateFilePath(%q) returned error: %v", path, err)
		}
		if result != filepath.Clean(path) {
			t.Errorf("ValidateFilePath(%q) = %q, want %q", path, result, filepath.Clean(path))
		}
	}
}

func TestValidateFilePath_Invalid(t *testing.T) {
	invalid := []struct {
		path string
		desc string
	}{
		{"", "empty string"},
		{"file\x00.txt", "null byte"},
		{"../etc/passwd", "parent traversal"},
		{"../../etc/passwd", "double parent traversal"},
		{"sub/../../etc/passwd", "nested traversal"},
		{"..", "bare parent"},
	}
	for _, tc := range invalid {
		_, err := ValidateFilePath(tc.path)
		if err == nil {
			t.Errorf("ValidateFilePath(%q) [%s] should have returned error", tc.path, tc.desc)
		}
	}
}

func TestSafePath_NonExistentTarget(t *testing.T) {
	// SafePath should return the lexically-checked path for non-existent targets
	tmp := t.TempDir()
	result, err := SafePath(tmp, "nonexistent/file.txt")
	if err != nil {
		t.Errorf("SafePath should not error on non-existent target: %v", err)
	}
	expected := filepath.Join(tmp, "nonexistent/file.txt")
	if result != expected {
		t.Errorf("SafePath returned %q, want %q", result, expected)
	}
}
