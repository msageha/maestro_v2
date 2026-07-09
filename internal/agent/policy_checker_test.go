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

	settings, err := pc.HookSettings("/path/to/script.sh", "worker")
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
	if group["matcher"] != ".*" {
		t.Errorf("matcher = %q, want %q", group["matcher"], ".*")
	}
}

func TestPolicyChecker_HookSettings_CommandsIncludeQuotedPathAndRole(t *testing.T) {
	dir := t.TempDir()
	pc := NewPolicyChecker(dir)
	scriptPath := "/custom/path/it' has spaces; $HOME/worker-policy.sh"

	for _, role := range []string{"worker", "planner", "orchestrator"} {
		t.Run(role, func(t *testing.T) {
			settings, err := pc.HookSettings(scriptPath, role)
			if err != nil {
				t.Fatalf("HookSettings failed: %v", err)
			}

			var parsed hookSettingsJSON
			if err := json.Unmarshal([]byte(settings), &parsed); err != nil {
				t.Fatalf("settings is not valid JSON: %v\nsettings: %s", err, settings)
			}
			if got := parsed.Hooks.PreToolUse[0].Hooks[0].Command; got != shellQuote(scriptPath)+" "+role {
				t.Errorf("command = %q, want %q", got, shellQuote(scriptPath)+" "+role)
			}
		})
	}
}

func TestPolicyChecker_HookSettings_SandboxFilesystemAllowWrite(t *testing.T) {
	dir := t.TempDir()
	pc := NewPolicyChecker(dir)
	settings, err := pc.HookSettings("/tmp/test-hook.sh", "worker")
	if err != nil {
		t.Fatalf("HookSettings failed: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(settings), &parsed); err != nil {
		t.Fatalf("settings is not valid JSON: %v\nsettings: %s", err, settings)
	}
	if jsonObjectHasKey(parsed, "enabled") {
		t.Fatalf("HookSettings must not emit sandbox.enabled or any enabled key, got: %s", settings)
	}

	sandbox, ok := parsed["sandbox"].(map[string]any)
	if !ok {
		t.Fatalf("settings missing sandbox object: %s", settings)
	}
	filesystem, ok := sandbox["filesystem"].(map[string]any)
	if !ok {
		t.Fatalf("settings missing sandbox.filesystem object: %s", settings)
	}
	rawAllowWrite, ok := filesystem["allowWrite"].([]any)
	if !ok {
		t.Fatalf("settings missing sandbox.filesystem.allowWrite array: %s", settings)
	}
	var allowWrite []string
	for _, raw := range rawAllowWrite {
		v, ok := raw.(string)
		if !ok {
			t.Fatalf("allowWrite contains non-string value %T: %v", raw, raw)
		}
		allowWrite = append(allowWrite, v)
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	want := []string{filepath.Join(absDir, "cache")}
	if resolved, err := filepath.EvalSymlinks(absDir); err == nil && resolved != absDir {
		want = append(want, filepath.Join(resolved, "cache"))
	}
	want = append(want, "~/.cache", "~/Library/Caches")

	if len(allowWrite) != len(want) {
		t.Fatalf("allowWrite length = %d, want %d\nallowWrite=%v\nwant=%v", len(allowWrite), len(want), allowWrite, want)
	}
	for i := range want {
		if allowWrite[i] != want[i] {
			t.Errorf("allowWrite[%d] = %q, want %q", i, allowWrite[i], want[i])
		}
	}
}

func jsonObjectHasKey(v any, key string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == key {
				return true
			}
			if jsonObjectHasKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if jsonObjectHasKey(child, key) {
				return true
			}
		}
	}
	return false
}

