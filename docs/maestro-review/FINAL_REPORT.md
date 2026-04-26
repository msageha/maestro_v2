# Maestro v2 包括的コードレビュー 最終統合レポート

- 作成日: 2026-04-26
- 対象 commit: e7dccde
- 由来コマンド: cmd_1777200395_b2770217f280e933 (cancelled) → cmd_1777204067_8a1139a5765fbd54 (resume)
- 統合元セクション: tmp/maestro-review/sections/01〜05 (全5ファイル、計1,015 行)

---

## 1. エグゼクティブサマリ

- **総 Finding 数(統合・重複除去後)**: **66 件** (01: 16, 02: 11, 03: 11, 04: 14, 05: 16、重複 2 件除去)
- **重要度別件数**:
  - **Critical**: 4 件
  - **High**: 16 件
  - **Medium**: 21 件
  - **Low**: 17 件
  - **Nit**: 8 件
- **統合された重複 Finding**:
  - F-014 (旧 01-F-14) と F-064 候補 (旧 05-F-C-03) は同一 (`internal/daemon/daemon.go:3-20` の `TODO(refactor)`) → 1 件に統合 (Low)
  - F-018 候補 (旧 02-F-04) と F-029 候補 (旧 03-A-3) は同一 (`task_heartbeat_handler.go:202` の `recordSelfWrite` 抜け) → 1 件に統合 (上位重要度 High を採用)

### 上位 Critical / High Finding (10 件以内)

1. **F-001 (Critical, 01)**: `worktreeManager` が `autoRecoverWorktreeManager` インターフェース実装を満たすかコード読みでは未確定 (要 build 検証)
2. **F-027 (Critical, 03)**: `advanceRepairPendingToPausedForReplan` で state→queue 逆順ロックを取得 (キャノニカル順序違反、将来デッドロック確実)
3. **F-039 (Critical, 04)**: `internal/daemon/worktree/merge_publish.go` 1361 行・100 行超関数 5 件 (関数分割急務)
4. **F-040 (Critical, 04)**: `recover.go` の `ResumeMerge` 177 行 / `mergeResolvedWorker` 146 行 (単一責務違反)
5. **F-005 (High, 01)**: `submit.go` phase fill での state lock 解放→queue 書込→再取得 TOCTOU リスク
6. **F-006 (High, 01)**: `result_write_phase_a.go` duplicate 経路で `taskRunOnIntegration` 未明示 (silent regression リスク)
7. **F-007 (High, 01)**: `r9EffectiveVerifyStallThreshold` の `configured = 0` 時に即時 stall 判定の保護なし
8. **F-018 (High, 02+03 統合)**: `task_heartbeat_handler.go:202` で `recordSelfWrite` を呼ばず、heartbeat 由来の自己書込が外部編集と誤認される
9. **F-019 (High, 02)**: lease 失効が stderr 文字列ベースで Worker に通知される (epoch mismatch のフォーマット不一致、`current_epoch` 未提供)
10. **F-026 (High, 03)**: `internal/plan/retry.go:22-33` `saveStateWithContext` の goroutine リーク + ロールバック巻き戻しリスク

---

## 2. Coverage Gaps

- **欠落セクション**: なし。期待ファイル 5 件 (01-uncommitted-diff / 02-architecture-and-agent-responsibility / 03-concurrency-bugs / 04-code-quality / 05-test-and-comments) はすべて存在し、本レポートに統合済み。

### 通知層 dead-letter 事象の記録

cmd_1777200395 中に以下の事象が発生し、本コマンドが resume された経緯がある:

- `.maestro/results/worker1.yaml` に `notify_dead_lettered=true` で残った result が存在
- エラー文字列: `tmux load-buffer (timeout): context deadline exceeded`
- リトライ上限: `notify_attempts (3) >= max_notify_attempts (3) for worker=worker1`
- 結果として planner 側に `task_result` 通知が届かず、Orchestrator が原コマンドを `lease_epoch` 増分で再配信した
- 本レポート作成タスク (task_1777204292_821e4f1881610658, lease_epoch=1) は再配信後の世代で実行されている

---

## 3. セクション別統合 Findings

各 Finding は通し番号 F-001〜F-066 で再採番。`source` は由来セクション番号 (01〜05)、`原ID` は由来セクション内での Finding ID を保持。

### 3.1 未コミット差分 (source: 01)

#### F-001

| 項目 | 値 |
|---|---|
| ID | F-001 |
| 由来 | 01 (原ID: F-01) |
| カテゴリ | 整合性 |
| 重要度 | Critical |
| 場所 | `internal/daemon/result_write_handler.go:105` 付近 |
| 現状 | `result_write_handler.go` の `worktreeManager` フィールド型が `autoRecoverWorktreeManager` 化されたが、`*WorktreeManager` がインターフェース (`AutoRecoverAfterResolution`, `ResetResolvingWorkerToConflict` 等) を充足しているかコード読みでは確定できない |
| 推奨アクション | merge 前に `go build ./...` と関連テスト (`result_write_handler_test.go`, `result_write_phase_b_lifecycle_test.go`) の green 確認。インターフェース定義箇所と `internal/daemon/worktree/manager.go` の公開メソッドを突き合わせる |

#### F-002

| 項目 | 値 |
|---|---|
| ID | F-002 |
| 由来 | 01 (原ID: F-02) |
| カテゴリ | 整合性 (確認済み) |
| 重要度 | Low |
| 場所 | `internal/agent/launcher.go:43-45,56`、`templates/instructions/orchestrator.md:44`、`templates/maestro.md:100` |
| 現状 | orchestrator.md と launcher.go の `allowedToolsByRole` (`maestro queue write planner --type command` / `maestro skill list` / `maestro plan request-cancel`) は整合 |
| 推奨アクション | 今後 sub-command 追加時は `allowedToolsByRole` と `instructions/*.md` の双方更新を忘れないこと |

#### F-003

| 項目 | 値 |
|---|---|
| ID | F-003 |
| 由来 | 01 (原ID: F-03) |
| カテゴリ | 整合性 (確認済み) |
| 重要度 | Low |
| 場所 | `templates/instructions/worker.md:411-447`、`templates/instructions/planner.md:898-919`、`internal/daemon/worktree/merge_publish.go:291-389` |
| 現状 | integration worktree での `git add`/`git commit` を Worker からはずし、Daemon 側 `stageResolvedForwardMergeFiles` に移管。ドキュメントとコードは整合 |
| 推奨アクション | `bytesContainConflictMarkers` 判定 (F-013 参照) を含むパス全体のテストを 1 件追加するのが望ましい |

#### F-004

| 項目 | 値 |
|---|---|
| ID | F-004 |
| 由来 | 01 (原ID: F-04) |
| カテゴリ | 整合性 (確認済み・反証) |
| 重要度 | Low |
| 場所 | `internal/daemon/reconcile/r9_verify_stall.go:391-403` |
| 現状 | `paused_for_replan` の dedup は実質 `(Kind, CommandID, PhaseID)` で機能する仕様通り |
| 推奨アクション | コメントで dedup の挙動を補足するとさらに保守性が上がる |

#### F-005

