# ─── Maestro Development Environment ─────────────────────────────────────────
# mise で管理された全ツール + Claude Code / Codex / Gemini CLI を含む開発環境。
# 任意のリポジトリをホストと同じパスにマウントして maestro を実行する用途。
#
# ビルド: docker build -t maestro-env -f docker/Dockerfile .
# 実行:   DIR=/path/to/repo mise run docker-run
# ──────────────────────────────────────────────────────────────────────────────

FROM ubuntu:24.04

# ─── Build arguments ──────────────────────────────────────────────────────────
ARG SWIFT_VERSION=6.1

# ─── Base environment ─────────────────────────────────────────────────────────
ENV DEBIAN_FRONTEND=noninteractive \
    TZ=UTC \
    HOME=/root \
    GOPATH=/root/go \
    MISE_DATA_DIR=/root/.local/share/mise \
    MISE_CONFIG_DIR=/root/.config/mise \
    MISE_CACHE_DIR=/root/.cache/mise

# 全ツールの実行パスをまとめて設定
ENV PATH="/root/.local/bin:/root/.local/share/mise/shims:/root/.cargo/bin:/root/.bun/bin:/opt/flutter/bin:/usr/share/swift/usr/bin:/usr/lib/dart/bin:/root/go/bin:/root/.pub-cache/bin:${PATH}"

