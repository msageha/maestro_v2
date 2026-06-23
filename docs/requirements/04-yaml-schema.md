# 4. YAML スキーマ定義

## 4.0 ID 体系

全 ID はグローバル一意とする。形式: `{type}_{unix_timestamp}_{random_hex16}`

```
コマンド ID:  cmd_1771722000_a3f2b7c19d4e5f60
タスク ID:    task_1771722060_b7c1d4e9f0a2b3c4
フェーズ ID:  phase_1771722000_c3d4e5f6a7b8c9d0
通知 ID:      ntf_1771722600_d4e9f0a2b3c4d5e6
結果 ID:      res_1771722300_e5f0c3d8a9b0c1d2
```

- `maestro queue write`、`maestro result write`、`maestro plan submit`（フェーズ ID 含む）、`maestro plan add-retry-task` が生成を担当
- Agent は ID を指定しない（writer の stdout で採番された ID を受け取る）
- タイムスタンプベースのため、アーカイブやファイル削除で衝突しない
- `random_hex16`（8 バイト・64bit）により、同一秒内に同種 ID を複数生成しても衝突確率は無視できる（誕生日問題: 同一秒内 100 万件生成でも衝突確率 < 0.0000003%）。なお旧仕様の `random_hex8`（32bit, 8hex）形式も後方互換のため受理する
- **type prefix の列挙**: `cmd`（command）/ `task` / `phase` / `ntf`（notification）/ `res`（result）/ `skc`（skill candidate）/ `dsp`（dispatch）。`skc` は skill 候補、`dsp` は配信トラッキング用 ID に使用する
- **ID 形式の正規表現**: `^(cmd|task|phase|ntf|res|skc|dsp)_[0-9]{10}_[0-9a-f]{8}([0-9a-f]{8})?$`（末尾 8hex は省略可能で、新規採番は 16hex・旧 ID の 8hex も受理）。デーモンは生成時・受理時にこの形式を検証する
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
  awaiting_fill_stall_notify_minutes: 5  # awaiting_fill の Planner が停滞した際に再プロンプト（awaiting_fill_stall シグナル）を発火するまでの経過分数。既定 5。0 で無効（R6 のハード fill_deadline_at タイムアウトのみ適用）

agents:
  orchestrator:
    id: "orchestrator"
    model: "opus"
    base_prompt_mode: "append"   # append（--append-system-prompt）| replace（--system-prompt）。既定 append
  planner:
    id: "planner"
    model: "opus"
    base_prompt_mode: "append"   # 各ロールで共通システムプロンプトを追記するか置換するかを制御。既定 append
  workers:
    count: 4                     # 1-8（上限 8。maestro setup / up で検証）
    default_model: "sonnet"      # models に未定義の Worker はこれを使用
    models:                      # 個別指定（省略時は default_model）
      worker3: "opus"
      worker4: "opus"
      # worker1, worker2 → default_model (sonnet)
    boost: false                 # true → 全 Worker を opus に昇格
    base_prompt_mode: "append"   # 全 Worker 共通。append | replace。既定 append

continuous:
  enabled: false                 # --continuous フラグで有効化
  max_iterations: 10             # 最大ループ回数
  pause_on_failure: true         # タスク失敗時に自動停止
  max_consecutive_failures: 3    # 連続失敗コマンド数がこの値に達したら継続モードを自動停止。0 で無効（pause_on_failure とは独立し、false でも発火）
  stall_notification_sec: 600    # 継続モード稼働中に次イテレーションが投入されないまま経過した場合、Orchestrator へ continuous_stalled 通知を発火する閾値（秒）。0/未設定で 600、負値で無効

# 注: 旧仕様の `notify:` セクション（macOS 通知）は実装に存在しない。
# ユーザーへの完了・停止の伝達は Orchestrator ペイン経由で行う（[§5.12](05-script-responsibilities.md) 参照）。

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
  max_dead_letter_archive_files: 100  # dead_letters/ に保持するアーカイブファイルの最大数（超過分は古い順に削除）。未設定時はデフォルト適用
  max_quarantine_files: 100           # quarantine/ に保持する破損 YAML の最大数。未設定時はデフォルト適用

