#!/usr/bin/env bash
set -euo pipefail

# Maestro agent PreToolUse hook.
#
# Usage: worker-policy.sh [role]
#
# The role argument is one of worker, planner, orchestrator. Missing or
# unknown roles fall back to worker, the most restrictive policy. The file
# name remains worker-policy.sh for compatibility with existing formations,
# but the hook now serves every claude-code managed role.
#
# Scope (2026-04-30 redesign; extended to all roles 2026-07-04): only enforce
# maestro orchestration-model constraints. Generic destructive-command defense
# (rm -rf, sudo, kill, base64 decode, eval, etc.) is the responsibility of the
# user's global Claude Code hooks (~/.claude/settings.json) and is intentionally
# NOT duplicated here. The duplication caused recurring false positives:
# the e2e regression captured a Worker stalling for 9+ minutes after
# the hook denied every escape path (heredoc, /tmp file, stdin) for a
# `maestro result write --summary` whose payload happened to include
# the literal substring `rm -rf /Users/...` while *describing* the
# work. That class of false positive is incompatible with autonomous
# LLM orchestration: the hook must protect the orchestration model,
# not re-implement system-level safety the host already enforces.
#
# Why planner/orchestrator get this hook (2026-07-04): managed org settings
# can force sandbox.enabled=true and disable bypassPermissions, silently
# downgrading every --dangerously-skip-permissions pane to default permission
# mode. In that downgraded mode, hook-less planner/orchestrator panes wedge on
# unsandboxed-retry approval prompts. A PreToolUse hook returning explicit
# `allow` is the only promptless path verified on claude 2.1.187.
#
# Role policy matrix (maestro-internal contracts only):
#
#   - worker (default): all rules below apply. Worker cannot invoke
#     daemon-owned maestro CLI subcommands
#     (`maestro plan submit/complete/...`, `maestro queue write`,
#     `maestro verify write`, operator-only recovery APIs). Crossing
#     these boundaries from a Worker pane corrupts the plan/queue
#     state machine.
#   - Worker cannot mutate the .maestro/ control plane via Bash or
#     Write/Edit; it is owned by the daemon.
#   - In worktree mode the daemon owns staging/commit/merge/push;
#     direct git mutations from the Worker pane silently desync
#     auto-commit and integration.
#   - Protected IDE/runtime config paths (.vscode, .idea, .claude, .codex,
#     .gemini, .git/hooks) cannot be written from Bash by any role. Claude Code
#     confirms before writing these paths; in downgraded permission mode that
#     confirmation wedges the pane, so the hook fast-fails instead.
#   - RUN_ON_MAIN (read-only verification against main) — Write/Edit
#     and Bash mutations are blocked while the @run_on_main pane flag
#     is set. The flag is set/cleared by the daemon per-dispatch.
#   - Write/Edit must land inside the Worker's worktree CWD, except
#     for standard scratch areas (/tmp, $TMPDIR). Without this, the
#     daemon's auto-commit + integration would never see the change.
#   - Package-manager mutation commands (pnpm/npm/yarn/bun install
#     etc.) are rewritten to run outside the OS sandbox via
#     updatedInput — Claude Code's built-in `**/.vscode/**` write-deny
#     EPERMs dependency extraction, and the post-failure unsandboxed
#     retry wedges on an approval prompt when managed settings disable
#     bypassPermissions. See allow_unsandboxed below.
#   - Maestro CLI calls are also rewritten to run outside the OS
#     sandbox for all roles. They connect to the daemon over a Unix
#     domain socket, and sandbox.network.allowUnixSockets is macOS-only;
#     running the CLI unsandboxed is the portable macOS/Linux fix.
#   - Both unsandbox rewrites are restricted to SINGLE commands (Fix C):
#     a compound command (`;`, `&`, `|`, newline anywhere in the argv)
#     stays sandboxed, because the rewrite would drop the sandbox for the
#     non-package-manager / non-maestro half too (denyRead bypass).
#   - Maestro lifecycle commands (`maestro down/up/daemon/shutdown/stop`)
#     are operator-only for every role (#3b): RunDown executes locally
#     (tmux.KillSession + daemon SIGTERM) with no daemon-side role gate,
#     so any agent pane invoking it tears down the running formation.
#   - planner / orchestrator: Bash role-environment manipulation (#1b),
#     git push (#3), maestro lifecycle commands (#3b), and .maestro/
#     control-plane redirects (#4) are denied. Bash protected-path writes
#     (#7) are also denied for all roles. Write/Edit/MultiEdit/NotebookEdit
#     are denied outright: these roles operate via maestro CLI + Read only,
#     and because this hook's explicit `allow` OVERRIDES --allowedTools
#     (verified on claude 2.1.187), the file-mutation tools must be denied
#     here or the blanket allow silently widens the role surface. The
#     worker-only maestro subcommand matrix (#1), worktree git deny (#2),
#     RUN_ON_MAIN (#5), package-manager unsandbox rewrite (#6), and WT001 are
#     intentionally skipped; the maestro CLI unsandbox rewrite still applies.
#
# Anything outside that surface is delegated to the global hook.
#
# Decision model (2026-06 update): for any tool call that does NOT
# trip a maestro deny rule below, this hook returns an explicit `allow`
# decision (see allow()). This is required for unattended agent operation:
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

