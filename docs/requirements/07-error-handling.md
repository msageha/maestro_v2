# 7. エラーハンドリング

## 7.1 タスク失敗時のフロー

```
Worker がタスク実行に失敗
  └── maestro result write worker{N} --task-id X --status failed --summary "..."
      ├── results/worker{N}.yaml に失敗結果追加
      ├── queue/worker{N}.yaml の該当タスクを failed に更新
      └── state/commands/{command_id}.yaml の task_states[X] を failed に更新
  │
  ▼
デーモン（result_handler）が Planner に失敗を通知
  │
  ▼
Planner が判断:
  ├── リトライ可能 → maestro plan add-retry-task --retry-of <failed_task_id> で失敗タスクを置換
  │   （デーモンが required_task_ids で旧→新を置換 + 推移的にキャンセルされた依存タスクを自動復旧。
  │    旧タスクは retry_lineage に記録。Planner は 1 回の呼び出しで済む）
  ├── 代替アプローチ → maestro plan add-retry-task --retry-of <failed_task_id> で別アプローチに置換
  └── 致命的 → maestro plan complete でコマンド全体を Orchestrator に報告（ステータスは自動導出）
```

### 7.1.1 フェーズ失敗時のフロー

```
調査フェーズが失敗（フェーズ内の required タスクが failed/cancelled）
  ├── デーモン（定期スキャン ステップ 0.7）がフェーズ failed を検知
  │   → 下流 deferred フェーズを cancelled にカスケード
  ├── Planner に通知
  └── Planner が判断:
      ├── add-retry-task --retry-of で失敗タスクを置換
      │   （デーモンが自動で: フェーズを active に再オープン + 推移的キャンセルの依存タスクを自動復旧）
      └── plan complete で command を報告（failed が自動導出）
```

```
deferred フェーズの fill タイムアウト（Planner が fill_deadline_at までにタスクを投入しなかった）
  ├── デーモン（定期スキャン ステップ 0.7）がフェーズ timed_out を検知
  │   → 下流 pending フェーズを cancelled にカスケード
  ├── Planner に通知
  └── Planner が plan complete で command を報告（failed が自動導出）
```

## 7.2 デーモンプロセス異常終了

- `maestro up` がデーモンの稼働状態を確認し、停止していれば再起動
- 再起動後は全 YAML をスキャンし、未処理の `pending`（queue/）/ `notified: false`（results/）を処理
- → デーモンクラッシュ中のメッセージもロストしない（YAML が権威ソース）
- デーモンの graceful shutdown（`SIGTERM`）では進行中の書き込みを完了してから終了

## 7.3 エージェント無応答（ビジー検知併用タイムアウト）

- デーモンの定期スキャンが `status: in_progress` かつ `lease_expires_at < now` のエントリを検知
- **Agent ペインのビジー判定を実行**:
  - Agent が busy（Working/Thinking 等）かつ `updated_at` からの経過が `max_in_progress_min` 未満（`<`）
    → 正常実行中と見なし lease を延長
  - Agent がアイドル、または `max_in_progress_min` 以上（`>=`、到達時点でタイムアウト）
    → Agent が停止/ハングしていると判断:
    1. agent_executor で `/clear` 送信してエージェントをリセット
    2. ステータスを `pending` に戻す（lease も解放）
    3. 次のスキャンで再配信

## 7.4 配信失敗のリカバリ

| 失敗パターン | 即時アクション | リカバリ |
|---|---|---|
| queue_handler 配信失敗（ビジータイムアウト） | ステータスを `pending` + lease 解放 | 定期スキャンで再試行 |
| result_handler 通知失敗（ビジータイムアウト） | notification lease クリア | 定期スキャンで再試行 |
| Orchestrator 配信失敗（厳格ビジー判定） | ステータスを `pending` + lease 解放 | 定期スキャンで再試行 + macOS 通知は即時送信済み |
| 再配信による同一タスク再実行 | result write が冪等キー重複を検知し結果の二重確定を拒否 | 同一 `task_id` / `command_id` は effectively-once で反映 |
| stale Worker からの遅着結果 | result write が `--lease-epoch` フェンシング検証で不一致を検出し結果を拒否 | Worker は stale であることを認識し、以降の作業を停止 |

→ いずれも恒久ロストにならない。YAML が権威ソースであり、定期スキャンが最終保証。

## 7.5 クラッシュリカバリ（整合性修復）

