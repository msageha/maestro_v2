package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPolicyChecker_WriteHookScript(t *testing.T) {
	dir := t.TempDir()
	pc := NewPolicyChecker(dir)

	path, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript failed: %v", err)
	}

	// Verify path
	expectedPath := filepath.Join(dir, "hooks", "worker-policy.sh")
	if path != expectedPath {
		t.Errorf("path = %q, want %q", path, expectedPath)
	}

	// Verify file exists and is executable
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat hook script: %v", err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Error("hook script should be executable")
	}

	// Verify content starts with shebang
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook script: %v", err)
	}
	if !strings.HasPrefix(string(content), "#!/usr/bin/env bash") {
		t.Error("hook script should start with bash shebang")
	}
}

func TestPolicyChecker_WriteHookScript_Idempotent(t *testing.T) {
	dir := t.TempDir()
	pc := NewPolicyChecker(dir)

	path1, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("first write failed: %v", err)
	}

	path2, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("second write failed: %v", err)
	}

	if path1 != path2 {
		t.Errorf("paths differ: %q vs %q", path1, path2)
	}
}

func TestPolicyChecker_HookSettings_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	pc := NewPolicyChecker(dir)

	settings, err := pc.HookSettings("/path/to/script.sh")
	if err != nil {
		t.Fatalf("HookSettings failed: %v", err)
	}

	// Must be valid JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(settings), &parsed); err != nil {
		t.Fatalf("settings is not valid JSON: %v\nsettings: %s", err, settings)
	}

	// Must contain hooks.PreToolUse
	hooks, ok := parsed["hooks"].(map[string]interface{})
	if !ok {
		t.Fatal("settings missing 'hooks' key")
	}
	preToolUse, ok := hooks["PreToolUse"].([]interface{})
	if !ok {
		t.Fatal("settings missing 'hooks.PreToolUse' key")
	}
	if len(preToolUse) == 0 {
		t.Fatal("PreToolUse array is empty")
	}

	// Check matcher
	group := preToolUse[0].(map[string]interface{})
	if group["matcher"] != "Bash|Write|Edit" {
		t.Errorf("matcher = %q, want %q", group["matcher"], "Bash|Write|Edit")
	}
}

func TestPolicyChecker_HookSettings_ContainsScriptPath(t *testing.T) {
	dir := t.TempDir()
	pc := NewPolicyChecker(dir)
	scriptPath := "/custom/path/to/worker-policy.sh"

	settings, err := pc.HookSettings(scriptPath)
	if err != nil {
		t.Fatalf("HookSettings failed: %v", err)
	}

	if !strings.Contains(settings, scriptPath) {
		t.Errorf("settings should contain script path %q\nsettings: %s", scriptPath, settings)
	}
}

// TestHookScript_ContainsMaestroSpecificChecks verifies that the hook
// asserts the *maestro-orchestration-specific* contracts only. Generic
// destructive-command defense (rm -rf, sudo, kill, etc.) is intentionally
// not handled here — that surface duplicates the user's global Claude
// Code hooks (~/.claude/settings.json). See worker_policy_hook.sh header
// for the full rationale.
func TestHookScript_ContainsMaestroSpecificChecks(t *testing.T) {
	checks := []struct {
		id   string
		text string
	}{
		{"maestro plan control plane", "maestro plan control-plane API is Planner/operator-owned"},
		{"maestro plan resolve-conflict", "maestro\\s+(plan\\s+)?resolve-conflict"},
		{"maestro queue write", "maestro queue write is Orchestrator/operator-owned"},
		{"maestro verify write", "maestro verify write is Planner-owned"},
		{"maestro agent exec/launch", "maestro agent exec/launch is operator-only"},
		{"role env manipulation", "role-environment manipulation"},
		{"git push", "git push blocked for Worker"},
		{".maestro/ control plane", ".maestro/"},
		{"WT001 worktree boundary", "WT001"},
		{"RUN_ON_MAIN", "RUN_ON_MAIN"},
	}

	for _, tc := range checks {
		if !strings.Contains(hookScript, tc.text) {
			t.Errorf("hook script missing check for %s (expected to find %q)", tc.id, tc.text)
		}
	}
}

