package agent

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// hookScriptPath returns the filesystem path for the policy hook script.
func (pc *PolicyChecker) hookScriptPath() string {
	return filepath.Join(pc.maestroDir, "hooks", "worker-policy.sh")
}

// WriteHookScript writes the policy enforcement shell script to disk.
// Returns the script path. The script is idempotently overwritten.
func (pc *PolicyChecker) WriteHookScript() (string, error) {
	dir := filepath.Join(pc.maestroDir, "hooks")
	if err := os.MkdirAll(dir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for a hooks directory
		return "", fmt.Errorf("create hooks dir: %w", err)
	}

	// The hook enforces .maestro/ control-plane and worktree boundaries via
	// path globs and the runtime `pwd -P`, so it needs no embedded project
	// root. (A former __PROJECT_ROOT__ placeholder / project_root shell var
	// was dead — the glob-based checks superseded it — and has been removed.)
	scriptPath := pc.hookScriptPath()
	if err := os.WriteFile(scriptPath, []byte(hookScript), 0750); err != nil { //nolint:gosec // hook script requires execute permission
		return "", fmt.Errorf("write hook script: %w", err)
	}

	return scriptPath, nil
}

// hookSettingsJSON is the settings JSON structure used for hook
// overrides. Only PreToolUse is emitted by Maestro: it is the
// destructive-action policy gate that the daemon authors and binds to
// every Worker, so it has to be expressed as a `--settings` payload.
//
// Notification / Stop are explicitly NOT touched here. Earlier
// versions tried to suppress them with empty arrays so per-turn
// verifier scripts (`~/.claude/settings.json` Stop hooks invoking a
// project-local lint runner) would not run inside Maestro panes, but
// Claude Code's `--settings` flag merges with the user layer rather
// than replacing it — empty arrays did not override the operator's
// hooks (Report 2026-05-06 Q-1 confirmed this in production). Rather
// than ship ineffective code, we leave hook suppression as a
// `~/.claude` responsibility: operators who don't want their Stop
// hook running in Maestro panes scope it themselves at the user /
// project settings layer.
type hookSettingsJSON struct {
	Hooks hookSettingsHooks `json:"hooks"`
}

type hookSettingsHooks struct {
	PreToolUse []hookMatcherGroup `json:"PreToolUse,omitempty"`
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
// PreToolUse policy hook for Workers. Notification / Stop suppression
// has been removed: see the hookSettingsJSON comment above for
// rationale. This produces a single --settings flag so that the
// existing argv plumbing stays simple.
//
// Sandbox settings are intentionally omitted: passing sandbox config via
// --settings overrides the user's global sandbox.enabled:false and prevents
// the /sandbox command from working. See launcher.go buildLaunchArgs for details.
func (pc *PolicyChecker) HookSettings(scriptPath string) (string, error) {
	settings := hookSettingsJSON{}
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

// hookScriptRaw holds the verbatim PreToolUse policy hook source. The shell
// script lives next to this file as worker_policy_hook.sh so it can be
// edited with full editor / shell-lint support. The string is later run
// through strings.TrimSpace + a trailing newline so the on-disk hook script
// ends with exactly one newline.
//
//go:embed worker_policy_hook.sh
var hookScriptRaw string

// hookScript is the normalized hook script body written to disk. Wrapping
// hookScriptRaw with TrimSpace + "\n" keeps the previous semantics (strip
// any embed-introduced leading/trailing whitespace, end with a single
// newline) so existing tests against the on-disk script content stay valid.
var hookScript = strings.TrimSpace(hookScriptRaw) + "\n"
