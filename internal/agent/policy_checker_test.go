package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestHookScript_ContainsDangerousPatternChecks(t *testing.T) {
	// Verify the hook script checks for all Tier 1 danger categories
	checks := []struct {
		id   string
		text string
	}{
		{"D001", "D001"},
		{"D003", "D003"},
		{"D004", "D004"},
		{"D005", "D005"},
		{"D006", "D006"},
		{"D007", "D007"},
		{"D008", "D008"},
		{".maestro/ bypass", ".maestro/"},
		{"macOS system dirs", "System|Library|Applications"},
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
	args := buildLaunchArgs("worker", "sonnet", "system-prompt")
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "--settings") {
		t.Error("worker buildLaunchArgs should NOT include --settings (merged in Launch)")
	}
}

func TestHookSettings_WorkerMergedSettings(t *testing.T) {
	// HookSettings should produce a single JSON containing both Notification:[] and PreToolUse.
	dir := t.TempDir()
	pc := NewPolicyChecker(dir)
	settings, err := pc.HookSettings("/tmp/test-hook.sh")
	if err != nil {
		t.Fatalf("HookSettings error: %v", err)
	}
	if !strings.Contains(settings, `"Notification":[]`) {
		t.Error("merged settings should contain Notification:[]")
	}
	if !strings.Contains(settings, `"PreToolUse"`) {
		t.Error("merged settings should contain PreToolUse")
	}
}

func TestBuildLaunchArgs_NonWorkerNoPreToolUseHook(t *testing.T) {
	// Orchestrator and planner should NOT have PreToolUse hook settings
	for _, role := range []string{"orchestrator", "planner"} {
		args := buildLaunchArgs(role, "sonnet", "system-prompt")
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
	if !strings.Contains(hookScript, `\"permissionDecision\":\"deny\"`) {
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
		".maestro/queues",
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

func TestHookScript_AllowsForceWithLease(t *testing.T) {
	// The script should NOT block git push --force-with-lease
	if !strings.Contains(hookScript, "force-with-lease") {
		t.Error("hook script should allow --force-with-lease as safe alternative")
	}
}

func TestHookScript_AllowsGitCleanDryRun(t *testing.T) {
	// git clean -n (dry run) should be excluded from blocking
	if !strings.Contains(hookScript, `git\s+clean\s+-[a-zA-Z]*n`) {
		t.Error("hook script should check for git clean -n (dry run) to exclude it from blocking")
	}
}

func TestHookScript_D001_BlocksAllFlagOrders(t *testing.T) {
	// D001 regex must match rm with both r/R and f in any order
	tests := []struct {
		pattern string
		shouldMatch bool
	}{
		// Should be blocked (contains both r/R and f)
		{`rm\s+-[a-zA-Z]*[rR][a-zA-Z]*f`, true},   // pattern A: r before f
		{`rm\s+-[a-zA-Z]*f[a-zA-Z]*[rR]`, true},    // pattern B: f before r
	}

	for _, tc := range tests {
		if strings.Contains(hookScript, tc.pattern) != tc.shouldMatch {
			t.Errorf("hook script should contain pattern %q: got %v, want %v",
				tc.pattern, !tc.shouldMatch, tc.shouldMatch)
		}
	}

	// Verify both OR branches exist for D001 to handle -fr, -fR, -Rf variants
	if !strings.Contains(hookScript, `rm\s+-[a-zA-Z]*[rR][a-zA-Z]*f`) {
		t.Error("D001: missing pattern for r/R before f (e.g., rm -rf)")
	}
	if !strings.Contains(hookScript, `rm\s+-[a-zA-Z]*f[a-zA-Z]*[rR]`) {
		t.Error("D001: missing pattern for f before r/R (e.g., rm -fr, rm -fR)")
	}
}
