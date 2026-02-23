# 3. ファイル構成

## リポジトリ構造

```
maestro/
├── install.sh                         # ブートストラップ（依存チェック + go build + バイナリ配置）
├── cmd/
│   └── maestro/
│       └── main.go                    # エントリポイント（サブコマンドルーティング）
├── internal/
│   ├── daemon/
│   │   ├── daemon.go                  # デーモンプロセス（Unix ドメインソケット + メインループ）
│   │   ├── queue_handler.go           # queue/ 変更検知 → Agent 配信
│   │   ├── result_handler.go          # results/ 変更検知 → 完了通知
│   │   ├── reconciler.go              # 定期スキャン + R0, R0b, R1-R5, R6 不整合修復
│   │   ├── dead_letter.go             # max_attempts 超過エントリの DLQ 処理
│   │   └── dashboard_gen.go           # dashboard.md + metrics.yaml 生成
│   ├── agent/
│   │   ├── executor.go                # tmux send-keys 配信（ビジー判定 + /clear 制御）
│   │   └── launcher.go                # Claude/Codex/Gemini CLI 起動
│   ├── queue/
│   │   └── writer.go                  # queue/ 書き込み（backpressure + ID 採番）
│   ├── result/
│   │   └── writer.go                  # results/ + queue/ + state/ 更新（2 フェーズロック）
│   ├── plan/
│   │   └── state.go                   # command 完了条件管理（DAG 検証 + キャンセル伝搬 + plan submit アトミック処理 + Worker 自動割当）
│   ├── tmux/
│   │   └── session.go                 # tmux セッション・ウィンドウ・ペイン管理
│   ├── notify/
│   │   └── macos.go                   # macOS 通知センター連携
│   ├── setup/
│   │   └── init.go                    # .maestro/ ディレクトリ初期化
│   ├── yaml/
│   │   ├── atomic.go                  # アトミック書き込み（tmp + validate + mv）
│   │   ├── schema.go                  # schema_version 検証 + マイグレーション
│   │   └── quarantine.go              # 破損 YAML 隔離・復旧
│   └── lock/
│       └── lock.go                    # sync.Mutex ベースの排他制御
├── templates/
│   ├── config.yaml                    # プロジェクト設定テンプレート
│   ├── maestro.md                     # 全エージェント共通システムプロンプト
│   ├── dashboard.md                   # ダッシュボードテンプレート
│   └── instructions/
│       ├── orchestrator.md            # Orchestrator 指令書
│       ├── planner.md                 # Planner 指令書
│       └── worker.md                  # Worker 指令書
├── go.mod                             # Go モジュール定義
├── go.sum                             # 依存ハッシュ
└── docs/
    └── requirements/
        ├── abstract.md                # 要件定義 概要・目次
        ├── 01-system-overview.md      # §1 システム概要
        ├── 02-user-flow.md            # §2 ユーザーフロー
        ├── 03-file-structure.md       # §3 ファイル構成（本ファイル）
        ├── 04-yaml-schema.md          # §4 YAML スキーマ定義
        ├── 05-script-responsibilities.md  # §5 サブコマンド責務定義
        ├── 06-execution-flow.md       # §6 実行フロー詳細
        ├── 07-error-handling.md       # §7 エラーハンドリング
        ├── 08-tmux-sendkeys.md        # §8 tmux send-keys 一元化
        ├── 09-safety-rules.md         # §9 安全規則
        ├── 10-continuous-mode.md      # §10 継続モード
        └── 11-future-extensions.md    # §11 将来の拡張ポイント
```

## プロジェクト初期化後の .maestro/ 構造

```
.maestro/
├── config.yaml                        # このプロジェクトの設定
├── maestro.md                         # 共通システムプロンプト
├── dashboard.md                       # デーモンが自動生成するダッシュボード
├── instructions/
│   ├── orchestrator.md
│   ├── planner.md
│   └── worker.md
├── queue/
│   ├── planner.yaml                   # Orchestrator → Planner（コマンド）
│   ├── orchestrator.yaml              # Planner 完了通知 → Orchestrator（通知）
│   ├── worker1.yaml                   # Planner → Worker1（タスク）
│   ├── worker2.yaml
│   └── ...                            # worker{N}.yaml（Worker 数分）
├── results/
│   ├── planner.yaml                   # Planner → Orchestrator（コマンド実行結果）
│   ├── worker1.yaml                   # Worker1 → Planner（タスク実行結果）
│   ├── worker2.yaml
│   └── ...                            # worker{N}.yaml（Worker 数分）
├── state/
│   ├── commands/
│   │   ├── cmd_1771722000_a3f2b7c1.yaml   # command 単位の完了条件と task 状態の正本
│   │   └── ...
│   ├── continuous.yaml                # 継続モード状態（イテレーションカウンタ等。デーモンが管理）
│   └── metrics.yaml                   # 可観測性メトリクス（デーモンが更新）
├── locks/                             # ロックファイル
│   └── daemon.lock                    # デーモン単一インスタンス保証（syscall.Flock）
│                                      # per-agent / per-command の排他制御はデーモンプロセス内の
│                                      # sync.Mutex で行うため、ファイルロックは daemon.lock のみ
├── dead_letters/                      # max_attempts 超過エントリの保存先
│   └── {filename}.{timestamp}.yaml
├── quarantine/                        # 破損 YAML の隔離先
│   └── {filename}.{timestamp}.corrupt
└── logs/
    ├── daemon.log                     # デーモンプロセスのログ
    └── agent_executor.log             # Agent 配信のログ
```
