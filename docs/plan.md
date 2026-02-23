# Maestro v2 実装計画書

## 現状

- 要件定義（`docs/requirements/` 全 12 ファイル）: 完了
- エージェント指令書テンプレート（`templates/`）: 完了
- Go 実装: **未着手**（全ディレクトリ構造は作成済み、ソースコードなし）
- `go.mod` / `install.sh`: 未作成

## 実装方針

- 依存の少ない基盤レイヤーから積み上げ、各フェーズ完了時にテスト可能な状態を維持する
- 外部依存は最小限: `fsnotify`, `gopkg.in/yaml.v3` のみ
- 各フェーズで実装するファイル・モジュールを明記し、フェーズ間の依存関係を明確にする
- **各フェーズにユニットテストを含める**: 状態遷移・障害復旧が複雑な設計のため、統合テストまで先送りしない。各フェーズの Done 条件にテストパスを含める
- **書き込みはデーモン経由一本化**: バックプレッシャーの check-then-add は必ず per-agent mutex 保持中にデーモン内で原子的に実行し、競合リスクを排除する
- **Agent 責務を固定化**: Orchestrator=`queue write`、Planner=`plan submit/complete/add-retry-task`、Worker=`result write` に限定。状況分岐は可能な限りデーモン側に集約し、Agent の実行コマンドを増やさない

---

## Phase 1: プロジェクト基盤 + データモデル

**目標**: Go モジュール初期化、全データ型の定義、YAML 読み書き基盤の構築、テスト基盤の確立

**Done 条件**: 全モデル型の YAML marshal/unmarshal テストパス、アトミック書き込みテストパス、quarantine リカバリテストパス

### 1.1 Go モジュール初期化

- `go.mod` 作成（`github.com/msageha/maestro_v2`）
- 依存追加: `github.com/fsnotify/fsnotify`, `gopkg.in/yaml.v3`
- テストフレームワーク: Go 標準 `testing` パッケージ + `testify` はオプション

### 1.2 データモデル定義

**ファイル**: `internal/model/`

- `id.go` — ID 生成（`{type}_{unix_timestamp}_{random_hex8}`）、正規表現バリデーション（`^(cmd|task|phase|ntf|res)_[0-9]{10}_[0-9a-f]{8}$`）
- `id_test.go` — ID 生成の一意性、形式バリデーション、各タイプの生成テスト
- `config.go` — `config.yaml` の構造体（Project, Maestro, Agents, Workers, Continuous, Notify, Watcher, Retry, Queue, Limits, Daemon, Logging）
- `queue.go` — queue エントリ型（Command, Task, Notification）、ステータス定数（`pending`, `in_progress`, `completed`, `failed`, `cancelled`, `dead_letter`）、lease フィールド、terminal 判定メソッド
- `result.go` — result エントリ型（TaskResult, CommandResult）、通知 lease フィールド
- `state.go` — state/commands 型（CommandState, CompletionPolicy, Phase, TaskDependencies, RetryLineage, CancelledReasons）
- `metrics.go` — metrics.yaml 型
- `continuous.go` — continuous.yaml 型
- `status.go` — ステータス遷移の合法性チェック関数（terminal 不変性の保証）
- `status_test.go` — ステータス遷移テスト（合法/違法遷移の網羅）

### 1.3 YAML 基盤

**ファイル**: `internal/yaml/`

- `atomic.go` — アトミック書き込み（`os.CreateTemp` → YAML 構文検証 → `os.Rename`）+ `.bak` 作成
- `atomic_test.go` — 正常書き込み、検証失敗時のロールバック、中間ファイル残留なしの確認
- `schema.go` — `schema_version` / `file_type` 検証、未サポートバージョンの fail fast、マイグレーション判定
- `schema_test.go` — バージョン検証、file_type 不一致検出
- `quarantine.go` — 破損 YAML の隔離（quarantine/）、`.bak` からのリストア、`.bak` 不在時のスケルトン再生成（空配列構造）、エラーログ記録
- `quarantine_test.go` — 破損ファイル隔離、.bak リストア、スケルトン再生成の各パス

### 1.4 ロック基盤

**ファイル**: `internal/lock/`

- `lock.go` — per-agent mutex マップ / per-command mutex マップの管理、daemon.lock ファイルロック（`syscall.Flock` LOCK_EX|LOCK_NB）
- `lock_test.go` — mutex 取得/解放、ファイルロックの二重取得拒否

---

## Phase 2: UDS 通信フレームワーク + CLI エントリポイント

**目標**: CLI ↔ デーモン間の通信基盤と、サブコマンドルーティングの骨格

**Done 条件**: UDS サーバー/クライアントの送受信テストパス、プロトコルバージョン不一致の拒否テストパス