role="${1:-worker}"
case "$role" in
  worker|planner|orchestrator) ;;
  *) role="worker" ;;
esac

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

# allow emits an explicit PreToolUse "allow" decision so the agent proceeds
# without an approval prompt. Reached only after every maestro deny rule has
# passed. See the decision-model note above for why this is necessary.
allow() {
  jq -nc --arg role "$role" '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":("maestro " + $role + " policy: no control-plane violation or role-scoped boundary violation")}}'
  exit 0
}

# allow_unsandboxed emits allow + updatedInput that sets
# dangerouslyDisableSandbox:true on the Bash call, so the command runs
# outside Claude Code's OS sandbox without any approval prompt.
#
# Why (2026-07-03 root cause): Claude Code's built-in Bash sandbox
# write-denies `**/.vscode/**`, `**/.idea/**`, `**/.claude/commands/**`,
# `**/.claude/agents/**` and `**/.git/hooks/**` at every depth under the
# CWD — including inside node_modules. Package managers extracting a
# dependency tarball that ships a `.vscode/` directory therefore fail
# with EPERM mid-install (observed with `pnpm install` in production).
# The model's natural recovery — retrying with dangerouslyDisableSandbox
# — is worse: when managed settings set
# `permissions.disableBypassPermissionsMode: "disable"`, the Worker's
# `--dangerously-skip-permissions` silently downgrades to default mode
# and the unsandboxed retry surfaces an approval prompt that no hook
# `allow` can short-circuit once the pane is wedged on it.
#
# Rewriting the input BEFORE execution avoids both failure modes: an
# E2E on claude 2.1.187 confirmed hook allow + updatedInput runs the
# command unsandboxed with no prompt, while the same command sandboxed
# fails EPERM and its unsandboxed retry wedges on approval. Scope is
# deliberately narrow — package-manager mutation commands only. They
# are worktree-scoped, node_modules is gitignored, and the runtime
# still honours `sandbox.allowUnsandboxedCommands:false` should an
# operator forbid escapes entirely.
# NOTE: the reason string must not contain the substring "deny" — several
# tests (and any future naive grep over hook output) classify decisions by
# that substring, and this is an *allow* decision.
allow_unsandboxed() {
  local reason="${1:-maestro policy: Bash command runs unsandboxed}"
  echo "$input" | jq -c --arg reason "$reason" '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":$reason,"updatedInput":(.tool_input + {"dangerouslyDisableSandbox":true})}}'
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
  maestro_cmd_prefix='(^|;|\||&&|&)[[:space:]]*([A-Za-z_][A-Za-z0-9_]*=[^;|&[:space:]]+[[:space:]]+)*(env[[:space:]]+(((-u[[:space:]]+[A-Za-z_][A-Za-z0-9_]*)|(-[A-Za-z0-9]+)|([A-Za-z_][A-Za-z0-9_]*=[^;|&[:space:]]+))[[:space:]]+)*)?(/[A-Za-z0-9_./-]+/)?maestro'

  # 1. Worker must not invoke daemon-owned maestro CLI control-plane
  #    subcommands. These are the explicit "operator/Planner/Orchestrator
  #    role" surfaces; a Worker calling them corrupts the plan/queue state
  #    machine even if the call would otherwise succeed.
  if [ "$role" = "worker" ]; then
    if echo "$cmd" | grep -qE "${maestro_cmd_prefix}[[:space:]]+plan[[:space:]]+(submit|complete|add-task|add-retry-task|request-cancel|rebuild|unquarantine|resume-merge|retry-publish)([[:space:]]|$)"; then
      deny "maestro plan control-plane API is Planner/operator-owned, not Worker-callable"
    fi
    if echo "$cmd" | grep -qE "${maestro_cmd_prefix}[[:space:]]+(plan[[:space:]]+)?resolve-conflict([[:space:]]|$)"; then
      deny "maestro resolve-conflict is operator-only (Worker resolves conflicts via the dispatched task, not the CLI)"
    fi
    if echo "$cmd" | grep -qE "${maestro_cmd_prefix}[[:space:]]+queue[[:space:]]+write([[:space:]]|$)"; then
      deny "maestro queue write is Orchestrator/operator-owned"
    fi
    if echo "$cmd" | grep -qE "${maestro_cmd_prefix}[[:space:]]+verify[[:space:]]+write([[:space:]]|$)"; then
      deny "maestro verify write is Planner-owned"
    fi
    if echo "$cmd" | grep -qE "${maestro_cmd_prefix}[[:space:]]+agent[[:space:]]+(exec|launch)([[:space:]]|$)"; then
      deny "maestro agent exec/launch is operator-only (direct pane messaging bypasses the daemon dispatch path: no lease, fencing, or dedupe)"
    fi
  fi

  # 1b. Block environment tampering that aims to bypass the daemon's
  #     role check before invoking maestro. The daemon distinguishes
  #     CLI / Worker / Planner / Orchestrator via MAESTRO_AGENT_ROLE
  #     and TMUX_PANE; clearing or rewriting them in front of a maestro
  #     invocation lets a Worker forge a CLI-role call. This is a
  #     maestro-specific role-impersonation guard, not a generic
  #     environment-restriction rule. This guard must stay broad because
  #     the daemon trusts the env-declared caller role; unlike rules #1/#4,
  #     command-position anchoring is deliberately NOT used here. A role-env
  #     rewrite plus any mention of `maestro` is suspicious, including
  #     `sh -c 'maestro ...'` subshell strings.
  #     The assignment form (`VAR=`) matches ANYWHERE in the command — the
  #     previous command-position anchor missed prefixed spellings such as
  #     `export MAESTRO_AGENT_ROLE=cli; maestro ...` (Fix A). Over-matching
  #     on payloads that merely mention `MAESTRO_AGENT_ROLE=...` as data is
  #     deliberate: a forged role corrupts the control plane, so false
  #     positives are preferred over false negatives here. `env -u VAR`,
  #     `unset VAR`, and `export -n VAR` stay command-position anchored
  #     (they are plain words otherwise). Residual gap (regex parsing is
  #     best-effort): value-less attribute tampering like
  #     `declare +x MAESTRO_AGENT_ROLE` is not caught.
  if echo "$cmd" | grep -qE '(MAESTRO_AGENT_ROLE|TMUX_PANE)=|(^|;|\||&&)\s*(env(\s+[^;|&[:space:]]+)*\s+(-u\s+)?(MAESTRO_AGENT_ROLE|TMUX_PANE)|(unset|export)\s+(-[A-Za-z]+\s+)*(MAESTRO_AGENT_ROLE|TMUX_PANE))' && \
     echo "$cmd" | grep -qE 'maestro[[:space:]]+'; then
    deny "maestro invocation with role-environment manipulation blocked (do not unset / rewrite MAESTRO_AGENT_ROLE or TMUX_PANE before calling maestro)"
  fi

  # 2. Worker is in worktree mode → daemon owns staging, commits, merges,
  #    publish. Direct git mutations from the worker pane bypass
  #    auto_commit and integration recovery.
  if [ "$role" = "worker" ]; then
    _wt_cwd="$(pwd -P 2>/dev/null || echo "")"
    if [ -n "$_wt_cwd" ] && echo "$_wt_cwd" | grep -qF '/.maestro/worktrees/'; then
      if echo "$cmd" | grep -qE '(^|;|\||&&)\s*git\s+(commit|add|merge|rebase|cherry-pick|revert|stash|restore|fetch|pull|worktree|tag)(\s|$)'; then
        deny "git mutation blocked in worktree mode (daemon owns staging, commits, merges, and publish recovery)"
      fi
    fi
  fi

  # 3. git push is always Maestro-agent-prohibited regardless of cwd. The daemon
  #    publishes via merge_publish/publish_completed; any direct push from
  #    an agent pane indicates a control-plane bypass attempt.
  if echo "$cmd" | grep -qE '(^|;|\||&&)\s*git\s+push(\s|$)'; then
    if [ "$role" = "worker" ]; then
      deny "git push blocked for Worker (daemon owns publish via merge_publish / publish_completed)"
    fi
    deny "git push blocked for $role (daemon owns publish via merge_publish / publish_completed)"
  fi

  # 3b. Maestro lifecycle commands are operator-only for EVERY role.
  #     `maestro down` (and up/daemon/shutdown/stop) runs RunDown locally
  #     (tmux.KillSession + daemon SIGTERM) with no daemon-side role gate,
  #     so any agent pane invoking it tears down the running formation.
  #     This rule must sit BEFORE the rule #6 unsandbox rewrite: rule #6
  #     allow-rewrites plain maestro CLI calls for all roles, and a
  #     lifecycle call must never reach that branch.
  if echo "$cmd" | grep -qE "${maestro_cmd_prefix}[[:space:]]+(down|up|daemon|shutdown|stop)([[:space:]]|$)"; then
    deny "maestro lifecycle command (down/up/daemon/shutdown/stop) is operator-only; no agent role may tear down or restart the running formation"
  fi

  # 4. .maestro/ control plane is daemon-owned. Block command-position
  #    writes (redirects, file-mutating commands targeting .maestro/).
  #    The check uses (^|[;|&]) anchors so a `--summary "wrote .maestro/"`
  #    payload that mentions the path as data does not trip the rule.
  #    `bin` is included: .maestro/bin/roles/<role>/maestro is the role
  #    wrapper the launcher generates; rewriting it is a role-forgery
  #    foothold (S-F3).
  if echo "$cmd" | grep -qiE '(^|[;|&(])\s*(echo|printf|tee|cat|cp|mv|rsync|install|ln)\s+.*>\s*\.maestro/(state|queue|results|locks|logs|hooks|bin|config\.yaml|dashboard\.md|verify\.yaml)'; then
    deny ".maestro/ control-plane write blocked (daemon-owned)"
  fi
  if echo "$cmd" | grep -qE '(^|[;|&(])\s*[^|]*>\s*\.maestro/(state|queue|results|locks|logs|hooks|bin|config\.yaml|dashboard\.md|verify\.yaml)'; then
    deny ".maestro/ control-plane redirect blocked (daemon-owned)"
  fi
  # tee/cp/mv/... write their file arguments WITHOUT any `>` redirection,
  # so the two redirect-shaped checks above miss
  # `printf x | tee .maestro/config.yaml`, `tee .maestro/state/x < in`,
  # and `cp payload .maestro/queue/y` (S-F1). Match the file-writing verbs
  # at any command position — `|` is in the anchor class, so pipe-fed
  # invocations across a pipeline are caught — with a control-plane path
  # anywhere in the argument list (the path may carry a prefix ending in
  # whitespace, `/` for absolute paths, or `=` for dd's of=). Regex-based
  # shell parsing stays best-effort: writes laundered through interpreters
  # (python/perl one-liners), xargs, or variable-expanded paths are NOT
  # caught here; daemon-side ownership of .maestro/ and the L1
  # --disallowedTools patterns remain the backstop.
  if echo "$cmd" | grep -qiE '(^|[;|&(])[[:space:]]*(tee|cp|mv|rsync|install|ln|dd|truncate)[[:space:]]([^;|&<>]*[[:space:]/=])?\.maestro/(state|queue|results|locks|logs|hooks|bin|config\.yaml|dashboard\.md|verify\.yaml)'; then
    deny ".maestro/ control-plane write blocked (daemon-owned)"
  fi

  # 7. Claude Code hard-protects IDE/runtime config directories and prompts
  #    before Write/Edit/Bash writes to them. In downgraded permission mode
  #    that prompt wedges the pane, and neither PreToolUse allow nor
  #    permissions.allow can short-circuit it. Fast-fail command-position Bash
  #    writes so the daemon can repair/replan. Reads such as
  #    `cat .vscode/settings.json` remain allowed.
  protected_config_path='["'\'']?(\./)?([^[:space:];|&<>]*/)?(\.(vscode|idea|claude|codex|gemini)|\.git/hooks)/'
  protected_redirect='(>{1,2}|>\|)'
  protected_path_reason="protected-path Bash write blocked (.vscode/.idea/.claude/.codex/.gemini/.git-hooks are IDE/runtime config dirs Claude Code confirms before writing; in downgraded permission mode that confirmation wedges the pane. Do not write these paths from a task — drop them from the task scope). This is a fast-fail so the daemon can repair/replan instead of the pane stalling."
  if echo "$cmd" | grep -qE "(^|[;|&(])[[:space:]]*(echo|printf|tee|cat|cp|mv|rsync|install|ln|sed|gsed|perl|dd|truncate)([[:space:]]|$)[^;|&]*${protected_redirect}[[:space:]]*${protected_config_path}"; then
    deny "$protected_path_reason"
  fi
  if echo "$cmd" | grep -qE "(^|[;|&(])[[:space:]]*(tee|cp|mv|rsync|install|ln)[[:space:]]+([^[:space:];|&<>]+[[:space:]]+)*${protected_config_path}"; then
    deny "$protected_path_reason"
  fi
  if echo "$cmd" | grep -qE "(^|[;|&(])[[:space:]]*dd[[:space:]][^;|&]*(^|[[:space:]])of=${protected_config_path}"; then
    deny "$protected_path_reason"
  fi
  # In-place editors write their later path arguments. We distinguish -i /
  # --in-place so read-only forms such as `sed -n p .vscode/x` remain allowed;
  # if a future editor syntax is ambiguous, fast-fail an in-place-shaped
  # protected-path command rather than risk a protected-path approval wedge.
  if echo "$cmd" | grep -qE "(^|[;|&(])[[:space:]]*(sed|gsed)[[:space:]]+([^;|&]*[[:space:]])?(-[A-Za-z]*i[A-Za-z]*|--in-place)(=|[[:space:]]|$)[^;|&]*${protected_config_path}"; then
    deny "$protected_path_reason"
  fi
  if echo "$cmd" | grep -qE "(^|[;|&(])[[:space:]]*perl[[:space:]]+[^;|&]*-[A-Za-z]*i[A-Za-z]*([^[:space:]]*)?[[:space:]][^;|&]*${protected_config_path}"; then
    deny "$protected_path_reason"
  fi
  if echo "$cmd" | grep -qE "(^|[;|&(])[[:space:]]*truncate[[:space:]][^;|&]*${protected_config_path}"; then
    deny "$protected_path_reason"
  fi
  if echo "$cmd" | grep -qE "(^|[;|&(])[[:space:]]*[^|]*${protected_redirect}[[:space:]]*${protected_config_path}"; then
    deny "$protected_path_reason"
  fi

  # 5. RUN_ON_MAIN: read-only verification mode. Block mutating commands
  #    at command-start boundaries only — payload data inside quoted
  #    arguments is not scanned, mirroring the design choice to delegate
  #    generic destructive-command defense to the global hook.
  if [ "$role" = "worker" ] && [ "$run_on_main" = "1" ]; then
    # Common file-mutating verbs at command position. tee/dd/truncate/rsync
    # are included because they write their file arguments without any `>`
    # redirection (`... | tee out.log` would otherwise slip past the
    # redirection check below).
    if echo "$cmd" | grep -qE '(^|[;|&(])\s*(/[A-Za-z0-9_./-]+/)?(cp|mv|rm|mkdir|rmdir|touch|chmod|chown|chgrp|ln|install|tee|dd|truncate|rsync)\b'; then
      deny "RUN_ON_MAIN: file-mutating command blocked (read-only verification mode)"
    fi
    # In-place editors.
    if echo "$cmd" | grep -qE '(^|[;|&(])\s*(sed|gsed|perl)\s+(-[a-zA-Z]*i|--in-place)\b'; then
      deny "RUN_ON_MAIN: in-place editor blocked (read-only verification mode)"
    fi
    # Output redirection (writing to a file). Covers plain `>`/`>>`,
    # noclobber-override `>|`, fd-prefixed forms (`1>`, `2>> err.log`),
    # and bash's `&>`/`&>>` both-streams form, while NOT matching pure fd
    # duplications (`2>&1`, `>&2`), which write no file (S-F2). Residual
    # gap: the legacy csh-style `>&file` spelling is indistinguishable
    # from an fd-dup by regex alone and is not caught; writes performed
    # inside interpreters are likewise out of regex reach.
    if echo "$cmd" | grep -qE '(^|[^0-9&])[0-9]*>{1,2}\|?[[:space:]]*[A-Za-z0-9_./~$-]|(^|[^&>])&>{1,2}[[:space:]]*[A-Za-z0-9_./~$-]'; then
      deny "RUN_ON_MAIN: output redirection blocked (read-only verification mode)"
    fi
    # Mutating git verbs.
    if echo "$cmd" | grep -qE '(^|[;|&(])\s*git\s+(commit|add|merge|rebase|cherry-pick|revert|stash|restore|fetch|pull|push|worktree|tag|reset|checkout|clean|am|apply|format-patch|mv|rm|init)(\s|$)'; then
      deny "RUN_ON_MAIN: git mutation blocked (read-only verification mode)"
    fi
  fi

  # 6. OS-sandbox rewrites. This runs LAST in the Bash branch so every
  #    maestro deny rule above has already passed. Package-manager mutation
  #    rewrites remain worker-only and gated on run_on_main=0 so read-only
  #    verification tasks keep the sandbox as an extra mutation barrier.
  #    Maestro CLI calls are different: `maestro result write` is how a
  #    run_on_main Worker reports its result, and the UDS connect must happen
  #    outside the sandbox on macOS and Linux. A command that already carries
  #    dangerouslyDisableSandbox falls through to the plain allow — no rewrite
  #    needed. Bare `yarn` (optionally with flags only, e.g.
  #    `yarn --frozen-lockfile`) is yarn-classic's install spelling and is
  #    matched separately. Maestro unsandboxing exists so the UDS connect()
  #    works on all OSes; a command with shell expansion is either not a plain
  #    maestro call or is a potential denyRead bypass, so it stays sandboxed.
  #    On this machine allowAllUnixSockets lets even the sandboxed connect
  #    succeed; the rare portability cost is accepted to preserve the
  #    secret-read barrier.
  _already_unsandboxed="$(echo "$input" | jq -r '.tool_input.dangerouslyDisableSandbox // false')"
  if [ "$_already_unsandboxed" != "true" ]; then
    # The rewrite disables the OS sandbox for the ENTIRE Bash argv, so it
    # must only ever apply to a single command: in a compound command such
    # as `cat /etc/hosts; maestro result write ...` the non-maestro half
    # would run unsandboxed too, bypassing the sandbox's denyRead secret
    # barrier (Fix C). Any connector (;, &, |, newline) keeps the whole
    # command sandboxed. Connectors inside quoted payloads also match —
    # regex/glob-level parsing cannot tell data from syntax — and that
    # false positive is the deliberate fail-safe direction: the command
    # still runs, just inside the sandbox.
    _cmd_is_compound="0"
    case "$cmd" in
      *';'*|*'&'*|*'|'*|*$'\n'*) _cmd_is_compound="1" ;;
    esac
    if [ "$_cmd_is_compound" = "0" ] && [ "$role" = "worker" ] && [ "$run_on_main" = "0" ]; then
      if echo "$cmd" | grep -qE '(^|[;|&(])\s*(pnpm|npm|yarn|bun)\s+(install|isolated-install|i|ci|add|update|up|upgrade|dedupe|rebuild|prune|import|link|unlink|remove|rm|un|uninstall)(\s|$)' || \
         echo "$cmd" | grep -qE '(^|[;|&(])\s*yarn(\s+-[^;|&]*)?\s*($|[;|&)])'; then
        allow_unsandboxed "maestro worker policy: package-manager command runs unsandboxed (the OS sandbox blocks **/.vscode/** writes during dependency extraction; installs are worktree-scoped)"
      fi
    fi
    _maestro_has_shell_expansion="0"
    if echo "$cmd" | grep -qE '(\$\(|`|<\(|>\()' || \
       echo "$cmd" | grep -qE '(^|[;|&(])[[:space:]]*eval([[:space:]]|$)' || \
       echo "$cmd" | grep -qE '(^|[;|&(])[[:space:]]*(/[A-Za-z0-9_./-]+/)?(sh|bash)[[:space:]]+(-[A-Za-z]*[[:space:]]+)*-c([[:space:]]|$)'; then
      _maestro_has_shell_expansion="1"
    fi
    if [ "$_cmd_is_compound" = "0" ] && [ "$_maestro_has_shell_expansion" = "0" ] && echo "$cmd" | grep -qE "${maestro_cmd_prefix}([[:space:]]|$)"; then
      allow_unsandboxed "maestro $role policy: maestro CLI command runs unsandboxed so its Unix-domain-socket daemon connection is outside Claude Code's OS sandbox"
    fi
  fi