定期スキャンの reconciliation ステップが全 8 パターン（R0, R0b, R1-R5, R6）の不整合を自動修復する。各パターンの詳細は [§5.8 ステップ 3](05-script-responsibilities.md#58-maestro-daemon) を参照。

## 7.6 排他制御の障害

- デーモン内の `sync.Mutex` はプロセス終了時に自動解放される → デッドロックしない
- デーモンクラッシュ時のファイルロック（`daemon.lock`）はプロセス終了で自動解放
- CLI サブコマンドは書き込みをデーモンに委譲するため、CLI 側でのロック障害は発生しない

## 7.7 YAML ファイル肥大化

**アーカイブ条件**（コマンドライフサイクルゲート付き）:

アーカイブは**保持ポリシー**であり、正確性に影響してはならない。Worker 結果は Planner のコマンド結果集約に必要なため、コマンド完了前にアーカイブすると正確性が壊れる。

| ディレクトリ | 条件 | 判定フィールド |
|---|---|---|
| `queue/worker{N}.yaml` | `status` が terminal（`completed\|failed\|cancelled`）**かつ** 対応する `state/commands/{command_id}.yaml` の `plan_status` が terminal | `status` + command lifecycle |
| `queue/planner.yaml` | `status` が terminal（`completed\|failed\|cancelled`） | `status` のみ |
| `queue/orchestrator.yaml` | `status: completed\|dead_letter` | `status` のみ |
| `results/worker{N}.yaml` | `notified: true` **かつ** 対応する `state/commands/{command_id}.yaml` の `plan_status` が terminal **かつ** `results/planner.yaml` に同一 `command_id` の terminal 結果が存在 | `notified` + command lifecycle + planner result |
| `results/planner.yaml` | `notified: true` **かつ** `queue/orchestrator.yaml` に対応する `source_result_id` の通知が存在 | `notified` + orchestrator notification |
| `state/commands/*` | `plan_status: completed\|failed\|cancelled` から 7 日経過 | `plan_status` + 経過日数 |

- デーモンの定期スキャン時に、上記条件を満たすエントリが 50 件超過したファイルを自動アーカイブ
- アーカイブ先: `.maestro/logs/{filename}.{timestamp}.archive.yaml`
- 直近 10 件は保持（直近の履歴参照用）
- ID はグローバル一意のため、アーカイブ後の新規採番と衝突しない
- `state/commands/` は command 単位ファイルのため、terminal + 7 日経過したファイルを `.maestro/logs/commands/` へ移動

## 7.8 command 完了条件（state/commands）不整合

| 不整合パターン | 検出箇所 | アクション |
|---|---|---|
| Planner が完了条件未達で完了報告を試行 | `maestro plan complete` | 内部の can-complete 検証で即時終了。`results/`・`queue/`・`state/commands/` を一切更新しない。Planner に未完了 task 一覧を返す |
| `required + optional` 件数と `expected_task_count` が不一致 | `maestro plan submit` / `plan complete` 内部 | submit 失敗（ロールバック）または完了拒否。Planner はタスク定義を修正して再 submit |
| worker 結果の `task_id` が `state/commands/{command_id}.yaml` に存在しない | `maestro result write worker` | 結果を拒否しエラーログへ記録。Planner へ異常通知を送る |
| `blocked_by` に循環依存が存在 | `maestro plan submit` | submit 失敗（全体ロールバック）。循環パスをエラーメッセージに含める |
| `plan_status: planning` が持続 | reconciler（R0 パターン） | `plan submit` のクラッシュと判断。state ファイル削除 + queue エントリ除去でロールバック。Planner に再 submit を通知 |
| 完了ステータスの不一致（手動指定 vs derived） | — | `plan complete` は `--status` フラグを持たず derived ステータスを自動使用するため、原理的に発生しない |
| `task_dependencies` と `queue/` の `blocked_by` が不一致 | `maestro plan submit` | submit 内部で task_id 生成と queue 書き込みを一括実行するため、原理的に発生しない（アトミック処理により常に整合） |
| リトライ置換後の旧タスクへの遅着結果 | `maestro result write worker` | `retry_lineage` で置換済みの旧タスク ID を検知し、ログに記録するが state には反映しない。Worker には stale を通知 |
| deferred フェーズが fill されない（タイムアウト） | 定期スキャン（R6） | `timed_out` に遷移。下流フェーズを `cancelled`。Planner に通知 |
| フェーズ fill が constraints に違反（タスク数超過、bloom_level 範囲外等） | `plan submit --phase` | 拒否。フェーズは `awaiting_fill` を維持。Planner が修正して再試行 |
| フェーズ fill 中のクラッシュ（`status: filling` が持続） | reconciler（R0b）/ `plan complete`（retryable エラー） | `awaiting_fill` に戻し部分書き込みを除去。Planner に再 fill 通知。`plan complete` が `filling` 状態を検出した場合は retryable エラーを返す |
| 調査フェーズ全失敗で下流が不要に | 定期スキャン ステップ 0.7 | 下流 `pending` / `awaiting_fill` フェーズを `cancelled` にカスケード |

## 7.9 リトライ上限と dead-letter

- 各 queue エントリは `attempts` をカウントし、config.yaml の `retry.*` で定義された上限に達すると `dead_letter` に遷移
- dead-letter エントリは `.maestro/dead_letters/` にアーカイブされ、queue ファイルからは除去される
- dead-letter は terminal であり、変更不可
- **queue 型別の後処理**:
  - タスク（`queue/worker{N}.yaml`）: `state/commands/` の `task_states` に合成 `failed` を記録し、Planner に通知
  - コマンド（`queue/planner.yaml`）: `state/commands/` が存在すれば `plan_status` を `failed` に更新。未 submit（state ファイル不在）の場合は state 更新をスキップし、Orchestrator にコマンド配信失敗を通知
  - 通知（`queue/orchestrator.yaml`）: state 更新不要。デーモンアラートメトリクスに記録

## 7.10 キャンセルフロー

Orchestrator は `maestro queue write planner --type cancel-request` でキャンセル意図を送信する。デーモンがコマンドのライフサイクル段階に応じてキャンセル処理を実行する。実際のタスク中断・状態遷移はデーモン Queue ハンドラの fsnotify 配信判定（§5.8.1 ステップ 2e）および定期スキャン（§5.8 ステップ 0.5, 0.6）が実行する。

```
ユーザーが Orchestrator にキャンセル指示
  └── Orchestrator が maestro queue write planner --type cancel-request を実行
      ├── [submit 済み] デーモンが state/commands/ の cancel.requested = true を設定（単調: 取り消し不可）
      └── [未 submit] デーモンが queue/planner.yaml のコマンドエントリを直接 cancelled に遷移
  │
  ▼
デーモン Queue ハンドラが実行:
  ├── [fsnotify 配信判定] cancel.requested チェック → 配信せず cancelled に遷移（§5.8.1 ステップ 2e）
  ├── [定期スキャン ステップ 0.5] pending タスク → cancelled に遷移
  ├── [定期スキャン ステップ 0.6] in_progress タスク → agent_executor --interrupt で中断 → cancelled
  ├── 遅着結果（キャンセル後に届いた result）→ ログに記録するが state には反映しない
  └── 全 task が terminal → Planner が command 結果を cancelled で報告
```

## 7.11 依存関係失敗伝搬

```
Worker がタスク X を failed で報告
  └── result write がフェーズ B で state/commands/ の task_states[X] を failed に更新
      │
      ▼
デーモン（Queue ハンドラ）が依存失敗を検知（fsnotify パス ステップ 2e / 定期スキャン ステップ 1 + 1.5 の blocked_by 判定）:
  ├── dependency_failure_policy = cancel_dependents の場合:
  │   ├── task_dependencies から X に依存する全タスク（推移的）を計算
  │   ├── pending 依存タスク → cancelled + cancelled_reasons に "blocked_dependency_terminal:X" を記録
  │   ├── in_progress 依存タスク → agent_executor --interrupt で中断 → cancelled + cancelled_reasons 記録（lease 解放）
  │   └── state/commands/ の task_states を cancelled に更新、cancelled_reasons を記録
  │   └── Planner に影響タスク一覧を 1 回通知
  └── Planner が判断:
      ├── 代替計画 → maestro plan add-retry-task --retry-of <failed_task_id> で失敗タスクを置換
      │   → デーモンが推移的にキャンセルされた依存タスク（reason: blocked_dependency_terminal:X）を自動復旧
      │   → Planner は失敗タスク 1 件に対して add-retry-task を 1 回呼ぶだけでよい
      └── 致命的 → maestro plan complete でコマンド結果を報告（ステータスは自動導出）
```

## 7.12 タスク実行の冪等性と再配信

- result write の冪等キーによりステータスの二重確定は防止されるが、**タスク実行自体（リポジトリへの副作用）の冪等性は保証されない**
- Worker は結果に `partial_changes_possible` と `retry_safe` を報告:
  - `partial_changes_possible: true` + `retry_safe: false` → Planner への通知メッセージにこれらの情報を含め、手動介入が必要であることを伝達
  - `retry_safe: true` → Planner への通知メッセージに安全にリトライ可能であることを伝達
- リトライは `add-retry-task --retry-of` で失敗タスクを新タスクに置換する方式。旧タスク ID は `required_task_ids` から除外されるが、`task_states` と `retry_lineage` に監査履歴として残る
- デーモンは `add-retry-task` 実行時に、失敗タスクの依存失敗でキャンセルされた下流タスク（`reason: blocked_dependency_terminal:{task_id}`）を自動的に復旧する（cascade recovery）。Planner は失敗タスク 1 件に対して 1 回の呼び出しで済む

**配信リトライとタスク実行リトライの分離**:

| レベル | トリガー | デーモンの挙動 | Planner の関与 |
|---|---|---|---|
| **配信リトライ**（インフラ層） | ビジータイムアウト等で Agent にメッセージを届けられなかった場合 | 常に `pending` に戻し、定期スキャンで再配信（Worker は未実行のため副作用なし） | 関与しない |
| **タスク実行リトライ**（アプリ層） | Worker が `status: failed` で結果を報告した場合 | タスクを `failed`（terminal）で確定。Planner に `retry_safe` / `partial_changes_possible` の情報を含めて通知 | Planner が `add-retry-task --retry-of` で置換するか `plan complete` で終了するかを判断 |

> **設計意図**: 配信失敗（インフラ事象）とタスク実行失敗（アプリ事象）を明確に分離する。配信失敗は Worker 未実行なので常に安全に再試行できる。タスク実行失敗は副作用が発生している可能性があるため、Planner が `retry_safe` を参照して判断する。

## 7.13 破損 YAML のリカバリ

| 状況 | アクション |
|---|---|
| YAML パースエラー | quarantine にコピー → .bak からリストア → 処理継続 |
| .bak も存在しない | 最小スケルトン再生成（空配列）→ 処理継続 |
| 書き込み中のクラッシュ | アトミック書き込みパターン（tmp + mv）により中間状態は残らない |

## 7.14 バックプレッシャー

check-then-add パターンのため、追加後に上限を超えないことを保証する判定基準を使用する。

| 条件 | アクション |
|---|---|
| pending コマンド数 >= `max_pending_commands` | `maestro queue write` がエラー終了（メッセージ: "Queue full"。追加すると上限超過するため拒否）|
| Worker の pending タスク数 >= `max_pending_tasks_per_worker` | `maestro queue write` がエラー終了（同上） |
| content バイト数 > `max_entry_content_bytes` | `maestro queue write` がエラー終了（content は追加前の単体検証のため `>` で判定） |
| YAML ファイルサイズ: `current_size + estimated_entry_size > max_yaml_file_bytes` | アーカイブ可能エントリを先にアーカイブ → リトライ → それでも超過ならエラー終了（追加後サイズで判定） |

> オーバーロード時もメッセージのサイレントドロップは発生しない。拒否は明示的エラーとして呼び出し元に返る。
>
> **`plan submit` のバックプレッシャー**: `plan submit` は内部で各 Worker への queue write を実行するため、バックプレッシャーチェック（Worker あたりの pending タスク上限）も submit 内部で自動的に行われる。全 Worker が上限に達している場合は submit 全体がロールバックされる。

## 7.15 継続モードのシステムコミットタスク失敗

継続モード（`continuous.enabled: true`）では、デーモンが `plan submit` 受理時にシステムコミットタスク（`__system_commit`）を自動挿入する（[§10](10-continuous-mode.md) 参照）。このタスクの失敗は通常のタスク失敗と同じフローで処理される。

| 状況 | Worker の報告 | 後続への影響 |
|---|---|---|
| 変更ファイルなし（no-op） | `status: completed`, `summary: "コミット対象の変更なし"` | Planner が `plan complete` で成果なしとして報告。Orchestrator が Decide ステップ（[§10](10-continuous-mode.md)）で判定 |
| コミット成功 | `status: completed`, `summary: "commit {hash}"` | 正常フロー。Planner が統合結果として報告 |
| ステージングエラー | `status: failed`, `partial_changes_possible: false` | Planner が `add-retry-task --retry-of` でリトライ or `plan complete` で失敗報告 |
| コミットコンフリクト | `status: failed`, `partial_changes_possible: true`, `retry_safe: false` | 手動復旧が必要。`pause_on_failure: true` なら自動停止。Planner は `plan complete` で失敗報告 |
| git コマンド実行エラー | `status: failed` | Planner が `add-retry-task --retry-of` でリトライ or `plan complete` で失敗報告 |

> システムコミットタスクの失敗は `required_task_ids` に含まれるため、コマンド全体のステータスに影響する。Planner はコミット失敗を他のタスク失敗と同様に扱い、リトライまたは失敗報告を行う。追加のロジックは不要。