| 項目 | 値 |
|---|---|
| ID | F-005 |
| 由来 | 01 (原ID: F-05) |
| カテゴリ | 危険変更 (要追検証) |
| 重要度 | High |
| 場所 | `internal/plan/submit.go` 周辺 (差分 +235 行)、reload+rollback ロジック |
| 現状 | phase fill で state lock を解放→queue 書込→再取得→`reloadPhaseFillState()` で整合確認する設計。再取得時に他処理が phase 状態を変えていた場合、queue rollback が発火するが rollback 自体が失敗した場合の整合性回復経路が不明 |
| 推奨アクション | rollback 失敗時に最低限 ERROR ログを残し、`paused_for_replan` シグナル化を経由する経路を持たせる。`submit_queue_test.go` (+108 行) に rollback 失敗 assertion ケースが含まれているか確認 |

#### F-006

| 項目 | 値 |
|---|---|
| ID | F-006 |
| 由来 | 01 (原ID: F-06) |
| カテゴリ | 整合性 |
| 重要度 | High |
| 場所 | `internal/daemon/result_write_phase_a.go:91, 105, 126` |
| 現状 | duplicate 検出パスが `taskRunOnIntegration` を明示的に false に書かず、構造体 zero value 依存。実害は無いが silent regression リスク |
| 推奨アクション | `taskRunOnIntegration: false` を明示代入、または `taskRunOnIntegrationSet bool` フラグを追加 |

#### F-007

| 項目 | 値 |
|---|---|
| ID | F-007 |
| 由来 | 01 (原ID: F-07) |
| カテゴリ | 危険変更 |
| 重要度 | High |
| 場所 | `internal/daemon/reconcile/r9_verify_stall.go:296-325` (新規) |
| 現状 | configured が 0 (operator が yaml で 0 を設定) の場合、verify config が無いコマンドでは threshold = 0 のまま `r9ApplyForCommand` に渡され、即時 stall 判定になる可能性 |
| 推奨アクション | `configured <= 0` で sane minimum (例: 60s) にクランプ、または startup 時の config validation で 0 を拒否 |

#### F-008

| 項目 | 値 |
|---|---|
| ID | F-008 |
| 由来 | 01 (原ID: F-08) |
| カテゴリ | 危険変更 |
| 重要度 | Medium |
| 場所 | `internal/daemon/verify_runner_real.go` (+150 行)、`expectedPaths` 検証 |
| 現状 | `git status` 直接呼び出し追加で、worktree 外実行や非 git ディレクトリで `not a git repo` エラーで verify 全体が失敗する経路 (推定) |
| 推奨アクション | テスト `verify_runner_real_test.go` (+170 行) に「非 git ディレクトリ」ケース追加、エラー文言を `expected_paths` 検証由来であると wrap |

#### F-009

| 項目 | 値 |
|---|---|
| ID | F-009 |
| 由来 | 01 (原ID: F-09) |
| カテゴリ | 整合性 (要評価) |
| 重要度 | Medium |
| 場所 | `internal/agent/runtime_launcher_test.go` (−266 行)、`internal/agent/launcher.go:138` 付近 |
| 現状 | codex / gemini ランタイムテストが大量削除され `TestRuntimeLauncher_RejectsUnsupportedRuntimes` 1 件に統合。設計判断としては legitimate だが汎用引数ビルダ等の検証退化リスク |
| 推奨アクション | カバレッジ比較。`launcher_test.go` (+314 行) に同等検証が引き継がれているか確認 |

#### F-010

| 項目 | 値 |
|---|---|
| ID | F-010 |
| 由来 | 01 (原ID: F-10) |
| カテゴリ | コメント / 命名 |
| 重要度 | Medium |
| 場所 | `internal/daemon/daemonapi/skill.go:158, 172` |
| 現状 | 旧 `daemonSlugify` / `daemonFormatSkillMD` から prefix を外して `slugify` / `formatSkillMD` に変更。grep で旧名追跡が困難 |
| 推奨アクション | パッケージ移動を反映するなら問題なし。気になる場合は `skillSlugify` / `formatSkillMarkdown` 等 |

#### F-011

| 項目 | 値 |
|---|---|
| ID | F-011 |
| 由来 | 01 (原ID: F-11) |
| カテゴリ | 整合性 |
| 重要度 | Medium |
| 場所 | `internal/daemon/learnings/fingerprint_db.go:20` (型 `FailurePattern`) |
| 現状 | `successCount` → `SuccessCount` に exported 化、JSON tag 付与。外部書き換えで `SuccessRate` の整合が崩れる侵入面 |
| 推奨アクション | godoc コメントで warn、または setter 経由のみ受け付ける |

#### F-012

| 項目 | 値 |
|---|---|
| ID | F-012 |
| 由来 | 01 (原ID: F-12) |
| カテゴリ | 整合性 |
| 重要度 | Medium |
| 場所 | `internal/daemon/result_write_postprocess.go:38-46`、`result_write_verify_orchestration.go:448-450` |
| 現状 | `AfterVerification` 二重実行抑止 flag 設定タイミングが sync / async 経路で異なる。sync 経路の assertion が未確認 |
| 推奨アクション | sync 経路で `AfterVerification` exactly-once 呼び出しを assert する単体テスト追加 |

#### F-013

| 項目 | 値 |
|---|---|
| ID | F-013 |
| 由来 | 01 (原ID: F-13) |
| カテゴリ | 危険変更 (エッジ) |
| 重要度 | Low |
| 場所 | `internal/daemon/worktree/merge_publish.go:386-389` (新規) |
| 現状 | `\n=======` / `\n>>>>>>>` を `strings.Contains` で探すため、ファイル先頭が `=======\n` で始まる病的ケースは marker 検出に失敗 |
| 推奨アクション | `bytes.HasPrefix(data, []byte("======="))` を OR 条件に追加 |

#### F-014

| 項目 | 値 |
|---|---|
| ID | F-014 |
| 由来 | 01 (原ID: F-14) + 05 (原ID: F-C-03) ※統合 |
| カテゴリ | コメント / 未完成 |
| 重要度 | Low |
| 場所 | `internal/daemon/daemon.go:3-20` |
| 現状 | パッケージ doc に 18 行の `TODO(refactor)` 計画が常駐。godoc に出力され「未着手」の語感で最新仕様か否か不明瞭 |
| 推奨アクション | issue 番号付与 (`TODO(refactor #NNN)`) または `docs/architecture/refactor-roadmap.md` 等に移管。godoc は完了済みの設計のみ残す |

#### F-015

| 項目 | 値 |
|---|---|
| ID | F-015 |
| 由来 | 01 (原ID: F-15) |
| カテゴリ | コメント / 設定外出し |
| 重要度 | Nit |
| 場所 | `internal/agent/message_deliverer.go:30` |
| 現状 | submit 確認の捜索行数 (8 行) や retry 試行回数 (8 回)、probe 間隔 (750ms) が定数直書き |
| 推奨アクション | `model.WatcherConfig` の追加項目として外出し |

#### F-016

| 項目 | 値 |
|---|---|
| ID | F-016 |
| 由来 | 01 (原ID: F-16) |
| カテゴリ | 危険変更 (silent failure) |
| 重要度 | Nit |
| 場所 | `internal/agent/message_deliverer.go:144-147` |
| 現状 | `maxSubmitProbeAttempts` (=8) 試行後も submit 確認できなかった場合、エラー文言を一切返さず `return nil` する |
| 推奨アクション | warn ログ + `Retryable: true` のエラー返却を検討 |

---

### 3.2 設計妥当性と Agent 責務 (source: 02)

