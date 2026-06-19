# 4. YAML スキーマ定義

## 4.0 ID 体系

全 ID はグローバル一意とする。形式: `{type}_{unix_timestamp}_{random_hex8}`

```
コマンド ID:  cmd_1771722000_a3f2b7c1
タスク ID:    task_1771722060_b7c1d4e9
フェーズ ID:  phase_1771722000_c3d4e5f6
通知 ID:      ntf_1771722600_d4e9f0a2
結果 ID:      res_1771722300_e5f0c3d8
```

- `maestro queue write`、`maestro result write`、`maestro plan submit`（フェーズ ID 含む）、`maestro plan add-retry-task` が生成を担当
- Agent は ID を指定しない（writer の stdout で採番された ID を受け取る）
- タイムスタンプベースのため、アーカイブやファイル削除で衝突しない
- `random_hex8`（32bit）により、同一秒内に同種 ID を複数生成しても衝突確率は十分に低い（誕生日問題: 同一秒内 100 件生成でも衝突確率 < 0.0002%）
- **ID 形式の正規表現**: `^(cmd|task|phase|ntf|res)_[0-9]{10}_[0-9a-f]{8}$`。デーモンは生成時・受理時にこの形式を検証する
- **不変条件**: ID 内の `unix_timestamp` と対応エントリの `created_at` は同一 UTC 基準の時刻である。ID 採番はデーモンが一元的に行うため、Agent に時刻整合の責務は発生しない

## 4.1 config.yaml

```yaml
project:
  name: "my-project"
  description: "プロジェクトの説明"

maestro:
  version: "2.0.0"
  created: "2026-02-22T10:00:00+09:00"
  project_root: "/absolute/path/to/project"

agents:
  orchestrator:
    id: "orchestrator"
    model: "opus"
  planner:
    id: "planner"
    model: "opus"
  workers:
    count: 4                     # 1-8（上限 8。maestro setup / up で検証）
    default_model: "sonnet"      # models に未定義の Worker はこれを使用
    models:                      # 個別指定（省略時は default_model）
      worker3: "opus"
      worker4: "opus"
      # worker1, worker2 → default_model (sonnet)
    boost: false                 # true → 全 Worker を opus に昇格

continuous:
  enabled: false                 # --continuous フラグで有効化
  max_iterations: 10             # 最大ループ回数
  pause_on_failure: true         # タスク失敗時に自動停止

notify:
  enabled: true                  # macOS 通知（--no-notify で無効化）

watcher:
  debounce_sec: 0.3              # ファイル監視デバウンス間隔（秒）
  scan_interval_sec: 60          # 定期スキャン間隔（秒）
  dispatch_lease_sec: 120        # 配信中エントリの lease 期限（秒）
  max_in_progress_min: 30        # in_progress のハード上限（分）
  busy_check_interval: 2         # ビジー判定のリトライ間隔（秒）
  busy_check_max_retries: 30     # 最大リトライ回数（busy_check_interval × 回数 = 60 秒。idle_stable_sec を加味すると最大約 210 秒）
  busy_patterns: "Working|Thinking|Planning|Sending|Searching"
  idle_stable_sec: 5             # ペイン内容がこの秒数変化しなければアイドルと判定
  cooldown_after_clear: 3        # /clear 後のクールダウン（秒）
  notify_lease_sec: 120          # 通知 lease 期限（秒）

retry:
  command_dispatch: 5              # queue/planner.yaml: コマンド配信の最大試行回数
  task_dispatch: 5                 # queue/worker{N}.yaml: タスク配信の最大試行回数
  orchestrator_notification_dispatch: 10  # queue/orchestrator.yaml: Orchestrator 通知配信の最大試行回数
  result_notification_send: 10     # results/*.yaml: Result ハンドラによる通知送信の最大試行回数

queue:
  priority_aging_sec: 300          # 優先度エイジング間隔（秒）。待機時間に応じて effective_priority を引き下げ

limits:
  max_pending_commands: 20         # 未処理コマンドの最大数
  max_pending_tasks_per_worker: 10 # Worker あたりの未処理タスク最大数
  max_entry_content_bytes: 65536   # 1 エントリの content フィールドの最大バイト数（64KB）
  max_yaml_file_bytes: 5242880     # YAML ファイルの最大サイズ（5MB）

daemon:
  shutdown_timeout_sec: 90       # graceful shutdown のドレイン待機上限（秒）

logging:
  level: "info"                  # debug | info | warn | error
```

