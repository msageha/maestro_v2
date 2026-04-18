#!/usr/bin/env bash
# Maestro Docker entrypoint
# - ホストの git ユーザー情報を環境変数から設定
# - mise を初期化して全ツールシムを有効化
# - 指定コマンドを実行

set -euo pipefail

# ─── mise 初期化 ──────────────────────────────────────────────────────────────
# PATH は Dockerfile で設定済みだが、シェル関数 (activate) も有効化しておく
if command -v mise &>/dev/null; then
    eval "$(mise activate bash 2>/dev/null)" || true
fi

# ─── JAVA_HOME ────────────────────────────────────────────────────────────────
if command -v mise &>/dev/null; then
    JAVA_HOME_PATH="$(mise where java 2>/dev/null)" || true
    if [ -n "${JAVA_HOME_PATH:-}" ]; then
        export JAVA_HOME="$JAVA_HOME_PATH"
    fi
fi

# ─── git ユーザー設定 (ホストから環境変数で受け取る) ─────────────────────────
if [ -n "${GIT_USER_NAME:-}" ]; then
    git config --global user.name  "$GIT_USER_NAME"
fi
if [ -n "${GIT_USER_EMAIL:-}" ]; then
    git config --global user.email "$GIT_USER_EMAIL"
fi

# ─── 実行 ──────────────────────────────────────────────────────────────────
exec "$@"