#### F-017

| 項目 | 値 |
|---|---|
| ID | F-017 |
| 由来 | 02 (原ID: F-01) |
| カテゴリ | 設計穴 |
| 重要度 | High |
| 場所 | `internal/agent/launcher.go:41-63,280-283`、`templates/instructions/orchestrator.md:1-256` |
| 現状 | Orchestrator は `command submit / status / list / cancel` のみで `plan unquarantine` / `plan resume-merge` / `plan retry-publish` を呼ぶ経路がなく、Planner も `--disallowedTools` でブロック。自然言語指示だけでは quarantine / publish 競合からの自動復旧不可、人間オペレータ必須 |
| 推奨アクション | (a) `plan unquarantine` を Planner に開放、または (b) Orchestrator に `command resume-quarantine <command_id>` 相当の高位 CLI 追加 |

#### F-018

| 項目 | 値 |
|---|---|
| ID | F-018 |
| 由来 | 02 (原ID: F-04) + 03 (原ID: A-3) ※統合 (上位重要度 High を採用) |
| カテゴリ | 閉ループ穴 / lease/fencing |
| 重要度 | High |
| 場所 | `internal/daemon/task_heartbeat_handler.go:202` (vs `result_write_phase_a.go:444,447` / `result_write_handler.go:562`) |
| 現状 | heartbeat 受領時の `yaml.AtomicWrite(queuePath, &queue)` で `recordSelfWrite` を呼んでいない。phase_a / result_write_handler は呼ぶ。fsnotify ベース self-write tracker が SHA-256 で検出する設計のため、heartbeat による queue 更新を Daemon 自身の event-bridge が外部編集と誤認しスキャンを誘発する競合が潜在 |
| 推奨アクション | `task_heartbeat_handler.go:202` の書き込み直前に `recordSelfWrite(<computed sha>)` を追加、または heartbeat 専用 write path に self-write tracker フックを共通化する `applyQueueWrite(...)` ヘルパーを追加 |

#### F-019

| 項目 | 値 |
|---|---|
| ID | F-019 |
| 由来 | 02 (原ID: F-03) |
| カテゴリ | 閉ループ穴 |
| 重要度 | High |
| 場所 | `templates/instructions/worker.md` (lease_epoch ライフサイクル節)、`internal/daemon/lease/manager.go:138-184` |
| 現状 | epoch 失効を能動照会する API 未提供。Worker は heartbeat / result write の stderr 文字列をパースして黙ってターン終了。stderr フォーマットが heartbeat (`task <id> epoch mismatch: queue=<N>, request=<M>`) と result write (`task <id> lease_epoch mismatch: queue=<N>, request=<M>`) で微妙に異なりテキストマッチが脆弱。失効後の状況 (再 dispatch / quarantine) を Worker は知れない |
| 推奨アクション | fencing エラー応答に `current_epoch` と `current_status` (`RECLAIMED_BY_OTHER` / `MAX_RUNTIME_EXCEEDED` / `QUARANTINED`) を構造化 JSON で同梱。CLI が機械可読 exit code を吐き分け、Worker 判定を文字列 grep から exit code 比較に置き換える |

#### F-020

| 項目 | 値 |
|---|---|
| ID | F-020 |
| 由来 | 02 (原ID: F-05) |
| カテゴリ | 手動介入残 |
| 重要度 | High |
| 場所 | `templates/instructions/planner.md:240,857`、`internal/agent/launcher.go:280-283` |
| 現状 | quarantine 解除 / publish 競合解決 / commit_failed エスカレーションは Orchestrator にも Planner にも CLI 経路なし。自然言語で「再開して」と指示しても自律復旧できない |
| 推奨アクション | quarantine の根本原因ごとに Planner が安全に解除できる条件を定義、(a) 自動解除は Daemon scan_orchestrator、(b) 残る人間判断は Orchestrator 用 CLI として表面化、の二段で縮退 |

#### F-021

| 項目 | 値 |
|---|---|
| ID | F-021 |
| 由来 | 02 (原ID: F-11) |
| カテゴリ | Agent 過剰責務 |
| 重要度 | High |
| 場所 | `templates/instructions/worker.md` (lease_epoch ライフサイクル節)、`templates/maestro.md` (Agent 責務原則) |
| 現状 | maestro.md は「Agent はビジネスロジックのみ」と原則を掲げる一方、worker.md は (a) `FENCING_REJECT` の機械的ハンドル、(b) stderr の `lease_epoch mismatch` 文字列を判定して `--no-retry-safe --partial-changes` を付けず黙ってターン終了、(c) `MAX_RUNTIME_EXCEEDED` を heartbeat で受領したら作業中断、の 3 種を Worker LLM 判断に委ねている |
| 推奨アクション | Daemon 側で構造化 status (`TERMINATE_SILENTLY` / `TERMINATE_WITH_PARTIAL_REPORT` / `RETRY_WITH_NEW_EPOCH`) として返し、Worker は status マッピング表で判断する形に統一。F-019 と一体で対処 |

#### F-022

| 項目 | 値 |
|---|---|
| ID | F-022 |
| 由来 | 02 (原ID: F-02) |
| カテゴリ | 設計穴 |
| 重要度 | Medium |
| 場所 | `templates/instructions/planner.md:1018-1052`、`internal/plan/validate.go:307-320`、`internal/plan/retry.go:125-126,317` |
| 現状 | `expected_paths` / `definition_of_abort` は必須化されたが `persona_hint` / `skill_refs` は任意のまま。planner.md は「ペルソナ前提」を要求しており、必須/任意の境界が文章とスキーマで不一致 |
| 推奨アクション | (a) `persona_hint` を validate.go で必須化、または (b) 省略時 Planner 自身が `implementer` をデフォルト設定する旨を planner.md に明記 |

#### F-023

| 項目 | 値 |
|---|---|
| ID | F-023 |
| 由来 | 02 (原ID: F-06) |
| カテゴリ | 手動介入残 |
| 重要度 | Medium |
| 場所 | `templates/instructions/planner.md` Phase 0 verify 節、`internal/plan/submit.go:78-83,98-116` |
| 現状 | Phase 0 で verify config snapshot を `maestro verify write` で先に書き込んでから plan submit する手順が Planner プロンプトでしか強制されていない。Daemon は自動的に「verify ステップを差し込む」フォローをしない |
| 推奨アクション | submit.go 側で snapshot 不在時に (a) `verify-bootstrap` 専用タスクを自動挿入、または (b) reject エラーで CLI ヒントを返す |

#### F-024

| 項目 | 値 |
|---|---|
| ID | F-024 |
| 由来 | 02 (原ID: F-07) |
| カテゴリ | instructions 乖離 |
| 重要度 | Medium |
| 場所 | `templates/instructions/planner.md:1018-1052` (包括例) と他抜粋ブロック |
| 現状 | `expected_paths` / `definition_of_abort` は包括 YAML 例には記載済みだが、衝突解決例や shell quoting 例の抜粋ブロックには未記載。Planner LLM が抜粋ブロックを参考にすると validate.go で reject される |
| 推奨アクション | planner.md 内のすべての YAML 抜粋に必須フィールド明示、または「§包括例参照」と注記 |

#### F-025

