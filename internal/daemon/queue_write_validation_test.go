package daemon

import (
	"testing"
)

// TestValidatePersonaAndSkillRefs guards the UDS-boundary identifier check
// added so that queue_write command/task entrypoints reject the same unsafe
// persona_hint / skill_refs values that internal/plan/validate.go rejects on
// the planner side. Without this regression test, a future refactor could
// drop the check and let path-traversal-like identifiers (e.g. "../foo")
// reach the queue files and downstream skill resolution.
func TestValidatePersonaAndSkillRefs(t *testing.T) {
	cases := []struct {
		name        string
		persona     string
		skills      []string
		wantInvalid bool
	}{
		{name: "all_empty", persona: "", skills: nil, wantInvalid: false},
		{name: "valid_persona_only", persona: "implementer", wantInvalid: false},
		{name: "valid_skill_only", skills: []string{"go-testing"}, wantInvalid: false},
		{name: "valid_both", persona: "researcher", skills: []string{"a", "b"}, wantInvalid: false},

		{name: "persona_path_traversal", persona: "../foo", wantInvalid: true},
		{name: "persona_with_slash", persona: "team/role", wantInvalid: true},
		{name: "persona_with_backslash", persona: "team\\role", wantInvalid: true},
		{name: "persona_null_byte", persona: "role\x00", wantInvalid: true},
		{name: "persona_dotdot", persona: "..", wantInvalid: true},
		{name: "persona_dot", persona: ".", wantInvalid: true},

		{name: "skill_path_traversal", skills: []string{"../etc/passwd"}, wantInvalid: true},
		{name: "skill_with_slash", skills: []string{"a/b"}, wantInvalid: true},
		{name: "skill_with_backslash", skills: []string{"a\\b"}, wantInvalid: true},
		{name: "skill_null_byte", skills: []string{"a\x00b"}, wantInvalid: true},
		{name: "skill_empty_string", skills: []string{""}, wantInvalid: true},
		{name: "skill_mixed_valid_invalid", skills: []string{"good", "../bad"}, wantInvalid: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := validatePersonaAndSkillRefs(tc.persona, tc.skills)
			gotInvalid := resp != nil
			if gotInvalid != tc.wantInvalid {
				t.Errorf("got invalid=%v (resp=%+v), want invalid=%v", gotInvalid, resp, tc.wantInvalid)
			}
		})
	}
}