### 2.1 Unix ドメインソケット

**ファイル**: `internal/uds/`

- `server.go` — デーモン側 UDS サーバー（JSON + 4byte BigEndian length-prefix フレーミング）、リクエストハンドラ登録、接続タイムアウト管理、ソケットファイル権限設定（`0600`）
- `client.go` — CLI 側 UDS クライアント（接続・送受信・タイムアウト）、接続失敗時のエラーメッセージ（「デーモン未起動」の案内）
- `protocol.go` — リクエスト/レスポンスの共通型定義、**プロトコルバージョンフィールド**（リクエストヘッダに `protocol_version: 1` を含め、サーバー側で検証。非互換バージョンは即拒否）
- `uds_test.go` — フレーミングの正確性、大きなペイロード、プロトコルバージョン不一致拒否、タイムアウト

### 2.2 CLI エントリポイント

**ファイル**: `cmd/maestro/main.go`

- サブコマンドルーティング（`os.Args` ベース、外部ライブラリ不使用）
- 各サブコマンドへの分岐:
  - `daemon`, `setup`, `up`, `down`, `status`
  - `queue write`, `result write`, `plan`
  - `agent launch`, `agent exec`
  - `worker standby`, `notify`, `dashboard`
- 不明サブコマンドの usage 表示

---

## Phase 3: ワンショットコマンド群（setup / notify / status）

**目標**: デーモン不要のワンショットコマンドを先に実装し、以降のフェーズの足場を作る

**Done 条件**: `maestro setup` で `.maestro/` が正しく生成される、notify が macOS 通知を送信する

### 3.1 setup

**ファイル**: `internal/setup/init.go`

- `.maestro/` ディレクトリ構造の作成
- `templates/` からのファイルコピー（config.yaml, maestro.md, instructions/, dashboard.md）
- `queue/worker{N}.yaml`, `results/worker{N}.yaml` の Worker 数に応じた生成
- `state/metrics.yaml`, `state/continuous.yaml` の初期値作成
- config.yaml の自動埋め（project.name, project_root, created）
- `setup_test.go` — ディレクトリ構造の正確性、Worker 数に応じたファイル生成

### 3.2 notify

**ファイル**: `internal/notify/macos.go`

- `osascript -e 'display notification'` 実行
- サウンド付き通知

### 3.3 status

**ファイル**: `internal/status/status.go`

- デーモン稼働確認（UDS ping）
- tmux ユーザー変数から Agent 状態取得
- queue depth 集計
- `--json` 対応

---

## Phase 4: tmux 管理 + Agent 起動

**目標**: tmux セッション管理と Agent CLI 起動の実装

**Done 条件**: tmux セッション/ウィンドウ/ペインの作成・取得・終了テストパス

### 4.1 tmux セッション管理

**ファイル**: `internal/tmux/session.go`

- tmux セッション作成/検索/終了
- ウィンドウ作成（orchestrator, planner, workers）
- ペイン分割（workers は最大 2 列 × 4 行グリッド）
- ユーザー変数設定/取得（`@agent_id`, `@role`, `@model`, `@status`）
- `capture-pane` によるペイン内容取得
- `session_test.go` — セッション CRUD、ユーザー変数の設定/取得

### 4.2 Agent ランチャー

**ファイル**: `internal/agent/launcher.go`

- tmux ユーザー変数の読み取り
- システムプロンプト構築（maestro.md + instructions/{role}.md の結合）
- `claude --model {model} --append-system-prompt "..." --dangerously-skip-permissions` の実行

### 4.3 Agent エグゼキューター

**ファイル**: `internal/agent/executor.go`

- ビジー判定（busy_patterns + idle_stable_sec のアクティビティプローブ）
  - ペインの `pane_current_command` 確認
  - `capture-pane` 最終 3 行 + `busy_patterns` 正規表現マッチング（ヒント）
  - `idle_stable_sec` 後の再キャプチャ → ハッシュ比較（確定）
  - 不確定時は retryable failure（メッセージロストより defer/retry を選択）
- `/clear` 送信 + `cooldown_after_clear` 秒待機
- `--interrupt`（C-c → cooldown → /clear → cooldown → 安定確認）
- `--with-clear`（/clear → cooldown → 安定確認 → メッセージ送信）
- `--is-busy`（戻り値: 0=busy, 1=idle）
- メッセージ送信（C-c クリーンアップ → tmux send-keys）
- 配信エンベロープ生成:
  - Worker 向け: `[maestro] task_id:{} command_id:{} lease_epoch:{} attempt:{}` + purpose/content/acceptance_criteria/constraints/tools_hint + 結果報告テンプレート
  - Planner 向けコマンド: `[maestro] command_id:{} lease_epoch:{} attempt:{}` + content + submit/complete テンプレート
  - Orchestrator 向け通知: `[maestro] kind:command_completed command_id:{} status:{}`