| 項目 | 値 |
|---|---|
| ID | F-025 |
| 由来 | 02 (原ID: F-09) |
| カテゴリ | Daemon 吸収すべき |
| 重要度 | Medium |
| 場所 | `internal/agent/policy_checker.go:121-690,72-244` |
| 現状 | 600 行超の安全ポリシー (D001-D012, B001-B004, M-AGT1, RUN_ON_MAIN, WT-GIT, WT001) が bash 正規表現として Worker サイドに毎回書き出される。Go 側 `internal/validate` と二重保守、shell escaping のバグが安全網の穴に |
| 推奨アクション | (a) Tier1/Tier2 判定を Go の `internal/validate/policy.go` に集約し、PreToolUse hook は Go バイナリを呼ぶラッパに痩せさせる、(b) 検査結果を構造化 JSON で記録し scan_orchestrator が後追いで監査 |

#### F-026

| 項目 | 値 |
|---|---|
| ID | F-026 |
| 由来 | 02 (原ID: F-10) |
| カテゴリ | Daemon 吸収すべき |
| 重要度 | Medium |
| 場所 | `internal/agent/policy_checker.go:159-176` |
| 現状 | Worker が main ブランチ直接編集を許されているか否かは tmux user-variable `@run_on_main` で hook に通知される。tmux 障害 / pane 再生成のレースで hook がフラグを取得できず fail-closed (=拒否) になり、Worker が編集できなくなる |
| 推奨アクション | 配信タスクの引数 (環境変数 / プロンプト先頭の structured field) として `MAESTRO_RUN_ON_MAIN` を Daemon が直接渡す。tmux は表示用途に限定 |

#### F-027

| 項目 | 値 |
|---|---|
| ID | F-027 |
| 由来 | 02 (原ID: F-08) |
| カテゴリ | instructions 乖離 |
| 重要度 | Low |
| 場所 | `templates/instructions/worker.md`、`templates/maestro.md` Tier3、`internal/agent/policy_checker.go:354-356` |
| 現状 | maestro.md Tier3 では `--force-with-lease` を安全代替として記載しているが、Worker には適用外と worker.md 側で打ち消し、policy_checker B003 も `--force-with-lease` を含む全面拒否を実装。共通プロンプト→Worker 専用プロンプトの上書き関係が読みづらい |
| 推奨アクション | maestro.md Tier3 の代替表に「※ Worker には適用外」と注記、policy_checker B003 拒否文言にも明示 |

---

### 3.3 並行性・状態遷移・I/O (source: 03)

#### F-028

| 項目 | 値 |
|---|---|
| ID | F-028 |
| 由来 | 03 (原ID: D-1) |
| カテゴリ | ファイルロック / デッドロック予兆 |
| 重要度 | Critical |
| 場所 | `internal/daemon/result_write_handler.go:570-595` (`advanceRepairPendingToPausedForReplan`) と `:597-635` (`emitPausedForReplanPlannerSignal`) |
| 現状 | `state:{commandID}` 取得 (L572) → 保持中に `emitPausedForReplanPlannerSignal` を呼び (L594) → 内部で `queue:planner_signals` 取得 (L612)。`internal/daemon/doc.go:13` のキャノニカル順序 `queue → state → result` の完全逆順。現状デッドロック相手は無いが将来コード追加で確実に踏む時限爆弾 |
| 推奨アクション | 2 段階に分割: (1) state ロック内でスナップショット → 解放、(2) `queue:planner_signals` 取得 → 書込。R1 reconciler (`r1_result_queue.go:107-202`) と同パターン |

#### F-029

| 項目 | 値 |
|---|---|
| ID | F-029 |
| 由来 | 03 (原ID: F-1) |
| カテゴリ | レース |
| 重要度 | High |
| 場所 | `internal/plan/retry.go:22-33` (`saveStateWithContext`) |
| 現状 | ctx タイムアウト時に goroutine を野放しにし、後続の `restoreStateOrLog` リカバリと書き込み順序が逆転して新しい状態を破棄するリスク。NFS 等で AtomicWrite が長時間ハングし、`stateSaveTimeout=30s` を超えた後にハング解放した際、(1) 復旧 SaveState → (2) ハング解放後の元 SaveState の順で **新しい状態が後勝ちで永続化** される |
| 推奨アクション | `saveStateWithContext` を `sync.Once` + `atomic.Int32` で保護し、タイムアウト後に goroutine が完走しても永続化しない (temp file unlink) ようにする。または `SaveState` に ctx を受けるフックを追加 |

#### F-030

| 項目 | 値 |
|---|---|
| ID | F-030 |
| 由来 | 03 (原ID: C-1) + (G-1 連動) |
| カテゴリ | YAML I/O / 復旧整合 |
| 重要度 | High |
| 場所 | `internal/plan/state.go:172` (`DeleteState`) / `internal/daemon/reconcile/r0_planning_stuck.go:114` (`os.Remove(statePath)`) |
| 現状 | 状態ファイル削除時に兄弟 `<state>.bak` を削除しない。`AtomicWriteRaw` は書き込みごとに `.bak` を作成するため、同一 commandID で再度状態が作成された後に YAML が破損すると `state_recovery.go:41-96` が古い `.bak` を「正当な前世代」として復元、ORC-3 epoch クランプでも整合できない論理状態に陥る (タスク存在/不在の時系列逆転) |
| 推奨アクション | `DeleteState` 内で `os.Remove(statePath + ".bak")` を best-effort 実施。R0 の `os.Remove` も同様。テスト `internal/plan/state_test.go:100` の `TestStateManager_DeleteState` を更新して `.bak` 不在 assert |

#### F-031

| 項目 | 値 |
|---|---|
| ID | F-031 |
| 由来 | 03 (原ID: G-1) |
| カテゴリ | 復旧整合 |
| 重要度 | High |
| 場所 | `internal/daemon/state_recovery.go:41-96` + `internal/plan/state.go:172` |
| 現状 | `recoverStateDir` は corrupted state を `.bak` から復元し、`LeaseEpoch` を「既存値の最大値 + 1」にクランプする (ORC-3, L186-202)。`DeleteState` が `.bak` を残置するため、commandID 再利用時に前世代 `.bak` が ORC-3 クランプ対象として参照され、本来到達不能な lease_epoch を再注入する。Worker が保持する epoch=1 は永久 stale となり、その時点で in_progress タスクは fencing_reject ループ |
| 推奨アクション | F-030 の修正で根本解決。加えて `recoverStateDir` で `.bak` の `created_at` / `command_id` を YAML 中身でも検証し、現行状態の commandID と不一致なら `.bak` を無視 |

#### F-032

| 項目 | 値 |
|---|---|
| ID | F-032 |
| 由来 | 03 (原ID: A-2) |
| カテゴリ | lease/fencing |
| 重要度 | Medium |
| 場所 | `internal/daemon/queue_scan_collect.go:417-421` (`checkInProgressDependencyFailuresDeferred`) |
| 現状 | `in_progress` の依存先タスクを cancelled に書き換える際、`task.LeaseEpoch` は維持される一方 `releaseLease` が呼ばれず `LeaseManager` の内部メトリクス・H5 監査ログが更新されない。後続 Worker が同じ epoch で heartbeat を投げると queue 上は cancelled となっており、`applyTaskDispatchResult` 系には遷移検証もない |
| 推奨アクション | `releaseLease(task)` を呼んで一本化。`result_write_phase_a.go:431` の terminal 設定前に `model.ValidateCommandTaskQueueTransition` |

#### F-033