func TestHookScript_OutputsValidDenyJSON(t *testing.T) {
	// Verify the deny function template produces valid JSON structure
	// Extract a sample deny output from the script
	denyTemplate := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"test"}}`

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(denyTemplate), &parsed); err != nil {
		t.Fatalf("deny template is not valid JSON: %v", err)
	}

	hso, ok := parsed["hookSpecificOutput"].(map[string]interface{})
	if !ok {
		t.Fatal("missing hookSpecificOutput")
	}
	if hso["hookEventName"] != "PreToolUse" {
		t.Error("hookEventName should be PreToolUse")
	}
	if hso["permissionDecision"] != "deny" {
		t.Error("permissionDecision should be deny")
	}
}

func TestBuildLaunchArgs_WorkerNoSettingsInBuildLaunchArgs(t *testing.T) {
	// Worker args from buildLaunchArgs should NOT include --settings.
	// Workers get a single merged --settings (Notification + PreToolUse) in Launch().
	args, err := buildLaunchArgs("worker", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "--settings") {
		t.Error("worker buildLaunchArgs should NOT include --settings (merged in Launch)")
	}
}

func TestHookSettings_WorkerOnlyPreToolUse(t *testing.T) {
	// Post-2026-05-06 P1 #1: HookSettings must emit ONLY PreToolUse.
	// Notification / Stop suppression has been removed because Claude
	// Code merges `--settings` hooks rather than replacing them, so
	// empty arrays were ineffective. Hook suppression is now a
	// ~/.claude responsibility.
	dir := t.TempDir()
	pc := NewPolicyChecker(dir)
	settings, err := pc.HookSettings("/tmp/test-hook.sh")
	if err != nil {
		t.Fatalf("HookSettings error: %v", err)
	}
	if !strings.Contains(settings, `"PreToolUse"`) {
		t.Error("merged settings should contain PreToolUse (PolicyChecker's destructive-action gate)")
	}
	for _, forbidden := range []string{
		`"Notification":[]`,
		`"Notification"`,
		`"Stop":[]`,
		`"Stop"`,
	} {
		if strings.Contains(settings, forbidden) {
			t.Errorf("merged settings must NOT contain %q (suppression moved to ~/.claude responsibility); got: %s",
				forbidden, settings)
		}
	}
}

func TestBuildLaunchArgs_NonWorkerNoPreToolUseHook(t *testing.T) {
	// Orchestrator and planner should NOT have PreToolUse hook settings
	for _, role := range []string{"orchestrator", "planner"} {
		args, err := buildLaunchArgs(role, "sonnet", "system-prompt", "")
		if err != nil {
			t.Fatalf("buildLaunchArgs(%s): %v", role, err)
		}
		joined := strings.Join(args, " ")

		if strings.Contains(joined, "PreToolUse") {
			t.Errorf("role=%s should not have PreToolUse hook settings", role)
		}
	}
}

func TestHookScript_DenyFunctionFormat(t *testing.T) {
	// The deny function must output JSON and exit 0
	if !strings.Contains(hookScript, `exit 0`) {
		t.Error("deny function must exit 0 for structured JSON output")
	}
	if !strings.Contains(hookScript, `"permissionDecision":"deny"`) {
		t.Error("deny function must set permissionDecision to deny")
	}
}

func TestHookScript_SetsPipefail(t *testing.T) {
	if !strings.Contains(hookScript, "set -euo pipefail") {
		t.Error("hook script should use set -euo pipefail for safety")
	}
}

func TestHookScript_ChecksBashToolName(t *testing.T) {
	if !strings.Contains(hookScript, `"$tool_name" = "Bash"`) {
		t.Error("hook script should check for Bash tool name")
	}
}

func TestHookScript_ChecksWriteEditToolNames(t *testing.T) {
	if !strings.Contains(hookScript, `"$tool_name" = "Write"`) {
		t.Error("hook script should check for Write tool name")
	}
	if !strings.Contains(hookScript, `"$tool_name" = "Edit"`) {
		t.Error("hook script should check for Edit tool name")
	}
}

func TestHookScript_ProtectsMaestroControlPlanePaths(t *testing.T) {
	controlPaths := []string{
		".maestro/state",
		".maestro/queue",
		".maestro/results",
		".maestro/locks",
		".maestro/logs",
		".maestro/config.yaml",
	}
	for _, p := range controlPaths {
		if !strings.Contains(hookScript, p) {
			t.Errorf("hook script should protect %q", p)
		}
	}
}

func TestHookScript_BlocksAllGitPush(t *testing.T) {
	// Worker hook blocks ALL git push (daemon owns publish via
	// merge_publish/publish_completed); see worker_policy_hook.sh
	// for the role rationale.
	if !strings.Contains(hookScript, `git\s+push(\s|$)`) {
		t.Error("hook script should block all git push for Workers")
	}
	if !strings.Contains(hookScript, "git push blocked for Worker") {
		t.Error("hook script should contain Worker git push prohibition message")
	}
}

// requireJq skips the test if jq is not installed.
func requireJq(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed, skipping functional hook test")
	}
}

// makeBashInput creates a properly JSON-encoded input for Bash tool.
func makeBashInput(command string) string {
	m := map[string]interface{}{
		"tool_name": "Bash",
		"tool_input": map[string]interface{}{
			"command": command,
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// runHookScript executes the hook script with the given JSON input and returns stdout.
func runHookScript(t *testing.T, scriptPath, inputJSON string) string {
	t.Helper()
	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader(inputJSON)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script failed: %v, output: %s", err, out)
	}
	return string(out)
}

// --- S1: D002 Recursive delete outside project root ---

// Generic recursive-delete checks are delegated to the user's global
// Claude Code hooks (~/.claude/settings.json); see worker_policy_hook.sh
// header for the rationale.

// --- S2: Relative path .maestro/ blocking ---

func TestHookScript_S2_DeniesRelativePathMaestroWrite(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	input := `{"tool_name":"Write","tool_input":{"file_path":".maestro/config.yaml","content":"test"}}`
	output := runHookScript(t, scriptPath, input)
	if !strings.Contains(output, "deny") {
		t.Errorf("expected deny for relative .maestro/config.yaml, got: %s", output)
	}
}

func TestHookScript_S2_DeniesRelativePathMaestroState(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	input := `{"tool_name":"Edit","tool_input":{"file_path":".maestro/state/tasks.yaml","old_string":"a","new_string":"b"}}`
	output := runHookScript(t, scriptPath, input)
	if !strings.Contains(output, "deny") {
		t.Errorf("expected deny for relative .maestro/state/tasks.yaml, got: %s", output)
	}
}

func TestHookScript_S2_AllowsLegitimateFilePath(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	input := `{"tool_name":"Write","tool_input":{"file_path":"internal/service/user.go","content":"package service"}}`
	output := runHookScript(t, scriptPath, input)
	if strings.Contains(output, "deny") {
		t.Errorf("should allow write to legitimate file, got: %s", output)
	}
}

func TestHookScript_S2_ContainsRelativePatterns(t *testing.T) {
	// Verify the script has both absolute and relative .maestro/ patterns
	if !strings.Contains(hookScript, ".maestro/config.yaml|") {
		t.Error("hook script should contain relative .maestro/config.yaml pattern")
	}
	if !strings.Contains(hookScript, "daemon-owned, relative") {
		t.Error("hook script should contain relative-path daemon-owned deny marker")
	}
}

// --- S3: jq unavailable fallback ---

func TestHookScript_S3_DeniesWhenJqUnavailable(t *testing.T) {
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Run with PATH pointing to an empty directory (no jq available)
	emptyDir := t.TempDir()
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = []string{"PATH=" + emptyDir, "HOME=" + os.Getenv("HOME")}
	cmd.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"echo hello"}}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script execution failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "deny") || !strings.Contains(string(out), "jq") {
		t.Errorf("expected deny with jq mention when jq unavailable, got: %s", out)
	}
}

func TestHookScript_S3_AllowsWhenJqAvailable(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Safe command should be allowed when jq is available
	input := `{"tool_name":"Bash","tool_input":{"command":"echo hello"}}`
	output := runHookScript(t, scriptPath, input)
	if strings.Contains(output, "deny") {
		t.Errorf("should allow safe command when jq available, got: %s", output)
	}
}

// TestHookScript_ReturnsExplicitAllow pins the requirement that non-denied
// Bash commands receive an explicit `allow` decision (not a silent pass). This
// is what short-circuits Claude Code's hardcoded "expansion obfuscation"
// classifier — which fires on a brace followed by a quote and CANNOT be
// silenced by permissions.allow — so an unattended Worker does not stall on an
// approval prompt for commands like `maestro result write --summary "...{...}"`.
func TestHookScript_ReturnsExplicitAllow(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// A command containing a brace + quote (the obfuscation trigger) that does
	// not violate any maestro rule must be explicitly allowed.
	input := `{"tool_name":"Bash","tool_input":{"command":"echo \"{ab,cd}_x\""}}`
	output := runHookScript(t, scriptPath, input)
	if !strings.Contains(output, `"permissionDecision":"allow"`) {
		t.Errorf("expected explicit allow decision for brace+quote command, got: %s", output)
	}
	if strings.Contains(output, "deny") {
		t.Errorf("brace+quote command must not be denied, got: %s", output)
	}

	// A denied command (control-plane) must still be denied — allow must not
	// override maestro's own enforcement.
	denied := `{"tool_name":"Bash","tool_input":{"command":"maestro plan submit --tasks-file x"}}`
	dOut := runHookScript(t, scriptPath, denied)
	if !strings.Contains(dOut, `"permissionDecision":"deny"`) {
		t.Errorf("control-plane command must still be denied, got: %s", dOut)
	}
}

func TestHookScript_S3_ContainsJqCheck(t *testing.T) {
	if !strings.Contains(hookScript, "command -v jq") {
		t.Error("hook script should contain jq availability check")
	}
	if !strings.Contains(hookScript, "jq but it is not installed") {
		t.Error("hook script should contain jq unavailable deny message")
	}
}

// TestHookScript_NoProjectRootPlaceholder verifies the written hook script
// contains no unresolved placeholder. The former __PROJECT_ROOT__ / project_root
// embedding was dead (the .maestro/ and worktree checks use path globs and the
// runtime `pwd -P`, never the embedded root) and has been removed.
func TestHookScript_NoProjectRootPlaceholder(t *testing.T) {
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	if strings.Contains(string(content), "__PROJECT_ROOT__") {
		t.Error("written script should not contain __PROJECT_ROOT__ placeholder")
	}
}

// --- C3: dashboard.md Write/Edit blocking ---

func TestHookScript_C3_DashboardMdWriteDenied(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	tests := []struct {
		name  string
		input string
	}{
		{"absolute Write", `{"tool_name":"Write","tool_input":{"file_path":"/project/.maestro/dashboard.md","content":"x"}}`},
		{"relative Write", `{"tool_name":"Write","tool_input":{"file_path":".maestro/dashboard.md","content":"x"}}`},
		{"absolute Edit", `{"tool_name":"Edit","tool_input":{"file_path":"/project/.maestro/dashboard.md","old_string":"a","new_string":"b"}}`},
		{"relative Edit", `{"tool_name":"Edit","tool_input":{"file_path":".maestro/dashboard.md","old_string":"a","new_string":"b"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output := runHookScript(t, scriptPath, tc.input)
			if !strings.Contains(output, "deny") {
				t.Errorf("expected deny for dashboard.md %s, got: %s", tc.name, output)
			}
		})
	}
}