shutdown_timeout_sec: 90         # graceful shutdown のドレイン待機上限（秒）。トップレベルキー（`daemon:` 配下ではない）

logging:
  level: "info"                  # debug | info | warn | error
```

### 4.1.1 追加セクション（実装で拡張済み）

上記のコアセクションに加え、実装は以下のセクションを持つ（いずれも `omitempty` で省略可。未設定時は安全側のデフォルトが適用される。YAML decode は非 strict であり、未知フィールドでデーモンを止めない）。各機能の意味は [REQUIREMENTS.md](REQUIREMENTS.md) を参照。

```yaml
admission_control:    # S0-1: 同時 verify 数・同時 repair 数のハード上限
circuit_breaker:      # S2-2: 多角的サーキットブレーカー（失敗連鎖の遮断）
quality_gates:        # 配信前/後・フェーズ遷移・command 検証のゲート（enforcement.failure_action: warn|block）
learnings:            # S2-1/C-5: Failure Fingerprint DB・学習知見
worktree:             # git worktree 隔離・integration マージ・publish の設定
verify:               # verify パイプライン（enabled: false は正常運用モード。S1-1）
review:               # A-1: 非同期 Read-only レビュアー（Advisory）
skills:               # skill レジストリ参照設定
feature_profiles:     # C-8: 複雑度レベル別の機能プロファイル（simple/standard/complex/critical）
# --- Phase C 個別機能（feature_profiles でゲート。既定は概ね無効） ---
bandit:               # C-2 適応的モデル選択（UCB1）
evolution:            # C-1 進化的コード品質
extended_verification:# C-3 多観点アンサンブル検証
search:               # C-4 探索的実装最適化
self_improvement:     # C-5 自己改善
complexity:           # C-6 適応的計算深度
```

> **`agents.workers.models` の値**: `worker3: opus` のような Claude モデル名のほか、`worker4: codex` / `gemini` で各ランタイムの既定モデル、`codex-5` / `gemini-2.5-pro` のように `codex-` / `gemini-` プレフィックス付きで明示モデルを指定して Worker のランタイムを切り替えられる（モデル名そのものへの exact / prefix マッチ。`codex/o4-mini` のようなスラッシュ区切りは claude-code 扱いになり起動失敗する。[§11](11-future-extensions.md) 参照）（[REQUIREMENTS.md](REQUIREMENTS.md) §5 C-7）。Orchestrator / Planner は claude-code 限定。

## 4.2 queue/planner.yaml（Orchestrator → Planner: コマンド）

```yaml
schema_version: 1
file_type: "queue_command"
commands:
  - id: "cmd_1771722000_a3f2b7c1"
    content: "認証機能を実装して、ログイン・ログアウト・セッション管理を含めてください"
    skill_refs: []                       # 参照する skill 名のリスト（任意。省略可）
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
    # --- 撤退条件・想定変更パス（REQUIREMENTS.md §2.2 / S3-1。全タスクで必須化） ---
    definition_of_done:                  # 完了条件（省略時は acceptance_criteria を使用）
      - "POST /api/login が 200 を返す"
    definition_of_abort:                 # 撤退条件（MUST）
      max_repair_count: 3
      max_wall_clock_sec: 1800
      explicit_failure_conditions: []
    expected_paths:                      # コンフリクト予測用の変更パスプレフィックス（S3-1）
      - "src/api/"
    # --- 役割・実行制御（実装拡張。未指定時はデフォルト動作） ---
    persona_hint: ""                     # Worker に適用する persona（省略可）
    skill_refs: []                       # 参照する skill 名のリスト（省略可）
    operation_type: ""                   # 操作種別ヒント（省略可）
    run_on_main: false                   # 実 main checkout 上で実行するか（true は非 claude worker に割り当てない。C-7）
    run_on_integration: false            # integration worktree 上で実行するか
    complexity_level: ""                 # simple | standard | complex | critical（C-8。未指定時 Planner 自動判定）
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
| `definition_of_done` | 任意 | 完了条件のリスト。省略時は `acceptance_criteria` を使用（`GetDoneConditions()`）|
| `definition_of_abort` | 必須 | 撤退条件（S3-1, S2-2）。`max_repair_count` / `max_wall_clock_sec` / `explicit_failure_conditions`。Circuit Breaker の閾値正本 |
| `expected_paths` | 必須 | 想定変更パスのプレフィックスリスト（S3-1）。Path-overlap Heuristic（A-4）のコンフリクト予測に使用 |
| `persona_hint` | 任意 | Worker に適用する persona 名 |
| `skill_refs` | 任意 | Worker が参照する skill 名のリスト |
| `operation_type` | 任意 | 操作種別ヒント |
| `run_on_main` | 任意 | 実 main checkout 上で実行するか（既定 false）。`true` は非 claude worker に割り当てない（C-7） |
| `run_on_integration` | 任意 | integration worktree 上で実行するか（既定 false） |
| `complexity_level` | 任意 | `simple` / `standard` / `complex` / `critical`（C-8）。未指定時 Planner 自動判定、Orchestrator オーバーライド可 |
| `priority` | 任意 | 優先度（小さいほど高優先。デフォルト 100）。デーモンが自動設定。Agent は指定しない |

