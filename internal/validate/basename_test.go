package validate

import "testing"

func TestIsValidBaseName(t *testing.T) {
	valid := []string{
		"valid-name",
		"worker1",
		"my_file",
		"cmd_1772512842_98221ce6",
	}
	for _, name := range valid {
		if !IsValidBaseName(name) {
			t.Errorf("IsValidBaseName(%q) = false, want true", name)
		}
	}
}

func TestIsValidBaseName_Invalid(t *testing.T) {
	invalid := []struct {
		name string
		desc string
	}{
		{".", "current directory"},
		{"..", "parent directory"},
		{"../evil", "parent traversal"},
		{"path/traversal", "slash in name"},
		{"", "empty string"},
	}
	for _, tc := range invalid {
		if IsValidBaseName(tc.name) {
			t.Errorf("IsValidBaseName(%q) [%s] = true, want false", tc.name, tc.desc)
		}
	}
}
