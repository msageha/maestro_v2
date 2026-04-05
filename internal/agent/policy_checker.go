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
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create hooks dir: %w", err)
	}

	scriptPath := pc.hookScriptPath()
	if err := os.WriteFile(scriptPath, []byte(hookScript), 0755); err != nil {
		return "", fmt.Errorf("write hook script: %w", err)
	}

	return scriptPath, nil
}

// hookSettingsJSON is the settings JSON structure for hook overrides.
// All hook types are optional; omitted keys are left unmodified by Claude Code.
type hookSettingsJSON struct {
	Hooks hookSettingsHooks `json:"hooks"`
}

type hookSettingsHooks struct {
	Notification *[]any      `json:"Notification,omitempty"`
	PreToolUse   []hookMatcherGroup  `json:"PreToolUse,omitempty"`
}

type hookMatcherGroup struct {
	Matcher string       `json:"matcher"`
	Hooks   []hookEntry  `json:"hooks"`
}

type hookEntry struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout"`
}

// HookSettings returns the --settings JSON string that configures the
// PreToolUse hook for Workers, with Notification hooks disabled.
// This produces a single merged JSON so that only one --settings flag is needed.
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

// hookScript is the shell script content for the PreToolUse policy hook.
// It reads JSON from stdin, checks for dangerous patterns, and outputs
// a deny decision if a violation is detected.
var hookScript = strings.TrimSpace(`
#!/usr/bin/env bash
set -euo pipefail

# Worker PreToolUse policy enforcement hook.
# Blocks destructive operations defined in Tier 1/Tier 2 safety rules.

input="$(cat)"
tool_name="$(echo "$input" | jq -r '.tool_name // ""')"

deny() {
  local reason="$1"
  echo "{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"$reason\"}}"
  exit 0
}

# --- Bash command checks ---
if [ "$tool_name" = "Bash" ]; then
  cmd="$(echo "$input" | jq -r '.tool_input.command // ""')"

  # D001: OS/home/root destruction (case-insensitive for macOS)
  if echo "$cmd" | grep -qiE 'rm\s+-[a-zA-Z]*r[a-zA-Z]*f[a-zA-Z]*\s+(/\s|/$|~|/Users)'; then
    deny "D001: Blocked rm -rf targeting system/home directory"
  fi

  # D003: git push --force (without --force-with-lease)
  if echo "$cmd" | grep -qE 'git\s+push\s+.*--force(\s|$)' && \
     ! echo "$cmd" | grep -qE 'git\s+push\s+.*--force-with-lease'; then
    deny "D003: Blocked git push --force (use --force-with-lease)"
  fi
  if echo "$cmd" | grep -qE 'git\s+push\s+(.*\s)?-f(\s|$)' && \
     ! echo "$cmd" | grep -qE 'git\s+push\s+.*--force-with-lease'; then
    deny "D003: Blocked git push -f (use --force-with-lease)"
  fi

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

  # B004: Absolute path shell invocation (bypasses restricted mode)
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*(/usr)?/bin/(ba)?sh\b'; then
    deny "B004: Blocked absolute path shell invocation"
  fi

  # .maestro/ access via Bash (bypass prevention, case-insensitive for macOS)
  if echo "$cmd" | grep -qiE '(cat|head|tail|less|more|vim|nano|sed|awk)\s+.*\.maestro/(state|queues|results|locks|logs|config)'; then
    deny "Blocked .maestro/ control-plane access via Bash"
  fi
  if echo "$cmd" | grep -qiE '(ls|find|grep|rg)\s+.*\.maestro/(state|queues|results|locks|logs|config)'; then
    deny "Blocked .maestro/ control-plane access via Bash"
  fi
  if echo "$cmd" | grep -qiE '(echo|printf|tee)\s.*>\s*\.maestro/'; then
    deny "Blocked write to .maestro/ via Bash"
  fi
  if echo "$cmd" | grep -qiE '>\s*\.maestro/'; then
    deny "Blocked redirect to .maestro/ via Bash"
  fi

  # macOS system directory protection (case-insensitive)
  if echo "$cmd" | grep -qiE 'rm\s+-[a-zA-Z]*r.*/(System|Library|Applications)/'; then
    deny "Blocked recursive delete targeting macOS system directory"
  fi
fi

# --- Write/Edit path checks ---
if [ "$tool_name" = "Write" ] || [ "$tool_name" = "Edit" ]; then
  file_path="$(echo "$input" | jq -r '.tool_input.file_path // ""')"
  # Normalize to lowercase for case-insensitive FS (macOS)
  file_path_lower="$(echo "$file_path" | tr '[:upper:]' '[:lower:]')"

  # Block writes to .maestro/ control plane
  case "$file_path_lower" in
    */.maestro/state/*|*/.maestro/queues/*|*/.maestro/results/*|*/.maestro/locks/*|*/.maestro/logs/*|*/.maestro/config.yaml)
      deny "Blocked write to .maestro/ control-plane path"
      ;;
  esac

  # Block writes to macOS system directories (case-insensitive)
  case "$file_path_lower" in
    /system/*|/library/*|/applications/*|/usr/*|/bin/*|/sbin/*)
      deny "Blocked write to system directory: $file_path"
      ;;
  esac
fi

# Allow: no output, exit 0
exit 0
`) + "\n"
