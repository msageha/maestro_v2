package agent

import (
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

	projectRoot := filepath.Dir(pc.maestroDir)
	if resolved, err := filepath.EvalSymlinks(projectRoot); err == nil {
		projectRoot = resolved
	}
	script := strings.ReplaceAll(hookScript, "__PROJECT_ROOT__", shellQuote(projectRoot))

	scriptPath := pc.hookScriptPath()
	if err := os.WriteFile(scriptPath, []byte(script), 0750); err != nil { //nolint:gosec // hook script requires execute permission
		return "", fmt.Errorf("write hook script: %w", err)
	}

	return scriptPath, nil
}

// hookSettingsJSON is the settings JSON structure for hook overrides.
// All hook types are optional; omitted keys are left unmodified by Claude Code.
type hookSettingsJSON struct {
	Sandbox *hookSettingsSandbox `json:"sandbox,omitempty"`
	Hooks   hookSettingsHooks    `json:"hooks"`
}

type hookSettingsSandbox struct {
	Network hookSettingsSandboxNetwork `json:"network"`
}

type hookSettingsSandboxNetwork struct {
	AllowAllUnixSockets bool `json:"allowAllUnixSockets"`
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
func (pc *PolicyChecker) HookSettings(scriptPath string) (string, error) {
	emptyNotification := []any{}
	settings := hookSettingsJSON{}
	settings.Sandbox = &hookSettingsSandbox{
		Network: hookSettingsSandboxNetwork{AllowAllUnixSockets: true},
	}
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
// using the standard '\'' technique (end quote, literal quote, start quote).
// Inside single quotes all shell metacharacters ($, `, ", \, etc.) are literal.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookScript is the shell script content for the PreToolUse policy hook.
// It reads JSON from stdin, checks for dangerous patterns, and outputs
// a deny decision if a violation is detected.
var hookScript = strings.TrimSpace(`
#!/usr/bin/env bash
set -euo pipefail

# Worker PreToolUse policy enforcement hook.
# Blocks destructive operations defined in Tier 1/Tier 2 safety rules.

# S3: jq dependency check - deny all if jq is unavailable (fail-safe)
if ! command -v jq >/dev/null 2>&1; then
  echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Policy hook requires jq but it is not installed. Denying for safety."}}'
  exit 0
fi

input="$(cat)"
tool_name="$(echo "$input" | jq -r '.tool_name // ""')"

deny() {
  local reason="$1"
  jq -nc --arg r "$reason" '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":$r}}'
  exit 0
}

# --- Bash command checks ---
if [ "$tool_name" = "Bash" ]; then
  cmd="$(echo "$input" | jq -r '.tool_input.command // ""')"

  # D001: OS/home/root destruction (case-insensitive for macOS)
  # Match both rm -rf and rm -fr (and variants like -fR, -Rf, -rRf, etc.)
  if echo "$cmd" | grep -qiE 'rm\s+-[a-zA-Z]*[rR][a-zA-Z]*f[a-zA-Z]*\s+(/\s|/$|~|/Users)' || \
     echo "$cmd" | grep -qiE 'rm\s+-[a-zA-Z]*f[a-zA-Z]*[rR][a-zA-Z]*\s+(/\s|/$|~|/Users)'; then
    deny "D001: Blocked rm -rf targeting system/home directory"
  fi

  # D001: Long-form flags (--recursive --force / --force --recursive)
  if echo "$cmd" | grep -qiE 'rm\s+.*--(recursive|force)\s+.*--(recursive|force).*\s+(/\s|/$|~|/Users)'; then
    deny "D001: Blocked rm --recursive --force targeting system/home directory"
  fi

  # D002: Recursive delete outside project root
  if echo "$cmd" | grep -qE 'rm\s+(-[a-zA-Z]*[rR]|--recursive)'; then
    project_root=__PROJECT_ROOT__
    set -f
    for word in $cmd; do
      case "$word" in
        /*|~*|../*|*/../*) ;;
        *) continue ;;
      esac
      resolved="$(realpath "$word" 2>/dev/null || echo "$word")"
      case "$resolved" in
        "$project_root"/*) ;;
        *) deny "D002: Blocked recursive delete outside project root: $word" ;;
      esac
    done
    set +f
  fi

  # Worker: git push is fully prohibited (all forms including --force-with-lease)
  # This supersedes D003 (git push --force) for Workers, as worker.md
  # prohibits all git push operations without exception.
  if echo "$cmd" | grep -qE 'git\s+push(\s|$)'; then
    deny "Worker git push is prohibited (all git push operations are blocked for Workers)"
  fi

  # Note: git merge --abort is not listed in this hook. Workers never run
  # git merge themselves -- merge orchestration (and any abort) is performed
  # by the daemon via the worktree manager (resolve_conflict / resume_merge
  # plan ops). If a worker ever attempts git merge --abort directly, it
  # indicates a bug or protocol violation and should surface as a normal
  # command error rather than be silently allowed by this hook.

  # D004: Uncommitted work destruction
  if echo "$cmd" | grep -qE 'git\s+reset\s+--hard'; then
    deny "D004: Blocked git reset --hard (use git stash)"
  fi
  if echo "$cmd" | grep -qE 'git\s+checkout\s+--\s+\.'; then
    deny "D004: Blocked git checkout -- . (destroys uncommitted changes)"
  fi
  if echo "$cmd" | grep -qE 'git\s+clean\s+-[a-zA-Z]*f' && \
     ! echo "$cmd" | grep -qE 'git\s+clean\s+-[a-zA-Z]*n'; then
    deny "D004: Blocked git clean -f (use git clean -n for dry run first)"
  fi

  # D005: Privilege escalation
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*sudo\s'; then
    deny "D005: Blocked sudo (privilege escalation)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*su\s'; then
    deny "D005: Blocked su (privilege escalation)"
  fi
  # D005: chmod -R targeting system paths (privilege escalation)
  if echo "$cmd" | grep -qE 'chmod\s+(-[a-zA-Z]*R[a-zA-Z]*|-R)\s' && \
     echo "$cmd" | grep -qiE '/(System|Library|Applications|usr|bin|sbin|etc)(/|\s|$)'; then
    deny "D005: Blocked chmod -R targeting system path"
  fi

  # D006: Process/infra destruction
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*kill(all)?\s'; then
    deny "D006: Blocked kill/killall (process destruction)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*pkill\s'; then
    deny "D006: Blocked pkill (process destruction)"
  fi

  # D007: Disk destruction
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*(mkfs|fdisk)\s'; then
    deny "D007: Blocked disk destruction command"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*dd\s+if='; then
    deny "D007: Blocked dd (disk destruction)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*diskutil\s+eraseDisk'; then
    deny "D007: Blocked diskutil eraseDisk"
  fi

  # D008: Remote code execution via pipe
  if echo "$cmd" | grep -qE '(curl|wget)\s.*\|\s*(ba)?sh'; then
    deny "D008: Blocked remote code execution (curl/wget piped to shell)"
  fi

  # B001: Pipe to shell (indirect execution bypass for bash --restricted)
  if echo "$cmd" | grep -qE '\|\s*(/usr)?/bin/(ba)?sh\b' || \
     echo "$cmd" | grep -qE '\|\s*(bash|sh)\s*($|;|\||&|>|<)' || \
     echo "$cmd" | grep -qE '\|\s*(bash|sh)\s+-'; then
    deny "B001: Blocked pipe to shell (restricted mode bypass)"
  fi

  # B002: Shell -c flag (unrestricted shell spawn)
  if echo "$cmd" | grep -qE '\b(bash|sh)\s+-[a-zA-Z]*c\b'; then
    deny "B002: Blocked shell -c execution (restricted mode bypass)"
  fi

  # B003: eval command (arbitrary command execution)
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*eval\s+'; then
    deny "B003: Blocked eval (arbitrary command execution)"
  fi

  # D009: Operator-only recovery API escape hatches.
  # Workers must never invoke these even if launcher --disallowedTools is
  # bypassed; the daemon enforces an additional role check, but rejecting
  # at the hook layer gives a faster, clearer failure.
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+plan\s+unquarantine(\s|$)'; then
    deny "D009: Blocked maestro plan unquarantine (operator-only recovery API)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+plan\s+resume-merge(\s|$)'; then
    deny "D009: Blocked maestro plan resume-merge (operator-only recovery API)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+resolve-conflict(\s|$)'; then
    deny "D009: Blocked maestro resolve-conflict (operator-only recovery API)"
  fi

  # B004: Absolute path shell invocation (bypasses restricted mode)
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*(/usr)?/bin/(ba)?sh\b'; then
    deny "B004: Blocked absolute path shell invocation"
  fi

  # .maestro/ access via Bash (bypass prevention, case-insensitive for macOS)
  if echo "$cmd" | grep -qiE '(cat|head|tail|less|more|vim|nano|sed|awk)\s+.*\.maestro/(state|queues|results|locks|logs|config|dashboard)'; then
    deny "Blocked .maestro/ control-plane access via Bash"
  fi
  if echo "$cmd" | grep -qiE '(ls|find|grep|rg)\s+.*\.maestro/(state|queues|results|locks|logs|config|dashboard)'; then
    deny "Blocked .maestro/ control-plane access via Bash"
  fi
  if echo "$cmd" | grep -qiE '(echo|printf|tee)\s.*>\s*\.maestro/'; then
    deny "Blocked write to .maestro/ via Bash"
  fi
  if echo "$cmd" | grep -qiE '>\s*\.maestro/'; then
    deny "Blocked redirect to .maestro/ via Bash"
  fi

  # macOS system directory protection (case-insensitive)
  if echo "$cmd" | grep -qiE 'rm\s+(-[a-zA-Z]*r|--recursive).*/(System|Library|Applications)/'; then
    deny "Blocked recursive delete targeting macOS system directory"
  fi
fi

# --- Write/Edit path checks ---
if [ "$tool_name" = "Write" ] || [ "$tool_name" = "Edit" ]; then
  file_path="$(echo "$input" | jq -r '.tool_input.file_path // ""')"
  # Normalize to lowercase for case-insensitive FS (macOS)
  file_path_lower="$(echo "$file_path" | tr '[:upper:]' '[:lower:]')"

  # Block writes to .maestro/ control plane (absolute and relative paths)
  case "$file_path_lower" in
    */.maestro/state/*|*/.maestro/queues/*|*/.maestro/results/*|*/.maestro/locks/*|*/.maestro/logs/*|*/.maestro/hooks/*|*/.maestro/config.yaml|*/.maestro/dashboard.md)
      deny "Blocked write to .maestro/ control-plane path"
      ;;
    .maestro/state/*|.maestro/queues/*|.maestro/results/*|.maestro/locks/*|.maestro/logs/*|.maestro/hooks/*|.maestro/config.yaml|.maestro/dashboard.md)
      deny "Blocked write to .maestro/ control-plane path (relative)"
      ;;
  esac

  # Block writes to macOS system directories (case-insensitive)
  case "$file_path_lower" in
    /system/*|/library/*|/applications/*|/usr/*|/bin/*|/sbin/*)
      deny "Blocked write to system directory: $file_path"
      ;;
  esac

  # WT001: Worktree boundary enforcement.
  # When the Worker's CWD is inside .maestro/worktrees/, all Write/Edit
  # operations must target paths within that CWD. This prevents the Worker
  # (Claude LLM) from writing to the repo root instead of the worktree,
  # which would cause auto_commit to see no changes and integration to stall.
  worker_cwd="$(pwd 2>/dev/null || echo "")"
  if [ -n "$worker_cwd" ] && echo "$worker_cwd" | grep -qF '/.maestro/worktrees/'; then
    case "$file_path" in
      "$worker_cwd"/*) ;; # OK: within worktree
      *) deny "WT001: Write outside worktree boundary: $file_path is not within working directory $worker_cwd" ;;
    esac
  fi
fi

# Allow: no output, exit 0
exit 0
`) + "\n"
