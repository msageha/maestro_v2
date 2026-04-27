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
project_root=__PROJECT_ROOT__
default_policy_bin=__MAESTRO_POLICY_CHECK_BIN__

shadow_policy_check() {
  local bash_allow="$1"
  local bash_reason="${2:-}"
  if [ "${MAESTRO_POLICY_SHADOW:-__MAESTRO_POLICY_SHADOW_DEFAULT__}" != "1" ]; then
    return 0
  fi

  local policy_bin="${MAESTRO_POLICY_CHECK_BIN:-$default_policy_bin}"
  local -a policy_args=("hook" "policy-check" "--project-root" "$project_root")
  if [ "${run_on_main:-0}" = "1" ]; then
    policy_args+=("--run-on-main")
  fi

  local go_output
  if ! go_output="$(printf '%s' "$input" | "$policy_bin" "${policy_args[@]}" 2>&1)"; then
    printf 'maestro_policy_shadow_error command=%q error=%q\n' "$policy_bin" "$go_output" >&2
    return 0
  fi

  local go_allow
  go_allow="$(printf '%s' "$go_output" | jq -r '.allow // empty' 2>/dev/null || true)"
  if [ "$go_allow" != "true" ] && [ "$go_allow" != "false" ]; then
    printf 'maestro_policy_shadow_error command=%q malformed_output=%q\n' "$policy_bin" "$go_output" >&2
    return 0
  fi

  local go_reason
  go_reason="$(printf '%s' "$go_output" | jq -r '.reason // ""' 2>/dev/null || true)"
  if [ "$go_allow" != "$bash_allow" ] || { [ "$bash_allow" = "false" ] && [ "$go_reason" != "$bash_reason" ]; }; then
    printf 'maestro_policy_shadow_divergence bash_allow=%s go_allow=%s bash_reason=%q go_reason=%q tool=%q\n' \
      "$bash_allow" "$go_allow" "$bash_reason" "$go_reason" "$tool_name" >&2
  fi
}

deny() {
  local reason="$1"
  shadow_policy_check false "$reason"
  jq -nc --arg r "$reason" '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":$r}}'
  exit 0
}

# --- RUN_ON_MAIN flag detection (shared by Bash + Write/Edit branches) ---
#
# The Daemon stamps @run_on_main=1 on the pane via tmux user variables before
# dispatching a run_on_main task and clears it (sets to "") on the next
# regular dispatch. The flag means: "this Worker is pointed at the main
# worktree for read-only verification — block any mutation".
#
# Fail policy:
#   - flag = "1"          → run_on_main mode (mutations DENY)
#   - flag = "" or unset  → normal mode (mutations OK as long as other rules pass)
#   - TMUX_PANE unset     → not in tmux at all; cannot be in run_on_main mode
#                           (the daemon only dispatches via tmux)
#   - TMUX_PANE set but tmux missing or display-message fails → fail-CLOSED:
#                           we are clearly inside a tmux pane the daemon owns,
#                           but cannot verify the flag, so the safe default is
#                           to assume run_on_main is in effect.
#
# F-026 design trade-off (intentionally NOT replaced with $MAESTRO_RUN_ON_MAIN):
#   The reviewer suggested passing run_on_main via an environment variable so
#   the hook does not depend on tmux. However, panes are reused across tasks
#   (Worker pane is long-lived), so a process-level env var would have to be
#   re-exported on every dispatch — which would require shipping export
#   commands through tmux send-keys anyway, reintroducing the same dependency
#   chain that env was supposed to remove. The current tmux user-variable
#   path has the dispatcher set the value atomically per-dispatch via
#   paneIO.SetUserVar (executor_core.go); a tmux outage causes the hook to
#   fail-closed, which is the safer default than any "stale env value left
#   over from the prior task" failure mode an env-based approach would
#   introduce.
run_on_main="0"
if [ -n "${TMUX_PANE:-}" ]; then
  if command -v tmux >/dev/null 2>&1; then
    if _flag="$(tmux display-message -t "$TMUX_PANE" -p '#{@run_on_main}' 2>/dev/null)"; then
      if [ "$_flag" = "1" ]; then
        run_on_main="1"
      fi
    else
      # tmux is installed but display-message failed. We are inside a tmux
      # pane managed by the daemon and cannot read the flag; fail closed
      # so a run_on_main task cannot mutate main due to a tmux glitch.
      run_on_main="1"
    fi
  else
    # Inside a tmux pane (TMUX_PANE set) but tmux binary missing — fail closed.
    run_on_main="1"
  fi
