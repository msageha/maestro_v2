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
		{"orchestrator", []string{"Bash(maestro:*)", "Read(.maestro/dashboard.md)", "Read(.maestro/results/*)", "Read(.maestro/config.yaml)", "Read(.maestro/state/continuous.yaml)"}, true},
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

	// Orchestrator Read is scoped to specific files only
	expectedReads := []string{
		"Read(.maestro/dashboard.md)",
		"Read(.maestro/results/*)",
		"Read(.maestro/config.yaml)",
	}
	for _, r := range expectedReads {
		if !strings.Contains(joined, r) {
			t.Errorf("orchestrator must have %q", r)
		}
	}
	// Must NOT have bare "Read" (unrestricted) or wildcard Read(.maestro/**)
	for _, tool := range tools {
		if tool == "Read" {
			t.Error("orchestrator must not have bare Read")
		}
		if tool == "Read(.maestro/**)" {
			t.Error("orchestrator must not have wildcard Read(.maestro/**)")
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

// writeConfigYAML writes a minimal valid config.yaml to the given maestro dir.
func writeConfigYAML(t *testing.T, dir string, skillsEnabled bool) {
	t.Helper()
	enabled := "false"
	if skillsEnabled {
		enabled = "true"
	}
	content := "project:\n  name: test\nmaestro:\n  version: \"1.0\"\nagents:\n  workers:\n    count: 1\nskills:\n  enabled: " + enabled + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSystemPrompt_OrchestratorNoSkillInjection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "maestro.md"), []byte("# Maestro"), 0644); err != nil {
		t.Fatal(err)
	}
	instDir := filepath.Join(dir, "instructions")
	if err := os.MkdirAll(instDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "orchestrator.md"), []byte("# Orchestrator Instructions"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create orchestrator-specific and shared skills — these should NOT be injected
	orchSkillDir := filepath.Join(dir, "skills", "orchestrator", "orch-skill")
	if err := os.MkdirAll(orchSkillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orchSkillDir, "SKILL.md"), []byte("---\nname: Orch Skill\n---\nOrchestrator skill body"), 0644); err != nil {
		t.Fatal(err)
	}

	shareSkillDir := filepath.Join(dir, "skills", "share", "shared-skill")
	if err := os.MkdirAll(shareSkillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shareSkillDir, "SKILL.md"), []byte("---\nname: Shared Skill\n---\nShared skill body"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := buildSystemPrompt(dir, "orchestrator")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "# Maestro") {
		t.Error("result should contain maestro.md content")
	}
	if !strings.Contains(result, "# Orchestrator Instructions") {
		t.Error("result should contain orchestrator instructions")
	}
	if strings.Contains(result, "スキル:") {
		t.Error("orchestrator system prompt should NOT contain skills section")
	}
}

// TestBuildSystemPrompt_OrchestratorSkillsDisabled is no longer needed because
// the orchestrator never injects skills regardless of config. Covered by
// TestBuildSystemPrompt_OrchestratorNoSkillInjection above.

func TestBuildSystemPrompt_NonOrchestratorNoSkillInjection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "maestro.md"), []byte("# Maestro"), 0644); err != nil {
		t.Fatal(err)
	}
	instDir := filepath.Join(dir, "instructions")
	if err := os.MkdirAll(instDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "worker.md"), []byte("# Worker"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create worker-specific skill
	skillDir := filepath.Join(dir, "skills", "worker", "some-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: Some Skill\n---\nBody"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := buildSystemPrompt(dir, "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(result, "スキル:") {
		t.Error("worker system prompt should NOT contain skills section")
	}
}

func TestBuildSystemPrompt_OrchestratorNoSkillsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "maestro.md"), []byte("# Maestro"), 0644); err != nil {
		t.Fatal(err)
	}
	instDir := filepath.Join(dir, "instructions")
	if err := os.MkdirAll(instDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "orchestrator.md"), []byte("# Orchestrator"), 0644); err != nil {
		t.Fatal(err)
	}
	writeConfigYAML(t, dir, true)

	// No skills directory — should still succeed
	result, err := buildSystemPrompt(dir, "orchestrator")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "# Maestro") {
		t.Error("result should contain maestro.md content")
	}
	if strings.Contains(result, "スキル:") {
		t.Error("result should NOT contain skills section when no skills exist")
	}
}

func TestBuildSystemPrompt_OrchestratorNoConfigYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "maestro.md"), []byte("# Maestro"), 0644); err != nil {
		t.Fatal(err)
	}
	instDir := filepath.Join(dir, "instructions")
	if err := os.MkdirAll(instDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "orchestrator.md"), []byte("# Orchestrator"), 0644); err != nil {
		t.Fatal(err)
	}
	// No config.yaml — skills injection should be silently skipped

	orchSkillDir := filepath.Join(dir, "skills", "orchestrator", "orch-skill")
	if err := os.MkdirAll(orchSkillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orchSkillDir, "SKILL.md"), []byte("---\nname: Orch Skill\n---\nBody"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := buildSystemPrompt(dir, "orchestrator")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result, "スキル:") {
		t.Error("should NOT inject skills when config.yaml is missing")
	}
}

