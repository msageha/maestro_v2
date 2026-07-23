# 5. サブコマンド責務定義

全機能は単一 Go バイナリ `maestro` のサブコマンドとして提供される。ビルド・インストールは mise task（`check-deps` / `build` / `install`。`mise.toml` の `[tasks.*]`）が担う。

## 共通規則

### アトミック書き込みパターン

全 YAML 書き込みは以下のパターンに従う（Go 標準ライブラリで実装）:

1. 一時ファイル（`os.CreateTemp`）に書き込み
2. YAML 構文検証（`gopkg.in/yaml.v3` でパース可能か確認）
3. 検証 OK → `os.Rename` で対象ファイルにアトミック置換（APFS 同一ボリュームでアトミック）
4. 検証 NG → 一時ファイル削除、エラーログ出力、処理中断

### 破損 YAML ハンドリング

YAML ファイルの読み取り時にパースに失敗した場合:

1. 破損ファイルを `.maestro/quarantine/{filename}.{timestamp}.corrupt` に退避
2. `.bak` ファイル（最終正常書き込み時のバックアップ）が存在すればリストア
3. `.bak` もなければ最小限のスケルトンファイルを再生成（空の配列構造）
4. `daemon.log` に WARN/ERROR で記録（dashboard.md / metrics.yaml にも反映）
5. 他のファイルの処理は継続（1 ファイルの破損で全体を停止しない）

### schema_version 検証

全リーダーは YAML ファイル読み取り時に `schema_version` と `file_type` を検証する。未サポートのメジャーバージョンはエラー終了（fail fast）。

### 排他制御

デーモンプロセス内では `sync.Mutex` で排他制御を行う。CLI サブコマンドからの書き込み要求は Unix ドメインソケット経由でデーモンに委譲されるため、CLI 側でのファイルロックは不要。

### Unix ドメインソケットプロトコル

CLI ↔ デーモン間の通信は `.maestro/daemon.sock` 上の Unix ドメインソケットで行い、ワイヤーフォーマットとして **JSON + length-prefix フレーミング**（4 バイト BigEndian のペイロード長 + JSON ペイロード）を採用する。

### ログフォーマット

ログファイル（`.maestro/logs/daemon.log`, `.maestro/logs/agent_executor.log`）はシステムがプログラム的に読むことはなく、人間によるデバッグ・トラブルシューティング専用のため、**human-readable 形式**を採用する。形式: `{timestamp} {level} {message}`（例: `2026-02-23T10:01:00+09:00 INFO queue write completed: cmd_1771722000_a3f2b7c1`）。ログレベルは `config.yaml` の `logging.level` に従う。

## 5.1 mise tasks（ビルド・インストール）

**責務**: システムレベルのビルドとインストール（旧 `install.sh` / `Makefile` の代替）

- `mise run check-deps`: 依存コマンドの存在チェック（`tmux`, `go`, `claude`）。不足時はインストール手順を案内して失敗
- `mise run build`: `go build` で単一バイナリ `maestro` をビルド
- `mise run install`: `check-deps` → `build` → ビルド済みバイナリを配置（実行権限付与）
- `mise run lint` / `mise run test` / `mise run format` / `mise run clean`、および `mise run docker-*`（コンテナ実行）も提供

> 旧 `install.sh`（Bash ブートストラップ）・旧 `Makefile` は廃止し、`mise.toml` の `[tasks.*]` に統合した。依存チェック・ビルド・配置のセマンティクスは同等。

## 5.2 maestro setup

**責務**: プロジェクト単位の初期化

```
Usage: maestro setup <project_dir> [project_name]
```

> `project_name` は任意。省略時は `<project_dir>` から導出する（ステップ 4 の `project.name` 自動埋めに使用）。明示指定した場合はその名前を採用する。

1. `<project_dir>/.maestro/` ディレクトリ構造を作成
2. `templates/` からファイルをコピー（config.yaml, maestro.md, instructions/, dashboard.md）
3. `queue/`, `results/`, `state/commands/`, `locks/`, `logs/`, `dead_letters/`, `quarantine/` ディレクトリを作成
   - `state/metrics.yaml` を初期値で作成
   - `state/continuous.yaml` を初期値で作成（`current_iteration: 0`, `status: "stopped"`）
   - `locks/daemon.lock` を作成（per-agent / per-command の排他制御はデーモン内の sync.Mutex で行うためファイルロックは daemon.lock のみ）
4. `config.yaml` の `project.name`, `maestro.project_root`, `maestro.created` を自動埋め
5. Worker 数に応じて `queue/worker{N}.yaml`, `results/worker{N}.yaml` を作成

## 5.3 maestro up

**責務**: フォーメーション起動・停止の統括

```
Usage: maestro up [--boost] [--continuous] [--detach|-d] [--force|-f]
```

**フラグ**:

- `--boost`: 全 Worker を opus に昇格して config.yaml に反映
- `--continuous`: 継続モードを有効化して config.yaml に反映
- `--detach` / `-d`: 起動後に tmux セッションへ自動 attach せず、バックグラウンドで起動完了して終了
- `--force` / `-f`: 既にフォーメーションが稼働中でも強制起動

> **`up` は起動時に常に状態をリセットする**: `maestro up` は起動シーケンスの一部として無条件に `resetFormation`（`internal/formation/up.go:68` "Always reset state on startup"）を実行し、`queue/` / `results/` / `state/commands/` の YAML をクリアし、`state/continuous.yaml` / `state/metrics.yaml` を初期値で再生成する。in-progress だったコマンドはクリア前に `quarantine/lost_commands/` へ退避され、前回セッションの作業記録が黙って失われないようにする。旧 `--reset` / `--no-notify` フラグは廃止済み（リセットはフラグではなく既定動作）。`--force` / `-f` は稼働中フォーメーションを破棄して再生成するためのガード解除であり、状態保全とは無関係（むしろ force-reset として lost_commands 退避の対象になる）。継続中の状態を保ったまま再操作したい場合は `up` を再実行せず `maestro attach` を使う。

**起動シーケンス**:

1. `.maestro/config.yaml` の存在チェック
2. **環境・runtime preflight**（リソース作成前の fail-fast）:
   - tmux バイナリの PATH 解決と AF_UNIX ソケット疎通プローブ（sandbox 検出）
   - config の agents.\* から解決される全 runtime（claude-code / codex / gemini）の CLI バイナリを PATH 解決。**バイナリ不在は確実な失敗として `up` を中断**（tmux セッション・デーモン作成前）
   - 認証状態は best-effort（env 変数・credential ファイルの存在確認のみ）。確認不能は stderr への警告に留め、起動は止めない。詳細診断は [§5.18 maestro doctor](#518-maestro-doctorワンショット)
3. **スタートアップリカバリ**:
   a. daemon ロックで他の `maestro up` との競合防止
   b. 必要なディレクトリ・YAML ファイルの存在確認。欠損があれば再作成
   c. 全 YAML ファイルの構文検証（破損ファイルは quarantine → バックアップからリストア）
   d. `schema_version` チェック。旧バージョン検出時はマイグレーション（バックアップ優先）
   e. ワンショット reconciliation を実行（R0, R0b, R1-R10 の全パターンの不整合修復。[§5.8 ステップ 3](#58-maestro-daemon) 参照）
4. `--boost` / `--continuous` フラグを config.yaml に反映
5. tmux セッション作成（内部で `maestro` の tmux モジュールを使用。per-instance socket で分離。既存セッションがあれば再利用 or 再作成）
   - Window 0 `orchestrator`: 1 ペイン
   - Window 1 `planner`: 1 ペイン
   - Window 2 `workers`: 最大 2 列 × 4 行のグリッド（Worker 数分のペイン）
   - 各ペインに tmux ユーザー変数を設定:
     - `@agent_id`: `orchestrator`, `planner`, `worker1` ... `worker{N}`
     - `@role`: `orchestrator`, `planner`, `worker`
     - `@model`: `opus`, `sonnet` 等（config.yaml の `default_model` + `models` から解決。Worker は `codex` / `codex-5` / `gemini-2.5-pro` 等の非 claude ランタイムも可。[§11](11-future-extensions.md) 参照）
     - `@status`: `idle`（初期値）
6. 各ペインで `maestro agent launch` を実行
7. `maestro daemon` をバックグラウンド起動（daemon ロック + PID 記録）
8. `--detach` でなければ tmux セッションへ attach。`--detach` の場合は起動完了メッセージを表示して終了

> **起動は冪等**: 何度実行しても安全。既に正常稼働中の場合はデーモンの daemon ロックで二重起動を防止。
> **再接続**: detach 後に再度操作したい場合は `maestro attach` でセッションへ戻る。

## 5.4 maestro down

**責務**: フォーメーション停止

```
Usage: maestro down
```

1. デーモンプロセスに graceful shutdown リクエストを送信（Unix ドメインソケット経由）
2. デーモンが `shutdown accepted` を返却後、graceful shutdown シーケンスを開始
3. CLI は shutdown 完了を待機（デフォルト 100 秒。デーモンの shutdown タイムアウト 90 秒 + マージン）
4. デーモン終了後、tmux セッションを終了
5. 終了コード: `0`（正常終了 or 既に停止済み）、`1`（タイムアウト or 異常）

**Graceful shutdown シーケンス**（デーモン内部）:

shutdown は `sync.Once` で冪等に実行する。UDS リクエスト・`SIGTERM`・`SIGINT` のいずれから発火しても同一のシーケンスを 1 回だけ実行する。2 回目の `SIGTERM` / `SIGINT` 受信時はドレインを打ち切り即座にプロセス終了する（緊急停止）。

1. **draining 状態に遷移** — 全ハンドラが参照する shutdown context をキャンセルし、新規処理を受け付けない状態にする
2. **全プロデューサーを停止** — 以下を並行して実行:
   - UDS listener を close（以降の CLI 接続は connection refused）
   - fsnotify watcher を close（新規イベントの受付を停止）
   - 定期スキャン ticker を停止（reconciler / dead_letter / dashboard_gen）
3. **受付済み in-flight 処理のドレイン** — shutdown context に紐づくデッドライン（デフォルト 90 秒、`config.yaml` の `daemon.shutdown_timeout_sec` で設定可能）付きで以下の完了を待機:
   - 処理中の UDS リクエスト（アトミック書き込み完了まで）
   - 処理中の agent_executor 配信（ビジー判定 + tmux send-keys 完了まで。各 agent_executor は shutdown context を監視し、新規のビジーリトライは行わない）
   - 処理中の fsnotify イベントハンドラ（現在の YAML 書き込み完了まで）
4. **タイムアウト時の処理** — デッドライン到達時に未完了の処理がある場合、未完了の操作をログに記録しドレインを終了する。`in_progress` のまま残ったエントリは lease 付きで保持され、次回デーモン起動時の定期スキャンで lease 期限切れとして回収・再配信される
5. **クリーンアップ → プロセス終了** — `.maestro/daemon.sock` を unlink → daemon ロックファイルを解放 → プロセス終了

> **in_progress エントリのロールバックは行わない**: shutdown 時に `in_progress` を `pending` に戻すと、Agent が既にタスクを受信して実行中の場合に二重実行のリスクが生じる。lease 期限切れによる自然回収の方が安全でシンプルである。

## 5.5 maestro agent launch

**責務**: 各ペインで Agent CLI を起動

1. tmux ユーザー変数（`@agent_id`, `@role`, `@model`）を読み取り
2. システムプロンプトを構築:
   - `maestro.md`（共通部分）
   - `instructions/{role}.md`（役割固有部分）
   - を結合
3. ランタイムに応じてコマンドを組み立てて実行:
   - **claude-code（既定 / Orchestrator・Planner は必須）**:
     ```bash
     claude --model {model} \
            --append-system-prompt "{system_prompt}" \
            --dangerously-skip-permissions
     ```
     > **注意（`--dangerously-skip-permissions` は best-effort）**: 組織の managed settings が `permissions.disableBypassPermissionsMode: "disable"` を配布している環境では、このフラグは無視され claude は default permission mode に silent downgrade する（2026-07-03 に `~/.claude/remote-settings.json` 経由で観測）。同様に `sandbox.enabled: true` が強制されると Bash が OS サンドボックス下で動く。無人運転はこのフラグ単独に依存せず、全 claude-code role に注入する PreToolUse ポリシーフック（`.maestro/hooks/worker-policy.sh`、明示的 `allow` 決定でプロンプトを短絡）と `workerDisallowedTools` のブロック、`--settings` のキャッシュ `allowWrite` 追加で成立させる（[internal/agent/launcher.go](../../internal/agent/launcher.go) の `dangerousPermissionBypassFlag` コメント参照）。
   - **codex / gemini（Worker のみ）**: `launchAlternativeWorker` が各ランタイムのコマンド・フラグ（codex: `--dangerously-bypass-approvals-and-sandbox`、gemini: `--yolo` 等）と環境を組み立てて exec する。これらは claude CLI を経由しないため、上記 claude 組織ポリシーの影響を受けない。Orchestrator / Planner role に non-claude-code が来た場合は launcher が hard-reject する（[REQUIREMENTS.md](REQUIREMENTS.md) §5 C-7）

## 5.6 maestro queue write（CLI → デーモン）

**責務**: queue/ ディレクトリへの YAML 書き込み（唯一の書き込み経路）

CLI は Unix ドメインソケット経由でデーモンに要求を送信し、デーモンが実際の書き込みを行う。

**名前付き引数インターフェース**（Agent が raw YAML を組み立てる必要を排除）:

```
# Orchestrator → Planner（コマンド追加）
maestro queue write planner \
  --type command \
  --content "認証機能を実装してください"
# → stdout に採番された ID を出力: cmd_1771722000_a3f2b7c1

# 注: Planner → Worker のタスク追加は maestro plan submit で一括処理する（§5.7.1 参照）
# Planner が queue write worker{N} を直接呼び出す必要はない

# Orchestrator → キャンセル要求（queue エントリは作成しない。デーモンが内部処理）
# 注: この `queue write --type cancel-request` 経路は deprecated。CLI 実行時に
#     「`maestro plan request-cancel --command-id <id>` を使え」という WARNING を出す。
#     canonical な経路は `plan request-cancel`（§5.7.1）。両経路はデーモン内部で
#     同一の cancel-request ハンドラに収束するため、振る舞いは等価。
maestro queue write planner \
  --type cancel-request \
  --command-id cmd_1771722000_a3f2b7c1 \
  --reason "ユーザーによるキャンセル"
# → デーモンがキャンセル処理を実行し、stdout にコマンド ID を出力
#   - state/commands/ が存在する場合: cancel.requested = true を設定
#   - state/commands/ が未作成の場合: queue/planner.yaml のエントリを直接 cancelled に遷移

# result_handler → Orchestrator（完了通知追加）
maestro queue write orchestrator \
  --type notification \
  --command-id cmd_1771722000_a3f2b7c1 \
  --notification-type command_completed \
  --source-result-id res_1771722600_f1a2b3c4 \
  --content "認証機能の実装が完了しました"

# User → Orchestrator（ユーザーメッセージ。tmux ペイン外から Orchestrator を操作する経路）
maestro queue write orchestrator \
  --type message \
  --content "優先度を見直してから続行してください"
# → queue/orchestrator.yaml に type=user_message のエントリを追加し、デーモンが
#   Orchestrator ペインへ配信する。結果ファイルを持たず content 自体が envelope で届く。
#   caller role は CLI のみ許可（Agent ペインからの呼び出しは拒否）。
#   continuous gate の対象外（paused/stopped 中でもユーザーは Orchestrator に到達できる）
```

**処理フロー**（デーモン内部）:

1. `--type` に応じて処理を分岐:
   - **`--type cancel-request` の場合**: queue エントリを作成せず、デーモンがコマンドのライフサイクル段階に応じてキャンセル処理を実行する。stdout にコマンド ID を出力して終了。以降のステップ 2-11 は実行しない
     - **`state/commands/{command_id}.yaml` が存在する場合（submit 済み）**: `cancel` フィールドを更新する（`cancel.requested: true`, `cancel.requested_at: now`, `cancel.requested_by: "orchestrator"`, `cancel.reason: {reason}`）。冪等: 既に `cancel.requested: true` の場合はスキップ
     - **`state/commands/{command_id}.yaml` が存在しない場合（未 submit）**: per-agent mutex を取得し、`queue/planner.yaml` の該当コマンドエントリ（`id == command_id`）を `status: cancelled` に遷移。エントリに `cancel_reason: {reason}`, `cancel_requested_at: now`, `cancel_requested_by: "orchestrator"` を記録。mutex 解放。エントリが存在しない場合はエラー終了（無効なコマンド ID）。**ガード: エントリが既に terminal ステータス（`completed`, `failed`, `cancelled`, `dead_letter`）の場合はスキップ**（terminal 不変性の保証。§4 ステータス状態遷移 参照）
   - **それ以外**: 以降のステップで YAML 構造を構築
2. `--content` のバイト数を検証（`max_entry_content_bytes` 超過（`>`）時はエラー終了）
3. ID をグローバル一意形式で生成（`{type}_{unix_timestamp}_{random_hex8}`）
4. per-agent mutex を取得
5. **バックプレッシャーチェック**（mutex 保持中。check-then-add パターンのため、追加後に上限を超えないことを保証する）:
   - `--type command`: pending コマンド数 `>= max_pending_commands` → エラー終了（追加すると上限超過するため拒否）
   - `--type task`: 対象 worker の pending タスク数 `>= max_pending_tasks_per_worker` → エラー終了（同上）
   - YAML ファイルサイズ: `current_size + estimated_entry_size > max_yaml_file_bytes` → アーカイブ可能エントリを先にアーカイブしてリトライ。それでも超過する場合はエラー終了（追加後サイズで判定）
6. **`--type notification` の場合**: `--source-result-id` で既存エントリと重複チェック。同一 `source_result_id` が既に存在する場合はスキップ（冪等）
7. **`--type task` の場合**: `--blocked-by` の簡易バリデーション（自己参照がないか、ID 形式が `task_` プレフィックスの正規形式か）
   - 注: `maestro plan submit` 経由でのタスク登録では、submit 内部で全タスクの DAG 検証（§5.7.1 参照）を一括で行うため、この簡易バリデーションは主にデーモン内部からの直接呼び出し時のガードとして機能する
8. YAML インジェクション防止しつつエントリ追加（Go の YAML ライブラリによる構造体マーシャリング）
9. `status: "pending"`, `attempts: 0`, `lease_owner: null`, `lease_expires_at: null`, `updated_at` を設定
10. mutex 解放
11. 採番した ID を CLI 経由で stdout に出力

## 5.7 maestro result write（CLI → デーモン）

**責務**: results/ への結果書き込み + 対応する queue/ エントリ更新 + state/commands/ の task 状態更新

**名前付き引数インターフェース**:

```
# Worker → Planner（タスク結果報告）
maestro result write worker1 \
  --task-id task_1771722060_b7c1d4e9 \
  --command-id cmd_1771722000_a3f2b7c1 \
  --lease-epoch 7 \
  --status completed \
  --summary "POST /api/login を実装。JWT 発行・検証のテストも追加済み。" \
  --files-changed "src/api/login.ts,tests/api/login.test.ts"

# Planner → Orchestrator（コマンド結果報告）
# 注: Planner は maestro plan complete を使用する（§5.7.1 参照）
# maestro result write planner を直接呼び出す必要はない
# plan complete が内部で can-complete 検証 + result write を一括実行する
```

**主要フラグ**:

- `--summary <text>` / `--summary-file <path|->`: 結果サマリの本文。`--summary-file` はファイルまたは `-`（stdin）から読み取る。両者は排他（デーモンに渡る値を一意にするため）。長文や shell 特殊文字を含む場合は `--summary-file` を使い、Worker の PreToolUse policy hook が短い構造化 argv のみを走査するようにする
- `--status failed` のときは `--exit-code <n>` が**必須**。デーモンの auto-retry 判定（`ShouldRetryTask`）は exit code を必須入力とするため、未指定だと repair pipeline が走らず silent drop になる。`completed` の場合は省略可（`-1` = 未報告 sentinel）
- `--learnings <text>`（繰り返し可）/ `--skill-candidates <text>`（繰り返し可）: 他タスクへの学びと skill 候補。個別エントリは上限超過時に切り詰める
- `--partial-changes`: 失敗時にリポジトリへ部分変更が残る可能性を示す
- `--no-retry-safe`: タスクをリトライ非安全としてマークする（既定は retry-safe）

> **placeholder summary の拒否**: `--summary` が既知の placeholder（`test` / `done` / `tbd` / `n/a` 等）に正規化一致する場合は拒否する。さらに `--status completed` のサマリは正規化後 16 文字以上を要求する（`failed` は post-mortem が簡潔になりがちなため下限を課さない）。これにより、policy hook で弾かれた Worker が中身のないサマリを正規結果として landing させる事故を、silent-completed ではなくクリーンな validation エラーとして表面化させる（`validateSummaryNotPlaceholder`）。

**処理フロー（2 フェーズロック）**（デーモン内部）:

**reporter が worker の場合**:

**フェーズ A（per-agent mutex）**: `results/` と `queue/` の更新

1. per-agent mutex を取得
2. **mutex 保持中に以下を実行**:
   a. 冪等キーを検査（`task_id`）。同一キーの確定済み結果がある場合は重複追加しない
   a'. **フェンシング検証**: デーモンが `queue/{reporter}.yaml` から該当タスクの現在の `lease_epoch` を取得し、Worker が報告した `--lease-epoch` と一致すること、かつ該当タスクが `in_progress` かつ正当な lease を持つことを確認。不一致の場合は stale worker とみなし結果を拒否
   b. 重複がない場合のみ結果 ID をグローバル一意形式で生成
   c. 重複がない場合のみ `results/{reporter}.yaml` に結果エントリを追加（`notified: false`, 通知 lease フィールド初期化）
   d. `queue/{reporter}.yaml` の該当エントリのステータスを `completed` または `failed` に更新
   （同時に `lease_owner`, `lease_expires_at` を `null` にクリア）
3. per-agent mutex 解放

**フェーズ B（per-command mutex）**: `state/commands/` の更新

4. per-command mutex を取得
5. **mutex 保持中に以下を実行**:
   - `state/commands/{command_id}.yaml` の `task_states[{task_id}]` を `completed` / `failed` に更新
   - `applied_result_ids[{task_id}] = {result_id}` を記録（再配信時の二重反映防止）
6. per-command mutex 解放

**依存解決のトリガー**: result write 側では依存解放処理を行わない。完了タスクに依存する他タスクは、デーモンのキュースキャン（`queue/{reporter}.yaml` 更新の fsnotify で誘発されるスキャン、および `scan_interval_sec` ごとの定期スキャン）が `blocked_by` を再評価して配信する（[§6](06-execution-flow.md) と同一の記述）。かつて設計されていた「依存タスクの queue ファイルを touch して fsnotify を強制発火させる」フェーズ C は実装されていない。

**reporter が planner の場合**:

> **注**: Planner は `maestro plan complete`（§5.7.1）を使用する。以下のフローは `plan complete` の内部処理として実行される。Planner が `maestro result write planner` を直接呼び出すことはない。

**フェーズ 0（per-command mutex、事前検証）**: 完了条件の検証

1. per-command mutex を取得
2. `maestro plan can-complete --command-id {id}` で完了条件を検証
3. 検証 NG の場合は per-command mutex を解放し、終了コード 1 で即時終了。`results/`・`queue/`・`state/commands/` を一切更新しない。未完了 task 一覧をエラーメッセージに含める
4. 検証 OK の場合は `derived_status`（`completed` / `failed` / `cancelled`）を保持
5. per-command mutex 解放

**フェーズ A（per-agent mutex）**: `results/` と `queue/` の更新

6. per-agent mutex を取得
7. **mutex 保持中に以下を実行**:
   a. 冪等キーを検査（`command_id`）。同一キーの確定済み結果がある場合は重複追加しない
   b. 重複がない場合のみ結果 ID をグローバル一意形式で生成
   c. `results/worker{N}.yaml` から同一 `command_id` のエントリを収集し `tasks` を自動構築
   d. `results/planner.yaml` に結果エントリを追加（`notified: false`, ステータスは `derived_status` を使用）
   e. `queue/planner.yaml` の該当エントリのステータスを `derived_status` に更新
   （同時に `lease_owner`, `lease_expires_at` を `null` にクリア）
8. per-agent mutex 解放

**フェーズ B（per-command mutex）**: `state/commands/` の更新

9. per-command mutex を取得
10. `state/commands/{command_id}.yaml` の `plan_status` を `derived_status` に更新（フェーズ 0 で検証済みのため再検証は行わない）
11. per-command mutex 解放

> **排他制御の範囲と限界**: per-agent mutex と per-command mutex を分離することで、同一 command に対する複数 Worker の同時完了でも `state/commands/` の書き込み競合が発生しない。
> 2 つの mutex は同時保持しない（ネストしない）ため、デッドロックリスクはゼロ。
> Planner の result write ではフェーズ 0 で事前に `can-complete` を検証するため、フェーズ A で不正な結果が書き込まれるリスクを排除している。
> Worker の result write ではフェーズ A 完了後・フェーズ B 開始前にクラッシュした場合、`results/` 書き込み済み + `state/` 未更新の不整合が残る可能性がある。
> この不整合は定期スキャンの **reconciliation ステップ**（[§5.8 ステップ 3](#58-maestro-daemon)）で自動修復される。
> 依存解放は result write 側では行わず、デーモンのキュースキャンが `blocked_by` を再評価して配信する。
>
> **Planner result write のフェンシング省略**: Worker の result write では `lease_epoch` フェンシング検証により stale worker の結果を拒否するが、Planner の result write（`plan complete` 経由）ではフェンシングを行わない。これは以下の理由による:
>
> - Planner は単一インスタンスであり、同一タスクに対する Worker のような並行競合が発生しない
> - `plan complete` がフェーズ 0 で `can-complete`（per-command mutex 付き CAS 検証）を事前実行するため、stale な完了報告は原理的に拒否される
> - Worker の `lease_epoch` は「同一タスクの再配信による並行報告の排除」が目的であり、Planner のユースケース（コマンド単位の完了報告）とは競合モデルが異なる

## 5.7.1 maestro plan（CLI → デーモン）

**責務**: `state/commands/{command_id}.yaml` の唯一の更新・参照窓口

Planner Agent は state ファイルを直接編集しない。必ず本サブコマンドを呼び出す。

Planner が使用する主要コマンド（`submit` / `complete` / `add-retry-task`）に加え、`add-task`（動的タスク追加）も現役である。`add-task` は seal 後の plan へのタスク純増（dynamic task injection）を担い、worktree の `merge_conflict` 解決でも Planner が `plan add-task --worker-id <worker>` で解決タスクを投入する経路として使われる（[§5.8 R7](#58-maestro-daemon)）。`add-retry-task` は失敗タスクを「置換」する例外（`expected_task_count` 不変）であり、`add-task` の純増とは目的が異なる:

```bash
# タスク分解・Worker 割当・plan 確定を 1 コマンドで実行
maestro plan submit \
  --command-id cmd_1771722000_a3f2b7c1 \
  --tasks-file /dev/stdin <<'EOF'
tasks:
  - name: "login-api"
    purpose: "ユーザー認証の入口となるログイン API を提供する"
    content: "JWT を使ったログイン API エンドポイントを実装"
    acceptance_criteria: "POST /api/login が 200 を返しトークンが発行される"
    constraints:
      - "既存の /api/health エンドポイントに影響を与えないこと"
    blocked_by: []
    bloom_level: 3
    required: true
  - name: "session-mgmt"
    purpose: "ログイン後のセッション管理を提供する"
    content: "セッション管理 API を実装"
    acceptance_criteria: "セッション CRUD API が動作する"
    constraints: []
    blocked_by: ["login-api"]
    bloom_level: 4
    required: true
EOF
# → stdout に JSON 出力（task_id + worker 対応表。§4.7.1 参照）

# 全タスク完了後にコマンド結果を Orchestrator に報告
# --summary の代わりに --summary-file <path|-> でファイル / stdin からサマリを
# 読み取れる（--summary と排他）。result write と同じ content-size 検証を通す。
maestro plan complete \
  --command-id cmd_1771722000_a3f2b7c1 \
  --summary "認証機能の実装が完了"

# タスク失敗時にリトライタスクを追加（失敗タスクを置換）
maestro plan add-retry-task \
  --command-id cmd_1771722000_a3f2b7c1 \
  --retry-of task_1771722060_b7c1d4e9 \
  --purpose "ログイン API の再実装（JWT ライブラリ変更）" \
  --content "jose ライブラリを使って POST /api/login を再実装" \
  --acceptance-criteria "POST /api/login が 200 を返す" \
  --bloom-level 3
# → stdout に JSON 出力（推移的にキャンセルされた依存タスクの自動復旧を含む）:
# {
#   "task_id": "task_...", "worker": "worker2", "model": "sonnet", "replaced": "task_1771722060_b7c1d4e9",
#   "cascade_recovered": [
#     {"task_id": "task_...", "worker": "worker3", "model": "opus", "replaced": "task_1771722120_c2d3e5f0"}
#   ]
# }

# フェーズ付き submit（調査→実装）
maestro plan submit \
  --command-id cmd_... \
  --tasks-file /dev/stdin <<'EOF'
phases:
  - name: "research"
    type: "concrete"
    tasks:
      - name: "analyze-codebase"
        purpose: "既存の認証パターンを分析"
        content: "コードベースの認証関連コードを読み解く"
        acceptance_criteria: "認証パターンのサマリが得られる"
        bloom_level: 4
        required: true
  - name: "implementation"
    type: "deferred"
    depends_on_phases: ["research"]
    constraints:
      max_tasks: 6
      timeout_minutes: 60
EOF

# フェーズ fill（調査完了後にデーモンから通知を受けて実行）
maestro plan submit \
  --command-id cmd_... \
  --phase implementation \
  --tasks-file /dev/stdin <<'EOF'
tasks:
  - name: "implement-login"
    purpose: "調査結果に基づきログイン API を実装"
    content: "JWT ベースの POST /api/login を実装"
    acceptance_criteria: "POST /api/login が 200 を返す"
    bloom_level: 3
    required: true
EOF
```

### `--dry-run` フラグ

`maestro plan submit --dry-run` はバリデーション（ステップ 1）のみを実行し、副作用（state 作成、queue 書き込み、Worker 割当）を一切発生させない。

- 検証 OK → 終了コード 0、stdout に `{"valid": true}` を出力
- 検証 NG → 終了コード 1、stderr にフィールドパス付きエラーメッセージを出力（後述）
- `--phase` との併用可（deferred フェーズ fill のバリデーションにも使用可能）

LLM Agent が複雑な plan を提出する前に事前検証するためのインターフェース。

### エラーメッセージ形式

`plan submit`（`--dry-run` 含む）および `plan submit --phase` のバリデーションエラーは、stderr に**フィールドパス付き**で出力する。LLM Agent がエラー箇所を特定し自動修正できるようにするため、以下の形式に従う:

```
error: {field_path}: {message}
```

例:

```
error: tasks[0].acceptance_criteria: required field is missing
error: tasks[1].blocked_by[0]: references unknown name "foo"
error: tasks[2].bloom_level: value 7 is out of range (1-6)
error: tasks[0].name: duplicate name "login-api"
error: tasks: circular dependency detected: login-api -> session-mgmt -> login-api
error: phases[1].constraints.max_tasks: must be > 0
```

フィールドパスは入力 YAML の構造に対応する。複数エラーがある場合は全て列挙する（最初の 1 件で打ち切らない）。

### `plan submit` の処理フロー（初回 submit）

`plan submit` は以下をアトミックに実行する。途中で失敗した場合は全変更をロールバックする。`--dry-run` 指定時はステップ 1 のみ実行し、ステップ 2 以降をスキップする。

1. **バリデーション**:
   - `--phase` 未指定であることを確認（`--phase` 指定時はフェーズ fill フローへ分岐）
   - `--command-id` の state ファイルが存在しないことを確認（二重 submit を防止）
   - **キャンセル済みチェック**: `queue/planner.yaml` の該当コマンドエントリが `status: cancelled` の場合はエラー終了（キャンセル要求と submit のレース条件を防止）
   - 入力が `phases` か `tasks` かを判定（排他。両方指定はエラー）
   - **`tasks` の場合（従来フロー）**:
     - tasks YAML の構文検証（必須フィールド、型）
     - `name` の一意性検証
     - `name` が `__` プレフィックス（システム予約名）でないことを検証
     - `blocked_by` のローカル name 参照を解決し、DAG 検証（トポロジカルソート）。循環依存検出時はエラー
     - `blocked_by` が同一 submit 内の name のみを参照していることを検証
     - bloom_level が 1-6 の範囲内であることを検証
     - `tools_hint` が指定されている場合は文字列配列であることを検証（省略可。デフォルト空配列）
   - **`phases` の場合（フェーズ付きフロー）**:
     - フェーズ name の一意性検証
     - **concrete フェーズの `depends_on_phases` は空配列のみ許可**（concrete フェーズは即座に `active` になるため、フェーズ間依存は deferred のみで表現する。違反時は明示エラー）
     - `depends_on_phases` のフェーズレベル DAG 検証（トポロジカルソート。循環依存検出時はエラー）
     - concrete フェーズが最低 1 つ存在すること
     - deferred フェーズの constraints 検証（`max_tasks > 0`, `timeout_minutes > 0`, `allowed_bloom_levels ⊆ {1..6}`（省略時はデーモンが `[1, 2, 3, 4, 5, 6]` をデフォルト補完し state に正規化保存））
     - 各 concrete フェーズ内の tasks: 既存のタスクバリデーション（name 一意、`__` プレフィックス予約名拒否、タスク DAG、bloom_level）
     - `blocked_by` は同一フェーズ内 name のみ参照可（フェーズ間のタスク依存は `depends_on_phases` で表現）

2. **システムコミットタスク自動挿入**（`worktree.enabled: false` の場合のみ。`continuous` の有無とは独立）:
   - **発火条件の正本**: `shouldInsertSystemCommit(cfg) == !cfg.Worktree.Enabled`（`internal/plan/submit_parse.go`）。worktree モード無効時は Worker が main checkout を直接編集するため、明示的なコミットタスクがないと変更が dirty のまま残る。単発・継続のどちらでも「completed = git 履歴に永続化済み」契約を保つために挿入する。worktree モード有効時はデーモンが merge/publish でコミットするため挿入しない（Worker 側コミットは pipeline と競合し有害）
   - Planner が定義したタスク（ユーザータスク）の後に、`__system_commit` タスクを 1 件自動追加
   - `name: "__system_commit"`（予約名）、`bloom_level: 2`、`required: true`、`expected_paths: ["."]`
   - **`tasks` の場合**: タスク配列の末尾に追加。`blocked_by` に全ユーザータスクの name を設定（全タスク完了後に実行）。通常のタスクと同じ `blocked_by` 解決で配信される
   - **`phases` の場合**: フェーズ構造の**外**に独立タスクとして追加（いずれのフェーズの `task_ids` にも含めない）。`blocked_by` は空配列。Queue ハンドラは `system_commit_task_id` と一致するタスクに対し、全ユーザーフェーズが terminal であることを配信条件とする特別判定を行う（[§5.8.1](#581-queue-ハンドラ) 参照）
   - state/commands/ の `system_commit_task_id` フィールドにタスク ID を記録
   - 継続モードとの関係は [§10](10-continuous-mode.md) 参照

3. **Worker 割当**（デーモン内部の割当アルゴリズムを実行。`worker standby` CLI とは独立）:
   - concrete フェーズのタスク + フェーズ外システムタスク（`system_commit_task_id`）が対象（deferred フェーズにはタスクがないため対象外。`__system_commit` はフェーズ構造に関係なく常に割当対象）
   - 各タスクについて以下の優先順で Worker を選択:
     a. **bloom_level → model マッチ**: L1-L3 → Sonnet Worker、L4-L6 → Opus Worker（`boost: true` 時は全て Opus）
     b. **pending 最小**: 同一モデルの Worker 間で pending タスク数が最小の Worker を選択
   - **バックプレッシャーチェック**（check-then-add パターン。§5.6 と同原則）:
     - **早期判定**: 全 Worker で `pending タスク数 + 1 > max_pending_tasks_per_worker`（最低 1 タスクも割当不可）の場合は submit 全体をロールバックしエラー終了
     - **割当後判定**: 各 Worker の `pending タスク数 + 新規割当タスク数 > max_pending_tasks_per_worker` でないことを確認

4. **アトミック書き込み**（以下を全て成功するか、全てロールバック）:
   a. `state/commands/{command_id}.yaml` を作成（`plan_status: planning` → 全書き込み成功後に `sealed` に遷移）
   - task_id をグローバル一意形式で生成
   - ローカル name → task_id のマッピングで `blocked_by` を task_id に変換
   - `required_task_ids`, `optional_task_ids`, `task_dependencies`, `task_states`, `expected_task_count` を設定
   - **`phases` の場合**: `phases` 配列を state ファイルに書き込み。phase_id をグローバル一意形式で生成。concrete フェーズは `status: active`、deferred フェーズは `status: pending`
   - **`tasks` の場合**: `phases: null`（内部で暗黙の単一 concrete フェーズとして処理）
     b. 各 Worker の `queue/worker{N}.yaml` にタスクエントリを追加（concrete フェーズのタスク + `__system_commit`）
     c. `state/commands/{command_id}.yaml` の `plan_status` を `sealed` に更新

5. **ロールバック（失敗時）**:
   - `state/commands/{command_id}.yaml` を削除
   - 追加済みの queue エントリを除去
   - フィールドパス付きエラーメッセージを stderr に出力（「エラーメッセージ形式」参照）

6. **出力**: task_id + worker 対応表を JSON で stdout に出力（§4.7.1 参照。`phases` の場合はフェーズ付き出力形式。`__system_commit` タスクも含む）

> **`planning` は内部一時状態**: `plan_status: planning` は submit 処理中のみ存在する。submit 成功時は `sealed` に遷移し、失敗時は state ファイルごと削除される。外部から `planning` 状態が観測されるのは submit のクラッシュ時のみであり、reconciler の R0 パターンで修復される。

### `plan submit --phase` の処理フロー（フェーズ fill）

`plan submit --phase <name>` は deferred フェーズにタスクを投入する。

1. **バリデーション**:
   - state ファイルが存在し `plan_status: sealed` であること
   - 指定フェーズが `type: deferred` かつ `status: awaiting_fill` であること
   - キャンセル要求なし（`cancel.requested: false`）
   - タスク数 ≤ `constraints.max_tasks`
   - bloom_level が `constraints.allowed_bloom_levels` 内
   - タスクの name 一意性検証
   - `blocked_by` のローカル name 参照を解決し、DAG 検証（フェーズ内のみ）

2. **Worker 割当**: 初回 submit と同じアルゴリズム（bloom_level → model → pending 最小）

3. **アトミック書き込み**:
   - フェーズ status: `awaiting_fill` → `filling`（内部一時状態）→ `active`
   - task_id をグローバル一意形式で生成
   - `required_task_ids` / `optional_task_ids` / `task_dependencies` / `task_states` / `expected_task_count` を更新
   - フェーズの `task_ids` にタスク ID を追加
   - `queue/worker{N}.yaml` にタスクエントリを追加
   - `plan_version` をインクリメント

4. **ロールバック（失敗時）**:
   - フェーズを `awaiting_fill` に戻す
   - 追加した task_ids / queue エントリを除去
   - フィールドパス付きエラーメッセージを stderr に出力（「エラーメッセージ形式」参照）

5. **出力**: task_id + worker 対応表を JSON で stdout に出力

> **`filling` は内部一時状態**: `planning` と同様、fill 処理中のみ存在する。fill 成功時は `active` に遷移し、失敗時は `awaiting_fill` にロールバックされる。クラッシュ時は reconciler の R0b パターンで修復される。

### `plan complete` の処理フロー

`plan complete` は内部で `can-complete` 検証 + `result write planner` を一括実行する。

1. `can-complete` でコマンドの完了条件を検証
   - **フェーズチェック（追加）**: 全フェーズが terminal（`completed` / `failed` / `cancelled` / `timed_out`）であること。いずれかのフェーズが `pending` / `awaiting_fill` / `filling` / `active` の場合は NG（`filling` はクラッシュ後の一時状態。retryable エラーを返す）
2. 検証 OK → derived_status（`completed` / `failed` / `cancelled`）を自動導出
   - **timed_out フェーズが存在する場合**: derived_status は `failed`
3. `result write planner` の内部フロー（§5.7 reporter=planner 参照）を実行
   - `--status` フラグなし: ステータスは `can-complete` から自動導出されるため、手動指定 vs derived の不一致エラーが原理的に発生しない
4. 検証 NG → エラー終了。未完了 task 一覧（+ 未完了フェーズ一覧）をエラーメッセージに含める

### `plan add-retry-task` の処理フロー

`plan add-retry-task` は sealed 状態の plan で、失敗タスクを新しいリトライタスクに**置換**する。

1. **バリデーション**:
   - `plan_status` が `sealed` であることを確認（`planning` / terminal 状態では拒否）
   - キャンセル要求がないことを確認（`cancel.requested: true` なら拒否）
   - `--retry-of` で指定されたタスクが同一 command 内に存在し、`task_states` が `failed` であることを検証。`failed` 以外の状態では拒否
   - `--blocked-by`（任意）は task_id を直接指定（ローカル name 参照ではない。単一タスク追加のため name 解決不要）。省略時は元タスクの `blocked_by` を継承
   - `--blocked-by` の参照先が同一フェーズ内に存在することを検証（`phases: null` の場合は同一 command 内と等価）

2. **フェーズ所属の決定**（`phases` が存在する場合）:
   - `--retry-of` の task_id からフェーズを逆引きし、リトライタスクを同一フェーズに追加
   - フェーズが `active` または `failed`（リトライ回復中）であることを検証。それ以外の状態では拒否

3. **Worker 割当**: `plan submit` と同じアルゴリズム（bloom_level → model → pending 最小）

4. **書き込み**:
   - task_id を生成
   - `state/commands/{command_id}.yaml` を更新:
     - `required_task_ids`（または `optional_task_ids`）で `--retry-of` のタスク ID を新タスク ID に**置換**（`expected_task_count` は変更なし）
     - `retry_lineage[new_task_id] = replaced_task_id` を記録（監査履歴）
     - `task_dependencies` で旧タスク ID への依存を新タスク ID への依存に**付け替え**（推移的依存を維持）
     - `task_states[new_task_id] = "pending"` を追加（旧タスクのエントリは `failed` のまま保持し削除しない）
   - フェーズの `task_ids` に新タスク ID を追加（`phases` が存在する場合）。フェーズが `failed` の場合は `active` に再オープンし `reopened_at` を記録
   - `queue/worker{N}.yaml` にタスクエントリを追加

5. **推移的キャンセル自動復旧（cascade recovery）**:
   - `--retry-of` の元タスク（X）の失敗を直接原因として `cancelled` になったタスクを `cancelled_reasons` から検出（`cancelled_reasons[{task_id}]` が `"blocked_dependency_terminal:X"` に一致するエントリ）。`cancelled_reasons` に理由が存在しない、または `command_cancel_requested` 等の他の理由を持つタスクは対象外
   - 検出された各キャンセル済みタスクについて、自動的にリトライタスクを生成:
     - 元タスクと同一の `purpose`, `content`, `acceptance_criteria`, `constraints`, `bloom_level` を引き継ぐ（queue エントリから取得）
     - **`blocked_by` の解決**: 元タスクの `blocked_by` に含まれる各タスク ID を `retry_lineage` で最新の有効な子孫 ID に写像する。例: 元タスク B が `blocked_by: [A]` を持ち、A が A' に置換済み（`retry_lineage[A'] = A`）の場合、復旧タスク B' の `blocked_by` は `[A']` となる
     - `required_task_ids` で旧→新を置換、`retry_lineage` に記録、`task_dependencies` 付け替え
     - Worker 割当（bloom_level → model → pending 最小）
     - `queue/worker{N}.yaml` にエントリ追加
   - 再帰的に適用: キャンセル済みタスクの下流にさらにキャンセル済みタスクがあれば同様に復旧
   - 復旧後の依存グラフ全体の DAG 検証を実施
   - フェーズの `task_ids` 更新（該当する場合）。フェーズが `failed` の場合は `active` に再オープンし `reopened_at` を記録（`cancelled` / `completed` / `timed_out` フェーズの再オープンは禁止。[§4.10](04-yaml-schema.md) 参照）

6. **ロールバック（失敗時）**: ステップ 4-5 の全変更をロールバック

7. **出力**: リトライタスク + cascade recovery 結果を JSON で stdout に出力:
   ```json
   {
     "task_id": "task_...",
     "worker": "worker{N}",
     "model": "...",
     "replaced": "task_...",
     "cascade_recovered": [
       {
         "task_id": "task_...",
         "worker": "worker{M}",
         "model": "...",
         "replaced": "task_..."
       }
     ]
   }
   ```

> **置換方式の設計意図**: `required_task_ids` で旧→新を置換するため、`can-complete` の完了判定ロジック（「required task が全て completed → completed」）を変更する必要がない。旧タスクは `required_task_ids` から除外されるが、`task_states` と `retry_lineage` に履歴として残るため、監査・トレーサビリティは維持される。
>
> **推移的キャンセル自動復旧の設計意図**: 失敗タスクのリトライ時に、依存失敗でキャンセルされた下流タスクを**デーモンが自動的に復旧**する。これにより Planner は失敗タスク 1 件に対して `add-retry-task` を 1 回呼ぶだけで済み、依存グラフの修復ロジックを持つ必要がない。復旧対象は `reason: blocked_dependency_terminal:{task_id}` で追跡可能なタスクに限定され、Planner が明示的にキャンセルしたタスク（`plan complete` 経由）は対象外。cascade recovery で生成されたタスクも `retry_lineage` に記録される。
>
> **`allow_dynamic_tasks` の例外**: `add-retry-task` は `allow_dynamic_tasks: false` でも実行可能。これはリトライ用の限定的な例外であり、通常のタスク追加（`plan add-task` による動的追加）とは異なる。タスクの純増ではなく置換であるため、`expected_task_count` は変更されない。cascade recovery で復旧されるタスクも置換方式（旧→新）であり、`expected_task_count` は変更されない。

### `plan rebuild` サブコマンド（reconciliation 用）

- per-command mutex 取得後、`results/worker{N}.yaml` から該当 `command_id` の全エントリを収集
- `task_states` と `applied_result_ids` を再構築（completion policy フィールドは変更しない）
- `last_reconciled_at` を更新
- 冪等: 何度実行しても同じ結果になる

### `plan request-cancel` サブコマンド（canonical なキャンセル要求経路）

> **canonical な経路は本サブコマンド**。`maestro plan request-cancel --command-id <id>` がコマンドキャンセル要求の正式な CLI エントリポイントである。`maestro queue write planner --type cancel-request`（§5.6 参照）は deprecated であり、CLI 実行時に本サブコマンドへの移行を促す WARNING を出す。両経路はデーモン内部で同一の cancel-request ハンドラに収束するため振る舞いは等価だが、新規の呼び出しは `plan request-cancel` を使う。

```bash
# コマンドキャンセル要求（単調: 一度要求したら取り消し不可）
maestro plan request-cancel \
  --command-id cmd_1771722000_a3f2b7c1 \
  --requested-by orchestrator \
  --reason "ユーザーによるキャンセル"
```

### worktree 回復サブコマンド群（インフラ / オペレーター用）

worktree モードの統合パイプライン（merge / publish）が停滞した場合の回復経路として、`plan` は以下のサブコマンドを提供する。いずれも reconciler の R7（`merge_conflict`）/ R8（`publish_failed`）と連動し、停滞した integration を再進行させる。

| コマンド                | 内部 operation     | 想定呼び出し元         | 責務                                                                                                                                                                                                                                                                               |
| ----------------------- | ------------------ | ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `plan recover`          | `auto_recover`     | Planner / オペレーター | command の現在の worktree 統合状態を判定し、適切な回復アクション（resume-merge / retry-publish 等）をデーモンが自動選択する                                                                                                                                                        |
| `plan resume-merge`     | `resume_merge`     | Planner / オペレーター | merge 失敗カウンタをリセットし、停滞した integration（`conflict` / `partial_merge` / `failed`）を再マージ可能状態へ戻す。Worker が conflict を解決した後にマージを replay する                                                                                                     |
| `plan retry-publish`    | `retry_publish`    | Planner / オペレーター | publish 失敗状態をリセットし、cooldown 経過後または Worker が `publish_conflict` を解決した後に publish-to-base を再試行する                                                                                                                                                       |
| `plan resolve-conflict` | `resolve_conflict` | Planner / オペレーター | publish をブロックする commit 失敗（`commit_failed_workers`）から指定 Worker を解除する。phase の `merge_conflict` 解決用**ではない**（その用途では Planner は `plan add-task --worker-id <worker>` で解決タスクを投入し、デーモンが解決報告後に自動で `resume_merge` を発火する） |
| `plan unquarantine`     | `unquarantine`     | **オペレーター専用**   | quarantine された command の integration ブランチ状態をクリアし、次の queue スキャンで再マージ試行を再キューする                                                                                                                                                                   |

> **`unquarantine` はオペレーター専用**: quarantine は「merge 連続失敗」や「publish_failed が retry budget 超過」に対するデーモン最後の砦であり、到達は自動リトライループが Agent でも解決できなかった失敗モードに収束したことを意味する。そのため CLI 層で Planner role を hard-reject する（`cmd_plan_ops.go` の caller-role ガード。デーモン側 `plan_handler` でも同一の信頼境界を二重に強制する）。回復可否はオペレーターが根本原因を確認した上で判断する。

### 厳密ルール

1. `plan_status: sealed` 以前は command 完了宣言（`plan complete`）を禁止
2. `required_task_ids + optional_task_ids` の件数が `expected_task_count` と一致しない限り完了宣言を禁止
3. `required_task_ids` の全 task が terminal 状態（`completed` / `failed` / `cancelled`）でない限り完了宣言を禁止
4. `allow_dynamic_tasks: false` の場合、seal 後の通常タスク追加を禁止（`add-retry-task` は例外。置換方式のため `expected_task_count` は変更されない）
5. `results/worker{N}.yaml` に未知 `task_id` が来た場合は永続化前に拒否（state/queue/results 無変更）。`maestro result write` の呼び出し元に同期エラー（queue 不在は `not_found`、state 未登録は `validation`）を返し、エラーログへ記録する。Planner への別途（非同期）の異常通知は行わない（[§7.8](07-error-handling.md) 参照）。ただし `retry_lineage` で置換済みの旧タスク ID に対する遅着結果は、ログに記録するが state には反映しない
   5a. `plan submit` / `plan submit --phase` で、タスクの `name` が `__` プレフィックスを持つ場合はシステム予約名として拒否（`__system_commit` 等のデーモン自動挿入タスクとの衝突を防止）。`plan add-retry-task` は `name` フィールドを持たないため本ルールの対象外
6. 完了判定は queue/results の推測ではなく state 正本でのみ行う
7. **`plan submit` 時に `task_dependencies` の DAG 検証**（トポロジカルソート）を実行。循環依存を検出した場合は submit 失敗。循環パスをエラーメッセージに含める。**フェーズ付き submit ではフェーズレベル DAG 検証も実行**
8. `blocked_by` / `task_dependencies` は同一フェーズ内のタスク ID のみ参照可能。異なるフェーズや異なる command のタスクを参照した場合は拒否。**フェーズ間依存は `depends_on_phases` で表現**（concrete フェーズの `depends_on_phases` は空配列のみ許可）。ただし `system_commit_task_id` が示すシステムコミットタスクはフェーズ構造の外に存在するため、同一フェーズ制約の対象外
9. **キャンセル要求は単調（monotonic）**: `cancel.requested: true` は取り消し不可。キャンセル後の新規タスク追加（`add-retry-task` 含む）・配信は禁止
10. **完了ステータス決定ルール**（`can-complete` / `plan complete` の内部ロジック）:
    - required task に `failed` が含まれる → command ステータスは `failed`
    - required task に `cancelled` が含まれる → command ステータスは `cancelled`
    - required task が全て `completed` → command ステータスは `completed`
    - `plan complete` は `can-complete` が返した derived ステータスを自動使用する。手動指定不可のため不一致は原理的に発生しない
11. **フェーズ構造は初回 submit で確定**。後からのフェーズ追加・削除・制約変更は禁止（Contract の不変性）
12. **deferred フェーズの fill は 1 回限り**。同一フェーズへの二重 fill は拒否（seal と同じ思想。`awaiting_fill` → `filling` → `active` の遷移は 1 回のみ）

**依存関係失敗伝搬**: [§7.11](07-error-handling.md) 参照。

## 5.8 maestro daemon

**責務**: 常駐プロセスとして `queue/` と `results/` の統合監視 → タスク・コマンドの配信 + 結果通知 + reconciliation + メトリクス

```
監視対象: .maestro/queue/*.yaml, .maestro/results/*.yaml
```

> **設計**: 単一プロセスが queue_handler と result_handler を内包。Go の `fsnotify` で両ディレクトリを監視し、変更されたパスに応じてハンドラを振り分ける。一方のエラーが他方をブロックしない。

### 5.8.1 Queue ハンドラ

**処理フロー**:

1. `fsnotify` で `queue/` を監視（debounce 付き）
2. 変更イベント検知時:
   a. 変更されたファイルを特定（`queue/planner.yaml`, `queue/worker{N}.yaml`, `queue/orchestrator.yaml`）
   b. per-agent mutex を取得
   c. **at-most-one-in-flight チェック**: 同一 agent queue 内に `status: in_progress` かつ `lease_expires_at >= now`（有効な lease）のエントリが存在する場合 → mutex 解放 → 何もしない（既にタスク実行中）
   d. 期限切れ lease の回収: `status: in_progress` かつ `lease_expires_at < now` のエントリがある場合 → lease 期限切れ回収パス（ステップ 2 参照）を先に実行し、同一パスでは新規 `pending` を配信しない
   e. ファイルを読み取り、`status: pending` または `status: in_progress` のエントリに対して配信可否を判定（依存タスクの状態は `state/commands/{command_id}.yaml` の `task_states` を正本として参照する）:
   - **キャンセル済みコマンドチェック**（タスクエントリの場合）: `state/commands/{command_id}.yaml` の `cancel.requested: true` を確認。`true` の場合:
     - `status: pending` のエントリ → `cancelled` に遷移。`state/commands/` の `task_states` も `cancelled` に更新。`cancelled_reasons[{task_id}] = "command_cancel_requested"` を記録。**配信しない**
     - `status: in_progress` のエントリ → `agent_executor --interrupt` で中断 → `cancelled` へ遷移し、lease 解放。`state/commands/` の `task_states` も `cancelled` に更新。`cancelled_reasons[{task_id}] = "command_cancel_requested"` を記録。合成的な `cancelled` 結果エントリを `results/worker{N}.yaml` に作成（ステップ 0.6 と同様）
   - **システムコミットタスク判定**（`phases` を持つ command の場合のみ）: エントリの `task_id` が `state/commands/{command_id}.yaml` の `system_commit_task_id` と一致する場合、`blocked_by` ではなく「全ユーザーフェーズが terminal（`completed` / `failed` / `cancelled` / `timed_out`）」を配信条件とする。条件を満たせば **配信可能**、満たさなければ **スキップ**
   - **通常の `blocked_by` 判定**: `blocked_by` が空、または `blocked_by` 内の全タスクが `completed`（`task_states` 上） → **配信可能**（`pending` の場合のみ。`in_progress` はそのまま維持）
   - `blocked_by` 内のいずれかのタスクが `failed` / `cancelled` / `dead_letter`（`task_states` 上では `failed` / `cancelled`） → 依存失敗処理:
     - `status: pending` の場合: 当該タスクを `cancelled`（reason: `blocked_dependency_terminal:{task_id}`）へ遷移し、`state/commands/` の `task_states` も `cancelled` に更新。`cancelled_reasons[{task_id}] = "blocked_dependency_terminal:{causing_task_id}"` を記録
     - `status: in_progress` の場合: `agent_executor --interrupt` で実行中のタスクを中断 → `cancelled` へ遷移し、lease 解放、`state/commands/` の `task_states` も `cancelled` に更新。`cancelled_reasons[{task_id}] = "blocked_dependency_terminal:{causing_task_id}"` を記録
   - それ以外（依存先が `pending` / `in_progress` を含む）→ **依存待ちでスキップ**
     f. 配信可能エントリが存在しない場合 → mutex 解放 → 何もしない
     g. 配信可能エントリが複数存在する場合は、定期スキャンと同一の優先度順で 1 件を選択: `effective_priority ASC` → `created_at ASC` → `id ASC`（安定ソート）。`priority` フィールドが未設定の場合はデフォルト値 100 を使用
     h. 配信対象エントリに対して:
   - **mutex 保持中に** `status: in_progress` を設定
   - `attempts += 1`
   - `lease_epoch += 1`（単調増加フェンシングトークン）
   - `lease_owner = "daemon:{pid}"`, `lease_expires_at = now + dispatch_lease_sec`
   - `updated_at` を現在時刻に設定
   - mutex 解放
   - agent_executor モジュールで配信（ビジー判定付き）。**Worker への配信は常に `--with-clear` を使用する**（前タスクのコンテキストが残っている可能性があるため、タスク実行前にコンテキストをリセットする。Planner・Orchestrator への配信には `--with-clear` を使用しない）。配信メッセージには `lease_epoch` をメタデータとして含める（Worker が結果報告時に `--lease-epoch` で返却するために必要）
   - 配信失敗（ビジー判定タイムアウト）の場合:
     - per-agent mutex を再取得
     - ステータスを `pending` に戻す
     - `lease_owner`, `lease_expires_at` を `null` に戻す
     - mutex 解放

> **同時二重配信防止**: lease 取得を配信の**前**に mutex 内で実行するため、
> 定期スキャンと fsnotify イベントが同時に走っても同時配信は発生しない。
> ただし配信直後クラッシュ時の再送はありうるため、全体保証は at-least-once（冪等前提）とする。
>
> **at-most-one-in-flight 不変条件**: 同一 agent queue に対して、有効な lease を持つ `in_progress` エントリは最大 1 つ。
> バックログ（`pending` の蓄積）は許可されるが、配信は 1 つずつ。
> Planner への result_handler からのサイドチャネル通知（タスク完了通知）はこの不変条件の対象外（queue 配信ではないため）。

**定期スキャン**（`scan_interval_sec` ごと）:

以下の概念的ステップ（0 → 0.5 → 0.6 → 0.7 → 1 → 1.5 → 2 → 3）を順に実行する。

> **ロック分割の不変条件（3 フェーズ構造）**: 以下の概念的ステップはすべて存在するが、実行時には slow な tmux I/O 中に `scanMu`（PeriodicScan の排他ロック）を握り続けないよう、`ScanPhaseExecutor`（`internal/daemon/scan_phase_executor.go`）が **Phase A → B → C** の 3 フェーズで実行する:
>
> - **Phase A（`scanMu.Lock` 保持）**: queue のロード + 高速な状態変更（dead-letter 化・キャンセル判定・フェーズ遷移判定など）を行い、Phase B で行う slow work（interrupt / busy probe / dispatch / signal / merge / publish）を収集して flush する
> - **Phase B（ロック解放）**: 収集した tmux I/O（中断・ビジー判定・配信・通知・merge / publish）を `scanMu` を握らずに実行する。これにより slow I/O 中も queue write / plan complete / verify write 等の UDS ハンドラが `scanMu.RLock` を取得できる
> - **Phase C（`scanMu.Lock` 再取得）**: queue を再ロードし、Phase B の結果を epoch フェンシング付きで適用 → flush → reconciliation を行う
>
> スキャンサイクル全体は `scanRunMu` で直列化され、Phase B がロックを解放しても次のスキャンが重複起動しない。

**ステップ 0: リトライ上限チェック + dead-letter 化**

- 全 agent queue ファイルをスキャン
- `status: pending` かつ `attempts >= max_attempts`（config.yaml の `retry.*` 参照）のエントリを検出
  → `status: dead_letter`, `dead_lettered_at = now`, `dead_letter_reason` に理由を記録
  → `.maestro/dead_letters/` にエントリをアーカイブ
  → **queue 型別の後処理**:
  - **`queue/worker{N}.yaml`（タスク）**: 対応する `state/commands/` の `task_states` を `failed` に更新（合成的 terminal result）。Planner に dead-letter 通知を送信
  - **`queue/planner.yaml`（コマンド）**: `state/commands/{command_id}.yaml` が存在する場合は `plan_status` を `failed` に更新。存在しない場合（未 submit）は state 更新をスキップ。Orchestrator にコマンド配信失敗を通知（`maestro queue write orchestrator --type notification --notification-type command_failed`）
  - **`queue/orchestrator.yaml`（通知）**: state 更新不要（元の result は既に terminal）。デーモンアラートメトリクスに記録（ユーザーへの通知がロストした可能性）

**ステップ 0.5: キャンセル済みコマンドのタスク処理**（定期スキャンの安全ネット。fsnotify パスのステップ 2e が一次ゲートとして同じチェックを実行するため、通常は fsnotify パスで先にキャンセル処理される）

- `state/commands/{command_id}.yaml` の `cancel.requested: true` を確認
- 該当コマンドの `pending` タスクは配信せず `cancelled` に更新。`cancelled_reasons[{task_id}] = "command_cancel_requested"` を記録

**ステップ 0.6: キャンセル済みコマンドの in_progress タスク中断**

- 対象: `cancel.requested: true` かつ `status: in_progress` のタスク
- per-agent mutex 取得後に `agent_executor --interrupt`（`C-c` 送信後 `/clear`）を実行
- 成功時:
  - queue エントリを `cancelled` に更新、lease 解放
  - `state/commands/{command_id}.yaml` の `task_states[{task_id}]` を `cancelled` に更新
  - **合成的な cancelled 結果エントリ**を `results/worker{N}.yaml` に作成（`status: cancelled`, `summary: "command_cancel_requested"`, `partial_changes_possible: true`, `retry_safe: false`）。これにより既存の Result ハンドラ通知パイプラインで Planner にキャンセルが伝達される
- 失敗時: `cancel_interrupt_failed` をログに記録し、次回定期スキャンで再試行（lease 期限切れ後にステップ 2 でも回収される）

**ステップ 0.7: フェーズ遷移チェック**

`phases` を持つ command に対して以下を実行:

1. **`active` フェーズの完了判定**: フェーズ内の全 required タスクが terminal（`completed` / `failed` / `cancelled`）
   - 全 required タスクが `completed` → フェーズを `completed` に遷移（`completed_at` を設定）
   - いずれかの required タスクが `failed` / `cancelled` → フェーズを `failed` / `cancelled` に遷移

2. **`pending` フェーズの活性化判定**: 依存フェーズ（`depends_on_phases`）が全て `completed`
   → フェーズを `awaiting_fill` に遷移 + `fill_deadline_at = now + constraints.timeout_minutes` を設定 + Planner に通知

3. **`pending` フェーズのカスケードキャンセル**: 依存フェーズにいずれか `failed` / `cancelled` / `timed_out` が存在
   → フェーズを `cancelled` に遷移（reason: `upstream_phase_failed:{phase_name}`）

4. **`awaiting_fill` フェーズのタイムアウト**: `fill_deadline_at < now`
   → フェーズを `timed_out` に遷移。下流の `pending` フェーズを `cancelled` にカスケード

**Planner への通知メッセージ**（ステップ 0.7 で `awaiting_fill` 遷移時）:

```
phase:{name} phase_id:{phase_id} status:awaiting_fill command_id:{id} — plan submit --phase {name} で次フェーズのタスクを投入してください
```

**ステップ 1: 未配信エントリの再試行**

- `status: pending` のエントリ → 配信を試行（上記と同じフロー）
- 配信順序: `effective_priority ASC`（小さいほど高優先）、同優先度は `created_at ASC`（FIFO）
  - `effective_priority = max(0, priority - floor((now - created_at) / priority_aging_sec))`

**ステップ 1.5: in_progress タスクの依存失敗チェック**（fsnotify パスの安全ネット）

- `status: in_progress` かつ有効な lease を持つタスクエントリを対象
- `state/commands/{command_id}.yaml` の `task_states` を参照し、`blocked_by` 内に terminal 失敗（`failed` / `cancelled`）が含まれるか検査
- 該当あり → per-agent mutex 取得後に `agent_executor --interrupt` で中断 → `cancelled` に遷移し lease 解放。`state/commands/` の `task_states` も `cancelled` に更新。`cancelled_reasons[{task_id}] = "blocked_dependency_terminal:{causing_task_id}"` を記録
- キャンセル済みコマンド（`cancel.requested: true`）の `in_progress` タスクも同様に中断（ステップ 0.6 と同じ処理）

> **設計意図**: fsnotify パス（ステップ 2e）が一次ゲートとして依存失敗の in_progress タスクを検知・中断するが、fsnotify イベントの取りこぼし時にはステップ 2 の lease 期限切れ回収（最大 `dispatch_lease_sec` 秒後）まで検知が遅延する。ステップ 1.5 により `scan_interval_sec` ごとの検知が保証され、不要な Worker 実行時間を短縮する。

**ステップ 2: lease 期限切れ回収（ビジー検知併用）**

- `status: in_progress` かつ `lease_expires_at < now` のエントリを検出
- 該当 Agent ペインを agent_executor の `is_busy()` で確認:
  - **Agent が busy** かつ `updated_at` からの経過が `max_in_progress_min` 未満（`<`）:
    - 正常実行中とみなし `lease_expires_at = now + dispatch_lease_sec` に延長（heartbeat）
  - **Agent がアイドル** または `max_in_progress_min` 以上（`>=`、到達時点でタイムアウト）:
    1. ステータスを `pending` に戻す
    2. `lease_owner`, `lease_expires_at` を `null` に戻す
    3. 次のスキャンで再配信

    エージェントへの `/clear` 自動送信はこの経路では行わない（[§7.3](07-error-handling.md) と同じ lease-release-to-pending のみ。ハングセッションの実体復旧は blocked-pane recovery が担う）

**ステップ 3: 整合性修復（reconciliation）**

以下の全不整合パターンを検出・修復する:

| #           | 不整合パターン                                                                               | 原因                                     | 修復アクション                                                                                                                                                                                                |
| ----------- | -------------------------------------------------------------------------------------------- | ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R0          | `state/commands/{command_id}.yaml` の `plan_status` が `planning` のまま持続                 | `plan submit` 処理中のクラッシュ         | state ファイルを削除し、追加済みの queue エントリも除去（submit のロールバックを完了）。Planner に再 submit を通知                                                                                            |
| R0b         | フェーズの `status: filling` が持続                                                          | `plan submit --phase` 処理中のクラッシュ | フェーズを `awaiting_fill` に戻し、部分追加された task_ids / queue エントリを除去。Planner に再 fill を通知                                                                                                   |
| R1          | `results/{agent}.yaml` に terminal あり + `queue/{agent}.yaml` が `in_progress`              | result write フェーズ A クラッシュ       | `queue/` を terminal に修正, lease 解放                                                                                                                                                                       |
| R2          | `results/worker{N}.yaml` に terminal あり + `state/commands/` の `task_states` が非 terminal | result write フェーズ B クラッシュ       | per-command mutex 取得 → `task_states` と `applied_result_ids` を修復                                                                                                                                         |
| R3          | `results/planner.yaml` に terminal あり + `queue/planner.yaml` が非 terminal                 | result write フェーズ A クラッシュ       | `queue/planner.yaml` を terminal に修正                                                                                                                                                                       |
| R4          | `results/planner.yaml` に terminal あり + `state/commands/` の `plan_status` が非 terminal   | result write フェーズ B クラッシュ       | per-command mutex 取得 → `maestro plan can-complete` を再評価。検証 OK の場合のみ `plan_status` を修復。検証 NG の場合は R4 を適用しない（results/ のエントリを quarantine に移動し、planner に再評価を通知） |
| R5          | `results/planner.yaml` に terminal あり + `queue/orchestrator.yaml` に対応通知なし           | result_handler クラッシュ                | `maestro queue write orchestrator` で通知再発行（`source_result_id` で冪等）                                                                                                                                  |
| R6          | フェーズの `status: awaiting_fill` かつ `fill_deadline_at < now`                             | Planner が deferred フェーズを未投入     | フェーズを `timed_out` に遷移。下流 `pending` フェーズを `cancelled`。Planner に通知                                                                                                                          |
| R7          | Worker が merge conflict 状態で停滞                                                          | integration マージ時の衝突               | conflict 解決タスクの起動、または Planner へエスカレーション                                                                                                                                                  |
| R8          | publish 失敗で command が quarantine                                                         | integration ブランチへの publish 失敗    | `publish_quarantined` を failed 終端化し Planner / Orchestrator へエスカレーション                                                                                                                            |
| R9          | タスクが `verify_pending` で停滞                                                             | verify パイプラインの stall              | 閾値超過で `repair_pending` へ強制遷移                                                                                                                                                                        |
| R10         | タスクが `paused_for_replan` で停滞                                                          | Planner 差し戻し後の放置                 | デッドライン超過（既定 1 時間）で `failed` へ終端化                                                                                                                                                           |
| R0-dispatch | 配信可能だが未配信のエントリが残存                                                           | fsnotify 取りこぼし                      | 配信を再起動（安全ネット）                                                                                                                                                                                    |

> **R0-dispatch の表記**: `metrics.yaml` / `dashboard.md` 等に出力される `RepairPatternID` 文字列値は `"R0-dispatch"`（`reconcile/types.go` の `PatternR0Dispatch`）。対応する Go の識別子（struct 名）は `R0Dispatch`（`reconcile/r0_dispatch.go`）。本書および他章では出力値に合わせ `R0-dispatch` で統一表記する。
>
> **実装の正規形**: reconciliation エンジン（`internal/daemon/reconcile/`）は上記を含む R0 / R0-dispatch / R0b / R1〜R10 を登録する（`reconcile/engine.go` の `NewEngine`。デーモンへの配線は `internal/daemon/reconciler.go`）。R7〜R10 は worktree 統合・verify パイプライン・Planner 差し戻しに対応する追加パターンであり、要件初版の 8 パターンから拡張されている。

各修復後に `state/commands/{command_id}.yaml` の `last_reconciled_at` を更新する。

**ダッシュボード・メトリクス更新**:

- 定期スキャン時に dashboard_gen モジュールで `dashboard.md` を更新
- 定期スキャン時に `state/metrics.yaml` を更新（queue depth, カウンター, daemon_heartbeat）
- Agent は dashboard.md / metrics.yaml を書き込まない（読み取りのみ許可）
- メトリクス更新はベストエフォート（失敗してもコア処理をブロックしない）

**アラート条件**（`daemon.log` への WARN/ERROR 記録 + dashboard.md / metrics.yaml へ反映。macOS 通知は行わない）:

| 条件                                                     | 記録内容                    |
| -------------------------------------------------------- | --------------------------- |
| dead-letter 発生                                         | エントリ ID + 理由          |
| daemon heartbeat が `scan_interval_sec * 3` 以上更新なし | デーモン停止の可能性        |
| YAML 破損検出                                            | ファイル名 + quarantine 先  |
| reconciliation 修復実行                                  | 修復パターン + 対象エントリ |

**プロセス管理**:

- **単一インスタンス保証**: 起動時に `.maestro/locks/daemon.lock` をファイルロック（`syscall.Flock` LOCK_EX|LOCK_NB）で取得。取得失敗時は「既に稼働中」としてエラー終了。ロックはプロセスライフタイム全体で保持
- `SIGTERM` / `SIGINT` で graceful shutdown（§5.4 のシーケンスを `sync.Once` で実行。draining 状態遷移 → 全プロデューサー停止 → in-flight ドレイン → タイムアウト 90 秒 → クリーンアップ → 終了。2 回目のシグナルで即座終了）
- ログを `.maestro/logs/daemon.log` に出力
- Queue ハンドラと Result ハンドラのエラーは独立してキャッチし、一方のエラーが他方をブロックしない
- クラッシュ時は `maestro up` が再起動を担当（while-true ラッパーではなく、明示的な再起動）

### 5.8.2 Result ハンドラ

**責務**: `results/` の変更検知 → 完了通知の配信

**処理フロー（notification lease パターン）**:

1. デーモンの fsnotify が `results/` の変更を検知
2. 変更イベント検知時:
   a. 変更されたファイルを特定
   b. **per-agent mutex を取得**
   c. 通知対象エントリを検索: `notified: false` かつ（`notify_lease_owner: null` または `notify_lease_expires_at < now`）
   d. 対象エントリが存在しない場合 → mutex 解放 → 何もしない
   e. 対象エントリが存在する場合:
   - **mutex 保持中に** notification lease を取得:
     - `notify_lease_owner = "daemon:{pid}"`
     - `notify_lease_expires_at = now + notify_lease_sec`
     - `notify_attempts += 1`
   - mutex 解放
   - 通知処理を実行（下記参照）
   - **成功時**: per-agent mutex 再取得 → `notified: true`, `notified_at = now`, lease フィールドクリア → mutex 解放
   - **失敗時**: per-agent mutex 再取得 → `notify_last_error` に理由を記録, lease フィールドクリア → mutex 解放（定期スキャンで再試行）

> **通知ロスト防止**: `notified: true` を side effect 成功後にのみマークするため、クラッシュ時も通知がロストしない。
> notification lease により、定期スキャンと fsnotify イベントの同時実行でも同じ結果を 2 回通知しない。
> lease 期限切れの通知は定期スキャンで回収・再試行される。

> **サイドチャネル通知のメッセージ競合防止**: Worker 結果の Planner への通知は `agent_executor` 経由の `tmux send-keys`（サイドチャネル）で行う。
> notification lease が同時に 1 つしか取得できないため、複数の Worker 結果が同時に到着しても Planner への通知は直列化される。
> ただし、前回の通知送信成功後に Planner がまだ処理を開始していない（アイドルに見える）タイミングで次の通知が来る場合がある。
> この場合 `agent_executor` のビジー判定（`idle_stable_sec` による安定確認）がガードとなり、Planner の入力バッファにメッセージが連結されるリスクを低減する。
> 万一連結が発生しても、各通知メッセージは `[maestro]` ヘッダ付きの自己完結的な形式であり、Planner は個別に解釈できる。

**通知処理**:

**Worker の結果 (`results/worker{N}.yaml`) の場合**:

- agent_executor で Planner に通知（サイドチャネル）。以下の固定フォーマットを使用:
  `"[maestro] kind:task_result command_id:{command_id} task_id:{task_id} worker_id:{worker_id} status:{completed|failed}\nresults/{worker_id}.yaml を確認してください"`

**Planner の結果 (`results/planner.yaml`) の場合**:

- `maestro queue write orchestrator` で `queue/orchestrator.yaml` に通知を追加
  （→ queue_handler が Orchestrator への配信を担当 → Orchestrator が tmux ペイン上でユーザーに報告）

> **macOS 通知は提供しない**: 旧仕様の `maestro notify`（osascript による通知センター連携）は実装されていない。ユーザーへの完了・停止の伝達は Orchestrator ペイン経由で行う。`up` は既定で tmux セッションへ attach するため、ユーザーは Orchestrator の出力を直接確認できる。

**定期スキャン**:

`scan_interval_sec` ごとに `results/` 全ファイルをスキャンし、`notified: false` のエントリを再処理（上記と同じ mutex 付きフロー）。

## 5.9 maestro agent exec（デーモン内部モジュール）

**責務**: エージェントへのメッセージ配信（ビジー判定 + `/clear` 制御を含む）

```
Usage（CLI 経由、主にデバッグ用。引数は named-flag）:
  maestro agent exec --agent-id <id> --message <msg>
  maestro agent exec --agent-id <id> --message <msg> --with-clear
  maestro agent exec --agent-id <id> --clear
  maestro agent exec --agent-id <id> --interrupt  # C-c 送信後 /clear（キャンセル時のタスク中断用）
  maestro agent exec --agent-id <id> --is-busy     # ビジー判定のみ。busy/idle を stdout に出力し、idle 時は exit code 1 を返す
```

> **caller role ガード（agent impersonation 防止）**: `maestro agent exec` は tmux ペインに直接メッセージを貼り付け、デーモンの UDS dispatch 経路（lease チェック・フェンシング・dispatch_id dedupe）を完全にバイパスする。オペレーター端末からは意図的なデバッグ手段だが、Agent から呼ぶと「Agent 間通信はデーモン経由のみ」という不変条件を破り、ある Agent が他の Agent になりすませてしまう。このため `guardAgentExecCaller`（`cmd_agent.go`）が CLI（operator）role 以外の呼び出し元を hard-reject する（`cmd_plan_ops.go` の `unquarantine` ガードと同じ多層防御の一翼。Worker は L1 `workerDisallowedTools` / L2 `worker_policy_hook.sh` でも別途ブロックされる）。

**処理フロー**:

1. `--agent-id` から tmux ペインターゲットを特定（`@agent_id` 変数で検索）
2. **ビジー判定**: 以下の複合条件でアイドル / ビジー / 不確定を判定:
   a. ペインの実行中プロセスを確認（`pane_current_command` で CLI が動作中か検証）
   b. ペイン最終 3 行を `tmux capture-pane` で取得
   c. `busy_patterns` 正規表現とマッチング → マッチすれば**ビジーヒント**（確定ではない）
   d. アクティビティプローブ: `idle_stable_sec` 秒後に再度キャプチャし、内容のハッシュを比較
   - ハッシュ変化あり → ビジー（確定）
   - ハッシュ変化なし + `busy_patterns` 非マッチ → アイドル（確定）
   - ハッシュ変化なし + `busy_patterns` マッチ → **不確定**（呼び出し元に retryable failure を返す）
   - ビジーの場合: `busy_check_interval` 秒待機してリトライ（最大 `busy_check_max_retries` 回）
   - 全リトライ失敗または不確定: エラーを返す（呼び出し元がロールバック・再試行を判断）

> **設計原則**: `busy_patterns` 正規表現は**ヒント**であり、ハード制御フロー（lease 延長、`/clear`、再配信）の唯一の根拠にしない。
> 不確定な状態ではメッセージロストよりも defer/retry を選択する。

3. **`--interrupt` 指定時**（キャンセルによるタスク中断）:
   a. `tmux send-keys -t {pane_target} "" C-c` で実行中の処理を中断
   b. `cooldown_after_clear` 秒待機
   c. `/clear` を `tmux send-keys` で送信（コンテキストをリセット）
   d. `cooldown_after_clear` 秒待機
   e. ペイン内容の安定を確認して終了
4. **`--with-clear` または `--clear` 指定時**:
   a. `/clear` を `tmux send-keys` で送信
   b. `cooldown_after_clear` 秒待機（コンテキスト再構築の時間確保）
   c. ペイン内容の安定を確認してから次へ
   d. `--clear` のみの場合はここで終了
5. **メッセージ送信**:
   a. `tmux send-keys -t {pane_target} "" C-c` でクリーンアップ（既存入力をキャンセル）
   b. `tmux send-keys -t {pane_target} "{message}" Enter` でメッセージ送信
6. tmux ユーザー変数 `@status` を `busy` に更新（`@status` ライフサイクルは後述）
7. 成功を返す

**Worker 向け配信エンベロープ**:

Queue ハンドラが Worker にタスクを配信する際、以下の固定フォーマットでメッセージを構築する。Worker はこのエンベロープに従ってタスクを実行し、末尾のコマンドテンプレートで結果を報告する。

```
[maestro] task_id:{task_id} command_id:{command_id} lease_epoch:{N} attempt:{N}

agent_id: {worker_id}
purpose: {purpose}
content: {content}
acceptance_criteria: {acceptance_criteria}
constraints: {constraints（カンマ区切り。空の場合は "なし"）}
tools_hint: {tools_hint（カンマ区切り。空の場合は "なし"）}
persona_hint: {persona_hint（空の場合は "なし"）}
skill_refs: {skill_refs（カンマ区切り。空の場合は "なし"）}

完了時: maestro result write {worker_id} --task-id {task_id} --command-id {command_id} --lease-epoch {N} --status <completed|failed> --summary "..."
失敗時に部分変更あり: 上記に加えて --partial-changes --no-retry-safe
```

> Worker は content に従ってタスクを実行し、結果をテンプレートの `--status` と `--summary` を埋めて報告するだけでよい。`lease_epoch` はテンプレートにプリフィルされているため、Worker が値を記憶・管理する必要はない。

**Planner 向けコマンド配信エンベロープ**:

Queue ハンドラが Planner にコマンドを配信する際、以下の固定フォーマットでメッセージを構築する。Planner はコマンドの `content` を分析してタスクに分解し、末尾のコマンドテンプレートで計画提出・完了報告を行う。

```
[maestro] command_id:{command_id} lease_epoch:{N} attempt:{N}

content: {content}

タスク分解後: maestro plan submit --command-id {command_id} --tasks-file -
全タスク完了後: maestro plan complete --command-id {command_id} --summary "..."
```

> Planner の terminal アクションは `plan submit`（タスク分解結果の提出）と `plan complete`（コマンド完了報告）の 2 つのみ。`plan complete` のステータスは `can-complete` からデーモンが自動導出するため、Planner は `--status` を指定しない。`command_id` はテンプレートにプリフィルされているため、Planner が値を記憶・管理する必要はない。コマンドには Worker タスクのような `purpose` / `acceptance_criteria` / `constraints` フィールドは存在しないため、エンベロープにも含めない。

**Orchestrator 向け通知配信エンベロープ**:

Queue ハンドラが Orchestrator に通知を配信する際、以下の固定フォーマットでメッセージを構築する。Orchestrator は通知を受け取り、結果ファイルを確認するだけでよい。

```
[maestro] kind:command_completed command_id:{command_id} status:{completed|failed}
results/planner.yaml を確認してください
```

> Orchestrator への通知は `queue/orchestrator.yaml` 経由で配信される。Orchestrator は通知内容に基づいて結果を確認し、ユーザーに報告する。配信エンベロープは `[maestro]` ヘッダで他の Agent 向けメッセージと同じ文法を共有する。

**`@status` tmux ユーザー変数のライフサイクル**:

`@status` はデーモンが管理する Agent 状態のキャッシュであり、以下の全経路で更新する:

| イベント                                         | 更新元                               | `@status` |
| ------------------------------------------------ | ------------------------------------ | --------- |
| lease 取得・配信開始（agent_executor 成功）      | agent_executor                       | `busy`    |
| result write 成功（queue エントリ terminal 化）  | queue_handler（result write 後処理） | `idle`    |
| 配信失敗ロールバック（`in_progress → pending`）  | queue_handler                        | `idle`    |
| キャンセル interrupt 完了                        | queue_handler（ステップ 0.6）        | `idle`    |
| lease 期限切れ回収（`/clear` 後 `pending` 戻し） | queue_handler（ステップ 2）          | `idle`    |
| dead-letter 化                                   | queue_handler（ステップ 0）          | `idle`    |

> reconciler の定期スキャン時に、queue 内に有効な lease を持つ `in_progress` がない Agent の `@status` が `busy` のままである場合、`idle` に修復する（最終保証）。

**Orchestrator ペイン保護**:

- `agent_id` が `orchestrator` の場合、ビジー判定を**厳格化**:
  - 通常のビジー判定に加え、ペイン内容の安定確認を必須とする
  - ビジーの場合はリトライせず即座にエラーを返す
  - → ユーザーの入力を絶対に妨害しない
  - 呼び出し元（queue_handler）は `pending` のまま維持 → 定期スキャンで再試行
  - ユーザーは attach 中の Orchestrator ペインで、idle 復帰後に配信される通知から完了を認識できる

## 5.10 maestro worker standby（デバッグ・可観測性用）

**責務**: Worker の状態表示（デバッグ・可観測性用 CLI）

> **注**: 本コマンドは Worker 割当には使用されない。Worker 割当は `plan submit` / `plan add-retry-task` 内部でデーモンが独自のアルゴリズム（bloom_level → model マッチ → pending タスク数最小）で自動実行する。割当アルゴリズムは全 Worker を対象にし、backpressure 上限に達していない限り常に Worker を選択する。本コマンドはオペレーターがフォーメーション状態を確認するためのデバッグ用途。

```
Usage: maestro worker standby [--model sonnet|opus]
出力: JSON（worker_id, model, pending_count, in_progress_count, status）
```

1. `queue/worker{N}.yaml` を全スキャン
2. 各 Worker の `pending` / `in_progress` タスク数を集計
3. 未完了タスクがない Worker を `idle`、それ以外を `busy` と表示
4. `--model` 指定時はモデルでフィルタリング
5. 結果を JSON 配列で標準出力に返す

## 5.11 maestro dashboard（CLI → デーモン）

**責務**: dashboard.md の手動再生成

```
Usage: maestro dashboard
```

1. `queue/` と `results/` の全 YAML ファイルを読み取り
2. コマンド・タスクの状態をサマリとして集計
3. `.maestro/dashboard.md` に書き出し（上書き）

> デーモンの定期スキャン時にも自動更新される。本サブコマンドはユーザーが手動で最新状態を即座に確認する場合に使用。

## 5.12 ユーザーへの完了・停止の伝達（macOS 通知は非提供）

旧仕様にあった `maestro notify <title> <message>`（macOS 通知センター連携）は**実装しない**。理由と代替は以下のとおり。

- `up` は既定で tmux セッションへ attach するため、ユーザーは Orchestrator ペインの出力を常時確認できる
- コマンド完了・継続モードの停止は、result_handler → `queue write orchestrator` → Orchestrator の報告という既存パイプラインで tmux 上に表示される
- 追加の OS 依存（osascript）を持たないことで、macOS 以外の実行環境（docker 等）との互換性も保てる

> このため `notify.enabled` / `--no-notify` フラグも存在しない。`config.yaml` の `notify_lease_sec` 等は通知**配信 lease**（エージェント間のサイドチャネル通知）の設定であり、macOS 通知とは無関係。

## 5.13 maestro status（ワンショット）

**責務**: フォーメーションの状態表示

```
Usage: maestro status [--json]
```

1. デーモンの稼働状態を確認（Unix ドメインソケットへの ping）
2. 各 Agent の状態を表示（tmux ユーザー変数から取得）
3. queue depth、pending/in_progress カウント
4. `--json` 指定時は JSON 形式で出力

## 5.14 maestro attach（ワンショット）

**責務**: 稼働中の maestro tmux セッションへの再接続

```
Usage: maestro attach
```

1. config.yaml から project name を解決し、per-instance socket 上の tmux セッション（`maestro-{project}`）を特定
2. 当該セッションへ `tmux attach`（既に attach 済みの場合は切替）
3. セッションが存在しない場合はエラー終了（先に `maestro up` が必要）

> `maestro up` は既定で起動後に自動 attach する。`--detach` で起動した場合や、ユーザーが一度 detach した後に操作を再開する場合に本コマンドを使う。

## 5.15 maestro task heartbeat（CLI → デーモン）

**責務**: 実行中タスクの lease 延命（長時間タスクのタイムアウト誤検知防止）

```
Usage: maestro task heartbeat --task-id <id> --worker-id <id> --epoch <N>
```

1. `--epoch`（lease epoch）のフェンシング検証（stale worker からの heartbeat を拒否）。`--worker-id` はどの Worker queue を参照してフェンシング判定を行うかの解決に必要
2. 検証 OK の場合、該当タスクの `lease_expires_at` を `now + dispatch_lease_sec` に延長
3. これにより、`max_in_progress_min` 未満であれば長時間タスクが lease 期限切れ回収（§5.8 ステップ 2）の対象にならない

> Worker が長時間タスク実行中に明示的に生存を申告するための任意コマンド。デーモン側のビジー判定（§5.9）も lease 延長の根拠になるため、heartbeat は補助的な明示シグナルである。

## 5.16 maestro verify write（CLI → デーモン）

**責務**: command-scoped verify config snapshot の登録（[REQUIREMENTS.md](REQUIREMENTS.md) S1-1）

```
Usage: maestro verify write --command-id <id> [--config-file <path>|-]
```

1. `--config-file`（または stdin `-`）から verify config YAML を読み取り
2. `build` / `lint` / `test` / `typecheck` / `security` / `performance` の 6 カテゴリに strict decode（未知キーは拒否）。最低 1 コマンドを含むことを検証
3. 検証 OK の場合、`state/verify/{command_id}.yaml` に snapshot として保存
4. 各コマンドは `bash -c` 経由で実行されるため通常の shell 構文（`&&` / `|` / `;` 等）を含められる。唯一の制約は「YAML スカラ 1 行に収まること」

> **submit ゲートとの関係**: `verify.enabled` が有効なとき、`plan submit` は本 snapshot の存在・妥当性を fail-closed で要求する（§5.7.1 / REQUIREMENTS.md S1-1）。Planner は `plan submit` の前に本コマンドで verify config を登録する。`verify.enabled: false` は正常運用モードであり、その場合 submit ゲートは発火しない。

## 5.17 maestro skill / maestro persona（CLI → デーモン）

**責務**: Worker のタスク実行に注入される skill / persona の参照・管理

### skill / persona とは（注入機構の定義）

Worker へのタスク配信時、デーモンは task content に以下を機械的に**注入（inject）**してから配信する。Agent はこの注入ロジックを持たず、注入の有無・内容はすべてデーモンの責務である（配線は `internal/daemon/dispatch/envelope.go` の `BuildTaskContent`）。

| 仕組み      | 解決するもの                                                                                      | 実体                                                                                    | 注入対象・方法                                                                                                                                              |
| ----------- | ------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **persona** | Worker の作業スタンス（実装者 / アーキテクト / QA / 調査者 / sweeper 等）を役割に応じて切り替える | `.maestro/persona/{name}.md`（frontmatter + 本文）                                      | タスクの `persona_hint` に一致する persona 本文を、システム生成ガイダンス境界（`--- BEGIN/END PERSONA GUIDANCE ---`）で囲んで task content の先頭に prepend |
| **skill**   | 再利用可能な知識・手順テンプレート（TDD 手順、エラー診断パターン等）を必要なタスクにだけ供給する  | `.maestro/skills/{role}/{name}/SKILL.md` および `.maestro/skills/share/{name}/SKILL.md` | タスクの `skill_refs` に列挙された skill 本文を task content に注入                                                                                         |

- いずれも `maestro setup` がテンプレートから配置・上書きする（バイナリに紐付くため repair モードでも常に上書き）。Worker から制御プレーンとして書き換えることはできない。
- **Worker 挙動への影響**: 注入された persona / skill は Worker のシステムコンテキストの一部として扱われ、タスク本文と区別可能な境界マーカーで囲まれる（プロンプトインジェクション混同の防止）。Worker は推奨として活用するが、`acceptance_criteria` / `constraints` を上書きするものではない。
- **Planner の責務**: タスク分解時に各タスクへ適切な `persona_hint`（任意）と `skill_refs`（任意）を設定する。skill 選定の指針は `skills/planner/worker-skill-selection` 等の planner 向け skill が補助する。

### CLI サブコマンド

```
Usage: maestro skill <list|candidates|approve|reject> [options]
       maestro persona list
```

| コマンド                                                          | 責務                                                                                    |
| ----------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `maestro skill list --role <role>`                                | 指定 role に登録済みの skill 一覧を表示                                                 |
| `maestro skill candidates [--status pending\|approved\|rejected]` | Worker のタスク結果から蓄積された skill 候補（`state/skill_candidates.yaml`）を一覧表示 |
| `maestro skill approve` / `reject`                                | skill 候補を承認 / 却下（承認されたものが正式な skill として参照可能になる）            |
| `maestro persona list`                                            | 登録済み persona の一覧表示                                                             |

> skill 候補の蓄積は将来的な自己改善（[REQUIREMENTS.md](REQUIREMENTS.md) C-5）の足場であり、承認は人間 / オペレーターのゲートを経る。Daemon が候補を自動承認することはない。

## 5.18 maestro doctor（ワンショット）

**責務**: runtime preflight の明示診断。フォーメーション起動前に、設定済みの全 agent runtime が起動可能かを検証して早期に問題を可視化する

```
Usage: maestro doctor [--json] [--probe-timeout-sec <n>]
```

検証対象は config の agents.\*（orchestrator / planner / worker1..N のモデル名）から `ParseRuntimeFromModel` で解決される runtime 集合（claude-code / codex / gemini。boost 有効時は全 Worker が opus = claude-code に集約される）と、tmux バイナリ。runtime ごとに以下を検証する:

| チェック    | 方式                                                                                                              | 失敗時の扱い                                   |
| ----------- | ----------------------------------------------------------------------------------------------------------------- | ---------------------------------------------- |
| **binary**  | CLI（`claude` / `codex` / `gemini`）の PATH 解決                                                                  | **fail**（終了コード 1。確実な失敗はこれのみ） |
| **version** | `<cli> --version` を非対話（stdin=null device）・タイムアウト付き（既定 10 秒）・出力上限付き（既定 8 KiB）で実行 | warn（終了コード 0）                           |
| **auth**    | credential env 変数と credential ファイルの存在確認のみ（サブプロセスなし・値は出力しない）                       | 確認不能は `unknown` の warn（終了コード 0）   |

- **外部 runtime probe のガードレール**: probe は対話プロンプトで hang しない（stdin が null device のため即 EOF）。タイムアウト超過は probe を kill して warn として報告し、取得出力は上限バイト数で切り捨てる。この上限は診断用であり、Worker のタスク成果・結果本文には適用しない。
- **既存 late-failure との関係**: doctor / up preflight は早期検知の前段であり置換ではない。launch 時の `exec.LookPath`（`internal/agent/launcher.go`）と pane terminal-error fast-fail（`authentication_error` 等のリアクティブ検知）はそのまま維持される。
- **汎用 bridge registry 化はしない**: 検証対象は Go 実装の `RuntimeDef` registry（`internal/agent/runtime_launcher.go`）に登録済みの runtime のみ。新 runtime の追加は引き続き Go 実装で行う。
- 実 LLM への test prompt 疎通確認は行わない（コスト・副作用があるため非実装。binary/version/auth チェックが既定かつ唯一のモード）。
- `--json` で機械可読なレポート（`tmux` / `runtimes[]` / `ok`）を出力する。
