# 10. 継続モード（Continuous Mode）

`config.yaml` の `continuous.enabled: true` で有効化。`maestro up --continuous` でも指定可能。

## イテレーションループ

> **命名規約**: 本章のループステップ（Execute / Commit / Evaluate / Decide）は継続モード固有の名称であり、[§6](06-execution-flow.md) のシステムフェーズ A-D とは独立した概念である。

```
Execute:  タスク実行（通常のコマンド実行サイクル [§6](06-execution-flow.md) フェーズ C）
    ↓
Commit:   コミット（デーモンが自動挿入したシステムコミットタスクを Worker が実行）
    ↓
Evaluate: 評価（Planner がコミット結果を含む全タスクの成果を評価し、Orchestrator に報告）
    ↓
Decide:   判定（Orchestrator が次のアクションを決定。デーモンが current_iteration をインクリメント）
    ├── a: 次のコマンドを自動生成 → Execute に戻る
    ├── b: 軽微な修正が必要 → Execute に戻る（修正コマンド付き）
    ├── c: ユーザー判断が必要 → 停止
    └── d: 完了 → 停止
```

## Commit ステップ: システムコミットタスクの自動挿入

### 設計原則

Agent（Planner・Worker）はコミットのための追加ロジックを持たない。デーモンが `plan submit` 受理時にシステムコミットタスクを自動付与し、全ユーザータスク完了後に Worker が実行する。

### デーモンの自動挿入フロー

1. デーモンが `plan submit` を受理する際、`continuous.enabled: true` を検出
2. Planner が定義した全タスク（以下「ユーザータスク」）の後に、**システムコミットタスク**を 1 件自動挿入:
   - `name: "__system_commit"`（予約名。Planner のタスク名との衝突を防止）
   - `purpose: "コマンド実行結果をリポジトリにコミットする"`
   - `content: "変更ファイルを確認し、git add + git commit を実行する。コミットメッセージはタスク実行結果のサマリから生成する。"`
   - `acceptance_criteria: "git commit が成功し、コミットハッシュが取得できる"`
   - `bloom_level: 2`（Sonnet で十分な定型作業）
   - `required: true`
3. 通常の `plan submit` フローでタスク ID 生成・Worker 割当・queue 書き込みを一括実行
4. state/commands/ の `system_commit_task_id` フィールドにコミットタスク ID を記録（デーモンがコミットタスクを識別するために使用）

> **Planner への影響なし**: Planner は通常通り `plan submit` するだけ。コミットタスクの存在を意識する必要はない。`plan complete` 時にはコミットタスクも含めた全タスクの完了が必要となるが、これは既存の完了条件ロジック（`required_task_ids` の全 task が terminal）で自然に処理される。

### `tasks` の場合（フェーズなし）

- タスク配列の末尾に `__system_commit` を追加
- `blocked_by` に全ユーザータスクの name を設定（全タスク完了後に実行）
- Queue ハンドラの通常の `blocked_by` 解決で配信される

### `phases` の場合（フェーズ付き）

フェーズ構成がある場合、`__system_commit` タスクは**フェーズ構造の外**に独立タスクとして追加される（いずれのフェーズの `task_ids` にも含めない）。

- `blocked_by` は空配列（フェーズ間依存をタスクレベルで表現しない）
- Queue ハンドラが `system_commit_task_id` と一致するタスクに対し、**全ユーザーフェーズが terminal**（`completed` / `failed` / `cancelled` / `timed_out`）であることを配信条件として特別判定する（[§5.8.1](05-script-responsibilities.md) 参照）
- フェーズの型定義（concrete / deferred）や `depends_on_phases` ルールに影響を与えない

### コミットタスク実行時の Worker の挙動

Worker はシステムコミットタスクを通常のタスクと同様に実行する。タスクの content に従い:

1. `git status` で変更ファイルを確認
2. 変更がある場合: `git add` + `git commit` を実行
3. 変更がない場合: 「コミット対象の変更なし」として `completed` で報告
4. 結果を `maestro result write` で報告

### コミット失敗ポリシー

| 状況 | Worker の報告 | 後続への影響 |
|---|---|---|
| 変更ファイルなし（no-op） | `status: completed`, `summary: "コミット対象の変更なし"` | Planner は成果なしとして評価 |
| コミット成功 | `status: completed`, `summary: "commit {hash}"` | 正常フロー |
| ステージングエラー（ファイル不在等） | `status: failed`, `partial_changes_possible: false` | Planner がリトライ or 失敗報告を判断 |
| コミットコンフリクト | `status: failed`, `partial_changes_possible: true`, `retry_safe: false` | 手動復旧が必要。`pause_on_failure: true` なら自動停止 |
| git コマンド実行エラー | `status: failed` | Planner がリトライ or 失敗報告を判断 |

## Evaluate ステップ: 評価の詳細

1. Planner が全タスク（ユーザータスク + システムコミットタスク）の完了通知を受け取る
2. command 全体の成果を評価（コミット結果を含む）
3. 評価結果（成功/部分的成功/失敗 + 要約）を Orchestrator に報告（`maestro plan complete`）

## Decide ステップ: 判定の詳細

Orchestrator は Planner からの報告を受け、以下の基準で次のアクションを決定する:

| 条件 | アクション |
|---|---|
| 評価が成功かつ未達成の目標がある | a: 次のコマンドを自動生成 → Execute に戻る |
| 評価に軽微な問題が含まれる | b: 修正コマンドを生成 → Execute に戻る |
| 評価に判断が困難な問題が含まれる | c: ユーザー判断が必要 → 停止 + macOS 通知 |
| 全目標が達成された | d: 完了 → 停止 + macOS 通知 |
| `max_iterations` に到達 | 強制停止 + macOS 通知 |
| `pause_on_failure: true` かつタスク失敗 | 自動停止 + macOS 通知 |

Orchestrator が次のコマンドを自動生成する場合、通常のコマンド実行サイクルと同じ経路（`maestro queue write planner`）でコマンドを投入する。

## イテレーションカウンタ

### インクリメントタイミング

`current_iteration` は「完了した Execute→Decide ループ数」を意味する。デーモンは以下のタイミングでインクリメントする:

1. Planner の `plan complete` が成功し、`results/planner.yaml` に結果が書き込まれた時点
2. デーモンの Result ハンドラが Orchestrator への通知処理を完了した時点でインクリメント
3. `last_command_id` による冪等ガード: 同一 `command_id` に対する二重インクリメントを防止

```
result_handler が results/planner.yaml の通知を処理
  ├── Orchestrator への通知キューイング（queue write orchestrator）
  ├── state/continuous.yaml の更新:
  │   ├── last_command_id が同一 → スキップ（冪等ガード）
  │   └── last_command_id が異なる → current_iteration += 1, last_command_id = {command_id}
  └── current_iteration >= max_iterations → status: "stopped", paused_reason: "max_iterations_reached"
```

### 暴走防止

- `max_iterations` に達したら強制停止（デーモンが `state/continuous.yaml` の `current_iteration` で管理）
- `pause_on_failure: true` でタスク失敗時に自動停止
- 各イテレーションで `dashboard.md` を自動更新（デーモン経由）し、進捗を可視化
- 停止時は必ず macOS 通知でユーザーに停止理由を通知（`notify.enabled: true` 時）

### 永続化

- デーモンは `state/continuous.yaml` でイテレーション数を永続化する（[§4.9](04-yaml-schema.md) 参照）
- デーモン再起動後も `current_iteration` が復元されるため、再起動による暴走防止バイパスは発生しない
- `maestro up --reset` 時は `state/continuous.yaml` もクリアされ、カウンタは 0 にリセットされる