## 4.2 queue/planner.yaml（Orchestrator → Planner: コマンド）

```yaml
schema_version: 1
file_type: "queue_command"
commands:
  - id: "cmd_1771722000_a3f2b7c1"
    content: "認証機能を実装して、ログイン・ログアウト・セッション管理を含めてください"
    priority: 100                        # 優先度（小さいほど高優先。デフォルト 100）
    status: "pending"                    # pending ⇄ in_progress → completed | failed | cancelled; pending → dead_letter [attempts >= max]（配信失敗時 in_progress → pending に戻る）
    attempts: 0                          # 配信試行回数（queue_handler が加算）
    last_error: null                     # 直近の配信失敗理由（文字列 / null）
    dead_lettered_at: null               # dead_letter 化時刻（ISO8601 / null）
    dead_letter_reason: null             # dead_letter 理由（文字列 / null）
    lease_owner: null                    # "daemon:<pid>" / null
    lease_expires_at: null               # ISO8601 / null
    lease_epoch: 0                       # 単調増加フェンシングトークン（配信ごとに +1）
    cancel_reason: null                    # キャンセル理由（文字列 / null。未 submit のコマンドが cancel-request で直接キャンセルされた場合に設定）
    cancel_requested_at: null              # キャンセル要求時刻（ISO8601 / null）
    cancel_requested_by: null              # キャンセル要求者（"orchestrator" / null）
    created_at: "2026-02-22T10:00:00+09:00"
    updated_at: "2026-02-22T10:00:00+09:00"
```

> **未 submit コマンドのキャンセル**: `state/commands/` が未作成の段階で `cancel-request` を受けた場合、デーモンは `queue/planner.yaml` のエントリを直接 `cancelled` に遷移し、上記の `cancel_*` フィールドにメタデータを記録する。submit 済みのコマンドでは `state/commands/{command_id}.yaml` の `cancel` フィールドが正本となり、queue エントリの `cancel_*` フィールドは使用しない。

## 4.3 queue/worker{N}.yaml（Planner → Worker: タスク）

```yaml
schema_version: 1
file_type: "queue_task"
tasks:
  - id: "task_1771722060_b7c1d4e9"
    command_id: "cmd_1771722000_a3f2b7c1"    # 親コマンド ID（トレーサビリティ用）
    purpose: "ユーザー認証の入口となるログイン API を提供する"
    content: "JWT を使ったログイン API エンドポイントを実装"
    acceptance_criteria: "POST /api/login が 200 を返しトークンが発行される"
    constraints:                         # 実行時の制約（省略可、デフォルト空配列）
      - "既存の /api/health エンドポイントに影響を与えないこと"
      - "パスワードを平文で保存しないこと"
    blocked_by: []                       # 先行タスク ID のリスト（空 = 即時実行可能）
    bloom_level: 3                       # Bloom's Taxonomy レベル（1-6）
    tools_hint:                          # Worker に推奨する MCP ツール名のリスト（省略可、デフォルト空配列）
      - "context7"
    priority: 100                        # 優先度（小さいほど高優先。デフォルト 100。デーモンが自動設定）
    status: "pending"                    # pending ⇄ in_progress → completed | failed | cancelled; pending → dead_letter [attempts >= max]（配信失敗時 in_progress → pending に戻る）
    attempts: 0                          # 配信試行回数（queue_handler が加算）
    last_error: null                     # 直近の配信失敗理由（文字列 / null）
    dead_lettered_at: null               # dead_letter 化時刻（ISO8601 / null）
    dead_letter_reason: null             # dead_letter 理由（文字列 / null）
    lease_owner: null                    # "daemon:<pid>" / null
    lease_expires_at: null               # ISO8601 / null
    lease_epoch: 0                       # 単調増加フェンシングトークン（配信ごとに +1）
    created_at: "2026-02-22T10:01:00+09:00"
    updated_at: "2026-02-22T10:01:00+09:00"
```

