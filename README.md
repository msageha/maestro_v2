# Maestro v2

Claude Code の複数インスタンスを tmux 上で協調動作させるマルチエージェントオーケストレーションシステム。

Orchestrator / Planner / Worker の 3 層構造で、ユーザーの指示を自動的にタスク分解・並列実行・結果集約する。

## 前提条件

- **Go** 1.26+
- **tmux**
- **Claude Code CLI** (`claude`)

```bash
# macOS の場合
brew install tmux go
npm install -g @anthropic-ai/claude-code
```

## インストール

```bash
git clone https://github.com/msageha/maestro_v2.git
cd maestro_v2
./install.sh
```

`install.sh` は依存チェック → `go build` → `~/bin/maestro` へ配置を行う。
インストール先は環境変数 `MAESTRO_INSTALL_DIR` で変更可能。

## クイックスタート

```bash
# 1. プロジェクトディレクトリで初期化
maestro setup .

# 2. フォーメーション起動（tmux セッション + デーモン）
maestro up

# 3. Orchestrator の tmux ペインに接続して指示を出す
tmux attach -t maestro

# 4. 終了
maestro down
```

## アーキテクチャ

```
┌─────────────┐
│ Orchestrator │  ユーザーとの対話・全体進捗管理
└──────┬──────┘
       │ queue/orchestrator.yaml（通知）
┌──────┴──────┐
│   Planner   │  タスク分解・完了判定
└──────┬──────┘
       │ queue/worker{1..N}.yaml（タスク）
┌──────┴──────────────────────┐
│ Worker1  Worker2  ...  WorkerN │  タスク実行
└─────────────────────────────┘
```

- **単一ライターアーキテクチャ**: 全 YAML 状態はデーモンプロセスが排他的に管理
- **IPC**: Unix Domain Socket (UDS) で CLI ↔ デーモン間を通信
- **配信保証**: at-least-once + 冪等性
- **障害復旧**: 8 パターンの自動リコンシリエーション (R0〜R6)

## コマンド一覧

### フォーメーション管理

```bash
maestro setup <dir>                    # .maestro/ ディレクトリを初期化
maestro up [--reset] [--boost] [--continuous] [--no-notify]
                                       # フォーメーション起動
maestro down                           # グレースフルシャットダウン
maestro status [--json]                # 現在の状態を表示
```

| フラグ | 説明 |
|--------|------|
| `--reset` | 起動前にキュー・結果・状態をクリア |
| `--boost` | 全 Worker のモデルを Opus に昇格 |
| `--continuous` | 継続モードを有効化 |
| `--no-notify` | macOS 通知を無効化 |

### キュー操作

```bash
# コマンド投入（Orchestrator → Planner）
maestro queue write planner --type command --content "認証機能を実装して"

# タスク投入（Planner → Worker）
maestro queue write worker1 --type task \
  --command-id cmd_xxx \
  --purpose "ログイン API" \
  --content "JWT ベースのログインエンドポイントを実装" \
  --acceptance-criteria "POST /api/login が 200 を返す" \
  --bloom-level 4

# キャンセル要求
maestro queue write planner --type cancel-request --command-id cmd_xxx
```

### プラン操作（Planner が使用）

```bash
# タスク分解結果を提出
maestro plan submit --command-id cmd_xxx --tasks-file plan.yaml

# フェーズ充填（deferred フェーズへのタスク追加）
maestro plan submit --command-id cmd_xxx --phase "phase2" --tasks-file tasks.yaml

# コマンド完了報告
maestro plan complete --command-id cmd_xxx --summary "全タスク完了"

# 失敗タスクのリトライ
maestro plan add-retry-task --command-id cmd_xxx \
  --retry-of task_yyy \
  --purpose "修正版" \
  --content "エラーを修正して再実装" \
  --acceptance-criteria "テスト通過" \
  --bloom-level 4

# コマンドキャンセル要求
maestro plan request-cancel --command-id cmd_xxx --reason "不要になった"

# 状態ファイル再構築（障害復旧用）
maestro plan rebuild --command-id cmd_xxx
```

