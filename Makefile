.PHONY: all build test lint format clean install help ensure-lint

BINARY    := maestro
CMD_DIR   := ./cmd/maestro
BUILD_DIR := .

GOLANGCI_LINT_VERSION := v2.10.1
GOBIN := $(shell go env GOBIN 2>/dev/null || echo "$(shell go env GOPATH)/bin")
GOLANGCI_LINT := $(GOBIN)/golangci-lint

all: lint test build

## ─── Build ────────────────────────────────────────────

build: ## Go バイナリをビルド
	go build -o $(BUILD_DIR)/$(BINARY) $(CMD_DIR)/

install: build ## ビルドして ~/Works/bin にインストール
	mv $(BUILD_DIR)/$(BINARY) $(HOME)/Works/bin/$(BINARY)

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