**タスクフィールド補足**:

| フィールド | 必須 | 説明 |
|---|---|---|
| `purpose` | 必須 | タスクが全体の中で果たす役割。Worker が成果物を親コマンドの意図と照合するために使用 |
| `content` | 必須 | 実行すべき具体的な作業内容 |
| `acceptance_criteria` | 必須 | 完了の検証条件（検証可能な形式で記述） |
| `constraints` | 任意 | 実行時に守るべき制約条件のリスト |
| `blocked_by` | 必須 | 先行タスク ID のリスト。空配列 `[]` なら即時実行可能。依存先が全て `completed` で配信可能。依存先に `failed` / `cancelled` / `dead_letter` が出た場合、当該タスクは自動的に `cancelled` へ遷移する（[§5.8.1](05-script-responsibilities.md) 参照） |
| `bloom_level` | 必須 | Bloom's Taxonomy レベル (1-6)。Planner がモデル割当の根拠として設定。L1-L3 → Sonnet, L4-L6 → Opus |
| `tools_hint` | 任意 | Worker に推奨する MCP ツール名の文字列配列。省略時は空配列。Planner が `plan submit` で設定し、デーモンがそのまま queue に転記する。Worker は推奨に従いツールの活用を検討するが、強制ではない |
| `priority` | 任意 | 優先度（小さいほど高優先。デフォルト 100）。デーモンが自動設定。Agent は指定しない |

## 4.4 results/worker{N}.yaml（Worker → Planner: タスク実行結果）

```yaml
schema_version: 1
file_type: "result_task"
results:
  - id: "res_1771722300_e5f0c3d8"
    task_id: "task_1771722060_b7c1d4e9"
    command_id: "cmd_1771722000_a3f2b7c1"
    status: "completed"                  # completed | failed（Worker 報告）| cancelled（デーモン生成のみ）
    summary: "POST /api/login を実装。JWT 発行・検証のテストも追加済み。"
    files_changed:                       # 変更ファイルリスト（任意）
      - "src/api/login.ts"
      - "tests/api/login.test.ts"
    partial_changes_possible: false      # true の場合、部分的な変更がリポジトリに残っている可能性あり
    retry_safe: true                     # false の場合、リトライ時に手動復旧が必要な可能性あり（Planner が判断）
    notified: false                      # result_handler が通知済みなら true
    notify_attempts: 0                   # 通知試行回数
    notify_lease_owner: null             # "daemon:<pid>" / null
    notify_lease_expires_at: null        # ISO8601 / null
    notified_at: null                    # 通知完了時刻（ISO8601 / null）
    notify_last_error: null              # 直近の通知失敗理由（文字列 / null）
    created_at: "2026-02-22T10:05:00+09:00"
```

> **`status` の設定者**: Worker は `completed` または `failed` のみを報告する（`maestro result write --status <completed|failed>`）。`cancelled` はデーモンがキャンセル中断（`agent_executor --interrupt`）実行時に**合成的な結果エントリ**として作成するシステムステータスである。これにより既存の通知パイプライン（Result ハンドラ → Planner 通知）をそのまま利用でき、キャンセル時の特殊経路が不要となる。

## 4.5 results/planner.yaml（Planner → Orchestrator: コマンド実行結果）

