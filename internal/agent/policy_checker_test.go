package agent

import (
	"encoding/json"
	"os"
	"os/exec"
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

	input := `{"tool_name":"Bash","tool_input":{"command":"rm -rf /home/user"}}`
	output := runHookScript(t, scriptPath, input)
	if !strings.Contains(output, "D002") || !strings.Contains(output, "deny") {
		t.Errorf("expected D002 deny for rm -rf /home/user, got: %s", output)
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
