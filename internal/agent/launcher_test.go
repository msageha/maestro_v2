package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllowedToolsByRole(t *testing.T) {
	tests := []struct {
		role      string
		wantTools []string
		wantOK    bool
	}{
		{"orchestrator", []string{"Bash(maestro:*)", "Read(.maestro/**)"}, true},
		{"planner", []string{"Bash(maestro:*)", "Read(.maestro/**)"}, true},
		{"worker", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			tools, ok := allowedToolsByRole[tt.role]
			if ok != tt.wantOK {
				t.Errorf("allowedToolsByRole[%q] ok = %v, want %v", tt.role, ok, tt.wantOK)
			}
			if ok {
				if len(tools) != len(tt.wantTools) {
					t.Fatalf("len = %d, want %d", len(tools), len(tt.wantTools))
				}
				for i, tool := range tools {
					if tool != tt.wantTools[i] {
						t.Errorf("tools[%d] = %q, want %q", i, tool, tt.wantTools[i])
					}
				}
			}
		})
	}
}

func TestAllowedToolsByRole_OrchestratorCannotUseBashFreely(t *testing.T) {
	tools := allowedToolsByRole["orchestrator"]
	joined := strings.Join(tools, ",")

	// Must have Bash(maestro:*), not bare "Bash"
	if strings.Contains(joined, ",Bash,") || joined == "Bash" || strings.HasPrefix(joined, "Bash,") {
		t.Error("orchestrator must use Bash(maestro:*), not bare Bash")
	}
	if !strings.Contains(joined, "Bash(maestro:*)") {
		t.Error("orchestrator must have Bash(maestro:*)")
	}
}

func TestAllowedToolsByRole_PlannerCannotUseBashFreely(t *testing.T) {
	tools := allowedToolsByRole["planner"]
	joined := strings.Join(tools, ",")

	if strings.Contains(joined, ",Bash,") || joined == "Bash" || strings.HasPrefix(joined, "Bash,") {
		t.Error("planner must use Bash(maestro:*), not bare Bash")
	}
	if !strings.Contains(joined, "Bash(maestro:*)") {
		t.Error("planner must have Bash(maestro:*)")
	}
}

func TestAllowedToolsByRole_OrchestratorReadHasPathRestriction(t *testing.T) {
	tools := allowedToolsByRole["orchestrator"]
	joined := strings.Join(tools, ",")

	if !strings.Contains(joined, "Read(.maestro/**)") {
		t.Error("orchestrator must have Read(.maestro/**)")
	}
	// Must NOT have bare "Read" (unrestricted)
	for _, tool := range tools {
		if tool == "Read" {
			t.Error("orchestrator must use Read(.maestro/**), not bare Read")
		}
	}
}

func TestAllowedToolsByRole_PlannerReadHasPathRestriction(t *testing.T) {
	tools := allowedToolsByRole["planner"]
	joined := strings.Join(tools, ",")

	if !strings.Contains(joined, "Read(.maestro/**)") {
		t.Error("planner must have Read(.maestro/**)")
	}
	for _, tool := range tools {
		if tool == "Read" {
			t.Error("planner must use Read(.maestro/**), not bare Read")
		}
	}
}

func TestAllowedToolsByRole_WorkerUnrestricted(t *testing.T) {
	if _, ok := allowedToolsByRole["worker"]; ok {
		t.Error("worker should not be in allowedToolsByRole (unrestricted)")
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	dir := t.TempDir()

	maestroContent := "# Common Prompt\nTest maestro content"
	if err := os.WriteFile(filepath.Join(dir, "maestro.md"), []byte(maestroContent), 0644); err != nil {
		t.Fatal(err)
	}

	instDir := filepath.Join(dir, "instructions")
	if err := os.MkdirAll(instDir, 0755); err != nil {
		t.Fatal(err)
	}
	roleContent := "# Orchestrator\nTest instructions"
	if err := os.WriteFile(filepath.Join(instDir, "orchestrator.md"), []byte(roleContent), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := buildSystemPrompt(dir, "orchestrator")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, maestroContent) {
		t.Error("result should contain maestro.md content")
	}
	if !strings.Contains(result, roleContent) {
		t.Error("result should contain role instruction content")
	}
	if !strings.Contains(result, "\n\n---\n\n") {
		t.Error("result should contain separator between sections")
	}

	// Verify order: maestro.md first, then instructions
	maestroIdx := strings.Index(result, maestroContent)
	roleIdx := strings.Index(result, roleContent)
	if maestroIdx >= roleIdx {
		t.Error("maestro.md content should come before role instructions")
	}
}

func TestBuildSystemPrompt_MissingMaestroMD(t *testing.T) {
	dir := t.TempDir()
	instDir := filepath.Join(dir, "instructions")
	if err := os.MkdirAll(instDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "worker.md"), []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := buildSystemPrompt(dir, "worker")
	if err == nil {
		t.Error("expected error for missing maestro.md")
	}
}

func TestBuildSystemPrompt_MissingInstructionsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "maestro.md"), []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "instructions"), 0755); err != nil {
		t.Fatal(err)
	}

	_, err := buildSystemPrompt(dir, "nonexistent")
	if err == nil {
		t.Error("expected error for missing instructions file")
	}
}

func TestBuildLaunchArgs_NotificationDisabledForNonOrchestrator(t *testing.T) {
	tests := []struct {
		role               string
		wantNotificationOff bool
	}{
		{"orchestrator", false},
		{"planner", true},
		{"worker", true},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			args := buildLaunchArgs(tt.role, "sonnet", "system-prompt")
			joined := strings.Join(args, " ")

			hasNotificationOff := strings.Contains(joined, `"Notification":[]`)
			if hasNotificationOff != tt.wantNotificationOff {
				t.Errorf("role=%s: Notification disabled = %v, want %v\nargs: %v",
					tt.role, hasNotificationOff, tt.wantNotificationOff, args)
			}

			// Must never use disableAllHooks (would kill PreToolUse/PostToolUse)
			if strings.Contains(joined, "disableAllHooks") {
				t.Errorf("role=%s: must not use disableAllHooks", tt.role)
			}
		})
	}
}