`tasks` フィールドは `maestro result write` が自動集約する（Agent は渡さない）。

```yaml
schema_version: 1
file_type: "result_command"
results:
  - id: "res_1771722600_f1a2b3c4"
    command_id: "cmd_1771722000_a3f2b7c1"
    status: "completed"                  # completed | failed | cancelled
    summary: "認証機能の実装が完了。ログイン・ログアウト・セッション管理の全 API とテストを作成。"
    tasks:                               # ← result write が results/worker{N}.yaml から自動集約
      - task_id: "task_1771722060_b7c1d4e9"
        worker: "worker3"
        status: "completed"
        summary: "ログイン API 実装完了"
      - task_id: "task_1771722120_c2d3e5f0"
        worker: "worker1"
        status: "completed"
        summary: "ログアウト API 実装完了"
    notified: false
    notify_attempts: 0
    notify_lease_owner: null
    notify_lease_expires_at: null
    notified_at: null
    notify_last_error: null
    created_at: "2026-02-22T10:10:00+09:00"
```

## 4.6 queue/orchestrator.yaml（Orchestrator への完了通知）

```yaml
schema_version: 1
file_type: "queue_notification"
notifications:
  - id: "ntf_1771722600_d4e9f0a2"
    command_id: "cmd_1771722000_a3f2b7c1"
    type: "command_completed"            # command_completed | command_failed | command_cancelled
    source_result_id: "res_1771722600_f1a2b3c4"  # 元の planner result ID（冪等重複排除用）
    content: "認証機能の実装が完了しました。詳細は results/planner.yaml を参照してください。"
    priority: 100                        # 優先度（小さいほど高優先。デフォルト 100。デーモンが自動設定）
    status: "pending"                    # pending ⇄ in_progress → completed | dead_letter（配信失敗時 in_progress → pending に戻る）
    attempts: 0
    last_error: null
    dead_lettered_at: null
    dead_letter_reason: null
    lease_owner: null
    lease_expires_at: null
    lease_epoch: 0                       # 単調増加フェンシングトークン（配信ごとに +1）
    created_at: "2026-02-22T10:10:00+09:00"
    updated_at: "2026-02-22T10:10:00+09:00"
```

## 4.7 state/commands/{command_id}.yaml（command 完了条件の正本）

`state/commands/{command_id}.yaml` は command 単位の完了条件と task 状態の**唯一の正本**である。
Planner Agent は直接編集しない。`maestro plan` と `maestro result write` のみが更新する。

