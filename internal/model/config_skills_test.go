package model

import (
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

func TestSkillsConfig_ExtraDirsDecode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "absent key decodes to nil",
			yaml: "skills:\n  enabled: true\n",
			want: nil,
		},
		{
			name: "empty list decodes to empty",
			yaml: "skills:\n  extra_dirs: []\n",
			want: []string{},
		},
		{
			name: "relative and absolute entries preserved in order",
			yaml: "skills:\n  extra_dirs:\n    - \".maestro-skills\"\n    - \"/abs/team/skills\"\n",
			want: []string{".maestro-skills", "/abs/team/skills"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cfg Config
			if err := yamlv3.Unmarshal([]byte(tt.yaml), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := cfg.Skills.ExtraDirs
			if len(got) != len(tt.want) {
				t.Fatalf("ExtraDirs = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ExtraDirs[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestValidateSkills_ExtraDirs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		dirs    []string
		wantErr string
	}{
		{name: "nil is valid", dirs: nil},
		{name: "non-empty entries are valid", dirs: []string{".maestro-skills", "/abs/path"}},
		// Missing directories are a use-time WARN, not a validation error.
		{name: "nonexistent path is valid at config level", dirs: []string{"does/not/exist"}},
		{name: "empty entry is rejected", dirs: []string{""}, wantErr: "skills.extra_dirs[0]: must not be empty"},
		{name: "whitespace entry is rejected", dirs: []string{".ok", "  "}, wantErr: "skills.extra_dirs[1]: must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{Skills: SkillsConfig{ExtraDirs: tt.dirs}}
			var errs []error
			cfg.validateSkills(&errs)
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("expected no errors, got %v", errs)
				}
				return
			}
			found := false
			for _, err := range errs {
				if strings.Contains(err.Error(), tt.wantErr) {
					found = true
				}
			}
			if !found {
				t.Errorf("expected error containing %q, got %v", tt.wantErr, errs)
			}
		})
	}
}