func TestBuildLaunchArgs_PlannerDisallowsOperatorOnlyCommands(t *testing.T) {
	args, err := buildLaunchArgs("planner", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	// Planner should have --disallowedTools blocking operator-only commands
	if !strings.Contains(joined, "Bash(maestro plan unquarantine:*)") {
		t.Error("planner disallowedTools should contain unquarantine")
	}

	// resume-merge is intentionally NOT blocked for Planner (hybrid b+c
	// conflict recovery: Planner triggers re-merge after worker resolves)
	if strings.Contains(joined, "Bash(maestro plan resume-merge:*)") {
		t.Error("planner disallowedTools should NOT contain resume-merge (Planner needs it for conflict recovery)")
	}

	// add-retry-task is intentionally NOT blocked for Planner (Planner's
	// standard mechanism for retrying failed tasks)
	if strings.Contains(joined, "Bash(maestro plan add-retry-task:*)") {
		t.Error("planner disallowedTools should NOT contain add-retry-task (Planner needs it for task retry)")
	}
}

func TestBuildLaunchArgs_PlannerAllowsNormalCommands(t *testing.T) {
	args, err := buildLaunchArgs("planner", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	// Planner must still have Bash(maestro:*) in allowedTools
	if !strings.Contains(joined, "Bash(maestro:*)") {
		t.Error("planner should still have Bash(maestro:*) in allowedTools")
	}

	// Normal planner commands (submit, complete) should NOT appear in disallowedTools
	for _, safe := range []string{
		"Bash(maestro plan submit:*)",
		"Bash(maestro plan complete:*)",
	} {
		if strings.Contains(joined, safe) {
			t.Errorf("planner disallowedTools should NOT contain %q", safe)
		}
	}
}

func TestBuildLaunchArgs_OrchestratorNoDisallowedOperatorCommands(t *testing.T) {
	args, err := buildLaunchArgs("orchestrator", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	// Orchestrator should NOT block operator-only commands (it IS the operator)
	for _, sub := range []string{
		"maestro plan unquarantine",
		"maestro plan resume-merge",
		"maestro plan add-retry-task",
	} {
		if strings.Contains(joined, sub) {
			t.Errorf("orchestrator should NOT block %q", sub)
		}
	}
}

func TestBuildLaunchArgs_WorkerDisallowsMaestroReads(t *testing.T) {
	args, err := buildLaunchArgs("worker", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	// Worker should have --disallowedTools containing Read restrictions for .maestro/ control-plane paths
	controlPlanePaths := []string{
		"Read(.maestro/state/**)",
		"Read(.maestro/queue/**)",
		"Read(.maestro/results/**)",
		"Read(.maestro/locks/**)",
		"Read(.maestro/logs/**)",
		"Read(.maestro/config.yaml)",
		"Read(.maestro/dashboard.md)",
	}

	for _, path := range controlPlanePaths {
		if !strings.Contains(joined, path) {
			t.Errorf("worker disallowedTools should contain %q, args: %v", path, args)
		}
	}

	// Verify tmux kill restrictions are still present
	if !strings.Contains(joined, "Bash(tmux kill-server:*)") {
		t.Error("worker should still have tmux kill-server restriction")
	}

	// D009: recovery API escape hatches must be in worker disallowedTools.
	for _, sub := range []string{
		"Bash(maestro plan unquarantine:*)",
		"Bash(maestro plan resume-merge:*)",
		"Bash(maestro resolve-conflict:*)",
	} {
		if !strings.Contains(joined, sub) {
			t.Errorf("worker disallowedTools should contain %q", sub)
		}
	}
}

func TestBuildLaunchArgs_WorkerDoesNotBlockWorktreeReads(t *testing.T) {
	args, err := buildLaunchArgs("worker", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	// Should NOT block .maestro/worktrees/** (workers need access to their worktree files)
	if strings.Contains(joined, "Read(.maestro/worktrees/**") {
		t.Error("worker should NOT have Read(.maestro/worktrees/**) in disallowedTools")
	}
	// Should NOT use a blanket .maestro/** block
	if strings.Contains(joined, "Read(.maestro/**)") {
		t.Error("worker should NOT use blanket Read(.maestro/**) block")
	}
}

func TestBuildLaunchArgs_NotificationDisabledForNonOrchestrator(t *testing.T) {
	tests := []struct {
		role               string
		wantNotificationOff bool
	}{
		{"orchestrator", false},
		{"planner", true},
		// Worker: Notification is disabled via merged --settings in Launch(),
		// NOT in buildLaunchArgs. So buildLaunchArgs output won't have it.
		{"worker", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			args, err := buildLaunchArgs(tt.role, "sonnet", "system-prompt", "")
			if err != nil {
				t.Fatalf("buildLaunchArgs(%s): %v", tt.role, err)
			}
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

func TestBuildLaunchArgs_BasePromptModeReplace(t *testing.T) {
	args, err := buildLaunchArgs("orchestrator", "sonnet", "my-prompt", "replace")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--system-prompt") {
		t.Error("base_prompt_mode=replace should use --system-prompt")
	}
	if strings.Contains(joined, "--append-system-prompt") {
		t.Error("base_prompt_mode=replace should NOT use --append-system-prompt")
	}
}

func TestBuildLaunchArgs_BasePromptModeAppend(t *testing.T) {
	args, err := buildLaunchArgs("worker", "sonnet", "my-prompt", "append")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--append-system-prompt") {
		t.Error("base_prompt_mode=append should use --append-system-prompt")
	}
	// --append-system-prompt contains --system-prompt as substring, so check exact flag
	for _, arg := range args {
		if arg == "--system-prompt" {
			t.Error("base_prompt_mode=append should NOT use --system-prompt")
		}
	}
}

func TestBuildLaunchArgs_BasePromptModeDefault(t *testing.T) {
	args, err := buildLaunchArgs("worker", "sonnet", "my-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--append-system-prompt") {
		t.Error("empty base_prompt_mode should default to --append-system-prompt")
	}
	for _, arg := range args {
		if arg == "--system-prompt" {
			t.Error("empty base_prompt_mode should NOT use --system-prompt")
		}
	}
}

func TestBuildLaunchArgs_UnknownRoleError(t *testing.T) {
	unknownRoles := []string{"admin", "superuser", "root", "unknown", ""}
	for _, role := range unknownRoles {
		t.Run(role, func(t *testing.T) {
			_, err := buildLaunchArgs(role, "sonnet", "system-prompt", "")
			if err == nil {
				t.Errorf("buildLaunchArgs(%q) should return error for unknown role", role)
			}
			if err != nil && !strings.Contains(err.Error(), "unknown role") {
				t.Errorf("error should mention 'unknown role', got: %v", err)
			}
		})
	}
}

func TestBuildLaunchArgs_KnownRolesSucceed(t *testing.T) {
	for _, role := range []string{"orchestrator", "planner", "worker"} {
		t.Run(role, func(t *testing.T) {
			_, err := buildLaunchArgs(role, "sonnet", "system-prompt", "")
			if err != nil {
				t.Errorf("buildLaunchArgs(%q) should succeed, got: %v", role, err)
			}
		})
	}
}

func TestValidTmuxPane(t *testing.T) {
	valid := []string{"%0", "%1", "%123", "%9999"}
	for _, v := range valid {
		if !validTmuxPane.MatchString(v) {
			t.Errorf("validTmuxPane should match %q", v)
		}
	}

	invalid := []string{
		"",
		"0",
		"%",
		"%-1",
		"%abc",
		"$(cmd)",
		"%0; malicious",
		"%0\ninjection",
		"%%0",
		" %0",
		"%0 ",
		"`whoami`",
	}
	for _, v := range invalid {
		if validTmuxPane.MatchString(v) {
			t.Errorf("validTmuxPane should NOT match %q", v)
		}
	}
}

func TestSanitizeForLog(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"normal", "hello", "hello"},
		{"control_chars", "ab\x00cd\x1f\x7f", "ab?cd??"},
		{"newline_tab", "line1\nline2\ttab", "line1?line2?tab"},
		{"truncate", strings.Repeat("a", 150), strings.Repeat("a", 100) + "..."},
		{"empty", "", ""},
		{"unicode", "日本語テスト", "日本語テスト"},
		{"unicode_line_sep", "before\u2028after", "before?after"},
		{"unicode_para_sep", "before\u2029after", "before?after"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeForLog(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeForLog(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildLaunchEnv_CoreBehavior(t *testing.T) {
	base := []string{"HOME=/home/test", "PATH=/usr/bin", "CLAUDECODE=something"}
	env := buildLaunchEnv(base, "worker")

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// CLAUDECODE must be removed.
	if _, ok := envMap["CLAUDECODE"]; ok {
		t.Error("CLAUDECODE should be filtered out")
	}

	// MAESTRO_AGENT_ROLE must be set.
	if v, ok := envMap["MAESTRO_AGENT_ROLE"]; !ok || v != "worker" {
		t.Errorf("MAESTRO_AGENT_ROLE = %q (present=%v), want \"worker\"", v, ok)
	}
}

func TestBuildLaunchEnv_FiltersDangerousEnvVars(t *testing.T) {
	base := []string{
		"HOME=/home/test",
		"PATH=/usr/bin",
		"DYLD_INSERT_LIBRARIES=/evil/lib.dylib",
		"DYLD_LIBRARY_PATH=/evil",
		"LD_PRELOAD=/evil/lib.so",
		"LD_LIBRARY_PATH=/evil",
		"GIT_EXEC_PATH=/evil/git",
		"GIT_DIR=/evil/.git",
		"SAFE_VAR=keep",
	}
	env := buildLaunchEnv(base, "worker")

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// Dangerous env vars must be filtered.
	dangerous := []string{
		"DYLD_INSERT_LIBRARIES",
		"DYLD_LIBRARY_PATH",
		"LD_PRELOAD",
		"LD_LIBRARY_PATH",
		"GIT_EXEC_PATH",
		"GIT_DIR",
	}
	for _, key := range dangerous {
		if _, ok := envMap[key]; ok {
			t.Errorf("%s should be filtered out", key)
		}
	}

	// Safe vars must be preserved.
	safe := []string{"HOME", "PATH", "SAFE_VAR"}
	for _, key := range safe {
		if _, ok := envMap[key]; !ok {
			t.Errorf("%s should be preserved", key)
		}
	}
}

func TestFilterDangerousEnv_PrefixMatching(t *testing.T) {
	tests := []struct {
		name     string
		envVar   string
		filtered bool
	}{
		{"DYLD_INSERT_LIBRARIES", "DYLD_INSERT_LIBRARIES=/evil", true},
		{"DYLD_FORCE_FLAT_NAMESPACE", "DYLD_FORCE_FLAT_NAMESPACE=1", true},
		{"LD_PRELOAD exact", "LD_PRELOAD=/evil.so", true},
		{"LD_PRELOAD_32", "LD_PRELOAD_32=/evil.so", true},
		{"LD_LIBRARY_PATH exact", "LD_LIBRARY_PATH=/evil", true},
		{"LD_LIBRARY_PATH_64", "LD_LIBRARY_PATH_64=/evil", true},
		{"GIT_EXEC_PATH", "GIT_EXEC_PATH=/evil", true},
		{"GIT_DIR", "GIT_DIR=/evil", true},
		{"safe DYLD_unrelated suffix", "MYDYLD_VAR=ok", false},
		{"safe LD_unrelated", "MYLD_PRELOAD=ok", false},
		{"safe GIT_AUTHOR_NAME", "GIT_AUTHOR_NAME=test", false},
		{"HOME", "HOME=/home/user", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterDangerousEnv([]string{tt.envVar})
			if tt.filtered && len(result) != 0 {
				t.Errorf("expected %q to be filtered, but it was kept", tt.envVar)
			}
			if !tt.filtered && len(result) == 0 {
				t.Errorf("expected %q to be kept, but it was filtered", tt.envVar)
			}
		})
	}
}

func TestBuildLaunchEnv_AllRoles(t *testing.T) {
	for _, role := range []string{"orchestrator", "planner", "worker"} {
		t.Run(role, func(t *testing.T) {
			env := buildLaunchEnv([]string{}, role)
			found := false
			for _, e := range env {
				if e == "MAESTRO_AGENT_ROLE="+role {
					found = true
				}
			}
			if !found {
				t.Errorf("role=%s: MAESTRO_AGENT_ROLE=%s not found in env", role, role)
			}
		})
	}
}

func TestLaunchCommand_Format(t *testing.T) {
	if !strings.Contains(LaunchCommand, "maestro agent launch") {
		t.Errorf("LaunchCommand should contain 'maestro agent launch', got %q", LaunchCommand)
	}
}

func TestCurrentPaneTarget_InvalidTmuxPane(t *testing.T) {
	invalid := []string{
		"",
		"0",
		"%",
		"%-1",
		"%abc",
		"$(cmd)",
		"%0; malicious",
	}
	for _, v := range invalid {
		t.Run(v, func(t *testing.T) {
			t.Setenv("TMUX_PANE", v)
			_, err := currentPaneTarget()
			if err == nil {
				t.Errorf("currentPaneTarget() should fail for TMUX_PANE=%q", v)
			}
			if v == "" {
				if !strings.Contains(err.Error(), "not set") {
					t.Errorf("empty TMUX_PANE error should mention 'not set', got: %v", err)
				}
			} else {
				if !strings.Contains(err.Error(), "invalid TMUX_PANE format") {
					t.Errorf("invalid TMUX_PANE error should mention 'invalid TMUX_PANE format', got: %v", err)
				}
			}
		})
	}
}