**実装拡張フィールド（デーモン管理。Agent は指定しない。いずれも `omitempty`）**:

`queue/worker{N}.yaml` のタスク型はデーモンが配信制御・リトライ追跡・ランタイム解決のために用いる内部管理フィールド群（`dispatch_id` / `execution_retries` / `original_task_id` / `not_before` / `in_progress_at` / `runtime` / `model_override` 等）を持つ。これらは Agent が指定するものではなく、各フィールドの型・意味・不変条件は **source of truth である `internal/model/queue.go` の定義（godoc コメント）を正本**とする。本書はこれら実装詳細を逐一ミラーせず、Agent が関与しない旨の明示に留める（要件文書が実装を後追いミラーする構造を避けるため）。

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
    notify_dead_lettered: false          # 通知リトライ上限超過で終端化されたら true（完了結果の silent loss を防ぐ）
    notify_dead_lettered_at: null        # 通知 dead-letter 化時刻（ISO8601 / null）
    notify_dead_letter_reason: null      # 通知 dead-letter 理由（文字列 / null）
    quality_gate_evaluation: null        # quality gate 評価結果（passed/action/failed_gates/evaluated_at 等。null = 未評価）
    run_on_integration: false            # queue タスクの run_on_integration をミラー。verify パイプライン通知ゲートの分類を結果に固定するため
    run_on_main: false                   # queue タスクの run_on_main をミラー（同上）
    verify_outcome_applied_at: null      # デーモン所有の verify パイプラインが最終結果を反映した時刻（RFC3339 / null）。run_on_integration/run_on_main の通知はこのマーカーが立つまで発火しない
    created_at: "2026-02-22T10:05:00+09:00"
