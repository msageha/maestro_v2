package validate

import (
	"path/filepath"
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
		if err := ID(id); err != nil {
			t.Errorf("ID(%q) returned error: %v", id, err)
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
		if err := ID(tc.id); err == nil {
			t.Errorf("ID(%q) [%s] should have returned error", tc.id, tc.desc)
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
		if err := ProjectName(name); err != nil {
			t.Errorf("ProjectName(%q) returned error: %v", name, err)
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
		if err := ProjectName(tc.name); err == nil {
			t.Errorf("ProjectName(%q) [%s] should have returned error", tc.name, tc.desc)
		}
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
		result, err := FilePath(path)
		if err != nil {
			t.Errorf("FilePath(%q) returned error: %v", path, err)
		}
		if result != filepath.Clean(path) {
			t.Errorf("FilePath(%q) = %q, want %q", path, result, filepath.Clean(path))
		}
	}
}

func TestContentLength(t *testing.T) {
	t.Run("within limit", func(t *testing.T) {
		if err := ContentLength("--content", "short", 100); err != nil {
			t.Errorf("ContentLength should accept short input: %v", err)
		}
	})
	t.Run("exactly at limit", func(t *testing.T) {
		val := string(make([]byte, 100))
		if err := ContentLength("--content", val, 100); err != nil {
			t.Errorf("ContentLength should accept input at exact limit: %v", err)
		}
	})
	t.Run("exceeds limit", func(t *testing.T) {
		val := string(make([]byte, 101))
		err := ContentLength("--content", val, 100)
		if err == nil {
			t.Fatal("ContentLength should reject input exceeding limit")
		}
		if !testing.Short() {
			expected := "validate: --content exceeds maximum size of 100 bytes (got 101 bytes)"
			if err.Error() != expected {
				t.Errorf("got error %q, want %q", err.Error(), expected)
			}
		}
	})
	t.Run("empty string", func(t *testing.T) {
		if err := ContentLength("--content", "", 100); err != nil {
			t.Errorf("ContentLength should accept empty string: %v", err)
		}
	})
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
		_, err := FilePath(tc.path)
		if err == nil {
			t.Errorf("FilePath(%q) [%s] should have returned error", tc.path, tc.desc)
		}
	}
}