| 項目 | 値 |
|---|---|
| ID | F-033 |
| 由来 | 03 (原ID: B-1) |
| カテゴリ | 状態遷移 |
| 重要度 | Medium |
| 場所 | `internal/daemon/queue_scan_collect.go:417` |
| 現状 | `task.Status = model.StatusCancelled` を直接代入し、`validCommandTaskQueueTransitions` の検証を通さない。`scanMu.Lock` を保持していないため別 goroutine の更新を見逃し、レアケースで pending・cancelled・completed のタスクを cancelled に上書きする可能性 |
| 推奨アクション | `model.ValidateCommandTaskQueueTransition(task.Status, model.StatusCancelled)` を呼び、不正遷移はスキップ (warn ログ)。走査時点の status を関数内で再検査 |

#### F-034

| 項目 | 値 |
|---|---|
| ID | F-034 |
| 由来 | 03 (原ID: C-2) |
| カテゴリ | YAML I/O / 復旧整合 |
| 重要度 | Medium |
| 場所 | `internal/daemon/state_recovery.go:41-96` (vs `.maestro/queues/`) |
| 現状 | 状態 YAML には `.bak` リカバリ + ORC-3 epoch floor clamp があるが、queue YAML には対応する `recoverQueueDir` が存在しない。queue ファイルが起動時に部分書き込み (truncated) になっていた場合、daemon は空キュー扱いで起動するため queue 上の `lease_epoch` を含む全タスク履歴が消滅 |
| 推奨アクション | queue ファイルにも corrupted 検出 → `.bak` 復元 → epoch floor clamp を実装。最低でも起動時の queue 破損を fatal にして手動リカバリを強制する選択肢 |

#### F-035

| 項目 | 値 |
|---|---|
| ID | F-035 |
| 由来 | 03 (原ID: A-1) |
| カテゴリ | lease/fencing |
| 重要度 | Low |
| 場所 | `internal/daemon/result_write_phase_a.go:430-434` |
| 現状 | terminal status 遷移時に `LeaseOwner` / `LeaseExpiresAt` をクリアするが、`LeaseEpoch` は残置。Phase A は `releaseLease` を経由せず直接構造体を書き換える規約バイパス。運用上の不整合は無いがコード規約は崩れる |
| 推奨アクション | `lease.Manager.releaseLease` を呼び出すように統一、または `// Phase A bypasses leaseManager intentionally` コメントで意図を明示 |

#### F-036

| 項目 | 値 |
|---|---|
| ID | F-036 |
| 由来 | 03 (原ID: B-2) |
| カテゴリ | 状態遷移 |
| 重要度 | Low |
| 場所 | `internal/daemon/result_write_phase_a.go:429-449` |
| 現状 | terminal status 代入前に遷移検証を行わない。`completed/failed/cancelled/dead_letter` への遷移はすべて `in_progress` 起点のみ valid だが、Phase A 到達時に queue 上のタスクが `pending` 等に戻っていた場合 (R1 reconciler 干渉直後) でも上書きされる |
| 推奨アクション | `model.ValidateCommandTaskQueueTransition(queueTask.Status, resultStatus)` を呼び、無効遷移は warn ログ + queue 書き込みスキップ (result file は既にコミット済みなので reconciler に任せる) |

#### F-037

| 項目 | 値 |
|---|---|
| ID | F-037 |
| 由来 | 03 (原ID: D-2) |
| カテゴリ | ファイルロック |
| 重要度 | Low |
| 場所 | `internal/lock/lock.go:192-228,245-250` |
| 現状 | `FileLock` は flock ベースで取得・解放するが、ロックファイル自体は意図的に削除しない。長時間運用で `.maestro/locks/` 配下が単調増加し inode 枯渇を招く可能性 (推定) |
| 推奨アクション | 起動時 / 定期 GC で 7 日以上アクセスのないロックファイルを削除。flock NB 取得が成功した時点で削除する 2 段階処理 |

---

### 3.4 コード品質・デッドコード・リファクタリング (source: 04)

#### F-038

| 項目 | 値 |
|---|---|
| ID | F-038 |
| 由来 | 04 (原ID: A-1) |
| カテゴリ | 重複 |
| 重要度 | High |
| 場所 | `internal/testutil/clock.go:9`, `internal/metrics/handler.go:17`, `internal/daemon/core/core.go:149` |
| 現状 | 同一概念 `Clock` インターフェースが 3 箇所で独立定義。`testutil` 版は `Since()` を追加した拡張シグネチャ。利用側でアダプタ or 型変換が必要 |
| 推奨アクション | `internal/daemon/core/core.go` を SSOT とし、他 2 箇所はインポート参照。拡張版は埋め込みインターフェースで表現 |

#### F-039

| 項目 | 値 |
|---|---|
| ID | F-039 |
| 由来 | 04 (原ID: A-2) |
| カテゴリ | 重複 |
| 重要度 | High |
| 場所 | `internal/plan/worker_assign.go:30`, `internal/daemon/core/core.go:249` |
| 現状 | `plan` と `daemon/core` で `ModelSelector` 二重定義。bridge パッケージで型変換 (推定) |
| 推奨アクション | `core` を正本とし `plan` は import 参照、または `internal/contract` に切り出し |

#### F-040

| 項目 | 値 |
|---|---|
| ID | F-040 |
| 由来 | 04 (原ID: C-1) |
| カテゴリ | 責務過多 |
| 重要度 | Critical |
| 場所 | `internal/daemon/worktree/merge_publish.go` (1361 行、100 行超関数 5 件) |
| 現状 | `SyncFromIntegration` (140 行)、`forwardMergeBaseToIntegration` (110 行 / L152)、`performPublishMerge` (104 行)、`mergeWorkerBranch` (98 行)、`MergeToIntegration` (93 行 / L435)、`PublishToBase` (88 行)。git マージ・パブリッシュ・競合解決の責務が密集 |
| 推奨アクション | (1) `forwardMergeBaseToIntegration` 系を `merge_forward.go` に分離、(2) `performPublishMerge` を staging / commit / verify の 3 段に分割、(3) ネスト 3 段以上を early-return で平坦化 |

#### F-041

| 項目 | 値 |
|---|---|
| ID | F-041 |
| 由来 | 04 (原ID: C-2) |
| カテゴリ | 責務過多 |
| 重要度 | Critical |
| 場所 | `internal/daemon/worktree/recover.go: ResumeMerge` (177 行), `mergeResolvedWorker` (146 行), `AutoRecoverAfterResolution` (96 行) |
| 現状 | `ResumeMerge` は単一関数で 177 行ありワーカー復旧・統合ツリー復旧・状態遷移判定が一体化。プロジェクトで最大の関数 |
| 推奨アクション | 状態判定 → 解決済みワーカー再マージ → 競合検査 → 完了通知の 4 ステップに分割 |

#### F-042

| 項目 | 値 |
|---|---|
| ID | F-042 |
| 由来 | 04 (原ID: C-3) |
| カテゴリ | 責務過多 |
| 重要度 | High |
| 場所 | `internal/daemon/result_handler.go` (1110 行) |
| 現状 | `writeNotificationToOrchestratorQueue` (78 行), `recordTaskResultLearning` (78 行), `sweepExhaustedNotifications` (75 行 ジェネリック), `ScanAllResults` (69 行), `processCommandResultFile` (61 行)。リース取得・通知・学習レコード・スキャン責務が混在 |
| 推奨アクション | Phase1 (リース取得)、Phase3 (結果反映)、通知、学習レコードの 4 ファイルに分割。`*_phase1.go` のような命名 |