### 結果報告（Worker が使用）

```bash
maestro result write worker1 \
  --task-id task_xxx \
  --command-id cmd_xxx \
  --lease-epoch 3 \
  --status completed \
  --summary "実装完了" \
  --files-changed src/auth.go \
  --files-changed src/auth_test.go
```

### ユーティリティ

```bash
maestro worker standby              # 空き Worker を表示
maestro dashboard                    # dashboard.md を再生成
maestro notify "タイトル" "メッセージ"  # macOS デスクトップ通知
maestro version                      # バージョン表示
```

## ディレクトリ構造

`maestro setup` で生成される `.maestro/` の構造:

```
.maestro/
├── config.yaml              # 設定ファイル
├── maestro.md               # コマンドログ
├── dashboard.md             # ステータスダッシュボード
├── instructions/            # エージェントごとのシステムプロンプト
│   ├── orchestrator.md
│   ├── planner.md
│   └── worker.md
├── queue/                   # キューファイル (YAML)
│   ├── planner.yaml         # コマンドキュー
│   ├── orchestrator.yaml    # 通知キュー
│   └── worker{1..N}.yaml    # タスクキュー
├── results/                 # 結果ファイル (YAML)
│   ├── planner.yaml         # コマンド結果
│   └── worker{1..N}.yaml    # タスク結果
├── state/
│   ├── commands/            # コマンドごとの状態 (正本)
│   ├── metrics.yaml         # メトリクス
│   └── continuous.yaml      # 継続モード状態
├── locks/                   # デーモンロック
├── logs/                    # デーモンログ
├── dead_letters/            # 配信失敗エントリ
└── quarantine/              # 破損 YAML の退避先
```

## 設定

`.maestro/config.yaml` の主要設定項目:

```yaml
agents:
  workers:
    count: 4                  # Worker 数 (1-8)
    default_model: "sonnet"   # デフォルトモデル
    boost: false              # true で全 Worker を Opus に

continuous:
  enabled: false              # 継続モード
  max_iterations: 10          # 最大イテレーション数

watcher:
  dispatch_lease_sec: 120     # リース期間 (秒)
  max_in_progress_min: 30     # タスク実行の最大時間 (分)
  scan_interval_sec: 60       # 定期スキャン間隔 (秒)

limits:
  max_pending_commands: 20    # 最大 pending コマンド数
  max_pending_tasks_per_worker: 10  # Worker あたり最大 pending タスク数

logging:
  level: "info"               # debug | info | warn | error
```

## Bloom's Taxonomy によるモデル選択

タスクの認知的複雑さに応じて自動的にモデルを割り当てる:

| レベル | 分類 | モデル |
|--------|------|--------|
| L1 記憶 | 定型的な変更・設定 | Sonnet |
| L2 理解 | 既存コードの読解・修正 | Sonnet |
| L3 応用 | パターン適用・実装 | Sonnet |
| L4 分析 | 設計判断・リファクタリング | Opus |
| L5 評価 | アーキテクチャ評価・最適化 | Opus |
| L6 創造 | 新規設計・アルゴリズム創出 | Opus |

`--boost` フラグで全 Worker を Opus に昇格可能。

## tmux セッション構成

`maestro up` で作成される tmux セッション:

| ウィンドウ | エージェント | 役割 |
|------------|-------------|------|
| 0 | Orchestrator | ユーザーとの対話、進捗管理 |
| 1 | Planner | タスク分解、完了判定 |
| 2 | Worker1〜N | タスク実行 (2列グリッドレイアウト) |

```bash
# セッションに接続
tmux attach -t maestro

# ウィンドウ切り替え: Ctrl-b + 0/1/2
# ペイン切り替え (Worker 間): Ctrl-b + 矢印キー
```

## 開発

```bash
# ビルド
go build -o maestro ./cmd/maestro/

# テスト
go test ./...

# 特定パッケージのテスト
go test ./internal/daemon/...
go test ./internal/plan/...
```