- Orchestrator ペイン保護（厳格ビジー判定、リトライなし即エラー）
- `@status` 更新（busy/idle のライフサイクル管理）
- `.maestro/logs/agent_executor.log` への human-readable ログ出力（配信先、lease_epoch、attempt、結果、失敗理由）

---

## Phase 5: デーモンコア + Queue ハンドラ

**目標**: デーモンプロセスの骨格と、queue/ の監視 → Agent 配信

**Done 条件**: デーモン起動/停止テストパス、pending エントリの配信テストパス、at-most-one-in-flight の保証テストパス

### 5.1 デーモンプロセス

**ファイル**: `internal/daemon/daemon.go`

- UDS サーバー起動（リクエストハンドラ登録）
- fsnotify watcher 初期化（queue/ + results/）
- 定期スキャン ticker（`scan_interval_sec` 間隔）
- daemon.lock ファイルロック（単一インスタンス保証）
- Graceful shutdown（`sync.Once` で冪等実行）:
  1. draining 状態遷移（shutdown context キャンセル）
  2. 全プロデューサー停止（UDS listener close、fsnotify close、ticker 停止）
  3. in-flight ドレイン（`shutdown_timeout_sec` デッドライン付き）
  4. タイムアウト時: 未完了操作をログ記録、in_progress は lease 付きで保持
  5. クリーンアップ（daemon.sock unlink → daemon.lock 解放 → プロセス終了）
  - 2 回目の SIGTERM/SIGINT で即座終了（緊急停止）
- ロギング（`.maestro/logs/daemon.log`、human-readable 形式: `{timestamp} {level} {message}`）

### 5.2 Queue ハンドラ

Queue ハンドラの責務が広いため、内部を以下のサブモジュールに分離する:

**ファイル**: `internal/daemon/queue_handler.go`（オーケストレーション）

- fsnotify イベントの振り分け（planner.yaml / worker{N}.yaml / orchestrator.yaml）
- debounce 処理（`debounce_sec`）
- 定期スキャンの全ステップ実行

**ファイル**: `internal/daemon/lease_manager.go`（lease 管理）

- lease 取得（`in_progress` + `lease_owner` + `lease_expires_at` + `lease_epoch` 加算）
- lease 延長（ビジー判定で正常実行中と判断した場合の heartbeat）
- lease 解放（`pending` 戻し + lease フィールド null 化）
- lease 期限切れ回収（定期スキャン ステップ 2）
- at-most-one-in-flight 不変条件の保証

**ファイル**: `internal/daemon/dependency_resolver.go`（依存解決）

- `blocked_by` 依存判定（state/commands の task_states を正本参照）
- 依存失敗伝搬（`dependency_failure_policy: cancel_dependents`）
  - `task_dependencies` から推移的依存タスクを計算
  - pending → cancelled + `cancelled_reasons` 記録
  - in_progress → agent_executor --interrupt → cancelled + `cancelled_reasons` 記録
  - Planner に影響タスク一覧を 1 回通知
- フェーズ遷移（定期スキャン ステップ 0.7）
  - active フェーズの完了判定:
    - フェーズ内 required が全て `completed` → フェーズ `completed`
    - required に `failed` があれば `failed`、`cancelled` があれば `cancelled`
  - pending deferred の活性化:
    - 依存フェーズが全て `completed` の場合、`awaiting_fill` 遷移 + `fill_deadline_at = now + timeout_minutes`
  - `awaiting_fill` 遷移時に Planner へ固定フォーマット通知を送信:
    `phase:{name} phase_id:{phase_id} status:awaiting_fill command_id:{id} — plan submit --phase {name} で次フェーズのタスクを投入してください`
  - pending フェーズのカスケードキャンセル:
    - 依存フェーズに `failed` / `cancelled` / `timed_out` があれば `cancelled` に遷移
  - `awaiting_fill` タイムアウト:
    - `fill_deadline_at < now` で `timed_out` に遷移し、下流 pending フェーズを `cancelled` にカスケード
- システムコミットタスク判定（`system_commit_task_id` 一致 + 全ユーザーフェーズ terminal が配信条件）

**ファイル**: `internal/daemon/cancel_handler.go`（キャンセル処理）

- キャンセル済みコマンドチェック（`cancel.requested: true`）
- pending タスク → cancelled 遷移（定期スキャン ステップ 0.5）
- in_progress タスク → agent_executor --interrupt → cancelled（定期スキャン ステップ 0.6）
- 合成的 cancelled 結果エントリの作成（Result ハンドラ通知パイプラインへ合流）

