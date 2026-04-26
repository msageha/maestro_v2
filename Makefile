.PHONY: all build test lint format clean install help ensure-lint check-deps \
        docker-build docker-run docker-shell

BINARY    := maestro
CMD_DIR   := ./cmd/maestro
BUILD_DIR := .

GOLANGCI_LINT_VERSION := v2.10.1
GOBIN := $(shell go env GOBIN 2>/dev/null || echo "$(shell go env GOPATH)/bin")
GOLANGCI_LINT := $(GOBIN)/golangci-lint

all: lint test build

## ─── Deps ─────────────────────────────────────────────

check-deps: ## tmux/go/claude/jq の依存チェック
	@missing=0; \
	if ! command -v tmux >/dev/null 2>&1; then \
		echo "error: tmux is not installed"; \
		echo "  macOS:         brew install tmux"; \
		echo "  Ubuntu/Debian: sudo apt install tmux"; \
		missing=1; \
	fi; \
	if ! command -v go >/dev/null 2>&1; then \
		echo "error: go is not installed"; \
		echo "  macOS:         brew install go"; \
		echo "  Ubuntu/Debian: sudo apt install golang"; \
		echo "  Or visit: https://go.dev/dl/"; \
		missing=1; \
	fi; \
	if ! command -v claude >/dev/null 2>&1; then \
		echo "error: claude (Claude Code CLI) is not installed"; \
		echo "  Install via: npm install -g @anthropic-ai/claude-code"; \
		missing=1; \
	fi; \
	if ! command -v jq >/dev/null 2>&1; then \
		echo "error: jq is not installed"; \
		echo "  macOS:         brew install jq"; \
		echo "  Ubuntu/Debian: sudo apt install jq"; \
		missing=1; \
	fi; \
	if [ "$$missing" -ne 0 ]; then \
		echo ""; \
		echo "error: Missing dependencies. Please install them and retry."; \
		exit 1; \
	fi; \
	echo "All dependencies found."

## ─── Build ────────────────────────────────────────────

build: ## Go バイナリをビルド
	go build -o $(BUILD_DIR)/$(BINARY) $(CMD_DIR)/

install: check-deps build ## ビルドしてインストール（MAESTRO_INSTALL_DIR > ~/bin > /usr/local/bin）
	$(eval INSTALL_DIR := $(or $(MAESTRO_INSTALL_DIR),$(if $(wildcard $(HOME)/bin),$(HOME)/bin,/usr/local/bin)))
	@mkdir -p $(INSTALL_DIR)
	mv $(BUILD_DIR)/$(BINARY) $(INSTALL_DIR)/$(BINARY)
	@chmod +x $(INSTALL_DIR)/$(BINARY)
	@echo "Installed $(BINARY) to $(INSTALL_DIR)/$(BINARY)"
	@echo "$(PATH)" | tr ':' '\n' | grep -qx "$(INSTALL_DIR)" || \
		echo "warning: $(INSTALL_DIR) is not in your PATH. Add: export PATH=\"$(INSTALL_DIR):\$$PATH\""

clean: ## ビルド成果物を削除
	rm -f $(BUILD_DIR)/$(BINARY)
	rm -f coverage.out coverage.html

## ─── Test ─────────────────────────────────────────────

test: ## 全テストを実行
	go test ./...

test-v: ## 全テストを verbose で実行
	go test -v ./...

test-race: ## Race detector 付きでテスト
	go test -race ./...

test-cover: ## カバレッジレポートを生成
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "coverage.html generated"

## ─── Lint & Format ────────────────────────────────────

ensure-lint:
	@test -x $(GOLANGCI_LINT) || { echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."; go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); }

lint: ensure-lint ## golangci-lint を実行
	$(GOLANGCI_LINT) run ./...

lint-fix: ensure-lint ## golangci-lint の自動修正を適用
	$(GOLANGCI_LINT) run --fix ./...

format: ensure-lint ## gofmt + goimports でフォーマット
	$(GOLANGCI_LINT) fmt ./...

## ─── Help ─────────────────────────────────────────────

help: ## このヘルプを表示
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## ─── Docker ───────────────────────────────────────────────────────────────────

DOCKER_IMAGE := maestro-env
DOCKER_TAG   := latest
DOCKER_FILE  := docker/Dockerfile

# マウントするリポジトリ (デフォルト: カレントディレクトリ)
# 例: make docker-run DIR=/Users/mzk/Works/src/github.com/dope-corp/2048
DIR ?= $(CURDIR)

# ホストの git ユーザー情報を取得
GIT_USER_NAME  := $(shell git config --global user.name 2>/dev/null || true)
GIT_USER_EMAIL := $(shell git config --global user.email 2>/dev/null || true)

docker-build: ## Docker 開発環境イメージをビルド
	docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) -f $(DOCKER_FILE) .

docker-run: ## 指定ディレクトリで maestro を起動 (make docker-run DIR=/path/to/repo)
	@mkdir -p "$(HOME)/.claude" "$(HOME)/.codex" "$(HOME)/.gemini"
	docker run --rm -it \
		-v "$(DIR):$(DIR)" \
		-v "$(HOME)/.claude:/root/.claude" \
		-v "$(HOME)/.codex:/root/.codex" \
		-v "$(HOME)/.gemini:/root/.gemini" \
		-e "GIT_USER_NAME=$(GIT_USER_NAME)" \
		-e "GIT_USER_EMAIL=$(GIT_USER_EMAIL)" \
		-e "ANTHROPIC_API_KEY=$(ANTHROPIC_API_KEY)" \
		-e "OPENAI_API_KEY=$(OPENAI_API_KEY)" \
		-e "GOOGLE_API_KEY=$(GOOGLE_API_KEY)" \
		-w "$(DIR)" \
		$(DOCKER_IMAGE):$(DOCKER_TAG)

docker-shell: ## 指定ディレクトリで bash を起動 (make docker-shell DIR=/path/to/repo)
	@mkdir -p "$(HOME)/.claude" "$(HOME)/.codex" "$(HOME)/.gemini"
	docker run --rm -it \
		-v "$(DIR):$(DIR)" \
		-v "$(HOME)/.claude:/root/.claude" \
		-v "$(HOME)/.codex:/root/.codex" \
		-v "$(HOME)/.gemini:/root/.gemini" \
		-e "GIT_USER_NAME=$(GIT_USER_NAME)" \
		-e "GIT_USER_EMAIL=$(GIT_USER_EMAIL)" \
		-e "ANTHROPIC_API_KEY=$(ANTHROPIC_API_KEY)" \
		-e "OPENAI_API_KEY=$(OPENAI_API_KEY)" \
		-e "GOOGLE_API_KEY=$(GOOGLE_API_KEY)" \
		-w "$(DIR)" \
		$(DOCKER_IMAGE):$(DOCKER_TAG) bash