```

> **`rejected_submissions`（results ファイルのトップレベル）**: `result write` の付随データ（learnings / skill_candidates）が、Worker が有効な lease を持たない（lease epoch 失効後の stale Worker が同一 task_id に再送した等）ために破棄された場合、その監査記録を `results` と並ぶトップレベル配列 `rejected_submissions` に残す。各エントリは `id` / `task_id` / `command_id` / `reporter` / `reason` / `request_lease_epoch` / `queue_lease_epoch` / `lost_learnings` / `lost_skill_candidates` / `dedup_key` 等を持つ。これらは plan reconcile・rebuild・metrics カウンターには関与せず、監査・ユーザー通知のためだけに保持される。

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
    task_stats:                          # ← result write が tasks から自動集計。Summary 自由文に依存しない機械可読な件数の正本
      total: 2
      completed: 2
      failed: 0
      cancelled: 0
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
    type: "command_completed"            # command_completed | command_failed | command_cancelled | continuous_paused | continuous_stopped | continuous_stalled
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

> **継続モード通知**: `continuous_paused`（pause_on_failure 等で paused に遷移）/ `continuous_stopped`（max_iterations・max_consecutive_failures 到達で stopped に遷移）/ `continuous_stalled`（稼働中に次イテレーションが `stall_notification_sec` 投入されない停滞）は、デーモンが継続モードの状態変化を Orchestrator に伝えるための通知型である。Orchestrator はこれらを受けて次イテレーション生成の再実行・停止判断・ユーザー報告を行う。

## 4.6.1 queue/planner_signals.yaml（デーモン → Planner: 構造化シグナル）

`queue/planner_signals.yaml` は、フェーズ完了・コミット失敗・マージコンフリクトなどのイベントを、自由文ではなく構造化された形で Planner に届けるためのシグナルキューである。Planner はこれらを受けてリカバリアクション（再計画・フェーズ fill・コンフリクト解決の指示等）を判断する。デーモンがリース方式で配信し、配信上限超過時は dead-letter 化する。

```yaml
schema_version: 1
file_type: "planner_signal_queue"
signals:
  - kind: "phase_completed"               # シグナル種別（phase_completed / commit_failed / merge_conflict 等）
    command_id: "cmd_1771722000_a3f2b7c1"
    phase_id: "phase_1771722000_a1b2c3d4"
    phase_name: "research"
    message: "research フェーズが完了しました"  # 後方互換の自由文（構造化フィールドを優先）
    worker_id: ""                         # per-worker シグナル（commit_failed 等）の識別子。phase レベルでは空
    reason: ""                            # 構造化分類（例 "all_files_filtered" / "policy_violation:max_files_exceeded"）
    attempts: 0                           # 配信試行回数
    last_attempt_at: null                 # 直近の配信試行時刻（ISO8601 / null）
    next_attempt_at: null                 # 次回試行時刻（ISO8601 / null）
    last_error: null                      # 直近の配信失敗理由（文字列 / null）
    # --- merge_conflict 系シグナルで設定される構造化フィールド ---
    conflict_type: ""                     # "task_merge_conflict"（worker → integration）/ "publish_conflict"（integration → base）
    conflict_base_ref: ""                 # 共通祖先 ref
    conflict_ours_ref: ""                 # integration 側 ref
    conflict_theirs_ref: ""               # worker 側 ref
    conflict_files: []                    # git が衝突報告したファイル一覧
    conflict_generation: ""               # integration HEAD / worker SHA / phase_id / worker_id から算出する CAS トークン
    # --- コンフリクト解決ライフサイクル（resolver 状態機械） ---
    resolution_state: ""                  # pending | dispatched | resolving | failed（resolver パイプライン未投入時は空）
    resolve_attempt: 0                    # resolver の commit 試行回数
    last_resolution_error: ""             # 直近の resolver エラー
    created_at: "2026-02-22T10:10:00+09:00"
    updated_at: "2026-02-22T10:10:00+09:00"