```yaml
schema_version: 1
file_type: "state_command"
command_id: "cmd_1771722000_a3f2b7c1"
plan_version: 1
plan_status: "sealed"                   # planning（plan submit 処理中の内部一時状態）| sealed | completed | failed | cancelled
completion_policy:
  mode: "all_required_completed"        # 現在はこれのみサポート
  allow_dynamic_tasks: false            # seal 後の task 追加を禁止（例外: plan add-retry-task は sealed 後も追加可能）
  on_required_failed: "fail_command"    # required task が failed → command を failed に
  on_required_cancelled: "cancel_command" # required task が cancelled → command を cancelled に
  on_optional_failed: "ignore"          # optional task の失敗は command 完了をブロックしない
  dependency_failure_policy: "cancel_dependents" # 依存先失敗時: cancel_dependents（デフォルト）
cancel:                                 # キャンセル情報（null = 未キャンセル）
  requested: false
  requested_at: null                    # ISO8601 / null
  requested_by: null                    # "orchestrator" / "planner" / null
  reason: null                          # キャンセル理由（文字列 / null）
expected_task_count: 2                  # required + optional の合計
required_task_ids:
  - "task_1771722060_b7c1d4e9"
  - "task_1771722120_c2d3e5f0"
optional_task_ids: []
task_dependencies:                      # タスク間依存グラフ（権威データ。DAG であること必須）
  task_1771722060_b7c1d4e9: []              # 依存なし
  task_1771722120_c2d3e5f0:                 # task_b7c1 完了後に実行
    - "task_1771722060_b7c1d4e9"
task_states:
  task_1771722060_b7c1d4e9: "completed"     # pending | in_progress | completed | failed | cancelled
  task_1771722120_c2d3e5f0: "completed"
cancelled_reasons: {}                        # キャンセル理由（key=task_id, value=理由文字列）
# --- 依存失敗キャンセルの例 ---
# cancelled_reasons:
#   task_1771722120_c2d3e5f0: "blocked_dependency_terminal:task_1771722060_b7c1d4e9"
#   → kind=blocked_dependency_terminal, source=失敗元タスク ID
#   → cascade recovery の対象判定に使用
# --- コマンドキャンセルの例 ---
# cancelled_reasons:
#   task_1771722120_c2d3e5f0: "command_cancel_requested"
#   → cascade recovery の対象外
applied_result_ids:
  task_1771722060_b7c1d4e9: "res_1771722300_e5f0c3d8"  # 冪等反映済み結果 ID
  task_1771722120_c2d3e5f0: "res_1771722360_a1b2c3d4"
system_commit_task_id: null               # 継続モード時にデーモンが自動挿入したコミットタスクの ID（null = 非継続モード / 未挿入）。phases 付きの場合、このタスクはいずれのフェーズの task_ids にも含まれない（フェーズ構造の外に独立して存在）
retry_lineage: {}                        # リトライ置換の監査履歴（key=new_task_id, value=replaced_task_id）
# --- リトライ発生時の例 ---
# retry_lineage:
#   task_1771722500_f1a2b3c4: "task_1771722060_b7c1d4e9"  # new_task が old_task を置換
phases: null                             # null = 単一フェーズ（後方互換）。フェーズ付き submit 時は配列
# --- フェーズ付きの場合の例 ---
# phases:
#   - phase_id: "phase_1771722000_a1b2c3d4"
#     name: "research"
#     type: "concrete"                     # concrete | deferred
#     status: "active"                     # pending | awaiting_fill | filling（内部一時状態）| active | completed | failed | cancelled | timed_out
#     depends_on_phases: []
#     task_ids:
#       - "task_1771722060_b7c1d4e9"
#     constraints: null                    # concrete では null
#     activated_at: "2026-02-22T10:01:00+09:00"
#     completed_at: null
#     fill_deadline_at: null
#
#   - phase_id: "phase_1771722000_e5f6a7b8"
#     name: "implementation"
#     type: "deferred"
#     status: "pending"
#     depends_on_phases: ["research"]
#     task_ids: []
#     constraints:
#       max_tasks: 6
#       allowed_bloom_levels: [1, 2, 3, 4, 5, 6]
#       timeout_minutes: 60
#     activated_at: null
#     completed_at: null
#     fill_deadline_at: null               # awaiting_fill 遷移時に設定
last_reconciled_at: null                # 直近の reconciliation 実行時刻
created_at: "2026-02-22T10:01:00+09:00"
updated_at: "2026-02-22T10:10:00+09:00"
```

**`phases` フィールド**:

- `phases: null` の場合はデーモン内部で暗黙の単一 concrete フェーズとして扱う（後方互換）
- フェーズ付き submit 時は配列として格納。フェーズ構造は初回 submit で確定し、後からのフェーズ追加・削除・制約変更は禁止

**フェーズ状態遷移**:

```
phase_status(concrete):  active → completed | failed | cancelled
                                              ↗ active（add-retry-task による再オープン。reopened_at を記録）
phase_status(deferred):  pending → awaiting_fill → filling（内部一時状態）→ active → completed | failed | cancelled
                                    ↘ timed_out（fill 期限超過）              ↗ active（add-retry-task による再オープン）
```

> `timed_out` は `awaiting_fill` からの遷移（Planner が `fill_deadline_at` までにタスクを投入しなかった場合）。`filling` は内部一時状態であり、`filling` からの `timed_out` 遷移はない。

