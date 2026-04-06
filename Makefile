.PHONY: all build test lint format clean install help ensure-lint check-deps

BINARY    := maestro
CMD_DIR   := ./cmd/maestro
BUILD_DIR := .

GOLANGCI_LINT_VERSION := v2.10.1
GOBIN := $(shell go env GOBIN 2>/dev/null || echo "$(shell go env GOPATH)/bin")
GOLANGCI_LINT := $(GOBIN)/golangci-lint

all: lint test build

## ─── Deps ─────────────────────────────────────────────

check-deps: ## tmux/go/claude の依存チェック
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
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