```

> **dedup キー**: phase レベルのシグナルは `(kind, command_id, phase_id)` で重複排除される。`commit_failed` のように worker ごとに区別が必要なシグナルは `worker_id` を加えて per-worker に別エントリとして保持する。
> 構造化フィールド（`reason` / `conflict_*` / `resolution_state` 等）は、Planner が自由文 `message` をパースせずにリカバリ判断できるようにするためのもの。`conflict_type` が空・未設定のレガシーシグナルは `task_merge_conflict` として扱う。

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
circuit_breaker:                        # command 単位の Circuit Breaker 状態（S2-2。失敗連鎖の遮断）
  consecutive_failures: 0               # 連続失敗カウント
  last_progress_at: null                # 直近で進捗があった時刻（ISO8601 / null）
  tripped: false                        # ブレーカー作動中なら true
  tripped_at: null                      # 作動時刻（ISO8601 / null）
  trip_reason: null                     # 作動理由（文字列 / null）
  half_open: false                      # half-open（試験的再開）状態か
  half_open_at: null                    # half-open 遷移時刻（ISO8601 / null）
  half_open_probe_active: false         # half-open 中のプローブタスクが実行中か
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
  task_1771722060_b7c1d4e9: "completed"     # 意味的ライフサイクル（拡張状態。下記注記参照）
  task_1771722120_c2d3e5f0: "completed"
# `task_states` の値は queue エントリの 6 状態（pending|in_progress|completed|failed|cancelled|dead_letter）に加え、
# 意味的ライフサイクルの拡張状態（planned|ready|dispatched|running|verify_pending|repair_pending|paused_for_replan|paused_for_human|aborted）を取りうる。
# 2 層モデルの詳細は §4.10 の「ステータス状態遷移」および [REQUIREMENTS.md](REQUIREMENTS.md) §2.1 を参照。
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
queue_write_failed: {}                    # 結果は反映済みだが queue の terminal 書き込みが失敗した task（key=task_id, value="workerID:resultID"。H2 sticky error の追跡）
idempotency_keys: {}                      # 冪等キー → task_id の対応（リトライ時の重複タスク注入を防止）
retry_lineage: {}                        # リトライ置換の監査履歴（key=new_task_id, value=replaced_task_id）
retry_enqueue_failed: {}                  # state は登録済みだが queue への追加が失敗した task（key=task_id, value=worker_id）
# --- リトライ発生時の例 ---
# retry_lineage:
#   task_1771722500_f1a2b3c4: "task_1771722060_b7c1d4e9"  # new_task が old_task を置換
heavy_verify_owners: {}                  # フェーズごとに重量級 verify（test/security/performance）を実行する task を予約（key=phase_id, value=task_id）。全 sibling が互いの verify_pending を見て defer し誰も repo 全体検証を回さない race を防ぐ
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
#     fill_deadline_at: null               # awaiting_fill 遷移時に設定（ハード期限）
#     filling_started_at: null             # filling 内部一時状態に入った時刻
#     awaiting_fill_since: null            # 直近に awaiting_fill に入った時刻。stall watchdog の経過計測に使用（awaiting_fill を抜けるとクリア）
#     awaiting_fill_stall_notified_at: null # stall watchdog が直近に再プロンプトを発火した時刻。scan 毎の重複発火を抑止
#     cancelled_reason: null               # フェーズがキャンセルされた理由。cascade-cancel は構造化プレフィックス付き（auto-recover 可）／空・nil は operator/manual キャンセル（terminal）
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
| `phase_id` | string | グローバル一意 ID（`phase_{unix_timestamp}_{random_hex16}`。§4.0 参照。旧 8hex も受理） |
| `name` | string | フェーズ名（submit 内で一意） |
| `type` | string | `concrete`（タスク即投入）/ `deferred`（遅延投入） |
| `status` | string | フェーズの現在状態 |
| `depends_on_phases` | string[] | 依存フェーズの name リスト |
| `task_ids` | string[] | フェーズに所属するタスク ID |
| `constraints` | object（deferred: **必須**）/ null（concrete: null 固定） | deferred フェーズの制約。`max_tasks`（`> 0`）と `timeout_minutes`（`> 0`）は必須。`allowed_bloom_levels` は任意（省略時はデーモンが `[1..6]` をデフォルト補完）。concrete では null 固定（設定時はバリデーションエラー） |
| `activated_at` | string/null | フェーズが active になった時刻 |
| `completed_at` | string/null | フェーズが terminal になった時刻 |
| `fill_deadline_at` | string/null | deferred フェーズの fill 期限（`awaiting_fill` 遷移時に設定） |
| `filling_started_at` | string/null | `filling` 内部一時状態に入った時刻 |
| `awaiting_fill_since` | string/null | 直近に `awaiting_fill` に入った時刻。stall watchdog が `fill_deadline_at` より前に再プロンプトを発火するための経過計測に使用。`awaiting_fill` を抜けるとクリア |
| `awaiting_fill_stall_notified_at` | string/null | stall watchdog が直近に stall シグナルを発火した時刻。scan サイクル毎の重複発火を抑止。`awaiting_fill_since` とともにクリア |
| `cancelled_reason` | string/null | フェーズがキャンセルされた理由。依存 cascade-cancel は構造化プレフィックス付きで記録され auto-recover 対象。空・nil は operator/manual キャンセルで terminal |
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
  signal_deliveries: 0                   # Planner シグナル配信成功数
  signal_retries: 0                      # シグナル配信リトライ数
  signal_dead_letters: 0                 # シグナル配信上限超過で dead-letter 化した数
  signal_inline_retry_successes: 0       # インラインリトライでシグナル配信が成功した数
  lease_renewals: 0                      # lease 更新数
  lease_extensions: 0                    # lease 延長数
  lease_releases: 0                      # lease 解放数
worktree_commands_stalled: 0             # ゲージ: Phase A の stall 検知で integration branch が stalled と判定されている command 数
bak_files_count: 0                       # ゲージ: スキャン時点で maestro ディレクトリ配下に存在する .bak ファイル総数
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
consecutive_failures: 0                  # 連続失敗コマンド数。非失敗コマンドで 0 にリセット。config の max_consecutive_failures ゲートが参照
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

> **状態モデルの 2 層構造**: タスクには独立した 2 つの状態レイヤーが存在する。混同を避けるため必ず区別すること。
> - **queue エントリの `status`（配信制御レイヤー）**: `queue/worker{N}.yaml` 等の各エントリが持つ。デーモンの配信の進行を表し、6 状態（`pending ⇄ in_progress → completed | failed | cancelled`、`pending → dead_letter`）のみを取る。
> - **`task_states` の値（意味的ライフサイクルレイヤー）**: `state/commands/{id}.yaml` が持つ正本。タスクの意味的進捗を表し、上記 6 状態に加え拡張状態（`planned` / `ready` / `dispatched` / `running` / `verify_pending` / `repair_pending` / `paused_for_replan` / `paused_for_human` / `aborted`）を取る。実装は `internal/model/status.go` の `validTaskStateTransitions` で管理する。`verify_pending` 以降の状態はこのレイヤーにのみ存在し、queue エントリには現れない。
>
> 2 層の対応とライフサイクル全体像は [REQUIREMENTS.md](REQUIREMENTS.md) §2.1 を参照。以下の図はレイヤーごとに分けて示す。

**terminal ステータスの不変性**:

- **task/command レベル**: `completed`, `failed`, `cancelled`, `dead_letter` が terminal（task_states レイヤーでは `aborted` も terminal）
- **phase レベル**: `completed`, `failed`, `cancelled`, `timed_out` が terminal（`dead_letter` はなし）

一度 terminal に遷移したステータスは変更不可（フェーズ再オープンの例外あり、後述）。

```
queue エントリ:            pending ⇄ in_progress → completed | failed | cancelled
                           pending → dead_letter  [attempts >= max_attempts]
                           （配信失敗 / lease 期限切れ回収で in_progress → pending に戻る。
                            dead_letter は pending からの条件付き遷移のみ。in_progress からの直接遷移はない）