- **フェーズ再オープン**: `failed` フェーズに対して `add-retry-task --retry-of` でリトライタスクが追加された場合、デーモンがフェーズを `active` に再オープンし `reopened_at` を記録する。これは terminal 不変性の唯一の例外であり、リトライ回復に限定される
- `plan_status` の値は変更なし（`planning` → `sealed` → `completed` | `failed` | `cancelled`）
- `plan_status: sealed` は「フェーズ骨格が確定」の意

**フェーズフィールド定義**:

| フィールド | 型 | 説明 |
|---|---|---|
| `phase_id` | string | グローバル一意 ID（`phase_{unix_timestamp}_{random_hex8}`） |
| `name` | string | フェーズ名（submit 内で一意） |
| `type` | string | `concrete`（タスク即投入）/ `deferred`（遅延投入） |
| `status` | string | フェーズの現在状態 |
| `depends_on_phases` | string[] | 依存フェーズの name リスト |
| `task_ids` | string[] | フェーズに所属するタスク ID |
| `constraints` | object（deferred: **必須**）/ null（concrete: null 固定） | deferred フェーズの制約。`max_tasks`（`> 0`）と `timeout_minutes`（`> 0`）は必須。`allowed_bloom_levels` は任意（省略時はデーモンが `[1..6]` をデフォルト補完）。concrete では null 固定（設定時はバリデーションエラー） |
| `activated_at` | string/null | フェーズが active になった時刻 |
| `completed_at` | string/null | フェーズが terminal になった時刻 |
| `fill_deadline_at` | string/null | deferred フェーズの fill 期限（`awaiting_fill` 遷移時に設定） |
| `reopened_at` | string/null | `failed` フェーズが `add-retry-task` により `active` に再オープンされた時刻（null = 再オープンなし） |

### 4.7.1 `plan submit` の入出力スキーマ

**入力（Planner が渡す tasks YAML）**:

```yaml
tasks:
  - name: "login-api"                    # plan 内ローカル名（blocked_by で参照。submit 内でのみ有効）
    purpose: "ユーザー認証の入口となるログイン API を提供する"
    content: "JWT を使ったログイン API エンドポイントを実装"
    acceptance_criteria: "POST /api/login が 200 を返しトークンが発行される"
    constraints:
      - "既存の /api/health エンドポイントに影響を与えないこと"
    blocked_by: []                       # ローカル name で参照（例: ["login-api"]）
    bloom_level: 3
    required: true                       # false → optional task
    tools_hint: ["context7"]             # Worker に推奨する MCP ツール名（任意）
  - name: "session-mgmt"
    purpose: "ログイン後のセッション管理を提供する"
    content: "セッション管理 API を実装"
    acceptance_criteria: "セッション CRUD API が動作する"
    constraints: []
    blocked_by: ["login-api"]            # ローカル name → submit 内で task_id に解決
    bloom_level: 4
    required: true
```

**出力（stdout JSON）**:

```json
{
  "command_id": "cmd_1771722000_a3f2b7c1",
  "tasks": [
    {
      "name": "login-api",
      "task_id": "task_1771722060_b7c1d4e9",
      "worker": "worker1",
      "model": "sonnet"
    },
    {
      "name": "session-mgmt",
      "task_id": "task_1771722120_c2d3e5f0",
      "worker": "worker3",
      "model": "opus"
    }
  ]
}
```

> `name` フィールドは `plan submit` 内部での依存解決のみに使用。state/commands/ や queue/ には保存されない。外部参照には必ず `task_id` を使用する。
> `__` プレフィックスを持つ name はシステム予約名であり、Planner が使用することを禁止する（`__system_commit` 等）。

**フェーズ付き入力**（既存の `tasks` トップレベルは後方互換で維持。`phases` と `tasks` は排他）:

```yaml
phases:
  - name: "research"
    type: "concrete"
    tasks:
      - name: "analyze-codebase"
        purpose: "既存の認証パターンを分析"
        content: "コードベースの認証関連コードを読み解く"
        acceptance_criteria: "認証パターンのサマリが得られる"
        constraints: []
        blocked_by: []            # 同一フェーズ内の name のみ参照可
        bloom_level: 4
        required: true
        tools_hint: []
  - name: "implementation"
    type: "deferred"
    depends_on_phases: ["research"]
    constraints:
      max_tasks: 6
      allowed_bloom_levels: [1, 2, 3, 4, 5, 6]
      timeout_minutes: 60
```

**フェーズ付き出力**:

```json
{
  "command_id": "cmd_...",
  "phases": [
    {
      "name": "research",
      "phase_id": "phase_...",
      "type": "concrete",
      "status": "active",
      "tasks": [{"name": "analyze-codebase", "task_id": "task_...", "worker": "worker1", "model": "opus"}]
    },
    {
      "name": "implementation",
      "phase_id": "phase_...",
      "type": "deferred",
      "status": "pending",
      "tasks": []
    }
  ]
}
```

**フェーズ fill 入力**（`plan submit --phase implementation`）:

```yaml
tasks:
  - name: "implement-login"
    purpose: "調査結果に基づきログイン API を実装"
    content: "JWT ベースの POST /api/login を実装"
    acceptance_criteria: "POST /api/login が 200 を返す"
    constraints: []
    blocked_by: []
    bloom_level: 3
    required: true
    tools_hint: []
```

## スキーマ原則まとめ

| ディレクトリ | 処理判定条件 | 意味 |
|---|---|---|
| `queue/*` | `status: "pending"` かつ `blocked_by` 解決済み（全依存 `completed`）、または (`status: "in_progress"` かつ `lease_expires_at < now`)。依存先が terminal 失敗（`failed`/`cancelled`/`dead_letter`）の場合は配信ではなく `cancelled` に遷移 | デーモンが配信/再配信/依存失敗処理すべきエントリ |
| `results/*` | `notified: false` | デーモンが通知すべき未処理エントリ |
| `state/commands/*` | `plan_status`, `required_task_ids`, `task_states` | command 完了可否の判定正本（`maestro plan` が管理） |

queue 側は lease を加味して判定し、クラッシュ時の取りこぼしを回収する。
完了判定は queue/results から直接推測せず、必ず `state/commands/{command_id}.yaml` を参照する。

## 4.8 state/metrics.yaml（可観測性メトリクス）

```yaml
schema_version: 1
file_type: "state_metrics"
queue_depth:
  planner: 0                            # pending コマンド数
  orchestrator: 0                       # pending 通知数
  workers:
    worker1: 0
    worker2: 0
counters:
  commands_dispatched: 0
  tasks_dispatched: 0
  tasks_completed: 0
  tasks_failed: 0
  tasks_cancelled: 0
  dead_letters: 0
  reconciliation_repairs: 0
  notification_retries: 0
daemon_heartbeat: null                   # デーモン最終活動時刻（ISO8601）
updated_at: null
```

> メトリクス更新はベストエフォート。コアの dispatch/result パスをブロックしてはならない。
> デーモンの定期スキャン時に更新。dashboard_gen モジュールがダッシュボードに反映。

## 4.9 state/continuous.yaml（継続モード状態）

`continuous.enabled: true` 時にデーモンが管理する。デーモン再起動後もイテレーション数を復元可能。

```yaml
schema_version: 1
file_type: "state_continuous"
current_iteration: 0                     # 現在のイテレーション数（デーモンが管理）
max_iterations: 10                       # config.yaml から読み込み（参照用コピー）
status: "stopped"                        # running | paused | stopped（初期値: stopped。maestro up --continuous で running に遷移）
paused_reason: null                      # 停止理由（文字列 / null）
last_command_id: null                    # 直近のイテレーションで投入された command_id
updated_at: null
```