**ファイル**: `internal/daemon/dispatcher.go`（配信ロジック）

- 優先度ソート（`effective_priority ASC` → `created_at ASC` → `id ASC`）
  - `effective_priority = max(0, priority - floor((now - created_at) / priority_aging_sec))`
- agent_executor 経由の配信
  - Worker 配信: `--with-clear` 付き
  - Planner/Orchestrator 配信: `--with-clear` なし
  - 配信メッセージに `lease_epoch` をメタデータ含める
- 配信失敗時ロールバック（per-agent mutex 再取得 → pending 戻し + lease 解放）

**定期スキャンステップ一覧**（queue_handler.go がオーケストレーション）:

| ステップ | 責務 | サブモジュール |
|----------|------|----------------|
| 0 | dead-letter 化（attempts >= max_attempts） | dead_letter（Phase 9 で実装） |
| 0.5 | キャンセル済みコマンドの pending タスク処理 | cancel_handler |
| 0.6 | キャンセル済みコマンドの in_progress タスク中断 | cancel_handler |
| 0.7 | フェーズ遷移チェック（完了/活性化/カスケード/タイムアウト）+ awaiting_fill 通知 | dependency_resolver |
| 1 | 未配信エントリの再試行 | dispatcher |
| 1.5 | in_progress タスクの依存失敗チェック | dependency_resolver |
| 2 | lease 期限切れ回収（ビジー検知併用） | lease_manager |

**テスト**: `queue_handler_test.go`, `lease_manager_test.go`, `dependency_resolver_test.go`, `cancel_handler_test.go`, `dispatcher_test.go`

---

## Phase 6: plan state 管理

**目標**: `state/commands/{command_id}.yaml` の正本管理ロジックを確立する。result write が state 更新に強く依存するため、先に固める

**Done 条件**: DAG 検証テストパス（循環検出含む）、can-complete 検証テストパス、Worker 割当アルゴリズムテストパス、キャンセル伝搬テストパス

### 6.1 plan state コア

**ファイル**: `internal/plan/state.go`

- `state/commands/{command_id}.yaml` の CRUD
- `can-complete` 検証ロジック:
  - `plan_status: sealed` 以上であること
  - `required_task_ids + optional_task_ids` 件数 == `expected_task_count`
  - `required_task_ids` の全 task が terminal
  - フェーズチェック: 全フェーズが terminal（filling は retryable エラー）
  - `derived_status` 自動導出:
    - required に `failed` あり → `failed`
    - required に `cancelled` あり → `cancelled`
    - required 全 `completed` → `completed`
    - timed_out フェーズあり → `failed`

### 6.2 DAG 検証

**ファイル**: `internal/plan/dag.go`

- トポロジカルソートによる DAG 検証
- 循環依存検出時に循環パスをエラーメッセージに含める
- タスクレベル DAG + フェーズレベル DAG の両方
- `blocked_by` が同一フェーズ内のみを参照していることの検証
- `dag_test.go` — 正常 DAG、循環検出、自己参照、フェーズ間参照の拒否

### 6.3 Worker 自動割当

**ファイル**: `internal/plan/worker_assign.go`

- bloom_level → model マッチ（L1-L3 → Sonnet, L4-L6 → Opus、boost: true 時は全 Opus）
- 同一モデル Worker 間で pending タスク数最小を選択
- バックプレッシャーチェック（per-agent mutex 保持中に原子的実行）:
  - 早期判定: 全 Worker が上限到達 → submit 全体ロールバック
  - 割当後判定: `pending + 新規割当 > max_pending_tasks_per_worker` でないこと
- `worker_assign_test.go` — モデルマッチ、pending 均等化、boost モード、上限到達時の拒否

### 6.4 キャンセル伝搬

**ファイル**: `internal/plan/cancel.go`

- `cancel.requested: true` の単調性（取り消し不可）
- キャンセル後の新規タスク追加・配信の禁止
- `cancel_test.go` — 単調性保証、キャンセル後の操作拒否

### 6.5 plan submit

- バリデーション:
  - tasks/phases 排他（両方指定はエラー）
  - 二重 submit 防止（state ファイル存在チェック）
  - キャンセル済みチェック（queue/planner.yaml の status: cancelled）
  - name 一意性、`__` プレフィックス拒否（システム予約名）
  - DAG 検証（Phase 6.2）
  - bloom_level 範囲（1-6）
  - `tools_hint` が文字列配列であること
  - フェーズ付き: concrete `depends_on_phases` は空配列のみ、concrete 最低 1 つ、deferred constraints（`max_tasks > 0`, `timeout_minutes > 0`, `allowed_bloom_levels ⊆ {1..6}`）
