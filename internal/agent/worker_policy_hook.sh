#!/usr/bin/env bash
set -euo pipefail

# Maestro Worker PreToolUse hook.
#
# Scope (2026-04-30 redesign): only enforce maestro orchestration-model
# constraints. Generic destructive-command defense (rm -rf, sudo, kill,
# base64 decode, eval, etc.) is the responsibility of the user's global
# Claude Code hooks (~/.claude/settings.json) and is intentionally NOT
# duplicated here. The duplication caused recurring false positives:
# the e2e regression captured a Worker stalling for 9+ minutes after
# the hook denied every escape path (heredoc, /tmp file, stdin) for a
# `maestro result write --summary` whose payload happened to include
# the literal substring `rm -rf /Users/...` while *describing* the
# work. That class of false positive is incompatible with autonomous
# LLM orchestration: the hook must protect the orchestration model,
# not re-implement system-level safety the host already enforces.
#
# What this hook still enforces (maestro-internal contracts only):
#
#   - Worker cannot invoke daemon-owned maestro CLI subcommands
#     (`maestro plan submit/complete/...`, `maestro queue write`,
#     `maestro verify write`, operator-only recovery APIs). Crossing
#     these boundaries from a Worker pane corrupts the plan/queue
#     state machine.
#   - Worker cannot mutate the .maestro/ control plane via Bash or
#     Write/Edit; it is owned by the daemon.
#   - In worktree mode the daemon owns staging/commit/merge/push;
#     direct git mutations from the Worker pane silently desync
#     auto-commit and integration.
#   - RUN_ON_MAIN (read-only verification against main) — Write/Edit
#     and Bash mutations are blocked while the @run_on_main pane flag
#     is set. The flag is set/cleared by the daemon per-dispatch.
#   - Write/Edit must land inside the Worker's worktree CWD, except
#     for standard scratch areas (/tmp, $TMPDIR). Without this, the
#     daemon's auto-commit + integration would never see the change.
#
# Anything outside that surface is delegated to the global hook.
#
# Decision model (2026-06 update): for any Bash/Write/Edit call that does NOT
# trip a maestro deny rule below, this hook returns an explicit `allow`
# decision (see allow()). This is required for unattended Worker operation:
# Claude Code runs several HARDCODED Bash safety classifiers (e.g. the
# "expansion obfuscation" check that fires on any command containing a brace
# followed by a quote, and the `cd && <write>` compound-command guard) which
# execute AHEAD of the permission allow-list and therefore CANNOT be silenced
# by `--dangerously-skip-permissions` or by `permissions.allow` rules. They
# routinely false-positive on legitimate Worker commands (e.g. a
# `maestro result write --summary "...{...}"` whose payload contains a brace
# and a quote), leaving the pane stuck on an approval prompt until the daemon's
# blocked-pane timeout fails the task. A PreToolUse hook returning `allow` is
# the only mechanism (besides full bypassPermissions, which we deliberately do
# NOT use) that short-circuits those hardcoded prompts. maestro's own deny
# rules still run first and short-circuit with `deny`, so the control-plane /
# git / .maestro / worktree-boundary / run_on_main protections are unchanged;
# and because hook `deny` takes precedence over `allow`, the operator's global
# deny hooks remain effective.

# S3: jq dependency check - deny all if jq is unavailable (fail-safe)
if ! command -v jq >/dev/null 2>&1; then
  echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Maestro policy hook requires jq but it is not installed. Denying for safety."}}'
  exit 0
fi

input="$(cat)"
tool_name="$(echo "$input" | jq -r '.tool_name // ""')"

deny() {
  local reason="$1"
  jq -nc --arg r "$reason" '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":$r}}'
  exit 0
}