# ─── 1. システムパッケージ ────────────────────────────────────────────────────
RUN apt-get update && apt-get install -y --no-install-recommends \
    # ビルドツール
    build-essential gcc g++ make cmake clang ninja-build pkg-config \
    # 基本ユーティリティ
    curl wget git git-lfs ca-certificates gnupg \
    unzip zip tar xz-utils bzip2 file procps less tree \
    # ターミナル
    tmux \
    # シェル
    zsh fish \
    # ネットワーク
    openssh-client \
    # テキスト処理
    jq yq shellcheck \
    # 開発ライブラリ (言語ビルド用)
    libssl-dev libffi-dev libsqlite3-dev libyaml-dev \
    libreadline-dev zlib1g-dev libncurses5-dev libncursesw5-dev \
    # Python ビルド用
    python3-dev \
    # Flutter Linux デスクトップ用
    libgtk-3-dev libblkid-dev liblzma-dev \
    # Swift runtime (Ubuntu 24.04)
    libpython3-dev libxml2 libedit-dev tzdata \
    # データベースクライアント
    mysql-client \
    # イメージ・グラフィクス
    imagemagick graphviz \
    # HTTP ツール
    httpie \
    # PHP (laravel-boost MCP 用)
    php-cli \
    # その他
    vim htop rlwrap p7zip-full \
    && rm -rf /var/lib/apt/lists/*

# ─── 2. GitHub CLI ────────────────────────────────────────────────────────────
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        | tee /etc/apt/sources.list.d/github-cli.list > /dev/null \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# ─── 3. Google Cloud CLI ──────────────────────────────────────────────────────
RUN curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
        | gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" \
        | tee /etc/apt/sources.list.d/google-cloud-sdk.list > /dev/null \
    && apt-get update && apt-get install -y --no-install-recommends google-cloud-cli \
    && rm -rf /var/lib/apt/lists/*

# ─── 4. mise ─────────────────────────────────────────────────────────────────
RUN curl https://mise.run | sh \
    && mise settings set experimental true

# ─── 5. mise: 言語ランタイム ──────────────────────────────────────────────────
# chezmoi/.config/mise/config.toml の内容を Linux 向けに展開
RUN mise use --global node@latest go@latest uv@latest python@latest ruby@latest java@temurin \
    && mise reshim

# ─── 6. mise: パッケージマネージャー / AWS CLI ───────────────────────────────
RUN mise use --global awscli@latest && mise reshim
# pnpm は node 経由でインストール (mise の aqua backend は GitHub releases に依存して不安定)
RUN npm install -g pnpm

# ─── 7. mise: CLI ツール群 ────────────────────────────────────────────────────
# 障害を分離するためツールごとに分割
RUN mise use --global direnv@latest fzf@latest ghq@latest && mise reshim
RUN mise use --global kubectl@latest terraform@latest && mise reshim
RUN mise use --global lefthook@latest starship@latest && mise reshim
# peco / stern は GitHub ARM64 releases が不安定なため go install でソースビルド
RUN go install github.com/peco/peco/cmd/peco@latest \
    && go install github.com/stern/stern@latest

# ─── 8. Rust ─────────────────────────────────────────────────────────────────
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
        | sh -s -- -y --no-modify-path \
    && cargo install ripgrep fd-find eza bat \
    && rm -rf /root/.cargo/registry /root/.cargo/git

# ─── 9. Bun ──────────────────────────────────────────────────────────────────
RUN curl -fsSL https://bun.sh/install | bash

# ─── 10. Dart SDK (standalone) ───────────────────────────────────────────────
RUN curl -fsSL https://dl-ssl.google.com/linux/linux_signing_key.pub \
        | gpg --dearmor -o /usr/share/keyrings/dart.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/dart.gpg arch=$(dpkg --print-architecture)] https://storage.googleapis.com/download.dartlang.org/linux/debian stable main" \
        | tee /etc/apt/sources.list.d/dart_stable.list > /dev/null \
    && apt-get update && apt-get install -y --no-install-recommends dart \
    && rm -rf /var/lib/apt/lists/*

# ─── 11. Flutter SDK (Linux) ─────────────────────────────────────────────────
# iOS / macOS ビルドは Docker 上では不可 (Xcode 依存)
RUN git clone https://github.com/flutter/flutter.git -b stable --depth=1 /opt/flutter \
    && /opt/flutter/bin/flutter precache --linux --web --no-android --no-ios \
    && /opt/flutter/bin/flutter doctor || true

# ─── 12. Swift (Linux) ───────────────────────────────────────────────────────
# iOS / macOS ビルドは Docker 上では不可 (Xcode 依存); サーバーサイドのみ対応
RUN set -e; \
    ARCH="$(uname -m)"; \
    if [ "$ARCH" = "aarch64" ]; then \
        SWIFT_PKG="swift-${SWIFT_VERSION}-RELEASE-ubuntu24.04-aarch64"; \
        SWIFT_DIR="ubuntu2404-aarch64"; \
    else \
        SWIFT_PKG="swift-${SWIFT_VERSION}-RELEASE-ubuntu24.04"; \
        SWIFT_DIR="ubuntu2404"; \
    fi; \
    wget -q "https://download.swift.org/swift-${SWIFT_VERSION}-release/${SWIFT_DIR}/swift-${SWIFT_VERSION}-RELEASE/${SWIFT_PKG}.tar.gz" \
    && tar -xzf "${SWIFT_PKG}.tar.gz" \
    && mv "${SWIFT_PKG}" /usr/share/swift \
    && rm "${SWIFT_PKG}.tar.gz"

# ─── 13. AI CLI ──────────────────────────────────────────────────────────────
# Claude Code
RUN npm install -g @anthropic-ai/claude-code
# OpenAI Codex (https://github.com/openai/codex)
RUN npm install -g @openai/codex
# Google Gemini CLI (https://github.com/google-gemini/gemini-cli)
RUN npm install -g @google/gemini-cli

# ─── 14. Playwright Chromium (browser 系 MCP 用) ─────────────────────────────
RUN npx playwright install --with-deps chromium 2>/dev/null || true

# ─── 15. maestro バイナリのビルド & インストール ──────────────────────────────
COPY . /tmp/maestro-src/
RUN cd /tmp/maestro-src \
    && go build -o /usr/local/bin/maestro ./cmd/maestro/ \
    && cd / \
    && rm -rf /tmp/maestro-src \
    && go clean -cache -testcache

# ─── 16. シェル設定 ───────────────────────────────────────────────────────────
RUN echo 'eval "$(mise activate bash)"'   >> /root/.bashrc \
    && echo 'eval "$(direnv hook bash)"'  >> /root/.bashrc \
    && echo 'eval "$(starship init bash)"' >> /root/.bashrc \
    && echo 'export JAVA_HOME="$(mise where java 2>/dev/null || true)"' >> /root/.bashrc

# ─── Entrypoint ───────────────────────────────────────────────────────────────
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["bash"]
