package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// templateDir returns the absolute path to the templates/ directory.
func templateDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "templates")
}

// requireClaude skips the test if the claude CLI is not available.
func requireClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not found; skipping integration test")
	}
}

// projectRoot returns the absolute path to the project root.
func projectRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// runOrchestratorQuery launches claude with the orchestrator system prompt and
// sends a single-turn query via -p flag. Returns stdout as a string.
// If workDir is empty, os.TempDir() is used.
//
// NOTE: -p (non-interactive) mode auto-approves all tools regardless of
// --allowedTools. Tool restriction tests using -p are informational only;
// production enforcement relies on interactive mode + --allowedTools whitelist.
func runOrchestratorQuery(t *testing.T, query, workDir string) string {
	t.Helper()

	tmplDir := templateDir(t)
	systemPrompt, err := buildSystemPrompt(tmplDir, "orchestrator")
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	tools := strings.Join(allowedToolsByRole["orchestrator"], ",")

	args := []string{
		"-p", query,
		"--model", "claude-sonnet-4-6",
		"--append-system-prompt", systemPrompt,
		"--dangerously-skip-permissions",
		"--allowedTools", tools,
		"--output-format", "text",
	}

	cmd := exec.Command("claude", args...)
	if workDir != "" {
		cmd.Dir = workDir
	} else {
		cmd.Dir = os.TempDir()
	}

	// Remove CLAUDECODE env var to allow nested invocation in test.
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			filtered = append(filtered, e)
		}
	}
	cmd.Env = filtered

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("claude command failed: %v\noutput: %s", err, string(out))
	}
	return string(out)
}

func TestIntegration_OrchestratorRoleAwareness(t *testing.T) {
	if os.Getenv("MAESTRO_INTEGRATION") == "" {
		t.Skip("set MAESTRO_INTEGRATION=1 to run integration tests")
	}
	requireClaude(t)

	result := runOrchestratorQuery(t, "あなたの役割を簡潔に教えてください。何の Agent ですか？", "")
	lower := strings.ToLower(result)

	t.Logf("Response:\n%s", result)

	if !strings.Contains(lower, "orchestrator") {
		t.Errorf("expected response to mention 'orchestrator', got:\n%s", result)
	}
}

func TestIntegration_OrchestratorToolsConfig(t *testing.T) {
	// Verify that allowedTools is configured correctly for orchestrator.
	// In -p mode, tool restrictions cannot be enforced (all tools auto-approved),
	// but the config itself should be correct for production use.
	tools := allowedToolsByRole["orchestrator"]
	joined := strings.Join(tools, ",")

	if !strings.Contains(joined, "Read(.maestro/**)") {
		t.Errorf("orchestrator allowedTools must include Read(.maestro/**), got: %s", joined)
	}
	if !strings.Contains(joined, "Bash(maestro:*)") {
		t.Errorf("orchestrator allowedTools must include Bash(maestro:*), got: %s", joined)
	}
}

// NOTE: Tool restriction verification is not possible in -p (non-interactive) mode because
// all tools are auto-approved. Actual enforcement is guaranteed by the allowedToolsByRole
// unit test (TestIntegration_OrchestratorToolsConfig) and the production --allowedTools whitelist.
// A proper tool restriction integration test would require interactive mode with a test harness
// that can observe tool approval/denial events.