func TestPolicyChecker_HookSettings_InvalidRole(t *testing.T) {
	dir := t.TempDir()
	pc := NewPolicyChecker(dir)
	if _, err := pc.HookSettings("/tmp/test-hook.sh", "bogus"); err == nil {
		t.Fatal("HookSettings should reject invalid role")
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
		{"maestro plan resolve-conflict", "maestro resolve-conflict is operator-only"},
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

func TestBuildLaunchArgs_NoSettingsInBuildLaunchArgs(t *testing.T) {
	// buildLaunchArgs stays pure. Claude-code roles get their policy --settings
	// later in the launch path via applyAgentPolicy.
	for _, role := range []string{"worker", "planner", "orchestrator"} {
		t.Run(role, func(t *testing.T) {
			args, err := buildLaunchArgs(role, "sonnet", "system-prompt", "")
			if err != nil {
				t.Fatalf("buildLaunchArgs: %v", err)
			}
			joined := strings.Join(args, " ")

			if strings.Contains(joined, "--settings") {
				t.Errorf("%s buildLaunchArgs should NOT include --settings (added in Launch)", role)
			}
		})
	}
}

func TestHookSettings_PreToolUseOnly(t *testing.T) {
	// Post-2026-05-06 P1 #1: HookSettings must emit ONLY PreToolUse.
	// Notification / Stop suppression has been removed because Claude
	// Code merges `--settings` hooks rather than replacing them, so
	// empty arrays were ineffective. Hook suppression is now a
	// ~/.claude responsibility.
	dir := t.TempDir()
	pc := NewPolicyChecker(dir)
	settings, err := pc.HookSettings("/tmp/test-hook.sh", "worker")
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

func TestApplyAgentPolicy_AllClaudeRolesGetPreToolUseHook(t *testing.T) {
	for _, role := range []string{"worker", "planner", "orchestrator"} {
		role := role
		t.Run(role, func(t *testing.T) {
			maestroDir := filepath.Join(t.TempDir(), ".maestro")
			args, err := buildLaunchArgs(role, "sonnet", "system-prompt", "")
			if err != nil {
				t.Fatalf("buildLaunchArgs(%s): %v", role, err)
			}
			args, err = applyAgentPolicy(maestroDir, role, args)
			if err != nil {
				t.Fatalf("applyAgentPolicy(%s): %v", role, err)
			}

			settings := launchArgValue(t, args, "--settings")
			if !strings.Contains(settings, `"PreToolUse"`) {
				t.Fatalf("role=%s should have PreToolUse hook settings; args=%v", role, args)
			}
			if !strings.Contains(settings, `"matcher":".*"`) {
				t.Fatalf("role=%s settings should match all tools; settings=%s", role, settings)
			}
			if !strings.Contains(settings, "worker-policy.sh' "+role) {
				t.Fatalf("role=%s settings should invoke worker-policy.sh with role argument; settings=%s", role, settings)
			}
		})
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
func runHookScript(t *testing.T, scriptPath, inputJSON string, scriptArgs ...string) string {
	t.Helper()
	args := append([]string{scriptPath}, scriptArgs...)
	cmd := exec.Command("bash", args...)
	cmd.Stdin = strings.NewReader(inputJSON)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script failed: %v, output: %s", err, out)
	}
	return string(out)
}

type decodedHookOutput struct {
	HookSpecificOutput struct {
		PermissionDecision string         `json:"permissionDecision"`
		UpdatedInput       map[string]any `json:"updatedInput"`
	} `json:"hookSpecificOutput"`
}

func decodeHookOutput(t *testing.T, out string) decodedHookOutput {
	t.Helper()
	var decoded decodedHookOutput
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("invalid hook JSON %q: %v", out, err)
	}
	return decoded
}

func assertHookUnsandboxed(t *testing.T, out, wantCommand string) {
	t.Helper()
	decoded := decodeHookOutput(t, out)
	if decoded.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("decision = %q, want allow; output=%s", decoded.HookSpecificOutput.PermissionDecision, out)
	}
	ui := decoded.HookSpecificOutput.UpdatedInput
	if ui == nil {
		t.Fatalf("expected updatedInput with dangerouslyDisableSandbox, got none: %s", out)
	}
	if v, ok := ui["dangerouslyDisableSandbox"].(bool); !ok || !v {
		t.Fatalf("updatedInput.dangerouslyDisableSandbox = %v, want true", ui["dangerouslyDisableSandbox"])
	}
	if got, _ := ui["command"].(string); got != wantCommand {
		t.Fatalf("updatedInput.command = %q, want original command %q", got, wantCommand)
	}
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

func TestHookScript_PlannerRolePolicy(t *testing.T) {
	requireJq(t)
	base := t.TempDir()
	projectDir := filepath.Join(base, "project")
	maestroDir := filepath.Join(projectDir, ".maestro")
	worktreeDir := filepath.Join(maestroDir, "worktrees", "cmd_123", "worker1")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	allowBash := []struct {
		name string
		cmd  string
	}{
		{"plan submit", "maestro plan submit --tasks-file -"},
		{"queue write", "maestro queue write planner --type command --command-id cmd_1"},
	}
	for _, tc := range allowBash {
		t.Run(tc.name, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(tc.cmd), "planner")
			if !strings.Contains(out, `"permissionDecision":"allow"`) || strings.Contains(out, `"permissionDecision":"deny"`) {
				t.Errorf("planner command %q should be allowed, got: %s", tc.cmd, out)
			}
		})
	}

	denyBash := []struct {
		name string
		cmd  string
		want string
	}{
		{"git push", "git push origin main", `"permissionDecision":"deny"`},
		{"role env manipulation", "env -u MAESTRO_AGENT_ROLE maestro plan submit --tasks-file -", "role-environment manipulation"},
		{".maestro redirect", "echo x > .maestro/state/x", ".maestro/ control-plane"},
	}
	for _, tc := range denyBash {
		t.Run(tc.name, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(tc.cmd), "planner")
			if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, tc.want) {
				t.Errorf("planner command %q should be denied with %q, got: %s", tc.cmd, tc.want, out)
			}
		})
	}

	writeMaestro := `{"tool_name":"Write","tool_input":{"file_path":".maestro/state/x","content":"x"}}`
	out := runHookScript(t, scriptPath, writeMaestro, "planner")
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("planner Write to .maestro/state should be denied, got: %s", out)
	}

	// Fix D: planner never mutates files — the role-scoped deny fires
	// before any path-based rule, so WT001 must not be the reason.
	outsideWorktree := fmt.Sprintf(`{"tool_name":"Write","tool_input":{"file_path":%q,"content":"package foo"}}`, filepath.Join(projectDir, "internal", "foo.go"))
	out = runHookScriptInDir(t, scriptPath, outsideWorktree, worktreeDir, "planner")
	if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "not permitted for planner/orchestrator") || strings.Contains(out, "WT001") {
		t.Errorf("planner Write should be denied by the role rule (not WT001), got: %s", out)
	}
}