task_states（意味的）:     pending → in_progress → verify_pending → completed | repair_pending
                           （拡張ライフサイクル: planned → ready → dispatched → running → verify_pending）
                           verify_pending → repair_pending → running（リトライ）| paused_for_replan
                           paused_for_replan → ready（再計画後）/ 任意の非 terminal → paused_for_human | aborted
                           pending → dead_letter | cancelled
                           （意味的進捗のため queue と異なり in_progress → pending の後退はしない）
results エントリ:          completed | failed | cancelled（作成時に terminal で書き込み）
plan_status:               planning（内部一時状態）→ sealed → completed | failed | cancelled
phase_status(concrete):    active → completed | failed | cancelled
                                                ↗ active（add-retry-task による再オープン）
phase_status(deferred):    pending → awaiting_fill → filling（内部一時状態）→ active → completed | failed | cancelled
                                      ↘ timed_out（fill 期限超過）              ↗ active（add-retry-task による再オープン）
                           pending / awaiting_fill / filling → failed（fast-track stall cleanup の例外遷移）
                           cancelled → pending（dependency-cascade recovery の例外遷移）
```

> **task_states 図の簡略化について**: 上図の task_states レイヤーは主要経路の要約であり、`validTaskStateTransitions`（`internal/model/status.go`）が遷移の正本である。実装では `in_progress` を `dispatched` / `running` の合成として扱い、`verify_pending` を意味的進捗の次段に置く（[REQUIREMENTS.md](REQUIREMENTS.md) §2.1）。`verify.enabled: false` の運用では `verify_pending` を経由せず `in_progress → completed` で確定する。
>
> **フェーズ再オープン**: `failed` → `active` への遷移は `add-retry-task --retry-of` によるリトライ回復に限定された唯一の例外。`completed` / `cancelled` / `timed_out` からの再オープンは禁止。
>
> **`cancelled` → `pending`（dependency-cascade recovery）**: scheduler 由来の cascade-cancel（`Phase.cancelled_reason` に構造化プレフィックスを持つ）で、上流の terminal 失敗が解消した場合に限り、dependency resolver がフェーズを `pending` に戻す。operator/manual キャンセル（プレフィックスなし）は terminal のまま。
>
> **`pending` / `awaiting_fill` / `filling` → `failed`（fast-track stall cleanup）**: Planner の停滞などで通常経路が進まないフェーズを、デーモンが直接 `failed` に落として publish gate に判断を委ねる例外遷移。
>
> **`timed_out` はフェーズ専用**: `timed_out` は deferred フェーズのみに存在する terminal ステータスであり、task/command レベルには存在しない。

**worktree 状態遷移**（`validWorktreeTransitions`。`internal/model/status.go`）:

worktree（worker 単位の隔離作業ツリー）は 10 状態を取る: `created` / `active` / `committed` / `integrated` / `published` / `conflict` / `resolving` / `failed` / `cleanup_done` / `cleanup_failed`。

```
created → active | committed | integrated（no-op merge）| conflict | failed | published | cleanup_done | cleanup_failed
active → active（再 sync）| committed | integrated | conflict | failed | published | cleanup_done | cleanup_failed
committed → active | committed | integrated | conflict | failed | published | cleanup_done | cleanup_failed
integrated → active（cross-phase sync）| committed | published | conflict | failed | cleanup_done | cleanup_failed
conflict → active（解決）| resolving | failed | published | cleanup_done | cleanup_failed
resolving → active（再 merge）| integrated | conflict | failed | cleanup_done | cleanup_failed
failed → published | cleanup_done | cleanup_failed
published → cleanup_done | cleanup_failed
cleanup_failed → cleanup_done（リトライ）
cleanup_done →（terminal）
```

> `cleanup_done` のみが terminal。`cleanup_failed` はリトライで `cleanup_done` に遷移できるため terminal ではない。

**integration 状態遷移**（`validIntegrationTransitions`。`internal/model/status.go`）:

integration（command 単位の統合ブランチ）は 10 状態を取る: `created` / `merging` / `merged` / `publishing` / `published` / `conflict` / `partial_merge` / `publish_failed` / `failed` / `quarantined`。

```
created → merging | failed | quarantined
merging → merged | conflict | partial_merge | failed | quarantined
merged → merging（次フェーズの再 merge）| publishing | failed | quarantined
publishing → published | conflict | publish_failed | failed | quarantined
publish_failed → publishing（リトライ）| merged（retry-publish 回復）| failed | quarantined
partial_merge → merging | failed | quarantined
conflict → merging（リトライ）| failed | quarantined
failed → failed（再失敗）| merging（リトライ）| quarantined
published →（terminal）
quarantined →（terminal。operator の手動介入が必要）
```

> `published` と `quarantined` が terminal。`failed` はリトライで `merging` に戻せるため terminal ではない。`quarantined` は merge/publish が繰り返し失敗した際の終端で、CLI 経由の operator 介入で復旧する。

**queue 型別の terminal ステータス**:

| queue 型 | terminal ステータス |
|---|---|
| command (`queue/planner.yaml`) | `completed` \| `failed` \| `cancelled` \| `dead_letter` |
| task (`queue/worker{N}.yaml`) | `completed` \| `failed` \| `cancelled` \| `dead_letter` |
| notification (`queue/orchestrator.yaml`) | `completed` \| `dead_letter` |

> 上記の一般化された遷移図はスーパーセットである。notification は配信成功 (`completed`) または配信上限超過 (`dead_letter`) のみ遷移し、`failed` / `cancelled` には遷移しない。