- `--dry-run`: バリデーションのみ実行、副作用なし
- フィールドパス付きエラーメッセージ（全エラー列挙、最初の 1 件で打ち切らない）
- `__system_commit` タスク自動挿入（`continuous.enabled: true` 時）:
  - tasks の場合: 末尾に追加、`blocked_by` に全ユーザータスク name を設定
  - phases の場合: フェーズ構造の外に独立タスクとして追加（`system_commit_task_id` に記録）
- Worker 割当（Phase 6.3）
- アトミック書き込み: state 作成（planning → sealed）+ queue 追加。途中失敗は全ロールバック
- `--phase` 対応（deferred フェーズ fill: awaiting_fill → filling → active）:
  - constraints 検証（タスク数、bloom_level 範囲）
  - 成功時 `plan_version` インクリメント
  - 失敗時 awaiting_fill に戻す
- JSON 出力（task_id + worker 対応表、phases 付き出力形式含む）

### 6.6 plan complete

- can-complete 検証（Phase 6.1）→ derived_status 導出
- result write planner の内部実行（tasks は results/worker{N}.yaml から自動集約）
- state/commands の plan_status 更新
- `--status` フラグなし（derived_status 自動使用、不一致は原理的に発生しない）

### 6.7 plan add-retry-task

- バリデーション: sealed、cancel 未要求、retry-of が failed
- フェーズ所属決定（retry-of の task_id からフェーズ逆引き）
- Worker 割当（Phase 6.3）
- `required_task_ids` で旧→新を置換（`expected_task_count` 不変）
- `retry_lineage[new_task_id] = replaced_task_id` 記録
- `task_dependencies` で旧→新に依存付け替え
- `task_states[new_task_id] = pending` 追加（旧タスクは failed のまま保持）
- フェーズ再オープン（failed → active、`reopened_at` 記録。completed/cancelled/timed_out からは禁止）
- **推移的キャンセル自動復旧（cascade recovery）**:
  - `cancelled_reasons` から `blocked_dependency_terminal:{task_id}` に一致するタスクを検出
  - 検出タスクの purpose/content/acceptance_criteria/constraints/bloom_level を引き継ぎ
  - `blocked_by` の解決: `retry_lineage` で最新の有効な子孫 ID に写像
  - 再帰的適用（下流にさらにキャンセル済みがあれば同様に復旧）
  - 復旧後の DAG 全体検証
  - `command_cancel_requested` 理由のタスクは対象外
- ロールバック（全変更を巻き戻し）
- JSON 出力（リトライタスク + cascade_recovered）

### 6.8 plan rebuild / request-cancel

- rebuild: per-command mutex 取得 → results/worker{N}.yaml から task_states/applied_result_ids 再構築 → `last_reconciled_at` 更新。冪等
- request-cancel: デバッグ/手動運用用 CLI（Agent は直接使用しない）

**テスト**: `state_test.go`, `dag_test.go`, `worker_assign_test.go`, `cancel_test.go`, `submit_test.go`, `complete_test.go`, `retry_test.go`

---

## Phase 7: queue write / result write

**目標**: Agent → デーモンへの書き込み経路の実装。Phase 6 の plan state に依存

**Done 条件**: queue write の冪等性テストパス、result write の 2 フェーズロック + フェンシング検証テストパス、依存解決トリガーテストパス

### 7.1 queue write

**ファイル**: `internal/queue/writer.go`

- CLI 引数パース（`--type`, `--content`, `--command-id`, `--source-result-id`, `--notification-type`, `--reason` 等）
- UDS 経由でデーモンに送信
- デーモン側処理（per-agent mutex 保持中に原子的実行）:
  1. `--type cancel-request`: queue エントリ作成せず、キャンセル処理を実行
     - state/commands/ 存在（submit 済み）:
       `cancel.requested=true`, `cancel.requested_at=now`, `cancel.requested_by="orchestrator"`, `cancel.reason={reason}` を設定（冪等: 既に requested=true ならスキップ）
     - state/commands/ 不在（未 submit）:
       queue/planner.yaml の該当エントリを cancelled に遷移し、
       `cancel_reason`, `cancel_requested_at`, `cancel_requested_by` を記録（terminal ガード付き）
  2. `--content` バイト数検証（`> max_entry_content_bytes` でエラー）
  3. ID 生成（`{type}_{unix_timestamp}_{random_hex8}`）
  4. バックプレッシャーチェック（mutex 保持中。check-then-add 原子的実行）:
     - command: `pending >= max_pending_commands` → エラー
     - task: `pending >= max_pending_tasks_per_worker` → エラー
     - ファイルサイズ: `current + estimated > max_yaml_file_bytes` → アーカイブ試行 → エラー
  5. `--type notification`: `source_result_id` 冪等重複チェック（同一 source_result_id 既存ならスキップ）
  6. `--type task`: `blocked_by` 簡易バリデーション（自己参照、ID 形式）
  7. YAML エントリ追加（アトミック書き込み）
  8. stdout に ID 出力