func TestHookScript_OrchestratorRolePolicy(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	out := runHookScript(t, scriptPath, makeBashInput("maestro queue write planner --type command --command-id cmd_1"), "orchestrator")
	if !strings.Contains(out, `"permissionDecision":"allow"`) || strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("orchestrator queue write should be allowed, got: %s", out)
	}

	out = runHookScript(t, scriptPath, makeBashInput("git push origin main"), "orchestrator")
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("orchestrator git push should be denied, got: %s", out)
	}
}

func TestHookScript_UnknownRoleFallsBackToWorkerPolicy(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	out := runHookScript(t, scriptPath, makeBashInput("maestro plan submit --tasks-file -"), "bogus")
	if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "Planner/operator-owned") {
		t.Errorf("unknown role should fall back to worker policy and deny plan submit, got: %s", out)
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
func runHookScriptInDir(t *testing.T, scriptPath, inputJSON, dir string, scriptArgs ...string) string {
	t.Helper()
	args := append([]string{scriptPath}, scriptArgs...)
	cmd := exec.Command("bash", args...)
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
		{"env role assignment before subshell maestro", "env MAESTRO_AGENT_ROLE=cli sh -c 'maestro queue write x'", true},
		{"inline role assignment before bash -c maestro", `MAESTRO_AGENT_ROLE=cli bash -c "maestro plan submit"`, true},
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

	output := runHookScriptInDir(t, scriptPath, makeBashInput(`maestro result write --summary "role stuff"`), dir)
	decoded := decodeHookOutput(t, output)
	if decoded.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("legitimate result write mentioning role text should allow, got: %s", output)
	}
	if strings.Contains(output, "role-environment manipulation") {
		t.Fatalf("role-env guard must not fire without role-env manipulation, got: %s", output)
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

func TestHookScript_P3_BlocksProtectedConfigBashWrites(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	tests := []struct {
		name            string
		role            string
		cmd             string
		wantDecision    string
		wantUnsandboxed bool
	}{
		{
			name:         "vscode redirect",
			cmd:          "echo x > .vscode/settings.json",
			wantDecision: "deny",
		},
		{
			name:         "vscode clobber redirect",
			cmd:          "echo x >| .vscode/settings.json",
			wantDecision: "deny",
		},
		{
			name:         "idea append redirect",
			cmd:          "echo x >> ./.idea/workspace.xml",
			wantDecision: "deny",
		},
		{
			name:         "vscode dd of target",
			cmd:          "dd if=/dev/null of=.vscode/x",
			wantDecision: "deny",
		},
		{
			name:         "vscode sed in place",
			cmd:          "sed -i 's/a/b/' .vscode/x",
			wantDecision: "deny",
		},
		{
			name:         "vscode gsed in place",
			cmd:          "gsed -i 's/a/b/' .vscode/x",
			wantDecision: "deny",
		},
		{
			name:         "claude perl in place",
			cmd:          "perl -pi -e 's/a/b/' .claude/x",
			wantDecision: "deny",
		},
		{
			name:         "idea truncate target",
			cmd:          "truncate -s0 .idea/workspace.xml",
			wantDecision: "deny",
		},
		{
			name:         "claude tee",
			cmd:          "echo x | tee .claude/settings.json",
			wantDecision: "deny",
		},
		{
			name:         "codex cp",
			cmd:          "cp foo .codex/config.toml",
			wantDecision: "deny",
		},
		{
			name:         "nested gemini redirect",
			cmd:          "printf x > sub/pkg/.gemini/y",
			wantDecision: "deny",
		},
		{
			name:         "git hooks redirect",
			cmd:          "echo x > .git/hooks/pre-commit",
			wantDecision: "deny",
		},
		{
			name:         "vscode read allowed",
			cmd:          "cat .vscode/settings.json",
			wantDecision: "allow",
		},
		{
			name:         "vscode sed read allowed",
			cmd:          "sed -n p .vscode/x",
			wantDecision: "allow",
		},
		{
			name:            "protected path as maestro summary data",
			cmd:             `maestro result write --summary "I edited .vscode/settings.json"`,
			wantDecision:    "allow",
			wantUnsandboxed: true,
		},
		{
			name:         "vscodex is not vscode",
			cmd:          "echo x > .vscodex/foo",
			wantDecision: "allow",
		},
		{
			name:         "non dot vscode name allowed",
			cmd:          "echo x > src/vscode-thing.txt",
			wantDecision: "allow",
		},
		{
			name:         "planner vscode redirect",
			role:         "planner",
			cmd:          "echo x > .vscode/settings.json",
			wantDecision: "deny",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			role := tc.role
			if role == "" {
				role = "worker"
			}
			out := runHookScript(t, scriptPath, makeBashInput(tc.cmd), role)
			decoded := decodeHookOutput(t, out)
			if decoded.HookSpecificOutput.PermissionDecision != tc.wantDecision {
				t.Fatalf("decision = %q, want %q; output=%s", decoded.HookSpecificOutput.PermissionDecision, tc.wantDecision, out)
			}
			if tc.wantDecision == "deny" {
				if !strings.Contains(out, "protected-path Bash write blocked") {
					t.Fatalf("deny reason should explain protected-path fast-fail, got: %s", out)
				}
				if decoded.HookSpecificOutput.UpdatedInput != nil {
					t.Fatalf("denied protected-path command must not carry updatedInput, got: %s", out)
				}
				return
			}
			if strings.Contains(out, "protected-path Bash write blocked") {
				t.Fatalf("allowed command must not trip protected-path rule, got: %s", out)
			}
			if tc.wantUnsandboxed {
				assertHookUnsandboxed(t, out, tc.cmd)
			} else if decoded.HookSpecificOutput.UpdatedInput != nil {
				t.Fatalf("command should be plain allow without updatedInput, got: %s", out)
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

// TestHookScript_RunOnMain_DeniesMultiEditAndNotebookEdit covers the
// matched-but-unhandled regression: the hook matcher also fires for
// MultiEdit/NotebookEdit, and the script used to fall through to the
// final allow() for any tool_name other than the exact strings
// Bash/Write/Edit — bypassing the run_on_main read-only mode entirely.
func TestHookScript_RunOnMain_DeniesMultiEditAndNotebookEdit(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "tmux")
	shim := "#!/usr/bin/env bash\n" +
		`if [ "$1" = "display-message" ]; then echo 1; exit 0; fi` + "\n" +
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

	inputs := map[string]string{
		"MultiEdit":    `{"tool_name":"MultiEdit","tool_input":{"file_path":"/tmp/foo.txt","edits":[{"old_string":"a","new_string":"b"}]}}`,
		"NotebookEdit": `{"tool_name":"NotebookEdit","tool_input":{"notebook_path":"/tmp/foo.ipynb","new_source":"x"}}`,
	}
	for toolName, input := range inputs {
		cmd := exec.Command("bash", scriptPath)
		cmd.Stdin = strings.NewReader(input)
		cmd.Env = []string{
			"PATH=" + pathEnv,
			"HOME=" + os.Getenv("HOME"),
			"TMUX_PANE=%0",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: hook script failed: %v, output: %s", toolName, err, out)
		}
		if !strings.Contains(string(out), "deny") || !strings.Contains(string(out), "RUN_ON_MAIN") {
			t.Errorf("%s: expected RUN_ON_MAIN deny when @run_on_main=1, got: %s", toolName, out)
		}
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

// --- SBX: package-manager OS-sandbox escape (allow + updatedInput) ---
//
// Claude Code's built-in Bash sandbox write-denies `**/.vscode/**` (and
// sibling IDE/VCS config globs) at every depth, so package managers
// extracting a dependency that ships a `.vscode/` directory fail with
// EPERM mid-install. The hook rewrites such commands to run unsandboxed
// BEFORE execution (allow + updatedInput.dangerouslyDisableSandbox) —
// the post-failure retry path wedges on an approval prompt when managed
// settings disable bypassPermissions. See worker_policy_hook.sh.

func TestHookScript_SBX_PackageManagerCommandsRewrittenUnsandboxed(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	commands := []string{
		"pnpm install",
		"pnpm install --frozen-lockfile",
		"pnpm add ./srcpkg-1.0.0.tgz",
		"pnpm i",
		"npm ci",
		"npm install --no-audit",
		"npm uninstall left-pad",
		"yarn install",
		"yarn add react",
		"yarn",
		"yarn --frozen-lockfile",
		"bun install",
		"bun add zod",
	}
	for _, cmd := range commands {
		out := runHookScript(t, scriptPath, makeBashInput(cmd))
		var decoded struct {
			HookSpecificOutput struct {
				PermissionDecision string         `json:"permissionDecision"`
				UpdatedInput       map[string]any `json:"updatedInput"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatalf("command %q: invalid hook JSON %q: %v", cmd, out, err)
		}
		if decoded.HookSpecificOutput.PermissionDecision != "allow" {
			t.Errorf("command %q: decision = %q, want allow", cmd, decoded.HookSpecificOutput.PermissionDecision)
			continue
		}
		ui := decoded.HookSpecificOutput.UpdatedInput
		if ui == nil {
			t.Errorf("command %q: expected updatedInput with dangerouslyDisableSandbox, got none: %s", cmd, out)
			continue
		}
		if v, ok := ui["dangerouslyDisableSandbox"].(bool); !ok || !v {
			t.Errorf("command %q: updatedInput.dangerouslyDisableSandbox = %v, want true", cmd, ui["dangerouslyDisableSandbox"])
		}
		if got, _ := ui["command"].(string); got != cmd {
			t.Errorf("command %q: updatedInput.command = %q — original command must be preserved", cmd, got)
		}
	}
}

func TestHookScript_SBX_NonPackageManagerCommandsNotRewritten(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	commands := []string{
		"echo hello",
		"go test ./...",
		"pnpm run build", // run scripts are not dependency extraction
		"pnpm test",
		"npm run lint",
		"yarn build",        // subcommand not in the mutation family
		"cat yarn.lock",     // mentions yarn only as data
		"echo pnpm install", // echo is at command position; pnpm install is argv data
	}
	for _, cmd := range commands {
		out := runHookScript(t, scriptPath, makeBashInput(cmd))
		if !strings.Contains(out, `"permissionDecision":"allow"`) {
			t.Errorf("command %q: expected plain allow, got: %s", cmd, out)
		}
		if strings.Contains(out, "updatedInput") {
			t.Errorf("command %q: must NOT be rewritten unsandboxed, got: %s", cmd, out)
		}
	}
}

func TestHookScript_SBX_AlreadyUnsandboxedInputNotRewritten(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	input := `{"tool_name":"Bash","tool_input":{"command":"pnpm install","dangerouslyDisableSandbox":true}}`
	out := runHookScript(t, scriptPath, input)
	if !strings.Contains(out, `"permissionDecision":"allow"`) {
		t.Errorf("expected allow for already-unsandboxed input, got: %s", out)
	}
	if strings.Contains(out, "updatedInput") {
		t.Errorf("already-unsandboxed input must fall through to plain allow, got: %s", out)
	}
}

func TestHookScript_SBX_DenyRulesStillWinOverRewrite(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// git push is Worker-prohibited; a compound command that also contains
	// a package-manager install must still be denied, not rewritten.
	out := runHookScript(t, scriptPath, makeBashInput("pnpm install && git push origin main"))
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("compound command with git push must be denied, got: %s", out)
	}
	if strings.Contains(out, "updatedInput") {
		t.Errorf("denied command must not carry updatedInput, got: %s", out)
	}
}

func TestHookScript_SBX_MaestroCommandsRewrittenUnsandboxedForAllRoles(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	for _, role := range []string{"worker", "planner", "orchestrator"} {
		t.Run(role+"_result_write", func(t *testing.T) {
			cmd := "maestro result write --summary x"
			out := runHookScript(t, scriptPath, makeBashInput(cmd), role)
			assertHookUnsandboxed(t, out, cmd)
		})
	}

	for _, cmd := range []string{
		"maestro version",
		"/opt/maestro/bin/maestro version",
		"env PATH=/opt/maestro/bin FOO=bar maestro version",
	} {
		t.Run(cmd, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(cmd))
			assertHookUnsandboxed(t, out, cmd)
		})
	}
}

func TestHookScript_SBX_MaestroCommandsWithShellExpansionStaySandboxed(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	plainUnsandboxed := "maestro result write --summary-file /tmp/x"
	out := runHookScript(t, scriptPath, makeBashInput(plainUnsandboxed))
	assertHookUnsandboxed(t, out, plainUnsandboxed)

	for _, cmd := range []string{
		`maestro result write --summary "$(cat /etc/hosts)"`,
		"maestro result write --summary `cat /etc/hosts`",
		"maestro result write --summary-file <(cat /etc/hosts)",
		"maestro result write --summary-file >(cat >/tmp/x)",
		`bash -c "maestro result write --summary x"`,
	} {
		t.Run(cmd, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(cmd))
			decoded := decodeHookOutput(t, out)
			if decoded.HookSpecificOutput.PermissionDecision != "allow" {
				t.Fatalf("decision = %q, want allow; output=%s", decoded.HookSpecificOutput.PermissionDecision, out)
			}
			if decoded.HookSpecificOutput.UpdatedInput != nil {
				t.Fatalf("maestro command with shell expansion/form must stay sandboxed, got: %s", out)
			}
		})
	}
}

// --- Fix C: unsandbox rewrites are single-command only ---
//
// The rewrite sets dangerouslyDisableSandbox on the ENTIRE Bash argv, so a
// compound command such as `cat /etc/hosts; maestro result write ...` would
// run its non-maestro half outside the sandbox too, bypassing the sandbox's
// denyRead secret barrier. Any connector (;, &, |, newline) must keep the
// command sandboxed; the plain allow still lets it run.
func TestHookScript_FixC_CompoundCommandsStaySandboxed(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	for _, cmd := range []string{
		"cat /etc/hosts; maestro result write --summary y",
		"cat /etc/hosts && maestro result write --summary y",
		"maestro result write --summary y | tee /tmp/out",
		"maestro version &",
		"cat /etc/hosts\nmaestro result write --summary y",
		"echo ready && maestro version",
		"cd pkg && pnpm install",
		"cat /etc/hosts; pnpm install",
		"pnpm install\ncat /etc/hosts",
		// A connector inside a quoted payload is indistinguishable from
		// syntax at glob level; staying sandboxed is the fail-safe side.
		`maestro result write --summary "a; b"`,
	} {
		t.Run(cmd, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(cmd))
			decoded := decodeHookOutput(t, out)
			if decoded.HookSpecificOutput.PermissionDecision != "allow" {
				t.Fatalf("decision = %q, want allow; output=%s", decoded.HookSpecificOutput.PermissionDecision, out)
			}
			if decoded.HookSpecificOutput.UpdatedInput != nil {
				t.Fatalf("compound command must stay sandboxed (no unsandbox rewrite), got: %s", out)
			}
		})
	}

	// Control: the single-command spellings are still rewritten.
	for _, cmd := range []string{
		"maestro result write --summary y",
		"pnpm install",
	} {
		t.Run("single "+cmd, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(cmd))
			assertHookUnsandboxed(t, out, cmd)
		})
	}
}

func TestHookScript_SBX_MaestroDeniedRulesStillWinOverRewrite(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	for _, cmd := range []string{
		"maestro plan submit --tasks-file x",
		"/opt/maestro/bin/maestro plan submit --tasks-file x",
	} {
		t.Run(cmd, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(cmd))
			decoded := decodeHookOutput(t, out)
			if decoded.HookSpecificOutput.PermissionDecision != "deny" {
				t.Fatalf("worker control-plane maestro call should be denied, got: %s", out)
			}
			if decoded.HookSpecificOutput.UpdatedInput != nil {
				t.Fatalf("denied command must not carry updatedInput, got: %s", out)
			}
		})
	}
}

func TestHookScript_SBX_MaestroMentionAsDataNotRewritten(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	cmd := `echo "run maestro later"`
	out := runHookScript(t, scriptPath, makeBashInput(cmd))
	decoded := decodeHookOutput(t, out)
	if decoded.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("expected allow, got: %s", out)
	}
	if decoded.HookSpecificOutput.UpdatedInput != nil {
		t.Fatalf("maestro mention as quoted data must not be rewritten, got: %s", out)
	}
}

func TestHookScript_SBX_RunOnMainAllowsMaestroResultWriteUnsandboxed(t *testing.T) {
	scriptPath, env := runOnMainBashEnv(t, "1")
	cmdText := "maestro result write --summary x"

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader(makeBashInput(cmdText))
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script failed: %v, output: %s", err, out)
	}
	assertHookUnsandboxed(t, string(out), cmdText)
}

func TestHookScript_SBX_RunOnMainMutationDenyStillWinsBeforeMaestroRewrite(t *testing.T) {
	scriptPath, env := runOnMainBashEnv(t, "1")
	cmdText := "mkdir out && maestro result write --summary x"

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader(makeBashInput(cmdText))
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script failed: %v, output: %s", err, out)
	}
	decoded := decodeHookOutput(t, string(out))
	if decoded.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("run_on_main mutation must be denied before maestro rewrite, got: %s", out)
	}
	if decoded.HookSpecificOutput.UpdatedInput != nil {
		t.Fatalf("denied run_on_main mutation must not carry updatedInput, got: %s", out)
	}
}

// --- Fix D (T-A1): planner/orchestrator file-mutation tools denied ---
//
// The hook's explicit `allow` for non-denied tools OVERRIDES --allowedTools
// (verified on claude 2.1.187), so without an explicit deny the blanket allow
// would grant Write/Edit to roles whose allowedTools deliberately omit them.
func TestHookScript_FixD_PlannerOrchestratorFileMutationDenied(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	inputs := map[string]string{
		"Write":        `{"tool_name":"Write","tool_input":{"file_path":"/tmp/x","content":"y"}}`,
		"Edit":         `{"tool_name":"Edit","tool_input":{"file_path":"/tmp/x","old_string":"a","new_string":"b"}}`,
		"MultiEdit":    `{"tool_name":"MultiEdit","tool_input":{"file_path":"/tmp/x","edits":[{"old_string":"a","new_string":"b"}]}}`,
		"NotebookEdit": `{"tool_name":"NotebookEdit","tool_input":{"notebook_path":"/tmp/x.ipynb","new_source":"y"}}`,
	}
	for _, role := range []string{"planner", "orchestrator"} {
		for toolName, input := range inputs {
			t.Run(role+"_"+toolName, func(t *testing.T) {
				out := runHookScript(t, scriptPath, input, role)
				if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "not permitted for planner/orchestrator") {
					t.Errorf("%s %s should be role-denied, got: %s", role, toolName, out)
				}
			})
		}
	}

	// Worker keeps its file-mutation surface (path rules still apply).
	workerWrite := fmt.Sprintf(`{"tool_name":"Write","tool_input":{"file_path":%q,"content":"y"}}`, filepath.Join(dir, "scratch.txt"))
	out := runHookScriptInDir(t, scriptPath, workerWrite, dir, "worker")
	if !strings.Contains(out, `"permissionDecision":"allow"`) || strings.Contains(out, "not permitted for planner/orchestrator") {
		t.Errorf("worker Write must not be hit by the planner/orchestrator role deny, got: %s", out)
	}

	// Bash for planner/orchestrator is unaffected by the Write/Edit role deny.
	bashOut := runHookScript(t, scriptPath, makeBashInput("echo hello"), "planner")
	if !strings.Contains(bashOut, `"permissionDecision":"allow"`) {
		t.Errorf("planner Bash echo should still be allowed, got: %s", bashOut)
	}
}

// --- Fix B (T-A2): maestro lifecycle commands denied for every role ---
//
// `maestro down` runs RunDown locally (tmux.KillSession + daemon SIGTERM)
// with no daemon-side role gate; any agent pane invoking it tears down the
// running formation. The deny must fire before the rule #6 unsandbox rewrite
// so a lifecycle call is never allow-rewritten.
func TestHookScript_FixB_MaestroLifecycleDeniedForAllRoles(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	denyCmds := []string{
		"maestro down",
		"maestro up",
		"maestro daemon",
		"maestro shutdown",
		"maestro stop",
		"maestro down --force",
		"cd /tmp && maestro down",
		"echo done; maestro down",
		"/opt/maestro/bin/maestro down",
		"env FOO=bar maestro down",
	}
	for _, role := range []string{"worker", "planner", "orchestrator"} {
		for _, cmd := range denyCmds {
			t.Run(role+"/"+cmd, func(t *testing.T) {
				out := runHookScript(t, scriptPath, makeBashInput(cmd), role)
				decoded := decodeHookOutput(t, out)
				if decoded.HookSpecificOutput.PermissionDecision != "deny" || !strings.Contains(out, "lifecycle command") {
					t.Errorf("%s %q should be lifecycle-denied, got: %s", role, cmd, out)
				}
				if decoded.HookSpecificOutput.UpdatedInput != nil {
					t.Errorf("%s %q: denied lifecycle command must not be unsandbox-rewritten, got: %s", role, cmd, out)
				}
			})
		}
	}

	allowCmds := []string{
		`maestro result write --summary "run maestro down later"`, // lifecycle verb as data
		"maestro download-report",                                 // word-boundary: down is a prefix only
		"maestro status",
	}
	for _, cmd := range allowCmds {
		t.Run("allow/"+cmd, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(cmd), "worker")
			if !strings.Contains(out, `"permissionDecision":"allow"`) || strings.Contains(out, "lifecycle command") {
				t.Errorf("%q should not be lifecycle-denied, got: %s", cmd, out)
			}
		})
	}
}

// --- Fix A (T-A4): prefixed role-env forgery forms denied ---
//
// The daemon trusts the env-declared caller role, so rule #1b must catch
// role-env rewrites in every spelling, including `export VAR=...;` and
// other prefixed forms the old command-position anchor missed.
func TestHookScript_FixA_RoleEnvPrefixedForgeryDenied(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Non-control-plane maestro subcommands are used so rule #1 (which runs
	// first and has its own deny message) does not mask the #1b guard.
	denyCmds := []string{
		"export MAESTRO_AGENT_ROLE=cli; maestro down",
		"export MAESTRO_AGENT_ROLE=cli && maestro version",
		"export TMUX_PANE=%99; maestro result write --summary x",
		"readonly MAESTRO_AGENT_ROLE=cli; maestro version",
		"export -n MAESTRO_AGENT_ROLE; maestro version",
		"true; MAESTRO_AGENT_ROLE=cli maestro result write --summary x",
	}
	for _, cmd := range denyCmds {
		t.Run(cmd, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(cmd), "worker")
			if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "role-environment manipulation") {
				t.Errorf("%q should be denied as role-env manipulation, got: %s", cmd, out)
			}
		})
	}

	allowCmds := []string{
		`maestro result write --summary "MAESTRO_AGENT_ROLE notes"`, // mention as data, no assignment
		"echo MAESTRO_AGENT_ROLE",                                   // no maestro invocation at all
	}
	for _, cmd := range allowCmds {
		t.Run("allow/"+cmd, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(cmd), "worker")
			if !strings.Contains(out, `"permissionDecision":"allow"`) || strings.Contains(out, "role-environment manipulation") {
				t.Errorf("%q must not trip the role-env guard, got: %s", cmd, out)
			}
		})
	}
}

// --- S-F1: no-redirect control-plane writers (tee/cp/mv/dd/...) denied ---
//
// tee writes its file arguments without `>`, so the redirect-shaped rule #4
// checks missed `printf x | tee .maestro/config.yaml` and
// `tee .maestro/state/x < in`. The dedicated verb rule must catch those,
// including across a pipe.
func TestHookScript_SF1_NoRedirectControlPlaneWriteDenied(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	denyCmds := []string{
		"printf x | tee .maestro/config.yaml",
		"tee .maestro/state/x < input.txt",
		"echo y | tee -a .maestro/queue/task.yaml",
		"cp payload .maestro/hooks/worker-policy.sh",
		"mv evil .maestro/verify.yaml",
		"dd if=/dev/zero of=.maestro/bin/roles/worker/maestro",
		"truncate -s0 .maestro/dashboard.md",
		"cat in | tee /abs/project/.maestro/logs/daemon.log",
	}
	for _, cmd := range denyCmds {
		t.Run(cmd, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(cmd), "worker")
			if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, ".maestro/ control-plane write blocked") {
				t.Errorf("%q should be denied as control-plane write, got: %s", cmd, out)
			}
		})
	}

	allowCmds := []string{
		"printf x | tee /tmp/out.txt",
		"tee build/log.txt < input.txt",
		`maestro result write --summary "piped to tee .maestro/config.yaml"`, // path as data
		"cp foo bar/my.maestro/state.txt",                                    // my.maestro is not .maestro
	}
	for _, cmd := range allowCmds {
		t.Run("allow/"+cmd, func(t *testing.T) {
			out := runHookScript(t, scriptPath, makeBashInput(cmd), "worker")
			if !strings.Contains(out, `"permissionDecision":"allow"`) || strings.Contains(out, "control-plane write blocked") {
				t.Errorf("%q must not trip the control-plane verb rule, got: %s", cmd, out)
			}
		})
	}
}

// --- S-F2: RUN_ON_MAIN redirect forms >| and fd-prefixed denied ---
//
// RUN_ON_MAIN is the read-only verification mode; `>|` (noclobber override)
// and fd-prefixed redirects (`1>`, `2>>`) previously slipped past the
// redirect regex, and `| tee out` slipped past the verb list.
func TestHookScript_SF2_RunOnMainRedirectVariantsDenied(t *testing.T) {
	scriptPath, env := runOnMainBashEnv(t, "1")

	runHook := func(t *testing.T, command string) string {
		t.Helper()
		cmd := exec.Command("bash", scriptPath)
		cmd.Stdin = strings.NewReader(makeBashInput(command))
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("hook script failed: %v, output: %s", err, out)
		}
		return string(out)
	}

	denyCmds := []string{
		"echo x >| out.txt",
		"echo x 1> out.txt",
		"echo x 2>> err.log",
		"echo x &> all.log",
		"go test ./... | tee test.log",
		"dd if=/dev/zero of=scratch.bin",
	}
	for _, command := range denyCmds {
		t.Run(command, func(t *testing.T) {
			out := runHook(t, command)
			if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "RUN_ON_MAIN") {
				t.Errorf("%q should be RUN_ON_MAIN-denied, got: %s", command, out)
			}
		})
	}

	allowCmds := []string{
		"go test ./... 2>&1", // fd duplication writes no file
		"echo msg >&2",       // dup to stderr
	}
	for _, command := range allowCmds {
		t.Run("allow/"+command, func(t *testing.T) {
			out := runHook(t, command)
			if strings.Contains(out, "RUN_ON_MAIN") {
				t.Errorf("%q is read-only and must not be RUN_ON_MAIN-denied, got: %s", command, out)
			}
		})
	}
}

// --- T-A5 / S-F3: L1 --disallowedTools pattern symmetry ---
//
// Read patterns need the **/ form because the Worker CWD is the worktree and
// project-root reads arrive as absolute paths (T-A5); bin/, hooks/, and
// verify.yaml must be protected at L1 like they are at L2 (S-F3).
func TestHookScript_L1_ControlPlanePatternSymmetry(t *testing.T) {
	args, err := buildLaunchArgs("worker", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	want := []string{
		// T-A5: absolute-path-capable Read forms alongside the relative ones.
		"Read(.maestro/state/**)", "Read(**/.maestro/state/**)",
		"Read(.maestro/queue/**)", "Read(**/.maestro/queue/**)",
		"Read(.maestro/results/**)", "Read(**/.maestro/results/**)",
		"Read(.maestro/locks/**)", "Read(**/.maestro/locks/**)",
		"Read(.maestro/logs/**)", "Read(**/.maestro/logs/**)",
		"Read(.maestro/config.yaml)", "Read(**/.maestro/config.yaml)",
		"Read(.maestro/dashboard.md)", "Read(**/.maestro/dashboard.md)",
		// S-F3: policy-enforcement paths protected in both layers.
		"Read(.maestro/bin/**)", "Read(**/.maestro/bin/**)",
		"Read(.maestro/hooks/**)", "Read(**/.maestro/hooks/**)",
		"Read(.maestro/verify.yaml)", "Read(**/.maestro/verify.yaml)",
		"Edit(**/.maestro/bin/**)", "Write(**/.maestro/bin/**)",
		"Edit(**/.maestro/hooks/**)", "Write(**/.maestro/hooks/**)",
		"Edit(**/.maestro/verify.yaml)", "Write(**/.maestro/verify.yaml)",
	}
	for _, pattern := range want {
		if !strings.Contains(joined, pattern) {
			t.Errorf("worker --disallowedTools should contain %q", pattern)
		}
	}
}
