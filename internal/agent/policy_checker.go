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
// enforce maestro role policy for claude-code agents.
//
// The hook script is written to .maestro/hooks/ and referenced in the Claude
// Code --settings JSON. It matches every tool call so the hook can return an
// explicit allow for otherwise-unrecognized tools in downgraded default mode.
type PolicyChecker struct {
	maestroDir string
}

// NewPolicyChecker creates a PolicyChecker for the given maestro directory.
func NewPolicyChecker(maestroDir string) *PolicyChecker {
	return &PolicyChecker{maestroDir: maestroDir}
}

// hookScriptPath returns the filesystem path for the policy hook script.
func (pc *PolicyChecker) hookScriptPath() string {
	// Keep the historical filename for compatibility. The script now serves
	// worker, planner, and orchestrator roles.
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
// every claude-code managed role, so it has to be expressed as a `--settings`
// payload.
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
	Hooks   hookSettingsHooks    `json:"hooks"`
	Sandbox *hookSettingsSandbox `json:"sandbox,omitempty"`
}

type hookSettingsHooks struct {
	PreToolUse []hookMatcherGroup `json:"PreToolUse,omitempty"`
}

type hookSettingsSandbox struct {
	Filesystem *hookSettingsSandboxFilesystem `json:"filesystem,omitempty"`
	Network    *hookSettingsSandboxNetwork    `json:"network,omitempty"`
}

type hookSettingsSandboxFilesystem struct {
	AllowWrite []string `json:"allowWrite,omitempty"`
}

type hookSettingsSandboxNetwork struct {
	AllowAllUnixSockets bool `json:"allowAllUnixSockets"`
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
// PreToolUse policy hook for a claude-code managed role. Notification / Stop
// suppression has been removed: see the hookSettingsJSON comment above for
// rationale. This produces a single --settings flag so that the existing argv
// plumbing stays simple.
//
// Sandbox settings add sandbox.network.allowAllUnixSockets (so the maestro CLI
// can reach the daemon UDS while sandboxed) and filesystem.allowWrite entries
// for Maestro-managed toolchain caches. Claude Code merges sandbox keys
// individually and merges filesystem.allowWrite additively (verified
// 2026-07-03/04), so omitting sandbox.enabled preserves the user / managed
// layer's value: this payload does not start the sandbox on non-sandbox
// machines and does not affect /sandbox there. In org-managed environments
// sandbox.enabled is already forced, so allowing the UDS keeps maestro CLI
// calls promptless and the extra writable cache paths prevent common toolchain
// EPERM failures, complementing buildLaunchEnvForAgent, which points cache env
// vars at <maestroDir>/cache.
func (pc *PolicyChecker) HookSettings(scriptPath, role string) (string, error) {
	if !knownRoles[role] {
		return "", fmt.Errorf("unknown policy role %q", role)
	}
	settings := hookSettingsJSON{}
	// Always allow Unix-domain sockets inside the OS sandbox so the maestro CLI
	// can connect to the daemon UDS from a sandboxed pane without a
	// dangerouslyDisableSandbox rewrite. In managed/enterprise environments
	// where remote settings force sandbox.enabled=true and
	// permissions.disableBypassPermissionsMode=disable, unsandboxing a maestro
	// CLI call surfaces an approval prompt that no hook `allow` can
	// short-circuit, wedging the pane. Keeping the call sandboxed + allowing the
	// UDS avoids the wedge while preserving the filesystem denyRead secret
	// barrier. Sandbox keys merge individually and sandbox.enabled is left
	// untouched, so this does not start the sandbox on non-sandbox machines.
	settings.Sandbox = &hookSettingsSandbox{
		Network: &hookSettingsSandboxNetwork{AllowAllUnixSockets: true},
	}
	if allowWrite := pc.sandboxFilesystemAllowWrite(); len(allowWrite) > 0 {
		settings.Sandbox.Filesystem = &hookSettingsSandboxFilesystem{AllowWrite: allowWrite}
	}
	// Match every tool. When managed settings downgrade
	// --dangerously-skip-permissions to default permission mode, any tool call
	// outside the operator machine's allowlists prompts. The hook script
	// explicitly allows any tool it does not recognize, restoring Maestro's
	// intended unattended operating mode. Org-managed deny rules still take
	// precedence over hook allow (verified 2026-07-03/04), and Claude's
	// hardcoded protected-path confirmations remain in force.
	settings.Hooks.PreToolUse = []hookMatcherGroup{
		{
			Matcher: ".*",
			Hooks: []hookEntry{
				{
					Type:    "command",
					Command: shellQuote(scriptPath) + " " + role,
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

func (pc *PolicyChecker) sandboxFilesystemAllowWrite() []string {
	if pc.maestroDir == "" {
		return nil
	}
	var paths []string
	add := func(path string) {
		if path == "" {
			return
		}
		path = filepath.Clean(path)
		for _, existing := range paths {
			if existing == path {
				return
			}
		}
		paths = append(paths, path)
	}

	if abs, err := filepath.Abs(pc.maestroDir); err == nil {
		add(filepath.Join(abs, "cache"))
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			add(filepath.Join(resolved, "cache"))
		}
	}
	add("~/.cache")
	add("~/Library/Caches")
	return paths
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
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