### 7.2 result write

**ファイル**: `internal/result/writer.go`

- CLI 引数パース（`--task-id`, `--command-id`, `--lease-epoch`, `--status`, `--summary`, `--files-changed`, `--partial-changes`, `--no-retry-safe` 等）
- UDS 経由でデーモンに送信

**reporter=worker の場合**（2 フェーズロック）:

- **フェーズ A（per-agent mutex）**: results/ と queue/ の更新
  1. 冪等キー検査（`task_id`）— 同一 task_id の確定済み結果は重複追加しない
  2. フェンシング検証 — queue/ の `lease_epoch` と Worker 報告の `--lease-epoch` 一致確認。かつ `in_progress` + 正当 lease。不一致は stale worker として拒否
  3. task_id が `state/commands/` に登録済みか検証（未知 task_id は永続化前に拒否 + エラーログ + Planner へ異常通知）。
     ただし `retry_lineage` で置換済みの旧タスク ID への遅着結果はログ記録のみ
  4. 結果 ID 生成、results/{reporter}.yaml に追加（`notified: false`）
  5. queue/{reporter}.yaml の該当タスクを terminal 更新 + lease クリア
- **フェーズ B（per-command mutex）**: state/ の更新
  6. `task_states[{task_id}]` を terminal 更新
  7. `applied_result_ids[{task_id}] = {result_id}` 記録
- **フェーズ C（依存解決トリガー）**: ベストエフォート
  8. 完了タスクに依存する他タスクの queue ファイルを touch → fsnotify 発火

**reporter=planner**: `plan complete`（Phase 6.6）経由のみ。直接呼び出し不可

**テスト**: `writer_test.go`（queue）, `writer_test.go`（result）— 冪等性、フェンシング拒否、2 フェーズクラッシュシミュレーション

---

## Phase 8: Result ハンドラ + Reconciliation コア（R0/R0b/R1/R2）

**目標**: results/ の監視 → 通知配信と、at-least-once 配信の安全網となる基本 Reconciliation パターン。Reconciliation は継続モードより先に実装する（lease 取り残し・重複配信の増幅防止）

**Done 条件**: notification lease テストパス、R0/R0b/R1/R2 の修復テストパス

### 8.1 Result ハンドラ

**ファイル**: `internal/daemon/result_handler.go`

- fsnotify で results/ 監視
- **notification lease パターン**:
  1. per-agent mutex 取得
  2. `notified: false` かつ（`notify_lease_owner: null` or `notify_lease_expires_at < now`）を検索
  3. mutex 保持中に notification lease 取得（owner, expires_at, attempts+1）
  4. mutex 解放
  5. 通知処理実行
  6. 成功: mutex 再取得 → `notified: true`, `notified_at` 設定, lease クリア → mutex 解放
  7. 失敗: mutex 再取得 → `notify_last_error` 記録, lease クリア → mutex 解放
- **Worker 結果通知** → Planner（agent_executor サイドチャネル）:
  - 固定フォーマット: `[maestro] kind:task_result command_id:{} task_id:{} worker_id:{} status:{}\nresults/{worker_id}.yaml を確認してください`
- **Planner 結果通知** → Orchestrator:
  - `queue write orchestrator --type notification` で通知キューイング
  - `notify` で macOS 通知
- 定期スキャン再試行（`scan_interval_sec` ごとに results/ 全スキャン）

### 8.2 Reconciliation コア

**ファイル**: `internal/daemon/reconciler.go`

at-least-once + lease ベース配信の安全網。Phase 9 で残りのパターンを追加するが、基本パターンを先に入れる:

| # | 不整合パターン | 修復アクション |
|---|---|---|
| R0 | `plan_status: planning` 持続 | state ファイル削除 + queue エントリ除去（submit ロールバック）。Planner に再 submit 通知 |
| R0b | phase `status: filling` 持続 | `awaiting_fill` に戻し + 部分追加 task_ids/queue 除去。Planner に再 fill 通知 |
| R1 | results/ terminal + queue/ `in_progress` | queue/ を terminal に修正, lease 解放 |
| R2 | results/worker terminal + state/ 非 terminal | per-command mutex → `task_states` + `applied_result_ids` 修復 |

- 各修復後に `last_reconciled_at` 更新
- per-entity バージョンチェック付きで直列化
- `reconciler_test.go` — 各パターンの不整合注入→修復確認

---

## Phase 9: Reconciliation 残り + Dead Letter + Dashboard + 継続モード