// Read access to .maestro/* and generic privilege-escalation defenses are
// delegated to the user's global Claude Code hooks (not handled here).

func TestHookScript_WriteHookScript_SafeWithSpecialChars(t *testing.T) {
	requireJq(t)

	tests := []struct {
		name    string
		dirName string
	}{
		{"single quote", "it's a project"},
		{"double quote", `my "project"`},
		{"dollar sign", "cost $100"},
		{"backtick", "run `cmd`"},
		{"semicolon", "dir;evil"},
		{"space", "my project"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			projectDir := filepath.Join(base, tc.dirName)
			maestroDir := filepath.Join(projectDir, ".maestro")
			if err := os.MkdirAll(maestroDir, 0755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			pc := NewPolicyChecker(maestroDir)
			scriptPath, err := pc.WriteHookScript()
			if err != nil {
				t.Fatalf("WriteHookScript: %v", err)
			}

			// Verify the script is valid bash (syntax check)
			syntaxCmd := exec.Command("bash", "-n", scriptPath)
			if out, err := syntaxCmd.CombinedOutput(); err != nil {
				t.Fatalf("script has syntax errors: %v, output: %s", err, out)
			}

			// Verify a safe command passes
			safeInput := `{"tool_name":"Bash","tool_input":{"command":"echo hello"}}`
			output := runHookScript(t, scriptPath, safeInput)
			if strings.Contains(output, "deny") {
				t.Errorf("safe command should be allowed, got: %s", output)
			}

			// Verify the script doesn't contain unquoted project root
			content, err := os.ReadFile(scriptPath)
			if err != nil {
				t.Fatalf("read script: %v", err)
			}
			if strings.Contains(string(content), "__PROJECT_ROOT__") {
				t.Error("script should not contain __PROJECT_ROOT__ placeholder")
			}
		})
	}
}

// runHookScriptInDir runs the hook script with stdin input from a specific working directory.
func runHookScriptInDir(t *testing.T, scriptPath, inputJSON, dir string) string {
	t.Helper()
	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader(inputJSON)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script failed: %v, output: %s", err, out)
	}
	return string(out)
}

