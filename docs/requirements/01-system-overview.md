# 1. システム概要

Maestro は macOS 専用のマルチエージェントオーケストレーションシステムである。
最大 8 つの Claude Code エージェントを tmux 上で並列起動し、ユーザーのコマンドを自律的に分解・実行する。

## 3 層構造

| 層 | 役割 | モデル | 数 |
|---|---|---|---|
| Orchestrator | ユーザー IF、コマンド受信・結果報告 | Claude Opus | 1 |
| Planner | タスク分解・Worker 割当・結果統合 | Claude Opus | 1 |
| Worker | 単純タスク実行 | Claude Sonnet / Opus | 最大 8（デフォルト 4） |

## モデル割当基準（Bloom's Taxonomy）

タスクの認知レベルに応じてモデルを使い分ける。Planner がタスク分解時に各タスクの認知レベルを判定し、適切なモデルの Worker に割り当てる。

| 認知レベル | Bloom's Taxonomy | モデル | タスク例 |
|---|---|---|---|
| L1 | Remember（記憶） | Sonnet | 定型コードの生成、既存パターンの複製 |
| L2 | Understand（理解） | Sonnet | コードリーディング、ドキュメント要約 |
| L3 | Apply（応用） | Sonnet | 既知パターンの適用、テスト作成 |
| L4 | Analyze（分析） | Opus | アーキテクチャ分析、パフォーマンス調査 |
| L5 | Evaluate（評価） | Opus | 設計判断、コードレビュー、トレードオフ評価 |
| L6 | Create（創造） | Opus | 新規アーキテクチャ設計、複雑なアルゴリズム実装 |

`config.yaml` の `boost: true` 時は全 Worker を Opus に昇格（L1-L3 タスクも Opus で実行）。

## タスク分解フレームワーク（Five Questions）

Planner はコマンドをタスクに分解する際、以下の 5 問を順に検討する:

1. **What（何を）**: コマンドの目的を達成するために必要な成果物は何か
2. **Who（誰が）**: 各タスクに適切な認知レベル（Bloom's L1-L6）と、それに対応するモデルは何か
3. **Order（順序）**: タスク間にファイル競合や論理的依存があるか → `blocked_by` で明示。
   事前調査が必要な場合はフェーズ分割（`depends_on_phases`）で段階的実行を定義
4. **Risk（リスク）**: 各タスクの失敗リスクと、失敗時の影響範囲は何か
5. **Verify（検証）**: 各タスクの `acceptance_criteria` は検証可能か

## Agent の責務原則

Agent（Claude Code セッション）の責務は以下に限定する。これ以外の処理（メッセージルーティング、ステータス管理、コマンド完了条件管理、ダッシュボード生成、ID 生成、tmux 操作、整合性修復、継続モードのシステムコミットタスク自動挿入）はインフラ側（`maestro` バイナリ）の責務であり、Agent が行ってはならない。

| Agent | 責務 | 呼び出す CLI コマンド |
|---|---|---|
| Orchestrator | ユーザー入力を構造化し、下流に渡す | `maestro queue write` |
| Planner | コマンドをタスク分解し Worker に割当。完了通知を受けたら結果を統合し上流に渡す | `maestro plan submit`, `maestro plan complete`, `maestro plan add-retry-task --retry-of` |
| Worker | 割り当てられたタスクを実行し、結果を報告する | `maestro result write` |

## 実装アーキテクチャ: 単一 Go バイナリ

状態管理の複雑性（lease + フェンシング、2 フェーズロック、reconciliation、DLQ、キャンセル伝搬、DAG 検証）を考慮し、全機能を単一の Go バイナリ `maestro` に統合する。唯一の例外は `install.sh`（Bash）。

### 設計方針

- **単一バイナリ**: `maestro` コマンドがデーモンモードと CLI サブコマンドの両方を提供
- **単一ライター**: デーモンモード (`maestro daemon`) が全 YAML ファイルへの唯一のライターとなり、クロスプロセスのロック複雑性を排除
- **Unix ドメインソケット**: CLI サブコマンドはソケット経由でデーモンと通信。Agent が `maestro queue write` を呼ぶと、デーモンが実際の書き込みを行う
- **Go 標準ライブラリ + 最小限の外部依存**: sync.Mutex（排他制御・標準ライブラリ）、fsnotify（ファイル監視・サードパーティ）、gopkg.in/yaml.v3（YAML 操作・サードパーティ）で外部依存を最小化

### サブコマンド体系

