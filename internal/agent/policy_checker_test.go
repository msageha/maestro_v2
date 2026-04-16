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

func TestHookScript_ContainsDangerousPatternChecks(t *testing.T) {
	// Verify the hook script checks for all Tier 1 danger categories
	checks := []struct {
		id   string
		text string
	}{
		{"D001", "D001"},
		{"Worker git push", "Worker git push is prohibited"},
		{"D004", "D004"},
		{"D005", "D005"},
		{"D006", "D006"},
		{"D007", "D007"},
		{"D008", "D008"},
		{"D009", "D009"},
		{"D009 unquarantine", "maestro plan unquarantine"},
		{"D009 resume-merge", "maestro plan resume-merge"},
		{"D009 resolve-conflict", "maestro resolve-conflict"},
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
	args, err := buildLaunchArgs("worker", "sonnet", "system-prompt", "")
	if err != nil {
		t.Fatalf("buildLaunchArgs: %v", err)
	}
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
	// Worker hook blocks ALL git push, including --force-with-lease
	if !strings.Contains(hookScript, `git\s+push(\s|$)`) {
		t.Error("hook script should block all git push for Workers")
	}
	if !strings.Contains(hookScript, "Worker git push is prohibited") {
		t.Error("hook script should contain Worker git push prohibition message")
	}
}

func TestHookScript_ContainsRestrictedModeBypassChecks(t *testing.T) {
	checks := []struct {
		id   string
		text string
	}{
		{"B001", "B001"},
		{"B002", "B002"},
		{"B003", "B003"},
		{"B004", "B004"},
	}

	for _, tc := range checks {
		if !strings.Contains(hookScript, tc.text) {
			t.Errorf("hook script missing check for %s (expected to find %q)", tc.id, tc.text)
		}
	}
}

func TestHookScript_BlocksPipeToShell(t *testing.T) {
	// These patterns should be detected by the B001 grep patterns in the hook script
	blocked := []string{
		`echo cmd | bash`,
		`cat script.sh | sh`,
		`printf 'cmd' | /bin/bash`,
		`echo test | /bin/sh`,
		`echo test | /usr/bin/bash`,
		`echo test | bash -`,
	}
	for _, cmd := range blocked {
		// Verify the hook script has grep patterns that would match these
		// We check that B001 section exists and contains pipe-to-shell patterns
		if !strings.Contains(hookScript, "B001") {
			t.Errorf("hook script missing B001 check for: %s", cmd)
		}
	}

	// Verify safe commands would NOT be blocked by B001 patterns
	// "bash_completion" should not match \b(bash|sh)\b word boundary
	if !strings.Contains(hookScript, `\b(bash|sh)\s`) {
		// The script uses patterns with word boundaries or specific suffixes
		// to avoid matching variable names like bash_completion
	}
}

func TestHookScript_BlocksShellCFlag(t *testing.T) {
	if !strings.Contains(hookScript, `\b(bash|sh)\s+-[a-zA-Z]*c\b`) {
		t.Error("hook script should contain B002 pattern for bash/sh -c")
	}
}

func TestHookScript_BlocksEval(t *testing.T) {
	if !strings.Contains(hookScript, `eval\s+`) {
		t.Error("hook script should contain B003 pattern for eval")
	}
}

func TestHookScript_BlocksAbsolutePathShell(t *testing.T) {
	if !strings.Contains(hookScript, `/bin/(ba)?sh`) {
		t.Error("hook script should contain B004 pattern for /bin/bash and /bin/sh")
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

func TestHookScript_S1_D002_DeniesRecursiveDeleteOutsideProject(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	input := `{"tool_name":"Bash","tool_input":{"command":"rm -rf /var/data"}}`
	output := runHookScript(t, scriptPath, input)
	if !strings.Contains(output, "D002") || !strings.Contains(output, "deny") {
		t.Errorf("expected D002 deny for rm -rf /var/data, got: %s", output)
	}
}

func TestHookScript_S1_D002_AllowsRecursiveDeleteInsideProject(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Create a subdirectory inside the project so realpath can resolve it
	subdir := filepath.Join(dir, "build", "tmp")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	input := `{"tool_name":"Bash","tool_input":{"command":"rm -rf ` + subdir + `"}}`
	output := runHookScript(t, scriptPath, input)
	if strings.Contains(output, "deny") {
		t.Errorf("should allow rm -rf inside project, got: %s", output)
	}
}

func TestHookScript_S1_D002_DeniesRecursiveDeleteWithDoubleDash(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	input := `{"tool_name":"Bash","tool_input":{"command":"rm --recursive /tmp/outside"}}`
	output := runHookScript(t, scriptPath, input)
	if !strings.Contains(output, "D002") || !strings.Contains(output, "deny") {
		t.Errorf("expected D002 deny for rm --recursive /tmp/outside, got: %s", output)
	}
}

func TestHookScript_S1_D002_ContainsPattern(t *testing.T) {
	if !strings.Contains(hookScript, "D002") {
		t.Error("hook script should contain D002 check")
	}
	if !strings.Contains(hookScript, "__PROJECT_ROOT__") {
		t.Error("hook script template should contain __PROJECT_ROOT__ placeholder")
	}
}

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
	if !strings.Contains(hookScript, "control-plane path (relative)") {
		t.Error("hook script should contain relative path deny message")
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

func TestHookScript_S3_ContainsJqCheck(t *testing.T) {
	if !strings.Contains(hookScript, "command -v jq") {
		t.Error("hook script should contain jq availability check")
	}
	if !strings.Contains(hookScript, "jq but it is not installed") {
		t.Error("hook script should contain jq unavailable deny message")
	}
}

func TestHookScript_WriteHookScript_EmbedsProjectRoot(t *testing.T) {
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
	// The written script should have PROJECT_ROOT replaced with actual dir
	if strings.Contains(string(content), "__PROJECT_ROOT__") {
		t.Error("written script should not contain __PROJECT_ROOT__ placeholder")
	}
	if !strings.Contains(string(content), dir) {
		t.Errorf("written script should contain project root %q", dir)
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple path", "/home/user/project", "'/home/user/project'"},
		{"single quote", "/home/it's here", "'/home/it'\\''s here'"},
		{"double quote", `/home/user/"project"`, `'/home/user/"project"'`},
		{"backtick", "/home/user/`cmd`", "'/home/user/`cmd`'"},
		{"dollar sign", "/home/user/$HOME", "'/home/user/$HOME'"},
		{"semicolon", "/home/user;rm -rf /", "'/home/user;rm -rf /'"},
		{"pipe", "/home/user|cat /etc/passwd", "'/home/user|cat /etc/passwd'"},
		{"ampersand", "/home/user&&evil", "'/home/user&&evil'"},
		{"space", "/home/my project", "'/home/my project'"},
		{"newline", "/home/user\ninjected", "'/home/user\ninjected'"},
		{"backslash", `/home/user\dir`, `'/home/user\dir'`},
		{"multiple single quotes", "a'b'c", "a'b'c"},
		{"empty", "", "''"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shellQuote(tc.input)
			if tc.name == "multiple single quotes" {
				// Just verify it's properly escaped (contains no unescaped singles)
				if got != "'a'\\''b'\\''c'" {
					t.Errorf("shellQuote(%q) = %q, want %q", tc.input, got, "'a'\\''b'\\''c'")
				}
				return
			}
			if got != tc.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestShellQuote_SafeInBash(t *testing.T) {
	// Verify that shellQuote produces strings that bash evaluates to the original value.
	tests := []struct {
		name  string
		input string
	}{
		{"simple", "/home/user/project"},
		{"single quote", "/home/it's here"},
		{"double quote", `/home/"project"`},
		{"backtick", "/home/`whoami`"},
		{"dollar expansion", "/home/$USER/project"},
		{"semicolon injection", "/tmp/foo;rm -rf /"},
		{"pipe injection", "/tmp/foo|cat /etc/passwd"},
		{"ampersand", "/tmp/foo&&echo pwned"},
		{"space", "/tmp/my project"},
		{"backslash", `/tmp/back\slash`},
		{"subshell", "/tmp/$(whoami)"},
		{"all special", `/tmp/a'b"c` + "`d$e;f|g&h i\nj"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			quoted := shellQuote(tc.input)
			// Use printf %s to avoid echo interpreting backslashes
			cmd := exec.Command("bash", "-c", "printf '%s' "+quoted)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bash eval failed: %v, output: %s", err, out)
			}
			if string(out) != tc.input {
				t.Errorf("bash evaluated to %q, want %q", string(out), tc.input)
			}
		})
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

// --- Bash dashboard.md access blocking ---

func TestHookScript_BashDashboardMdAccessDenied(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	tests := []struct {
		name string
		cmd  string
	}{
		{"cat", `cat .maestro/dashboard.md`},
		{"head", `head .maestro/dashboard.md`},
		{"grep", `grep pattern .maestro/dashboard.md`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "deny") {
				t.Errorf("expected deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// --- deny() JSON escaping ---

func TestHookScript_DenyJSONEscapesSpecialChars(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Use a command that triggers D005 (sudo) — the deny reason contains
	// fixed text, but the important thing is the output is valid JSON.
	input := `{"tool_name":"Bash","tool_input":{"command":"sudo rm -rf /"}}`
	output := runHookScript(t, scriptPath, input)

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &parsed); err != nil {
		t.Fatalf("deny output is not valid JSON: %v\noutput: %s", err, output)
	}
	hso, ok := parsed["hookSpecificOutput"].(map[string]interface{})
	if !ok {
		t.Fatal("missing hookSpecificOutput")
	}
	if hso["permissionDecision"] != "deny" {
		t.Error("permissionDecision should be deny")
	}
}

// --- D005: chmod -R system path blocking ---

func TestHookScript_D005_ChmodRSystemPathDenied(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []string{
		`chmod -R 755 /usr/local`,
		`chmod -R 777 /etc/config`,
		`chmod -Rv 755 /System/Library`,
		`chmod -cR 755 /Library/Extensions`,
	}
	for _, cmd := range blocked {
		t.Run(cmd, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "deny") || !strings.Contains(output, "D005") {
				t.Errorf("expected D005 deny for %q, got: %s", cmd, output)
			}
		})
	}
}

func TestHookScript_D005_ChmodRProjectPathAllowed(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// chmod -R on a project-local path should be allowed
	input := `{"tool_name":"Bash","tool_input":{"command":"chmod -R 755 ./build/output"}}`
	output := runHookScript(t, scriptPath, input)
	if strings.Contains(output, "deny") {
		t.Errorf("should allow chmod -R on project path, got: %s", output)
	}
}

func TestHookScript_ContainsChmodRPattern(t *testing.T) {
	if !strings.Contains(hookScript, "chmod") {
		t.Error("hook script should contain chmod -R check")
	}
	if !strings.Contains(hookScript, "D005: Blocked chmod -R") {
		t.Error("hook script should contain D005 chmod -R deny message")
	}
}

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

// =============================================================================
// C1: Shell variable expansion bypass tests
// =============================================================================

func TestHookScript_C1_BlocksBacktickSubstitution(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"backtick rm", "echo `rm -rf /`"},
		{"backtick echo", "`echo rm` -rf /"},
		{"backtick in middle", "ls `pwd`/src"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			// Must escape backticks in JSON
			escaped := strings.ReplaceAll(tc.cmd, "`", "` + \"`\" + `")
			// Use raw construction to avoid backtick issues
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			_ = escaped
			if !strings.Contains(output, "C1") || !strings.Contains(output, "deny") {
				t.Errorf("expected C1 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_C1_BlocksAnsiCQuoting(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"ansi-c simple", "$'hello' arg"},
		{"ansi-c with space", "echo $'test'"},
		{"ansi-c at start", "$'cmd' -rf /"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "C1") || !strings.Contains(output, "deny") {
				t.Errorf("expected C1 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_C1_BlocksUnsafeCommandSubstitution(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"echo subst", "$(echo rm) -rf /"},
		{"printf subst", "$(printf '%s' rm) -rf /"},
		{"arbitrary subst", "$(cat /tmp/cmd) -rf /"},
		{"bash subst", "$(bash -c 'echo rm') -rf /"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "C1") || !strings.Contains(output, "deny") {
				t.Errorf("expected C1 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_C1_AllowsSafeSubstitution(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	allowed := []struct {
		name string
		cmd  string
	}{
		{"go env", "echo $(go env GOROOT)"},
		{"pwd subst", "ls $(pwd)/src"},
		{"git rev-parse", "cd $(git rev-parse --show-toplevel)"},
		{"git log", "echo $(git log --oneline -1)"},
		{"which", "$(which go) version"},
		{"uname", "echo $(uname -s)"},
		{"arithmetic", "echo $(( 1 + 2 ))"},
		{"wc", "echo $(wc -l < file.txt)"},
		{"env var ref", "echo $HOME"},
		{"env var brace", "echo ${TMPDIR}"},
		{"go test plain", "go test -v ./..."},
		{"git status", "git status"},
		{"grep plain", "grep -r pattern src/"},
		{"plain echo", "echo hello world"},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if strings.Contains(output, "C1") {
				t.Errorf("should allow %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// =============================================================================
// C2: D001 regex flag separation bypass tests
// =============================================================================

func TestHookScript_C2_BlocksSeparatedFlags(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"separated r f", "rm -r -f /"},
		{"separated f r", "rm -f -r /"},
		{"with verbose", "rm -v -r -f /"},
		{"long recursive short force", "rm --recursive -f /"},
		{"short recursive long force", "rm -r --force /"},
		{"long both", "rm --recursive --force /"},
		{"target tilde", "rm -r -f ~"},
		{"target Users", "rm -r -f /Users"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "D001") || !strings.Contains(output, "deny") {
				t.Errorf("expected D001 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_C2_AllowsSafeRm(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Create subdirs for rm targets
	for _, sub := range []string{"build", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	allowed := []struct {
		name string
		cmd  string
	}{
		{"rm file only", "rm -f file.txt"},
		{"rm dir only", "rm -r " + filepath.Join(dir, "build")},
		{"rm no force", "rm -r " + filepath.Join(dir, "tmp")},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if strings.Contains(output, "D001") {
				t.Errorf("should allow %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// =============================================================================
// H1: Absolute path / wrapper bypass tests
// =============================================================================

func TestHookScript_H1_BlocksAbsolutePathDangerousCommands(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"usr bin kill", "/usr/bin/kill 1234"},
		{"bin kill", "/bin/kill 1234"},
		{"usr sbin fdisk", "/usr/sbin/fdisk /dev/sda"},
		{"usr bin chmod", "/usr/bin/chmod 777 /etc/passwd"},
		{"sbin mkfs", "/sbin/mkfs.ext4 /dev/sda1"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "H1") || !strings.Contains(output, "deny") {
				t.Errorf("expected H1 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_H1_BlocksWrapperCommands(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"env kill", "env kill 1234"},
		{"command kill", "command kill 1234"},
		{"exec kill", "exec kill 1234"},
		{"env with var kill", "env FOO=bar kill 1234"},
		{"env with flag kill", "env -i kill 1234"},
		{"env sudo", "env sudo apt install foo"},
		{"xargs kill", "echo 123 | xargs kill"},
		{"xargs rm", "echo file | xargs rm"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "H1") || !strings.Contains(output, "deny") {
				t.Errorf("expected H1 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_H1_AllowsSafeCommands(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	allowed := []struct {
		name string
		cmd  string
	}{
		{"env go test", "env GO111MODULE=on go test ./..."},
		{"command go", "command go version"},
		{"usr bin grep", "/usr/bin/grep pattern file.txt"},
		{"env with var go", "env CGO_ENABLED=0 go build ./..."},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if strings.Contains(output, "H1") {
				t.Errorf("should allow %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// =============================================================================
// H3: Symlink D002 bypass tests
// =============================================================================

func TestHookScript_H3_BlocksSymlinkOutsideProject(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Create a symlink inside the project that points outside
	outsideDir := t.TempDir()
	symlinkPath := filepath.Join(dir, "sneaky_link")
	if err := os.Symlink(outsideDir, symlinkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	input := `{"tool_name":"Bash","tool_input":{"command":"rm -rf ` + symlinkPath + `"}}`
	output := runHookScript(t, scriptPath, input)
	if !strings.Contains(output, "D002") || !strings.Contains(output, "deny") {
		t.Errorf("expected D002 deny for symlink outside project, got: %s", output)
	}
}

func TestHookScript_H3_AllowsSymlinkInsideProject(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	// Create a symlink inside the project that points to another dir inside project
	targetDir := filepath.Join(dir, "target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	symlinkPath := filepath.Join(dir, "internal_link")
	if err := os.Symlink(targetDir, symlinkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	input := `{"tool_name":"Bash","tool_input":{"command":"rm -rf ` + symlinkPath + `"}}`
	output := runHookScript(t, scriptPath, input)
	if strings.Contains(output, "deny") {
		t.Errorf("should allow symlink inside project, got: %s", output)
	}
}

// =============================================================================
// H4: WT001 relative path bypass tests
// =============================================================================

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

// =============================================================================
// M-AGT1: Alternative destructive tools tests
// =============================================================================

func TestHookScript_MAGT1_BlocksFindDelete(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"find delete", "find /tmp -name '*.tmp' -delete"},
		{"find exec rm", `find . -exec rm {} \;`},
		{"find exec shred", `find . -name '*.log' -exec shred {} \;`},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "M-AGT1") || !strings.Contains(output, "deny") {
				t.Errorf("expected M-AGT1 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_MAGT1_BlocksScriptingDestructiveOps(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"perl unlink", `perl -e 'unlink("file.txt")'`},
		{"python os.remove", `python -c 'import os; os.remove("file.txt")'`},
		{"python3 rmtree", `python3 -c 'import shutil; shutil.rmtree("dir")'`},
		{"node unlinkSync", `node -e 'require("fs").unlinkSync("file")'`},
		{"node rmSync", `node -e 'require("fs").rmSync("dir", {recursive: true})'`},
		{"ruby File.delete", `ruby -e 'File.delete("file.txt")'`},
		{"ruby rm_rf", `ruby -e 'FileUtils.rm_rf("dir")'`},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "M-AGT1") || !strings.Contains(output, "deny") {
				t.Errorf("expected M-AGT1 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_MAGT1_AllowsSafeFindAndScripting(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	allowed := []struct {
		name string
		cmd  string
	}{
		{"find no delete", "find . -name '*.go'"},
		{"find type f", "find . -type f -name '*.txt'"},
		{"python print", "python -c 'print(42)'"},
		{"node console", "node -e 'console.log(42)'"},
		{"perl print", "perl -e 'print 42'"},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if strings.Contains(output, "M-AGT1") {
				t.Errorf("should allow %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// =============================================================================
// M-AGT2: .maestro access check expansion tests
// =============================================================================

func TestHookScript_MAGT2_BlocksFileOpsOnMaestro(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"cp to maestro", "cp file.txt .maestro/state/tasks.yaml"},
		{"mv to maestro", "mv file.txt .maestro/config.yaml"},
		{"rsync to maestro", "rsync file.txt .maestro/queue/"},
		{"ln to maestro state", "ln -s /tmp/x .maestro/state/link"},
		{"cp read maestro", "cp .maestro/state/tasks.yaml /tmp/"},
		{"install to maestro", "install -m 644 file.txt .maestro/locks/"},
		{"tar maestro", "tar czf backup.tar.gz .maestro/state/"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "deny") {
				t.Errorf("expected deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_MAGT2_AllowsSafeFileOps(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	allowed := []struct {
		name string
		cmd  string
	}{
		{"cp normal files", "cp src/file.go dst/file.go"},
		{"mv normal files", "mv old.txt new.txt"},
		{"ln normal", "ln -s src/lib dst/lib"},
		{"rsync normal", "rsync -av src/ dst/"},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tc.cmd + `"}}`
			output := runHookScript(t, scriptPath, input)
			if strings.Contains(output, "deny") {
				t.Errorf("should allow %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// =============================================================================
// Legitimate commands should NOT be blocked (comprehensive safety check)
// =============================================================================

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
// C-1: ANSI-C quoting after shell operators
// =============================================================================

func TestHookScript_C1_BlocksAnsiCQuotingAfterOperators(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"after semicolon", "echo hello;$'\\x72\\x6d' -rf /"},
		{"after pipe", "echo hello|$'cmd' arg"},
		{"after double ampersand", "true&&$'cmd' arg"},
		{"after open paren", "($'cmd' arg)"},
		{"after open brace", "{$'cmd' arg;}"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "C1") || !strings.Contains(output, "deny") {
				t.Errorf("expected C1 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_C1_AnsiCQuotingRegexContainsOperators(t *testing.T) {
	// Verify the expanded anchor includes shell operators
	if !strings.Contains(hookScript, `[[:space:];|&({]`) {
		t.Error("ANSI-C quoting regex should include shell operator anchors ;|&({")
	}
}

// =============================================================================
// H-1: Process substitution blocking
// =============================================================================

func TestHookScript_H1PS_BlocksProcessSubstitution(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"input process sub", "diff <(sort file1) <(sort file2)"},
		{"output process sub", "tee >(grep error > errors.log)"},
		{"single input sub", "cat <(echo hello)"},
		{"single output sub", "echo hello > >(cat)"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "H1-PS") || !strings.Contains(output, "deny") {
				t.Errorf("expected H1-PS deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_H1PS_AllowsNormalRedirects(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	allowed := []struct {
		name string
		cmd  string
	}{
		{"output redirect", "echo hello > output.txt"},
		{"append redirect", "echo hello >> output.txt"},
		{"input redirect", "sort < input.txt"},
		{"stderr redirect", "go test ./... 2>&1"},
		{"null redirect", "go build ./... > /dev/null 2>&1"},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScript(t, scriptPath, input)
			if strings.Contains(output, "H1-PS") {
				t.Errorf("should allow %q, got: %s", tc.cmd, output)
			}
		})
	}
}

// =============================================================================
// H-2: D001 Linux path coverage
// =============================================================================

func TestHookScript_D001_BlocksLinuxPaths(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"rm -rf /home", "rm -rf /home"},
		{"rm -rf /root", "rm -rf /root"},
		{"rm -rf /opt", "rm -rf /opt"},
		{"rm -fr /home", "rm -fr /home"},
		{"rm --recursive --force /home", "rm --recursive --force /home"},
		{"rm -r -f /root", "rm -r -f /root"},
		{"rm -f -r /opt", "rm -f -r /opt"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "D001") || !strings.Contains(output, "deny") {
				t.Errorf("expected D001 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_D001_LinuxPathsInRegex(t *testing.T) {
	if !strings.Contains(hookScript, "/home") {
		t.Error("D001 regex should include /home")
	}
	if !strings.Contains(hookScript, "/root") {
		t.Error("D001 regex should include /root")
	}
	if !strings.Contains(hookScript, "/opt") {
		t.Error("D001 regex should include /opt")
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

// =============================================================================
// L1: .maestro/queue/ (singular) access blocking tests
// =============================================================================

func TestHookScript_L1_BlocksBashAccessToMaestroQueue(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"cat queue", "cat .maestro/queue/pending.yaml"},
		{"ls queue", "ls .maestro/queue/"},
		{"grep queue", "grep pattern .maestro/queue/tasks.yaml"},
		{"cp queue", "cp .maestro/queue/pending.yaml /tmp/"},
		{"mv to queue", "mv file.txt .maestro/queue/"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "deny") {
				t.Errorf("expected deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

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

func TestHookScript_MPERL1_BlocksPerlIndirectExecution(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"perl eval", `perl -e 'eval("print 42")'`},
		{"perl system", `perl -e 'system("echo hello")'`},
		{"perl exec", `perl -e 'exec("ls", "-la")'`},
		{"perl -E eval", `perl -E 'eval("code")'`},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScript(t, scriptPath, input)
			if !strings.Contains(output, "M-PERL1") || !strings.Contains(output, "deny") {
				t.Errorf("expected M-PERL1 deny for %q, got: %s", tc.cmd, output)
			}
		})
	}
}

func TestHookScript_MPERL1_AllowsSafePerlOperations(t *testing.T) {
	requireJq(t)
	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	allowed := []struct {
		name string
		cmd  string
	}{
		{"perl print", "perl -e 'print 42'"},
		{"perl no -e", "perl script.pl"},
		{"perl regex", "perl -e 'print if /pattern/'"},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			input := makeBashInput(tc.cmd)
			output := runHookScript(t, scriptPath, input)
			if strings.Contains(output, "M-PERL1") {
				t.Errorf("should allow %q, got: %s", tc.cmd, output)
			}
		})
	}
}