#### F-043

| 項目 | 値 |
|---|---|
| ID | F-043 |
| 由来 | 04 (原ID: F-1) |
| カテゴリ | 過大ファイル |
| 重要度 | High |
| 場所 | 行数上位 10 ファイル (>485 行) |
| 現状 | 1361 (merge_publish.go) / 1110 (result_handler.go) / 1043 (recover.go) / 880 (worktree/manager.go) / 864 (tmux/session.go) / 795 (launcher.go) / 705 (cleanup_gc.go) / 690 (policy_checker.go) / 674 (quality/engine.go) / 665 (executor_core.go) |
| 推奨アクション | 500 行超は責務分割を検討。1000 行超 3 件は最優先 |

#### F-044

| 項目 | 値 |
|---|---|
| ID | F-044 |
| 由来 | 04 (原ID: C-4) |
| カテゴリ | 責務過多 |
| 重要度 | Medium |
| 場所 | `internal/agent/launcher.go: buildLaunchArgs` (95 行), `Launch` (67 行) |
| 現状 | `buildLaunchArgs` は role / model / system prompt / base prompt mode から CLI 引数列を組み立てる関数で 95 行。条件分岐多数 |
| 推奨アクション | role 別の小さな builder 関数に分割、または builder pattern (`AgentLaunchArgs` + `Build()`) |

#### F-045

| 項目 | 値 |
|---|---|
| ID | F-045 |
| 由来 | 04 (原ID: D-1) |
| カテゴリ | 循環依存 |
| 重要度 | Medium |
| 場所 | `internal/plan/worker_assign.go:30`、`internal/daemon/core/core.go:249` |
| 現状 | (推定) `plan` と `daemon/core` は同名インターフェースを独立定義し、`bridge` パッケージで型変換。コンパイル可能だが変更時の型変換ロジック保守コスト発生 |
| 推奨アクション | 共通インターフェース定義を `internal/contract` に集約、`bridge` のアダプタ層を簡素化 |

#### F-046

| 項目 | 値 |
|---|---|
| ID | F-046 |
| 由来 | 04 (原ID: E-1) |
| カテゴリ | ネスト |
| 重要度 | Medium |
| 場所 | `internal/daemon/worktree/merge_publish.go:152-263` (`forwardMergeBaseToIntegration` ほか) |
| 現状 | (推定) `if integrationHasMergeHead → reuseInFlightForwardMerge → if err → if done` の 3-4 段ネストが複数関数に存在 |
| 推奨アクション | early return / guard clause で段数を 2 段以下に削減。状態判定を別関数に切り出し |

#### F-047

| 項目 | 値 |
|---|---|
| ID | F-047 |
| 由来 | 04 (原ID: E-2) |
| カテゴリ | ネスト |
| 重要度 | Medium |
| 場所 | `internal/agent/executor_core.go` (665 行) |
| 現状 | (推定) `execWithClear` 等で switch + if のネスト構造が深い |
| 推奨アクション | 状態遷移テーブル化、または handler 関数群への分散 |

#### F-048

| 項目 | 値 |
|---|---|
| ID | F-048 |
| 由来 | 04 (原ID: B-2) |
| カテゴリ | 古い設定 |
| 重要度 | Low |
| 場所 | `templates/config.yaml`, `internal/model/config*.go` |
| 現状 | (推定) `templates/config.yaml` に `admission_control` / `review` / `rollout` / `judge` 等の未接続セクションがコメントアウト形式で残存 |
| 推奨アクション | 未実装セクションには `# EXPERIMENTAL: not yet implemented` コメント、または実装が進むまで削除 |

#### F-049

| 項目 | 値 |
|---|---|
| ID | F-049 |
| 由来 | 04 (原ID: G-1) |
| カテゴリ | 命名 |
| 重要度 | Low |
| 場所 | プロジェクト全体 |
| 現状 | (推定) 状態管理に `Status` / `State` / `Phase` の 3 用語が併用。意味の境界 (例: `Status` = user-facing、`State` = internal、`Phase` = pipeline 段階) は一貫しているように見えるが明文化なし |
| 推奨アクション | `docs/naming-convention.md` で用語境界を明文化。Linter ルールで逸脱検出 |

#### F-050

| 項目 | 値 |
|---|---|
| ID | F-050 |
| 由来 | 04 (原ID: A-3) |
| カテゴリ | 重複 |
| 重要度 | Nit |
| 場所 | `templates/maestro.md` (114 行), `templates/instructions/orchestrator.md` (255 行), `planner.md` (1182 行), `worker.md` (482 行) |
| 現状 | 各 instructions ファイルが「破壊的操作の安全規則」「Worker Bash / ツール制約」等の見出しを持ち、内容を maestro.md に集約する形式は取れているが、見出しと冒頭文言の繰り返しが Role 跨ぎで残存 |
| 推奨アクション | 既存方針 (maestro.md = SSOT) を維持。Role 共通の Tier1-3 表は instructions 側から重複しないよう監査ルールを CI に追加 |

#### F-051

| 項目 | 値 |
|---|---|
| ID | F-051 |
| 由来 | 04 (原ID: G-2) |
| カテゴリ | 命名 (反証) |
| 重要度 | Nit |
| 場所 | — |
| 現状 | Explore SubAgent は `Cfg` 型混在を指摘したが Grep `^type [A-Z][a-zA-Z]*Cfg\b` で 0 件。実際は変数名 `cfg` のみ、型名は `Config` で統一 |
| 推奨アクション | 不要 (誤検知の記録) |

---

### 3.5 テスト品質・コメント (source: 05)

#### F-052

| 項目 | 値 |
|---|---|
| ID | F-052 |
| 由来 | 05 (原ID: F-T-01) |
| カテゴリ | 無駄テスト / テスト hack |
| 重要度 | High |
| 場所 | `internal/plan/state_test.go:735-784` |
| 現状 | `currentSchemaVersion` が `const = 1` のため移行ロジック自体は呼ばれない。コメントに「Since currentSchemaVersion is const=1, NeedsMigration(1) returns false」と自認しているが、テスト名は「Migration_OlderVersion」のまま。`origMigrator`/`testMigrator` 差し替えは実行されるが LoadState に影響しない |
| 推奨アクション | 名称を `TestLoadState_NoMigrationNeededAtCurrentVersion` 等に変更、または `migrator_test.go` でカバーした上で本テストは削除。スキーマ v2 導入時に migration テストを書く |

#### F-053

| 項目 | 値 |
|---|---|
| ID | F-053 |
| 由来 | 05 (原ID: F-T-02) |
| カテゴリ | 不安定要素 / assertion 不足 |
| 重要度 | Medium |
| 場所 | `internal/agent/policy_checker_test.go` (42 件、特に L289-365, L1813-1823) |
| 現状 | hook script のソース断片 (例: ``\b(bash|sh)\s+-[a-zA-Z]*c\b``) を `strings.Contains` で検証する実装一致テスト。リファクタで等価 regex に書き換えると意味なく落ちる。L289-313 はループ中の `blocked` 値を実行に使わず L307-312 の if ブロック中身が空でデッドコード |
| 推奨アクション | `requireJq(t)` 経由で実 hook を起動する S シリーズのテストに統合し、`strings.Contains` 系は冗長として削除。L307-312 の空条件は削除 |