| サブコマンド | 責務 | モード |
|---|---|---|
| `maestro daemon` | デーモンプロセス起動（watcher + reconciliation + メトリクス） | 常駐 |
| `maestro setup <dir>` | `.maestro/` ディレクトリ構造の初期化 | ワンショット |
| `maestro up [--boost] [--continuous] [--no-notify]` | フォーメーション起動（デーモン + tmux + Agent） | ワンショット |
| `maestro down` | フォーメーション停止（デーモン + tmux セッション終了） | ワンショット |
| `maestro status` | 状態表示（Agent 一覧、queue depth、メトリクス） | ワンショット |
| `maestro queue write <target> [flags]` | queue/ への書き込み（backpressure + ID 採番）+ キャンセル要求（`--type cancel-request`）の内部処理 | CLI→デーモン |
| `maestro result write <reporter> [flags]` | results/ + queue/ + state/ 更新（2 フェーズロック） | CLI→デーモン |
| `maestro plan <subcommand> [flags]` | command 完了条件の状態管理（submit / complete / add-retry-task / request-cancel / rebuild） | CLI→デーモン |
| `maestro worker standby [--model <model>]` | 待機 Worker 検索（デーモン内部利用。`plan submit` が自動で Worker 割当を行うため、Planner が直接呼び出す必要はない） | CLI→デーモン |
| `maestro agent launch` | Agent CLI 起動（tmux ペイン内で実行） | ワンショット |
| `maestro agent exec <agent_id> <message>` | tmux send-keys 配信（ビジー判定付き） | CLI→デーモン |
| `maestro notify <title> <message>` | macOS 通知センターへの通知 | ワンショット |
| `maestro dashboard` | dashboard.md の手動再生成 | CLI→デーモン |

### デーモン内部モジュール

`maestro daemon` は以下のモジュールを内包する:

| モジュール | 責務 |
|---|---|
| queue_handler | queue/ 変更検知 → Agent 配信（at-most-one-in-flight、lease 管理、priority ordering） |
| result_handler | results/ 変更検知 → 完了通知配信（notification lease パターン） |
| reconciler | 定期スキャン + R0, R0b, R1-R5, R6 不整合修復（全 8 パターン） |
| plan_state | command 完了条件管理（DAG 検証、キャンセル伝搬、`plan submit` 時の Worker 自動割当） |
| agent_executor | tmux send-keys の唯一の実行者（ビジー判定 + `/clear` 制御） |
| dashboard_gen | dashboard.md + metrics.yaml 生成 |
| dead_letter | max_attempts 超過エントリの DLQ 処理 |

### 依存関係

| 依存 | 用途 | インストール |
|---|---|---|
| tmux | エージェント実行環境 | `brew install tmux` |
| claude CLI | Claude Code エージェント | Anthropic 公式 |
| `maestro` | 単一 Go バイナリ（デーモン + CLI） | `install.sh` でビルド・配置 |

> `install.sh` のみ Bash（Go ビルド前のブートストラップ）。詳細は [§5.1](05-script-responsibilities.md) 参照。
> fswatch, yq, flock は不要（Go の fsnotify, YAML ライブラリ, sync.Mutex に置換）。

## 通信方式

YAML ファイルベースの単方向メッセージキュー + Go fsnotify (kqueue) イベント駆動。

- **下り方向 (queue/)**: コマンド・タスクの配信（Orchestrator → Planner → Worker）
- **上り方向 (results/)**: 実行結果の報告（Worker → Planner → Orchestrator）

## 配信保証モデル

本システムの配信保証は **at-least-once + 冪等** を採用する。

| 条件 | 保証レベル | 説明 |
|---|---|---|
| 正常動作時 | **at-least-once（DLQ 上限付き）** | watcher が lease + `lease_epoch`（フェンシングトークン）を取得してから配信。成功時は 1 回で完了するが、障害時は再配信がありうる。`max_attempts` 超過時は dead-letter 化し手動介入を要求 |
| 同時実行抑止 | **at-most-one-in-flight / agent** | per-agent ロック + lease により、同一 Agent queue 内に有効な lease を持つ `in_progress` は最大 1 つ。バックログ（`pending` 蓄積）は許可 |
| 配信失敗時（ビジータイムアウト/クラッシュ） | **再試行付き at-least-once** | `pending` 戻し、または lease 期限切れ回収後に定期スキャンで再試行 |
| 結果反映 | **effectively-once / task_id** | `maestro result write` が冪等キー（`task_id` for worker, `command_id` for planner）の重複を検知し、同一成果を二重確定しない。`lease_epoch` が古い結果は拒否（フェンシング） |
| 通知配信 | **at-least-once（notification lease）** | Result ハンドラが notification lease を取得してから side effect を実行。成功後にのみ `notified: true` をマーク。クラッシュ時もロストしない |
| Orchestrator 通知 | **effectively-once / source_result_id** | `maestro queue write` が `source_result_id` で重複チェック。同一 result に対する通知は 1 回のみ |
| クラッシュリカバリ時 | **最終整合性（reconciliation）** | 定期スキャンの reconciliation が queue/ + results/ + state/ の不整合を 8 パターン（R0, R0b, R1-R5, R6）で検知・修復。per-entity のバージョンチェック付きで直列化 |

**配信保証の範囲と限界**:

- **制御プレーン（queue/results/state の状態遷移）**: 上記の保証は制御プレーンに対して防御可能（defensible）
- **データプレーン（タスク実行によるリポジトリ変更）**: 再配信されたタスクがリポジトリに二重変更を加える可能性は排除できない。Worker は `retry_safe` と `partial_changes_possible` フィールドで副作用の冪等性を報告し、`retry_safe: false` の場合は Planner がリトライ可否を判断する（`add-retry-task` 実行 or `plan complete` で終了。[§7.10](07-error-handling.md) 参照）。完全な副作用の冪等性はアプリケーション層の責務であり、インフラ層では保証しない