fi

# --- Write/Edit/MultiEdit/NotebookEdit path checks ---
# MultiEdit shares Edit's file_path input shape; NotebookEdit uses
# notebook_path. All four mutate files and must obey the same
# run_on_main / control-plane / worktree-boundary rules.
if [ "$tool_name" = "Write" ] || [ "$tool_name" = "Edit" ] || [ "$tool_name" = "MultiEdit" ] || [ "$tool_name" = "NotebookEdit" ]; then
  # 0. planner / orchestrator never mutate files directly: their launch
  #    args restrict --allowedTools to the maestro CLI + Read surface, but
  #    the explicit `allow` this hook returns for non-denied tools OVERRIDES
  #    --allowedTools (verified on claude 2.1.187). Without this deny the
  #    blanket allow would silently grant Write/Edit to both roles.
  if [ "$role" = "planner" ] || [ "$role" = "orchestrator" ]; then
    deny "Write/Edit is not permitted for planner/orchestrator (these roles operate via maestro CLI + Read only; the PreToolUse allow must not widen allowedTools)"
  fi

  file_path="$(echo "$input" | jq -r '.tool_input.file_path // .tool_input.notebook_path // ""')"
  file_path_lower="$(echo "$file_path" | tr '[:upper:]' '[:lower:]')"

  # 1. RUN_ON_MAIN: read-only verification mode forbids all Worker Write/Edit.
  if [ "$role" = "worker" ] && [ "$run_on_main" = "1" ]; then
    deny "RUN_ON_MAIN: Write/Edit blocked while task runs against main branch (read-only verification mode)"
  fi

  # 2. .maestro/ control-plane is daemon-owned (absolute and relative paths).
  #    bin/ holds the launcher-generated role wrappers
  #    (.maestro/bin/roles/<role>/maestro); rewriting one is a role-forgery
  #    foothold (S-F3).
  case "$file_path_lower" in
    */.maestro/state/*|*/.maestro/queue/*|*/.maestro/results/*|*/.maestro/locks/*|*/.maestro/logs/*|*/.maestro/hooks/*|*/.maestro/bin/*|*/.maestro/config.yaml|*/.maestro/dashboard.md|*/.maestro/verify.yaml)
      deny "write to .maestro/ control-plane path blocked (daemon-owned)"
      ;;
    .maestro/state/*|.maestro/queue/*|.maestro/results/*|.maestro/locks/*|.maestro/logs/*|.maestro/hooks/*|.maestro/bin/*|.maestro/config.yaml|.maestro/dashboard.md|.maestro/verify.yaml)
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
  if [ "$role" = "worker" ] && [ -n "$worker_cwd" ] && echo "$worker_cwd" | grep -qF '/.maestro/worktrees/'; then
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
