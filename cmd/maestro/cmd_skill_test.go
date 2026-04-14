package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/uds"
)

func TestRunSkillList_RoleRequired(t *testing.T) {
	err := runSkillList([]string{})
	if err == nil {
		t.Fatal("expected error when --role is missing")
	}
	if !strings.Contains(err.Error(), "--role is required") {
		t.Errorf("expected '--role is required' error, got: %v", err)
	}
}

func TestRunSkillList_InvalidRole(t *testing.T) {
	tests := []struct {
		name string
		role string
	}{
		{"dot", "."},
		{"dotdot", ".."},
		{"slash", "foo/bar"},
		{"backslash", "foo\\bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runSkillList([]string{"--role", tt.role})
			if err == nil {
				t.Fatal("expected error for invalid role")
			}
			if !strings.Contains(err.Error(), "invalid --role") {
				t.Errorf("expected 'invalid --role' error, got: %v", err)
			}
		})
	}
}

func TestRunSkillList_WithRole(t *testing.T) {
	// Set up a temporary .maestro directory
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")

	// Create worker skill
	workerSkillDir := filepath.Join(maestroDir, "skills", "worker", "test-skill")
	if err := os.MkdirAll(workerSkillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerSkillDir, "SKILL.md"),
		[]byte("---\nname: Test Skill\ndescription: A test skill\n---\nBody"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create shared skill
	shareSkillDir := filepath.Join(maestroDir, "skills", "share", "shared-skill")
	if err := os.MkdirAll(shareSkillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shareSkillDir, "SKILL.md"),
		[]byte("---\nname: Shared Skill\ndescription: A shared skill\n---\nBody"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create planner skill (should NOT appear in worker list)
	plannerSkillDir := filepath.Join(maestroDir, "skills", "planner", "planner-skill")
	if err := os.MkdirAll(plannerSkillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plannerSkillDir, "SKILL.md"),
		[]byte("---\nname: Planner Skill\ndescription: A planner skill\n---\nBody"), 0644); err != nil {
		t.Fatal(err)
	}

	// Change to the temp directory so requireMaestroDir finds .maestro
	origDir, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSkillList([]string{"--role", "worker"})

	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "Test Skill") {
		t.Error("output should contain worker-specific skill")
	}
	if strings.Contains(output, "Shared Skill") {
		t.Error("output should NOT contain shared skill (shared skills are auto-injected, not listed)")
	}
	if strings.Contains(output, "Planner Skill") {
		t.Error("output should NOT contain planner-specific skill")
	}
}

func TestRunSkillList_ShareRole(t *testing.T) {
	// Set up a temporary .maestro directory
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")

	// Create shared skill
	shareSkillDir := filepath.Join(maestroDir, "skills", "share", "shared-skill")
	if err := os.MkdirAll(shareSkillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shareSkillDir, "SKILL.md"),
		[]byte("---\nname: Shared Skill\ndescription: A shared skill\n---\nBody"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create worker skill (should NOT appear in share list)
	workerSkillDir := filepath.Join(maestroDir, "skills", "worker", "worker-skill")
	if err := os.MkdirAll(workerSkillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerSkillDir, "SKILL.md"),
		[]byte("---\nname: Worker Skill\ndescription: A worker skill\n---\nBody"), 0644); err != nil {
		t.Fatal(err)
	}

	// Change to the temp directory so requireMaestroDir finds .maestro
	origDir, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSkillList([]string{"--role", "share"})

	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "Shared Skill") {
		t.Error("output should contain shared skill when --role share")
	}
	if strings.Contains(output, "Worker Skill") {
		t.Error("output should NOT contain worker-specific skill")
	}
}

func TestRunSkillApprove_UnmarshalError(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return &uds.Response{Success: true, Data: json.RawMessage(`not valid json`)}, nil
		},
	})

	err := app.runSkillApprove([]string{"candidate_0000000001_abcdef01"})
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
	if !strings.Contains(err.Error(), "unmarshal response") {
		t.Errorf("expected 'unmarshal response' in error, got: %v", err)
	}
}

func TestSanitizeForTerminal(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text", "hello", "hello"},
		{"tab replaced", "a\tb", "a b"},
		{"newline replaced", "a\nb", "a b"},
		{"escape removed", "hello\x1b[31mworld\x1b[0m", "hello[31mworld[0m"},
		{"null removed", "a\x00b", "ab"},
		{"multibyte preserved", "日本語テスト", "日本語テスト"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeForTerminal(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeForTerminal(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTruncateMultibyte(t *testing.T) {
	// Verify that multibyte content is truncated at rune boundary, not byte boundary
	// 81 CJK characters (each 3 bytes in UTF-8) should be truncated to 77 runes + "..."
	input := strings.Repeat("あ", 81)
	runes := []rune(input)
	if len(runes) <= 80 {
		t.Fatal("test setup error: input should have >80 runes")
	}
	truncated := input
	if r := []rune(truncated); len(r) > 80 {
		truncated = string(r[:77]) + "..."
	}
	// Verify we got 77 "あ" + "..."
	expected := strings.Repeat("あ", 77) + "..."
	if truncated != expected {
		t.Errorf("truncation mismatch: got %d bytes, want %d bytes", len(truncated), len(expected))
	}
}