> `config.yaml` の `continuous.*` は静的設定。`state/continuous.yaml` は実行時の動的状態。デーモンは起動時に本ファイルが存在すれば `current_iteration` を復元し、存在しなければ初期値で新規作成する。

## 4.10 スキーマバージョニング

全 YAML ファイルにトップレベル `schema_version`（整数）と `file_type`（文字列）を持つ。

| フィールド | 用途 |
|---|---|
| `schema_version` | YAML 構造のバージョン。メジャー変更時にインクリメント |
| `file_type` | ファイルの種類を識別。リーダーが期待する構造と照合 |

**バージョニング規則**:

- リーダーは `schema_version` と `file_type` を検証。未サポートのメジャーバージョンは処理拒否（fail fast）
- `maestro up` 起動時に旧バージョン検出時はマイグレーション（バックアップ優先）
- 1 プロジェクト内で複数のスキーマメジャーバージョンが混在してはならない
- `maestro.version`（config.yaml）はアプリケーションバージョンであり、スキーマバージョンとは独立

### YAML ストレージの exit criteria

現在は YAML 配列 + Go YAML ライブラリによる書き換えを採用している。以下の条件のいずれかに達した場合、SQLite 等への移行を検討する:

- YAML ファイル書き込みの p95 レイテンシが 500ms を超過
- 定常的にファイルサイズが `max_yaml_file_bytes` の 80% を超過
- ロック競合によるリトライが定期スキャン 1 回あたり 3 回以上発生

この判断は意図的な保留（deliberate deferral）であり、無期限の先送りではない。

### ステータス状態遷移

**terminal ステータスの不変性**:

- **task/command レベル**: `completed`, `failed`, `cancelled`, `dead_letter` が terminal
- **phase レベル**: `completed`, `failed`, `cancelled`, `timed_out` が terminal（`dead_letter` はなし）

一度 terminal に遷移したステータスは変更不可（フェーズ再オープンの例外あり、後述）。

```
queue エントリ:            pending ⇄ in_progress → completed | failed | cancelled
                           pending → dead_letter  [attempts >= max_attempts]
                           （配信失敗 / lease 期限切れ回収で in_progress → pending に戻る。
                            dead_letter は pending からの条件付き遷移のみ。in_progress からの直接遷移はない）
results エントリ:          completed | failed | cancelled（作成時に terminal で書き込み）
plan_status:               planning（内部一時状態）→ sealed → completed | failed | cancelled
phase_status(concrete):    active → completed | failed | cancelled
                                                ↗ active（add-retry-task による再オープン）
phase_status(deferred):    pending → awaiting_fill → filling（内部一時状態）→ active → completed | failed | cancelled
                                      ↘ timed_out（fill 期限超過）              ↗ active（add-retry-task による再オープン）
```

> **フェーズ再オープン**: `failed` → `active` への遷移は `add-retry-task --retry-of` によるリトライ回復に限定された唯一の例外。`completed` / `cancelled` / `timed_out` からの再オープンは禁止。
>
> **`timed_out` はフェーズ専用**: `timed_out` は deferred フェーズのみに存在する terminal ステータスであり、task/command レベルには存在しない。

**queue 型別の terminal ステータス**:

| queue 型 | terminal ステータス |
|---|---|
| command (`queue/planner.yaml`) | `completed` \| `failed` \| `cancelled` \| `dead_letter` |
| task (`queue/worker{N}.yaml`) | `completed` \| `failed` \| `cancelled` \| `dead_letter` |
| notification (`queue/orchestrator.yaml`) | `completed` \| `dead_letter` |

> 上記の一般化された遷移図はスーパーセットである。notification は配信成功 (`completed`) または配信上限超過 (`dead_letter`) のみ遷移し、`failed` / `cancelled` には遷移しない。
