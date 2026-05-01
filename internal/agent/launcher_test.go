package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestEnsureRoleMaestroWrapper_PATHFallback pins the 2026-04-28 fix for the
// "in-tree build artifact disappears mid-run" failure mode. Earlier the
// generated wrapper script `exec`'d the absolute path resolved at startup
// (e.g. /repo/maestro). When that path was removed by a rebuild or
// worktree rotation, every subsequent CLI call from the agent died with
// shell exit 126/127. The wrapper now tests the absolute path with `[ -x
// ... ]` first and falls back to PATH-resolved `maestro` (the launching
// binary's directory is already prepended to PATH by buildLaunchEnv, so
// the same binary is picked when both are valid). This test reads the
// generated wrapper script and asserts the structural pieces are present;
// integration coverage is provided by the full agent E2E run.
func TestEnsureRoleMaestroWrapper_PATHFallback(t *testing.T) {
	maestroDir := t.TempDir()
	for _, role := range []string{"orchestrator", "planner", "worker"} {
		role := role
		t.Run(role, func(t *testing.T) {
			wrapperDir, err := ensureRoleMaestroWrapper(maestroDir, role)
			if err != nil {
				t.Fatalf("ensureRoleMaestroWrapper(%q): %v", role, err)
			}
			body, err := os.ReadFile(filepath.Join(wrapperDir, "maestro"))
			if err != nil {
				t.Fatalf("read wrapper script: %v", err)
			}
			script := string(body)

			// The absolute-path branch must still be tried first to preserve
			// version-skew protection for the common case where the binary is
			// stable across the agent's lifetime.
			if !strings.Contains(script, `if [ -x "$maestro_exec_path" ]; then`) {
				t.Errorf("wrapper missing absolute-path probe; script=\n%s", script)
			}
			if !strings.Contains(script, `exec "$maestro_exec_path" "$@"`) {
				t.Errorf("wrapper missing absolute-path exec; script=\n%s", script)
			}

			// PATH fallback branch — required for Issue 1 recovery.
			if !strings.Contains(script, `exec maestro "$@"`) {
				t.Errorf("wrapper missing PATH-resolved fallback exec; script=\n%s", script)
			}

			// The fallback must announce itself so operators can correlate
			// the recovery with a rebuild / worktree rotation.
			if !strings.Contains(script, "[maestro role wrapper]") {
				t.Errorf("wrapper missing operator-visible warning prefix; script=\n%s", script)
			}
			if !strings.Contains(script, "no longer exists") {
				t.Errorf("wrapper missing 'no longer exists' marker on fallback; script=\n%s", script)
			}

			// The role and MAESTRO_DIR exports must still be set on every
			// path so the fallback binary inherits the same trust boundary
			// and config root as the absolute-path exec.
			if !strings.Contains(script, "export MAESTRO_AGENT_ROLE=") {
				t.Errorf("wrapper missing MAESTRO_AGENT_ROLE export; script=\n%s", script)
			}
			if !strings.Contains(script, "export MAESTRO_DIR=") {
				t.Errorf("wrapper missing MAESTRO_DIR export; script=\n%s", script)
			}
		})
	}
}

