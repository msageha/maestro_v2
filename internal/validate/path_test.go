package validate

import (
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
