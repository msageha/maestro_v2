# 6. 実行フロー詳細

## フェーズ A: インストール → セットアップ（手動・1 回）

```
① install.sh
   ├── 依存チェック: tmux, go, claude
   ├── go build -o maestro ./cmd/maestro/
   └── バイナリを ~/bin/ or /usr/local/bin/ に配置

② maestro setup <project_dir>
   └── .maestro/ ディレクトリ構造を作成
       ├── templates/ からファイルコピー（config.yaml, maestro.md, instructions/, dashboard.md）
       ├── queue/, results/, state/commands/, locks/, logs/, dead_letters/, quarantine/ ディレクトリ作成
       ├── state/metrics.yaml, state/continuous.yaml を初期値で作成
       ├── locks/daemon.lock を作成（ファイルロックは daemon.lock のみ）
       ├── config.yaml の自動埋め（project.name, project_root, created）
       └── Worker 数に応じた queue/worker{N}.yaml, results/worker{N}.yaml を作成
```

## フェーズ B: フォーメーション起動

```
③ maestro up [--reset] [--boost] [--continuous] [--no-notify]
   │
   ├── --reset 指定時:
   │   ├── 既存 tmux セッション・デーモンプロセスの停止
   │   ├── queue/, results/, state/commands/ の YAML クリア
   │   ├── state/continuous.yaml のリセット（current_iteration: 0）
   │   ├── state/metrics.yaml のカウンターリセット
   │   ├── dead_letters/ のクリア
   │   ├── quarantine/ は保持（デバッグ・フォレンジクス用）
   │   ├── --reset のみ: 終了（フォーメーション起動は行わない）
   │   └── --reset + 他フラグ: リセット後にフォーメーション起動へ進む
   │
   ├── config.yaml 読み込み・フラグ反映
   │
   ├── スタートアップリカバリ
   │   ├── daemon ロックで競合防止
   │   ├── YAML 構文検証（破損 → quarantine → リストア）
   │   ├── schema_version チェック（旧版 → マイグレーション）
   │   └── ワンショット reconciliation（R0, R0b, R1-R5, R6 の全 8 パターン）
   │
   ├── tmux セッション作成（内部 tmux モジュール使用）
   │   ├── Window 0: orchestrator (1 pane)
   │   ├── Window 1: planner (1 pane)
   │   ├── Window 2: workers (grid layout)
   │   └── 各ペインに @agent_id, @role, @model, @status を設定
   │       （model は default_model + models map から解決）
   │
   ├── 各ペインで maestro agent launch を実行
   │   └── claude --model {model} --append-system-prompt "..." --dangerously-skip-permissions
   │
   └── maestro daemon (background, daemon lock)
       ├── Queue ハンドラ: queue/ 変更検知 → 配信
       └── Result ハンドラ: results/ 変更検知 → 通知
```

## フェーズ C: コマンド実行サイクル