#### F-054

| 項目 | 値 |
|---|---|
| ID | F-054 |
| 由来 | 05 (原ID: F-T-03) |
| カテゴリ | テスト hack |
| 重要度 | Medium |
| 場所 | `internal/daemon/panic_recovery_test.go:51-53,121-122`, `integration_test.go:1635-1637` |
| 現状 | パニックリカバリは実時間 5 秒以内で完了する想定 (`panicRecoveryTimeout = 5s`) であり「short モードで重い」根拠が乏しい。CI ランナーが `-short` を付けると panic recovery が走らずカバレッジが落ちる |
| 推奨アクション | panic recovery 系は `testing.Short()` ガードを外す、または `-short` で skip する明確な閾値 (例: 1s 超) を導入してコメントに記載 |

#### F-055

| 項目 | 値 |
|---|---|
| ID | F-055 |
| 由来 | 05 (原ID: F-T-04) |
| カテゴリ | 不安定要素 |
| 重要度 | Medium |
| 場所 | `internal/uds/uds_test.go:852`, `internal/daemon/integration_test.go:1696,1919-1920`, `internal/daemon/event_bridge_timeout_test.go:103`, `internal/daemon/spawn_tracked_test.go:70` |
| 現状 | `time.Sleep` を並行同期に使用 (50ms / 5ms × 6 等)。CI 高負荷で flake。debounce_controller_test.go:73 では「replacing non-deterministic time.Sleep with deterministic signaling」と置換方針が確立されているが他箇所未適用 |
| 推奨アクション | `<-readyCh` / `require.Eventually` / fake clock に置換 |

#### F-056

| 項目 | 値 |
|---|---|
| ID | F-056 |
| 由来 | 05 (原ID: F-C-01) |
| カテゴリ | TODO 放置 |
| 重要度 | Medium |
| 場所 | `internal/plan/state_test.go:1-5`, `internal/daemon/daemon_test.go:1-9` |
| 現状 | 「lack dedicated unit tests」「lack unit test coverage. Priority order」が package コメント先頭に列挙されているが、issue 番号も担当者も日付も無い。state_reader はテスト済みなど一部は古い記述 |
| 推奨アクション | issue 化して `TODO(coverage #NNN)` を残すか、TASKS.md / GitHub issue に移管しコメント削除 |

#### F-057

| 項目 | 値 |
|---|---|
| ID | F-057 |
| 由来 | 05 (原ID: F-T-05) |
| カテゴリ | 不安定要素 |
| 重要度 | Low |
| 場所 | `internal/agent/message_deliverer_test.go:467-476` |
| 現状 | `elapsed < 3*time.Millisecond` でしか検証しない。1ms 単位閾値はスケジューラ揺らぎで偽陽性、期待値 4ms に対して 3ms 下限のみで上限なし、指数増加性は実質テストしていない |
| 推奨アクション | base interval を 50-100ms に上げる、または呼び出し回数とバックオフ計算式を直接検証するテストに置き換え |

#### F-058

| 項目 | 値 |
|---|---|
| ID | F-058 |
| 由来 | 05 (原ID: F-T-06) |
| カテゴリ | 無駄テスト (重複) |
| 重要度 | Low |
| 場所 | `internal/daemon/queue_scan_phase_test.go:27-153` |
| 現状 | `TestCheckCommandTasksTerminal_*` が 7 関数並ぶ同一構造。総行数 130 行弱だが table-driven なら 30 行 |
| 推奨アクション | `tests := []struct{ name string; tasks ...; wantTerminal, wantFailed bool }` で集約 |

#### F-059

| 項目 | 値 |
|---|---|
| ID | F-059 |
| 由来 | 05 (原ID: F-T-07) |
| カテゴリ | 無駄テスト (冗長) |
| 重要度 | Low |
| 場所 | `internal/agent/executor_test.go:39-103` |
| 現状 | 既定値検証 8 フィールド × 2 ケースを 16 連の if で展開。フィールド追加時の保守コスト増 |
| 推奨アクション | フィールド名と期待値のテーブル + `reflect.ValueOf(cfg).FieldByName(name)` による table-driven、または構造体 deep equal |

#### F-060

| 項目 | 値 |
|---|---|
| ID | F-060 |
| 由来 | 05 (原ID: F-T-08) |
| カテゴリ | 不安定要素 |
| 重要度 | Low |
| 場所 | `internal/daemon/daemon_startup_test.go:114`, `internal/plan/retry_test.go:1528` |
| 現状 | sleep window を超えるたびにテストが伸びる。retry_test.go では 150ms 固定 sleep が決定論的だが時間コストが高い |
| 推奨アクション | clock モックに置き換え (sliding-window スロットリング側で fake clock を受け付ける小改修) |

#### F-061

| 項目 | 値 |
|---|---|
| ID | F-061 |
| 由来 | 05 (原ID: F-C-02) |
| カテゴリ | TODO 放置 |
| 重要度 | Low |
| 場所 | `internal/daemon/queue_handler_test.go:24-27`, `internal/daemon/worktree_test_helper_test.go:14-17` |
| 現状 | `Target: internal/daemon/testhelper_test.go` 等の移送先は書かれているが「Prerequisite: daemon test suite structure stabilization」の到達条件が抽象的 |
| 推奨アクション | trigger 条件 (例: テストファイル数が N 以下に減ったら / refactor フェーズ X 完了後) を明文化、または issue 化 |

#### F-062

| 項目 | 値 |
|---|---|
| ID | F-062 |
| 由来 | 05 (原ID: F-C-06) |
| カテゴリ | 言語混在 |
| 重要度 | Low |
| 場所 | 20 ファイル (`internal/model/fingerprint.go`, `internal/model/queue.go:138-189`, `cmd/maestro/cmd_result.go:50-112`, `internal/daemon/quality_gate.go:202`, `internal/lock/lock_order_enabled.go:37` 等) |
| 現状 | パッケージ単位ではほぼ統一されているが、`cmd_result.go` のように同一ファイル内で関数 doc は英語・補足は日本語とミックスする例。godoc 出力の言語が不統一 |
| 推奨アクション | プロジェクト全体方針 (godoc 公開部は英語、日本語は実装ノートに限定) を `CONTRIBUTING.md` で明記し段階的に統一 |

#### F-063

| 項目 | 値 |
|---|---|
| ID | F-063 |
| 由来 | 05 (原ID: F-T-09) |
| カテゴリ | テスト hack |
| 重要度 | Nit |
| 場所 | `internal/uds/uds_fuzz_test.go:35`, `internal/plan/state_fuzz_test.go:49` |
| 現状 | `t.Skip()` 引数なし。skip 理由が明示されておらずコード読まないと分からない |
| 推奨アクション | `t.Skip("invalid seed: <reason>")` と書く |

#### F-064

| 項目 | 値 |
|---|---|
| ID | F-064 |
| 由来 | 05 (原ID: F-C-04) |
| カテゴリ | TODO 放置 |
| 重要度 | Nit |
| 場所 | `internal/plan/migrator.go:60-80` |
| 現状 | `// TODO(schema): Register migration steps when currentSchemaVersion is bumped above 1.` の直下に「Status: currentSchemaVersion=1, no migrations registered yet.」と書かれており TODO というよりも将来手順の記録。F-052 のテストと連動して読み手を混乱させる |
| 推奨アクション | `TODO` ではなく `// Migration procedure (when bumping currentSchemaVersion):` に書き換え |