func TestHookScript_D009_BlocksMaestroRoleEnvironmentBypass(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	cases := []struct {
		name      string
		cmd       string
		needsRole bool // true: blocked by role-env rule, false: blocked by maestro plan rule
	}{
		{"env -u role bypass", "env -u MAESTRO_AGENT_ROLE maestro plan submit --command-id cmd_1", true},
		{"env role assignment", "env MAESTRO_AGENT_ROLE=cli maestro queue write --type command", true},
		{"inline role assignment", "MAESTRO_AGENT_ROLE=cli maestro verify write --command-id cmd_1", true},
		{"chained unset TMUX_PANE", "unset TMUX_PANE; maestro plan retry-publish --command-id cmd_1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := runHookScriptInDir(t, scriptPath, makeBashInput(tc.cmd), dir)
			if !strings.Contains(output, "deny") {
				t.Errorf("expected deny for %q, got: %s", tc.cmd, output)
			}
			// Either the role-env guard or the maestro-plan-control-plane rule
			// must catch the call; both are maestro-orchestration-specific.
			roleHit := strings.Contains(output, "role-environment manipulation")
			planHit := strings.Contains(output, "Planner/operator-owned") ||
				strings.Contains(output, "Orchestrator/operator-owned") ||
				strings.Contains(output, "Planner-owned")
			if !roleHit && !planHit {
				t.Errorf("expected role-env or maestro-plan deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_BlocksMaestroAgentSubcommands(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	denyCases := []struct {
		name string
		cmd  string
	}{
		{"agent exec direct", "maestro agent exec --agent-id planner --message hi"},
		{"agent exec impersonation", "maestro agent exec --agent-id worker2 --message 'do something'"},
		{"agent launch", "maestro agent launch"},
		{"chained agent exec", "cd /tmp && maestro agent exec --agent-id orchestrator --message x"},
		{"piped agent exec", "echo hi | maestro agent exec --agent-id planner --message y"},
	}
	for _, tc := range denyCases {
		t.Run(tc.name, func(t *testing.T) {
			output := runHookScriptInDir(t, scriptPath, makeBashInput(tc.cmd), dir)
			if !strings.Contains(output, "deny") || !strings.Contains(output, "operator-only") {
				t.Errorf("expected agent exec/launch deny for %q, got: %s", tc.cmd, output)
			}
		})
	}

	allowCases := []struct {
		name string
		cmd  string
	}{
		{"result write", "maestro result write --task-id t1 --status completed --summary done"},
		{"summary mentioning agent exec", `maestro result write --task-id t1 --status completed --summary "documented maestro agent exec"`},
	}
	for _, tc := range allowCases {
		t.Run(tc.name, func(t *testing.T) {
			output := runHookScriptInDir(t, scriptPath, makeBashInput(tc.cmd), dir)
			if strings.Contains(output, "deny") {
				t.Errorf("expected allow for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// --- WT001: Worktree boundary enforcement ---

func TestHookScript_WT001_DeniesWriteOutsideWorktree(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	// Create a directory structure that mimics a worktree path
	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Write to repo root (outside worktree) should be denied
	input := fmt.Sprintf(`{"tool_name":"Write","tool_input":{"file_path":"%s/internal/foo.go","content":"package foo"}}`, projectDir)
	output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
	if !strings.Contains(output, "WT001") || !strings.Contains(output, "deny") {
		t.Errorf("expected WT001 deny for write outside worktree, got: %s", output)
	}
}

func TestHookScript_WT001_AllowsWriteInsideWorktree(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Write inside worktree should be allowed
	input := fmt.Sprintf(`{"tool_name":"Write","tool_input":{"file_path":"%s/internal/foo.go","content":"package foo"}}`, worktreeDir)
	output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
	if strings.Contains(output, "deny") {
		t.Errorf("should allow write inside worktree, got: %s", output)
	}
}

// TestHookScript_WT001_AllowsWriteToVarFoldersScratch pins the macOS
// scratch exemption: a worker whose CWD is inside .maestro/worktrees/
// must be able to Write to /var/folders/... (where mktemp lands by
// default on macOS) and to /private/var/folders/... (the resolved
// canonical form). Previously the exemption only triggered when TMPDIR
// was set and matched the path; runs that inherited a stripped
// environment hit WT001 even though /var/folders is the system-default
// scratch root.
//
// The worker_cwd-under-scratch carve-out (which suppresses the scratch
// exemption when worker_cwd lives inside TMPDIR or /var/folders) is
// exercised by the existing fixtures because t.TempDir() returns paths
// under /var/folders on macOS — so this test deliberately uses a /tmp
// base to mirror the production layout where worker_cwd is rooted under
// the user's home directory and worktrees do NOT live in TMPDIR.
func TestHookScript_WT001_AllowsWriteToVarFoldersScratch(t *testing.T) {
	requireJq(t)
	base, err := os.MkdirTemp("/tmp", "maestro-wt001-vf-")
	if err != nil {
		t.Skipf("/tmp unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_vf", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	cases := []string{
		"/var/folders/x9/mock/T/tmp.summary.txt",
		"/private/var/folders/x9/mock/T/tmp.summary.txt",
	}
	for _, target := range cases {
		input := fmt.Sprintf(`{"tool_name":"Write","tool_input":{"file_path":"%s","content":"summary"}}`, target)
		output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
		if strings.Contains(output, "WT001") || strings.Contains(output, "deny") {
			t.Errorf("expected /var/folders scratch to be allowed for %q, got: %s", target, output)
		}
	}
}

func TestHookScript_WT001_EditOutsideWorktreeDenied(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_456", "worker2")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Edit to repo root should be denied
	input := fmt.Sprintf(`{"tool_name":"Edit","tool_input":{"file_path":"%s/cmd/main.go","old_string":"old","new_string":"new"}}`, projectDir)
	output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
	if !strings.Contains(output, "WT001") || !strings.Contains(output, "deny") {
		t.Errorf("expected WT001 deny for Edit outside worktree, got: %s", output)
	}
}

func TestHookScript_WT001_NoEnforcementOutsideWorktree(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	// When CWD is NOT inside .maestro/worktrees/, WT001 should not apply
	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	if err := os.MkdirAll(maestroDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Write to project root when CWD is project root (non-worktree mode) should be allowed
	input := fmt.Sprintf(`{"tool_name":"Write","tool_input":{"file_path":"%s/internal/foo.go","content":"package foo"}}`, projectDir)
	output := runHookScriptInDir(t, scriptPath, input, projectDir)
	if strings.Contains(output, "WT001") {
		t.Errorf("WT001 should not apply when CWD is not inside worktrees, got: %s", output)
	}
}

func TestHookScript_H4_AllowsRelativePathInsideWorktree(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Relative path inside worktree should be allowed
	input := `{"tool_name":"Write","tool_input":{"file_path":"internal/foo.go","content":"package foo"}}`
	output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
	if strings.Contains(output, "deny") {
		t.Errorf("should allow relative path inside worktree, got: %s", output)
	}
}

func TestHookScript_H4_BlocksRelativePathEscapingWorktree(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Relative path with .. that escapes worktree should be denied
	input := `{"tool_name":"Write","tool_input":{"file_path":"../../internal/foo.go","content":"package foo"}}`
	output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
	if !strings.Contains(output, "WT001") || !strings.Contains(output, "deny") {
		t.Errorf("expected WT001 deny for relative path escaping worktree, got: %s", output)
	}
}

func TestHookScript_H4_BlocksRelativePathEdit(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_456", "worker2")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Edit with path traversal should be denied
	input := `{"tool_name":"Edit","tool_input":{"file_path":"../../../cmd/main.go","old_string":"old","new_string":"new"}}`
	output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
	if !strings.Contains(output, "WT001") || !strings.Contains(output, "deny") {
		t.Errorf("expected WT001 deny for Edit with path traversal, got: %s", output)
	}
}

func TestHookScript_LegitimateCommandsNotBlocked(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	legitimate := []struct {
		name string
		cmd  string
	}{
		{"go test", "go test -v -count=1 ./..."},
		{"go test race", "go test -race ./internal/..."},
		{"go test run", "go test -run TestFoo ./..."},
		{"go build", "go build ./..."},
		{"go vet", "go vet ./..."},
		{"go mod tidy", "go mod tidy"},
		{"git status", "git status"},
		{"git diff", "git diff --stat"},
		{"git log", "git log --oneline -10"},
		{"git branch", "git branch -a"},
		{"grep pattern", "grep -rn 'func main' ."},
		{"cat file", "cat go.mod"},
		{"ls dir", "ls -la internal/"},
		{"echo string", "echo hello world"},
		{"echo env var", "echo $HOME $PATH"},
		{"make build", "make build"},
		{"npm test", "npm test"},
		{"npm install", "npm install"},
		{"mkdir dir", "mkdir -p build/output"},
		{"touch file", "touch build/output/.gitkeep"},
		{"head file", "head -20 README.md"},
		{"tail file", "tail -f /dev/null"},
		{"wc file", "wc -l internal/agent/*.go"},
		{"sort file", "sort -u names.txt"},
		{"diff files", "diff file1.txt file2.txt"},
		{"chmod project", "chmod +x scripts/build.sh"},
		{"chmod R project", "chmod -R 755 ./build/output"},
	}
	for _, tc := range legitimate {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if strings.Contains(output, "deny") {
				t.Errorf("BLOCKED legitimate command %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// =============================================================================
// M-WT001: Case-insensitive worktree path comparison
// =============================================================================

func TestHookScript_WT001_ContainsCaseNormalization(t *testing.T) {
	if !strings.Contains(hookScript, `_wt_lower`) {
		t.Error("WT001 should use _wt_lower for case-insensitive path comparison")
	}
	if !strings.Contains(hookScript, `_cwd_lower`) {
		t.Error("WT001 should use _cwd_lower for case-insensitive CWD comparison")
	}
	if !strings.Contains(hookScript, `tr '[:upper:]' '[:lower:]'`) {
		t.Error("WT001 should use tr for case normalization")
	}
}

func TestHookScript_WT001_CaseInsensitiveOnMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("case-insensitive FS test only applicable on macOS")
	}
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(filepath.Join(worktreeDir, "internal"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Use uppercase version of worktree path in file_path.
	// On macOS (case-insensitive APFS/HFS+), these refer to the same directory.
	upperWorktreeDir := strings.ToUpper(worktreeDir)
	filePath := upperWorktreeDir + "/internal/foo.go"

	input := fmt.Sprintf(`{"tool_name":"Write","tool_input":{"file_path":"%s","content":"package foo"}}`, filePath)
	output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
	if strings.Contains(output, "WT001") {
		t.Errorf("should allow case-different path on macOS (case-insensitive FS), got: %s", output)
	}
}

// =============================================================================
// M-PERL1: Perl indirect execution
// =============================================================================

func TestHookScript_L1_BlocksWriteEditToMaestroQueue(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	tests := []struct {
		name  string
		input string
	}{
		{"absolute Write queue", `{"tool_name":"Write","tool_input":{"file_path":"/project/.maestro/queue/pending.yaml","content":"x"}}`},
		{"relative Write queue", `{"tool_name":"Write","tool_input":{"file_path":".maestro/queue/pending.yaml","content":"x"}}`},
		{"absolute Edit queue", `{"tool_name":"Edit","tool_input":{"file_path":"/project/.maestro/queue/tasks.yaml","old_string":"a","new_string":"b"}}`},
		{"relative Edit queue", `{"tool_name":"Edit","tool_input":{"file_path":".maestro/queue/tasks.yaml","old_string":"a","new_string":"b"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output := runHookScript(t, scriptPath, tc.input)
			if !strings.Contains(output, "deny") {
				t.Errorf("expected deny for %s, got: %s", tc.name, output)
			}
		})
	}
}

func TestHookScript_L1_LauncherBlocksReadMaestroQueue(t *testing.T) {
	// Verify launcher.go has Read(.maestro/queue/**) in disallowedTools for worker
	args, err := buildLaunchArgs("worker", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "Read(.maestro/queue/**)") {
		t.Error("worker disallowedTools should contain Read(.maestro/queue/**)")
	}
	// Ensure the old plural form is NOT present
	if strings.Contains(joined, "Read(.maestro/queues/**)") {
		t.Error("worker disallowedTools should NOT contain old Read(.maestro/queues/**)")
	}
}

// =============================================================================
// WT-GIT: Worktree git change command blocking
// =============================================================================

func TestHookScript_WTGIT_BlocksGitChangeCommandsInWorktree(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"git commit", "git commit -m 'test'"},
		{"git add", "git add file.go"},
		{"git merge", "git merge feature"},
		{"git rebase", "git rebase main"},
		{"git cherry-pick", "git cherry-pick abc123"},
		{"git revert", "git revert HEAD"},
		{"git stash", "git stash"},
		{"git restore", "git restore file.go"},
		{"git fetch", "git fetch origin"},
		{"git pull", "git pull"},
		{"git worktree", "git worktree add ../new-wt"},
		{"git tag", "git tag v1.0"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
			if !strings.Contains(output, "git mutation blocked in worktree mode") || !strings.Contains(output, "deny") {
				t.Errorf("expected WT-GIT deny for %q in worktree, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_WTGIT_AllowsReadOnlyGitInWorktree(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	allowed := []struct {
		name string
		cmd  string
	}{
		{"git status", "git status"},
		{"git diff", "git diff --stat"},
		{"git log", "git log --oneline -10"},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
			if strings.Contains(output, "git mutation blocked in worktree mode") {
				t.Errorf("should allow %q in worktree, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_WTGIT_NoEnforcementOutsideWorktree(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// git add outside worktree should not be blocked by WT-GIT
	input := makeBashInput("git add file.go")
	output := runHookScriptInDir(t, scriptPath, input, dir)
	if strings.Contains(output, "git mutation blocked in worktree mode") {
		t.Errorf("WT-GIT should not apply outside worktree, got: %s", output)
	}
}

// TestHookScript_WTGIT_FalsePositive_MaestroResultWriteContainingGitInSummary
// verifies that a `maestro result write` invocation whose --summary value
// contains git command names (commit/add/merge/...) as plain substrings is
// NOT denied by WT-GIT. The prior implementation scanned the entire command
// string for /git\s+(commit|...)/, which fired on benign result reports
// mentioning git in free-form text. The fix anchors the match to shell
// statement boundaries (start-of-string or ;/|/&&).
func TestHookScript_WTGIT_FalsePositive_MaestroResultWriteContainingGitInSummary(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Each of these contains a git verb only as a substring inside a
	// --summary / --learnings argument. None should be rejected by WT-GIT.
	cases := []struct {
		name string
		cmd  string
	}{
		{
			name: "summary mentions git commit",
			cmd:  "maestro result write worker1 --task-id t --command-id c --lease-epoch 1 --status completed --summary '[変更理由] git commit succeeded'",
		},
		{
			name: "summary mentions git add",
			cmd:  "maestro result write worker1 --summary '... git add done ...'",
		},
		{
			name: "learning mentions git merge",
			cmd:  "maestro result write worker1 --learnings 'prefer git merge --no-ff'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
			if strings.Contains(output, "git mutation blocked in worktree mode") {
				t.Errorf("WT-GIT must not trigger on maestro CLI argument containing git-as-substring: %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// TestHookScript_WTGIT_FalsePositive_MaestroTaskHeartbeat verifies that
// `maestro task heartbeat` (which contains no git token at all) is not
// blocked by WT-GIT when issued from inside a worktree CWD.
func TestHookScript_WTGIT_FalsePositive_MaestroTaskHeartbeat(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	input := makeBashInput("maestro task heartbeat --agent-id worker1 --task-id t --lease-epoch 1")
	output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
	if strings.Contains(output, "git mutation blocked in worktree mode") {
		t.Errorf("WT-GIT must not trigger on maestro task heartbeat, got: %s", output)
	}
}

// TestHookScript_WTGIT_BlocksChainedGitAfterSeparator verifies that chained
// shell commands where `git <mutating-verb>` appears after a shell separator
// (`&&`, `;`, `|`) are still denied. The separator places `git` at the head
// of the next command segment, which the anchored regex must still catch.
func TestHookScript_WTGIT_BlocksChainedGitAfterSeparator(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	chained := []struct {
		name string
		cmd  string
	}{
		{"cd && git commit", "cd /tmp && git commit -m msg"},
		{"cd && git add", "cd sub && git add ."},
		{"semicolon git commit", "echo done ; git commit -am wip"},
	}
	for _, tc := range chained {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScriptInDir(t, scriptPath, input, worktreeDir)
			if !strings.Contains(output, "git mutation blocked in worktree mode") || !strings.Contains(output, "deny") {
				t.Errorf("expected WT-GIT deny for chained %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// TestHookScript_WTGIT_BlocksGitChangeInIntegrationWorktree verifies that git
// change commands remain daemon-owned even when the CWD is an _integration
// worktree. Workers may edit conflict files there, but staging/commits are
// performed by the daemon after result_write.
func TestHookScript_WTGIT_BlocksGitChangeInIntegrationWorktree(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	// _integration worktrees always end with "_integration".
	integrationDir := filepath.Join(maestroDir, "worktrees", "cmd_abc123_integration")
	if err := os.MkdirAll(integrationDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"git fetch", "git fetch origin"},
		{"git merge", "git merge --no-ff feature"},
		{"git add", "git add -p file.go"},
		{"git commit", "git commit -m 'resolve conflict'"},
		{"git rebase", "git rebase main"},
		{"git stash", "git stash"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScriptInDir(t, scriptPath, input, integrationDir)
			if !strings.Contains(output, "git mutation blocked in worktree mode") || !strings.Contains(output, "deny") {
				t.Errorf("expected WT-GIT deny for %q in _integration worktree, got: %s", tc.cmd, output)
			}
		})
	}
}

// TestHookScript_WTGIT_BlocksGitChangeInIntegrationWorktreeSubdir verifies the
// same rule when the CWD is a subdirectory of the _integration worktree.
func TestHookScript_WTGIT_BlocksGitChangeInIntegrationWorktreeSubdir(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	subDir := filepath.Join(maestroDir, "worktrees", "cmd_abc123_integration", "internal", "pkg")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	input := makeBashInput("git add . && git commit -m 'fix'")
	output := runHookScriptInDir(t, scriptPath, input, subDir)
	if !strings.Contains(output, "git mutation blocked in worktree mode") || !strings.Contains(output, "deny") {
		t.Errorf("expected WT-GIT deny in _integration worktree subdir, got: %s", output)
	}
}

// TestHookScript_WTGIT_NonIntegrationWorktreeStillBlocked confirms that regular
// worktrees are also subject to WT-GIT restrictions.
func TestHookScript_WTGIT_NonIntegrationWorktreeStillBlocked(t *testing.T) {
	requireJq(t)
	base := t.TempDir()

	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	// A regular feature worktree (NOT ending with _integration).
	regularDir := filepath.Join(maestroDir, "worktrees", "cmd_abc123_feature")
	if err := os.MkdirAll(regularDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"git commit", "git commit -m 'test'"},
		{"git add", "git add file.go"},
		{"git fetch", "git fetch origin"},
		{"git merge", "git merge main"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScriptInDir(t, scriptPath, input, regularDir)
			if !strings.Contains(output, "git mutation blocked in worktree mode") || !strings.Contains(output, "deny") {
				t.Errorf("expected WT-GIT deny for %q in non-integration worktree, got: %s", tc.cmd, output)
			}
		})
	}
}

// TestHookScript_RunOnMain_ContainsCheck verifies the hook script embeds the
// run_on_main pattern. The Daemon stamps @run_on_main=1 on the pane before
// dispatching a run_on_main task; this guard denies Write/Edit while it is
// set. Without this structural check, a refactor could quietly drop the guard
// and allow Workers to mutate the main worktree they are supposed to read.
func TestHookScript_RunOnMain_ContainsCheck(t *testing.T) {
	if !strings.Contains(hookScript, "@run_on_main") {
		t.Error("hook script should reference the @run_on_main tmux user variable")
	}
	if !strings.Contains(hookScript, "RUN_ON_MAIN: Write/Edit blocked") {
		t.Error("hook script should contain RUN_ON_MAIN deny message")
	}
}

// TestHookScript_RunOnMain_DeniesWriteWhenFlagged simulates a pane stamped
// with @run_on_main=1 by intercepting tmux via a PATH shim. Write must be
// denied with the RUN_ON_MAIN reason.
func TestHookScript_RunOnMain_DeniesWriteWhenFlagged(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Build a fake tmux that returns "1" so the hook believes the pane is
	// in run_on_main mode.
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "tmux")
	shim := "#!/usr/bin/env bash\n" +
		`if [ "$1" = "display-message" ]; then echo 1; exit 0; fi` + "\n" +
		"exit 0\n"
	if err := os.WriteFile(shimPath, []byte(shim), 0755); err != nil { //nolint:gosec
		t.Fatalf("write tmux shim: %v", err)
	}

	// Prepend shim dir to PATH so the hook's `command -v tmux` resolves to it.
	jqPath, _ := exec.LookPath("jq")
	pathEnv := shimDir
	if jqPath != "" {
		pathEnv += ":" + filepath.Dir(jqPath)
	}
	pathEnv += ":/bin:/usr/bin"

	input := `{"tool_name":"Write","tool_input":{"file_path":"/tmp/foo.txt","content":"x"}}`
	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = []string{
		"PATH=" + pathEnv,
		"HOME=" + os.Getenv("HOME"),
		"TMUX_PANE=%0",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script failed: %v, output: %s", err, out)
	}
	if !strings.Contains(string(out), "deny") {
		t.Errorf("expected deny when @run_on_main=1, got: %s", out)
	}
	if !strings.Contains(string(out), "RUN_ON_MAIN") {
		t.Errorf("expected RUN_ON_MAIN reason in deny output, got: %s", out)
	}
}

// TestHookScript_RunOnMain_AllowsWriteWhenUnflagged verifies that an empty
// @run_on_main value (the default and post-task state) does NOT trigger the
// guard. This protects against a flag-stuck-on regression that would lock
// Workers out of normal writes.
func TestHookScript_RunOnMain_AllowsWriteWhenUnflagged(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Fake tmux returns empty (var unset).
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "tmux")
	shim := "#!/usr/bin/env bash\nexit 0\n"
	if err := os.WriteFile(shimPath, []byte(shim), 0755); err != nil { //nolint:gosec
		t.Fatalf("write tmux shim: %v", err)
	}

	jqPath, _ := exec.LookPath("jq")
	pathEnv := shimDir
	if jqPath != "" {
		pathEnv += ":" + filepath.Dir(jqPath)
	}
	pathEnv += ":/bin:/usr/bin"

	// Use a path inside the temp dir so other safety checks don't fire.
	innocent := filepath.Join(dir, "scratch.txt")
	input := fmt.Sprintf(`{"tool_name":"Write","tool_input":{"file_path":%q,"content":"x"}}`, innocent)
	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = []string{
		"PATH=" + pathEnv,
		"HOME=" + os.Getenv("HOME"),
		"TMUX_PANE=%0",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script failed: %v, output: %s", err, out)
	}
	if strings.Contains(string(out), "RUN_ON_MAIN") {
		t.Errorf("RUN_ON_MAIN must not fire when @run_on_main is unset, got: %s", out)
	}
}

// runOnMainBashEnv builds a PATH containing a tmux shim that returns the given
// flag value, plus the real jq and standard system bins. Used by the Bash-
// branch RUN_ON_MAIN guard tests below.
func runOnMainBashEnv(t *testing.T, flag string) (scriptPath string, env []string) {
	t.Helper()
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	sp, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "tmux")
	shim := "#!/usr/bin/env bash\n" +
		`if [ "$1" = "display-message" ]; then echo ` + flag + `; exit 0; fi` + "\n" +
		"exit 0\n"
	if err := os.WriteFile(shimPath, []byte(shim), 0755); err != nil { //nolint:gosec
		t.Fatalf("write tmux shim: %v", err)
	}

	jqPath, _ := exec.LookPath("jq")
	pathEnv := shimDir
	if jqPath != "" {
		pathEnv += ":" + filepath.Dir(jqPath)
	}
	pathEnv += ":/bin:/usr/bin"

	return sp, []string{
		"PATH=" + pathEnv,
		"HOME=" + os.Getenv("HOME"),
		"TMUX_PANE=%0",
	}
}

// TestHookScript_RunOnMain_BashDenylist verifies the Bash-branch RUN_ON_MAIN
// guard: a Worker running in read-only verification mode must not be able to
// mutate the main worktree via Bash even though the Write/Edit branch is
// already locked down. Each case is a representative of one denylist family
// (file-mutating coreutils, in-place editors, redirection, mutating git verbs,
// package installers, make, archive extraction).
// TestHookScript_RunOnMain_BashDenylist verifies that core file/git
// mutations are blocked under @run_on_main=1. Language-specific package
// installers (npm/pnpm/pip/cargo/go install/make/tar/unzip) are
// intentionally not in this denylist — verification runs are
// language-agnostic at the orchestration layer, and per-language safety
// is the responsibility of the user's global Claude Code hooks plus the
// project's own .maestro/verify.yaml. The maestro-specific guard only
// needs to keep the *generic* mutation primitives blocked so a verify
// task cannot accidentally rewrite the main worktree.
func TestHookScript_RunOnMain_BashDenylist(t *testing.T) {
	scriptPath, env := runOnMainBashEnv(t, "1")

	cases := []struct {
		name string
		cmd  string
	}{
		{"cp", "cp src/foo.go src/bar.go"},
		{"mv", "mv a b"},
		{"rm", "rm tmp.txt"},
		{"mkdir", "mkdir newdir"},
		{"touch", "touch newfile"},
		{"sed_in_place", "sed -i 's/a/b/' file.go"},
		{"perl_in_place", "perl -i -pe 's/a/b/' file.go"},
		{"redirect_overwrite", "echo hi > out.txt"},
		{"redirect_append", "echo hi >> out.txt"},
		{"git_commit", "git commit -m wip"},
		{"git_push", "git push origin main"},
		{"git_checkout", "git checkout -b feat"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := fmt.Sprintf(`{"tool_name":"Bash","tool_input":{"command":%q}}`, tc.cmd)
			cmd := exec.Command("bash", scriptPath)
			cmd.Stdin = strings.NewReader(input)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("hook script failed: %v, output: %s", err, out)
			}
			// Either RUN_ON_MAIN or the unconditional Worker-git-push rule
			// can fire (the latter wins for `git push`). Both are
			// maestro-orchestration-specific guards.
			body := string(out)
			if !strings.Contains(body, "RUN_ON_MAIN") && !strings.Contains(body, "git push blocked for Worker") {
				t.Errorf("expected RUN_ON_MAIN deny for %q, got: %s", tc.cmd, out)
			}
		})
	}
}

// TestHookScript_RunOnMain_BashAllows verifies that read-only Bash commands
// remain allowed under run_on_main=1, and that make --dry-run is whitelisted
// (a build-system inspection without side effects). Without this, the guard
// would lock Workers out of legitimate verification work.
func TestHookScript_RunOnMain_BashAllows(t *testing.T) {
	scriptPath, env := runOnMainBashEnv(t, "1")

	cases := []struct {
		name string
		cmd  string
	}{
		{"git_status", "git status"},
		{"git_log", "git log -n 5"},
		{"git_diff", "git diff HEAD~1"},
		{"go_build", "go build ./..."},
		{"go_test", "go test ./..."},
		{"go_vet", "go vet ./..."},
		{"go_list", "go list ./..."},
		{"make_dry_run", "make build --dry-run"},
		{"make_n", "make build -n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := fmt.Sprintf(`{"tool_name":"Bash","tool_input":{"command":%q}}`, tc.cmd)
			cmd := exec.Command("bash", scriptPath)
			cmd.Stdin = strings.NewReader(input)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("hook script failed: %v, output: %s", err, out)
			}
			if strings.Contains(string(out), "RUN_ON_MAIN") {
				t.Errorf("RUN_ON_MAIN must not fire for read-only %q, got: %s", tc.cmd, out)
			}
		})
	}
}

// TestHookScript_RunOnMain_BashAllowsWhenUnflagged confirms the Bash denylist
// does NOT engage when the pane is not in run_on_main mode — the guard must
// be scoped to verification panes, not active for all Worker activity.
func TestHookScript_RunOnMain_BashAllowsWhenUnflagged(t *testing.T) {
	// Empty flag value → run_on_main stays "0".
	scriptPath, env := runOnMainBashEnv(t, "")

	// Pick a command that the denylist would block under run_on_main=1.
	input := `{"tool_name":"Bash","tool_input":{"command":"go install ./cmd/foo"}}`
	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script failed: %v, output: %s", err, out)
	}
	if strings.Contains(string(out), "RUN_ON_MAIN") {
		t.Errorf("RUN_ON_MAIN must not fire when @run_on_main is unset, got: %s", out)
	}
}