# allow emits an explicit PreToolUse "allow" decision so the Worker proceeds
# without an approval prompt. Reached only after every maestro deny rule has
# passed. See the decision-model note above for why this is necessary.
allow() {
  jq -nc '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"maestro worker policy: no control-plane / worktree-boundary / run_on_main violation"}}'
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
run_on_main="0"
if [ -n "${TMUX_PANE:-}" ]; then
  if command -v tmux >/dev/null 2>&1; then
    if _flag="$(tmux display-message -t "$TMUX_PANE" -p '#{@run_on_main}' 2>/dev/null)"; then
      if [ "$_flag" = "1" ]; then
        run_on_main="1"
      fi
    else
      # tmux is installed but display-message failed. Fail closed.
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

  # 1. Worker must not invoke daemon-owned maestro CLI control-plane
  #    subcommands. These are the explicit "operator/Planner/Orchestrator
  #    role" surfaces; a Worker calling them corrupts the plan/queue state
  #    machine even if the call would otherwise succeed.
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+plan\s+(submit|complete|add-task|add-retry-task|request-cancel|rebuild|unquarantine|resume-merge|retry-publish)(\s|$)'; then
    deny "maestro plan control-plane API is Planner/operator-owned, not Worker-callable"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+(plan\s+)?resolve-conflict(\s|$)'; then
    deny "maestro resolve-conflict is operator-only (Worker resolves conflicts via the dispatched task, not the CLI)"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+queue\s+write(\s|$)'; then
    deny "maestro queue write is Orchestrator/operator-owned"
  fi
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*maestro\s+verify\s+write(\s|$)'; then
    deny "maestro verify write is Planner-owned"
  fi

  # 1b. Block environment tampering that aims to bypass the daemon's
  #     role check before invoking maestro. The daemon distinguishes
  #     CLI / Worker / Planner / Orchestrator via MAESTRO_AGENT_ROLE
  #     and TMUX_PANE; clearing or rewriting them in front of a maestro
  #     invocation lets a Worker forge a CLI-role call. This is a
  #     maestro-specific role-impersonation guard, not a generic
  #     environment-restriction rule.
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*(env(\s+[^;|&[:space:]]+)*\s+(-u\s+)?(MAESTRO_AGENT_ROLE|TMUX_PANE)|unset\s+(MAESTRO_AGENT_ROLE|TMUX_PANE)|((MAESTRO_AGENT_ROLE|TMUX_PANE)=))' && \
     echo "$cmd" | grep -qE '(^|;|\||&&).*maestro\s+'; then
    deny "maestro invocation with role-environment manipulation blocked (do not unset / rewrite MAESTRO_AGENT_ROLE or TMUX_PANE before calling maestro)"
  fi

  # 2. Worker is in worktree mode → daemon owns staging, commits, merges,
  #    publish. Direct git mutations from the worker pane bypass
  #    auto_commit and integration recovery.
  _wt_cwd="$(pwd -P 2>/dev/null || echo "")"
  if [ -n "$_wt_cwd" ] && echo "$_wt_cwd" | grep -qF '/.maestro/worktrees/'; then
    if echo "$cmd" | grep -qE '(^|;|\||&&)\s*git\s+(commit|add|merge|rebase|cherry-pick|revert|stash|restore|fetch|pull|worktree|tag)(\s|$)'; then
      deny "git mutation blocked in worktree mode (daemon owns staging, commits, merges, and publish recovery)"
    fi
  fi

  # 3. git push is always Worker-prohibited regardless of cwd. The daemon
  #    publishes via merge_publish/publish_completed; any direct push from
  #    a Worker pane indicates a control-plane bypass attempt.
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*git\s+push(\s|$)'; then
    deny "git push blocked for Worker (daemon owns publish via merge_publish / publish_completed)"
  fi

  # 4. .maestro/ control plane is daemon-owned. Block command-position
  #    writes (redirects, file-mutating commands targeting .maestro/).
  #    The check uses (^|[;|&]) anchors so a `--summary "wrote .maestro/"`
  #    payload that mentions the path as data does not trip the rule.
  if echo "$cmd" | grep -qiE '(^|[;|&(])\s*(echo|printf|tee|cat|cp|mv|rsync|install|ln)\s+.*>\s*\.maestro/(state|queue|results|locks|logs|hooks|config\.yaml|dashboard\.md|verify\.yaml)'; then
    deny ".maestro/ control-plane write blocked (daemon-owned)"
  fi
  if echo "$cmd" | grep -qE '(^|[;|&(])\s*[^|]*>\s*\.maestro/(state|queue|results|locks|logs|hooks|config\.yaml|dashboard\.md|verify\.yaml)'; then
    deny ".maestro/ control-plane redirect blocked (daemon-owned)"
  fi

  # 5. RUN_ON_MAIN: read-only verification mode. Block mutating commands
  #    at command-start boundaries only — payload data inside quoted
  #    arguments is not scanned, mirroring the design choice to delegate
  #    generic destructive-command defense to the global hook.
  if [ "$run_on_main" = "1" ]; then
    # Common file-mutating verbs at command position.
    if echo "$cmd" | grep -qE '(^|[;|&(])\s*(/[A-Za-z0-9_./-]+/)?(cp|mv|rm|mkdir|rmdir|touch|chmod|chown|chgrp|ln|install)\b'; then
      deny "RUN_ON_MAIN: file-mutating command blocked (read-only verification mode)"
    fi
    # In-place editors.
    if echo "$cmd" | grep -qE '(^|[;|&(])\s*(sed|gsed|perl)\s+(-[a-zA-Z]*i|--in-place)\b'; then
      deny "RUN_ON_MAIN: in-place editor blocked (read-only verification mode)"
    fi
    # Output redirection (writing to a file).
    if echo "$cmd" | grep -qE '(^|[^0-9&])>{1,2}[[:space:]]*[A-Za-z0-9_./~$-]'; then
      deny "RUN_ON_MAIN: output redirection blocked (read-only verification mode)"
    fi
    # Mutating git verbs.
    if echo "$cmd" | grep -qE '(^|[;|&(])\s*git\s+(commit|add|merge|rebase|cherry-pick|revert|stash|restore|fetch|pull|push|worktree|tag|reset|checkout|clean|am|apply|format-patch|mv|rm|init)(\s|$)'; then
      deny "RUN_ON_MAIN: git mutation blocked (read-only verification mode)"
    fi
  fi
fi

# --- Write/Edit path checks ---
if [ "$tool_name" = "Write" ] || [ "$tool_name" = "Edit" ]; then
  file_path="$(echo "$input" | jq -r '.tool_input.file_path // ""')"
  file_path_lower="$(echo "$file_path" | tr '[:upper:]' '[:lower:]')"

  # 1. RUN_ON_MAIN: read-only verification mode forbids all Write/Edit.
  if [ "$run_on_main" = "1" ]; then
    deny "RUN_ON_MAIN: Write/Edit blocked while task runs against main branch (read-only verification mode)"
  fi

  # 2. .maestro/ control-plane is daemon-owned (absolute and relative paths).
  case "$file_path_lower" in
    */.maestro/state/*|*/.maestro/queue/*|*/.maestro/results/*|*/.maestro/locks/*|*/.maestro/logs/*|*/.maestro/hooks/*|*/.maestro/config.yaml|*/.maestro/dashboard.md|*/.maestro/verify.yaml)
      deny "write to .maestro/ control-plane path blocked (daemon-owned)"
      ;;
    .maestro/state/*|.maestro/queue/*|.maestro/results/*|.maestro/locks/*|.maestro/hooks/*|.maestro/config.yaml|.maestro/dashboard.md|.maestro/verify.yaml)
      deny "write to .maestro/ control-plane path blocked (daemon-owned, relative)"
      ;;
  esac

  # 3. WT001: Worktree boundary enforcement (relaxed in 2026-04-30
  #    redesign). When the Worker's CWD is inside .maestro/worktrees/,
  #    Write/Edit operations should target the worktree so auto_commit
  #    sees the change. Standard scratch areas (/tmp, $TMPDIR, /var/tmp,
  #    /private/tmp) are exempt because workers legitimately use them
  #    for ephemeral payload files (e.g. the --summary-file recipe in
  #    worker.md uses `mktemp` to stage long summaries off the Bash
  #    argv before invoking `maestro result write --summary-file`).
  worker_cwd="$(pwd -P 2>/dev/null || echo "")"
  if [ -n "$worker_cwd" ] && echo "$worker_cwd" | grep -qF '/.maestro/worktrees/'; then
    # Resolve file_path to its eventual absolute realpath.
    _wt_check="$file_path"
    case "$file_path" in
      /*) ;; # absolute path, use as-is
      *) _wt_check="$worker_cwd/$file_path" ;;
    esac
    if [ -e "$_wt_check" ] || [ -L "$_wt_check" ]; then
      _wt_check="$(realpath -P "$_wt_check" 2>/dev/null || realpath "$_wt_check" 2>/dev/null || echo "$_wt_check")"
    else
      # For non-existent paths (new files), walk up to the first
      # existing ancestor directory and resolve its symlinks. On macOS
      # /var -> /private/var, so pwd -P returns /private/var/... while
      # the file_path may still use /var/...; without this, prefix
      # comparison would mis-fire.
      _remainder="$(basename "$_wt_check")"
      _ancestor="$(dirname "$_wt_check")"
      while [ "$_ancestor" != "/" ] && [ ! -d "$_ancestor" ]; do
        _remainder="$(basename "$_ancestor")/$_remainder"
        _ancestor="$(dirname "$_ancestor")"
      done
      if [ -d "$_ancestor" ]; then
        _resolved_ancestor="$(cd "$_ancestor" && pwd -P 2>/dev/null || echo "$_ancestor")"
        # Avoid the double-slash that "$_resolved_ancestor/$_remainder"
        # produces when the walk reached the filesystem root (e.g. on
        # Linux a /private/var/... probe whose ancestors do not exist
        # would otherwise yield //private/var/... and miss the
        # /private/var/folders/* case pattern further down).
        if [ "$_resolved_ancestor" = "/" ]; then
          _wt_check="/$_remainder"
        else
          _wt_check="$_resolved_ancestor/$_remainder"
        fi
      fi
    fi

    # Reject path traversal that survived realpath.
    if echo "$_wt_check" | grep -qF '..'; then
      deny "WT001: Write outside worktree boundary: $file_path contains path traversal"
    fi

    # Standard scratch areas — exempt from worktree boundary.
    #
    # /tmp and /var/tmp are universally treated as scratch on Unix-likes.
    # On macOS `mktemp` defaults to $TMPDIR which lives under
    # /var/folders/... (per-user); /var/folders and its /private/var/folders
    # canonical form are explicitly listed so mktemp-based workflows land
    # without tripping WT001 even when TMPDIR is unset / inherited from a
    # restricted shell. The TMPDIR exemption further down adds belt-and-
    # braces coverage for non-standard tmp roots.
    #
    # Each scratch exemption is suppressed *per-area* when worker_cwd is
    # rooted under that same scratch area — that is the test-fixture
    # configuration, where a synthetic .maestro/worktrees/... lives inside
    # TempDir(). Without this carve-out, every test would silently fall
    # into the scratch exemption and stop exercising the boundary rule.
    # On Linux CI t.TempDir() returns /tmp/...; on macOS it returns
    # /var/folders/.../T/... — the carve-out must therefore cover both.
    _wt_allowed=0
    case "$_wt_check" in
      /tmp/*|/private/tmp/*|/var/tmp/*|/private/var/tmp/*)
        case "$worker_cwd" in
          /tmp/*|/private/tmp/*|/var/tmp/*|/private/var/tmp/*) ;;
          *) _wt_allowed=1 ;;
        esac
        ;;
      /var/folders/*|/private/var/folders/*)
        case "$worker_cwd" in
          /var/folders/*|/private/var/folders/*) ;;
          *) _wt_allowed=1 ;;
        esac
        ;;
    esac
    if [ "$_wt_allowed" = "0" ]; then
      _tmpdir="${TMPDIR:-}"
      if [ -n "$_tmpdir" ]; then
        # Resolve TMPDIR symlinks to handle macOS /var -> /private/var.
        _tmpdir_resolved="$(cd "$_tmpdir" 2>/dev/null && pwd -P 2>/dev/null || echo "$_tmpdir")"
        case "$_wt_check" in
          "$_tmpdir"/*|"$_tmpdir_resolved"/*)
            case "$worker_cwd" in
              "$_tmpdir"/*|"$_tmpdir_resolved"/*) ;;
              *) _wt_allowed=1 ;;
            esac
            ;;
        esac
      fi
    fi
    if [ "$_wt_allowed" = "0" ]; then
      _wt_lower="$(echo "$_wt_check" | tr '[:upper:]' '[:lower:]')"
      _cwd_lower="$(echo "$worker_cwd" | tr '[:upper:]' '[:lower:]')"
      case "$_wt_lower" in
        "$_cwd_lower"/*) ;; # OK: within worktree (case-insensitive)
        *) deny "WT001: Write outside worktree boundary: $file_path is not within working directory $worker_cwd (allowed scratch areas: /tmp, /var/tmp, \$TMPDIR)" ;;
      esac
    fi
  fi
fi

# No maestro deny rule tripped → explicitly allow so Claude Code's hardcoded
# Bash safety classifiers (expansion-obfuscation / compound-cd) do not stall
# the unattended Worker on an approval prompt.
allow