fi

# --- Bash command checks ---
if [ "$tool_name" = "Bash" ]; then
  cmd="$(echo "$input" | jq -r '.tool_input.command // ""')"

  # RUN_ON_MAIN: read-only verification mode also applies to Bash. The
  # Write/Edit hooks only catch the Claude Code Write/Edit tools -- a Worker
  # that runs sed -i, cp, go install, package installers, or shell
  # redirections via Bash would otherwise mutate the main worktree it is
  # supposed to be inspecting. The list below covers the common mutation
  # vectors; verbs already blocked elsewhere (git push, sudo, rm -rf, etc.)
  # remain blocked by their own rules and do not need to be listed here.
  if [ "$run_on_main" = "1" ]; then
    # File-mutating coreutils (any form, including absolute paths).
    if echo "$cmd" | grep -qE '(^|[;|&(])\s*(/[A-Za-z0-9_./-]+/)?(cp|mv|rm|mkdir|rmdir|touch|chmod|chown|chgrp|ln|install)\b'; then
      deny "RUN_ON_MAIN: Bash file-mutating command blocked (read-only verification mode)"
    fi
    # In-place editors.
    if echo "$cmd" | grep -qE '\b(sed|gsed)\s+(-[a-zA-Z]*i|--in-place)\b'; then
      deny "RUN_ON_MAIN: Bash in-place edit blocked (read-only verification mode)"
    fi
    if echo "$cmd" | grep -qE '\bperl\s+(-[a-zA-Z]*[ip]|--in-place)\b'; then
      deny "RUN_ON_MAIN: Bash perl in-place edit blocked (read-only verification mode)"
    fi
    # Output redirection that writes a file.
    # Match >, >>, &> when followed by a path-like token (not >&N file-descriptor dup).
    if echo "$cmd" | grep -qE '(^|[^0-9&])>{1,2}[[:space:]]*[A-Za-z0-9_./~$-]'; then
      deny "RUN_ON_MAIN: Bash output redirection to file blocked (read-only verification mode)"
    fi
    if echo "$cmd" | grep -qE '\|\s*tee\b'; then
      deny "RUN_ON_MAIN: Bash tee blocked (read-only verification mode)"
    fi
    # Mutating git verbs. Read-only forms (status, log, diff, show, branch -v,
    # rev-parse, etc.) are deliberately not in this list.
    if echo "$cmd" | grep -qE '(^|[;|&])\s*git\s+(commit|add|merge|rebase|cherry-pick|revert|stash|restore|fetch|pull|push|worktree|tag|reset|checkout|clean|am|apply|format-patch|mv|rm|init)(\s|$)'; then
      deny "RUN_ON_MAIN: Bash git mutation blocked (read-only verification mode)"
    fi
    # Package / dependency installers and code generators.
    if echo "$cmd" | grep -qE '\b(npm|pnpm|yarn)\s+(install|i|ci|add|remove|uninstall|update|upgrade)\b'; then
      deny "RUN_ON_MAIN: Bash JS package mutation blocked (read-only verification mode)"
    fi
    if echo "$cmd" | grep -qE '\b(pip|pip3)\s+(install|uninstall|wheel)\b'; then
      deny "RUN_ON_MAIN: Bash Python package mutation blocked (read-only verification mode)"
    fi
    if echo "$cmd" | grep -qE '\bcargo\s+(install|build|run|update|fetch|generate)\b'; then
      deny "RUN_ON_MAIN: Bash cargo mutation blocked (read-only verification mode)"
    fi
    if echo "$cmd" | grep -qE '\bgo\s+(install|generate|mod\s+(tidy|download|edit|init))\b'; then
      deny "RUN_ON_MAIN: Bash go install/generate/mod blocked (read-only verification mode)"
    fi
    if echo "$cmd" | grep -qE '\b(make|gmake)\b'; then
      # Allow make -n / make --dry-run (no actual side effects).
      if ! echo "$cmd" | grep -qE '\bmake\b.*\s(-n\b|--dry-run\b|--just-print\b)'; then
        deny "RUN_ON_MAIN: Bash make blocked except --dry-run (read-only verification mode)"
      fi
    fi
    # Archive extraction that writes files.
    if echo "$cmd" | grep -qE '\b(tar|gtar)\s+.*-[a-zA-Z]*x'; then
      deny "RUN_ON_MAIN: Bash archive extraction blocked (read-only verification mode)"
    fi
    if echo "$cmd" | grep -qE '\bunzip\b|\bgunzip\b|\b7z\s+x\b'; then
      deny "RUN_ON_MAIN: Bash archive extraction blocked (read-only verification mode)"
    fi
  fi

  # C1: Shell expansion bypass prevention (deny-by-default + allowlist)
  # Block backtick command substitution (legacy syntax, use $() instead)
  _bt=$(printf '\x60')
  if printf '%s' "$cmd" | grep -qF "$_bt"; then
    deny "C1: Blocked command containing backtick command substitution"
  fi

  # Block ANSI-C quoting ($'...' can encode arbitrary bytes to bypass pattern checks)
  _ansi_re="(^|[[:space:];|&({\"'=])[$]'"
  if echo "$cmd" | grep -qE "$_ansi_re"; then
    deny "C1: Blocked command containing ANSI-C quoting"
  fi

  # H-1: Block process substitution <(cmd) and >(cmd)
  if echo "$cmd" | grep -qE '[<>]\('; then
    deny "H1-PS: Blocked process substitution (<(cmd) or >(cmd))"
  fi

  # Block unsafe $(...) command substitution (allowlist approach)
  if echo "$cmd" | grep -qF '$('; then
    cmd_check="$(echo "$cmd" | sed -E '
      s/\$\(\([^)]*\)\)/__ASAFE__/g
      s/\$\((go (env|list|version)|pwd|dirname|basename|realpath|which|type|command -v|uname|date|hostname|mktemp|nproc|getconf|id|whoami)[^)]*\)/__SAFE__/g
      s/\$\(git (rev-parse|log|diff|status|branch|describe|remote|tag|ls-files|ls-tree|cat-file|config|symbolic-ref|name-rev|for-each-ref|merge-base)[^)]*\)/__SAFE__/g
      s/\$\((wc|sort|tr|cut)[^)]*\)/__SAFE__/g
    ')"
    if echo "$cmd_check" | grep -qF '$('; then
      deny "C1: Blocked command substitution with non-allowlisted command"
    fi
  fi

  # D001: OS/home/root destruction (case-insensitive for macOS)
  # Match both rm -rf and rm -fr (and variants like -fR, -Rf, -rRf, etc.)
  if echo "$cmd" | grep -qiE 'rm\s+-[a-zA-Z]*[rR][a-zA-Z]*f[a-zA-Z]*\s+(/\s|/$|~|/Users|/home|/root|/opt)' || \
     echo "$cmd" | grep -qiE 'rm\s+-[a-zA-Z]*f[a-zA-Z]*[rR][a-zA-Z]*\s+(/\s|/$|~|/Users|/home|/root|/opt)'; then
    deny "D001: Blocked rm -rf targeting system/home directory"
  fi

  # D001: Long-form flags (--recursive --force / --force --recursive)
  if echo "$cmd" | grep -qiE 'rm\s+.*--(recursive|force)\s+.*--(recursive|force).*\s+(/\s|/$|~|/Users|/home|/root|/opt)'; then
    deny "D001: Blocked rm --recursive --force targeting system/home directory"
  fi

  # C2: D001 with separated flags (rm -r -f /, rm -v -r -f /, etc.)
  # Catches cases where -r and -f are in separate arguments
  if echo "$cmd" | grep -qE 'rm\s' && \
     echo "$cmd" | grep -qE '(-[a-zA-Z]*[rR]|--recursive)' && \
     echo "$cmd" | grep -qE '(-[a-zA-Z]*f|--force)' && \
     echo "$cmd" | grep -qE '(/\s|/$|~|/Users|/home|/root|/opt)'; then
    deny "D001: Blocked rm with recursive+force targeting system/home directory"
  fi

  # D002: Recursive delete outside project root
  if echo "$cmd" | grep -qE 'rm\s+(-[a-zA-Z]*[rR]|--recursive)'; then
    set -f
    for word in $cmd; do
      case "$word" in
        rm|*/rm|-*) continue ;;
      esac
      # H3: Resolve symlinks for existing paths
      #
      # TOCTOU (Time-of-Check-to-Time-of-Use) Risk Assessment:
      #   Race condition: Between the realpath resolution below and the actual rm
      #   execution by the shell, an attacker could swap a symlink target to point
      #   outside the project root (symlink swap attack).
      #
      #   Why kernel-level mitigation is impractical here:
      #   - O_NOFOLLOW / /proc/self/fd patterns require opening the file descriptor
      #     first and operating on it directly, which is not applicable to a shell-level
      #     hook intercepting an arbitrary rm command before execution.
      #   - Go's os.OpenFile does not expose O_RESOLVE_BENEATH or similar kernel flags
      #     that would allow atomic path-and-open verification.
      #
      #   Accepted risk rationale:
      #   - This hook runs inside Claude Code's sandbox, which already restricts
      #     filesystem access to allowed directories.
      #   - Exploiting the TOCTOU window requires the attacker to have filesystem write
      #     access within the sandbox, which contradicts the threat model (the hook
      #     protects against accidental AI-generated destructive commands, not against
      #     an attacker with local filesystem control).
      #   - The race window is extremely narrow (microseconds between realpath and rm).
      #
      #   Future improvement: If Go adds O_RESOLVE_BENEATH support or an equivalent
      #   safe path resolution API, consider replacing this realpath-based check with
      #   an atomic resolution mechanism.
      if [ -e "$word" ] || [ -L "$word" ]; then
        resolved="$(realpath -P "$word" 2>/dev/null || realpath "$word" 2>/dev/null || echo "$word")"
      else
        # For non-existent paths, only check those that look like filesystem paths
        case "$word" in
          /*|~/*|~|../*|*/../*) ;;
          *) continue ;;
        esac
        case "$word" in
          /*) resolved="$word" ;;
          ~/*) resolved="$HOME/${word#\~/}" ;;
          ~) resolved="$HOME" ;;
          *) resolved="$(pwd)/$word" ;;
        esac
      fi
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
  #
  # F-027: maestro.md Tier3 lists --force-with-lease as the safe alternative
  # to --force, but that guidance is for Orchestrator / Planner contexts.
  # Workers route all publish-time pushes through the Daemon (publish_completed
  # / merge_publish), so any direct git push from a Worker — including
  # --force-with-lease — indicates a content-injection attempt and must be
  # rejected.
  if echo "$cmd" | grep -qE 'git\s+push(\s|$)'; then
    deny "Worker git push is prohibited (all git push operations are blocked for Workers, including --force-with-lease — Tier3 alternative does not apply to this role; see maestro.md F-027 / worker.md)"
  fi

  # WT-GIT: Block git change commands in worktree mode
  # When CWD is inside .maestro/worktrees/, only read-only git commands are allowed.
  #
  # Rationale: this hook blocks raw git mutations initiated from inside a
  # worktree (commit orchestration is owned by the daemon). A naive regex
  # scanning the entire command string matched benign invocations such as
  #   maestro result write --summary "...git commit succeeded..."
  # where the verb "git commit" appears only as a literal substring of a
  # CLI argument. To prevent that false positive, we only fire when the
  # word "git" appears as the first token of a shell command segment,
  # anchored by start-of-string or a shell separator ( ; | && ). This
  # mirrors the (^|;|\||&&)\s* pattern used by D005/D006/etc. and still
  # catches chained forms like "cd worktree && git commit" because the
  # && separator places git at the head of the next segment.
  #
  # Integration worktrees are not exempt: Workers may edit conflict files, but
  # staging and merge commits remain daemon-owned.
  _wt_cwd="$(pwd -P 2>/dev/null || echo "")"
  if [ -n "$_wt_cwd" ] && echo "$_wt_cwd" | grep -qF '/.maestro/worktrees/'; then
    if echo "$cmd" | grep -qE '(^|;|\||&&)\s*git\s+(commit|add|merge|rebase|cherry-pick|revert|stash|restore|fetch|pull|worktree|tag)(\s|$)'; then
      deny "WT-GIT: Blocked git change command in worktree mode (daemon owns staging, commits, merges, and publish recovery)"
    fi
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
  # D004: "git checkout ." (no "--" separator, single dot argument) resets all
  # tracked files in the working tree to HEAD, destroying uncommitted changes.
  # The anchor (\s|$) ensures we only match a bare "." token, so benign forms
  # such as "git checkout .github" or "git checkout ./path" are not affected.
  if echo "$cmd" | grep -qE 'git\s+checkout\s+\.(\s|$)'; then
    deny "D004: Blocked git checkout . (destroys uncommitted changes)"
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
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*(env(\s+[^;|&[:space:]]+)*\s+(-u\s+)?(MAESTRO_AGENT_ROLE|TMUX_PANE)|unset\s+(MAESTRO_AGENT_ROLE|TMUX_PANE)|((MAESTRO_AGENT_ROLE|TMUX_PANE)=))' && \
     echo "$cmd" | grep -qE '(^|;|\||&&).*maestro\s+'; then
    deny "D009: Blocked maestro invocation with caller-role environment manipulation"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+plan\s+unquarantine(\s|$)'; then
    deny "D009: Blocked maestro plan unquarantine (operator-only recovery API)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+plan\s+resume-merge(\s|$)'; then
    deny "D009: Blocked maestro plan resume-merge (operator-only recovery API)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+plan\s+retry-publish(\s|$)'; then
    deny "D009: Blocked maestro plan retry-publish (operator-only recovery API)"
  fi
  # Match both the canonical "maestro plan resolve-conflict" form (current
  # CLI router) and the legacy "maestro resolve-conflict" spelling -- the
  # latter is unreachable via the router today but is kept as defense-in-depth
  # in case future content reintroduces it.
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+(plan\s+)?resolve-conflict(\s|$)'; then
    deny "D009: Blocked maestro plan resolve-conflict (operator-only recovery API)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+plan\s+(submit|complete|add-retry-task|add-task|request-cancel|rebuild)(\s|$)'; then
    deny "D010: Blocked maestro plan control-plane API (Planner/operator-owned)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+queue\s+write(\s|$)'; then
    deny "D011: Blocked maestro queue write (Orchestrator/operator control plane)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+verify\s+write(\s|$)'; then
    deny "D012: Blocked maestro verify write (Planner-owned verification control plane)"
  fi

  # B004: Absolute path shell invocation (bypasses restricted mode)
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*(/usr)?/bin/(ba)?sh\b'; then
    deny "B004: Blocked absolute path shell invocation"
  fi

  # M2: Decode command bypass prevention (base64/xxd can reconstruct dangerous commands)
  if echo "$cmd" | grep -qE 'base64\s+(-d|--decode)'; then
    deny "M2: Blocked base64 decode (potential command reconstruction bypass)"
  fi
  if echo "$cmd" | grep -qE 'xxd\s+-[a-zA-Z]*r'; then
    deny "M2: Blocked xxd reverse (potential command reconstruction bypass)"
  fi

  # M3: Shell variable manipulation bypass prevention
  if echo "$cmd" | grep -qE '(^|[;|&[:space:]])IFS='; then
    deny "M3: Blocked IFS variable manipulation (potential bypass vector)"
  fi
  if echo "$cmd" | grep -qE '\$\{![a-zA-Z_]'; then
    deny "M3: Blocked indirect variable reference (potential bypass vector)"
  fi
  if echo "$cmd" | grep -qE 'declare\s+-[a-zA-Z]*n'; then
    deny "M3: Blocked declare -n nameref (potential bypass vector)"
  fi

  # H1: Absolute path invocation of dangerous commands
  if echo "$cmd" | grep -qE '(^|[;|&(])\s*(/usr(/local)?)?/s?bin/(rm|kill|killall|pkill|mkfs|fdisk|dd|diskutil|chmod|chown)\b'; then
    deny "H1: Blocked absolute path invocation of dangerous command"
  fi

  # H1: Wrapper commands executing dangerous programs (env, command, exec, xargs)
  if echo "$cmd" | grep -qE '(^|[;|&])\s*(env|command|exec)\s+(-[^ ]+\s+)*([A-Za-z_][A-Za-z0-9_]*=[^ ]*\s+)*(rm|kill|killall|pkill|sudo|su|mkfs|dd|fdisk|diskutil)\b'; then
    deny "H1: Blocked dangerous command via wrapper (env/command/exec)"
  fi
  if echo "$cmd" | grep -qE '\|\s*xargs\s+(-[^ ]+\s+)*(rm|kill|killall|pkill|sudo|mkfs|dd|fdisk|diskutil)\b'; then
    deny "H1: Blocked dangerous command via xargs"
  fi

  # M-AGT1: find with destructive actions
  if echo "$cmd" | grep -qE 'find\s.*\s-delete(\s|$)'; then
    deny "M-AGT1: Blocked find with -delete flag"
  fi
  if echo "$cmd" | grep -qE 'find\s.*-exec\s+(rm|shred|srm)\s'; then
    deny "M-AGT1: Blocked find with -exec calling destructive command"
  fi

  # M-AGT1: Scripting language destructive file operations
  if echo "$cmd" | grep -qE 'perl\s.*-[eE]\s.*\b(unlink|rmdir|rmtree|remove_tree)\b'; then
    deny "M-AGT1: Blocked destructive file operation via perl"
  fi
  # M-PERL1: Perl indirect execution via eval/system/exec (variable indirection bypass)
  if echo "$cmd" | grep -qE 'perl\s.*-[eE]\s.*\b(eval|system|exec)\b'; then
    deny "M-PERL1: Blocked perl indirect command execution (eval/system/exec)"
  fi
  if echo "$cmd" | grep -qE 'python[23]?\s.*-c\s.*\b(os\.remove|os\.unlink|os\.rmdir|shutil\.rmtree)\b'; then
    deny "M-AGT1: Blocked destructive file operation via python"
  fi
  if echo "$cmd" | grep -qE 'node\s.*-e\s.*\b(unlinkSync|rmdirSync|rmSync)\b'; then
    deny "M-AGT1: Blocked destructive file operation via node"
  fi
  if echo "$cmd" | grep -qE 'ruby\s.*-e\s.*\b(File\.delete|FileUtils\.rm|FileUtils\.rm_rf)\b'; then
    deny "M-AGT1: Blocked destructive file operation via ruby"
  fi

  # SEC-2: Script language indirect code execution
  if echo "$cmd" | grep -qE 'python[23]?\s.*-c\s.*\b(exec|eval|compile|getattr|__import__)\b'; then
    deny "SEC2: Blocked python indirect code execution (exec/eval/compile/getattr/__import__)"
  fi
  if echo "$cmd" | grep -qE 'node\s.*-e\s.*\b(child_process|require\s*\(\s*["'"'"']child_process)\b'; then
    deny "SEC2: Blocked node child_process access"
  fi
  if echo "$cmd" | grep -qE 'ruby\s.*-e\s.*\b(system|Kernel\.system|exec)\b'; then
    deny "SEC2: Blocked ruby indirect command execution (system/Kernel.system/exec)"
  fi

  # m3: rsync destructive operation
  if echo "$cmd" | grep -qE 'rsync\s.*--remove-source-files'; then
    deny "m3: Blocked rsync --remove-source-files (destructive file operation)"
  fi

  # m5: Perl indirect file/command execution via open() and qx//
  if echo "$cmd" | grep -qE 'perl\s.*-[eE]\s.*\b(open|qx)\b'; then
    deny "m5: Blocked perl open()/qx// (indirect file/command execution)"
  fi

  # .maestro/ access via Bash (bypass prevention, case-insensitive for macOS)
  if echo "$cmd" | grep -qiE '(cat|head|tail|less|more|vim|nano|sed|awk)\s+.*\.maestro/(state|queue|results|locks|logs|config|dashboard|verify\.yaml)'; then
    deny "Blocked .maestro/ control-plane access via Bash"
  fi
  if echo "$cmd" | grep -qiE '(ls|find|grep|rg)\s+.*\.maestro/(state|queue|results|locks|logs|config|dashboard|verify\.yaml)'; then
    deny "Blocked .maestro/ control-plane access via Bash"
  fi
  # M-AGT2: File manipulation commands accessing .maestro/ control-plane
  if echo "$cmd" | grep -qiE '(cp|mv|rsync|ln|install|tar|zip)\s+.*\.maestro/(state|queue|results|locks|logs|config|dashboard|verify\.yaml)'; then
    deny "Blocked .maestro/ control-plane access via Bash"
  fi
  # M-AGT2: Write operations targeting .maestro/ directory
  if echo "$cmd" | grep -qiE '(cp|mv|rsync|install)\s+.+\s+[^ ]*\.maestro/'; then
    deny "Blocked write to .maestro/ via file copy/move"
  fi
  if echo "$cmd" | grep -qiE 'ln\s+(-[a-zA-Z]*\s+)*[^ ]+\s+[^ ]*\.maestro/'; then
    deny "Blocked symlink creation in .maestro/ via Bash"
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

  # RUN_ON_MAIN: read-only verification mode. The shared run_on_main flag is
  # computed at script start (see top of file). Fail-closed semantics there
  # ensure tmux outages do not silently allow mutations.
  if [ "$run_on_main" = "1" ]; then
    deny "RUN_ON_MAIN: Write/Edit blocked while task runs against main branch (read-only verification mode)"
  fi

  # Block writes to .maestro/ control plane (absolute and relative paths)
  case "$file_path_lower" in
    */.maestro/state/*|*/.maestro/queue/*|*/.maestro/results/*|*/.maestro/locks/*|*/.maestro/logs/*|*/.maestro/hooks/*|*/.maestro/config.yaml|*/.maestro/dashboard.md|*/.maestro/verify.yaml)
      deny "Blocked write to .maestro/ control-plane path"
      ;;
    .maestro/state/*|.maestro/queue/*|.maestro/results/*|.maestro/locks/*|.maestro/logs/*|.maestro/hooks/*|.maestro/config.yaml|.maestro/dashboard.md|.maestro/verify.yaml)
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
  worker_cwd="$(pwd -P 2>/dev/null || echo "")"
  if [ -n "$worker_cwd" ] && echo "$worker_cwd" | grep -qF '/.maestro/worktrees/'; then
    # H4: Normalize file_path (resolve relative paths and symlinks)
    _wt_check="$file_path"
    case "$file_path" in
      /*) ;; # absolute path, use as-is
      *) _wt_check="$worker_cwd/$file_path" ;;
    esac
    # Resolve symlinks for existing paths
    # TOCTOU note: a race exists between this check and the actual write.
    # Best-effort mitigation; full protection requires kernel-level enforcement.
    if [ -e "$_wt_check" ] || [ -L "$_wt_check" ]; then
      _wt_check="$(realpath -P "$_wt_check" 2>/dev/null || realpath "$_wt_check" 2>/dev/null || echo "$_wt_check")"
    else
      # For non-existent paths (new files), walk up to the first existing
      # ancestor directory, resolve its symlinks, and reconstruct the path.
      # On macOS /var -> /private/var, so pwd -P gives /private/var/... but
      # the file_path may use /var/...; without this, prefix comparison fails.
      _remainder="$(basename "$_wt_check")"
      _ancestor="$(dirname "$_wt_check")"
      while [ "$_ancestor" != "/" ] && [ ! -d "$_ancestor" ]; do
        _remainder="$(basename "$_ancestor")/$_remainder"
        _ancestor="$(dirname "$_ancestor")"
      done
      if [ -d "$_ancestor" ]; then
        _resolved_ancestor="$(cd "$_ancestor" && pwd -P 2>/dev/null || echo "$_ancestor")"
        _wt_check="$_resolved_ancestor/$_remainder"
      fi
    fi
    # Reject paths with unresolved traversal (..)
    if echo "$_wt_check" | grep -qF '..'; then
      deny "WT001: Write outside worktree boundary: $file_path contains path traversal"
    fi
    _wt_lower="$(echo "$_wt_check" | tr '[:upper:]' '[:lower:]')"
    _cwd_lower="$(echo "$worker_cwd" | tr '[:upper:]' '[:lower:]')"
    case "$_wt_lower" in
      "$_cwd_lower"/*) ;; # OK: within worktree (case-insensitive)
      *) deny "WT001: Write outside worktree boundary: $file_path is not within working directory $worker_cwd" ;;
    esac
  fi
fi

# Allow: no output, exit 0
shadow_policy_check true ""
exit 0