func TestAllowedToolsByRole(t *testing.T) {
	tests := []struct {
		role      string
		wantTools []string
		wantOK    bool
	}{
		{"orchestrator", []string{"Bash(maestro queue write planner --type command:*)", "Bash(maestro skill list:*)", "Bash(maestro plan request-cancel:*)", "Read(.maestro/dashboard.md)", "Read(.maestro/results/planner.yaml)", "Read(.maestro/config.yaml)", "Read(.maestro/state/continuous.yaml)"}, true},
		{"planner", []string{"Bash(maestro:*)", "Read(.maestro/**)", "Read(.maestro/dashboard.md)", "Read(.maestro/config.yaml)", "Read(.maestro/results/*)"}, true},
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

	// Must have narrow maestro Bash patterns, not bare "Bash" or Bash(maestro:*).
	if strings.Contains(joined, ",Bash,") || joined == "Bash" || strings.HasPrefix(joined, "Bash,") {
		t.Error("orchestrator must use narrow maestro Bash patterns, not bare Bash")
	}
	if strings.Contains(joined, "Bash(maestro:*)") {
		t.Error("orchestrator must not have broad Bash(maestro:*)")
	}
	for _, want := range []string{
		"Bash(maestro queue write planner --type command:*)",
		"Bash(maestro skill list:*)",
		"Bash(maestro plan request-cancel:*)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("orchestrator must have %s", want)
		}
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
		"Read(.maestro/results/planner.yaml)",
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
	for _, want := range []string{
		"Read(.maestro/dashboard.md)",
		"Read(.maestro/config.yaml)",
		"Read(.maestro/results/*)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("planner must have %q", want)
		}
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

func TestBuildLaunchArgs_OrchestratorDelegationSurfaceOnly(t *testing.T) {
	args, err := buildLaunchArgs("orchestrator", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	for _, allowed := range []string{
		"Bash(maestro queue write planner --type command:*)",
		"Bash(maestro skill list:*)",
		"Bash(maestro plan request-cancel:*)",
	} {
		if !strings.Contains(joined, allowed) {
			t.Errorf("orchestrator allowedTools missing %q", allowed)
		}
	}
	if strings.Contains(joined, "Bash(maestro:*)") {
		t.Errorf("orchestrator must not allow broad maestro CLI access: %s", joined)
	}
	for _, blockedByOmission := range []string{
		"Bash(maestro plan unquarantine:*)",
		"Bash(maestro plan resume-merge:*)",
		"Bash(maestro plan add-retry-task:*)",
	} {
		if strings.Contains(joined, blockedByOmission) {
			t.Errorf("orchestrator should not include %q in allowedTools", blockedByOmission)
		}
	}
}

func TestBuildLaunchArgs_DangerousPermissionBypassForAllManagedRoles(t *testing.T) {
	for _, role := range []string{"orchestrator", "planner", "worker"} {
		t.Run(role, func(t *testing.T) {
			args, err := buildLaunchArgs(role, "sonnet", "system-prompt", "")
			if err != nil {
				t.Fatalf("buildLaunchArgs: %v", err)
			}
			if !slices.Contains(args, dangerousPermissionBypassFlag) {
				t.Fatalf("role=%s missing %s; args=%v", role, dangerousPermissionBypassFlag, args)
			}
		})
	}
}

func TestAppendWorkspaceReadAllowances_PlannerAddsAbsoluteMaestroDir(t *testing.T) {
	maestroDir := filepath.Join(t.TempDir(), ".maestro")
	if err := os.MkdirAll(maestroDir, 0o755); err != nil {
		t.Fatalf("mkdir maestro dir: %v", err)
	}
	args, err := buildLaunchArgs("planner", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	args = appendWorkspaceReadAllowances(args, maestroDir, "planner")

	allowedTools := launchArgValue(t, args, "--allowedTools")
	for _, want := range []string{
		"Read(" + filepath.ToSlash(filepath.Join(maestroDir, "dashboard.md")) + ")",
		"Read(" + filepath.ToSlash(filepath.Join(maestroDir, "config.yaml")) + ")",
		"Read(" + filepath.ToSlash(filepath.Join(maestroDir, "results")) + "/*)",
		"Read(" + filepath.ToSlash(maestroDir) + "/**)",
	} {
		if !strings.Contains(allowedTools, want) {
			t.Fatalf("allowedTools missing %q: %s", want, allowedTools)
		}
	}
}

func TestAppendWorkspaceReadAllowances_OrchestratorAddsExactAbsoluteFiles(t *testing.T) {
	maestroDir := filepath.Join(t.TempDir(), ".maestro")
	if err := os.MkdirAll(filepath.Join(maestroDir, "results"), 0o755); err != nil {
		t.Fatalf("mkdir maestro results dir: %v", err)
	}
	args, err := buildLaunchArgs("orchestrator", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	args = appendWorkspaceReadAllowances(args, maestroDir, "orchestrator")

	allowedTools := launchArgValue(t, args, "--allowedTools")
	for _, rel := range []string{
		"dashboard.md",
		"results/planner.yaml",
		"config.yaml",
		"state/continuous.yaml",
	} {
		want := "Read(" + filepath.ToSlash(filepath.Join(maestroDir, rel)) + ")"
		if !strings.Contains(allowedTools, want) {
			t.Fatalf("allowedTools missing %q: %s", want, allowedTools)
		}
	}
}

func TestAppendWorkspaceReadAllowances_ResolvesSymlinkedMaestroDir(t *testing.T) {
	realDir := filepath.Join(t.TempDir(), ".maestro")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real maestro dir: %v", err)
	}
	linkDir := filepath.Join(t.TempDir(), "maestro-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	args, err := buildLaunchArgs("planner", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	args = appendWorkspaceReadAllowances(args, linkDir, "planner")

	allowedTools := launchArgValue(t, args, "--allowedTools")
	resolvedRealDir, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("resolve real maestro dir: %v", err)
	}
	want := "Read(" + filepath.ToSlash(resolvedRealDir) + "/**)"
	if !strings.Contains(allowedTools, want) {
		t.Fatalf("allowedTools missing resolved path %q: %s", want, allowedTools)
	}
}

func TestAppendResolvedMaestroBashAllowances_Orchestrator(t *testing.T) {
	args, err := buildLaunchArgs("orchestrator", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	args = appendResolvedMaestroBashAllowances(args, "orchestrator", "/tmp/current/maestro")

	allowedTools := launchArgValue(t, args, "--allowedTools")
	for _, want := range []string{
		"Bash(/tmp/current/maestro queue write planner --type command:*)",
		"Bash(/tmp/current/maestro skill list:*)",
		"Bash(/tmp/current/maestro plan request-cancel:*)",
	} {
		if !strings.Contains(allowedTools, want) {
			t.Fatalf("allowedTools missing %q: %s", want, allowedTools)
		}
	}
}

func TestAppendResolvedMaestroBashAllowances_Planner(t *testing.T) {
	args, err := buildLaunchArgs("planner", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	args = appendResolvedMaestroBashAllowances(args, "planner", "/tmp/current/maestro")

	allowedTools := launchArgValue(t, args, "--allowedTools")
	want := "Bash(/tmp/current/maestro:*)"
	if !strings.Contains(allowedTools, want) {
		t.Fatalf("allowedTools missing %q: %s", want, allowedTools)
	}
}

func TestAppendRuntimeCLIPathInstruction(t *testing.T) {
	maestroDir := t.TempDir()
	got := appendRuntimeCLIPathInstruction("base prompt", "/tmp/current/maestro", maestroDir)
	if !strings.Contains(got, "/tmp/current/maestro") {
		t.Fatalf("runtime CLI path instruction missing path: %s", got)
	}
	if !strings.Contains(got, filepath.ToSlash(maestroDir)) {
		t.Fatalf("runtime CLI path instruction missing maestro dir: %s", got)
	}
	if !strings.Contains(got, "Do not rely on the shell `PATH`") {
		t.Fatalf("runtime CLI path instruction missing PATH warning: %s", got)
	}
}

func launchArgValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	t.Fatalf("flag %s not found in args: %v", flag, args)
	return ""
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
	// Both the current `plan resolve-conflict` and the legacy
	// `resolve-conflict` spellings are blocked as defense-in-depth.
	for _, sub := range []string{
		"Bash(maestro plan unquarantine:*)",
		"Bash(maestro plan resume-merge:*)",
		"Bash(maestro plan resolve-conflict:*)",
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
		role                string
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

func TestBuildLaunchEnvForAgent_PrependsRoleWrapper(t *testing.T) {
	maestroDir := t.TempDir()
	env, err := buildLaunchEnvForAgent([]string{"PATH=/usr/bin"}, "orchestrator", maestroDir)
	if err != nil {
		t.Fatalf("buildLaunchEnvForAgent: %v", err)
	}

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	wrapperDir := filepath.Join(maestroDir, "bin", "roles", "orchestrator")
	pathParts := strings.Split(envMap["PATH"], string(os.PathListSeparator))
	if len(pathParts) == 0 || pathParts[0] != wrapperDir {
		t.Fatalf("PATH first entry = %q, want %q (PATH=%q)", pathParts[0], wrapperDir, envMap["PATH"])
	}
	if envMap["MAESTRO_DIR"] == "" {
		t.Fatal("MAESTRO_DIR must be set for launched agents")
	}

	wrapperPath := filepath.Join(wrapperDir, "maestro")
	info, err := os.Stat(wrapperPath)
	if err != nil {
		t.Fatalf("stat wrapper: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("wrapper is not executable: mode=%v", info.Mode().Perm())
	}
	content, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("read wrapper: %v", err)
	}
	body := string(content)
	if !strings.Contains(body, "export MAESTRO_AGENT_ROLE='orchestrator'") {
		t.Fatalf("wrapper missing role export:\n%s", body)
	}
	if !strings.Contains(body, "export MAESTRO_DIR=") {
		t.Fatalf("wrapper missing MAESTRO_DIR export:\n%s", body)
	}
	if !strings.Contains(body, "exec ") || !strings.Contains(body, " \"$@\"") {
		t.Fatalf("wrapper must exec real maestro with original args:\n%s", body)
	}
}

func TestBuildLaunchEnvForAgent_RejectsUnknownRole(t *testing.T) {
	_, err := buildLaunchEnvForAgent([]string{"PATH=/usr/bin"}, "admin", t.TempDir())
	if err == nil {
		t.Fatal("expected unknown role error")
	}
	if !strings.Contains(err.Error(), "unknown role") {
		t.Fatalf("expected unknown role error, got: %v", err)
	}
}

func TestShellSingleQuote(t *testing.T) {
	got := shellSingleQuote("/tmp/a'b/maestro")
	want := "'/tmp/a'\"'\"'b/maestro'"
	if got != want {
		t.Fatalf("shellSingleQuote() = %q, want %q", got, want)
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

// TestResolvedLaunchCommandFor_PrependsMaestroDirEnv pins the 2026-04-29 fix:
// when the formation re-launches `maestro agent launch` in a worker pane that
// sits inside a worktree, the command must carry MAESTRO_DIR so that
// findMaestroDir takes the env-var branch instead of falling back to a cwd
// walk-up that can stop at a partial worktree-local .maestro/ fragment.
func TestResolvedLaunchCommandFor_PrependsMaestroDirEnv(t *testing.T) {
	dir := t.TempDir()
	got := ResolvedLaunchCommandFor(dir)
	if !strings.Contains(got, "MAESTRO_DIR=") {
		t.Errorf("expected MAESTRO_DIR= prefix in launch command, got %q", got)
	}
	if !strings.Contains(got, "agent launch") {
		t.Errorf("expected `agent launch` suffix, got %q", got)
	}
	canonical, err := canonicalMaestroDir(dir)
	if err != nil {
		t.Fatalf("canonicalMaestroDir: %v", err)
	}
	wantSubstr := "MAESTRO_DIR=" + shellSingleQuote(canonical)
	if !strings.Contains(got, wantSubstr) {
		t.Errorf("expected canonical maestroDir in MAESTRO_DIR assignment %q, got %q", wantSubstr, got)
	}
}

// TestResolvedLaunchCommandFor_FallsBackOnEmptyMaestroDir keeps backward
// compatibility for tests / edge paths that pass an empty maestroDir: the
// command must still launch the agent rather than embedding `MAESTRO_DIR=”`.
func TestResolvedLaunchCommandFor_FallsBackOnEmptyMaestroDir(t *testing.T) {
	got := ResolvedLaunchCommandFor("")
	if strings.Contains(got, "MAESTRO_DIR=") {
		t.Errorf("did not expect MAESTRO_DIR= when maestroDir is empty, got %q", got)
	}
	if !strings.Contains(got, "agent launch") {
		t.Errorf("expected `agent launch` in fallback command, got %q", got)
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

// TestBuildLaunchEnvForAgent_DefaultsGOCACHE pins the 2026-04-28 retest2
// fix for the Worker `go build` permission failure inside claude-code's
// sandbox. The Worker hit "permission denied" against the default
// ~/Library/Caches/go-build path on every fresh run. The launcher now
// defaults GOCACHE to a project-local path so go-toolchain commands run
// under the agent never fall back to a sandboxed user-cache directory
// that's likely to be denied. Operator overrides via an inherited
// `GOCACHE=...` export must still win, otherwise CI/dev shell setups
// that share a global cache would lose their cache reuse silently.
func TestBuildLaunchEnvForAgent_DefaultsGOCACHE(t *testing.T) {
	t.Run("default_points_under_maestro_dir", func(t *testing.T) {
		maestroDir := t.TempDir()
		env, err := buildLaunchEnvForAgent([]string{"PATH=/usr/bin"}, "worker", maestroDir)
		if err != nil {
			t.Fatalf("buildLaunchEnvForAgent: %v", err)
		}
		envMap := envSliceToMap(env)
		want := filepath.Join(envMap["MAESTRO_DIR"], "cache", "go-build")
		if envMap["GOCACHE"] != want {
			t.Errorf("GOCACHE = %q, want %q", envMap["GOCACHE"], want)
		}
	})

	t.Run("operator_override_wins", func(t *testing.T) {
		maestroDir := t.TempDir()
		env, err := buildLaunchEnvForAgent(
			[]string{"PATH=/usr/bin", "GOCACHE=/shared/go-build"},
			"worker", maestroDir)
		if err != nil {
			t.Fatalf("buildLaunchEnvForAgent: %v", err)
		}
		if got := envSliceToMap(env)["GOCACHE"]; got != "/shared/go-build" {
			t.Errorf("GOCACHE = %q, want operator override /shared/go-build", got)
		}
	})
}

func envSliceToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			m[parts[0]] = parts[1]
		}
	}
	return m
}

// TestBuildLaunchEnvForAgent_NormalizesNoisyEnv pins the 2026-04-28 retest3
// environment-noise suppression. The retest reported two issues in agent
// panes that both stem from inherited env:
//   - TERM=dumb caused starship to spam "unable to determine terminal
//     type" into the CLI/pane output;
//   - the default ~/.cache/mise path was sandbox-denied so mise emitted
//     "Operation not permitted" cache-write WARNs every shell init.
//
// Both noise sources are configurable via env, so the launcher now
// normalises them when no operator override is present. Operators that
// pin TERM or MISE_CACHE_DIR in their dev shell still win — explicit
// inheritance must beat our defaults.
func TestBuildLaunchEnvForAgent_NormalizesNoisyEnv(t *testing.T) {
	t.Run("term_dumb_promoted_to_xterm", func(t *testing.T) {
		maestroDir := t.TempDir()
		env, err := buildLaunchEnvForAgent([]string{"PATH=/usr/bin", "TERM=dumb"}, "worker", maestroDir)
		if err != nil {
			t.Fatalf("buildLaunchEnvForAgent: %v", err)
		}
		if got := envSliceToMap(env)["TERM"]; got != "xterm-256color" {
			t.Errorf("TERM = %q, want \"xterm-256color\" so starship can render", got)
		}
	})

	t.Run("term_unset_gets_default", func(t *testing.T) {
		maestroDir := t.TempDir()
		env, err := buildLaunchEnvForAgent([]string{"PATH=/usr/bin"}, "worker", maestroDir)
		if err != nil {
			t.Fatalf("buildLaunchEnvForAgent: %v", err)
		}
		if got := envSliceToMap(env)["TERM"]; got != "xterm-256color" {
			t.Errorf("TERM = %q, want \"xterm-256color\" default", got)
		}
	})

	t.Run("term_real_value_preserved", func(t *testing.T) {
		maestroDir := t.TempDir()
		env, err := buildLaunchEnvForAgent([]string{"PATH=/usr/bin", "TERM=screen-256color"}, "worker", maestroDir)
		if err != nil {
			t.Fatalf("buildLaunchEnvForAgent: %v", err)
		}
		if got := envSliceToMap(env)["TERM"]; got != "screen-256color" {
			t.Errorf("TERM = %q, want operator-supplied value preserved", got)
		}
	})

	t.Run("mise_cache_dir_under_maestro", func(t *testing.T) {
		maestroDir := t.TempDir()
		env, err := buildLaunchEnvForAgent([]string{"PATH=/usr/bin"}, "worker", maestroDir)
		if err != nil {
			t.Fatalf("buildLaunchEnvForAgent: %v", err)
		}
		envMap := envSliceToMap(env)
		want := filepath.Join(envMap["MAESTRO_DIR"], "cache", "mise")
		if envMap["MISE_CACHE_DIR"] != want {
			t.Errorf("MISE_CACHE_DIR = %q, want %q", envMap["MISE_CACHE_DIR"], want)
		}
	})

	t.Run("mise_cache_operator_override_wins", func(t *testing.T) {
		maestroDir := t.TempDir()
		env, err := buildLaunchEnvForAgent(
			[]string{"PATH=/usr/bin", "MISE_CACHE_DIR=/shared/mise-cache"},
			"worker", maestroDir)
		if err != nil {
			t.Fatalf("buildLaunchEnvForAgent: %v", err)
		}
		if got := envSliceToMap(env)["MISE_CACHE_DIR"]; got != "/shared/mise-cache" {
			t.Errorf("MISE_CACHE_DIR = %q, want operator override preserved", got)
		}
	})
}