#### F-065

| 項目 | 値 |
|---|---|
| ID | F-065 |
| 由来 | 05 (原ID: F-C-05) |
| カテゴリ | 自明コメント |
| 重要度 | Nit |
| 場所 | `internal/daemon/quality_gate_test.go:102,113,117,479,514` |
| 現状 | `// Initialize the gate engine` の直後 `qg.loadGateDefinitions()`、`// Start the daemon` → `qg.Start()` 等、メソッド名と完全に重複 |
| 推奨アクション | 削除。意味的に追加すべきは「Why」(なぜここで Start するか) のみ |

#### F-066

| 項目 | 値 |
|---|---|
| ID | F-066 |
| 由来 | 05 (原ID: F-C-07) |
| カテゴリ | 自明コメント (誤検知防御) |
| 重要度 | Nit |
| 場所 | `internal/model/runtime_test.go:28`, `internal/daemon/learnings_test.go:664`, `cmd/maestro/cmd_queue_test.go:14` |
| 現状 | `"geminiXXX"` / `"AAAAAXXX"` / `"deprecated"` はテスト用ペイロードであり TODO ではない。Grep で `XXX|deprecated` をかけるとヒットし将来の自動化 grep の混乱要因 |
| 推奨アクション | 必要なら中立な名前に置換 (優先度低) |

---

## 4. 総評

### 4.1 全体傾向

本リポジトリは「Daemon が Single Source of Truth、Agent はビジネスロジック専念」という maestro.md の原則を概ね保持しており、未コミット差分 132 ファイルの大半は UDS API 層分離 (`internal/daemon/daemonapi/` 新設) と verify サブシステム強化という直交した 2 系統のリファクタで占められている。テスト本数 (247 ファイル)、`t.Parallel()` の活用、決定論的時刻、worktree モード対応など基本品質は高い。Critical 級の即時障害は 4 件中 3 件が「現時点では潜在的だが将来確実に踏む時限爆弾」(F-001 のビルド未検証、F-028 のロック逆順、F-040/F-041 の超大型関数) である。

一方で、(a) **閉ループの穴** が複数残されており、特に lease epoch 失効通知が stderr 文字列に依存し Worker 側の文字列マッチ判定が脆い (F-019, F-021)、heartbeat 経路で `recordSelfWrite` が抜けて自己書込が外部編集と誤認される (F-018) など、Daemon 側の最終整合保証が「偶然動いている」状態の箇所が散見される。(b) **手動介入残** として quarantine / publish 競合解決が Orchestrator にも Planner にも CLI 経路を持たず、人間オペレータに依存する (F-017, F-020, F-023)。(c) **Daemon 吸収すべきロジック** として 600 行超の bash 安全ポリシー hook (F-025) と tmux user-variable 経由の RUN_ON_MAIN フラグ伝播 (F-026) が挙げられる。

コード品質面では `merge_publish.go` 1361 行 / `recover.go` 1043 行 / `result_handler.go` 1110 行の三大ファイルが分割未着手で残っており、`Clock` / `ModelSelector` インターフェースの二重・三重定義 (F-038, F-039) が `bridge` 層の保守コストを生んでいる。テスト面では `policy_checker_test.go` の 42 件の `strings.Contains(hookScript, ...)` (F-053) が実装一致テストになっており、リファクタ阻害要因。

### 4.2 最優先で対処すべき上位項目

1. **F-001**: マージ前に `go build ./...` を必ず通す。現状 `worktreeManager` インターフェース充足はコード読みでは確定不能
2. **F-028**: `advanceRepairPendingToPausedForReplan` の state→queue 逆順ロックを R1 reconciler パターンで 2 段階分割
3. **F-018**: `task_heartbeat_handler.go:202` に `recordSelfWrite` を追加 (heartbeat 経路の自己書込誤認解消)
4. **F-029 + F-030 + F-031**: `saveStateWithContext` のロールバック巻き戻し、`DeleteState` の `.bak` 残置、ORC-3 epoch クランプによる旧世代復元の三点セットを同時に修正
5. **F-019 + F-021**: fencing エラー応答に `current_epoch` / `current_status` を構造化 JSON で同梱し、Worker 側の文字列マッチ判定を exit code 比較に置き換え
6. **F-040 + F-041 + F-043**: 1000 行超 3 ファイルの責務分割
7. **F-007**: `r9EffectiveVerifyStallThreshold` の `configured = 0` 即時 stall 判定保護

### 4.3 通知層 dead-letter 事象の現場検出ログ

本レビュー過程 (cmd_1777200395 → cmd_1777204067 への resume) で観察された通知層障害:

- `.maestro/results/worker1.yaml` に `notify_dead_lettered=true` で残った result の数と task 名: cmd_1777200395 中の result 通知が dead-letter 化 (具体的なタスク名は本レポート作成タスクのスコープ上 `.maestro/results/` を Read 不可のため外部観測としては未確定)
- エラーメッセージ: `tmux load-buffer (timeout): context deadline exceeded`
- リトライ上限: `notify_attempts (3) >= max_notify_attempts (3) for worker=worker1`
- 結果として planner 側に `task_result` 通知が届かず、Orchestrator が原コマンドを `lease_epoch` 増分で再配信
- 本レポート作成タスク (lease_epoch=1) は再配信後の世代

**運用課題**: tmux load-buffer のタイムアウトが verify timeout より優先で 3 回再試行で諦める設計は、Worker タスク完了報告という business-critical なパスに対しては保守的すぎる。具体的には:

- `notify_attempts` のリセット契機 (Daemon 再起動 / 一定時間経過後) が Worker 側からは観測できない
- dead-letter 化した result が「再 dispatch されるか」「人間オペレータに通知されるか」の判定経路が Worker から不可視
- F-019 で挙げた fencing 通知の構造化 JSON 化と同根の問題: Daemon → Agent への状態伝達が文字列ベースで脆い

### 4.4 自己評価 (本レビュー過程で発見した運用課題)

- **コンテキストリセットへの耐性**: 各タスク配信前にコンテキストがリセットされる設計のため、5 セクションを順に走破するには各 Worker が前提を独立に再構築する必要があった。先行セクション群を `tmp/maestro-review/sections/` に書き出すパターンは有効に機能した
- **Coverage Gaps の検出時系列**: 当初は cmd_1777200395 で 5 セクション分の Worker タスクが走ったが、通知層 dead-letter で結果が planner に伝わらず、Orchestrator が再配信。本レポート作成タスクの段階で `tmp/maestro-review/sections/` を検査したところ全 5 ファイルが揃っていたため、再調査は不要だった (既存セクション群を活用)
- **重複検出の精度**: 5 セクション間で 2 件の重複 Finding (F-014, F-018) を検出。設計穴 / 閉ループ穴 / 並行性 の観点が `recordSelfWrite` 抜けを別角度から指摘していた事例で、複数視点からの重複検出は健全な兆候
- **不確実性の保全**: 「推定」「(Explore SubAgent 観察)」と明記された Finding が複数 (F-008, F-046, F-047, F-048, F-049 等) あり、本レポートでも由来情報を保持。コードベース読解で確定できなかった項目は曖昧表記のまま残し、ハルシネーション混入を回避している