```
④ ユーザーが Orchestrator に指示入力
   │
   ▼
⑤ Orchestrator Agent が指示を構造化
   └── maestro queue write planner --type command --content "認証機能を実装して..."
       → queue/planner.yaml に pending エントリ追加（ID は自動生成）
   │
   ▼
⑥ デーモン（Queue ハンドラ）が queue/planner.yaml の変更を検知
   ├── per-agent mutex 取得 → pending（または期限切れ in_progress）を取得
   │   → in_progress + lease 付与 + attempts 加算 → mutex 解放
   ├── agent_executor で Planner にコマンド内容を配信
   │   ├── ビジー判定（busy_patterns + idle_stable_sec）→ idle 確認
   │   └── tmux send-keys でコマンド内容を配信
   └── 配信失敗時: mutex 再取得 → pending + lease 解放 → mutex 解放
   │
   ▼
⑦ Planner Agent がコマンドを受信・タスク分解（Five Questions フレームワーク）
   ├── Five Questions で分解:
   │   ├── What: 成果物を洗い出し → 事前調査が必要か判断
   │   │   ├── 単一フェーズで完結 → 従来通りの plan submit（phases なし）
   │   │   └── 調査→実装の段階が必要 → phases 付き plan submit
   │   ├── Who: bloom_level 設定（deferred は constraints.allowed_bloom_levels で範囲指定）
   │   ├── Order: フェーズ間依存 (depends_on_phases) + フェーズ内タスク依存 (blocked_by)
   │   ├── Risk: フェーズ単位の失敗リスク評価
   │   └── Verify: concrete フェーズの acceptance_criteria 確認
   └── maestro plan submit（phases 付きまたは tasks のみ）
       ├── 内部処理（デーモン側で自動実行）:
       │   ├── tasks/phases YAML バリデーション + DAG 検証（タスクレベル + フェーズレベル）
       │   ├── continuous.enabled: true → __system_commit タスクを自動挿入（§10 参照）
       │   │   ├── tasks の場合: blocked_by に全ユーザータスクを設定
       │   │   └── phases の場合: フェーズ構造の外に独立タスクとして追加（配信条件は全フェーズ terminal）
       │   ├── Worker 自動割当（concrete フェーズのタスク + __system_commit。bloom_level → model → pending 最小）
       │   ├── task_id / phase_id 生成 + ローカル name → task_id 変換
       │   ├── state/commands/{command_id}.yaml 作成（planning → sealed）
       │   │   ├── phases 付き: phases 配列を格納（concrete=active, deferred=pending）
       │   │   └── phases なし: phases: null（暗黙の単一 concrete フェーズ）
       │   └── queue/worker{N}.yaml にタスクエントリ追加（concrete フェーズのタスク + __system_commit）
       └── stdout に task_id + worker 対応表を JSON 出力（phases 付き出力含む）
   │
   ▼
⑦.5 デーモン（定期スキャン ステップ 0.7）がフェーズ完了を検知
    ├── active フェーズの全 required タスク terminal → フェーズ completed/failed/cancelled
    ├── pending フェーズの依存フェーズ全 completed → awaiting_fill に遷移
    │   ├── fill_deadline_at を設定（now + constraints.timeout_minutes）
    │   └── agent_executor で Planner に通知
    ├── pending フェーズの依存フェーズに failed/cancelled/timed_out → cancelled（カスケード）
    └── awaiting_fill の fill_deadline_at < now → timed_out
    │
    ▼
⑦.6 Planner がフェーズ完了通知を受信
    ├── 前フェーズの結果を results/worker{N}.yaml から確認
    ├── 調査結果に基づき次フェーズのタスクを設計
    └── maestro plan submit --command-id cmd_... --phase <name> --tasks-file <tasks.yaml>
        → 以降は ⑧（Worker へのタスク配信）に合流
    │
    ▼
    （注: フェーズなし command では ⑦.5 / ⑦.6 はスキップされ、従来通り ⑦ → ⑧ に進む）
    │
    ▼
⑧ デーモン（Queue ハンドラ）が queue/worker{N}.yaml の変更を検知
   ├── per-agent mutex → pending エントリを取得
   │   ├── blocked_by 判定: state/commands/{command_id}.yaml の task_states を参照
   │   │   └── blocked_by 内の全タスクが completed → 配信可能 / 未完了あり → スキップ
   │   └── 配信可能 → in_progress + lease 付与 + attempts 加算 → mutex 解放
   ├── agent_executor で Worker にタスク内容を配信 (--with-clear)
   │   ├── /clear を送信（前タスクのコンテキストをリセット）
   │   ├── cooldown_after_clear 秒待機
   │   └── タスク内容を送信
   └── 配信失敗時: pending + lease 解放
   │
   ▼
⑨ Worker Agent がタスク実行
   └── maestro result write worker{N} --task-id ... --lease-epoch {N} --status completed --summary "..."
       ├── [フェーズ A: per-agent mutex]
       │   ├── per-agent mutex 取得
       │   ├── task_id 存在検証（state/commands/ に未登録なら永続化前に拒否。§5.7.1 厳密ルール 5 参照）
       │   ├── lease_epoch フェンシング検証（stale worker の結果を拒否）
       │   ├── task_id 冪等チェック（重複結果を拒否）
       │   ├── results/worker{N}.yaml に結果追加 (notified: false)
       │   ├── queue/worker{N}.yaml の該当タスクを completed に更新
       │   └── per-agent mutex 解放
       ├── [フェーズ B: per-command mutex]
       │   ├── per-command mutex 取得
       │   ├── state/commands/{command_id}.yaml の task_states/applied_result_ids を更新
       │   └── per-command mutex 解放
       └── [フェーズ C: 依存解決トリガー]
           └── 完了タスクに依存する他タスクの queue ファイルを touch → fsnotify 発火
   │
   ▼
⑩ デーモン（Result ハンドラ）が results/worker{N}.yaml の変更を検知
   ├── per-agent mutex 取得 → notified: false 発見 → notification lease 取得 → mutex 解放
   ├── agent_executor で Planner に通知 "<task_id> 完了通知"
   │   ├── 成功 → mutex 再取得 → notified: true → mutex 解放
   │   └── 失敗 → mutex 再取得 → lease クリア → mutex 解放（定期スキャンで再試行）
   │
   ▼
⑪ Planner が全タスクの完了を確認・結果統合
   └── maestro plan complete --command-id cmd_... --summary "..."
       ├── 内部処理（デーモン側で自動実行）:
       │   ├── [can-complete 検証]
       │   │   ├── per-command mutex 取得
       │   │   ├── 完了条件を検証 → derived_status を自動導出
       │   │   ├── NG → mutex 解放 → 即時終了（何も書き込まない）
       │   │   └── per-command mutex 解放
       │   ├── [フェーズ A: per-agent mutex]（result write planner 相当）
       │   │   ├── per-agent mutex 取得
       │   │   ├── results/planner.yaml に統合結果追加（tasks は自動集約、ステータスは derived_status）
       │   │   ├── queue/planner.yaml の該当コマンドを derived_status に更新
       │   │   └── per-agent mutex 解放
       │   └── [フェーズ B: per-command mutex]
       │       ├── per-command mutex 取得
       │       ├── state/commands/{command_id}.yaml の plan_status を derived_status に更新
       │       └── per-command mutex 解放
       └── ステータスは can-complete から自動導出（--status フラグ不要）
   │
   ▼
⑫ デーモン（Result ハンドラ）が results/planner.yaml の変更を検知
   ├── per-agent mutex → notification lease 取得 → mutex 解放
   ├── maestro queue write orchestrator --type notification --command-id cmd_... --content "..."
   │   → queue/orchestrator.yaml に pending 通知追加
   ├── maestro notify "Maestro" "cmd_... が完了しました"
   └── 成功 → mutex 再取得 → notified: true → mutex 解放
       失敗 → mutex 再取得 → lease クリア → mutex 解放（定期スキャンで再試行）
   │
   ▼
⑬ デーモン（Queue ハンドラ）が queue/orchestrator.yaml の変更を検知
   ├── per-agent mutex → pending（または期限切れ in_progress）を取得
   │   → in_progress + lease 付与 + attempts 加算 → mutex 解放
   ├── agent_executor で Orchestrator に通知
   │   ├── 厳格ビジー判定（busy_patterns + idle_stable_sec、リトライなし）
   │   ├── idle → 配信成功 → Orchestrator がユーザーに結果報告
   │   └── busy → 配信失敗 → pending + lease 解放（定期スキャンで再試行）
   │                         macOS 通知は ⑫ で送信済み（ユーザーは完了を認識可能）
   └── （配信失敗は恒久ロストにならない — 定期スキャンが保証）
```

## フェーズ D: Worker のコンテキスト管理

Worker への全タスク配信は `--with-clear` 付きで行われる（⑧ 参照）。
これにより、前タスクのコンテキストが次タスクに漏れることを防止する。

- **初回配信**（`agent launch` 直後）: `/clear` は新規セッションに対して無害
- **2 回目以降の配信**: `/clear` で前タスクのコンテキストをリセットしてからタスクを配信
- **次タスクがない場合**: Worker は idle 状態を維持（前タスクのコンテキストが残るが、次回配信時に `/clear` されるため問題なし）

> Planner・Orchestrator への配信は `--with-clear` を使用しない。
> Planner は複数コマンドの文脈を保持することで判断精度が向上する。
> Orchestrator はユーザーとの対話コンテキストを維持する必要がある。