**目標**: 残りの Reconciliation パターン（R3-R6）、Dead Letter 処理、ダッシュボード生成、メトリクス収集、継続モード

**Done 条件**: R3-R6 修復テストパス、dead-letter 化テストパス、metrics.yaml 更新確認、継続モード冪等ガードテストパス

### 9.1 Reconciliation 残り（R3-R6）

`internal/daemon/reconciler.go` に追加:

| # | 不整合パターン | 修復アクション |
|---|---|---|
| R3 | results/planner terminal + queue/planner 非 terminal | queue/planner を terminal に修正 |
| R4 | results/planner terminal + state/ 非 terminal | can-complete 再評価 → OK なら plan_status 修復。NG なら results/ エントリを quarantine + Planner に再評価通知 |
| R5 | results/planner terminal + orchestrator 通知なし | `queue write orchestrator` で通知再発行（`source_result_id` 冪等） |
| R6 | `awaiting_fill` + `fill_deadline_at < now` | `timed_out` 遷移。下流 pending フェーズを `cancelled`。Planner に通知 |

### 9.2 Dead Letter

**ファイル**: `internal/daemon/dead_letter.go`

- `status: pending` かつ `attempts >= max_attempts` 検出
- dead_letters/ へアーカイブ（`{filename}.{timestamp}.yaml`）
- queue ファイルからエントリ除去
- **queue 型別後処理**:
  - タスク（worker{N}）: state/commands/ の task_states を `failed` に更新（合成 terminal）。Planner に通知
  - コマンド（planner）: state/commands/ 存在時は plan_status を `failed` 更新。未 submit 時は state 更新スキップ。Orchestrator に `command_failed` 通知
  - 通知（orchestrator）: state 更新不要。メトリクスに記録
- `dead_letter_test.go`

### 9.3 Dashboard + メトリクス

**ファイル**: `internal/daemon/dashboard_gen.go`

- queue/ + results/ の集計 → dashboard.md 生成
- **metrics.yaml 更新**:
  - queue_depth（planner, orchestrator, 各 worker の pending 数）
  - counters: commands_dispatched, tasks_dispatched, tasks_completed, tasks_failed, tasks_cancelled, dead_letters, reconciliation_repairs, notification_retries
  - daemon_heartbeat（最終活動時刻）
- メトリクス更新はベストエフォート（コア処理をブロックしない）
- **アラート条件**（`notify` 連携）:
  - dead-letter 発生
  - daemon heartbeat が `scan_interval_sec * 3` 以上更新なし
  - YAML 破損検出
  - reconciliation 修復実行
- **YAML アーカイブ**（terminal エントリの自動アーカイブ）:
  - コマンドライフサイクルゲート付き条件（§7.7 準拠）
  - 50 件超過ファイルをアーカイブ、直近 10 件は保持
  - state/commands/ は terminal + 7 日経過で移動

### 9.4 継続モード

- `state/continuous.yaml` の管理:
  - `current_iteration`, `max_iterations`, `status`, `paused_reason`, `last_command_id`
- **イテレーションカウンタ管理**:
  - Result ハンドラが results/planner.yaml の通知処理完了時にインクリメント
  - `last_command_id` 冪等ガード（同一 command_id の二重インクリメント防止）
  - `current_iteration >= max_iterations` → `status: stopped`, `paused_reason: max_iterations_reached`
- `pause_on_failure: true` → タスク失敗時に自動停止
- 停止時は macOS 通知で停止理由を通知
- `continuous_test.go` — カウンタインクリメント、冪等ガード、max_iterations 到達時停止

---

## Phase 10: up / down + install.sh

**目標**: フォーメーション起動/停止の統括と、ブートストラップスクリプト

**Done 条件**: up → daemon 起動 → down → daemon 停止の一連フローが動作する

### 10.1 maestro up

- config.yaml 読み込み + フラグ反映（`--reset`, `--boost`, `--continuous`, `--no-notify`）
- `--reset`:
  - 既存 tmux セッション・デーモンプロセスの停止
  - queue/, results/, state/commands/ の YAML クリア
  - state/continuous.yaml リセット（current_iteration: 0）
  - state/metrics.yaml カウンターリセット
  - dead_letters/ クリア
  - quarantine/ は保持（デバッグ・フォレンジクス用）
  - `--reset` のみ: リセット後に終了
  - `--reset` + 他フラグ: リセット後にフォーメーション起動へ進む
- **スタートアップリカバリ**:
  1. daemon ロックで競合防止
  2. 必要なディレクトリ・YAML の存在確認（欠損は再作成）
  3. 全 YAML 構文検証（破損 → quarantine → .bak リストア → スケルトン再生成）
  4. schema_version チェック（旧版 → マイグレーション）
  5. ワンショット reconciliation（R0, R0b, R1-R6 の全 8 パターン）
