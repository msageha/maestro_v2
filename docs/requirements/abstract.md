# Maestro v2 要件定義書 — Abstract

## 目的

本ドキュメント群は Maestro v2 の完全な要件定義である。Maestro は macOS 専用のマルチエージェントオーケストレーションシステムで、最大 8 つの Claude Code エージェントを tmux 上で並列起動し、ユーザーのコマンドを自律的に分解・実行する。

## アーキテクチャ概要

### 3 層階層

```
User → Orchestrator (Opus) → Planner (Opus) → Workers 1-N (Sonnet/Opus)
```

| 層 | 責務 | モデル |
|---|---|---|
| Orchestrator | ユーザー IF。指示を構造化し下流に渡す | Opus |
| Planner | Five Questions フレームワークでタスク分解・Worker 割当・結果統合 | Opus |
| Worker | 割り当てられたタスクを実行し結果を報告 | Sonnet (L1-L3) / Opus (L4-L6) |

### 通信方式

YAML ファイルベースの単方向メッセージキュー + Go fsnotify (kqueue) イベント駆動。

- **下り (queue/)**: コマンド・タスクの配信
- **上り (results/)**: 実行結果の報告
- **状態管理 (state/commands/)**: command 単位の完了条件の正本

### Agent の責務境界

Agent（Claude Code セッション）の責務はビジネスロジックに限定する。メッセージルーティング、ステータス管理、ダッシュボード生成、ID 採番、tmux 操作、整合性修復、継続モードのシステムコミットタスク自動挿入はすべてインフラ側（`maestro` バイナリ: デーモン + CLI サブコマンド）の責務。

## 設計原則

- **モデル割当**: Bloom's Taxonomy L1-L3 → Sonnet、L4-L6 → Opus（詳細は [§1](01-system-overview.md)）
- **タスク分解**: Five Questions（What / Who / Order / Risk / Verify）で構造化（詳細は [§1](01-system-overview.md)）
- **配信保証**: at-least-once + 冪等キー。lease ベース配信 + reconciliation で不整合自動修復
- **タスク依存**: `blocked_by` フィールドで DAG を明示。全依存が `completed` になるまで配信しない

## ドキュメント構成

| ファイル | 内容 | 対象読者 |
|---|---|---|
| [abstract.md](abstract.md) | 本ドキュメント。全体概要 | 全員 |

### 章別ファイル

| 章 | ファイル | 内容 |
|---|---|---|
| §1 | [01-system-overview.md](01-system-overview.md) | 3 層構造、Bloom's Taxonomy、Five Questions、Agent の責務原則、通信方式、配信保証モデル、単一 Go バイナリアーキテクチャ |
| §2 | [02-user-flow.md](02-user-flow.md) | インストールから実行までの手順 |
| §3 | [03-file-structure.md](03-file-structure.md) | リポジトリ構造（Go プロジェクト）と `.maestro/` ディレクトリ構造 |
| §4 | [04-yaml-schema.md](04-yaml-schema.md) | ID 体系、config / queue / results / state の全スキーマ |
| §5 | [05-script-responsibilities.md](05-script-responsibilities.md) | 全 13 サブコマンドの責務・インターフェース・処理フロー（Planner が使用するのは `plan submit` / `plan complete` / `plan add-retry-task` の 3 サブコマンド。他の `plan` サブコマンド（`request-cancel`, `rebuild`）はインフラ / オペレーター / reconciliation 用。キャンセルは Orchestrator が `queue write --type cancel-request` で要求し、デーモンが内部処理する。`add-retry-task` は `--retry-of` で失敗タスクを置換し、推移的にキャンセルされた依存タスクをデーモンが自動復旧する方式。Phase-Contract Model で段階的計画に対応） |
| §6 | [06-execution-flow.md](06-execution-flow.md) | フェーズ A-D の全ステップ（インストール → 起動 → 実行サイクル → クリーンアップ） |
| §7 | [07-error-handling.md](07-error-handling.md) | タスク失敗、デーモン異常、ビジー検知、配信失敗、クラッシュリカバリ、YAML 肥大化 |
| §8 | [08-tmux-sendkeys.md](08-tmux-sendkeys.md) | agent_executor への集約理由 |
| §9 | [09-safety-rules.md](09-safety-rules.md) | Tier 1 絶対禁止 / Tier 2 停止報告 / Tier 3 安全デフォルト / プロンプトインジェクション防御 |
| §10 | [10-continuous-mode.md](10-continuous-mode.md) | イテレーションループ（Execute→Commit→Evaluate→Decide）、デーモン主導のシステムコミットタスク自動挿入、イテレーションカウンタ、暴走防止 |
| §11 | [11-future-extensions.md](11-future-extensions.md) | マルチプロバイダー対応、YAML 移行基準、動的スケーリング、Web UI |

## システム構成図

```
┌─────────────────────────────────────────────────────────────────────────┐
│  tmux session: maestro-{project}                                        │
│                                                                         │
│  ┌──────────────┐  queue/   ┌─────────────┐  queue/   ┌───────────────┐ │
│  │ Orchestrator │ ───────→  │   Planner   │ ───────→  │  Workers 1-N  │ │
│  │   (Opus)     │           │   (Opus)    │           │ (Sonnet/Opus) │ │
│  │              │ ←───────  │             │ ←───────  │               │ │
│  └──────────────┘ results/  └─────────────┘ results/  └───────────────┘ │
│                                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│  Infrastructure: maestro (single Go binary)                             │
│                                                                         │
│  maestro daemon (常駐)                                                  │
│    queue_handler → agent_executor → tmux send-keys                      │
│    result_handler, plan_state, reconciler                               │
│    dashboard_gen, dead_letter, metrics                                   │
│  CLI subcommands: setup, up, down, status, queue write, result write,   │
│    plan, worker standby, agent launch/exec, notify, dashboard           │
│  Bash: install.sh (bootstrap only)                                      │
└─────────────────────────────────────────────────────────────────────────┘
```

## 依存関係

tmux, claude CLI, `maestro`（単一 Go バイナリ）の 3 つのみ。詳細は [§1](01-system-overview.md) 参照。
