package agent

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	policyHookImplementationBash   = "bash"
	policyHookImplementationShadow = "shadow"
	policyHookImplementationGo     = "go"
)

// PolicyChecker generates PreToolUse hook scripts and settings to technically
// enforce destructive operation prevention for Worker agents.
//
// The hook script is written to .maestro/hooks/ and referenced in the Claude
// Code --settings JSON. It intercepts Bash, Write, and Edit tool calls,
// blocking dangerous commands defined in Tier 1/Tier 2 of the Worker safety rules.
type PolicyChecker struct {
	maestroDir string
}

// NewPolicyChecker creates a PolicyChecker for the given maestro directory.
func NewPolicyChecker(maestroDir string) *PolicyChecker {
	return &PolicyChecker{maestroDir: maestroDir}
}

// HookScriptOptions selects which worker policy hook implementation is written.
// Empty fields preserve the production-safe default: the legacy bash policy
// with shadow comparison disabled.
type HookScriptOptions struct {
	Implementation string
	MaestroBinary  string
}

// hookScriptPath returns the filesystem path for the policy hook script.
func (pc *PolicyChecker) hookScriptPath() string {
	return filepath.Join(pc.maestroDir, "hooks", "worker-policy.sh")
}

// WriteHookScript writes the policy enforcement shell script to disk.
// Returns the script path. The script is idempotently overwritten.
func (pc *PolicyChecker) WriteHookScript() (string, error) {
	return pc.WriteHookScriptWithOptions(HookScriptOptions{Implementation: policyHookImplementationBash})
}

// WriteHookScriptWithOptions writes the selected worker policy hook script to
// disk. The default bash implementation remains authoritative until the Go
// policy reaches full corpus parity.
func (pc *PolicyChecker) WriteHookScriptWithOptions(opts HookScriptOptions) (string, error) {
	dir := filepath.Join(pc.maestroDir, "hooks")
	if err := os.MkdirAll(dir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for a hooks directory
		return "", fmt.Errorf("create hooks dir: %w", err)
	}

	// Resolve symlinks in maestroDir so all derived paths are canonical.
	// On macOS, /tmp is a symlink to /private/tmp; without resolution,
	// the hook script's runtime pwd -P and the embedded project root
	// could mismatch, causing false WT001 rejections.
	maestroDir := pc.maestroDir
	if resolved, err := filepath.EvalSymlinks(maestroDir); err == nil {
		maestroDir = resolved
	}
	projectRoot := filepath.Dir(maestroDir)
	maestroBinary := opts.MaestroBinary
	if maestroBinary == "" {
		maestroBinary = "maestro"
	}
	implementation := opts.Implementation
	if implementation == "" {
		implementation = policyHookImplementationBash
	}

	var script string
	switch implementation {
	case policyHookImplementationBash:
		script = renderBashPolicyScript(projectRoot, maestroBinary, false)
	case policyHookImplementationShadow:
		script = renderBashPolicyScript(projectRoot, maestroBinary, true)
	case policyHookImplementationGo:
		script = renderGoPolicyWrapper(projectRoot, maestroBinary)
	default:
		return "", fmt.Errorf("unknown worker policy hook implementation %q", implementation)
	}

	scriptPath := pc.hookScriptPath()
	if err := os.WriteFile(scriptPath, []byte(script), 0750); err != nil { //nolint:gosec // hook script requires execute permission
		return "", fmt.Errorf("write hook script: %w", err)
	}

	return scriptPath, nil
}

func renderBashPolicyScript(projectRoot, maestroBinary string, shadow bool) string {
	shadowDefault := "0"
	if shadow {
		shadowDefault = "1"
	}
	script := strings.ReplaceAll(hookScript, "__PROJECT_ROOT__", shellQuote(projectRoot))
	script = strings.ReplaceAll(script, "__MAESTRO_POLICY_CHECK_BIN__", shellQuote(maestroBinary))
	script = strings.ReplaceAll(script, "__MAESTRO_POLICY_SHADOW_DEFAULT__", shadowDefault)
	return script
}