- tmux セッション作成（Phase 4 の tmux モジュール使用）:
  - Window 0: orchestrator（1 ペイン）
  - Window 1: planner（1 ペイン）
  - Window 2: workers（グリッドレイアウト）
  - 各ペインに `@agent_id`, `@role`, `@model`, `@status` 設定
- 各ペインで `maestro agent launch` 実行
- `maestro daemon` バックグラウンド起動
- 起動完了メッセージ表示

### 10.2 maestro down

- UDS 経由 graceful shutdown リクエスト
- `shutdown accepted` 応答後、完了待機（100 秒: daemon 90 秒 + マージン）
- デーモン終了後、tmux セッション終了
- 終了コード: 0（正常 or 既に停止済み）、1（タイムアウト or 異常）

### 10.3 install.sh

- 依存チェック: `tmux`, `go`, `claude`
- 不足時: インストール手順を案内して終了
- `go build -o maestro ./cmd/maestro/`
- バイナリを `~/bin/` or `/usr/local/bin/` に配置（実行権限付与）

---

## Phase 11: worker standby + dashboard CLI

**目標**: デバッグ・可観測性用 CLI の実装

**Done 条件**: JSON 出力のスキーマ検証

### 11.1 worker standby

- queue/worker{N}.yaml 全スキャン
- 各 Worker の pending / in_progress 集計
- idle / busy 判定
- `--model` フィルタリング
- JSON 配列出力

### 11.2 dashboard CLI

- UDS 経由でデーモンに再生成要求
- dashboard.md 書き出し

---

## Phase 12: 統合テスト

**目標**: E2E テスト、エッジケース検証。各フェーズのユニットテストを前提とし、ここではモジュール間の連携を検証

**Done 条件**: 全テストシナリオのパス

### 12.1 統合テストシナリオ

- **正常フロー**: setup → up → コマンド投入 → タスク分解 → Worker 実行 → 結果統合 → down
- **キャンセルフロー**: cancel-request → pending/in_progress タスクの cancelled 遷移
- **依存失敗伝搬**: タスク failed → 推移的 cancelled → Planner 通知
- **add-retry-task + cascade recovery**: 失敗タスク置換 → 依存キャンセルの自動復旧
- **フェーズ付き submit**: concrete → deferred（awaiting_fill → fill → active → completed）
- **フェーズ失敗**: 調査フェーズ失敗 → 下流 deferred のカスケードキャンセル
- **継続モード**: Execute→Commit→Evaluate→Decide ループ、max_iterations 到達停止
- **lease 期限切れ回収**: busy 延長 / idle 回収
- **reconciliation**: R0〜R6 各パターンの不整合修復
- **dead-letter 化**: max_attempts 超過 → dead_letters/ アーカイブ
- **バックプレッシャー**: queue full 時の拒否
- **YAML 破損リカバリ**: quarantine → .bak リストア → スケルトン再生成
- **Graceful shutdown**: in-flight ドレイン + 2 回目シグナルで即座終了

### 12.2 config.yaml テンプレート更新

- `templates/config.yaml` をスキーマ定義（§4.1）に完全準拠させる

---

## 依存関係グラフ

```
Phase 1 (基盤 + モデル + テスト基盤)
  ↓
Phase 2 (UDS + CLI)
  ↓
Phase 3 (setup / notify / status)
  ↓
Phase 4 (tmux + Agent 起動/エグゼキューター)
  ↓
Phase 5 (デーモン + Queue ハンドラ)
  ↓
Phase 6 (plan state 管理)
  ↓
Phase 7 (queue write / result write)
  ↓
Phase 8 (Result ハンドラ + Reconciliation コア R0/R0b/R1/R2)
  ↓
Phase 9 (Reconciliation 残り R3-R6 + Dead Letter + Dashboard + 継続モード)
  ↓
Phase 10 (up / down / install.sh)
  ↓
Phase 11 (worker standby / dashboard CLI)
  ↓
Phase 12 (統合テスト)
```

## 実装優先度

| 優先度 | Phase | 理由 |
|--------|-------|------|
| P0 | 1-2 | 他の全フェーズの前提条件 |
| P0 | 3 | setup がないとデーモン起動できない |
| P0 | 4-5 | コアループの骨格 |
| P0 | 6-7 | plan state → Agent 書き込み経路（依存順序を守る） |
| P1 | 8 | Result 通知 + 基本 Reconciliation（安全網） |
| P1 | 9 | 残り Reconciliation + Dead Letter + Dashboard + 継続モード |
| P1 | 10 | 実際の運用に必要 |
| P2 | 11 | デバッグ・可観測性（コアフローには不要） |
| P2 | 12 | モジュール間連携の品質保証 |