func renderGoPolicyWrapper(projectRoot, maestroBinary string) string {
	return strings.TrimSpace(fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

input="$(cat)"
project_root=%s
maestro_bin=%s

run_on_main="0"
if [ -n "${TMUX_PANE:-}" ]; then
  if command -v tmux >/dev/null 2>&1; then
    if _flag="$(tmux display-message -t "$TMUX_PANE" -p '#{@run_on_main}' 2>/dev/null)"; then
      if [ "$_flag" = "1" ]; then
        run_on_main="1"
      fi
    else
      run_on_main="1"
    fi
  else
    run_on_main="1"
  fi
fi

args=("hook" "policy-check" "--project-root" "$project_root")
if [ "$run_on_main" = "1" ]; then
  args+=("--run-on-main")
fi

if ! output="$(printf '%%s' "$input" | "$maestro_bin" "${args[@]}" 2>&1)"; then
  echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Policy hook Go checker failed. Denying for safety."}}'
  exit 0
fi
if command -v jq >/dev/null 2>&1 && [ "$(printf '%%s' "$output" | jq -r '.allow // empty' 2>/dev/null || true)" = "true" ]; then
  exit 0
fi
printf '%%s\n' "$output"
`, shellQuote(projectRoot), shellQuote(maestroBinary))) + "\n"
}

// hookSettingsJSON is the settings JSON structure for hook overrides.
// All hook types are optional; omitted keys are left unmodified by Claude Code.
type hookSettingsJSON struct {
	Hooks hookSettingsHooks `json:"hooks"`
}

type hookSettingsHooks struct {
	Notification *[]any             `json:"Notification,omitempty"`
	PreToolUse   []hookMatcherGroup `json:"PreToolUse,omitempty"`
}

type hookMatcherGroup struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// HookSettings returns the --settings JSON string that configures the
// PreToolUse hook for Workers, with Notification hooks disabled.
// This produces a single merged JSON so that only one --settings flag is needed.
//
// Sandbox settings are intentionally omitted: passing sandbox config via
// --settings overrides the user's global sandbox.enabled:false and prevents
// the /sandbox command from working. See launcher.go buildLaunchArgs for details.
func (pc *PolicyChecker) HookSettings(scriptPath string) (string, error) {
	emptyNotification := []any{}
	settings := hookSettingsJSON{}
	settings.Hooks.Notification = &emptyNotification
	settings.Hooks.PreToolUse = []hookMatcherGroup{
		{
			Matcher: "Bash|Write|Edit",
			Hooks: []hookEntry{
				{
					Type:    "command",
					Command: scriptPath,
					Timeout: 10,
				},
			},
		},
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("marshal hook settings: %w", err)
	}
	return string(b), nil
}

// shellQuote safely quotes a string for embedding in a shell script.
// It wraps the string in single quotes, escaping internal single quotes
// using the standard '\” technique (end quote, literal quote, start quote).
// Inside single quotes all shell metacharacters ($, `, ", \, etc.) are literal.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookScriptRaw holds the verbatim PreToolUse policy hook source. The shell
// script lives next to this file as worker_policy_hook.sh so it can be
// edited with full editor / shell-lint support; F-025 step 1 (decoupling
// readability from the eventual Go-side rewrite). The string is later run
// through strings.TrimSpace + a trailing newline so the on-disk hook script
// matches byte-for-byte what the previous embedded raw-string produced.
//
//go:embed worker_policy_hook.sh
var hookScriptRaw string

// hookScript is the normalized hook script body written to disk. Wrapping
// hookScriptRaw with TrimSpace + "\n" keeps the previous semantics (strip
// any embed-introduced leading/trailing whitespace, end with a single
// newline) so existing tests against the on-disk script content stay valid.
var hookScript = strings.TrimSpace(hookScriptRaw) + "\n"
