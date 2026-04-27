# Maestro v2 包括的コードレビュー 最終統合レポート

- 作成日: 2026-04-26
- 対象 commit: e7dccde
- 由来コマンド: cmd_1777200395_b2770217f280e933 (cancelled) → cmd_1777204067_8a1139a5765fbd54 (resume)
- 統合元セクション: tmp/maestro-review/sections/01〜05 (全5ファイル、計1,015 行)

## 対応状況 (2026-04-27 追記)

以下は総 Finding **66 件** の対応状況。✅ は修正済み / 設計判断済み / 対応不要、⚠ は当面の改善は着地したが別 sprint の継続タスクを残す項目を示す。各 Finding 表内にも「対応状況」行を追加。

| ID | 重要度 | 概要 | 状態 |
|---|---|---|---|
| F-001 | Critical | `go build ./...` で interface 充足を確認 | ✅ |
| F-028 | Critical | `advanceRepairPendingToPausedForReplan` の state→queue 逆順ロックを 2 段階に分割 | ✅ |
| F-005 | High | phase fill rollback 失敗時に `paused_for_replan` planner signal を発行 | ✅ |
| F-006 | High | duplicate 経路で `taskRunOnIntegration: false` を明示 | ✅ |
| F-007 | High | `r9EffectiveVerifyStallThreshold` で `configured ≤ 0` を 60s フロアにクランプ | ✅ |
| F-018 | High | heartbeat 経路で `recordSelfWrite` を呼ぶよう修正 | ✅ |
| F-029 | High | `saveStateWithContext` に late-completion 検出 + warn ログ追加。retry state save timeout 経路では late completion 後に current-state guard 付きで rollback snapshot を再保存 | ✅ |
| F-030 | High | `DeleteState` および R0 reconciler で `.bak` も削除 | ✅ |
| F-031 | High | `recoverStateDir` で `.bak` の commandID 整合性を検証 | ✅ |
| F-038 | High | Clock interface を `internal/clock` に集約しエイリアスで参照 | ✅ |
| F-039 | High | ModelSelector を `internal/contract` に集約、bridge adapter を簡素化 | ✅ |
| F-052 | High | テスト名を `TestLoadState_NoMigrationAtCurrentSchemaVersion` に変更し整理 | ✅ |
| F-053 | Medium | policy_checker_test.go の dead code (空 if ブロック) を削除し意図を明確化 | ✅ |
| F-058 | Low | `TestCheckCommandTasksTerminal_*` 7 関数を table-driven 1 関数に統合 | ✅ |
| F-008 | Medium | `verify_runner_real_test.go` に非 git ディレクトリで expected_paths 検証が失敗するケースのテストを追加 | ✅ |
| F-011 | Medium | `FailurePattern.SuccessCount` / `SuccessRate` の整合性 invariant を godoc に明記 | ✅ |
| F-013 | Low | `bytesContainConflictMarkers` 先頭マーカー対応 + 単体テスト追加 | ✅ |
| F-014 | Low | daemon.go パッケージ doc の `TODO(refactor)` を整理しレビュー報告書へ参照 | ✅ |
| F-016 | Nit | `confirmSubmittedOrRetry` の probe budget 枯渇時に warn ログを出力 | ✅ |
| F-022 | Medium | `persona_hint` 省略時のデフォルト挙動を planner.md に明記 | ✅ |
| F-024 | Medium | planner.md の YAML 抜粋ブロックに `expected_paths` / `definition_of_abort` 必須参照を追記 | ✅ |
| F-033 | Medium | `checkInProgressDependencyFailuresDeferred` に transition 検証を追加 | ✅ |
| F-056 | Medium | `state_test.go` / `daemon_test.go` の package コメント TODO 列挙を整理しレビュー報告書を参照する形に | ✅ |
| F-061 | Low | `queue_handler_test.go` / `worktree_test_helper_test.go` の DRY TODO に明確な trigger 条件を追加 | ✅ |
| F-064 | Nit | `migrator.go` の TODO コメントを将来手順記録に書き換え | ✅ |
| F-065 | Nit | `quality_gate_test.go` の自明コメントを削除 | ✅ |
| F-066 | Nit | テストペイロードの `XXX` 命名を `ZZZ` に変更し repo-wide grep 混乱を回避 | ✅ |
| F-063 | Nit | fuzz テストの `t.Skip()` に理由文字列を追加 | ✅ |
| F-009 | Medium | runtime_launcher_test.go 削減カバレッジを launcher_test.go (TestBuildLaunchArgs_*) で確認、ランタイム検証 6 関数も維持されており追加変更不要 | ✅ |
| F-010 | Medium | `daemonapi/skill.go` の `slugify` → `skillSlugify`、`formatSkillMD` → `formatSkillMarkdown` に改名し grep 追跡性を改善 | ✅ |
| F-034 | Medium | `internal/daemon/queue_recovery.go` を新設し queue YAML の corrupted 検出 + .bak 復元 + ORC-3 epoch floor clamp を実装。daemon 起動時に呼び出し + 5 ケースのテスト追加 | ✅ |
| F-035 | Low | `updateQueueState` の lease 規約バイパス意図を godoc に明記 (terminal 遷移は in-place で lease lifecycle を畳む設計理由) | ✅ |
| F-036 | Low | `updateQueueState` で `model.ValidateCommandTaskQueueTransition` を呼び terminal 遷移検証を追加。invalid 時は H2 sticky-error 経由で R1 reconciler に repair 委譲 | ✅ |
| F-044 | Medium | `buildLaunchArgs` 95 行を `launchArgsCore` / `appendAllowedTools` / `appendDisallowedTools` / `appendNotificationSettings` の 4 ヘルパーに分割。`workerDisallowedTools` を package-level const に切り出し | ✅ |
| F-015 | Nit | submit-probe 定数 (`submitRetryProbeDelay` ほか) の意図と WatcherConfig 化保留理由を godoc 化 | ✅ |
| F-004 | Low | r9 paused_for_replan signal の dedup key 仕様を godoc に明記 (PhaseID `__task_` prefix の意図含む) | ✅ |
| F-002 | Low | `allowedToolsByRole` に「instructions/*.md と CLI 引数の同期義務」maintenance invariant を godoc 追記 | ✅ |
| F-046 | Medium | `forwardMergeBaseToIntegration` の conflict 処理 30 行を `recordForwardMergeConflict` ヘルパーに切り出し、merge 成功パスを平坦化 | ✅ |
| F-048 | Low | `templates/config.yaml` の admission_control / review / rollout / judge セクションに「コメントアウトしたままで運用 OK」「EXPERIMENTAL: not yet wired」明記 | ✅ |
| F-049 | Low | `internal/model/doc.go` を新設し Status / State / Phase の用語境界を package-level godoc に明文化 (リンター guidance + 歴史的例外含む) | ✅ |
| F-054 | Medium | panic recovery テスト 2 件の `testing.Short()` ガードを除去 (5s timeout は最悪値、典型 20ms で完了) | ✅ |
| F-057 | Low | `backoffDuration` を pure 関数として抽出 → `TestBackoffDuration_ExactSchedule` で algebra を厳密検証、wall-clock 検証は base=50ms に上げて閾値 150ms に緩和 | ✅ |
| F-037 | Low | `internal/lock/gc.go` を新設し起動時 1 回のみの flock GC を実装。dev/ino 一致確認 + 7 日 mtime ベース + daemon.lock 保護 + 5 ケースのテスト追加。codex 相談で race window を回避する戦略を確立 | ✅ |
| F-023 | Medium | `validateRequiredVerifySnapshot` のエラー文言を拡充 — `maestro verify write` 完全例 (CLI / stdin) と `templates/verify.yaml.example` への参照を含む復旧手順を提示 | ✅ |
| F-062 | Low | README.md「コード規約」セクションに godoc 言語使い分け方針 (公開 API は英語 / 実装ノートは日本語可 / ファイル単位で混在を避ける) を明文化 | ✅ |
| F-055 | Medium | 残る `time.Sleep` 5 箇所は race 再現・不在確認・シャッフルなど deterministic signaling で置換不能な意図を持つと確認、現状維持と判断 (実装変更なし) | ✅ |
| F-050 | Nit | 各 instructions/*.md (orchestrator / planner / worker) の冒頭に「SSOT 規約 (F-050)」block を追加し maestro.md が単一正本である旨を明示 | ✅ |
| F-027 | Low | `policy_checker.go` の B003 deny 文言を拡充 (`--force-with-lease` Tier3 alternative が Worker には適用外である旨を明記、F-027 / worker.md への参照を含める) | ✅ |
| F-040 | Critical | 段階1+2+3: `SyncFromIntegration` 137 行 → 3 ヘルパー、`performPublishMerge` 99 行 → 2 ヘルパー (段階1+2)。さらに `merge_publish.go` 1435 行 → 783 行へ物理分離 (`merge_forward.go` 325 行 + `publish.go` 370 行) — 各ファイル 800 行未満 (段階3) | ✅ (段階1+2+3) |
| F-041 | Critical | 段階1+2+3+4: `ResumeMerge` / `mergeResolvedWorker` / `AutoRecoverAfterResolution` を計 9 ヘルパーに分割 (段階1+2+3)。さらに `recover.go` 1116 行 → 302 行へ物理分離 (`recover_resume.go` 589 行 + `recover_auto.go` 274 行) — 各ファイル 600 行未満 (段階4) | ✅ (段階1+2+3+4) |
| F-042 | High | 段階1+2+3+4: `sweepExhaustedNotifications` / `recordTaskResultLearning` / `writeNotificationToOrchestratorQueue` を 4+5+4 ヘルパーに分割。さらに `result_handler.go` 1217 行 → 693 行に物理ファイル分離 (`result_dead_letter.go` 157 行 / `result_learning.go` 152 行 / `result_notification.go` 278 行) | ✅ (段階1+2+3+4) |
| F-025 | Medium | Step 1: bash hook 580 行を `worker_policy_hook.sh` に外部化、`policy_checker.go` 710 → 133 行。Step 2: `internal/validate/policy_corpus.json` を作成し、parity test を corpus + deny reason 比較へ移行。Step 3-5: Go policy 10 ルール翻訳 + `maestro hook policy-check` CLI。Step 6: `shadow` mode を追加。Step 7: `agents.workers.policy_hook_implementation` (`bash`/`shadow`/`go`) を追加。`go` は parity 未達のため config validation で reject、escape hatch `MAESTRO_POLICY_HOOK_ALLOW_GO_UNSAFE=1` で実験運用のみ許可。Step 8 は Go full parity 未達のため安全ゲートで保留 | ⚠ 継続 |
| F-003 | Low | F-013 で `bytesContainConflictMarkers` の単体テスト追加で実質対応済 (推奨アクション達成) | ✅ |
| F-051 | Nit | Explore SubAgent の誤検知記録 (`type *Cfg` 型は 0 件で確認済み)、対応不要 | ✅ |
| F-059 | Low | `executor_test.go` `TestApplyDefaults` を 16 連 if → 入力/期待 cfg ペア table-driven + accessor closure リストに書き換え | ✅ |
| F-012 | Medium | `AfterVerification` の重複条件を `shouldRunAfterVerificationSync` pure 関数に統合し、4 ケースの `TestShouldRunAfterVerificationSync` で sync 経路 exactly-once 不変条件を pin | ✅ |
| F-060 | Low | `logSuppressor` に `nowFn` を追加し `newLogSuppressorWithClock` テスト用コンストラクタを提供。`TestLogSuppressor_Allow` の 150ms `time.Sleep` を fake clock advance に置換 | ✅ |
| F-019 | High | 段階1+2: `FencingDetails` 構造体 + UDS error details (段階1)。CLI で `classifyFencingExitCode` / `fencingCLIError` を新設し、cmd_task / cmd_result から exit code 10/11/12 (`ExitCodeFencingEpoch` / `ExitCodeMaxRuntimeExceeded` / `ExitCodeFencingStatus`) に変換 (段階2)。stderr に JSON 1 行 (`maestro_error` tag) も追加 | ✅ (段階1+2) |
| F-021 | High | `worker.md` に新 exit code (10/11/12) ベースの fencing 判定手順を追記。pseudo シェル例 (case 文) を含め、stderr 文字列 grep を deprecated に格下げ | ✅ |
| F-026 | Medium | tmux user-variable 経路の設計 trade-off を policy_checker.go コメントに明文化 (env 経由は pane 再利用のため send-keys 依存が残り解消にならない / fail-closed が最も安全な default) | ✅ |
| F-017 | High | quarantine = operator 判断という設計意図を確定。`runPlanUnquarantine` godoc に Planner 開放を見送る理由を明記し、orchestrator.md に「quarantine 通知のエスカレーション」節を追加 | ✅ |
| F-020 | High | publish/merge/commit_failed は Planner 経由 CLI、unquarantine は operator-only として振り分けを明文化 | ✅ |
| F-043 | High | 1000 行超 3 ファイル (`merge_publish.go` / `recover.go` / `result_handler.go`) の物理分割を完了 | ✅ |
| F-032 | Medium | F-033 (transition 検証) / F-035 (Phase A bypass godoc) と一体で対処済。queue_scan_collect の cancelled 遷移にも「in-place lease lifecycle 畳み込み」設計意図と「lease.Manager に metrics 機構なし」を godoc 明記 | ✅ |
| F-045 | Medium | F-039 で完全解決済 (内部 `contract` パッケージへの SSOT 化、bridge adapter shim 削除)。実装変更なし | ✅ |
| F-047 | Medium | `executor_core.go` の各関数を精査、いずれも early-return でネスト 2 段以下に既に整理済み。実装変更なし | ✅ |

未対応の主なマージブロッカー級リスク: **なし**。継続課題は F-025 の段階移行。

別 sprint 移送タスク:
- **F-025 Step 8** (Medium): Go full parity と shadow divergence ゼロ確認後に bash ロジック削除 (詳細: `F-025-migration-plan.md`)
- F-040 / F-041 / F-042 / F-043 の物理ファイル分割は完全達成。1000 行超ファイルは残存しない

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

(✅ = 2026-04-26〜27 修正対応済み、⚠ = 補足課題あり)

1. ✅ **F-001 (Critical, 01)**: `worktreeManager` が `autoRecoverWorktreeManager` インターフェース実装を満たすかコード読みでは未確定 (要 build 検証)
2. ✅ **F-028 (Critical, 03)**: `advanceRepairPendingToPausedForReplan` で state→queue 逆順ロックを取得 (キャノニカル順序違反、将来デッドロック確実) ※サマリ初版の F-027 表記は誤り、本文 F-028 が正
3. ✅ **F-040 (Critical, 04)**: `internal/daemon/worktree/merge_publish.go` 1361 行・100 行超関数 5 件 (関数分割急務) ※サマリ初版 F-039 は本文 F-040 が正
4. ✅ **F-041 (Critical, 04)**: `recover.go` の `ResumeMerge` 177 行 / `mergeResolvedWorker` 146 行 (単一責務違反) ※サマリ初版 F-040 は本文 F-041 が正
5. ✅ **F-005 (High, 01)**: `submit.go` phase fill での state lock 解放→queue 書込→再取得 TOCTOU リスク
6. ✅ **F-006 (High, 01)**: `result_write_phase_a.go` duplicate 経路で `taskRunOnIntegration` 未明示 (silent regression リスク)
7. ✅ **F-007 (High, 01)**: `r9EffectiveVerifyStallThreshold` の `configured = 0` 時に即時 stall 判定の保護なし
8. ✅ **F-018 (High, 02+03 統合)**: `task_heartbeat_handler.go:202` で `recordSelfWrite` を呼ばず、heartbeat 由来の自己書込が外部編集と誤認される
9. ✅ **F-019 (High, 02)**: lease 失効が stderr 文字列ベースで Worker に通知される (epoch mismatch のフォーマット不一致、`current_epoch` 未提供)
10. ✅ **F-029 (High, 03)**: `internal/plan/retry.go:22-33` `saveStateWithContext` の goroutine リーク + ロールバック巻き戻しリスク ※late-completion 後の current-state guard 付き rollback resave まで対応。サマリ初版 F-026 は本文 F-029 が正

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
| 対応状況 | ✅ 修正済み (2026-04-26): `go build ./...` および `go vet ./...` が green、関連テストも全 pass を確認 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `allowedToolsByRole` に MAINTENANCE INVARIANT godoc を追記 — `Bash(maestro …)` の追加/削除時に instructions/*.md と同コミットで更新する義務を明示。`workerDisallowedTools` の deny list にも同等のルール明記 |

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
| 対応状況 | ✅ 実質対応済 (2026-04-27): F-013 で `bytesContainConflictMarkers` の単体テスト (先頭マーカーケース含む) を追加し、`stageResolvedForwardMergeFiles` 経由のマーカー検出は同関数を呼ぶ。`merge_forward.go` 物理分離 (F-040 段階3) でこのコードは別ファイルになったが論理は不変。本 finding 自体は「整合性確認済み」が現状であり、推奨アクションは F-013 で達成済 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `r9QueuePausedForReplanSignal` 内に dedup key (`Kind` / `CommandID` / `PhaseID` / `WorkerID` / `ConflictGeneration`) と PhaseID `__task_` prefix の意図を godoc 化。canonical 定義は `plannerSignalDuplicate` / `signalDedupKey` を参照する形に統一 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `internal/plan/planner_signal.go` を新設し `emitPausedForReplanSignal` ヘルパーを追加 (queue:planner_signals lockMap + flock + ReadModifyWrite + dedup)。`submitPhaseFill` の 4 つの recoverable=false 経路 (queue_write/reload_queue_rollback/apply_queue_rollback/save_state) で state lock 解放後にシグナル発行。`planner_signal_test.go` に append/dedup/nil-lockMap の 3 ケース + dedupKey の 6 ケースを追加 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): 3 つの duplicate 経路で `taskRunOnIntegration: false` を明示し、意図を説明するコメントを追加 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `configured <= 0` で `verifyStallSaneMinimum=60s` にクランプし、warn ログ `verify_stall_threshold_clamped` で誤設定を可視化 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): エラー文言は既に `verify_expected_paths_check_failed` で wrap 済み (実装側変更なし)。`TestRealVerifyRunner_RejectsExpectedPathsCheckOutsideGitRepo` を追加し、非 git ディレクトリで `Reason` がこの prefix を含み、verify コマンドが実行されないことを assert |

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
| 対応状況 | ✅ 確認済み (2026-04-26): `runtime_launcher_test.go` には削除後も 6 つのテスト (ClaudeCodeAvailable / RejectsUnsupportedRuntimes / EmptyRuntimeDefaultsToClaudeCode / ClaudeCodeWithModel / FallbackToDefault_AlwaysClaudeCode / LaunchAlternativeRuntimeRejectsManagedRoles) が残り、`launcher_test.go` 側に `TestBuildLaunchArgs_*` を含む 30+ 関数で汎用引数ビルダの検証が維持されている。カバレッジ後退なしと判断、追加変更不要 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `slugify` → `skillSlugify`、`formatSkillMD` → `formatSkillMarkdown` に改名。skill ドメイン専用であることをローカルスコープでも示し、grep 追跡性を改善 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `FailurePattern` 型 godoc に F-011 invariant を明記 — `SuccessCount` と `SuccessRate` は package 外から独立に変更してはならず、`FingerprintDB.RecordOutcome` 等 package 内 API のみ更新。setter 制約までは非導入 (既存コードへの影響を抑える) |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `AfterVerification` の重複条件 (2 つの `if !duplicate && !verifyWillRunAsync`) を `shouldRunAfterVerificationSync` pure 関数に統合し、early-return で fall-through を防ぐ構造に整理。`TestShouldRunAfterVerificationSync` で 4 ケース (sync / duplicate / async / 両方) を pin |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `bytes.HasPrefix` で `=======` / `>>>>>>>` 先頭ケースを OR 条件追加、`merge_publish_test.go` に `TestBytesContainConflictMarkers` (8 ケース) を追加 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): godoc 冒頭の `TODO(refactor)` 計画文 (18 行) を簡潔化し、未着手の責務分割は本レビュー報告書 (F-014/F-040..F-043) を参照する形に。「未着手」の語感を排除し godoc は概要のみ残す形へ |

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
| 対応状況 | ✅ 部分対応 (2026-04-26): WatcherConfig 化は保留 (既存ランタイムで個別チューニングが不要なため)。代わりに各定数の意図と `8 * submitRetryProbeDelay = 6s` 上限の根拠を godoc に明記し、外出しが必要になった際の移行方針 (call site が定数名で参照済み) もコメントに残した |

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
| 対応状況 | ✅ 修正済み (2026-04-26): probe budget 枯渇時に `submit_confirm probe_budget_exhausted` warn ログを追加。`Retryable: true` 化は double plan_submit (Bug L) の懸念があるため見送り、運用判断時の手掛かりを残す形で silent failure を解消 |

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
| 対応状況 | ✅ 設計判断確定 (2026-04-27): 確認の結果 `plan resume-merge` / `plan retry-publish` / `plan resolve-conflict` は既に Planner / オペレーター双方で利用可能。`plan unquarantine` のみ operator-only に残すのが正しい設計 (3+ 連続失敗の根本原因を agent が握るべきでない)。`runPlanUnquarantine` godoc にこの trade-off を明記、`orchestrator.md` に「quarantine 通知のエスカレーション」節 (振り分け表 + 設計意図) を追加 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `TaskHeartbeatHandler` に `selfWrites *selfWriteTracker` を追加し `WithHeartbeatSelfWrites` option 経由で wiring。heartbeat の queue 書き込み直前に `selfWrites.Record(queuePath, &queue)` を呼ぶよう変更 |

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
| 対応状況 | ✅ 段階1+2 完了 (2026-04-27): 段階1 で `uds.errorDetail.Details` + `FencingDetails` + 各 fencing 経路で details attach。段階2 で CLI 側に `classifyFencingExitCode` / `fencingCLIError` ヘルパーを新設し、heartbeat / result_write から exit code 10 (`fencing_epoch`) / 11 (`max_runtime`) / 12 (`fencing_status`) に変換。stderr に `{"maestro_error":...,"details":{...}}` JSON 1 行を併記 (互換のため legacy stderr 文字列も維持)。F-021 で worker.md を `$?` 判定に移行。codex 相談で legacy fallback / pipe 越し $? の罠 / 重複出力戦略を確立 |

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
| 対応状況 | ✅ 設計判断確定 (2026-04-27): F-017 と一体で対処。publish_conflict 解決 / merge_conflict 解決 / commit_failed 対応は既に Planner CLI (`plan resume-merge` / `plan retry-publish` / `plan resolve-conflict`) で自律可能。残る `unquarantine` は operator-only を維持し、`orchestrator.md` の通知エスカレーション節でユーザーへの伝達経路を文書化 |

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
| 対応状況 | ✅ 修正済み (2026-04-27): F-019 段階2 と一体で対処。`worker.md` の epoch 失効検知節を全面改訂し、`$?` (exit code 10/11/12) ベースの判定手順 + pseudo シェル例 + 「stderr 文字列 grep は deprecated」明示を追加。Worker LLM 判断 → 機械可読 exit code への移行を完了 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): planner.md の「ペルソナ活用ガイド」と必須/任意フィールド表に省略時の挙動 (注入なし、デフォルト補完なし、実装系には明示的 `implementer` 推奨) を追記。実装挙動 (envelope/dispatch) を変えず仕様明確化のみ実施 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): 推奨 (b) を採用。`validateRequiredVerifySnapshot` のエラー文言 3 種 (NotExist / Invalid / Empty) すべてに「Quick fix: 完全な CLI 例 / stdin 例 / templates/verify.yaml.example 参照」を含めた段階的修復手順を返す形に拡充。(a) の verify-bootstrap 自動挿入は別途検討 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): 主要な抜粋ブロック 3 箇所 (skill_refs 使い方例・verification phase 構成・段階実行 phases 例) に「expected_paths / definition_of_abort 必須」を明記し、§「plan submit 入力形式 / タスクのみ（単一フェーズ）」への参照を付与 |

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
| 対応状況 | ⚠ Step 1-6 着地 / Step 7 はゲート付き experimental / 10 ルール翻訳済、bash 削除は安全ゲートで保留 (2026-04-27): **Step 1 (bash 外部ファイル化)** で `worker_policy_hook.sh` 新設 + `//go:embed`、`policy_checker.go` を縮小 (バイト等価ではなく semantics-preserving + shadow フック追加)。**Step 2 (golden corpus)** で `internal/validate/policy_corpus.json` を新設し、parity test を hardcoded matrix から corpus 読み込みへ変更。**Step 3 (Go 純粋関数)** で `CheckWorkerPolicy` 実装、10 ルール翻訳完了: C1 backtick / C1 ANSI-C quoting / H1-PS / D001 / D004 / Worker git push / D005 sudo+su / D006 kill+killall+pkill / D008 curl\|sh。**Step 4-5** で bash↔Go parity と `maestro hook policy-check` CLI を追加。**Step 6** で shadow mode を追加。**Step 7** で `agents.workers.policy_hook_implementation: bash\|shadow\|go` を追加 — ただし `go` は parity 未達のため `config_validate.go` で reject、escape hatch `MAESTRO_POLICY_HOOK_ALLOW_GO_UNSAFE=1` 経由でのみ実験運用可能 (理由: 未翻訳ルールが silently 無効化されるため)。残: Go 未翻訳ルール (D002, D007, D009-D012, B001-B004, M-AGT1, RUN_ON_MAIN, WT-GIT, WT001, Write/Edit checks 等) を埋め、shadow divergence ゼロ確認後に Step 8 の bash ロジック削除と Step 7 ゲート解除を行う。詳細は `F-025-migration-plan.md` |

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
| 対応状況 | ✅ 設計判断確定 (2026-04-27): env 経由は実装非効率と判断 (Worker pane は再利用される長寿命プロセスのため、process-level env はタスクごとに `tmux send-keys export` で更新する必要があり tmux 依存が消えない)。fail-closed の方が「stale env 値の使い回し」より安全。policy_checker.go コメントに F-026 trade-off rationale を明文化 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `templates/maestro.md` L76 に既に「**Worker には適用外**」明記済 (確認)。`policy_checker.go` の B003 deny 文言を拡充し「--force-with-lease を含む / Tier3 alternative does not apply / F-027 worker.md 参照」を含めるよう更新 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `defer h.lockMap.Unlock(cmdLockKey)` を解除し、`updateYAMLFile` 完了直後に明示 Unlock。state ロック解放後に `emitPausedForReplanPlannerSignal` を呼ぶ形に変更し、canonical 順序 `queue → state → result` を遵守 |

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
| 対応状況 | ✅ 修正済み (2026-04-27): `saveStateWithContext` は `stateSaveTimeoutError` で late completion handle を返す。retry state save timeout 経路では queue rollback + in-memory restore 後、late SaveState 完了を待ってから state lock を取り直し、現在の on-disk state が attempted snapshot と一致する場合のみ original rollback snapshot を再保存する。後続操作が既に state を進めていた場合は `skipped_current_state_changed` として上書きしない |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `StateManager.DeleteState` で `.bak` を best-effort 削除 (ErrNotExist は許容)、R0 reconciler の state 削除パスにも `.bak` 削除を追加。F-030/F-031 の関連性を godoc に明記 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): F-030 と併せて `recoverStateDir` に `extractCommandID` ヘルパーを追加し、`.bak` 中身の `command_id` がファイル名から導出される expected commandID と不一致なら復元をスキップ + warn ログ `state_recovery bak_command_id_mismatch` を出す |

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
| 対応状況 | ✅ 修正済み (2026-04-27): F-033 (transition 検証) と F-035 / F-036 (Phase A bypass godoc + transition 検証) で実質対応済。queue_scan_collect の cancelled 遷移にも「in-place lease lifecycle 畳み込み」設計意図と「`lease.Manager` には per-release metrics 機構が実装上存在しない (audit log は H5 acquire 側のみ)」ことを godoc 明記し、`releaseLease` 経由化が観測可能な利点を生まない理由を文書化 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `checkInProgressDependencyFailuresDeferred` で cancelled 直接代入の前に `model.ValidateCommandTaskQueueTransition` を呼び、invalid transition を warn ログ + skip。F-032 の `releaseLease` 統一は API 不整合のため別途要対応 (Manager.ReleaseTaskLease は in_progress→pending 専用) |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `internal/daemon/queue_recovery.go` を新設し `recoverQueueFiles` / `recoverQueueDir` を実装。daemon 起動時 (`recoverStateFiles` 直後) に呼び出し、`.bak` 復元 + ORC-3 epoch floor clamp を提供。`queue_recovery_test.go` で 5 ケース (valid 不変 / corrupt 復元 / no_backup / epoch clamp / 欠落 dir) を追加 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `updateQueueState` の godoc に「lease.Manager.releaseLease を意図的に経由しない (terminal で in-place 畳み込み、LeaseEpoch は late heartbeat の fencing key として保持)」と明記し設計意図を文書化 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `updateQueueState` で `model.ValidateCommandTaskQueueTransition` を呼び、invalid 遷移時は warn ログ + queueWriteFailed=true で H2 sticky-error 経由で R1 reconciler に repair 委譲 (result file はコミット済み) |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `internal/lock/gc.go` を新設し `GCStaleLockFiles` を実装。起動時 1 回のみ実行、`open → fstat → LOCK_EX|LOCK_NB → stat 比較で dev/ino 一致確認 → unlink` の 4 ステップで race 安全。daemon.lock は protect リストで保護、symlink/非 regular file はスキップ。`gc_test.go` に 5 ケース。codex 相談で race window 戦略を確立 (定期 GC は危険のため不採用) |

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
| 対応状況 | ✅ 修正済み (2026-04-26): 中立な `internal/clock` パッケージを新設し `Clock` / `RealClock` を SSOT 化。`core.Clock` / `metrics.Clock` を `clock.Clock` のエイリアスに、`testutil.Clock` を `clock.Clock` を埋め込んだ拡張インターフェースに変更。利用側の変更ゼロで重複解消 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `internal/contract` パッケージを新設し `ModelSelector` を SSOT 化。`plan.ModelSelector` / `core.ModelSelector` を共に `contract.ModelSelector` のエイリアスに変更。`bridge.PlanExecutorImpl.SetModelSelector` の `coreSelectorAdapter` shim を削除し直接代入で済む形に簡素化 |

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
| 対応状況 | ✅ 段階1+2+3 完了 (2026-04-27): `SyncFromIntegration` 137 行 → 3 ヘルパー分割 (段階1)、`performPublishMerge` 99 行 → `mergeIntegrationIntoPublishTemp` / `fastForwardBaseBranchRef` 分割 (段階2)、F-046 で `forwardMergeBaseToIntegration` conflict 処理切り出し。**段階3**: `merge_publish.go` 1435 行を **3 ファイルに物理分離** — `merge_forward.go` (325 行: forward-merge 7 関数)、`publish.go` (370 行: publish パイプライン 8 関数)。本体 `merge_publish.go` は 783 行に縮小し各ファイル 800 行未満を達成、`go test ./internal/daemon/worktree/... -count=1` および全 40+ パッケージ green |

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
| 対応状況 | ✅ 段階1+2+3+4 完了 (2026-04-27): 関数分割 (段階1+2+3) に加え、**段階4** で `recover.go` 1116 行を **3 ファイルに物理分離** — `recover_resume.go` (589 行: ResumeMerge パイプライン 9 関数 + mergeResolvedWorker 系 3 関数)、`recover_auto.go` (274 行: AutoRecover / AutoRecoverAfterResolution + selectAutoRecoverAction / publishBackoffElapsed)。本体 `recover.go` は 302 行 (Unquarantine / RetryPublish / ResetResolvingWorkerToConflict / ResolveConflict のみ) に縮小し各ファイル 600 行未満を達成、`go test ./... -count=1` 全 43 パッケージ green を確認 |

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
| 対応状況 | ✅ 段階1+2+3+4 完了 (2026-04-27): 関数分割 (段階1+2+3) に加え、`result_handler.go` 1217 行を **3 ファイルに物理分割** — `result_dead_letter.go` (157 行: sweep 系)、`result_learning.go` (152 行: Phase C 学習系 4 関数)、`result_notification.go` (278 行: Planner/Orchestrator 通知 + dedup helper)。本体 `result_handler.go` は 693 行に縮小し、レシーバ・package 不変。`go test ./internal/daemon/... -count=1` 全 24 パッケージ green を確認。1 関数あたり最大 50 行以下 / 1 ファイルあたり最大 700 行以下を達成 |

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
| 対応状況 | ✅ 完了 (2026-04-27): 1000 行超 3 ファイルすべて物理分割完了 — `result_handler.go` 1217 → 693 行 (F-042 段階4)、`merge_publish.go` 1435 → 783 行 (F-040 段階3, `merge_forward.go` 325 + `publish.go` 370 行)、`recover.go` 1116 → 302 行 (F-041 段階4, `recover_resume.go` 589 + `recover_auto.go` 274 行)。さらに F-044 で `launcher.go`、F-046 で merge conflict 切り出しを実施し関数単位責務分割も完了 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `buildLaunchArgs` を `launchArgsCore` (基本フラグ) / `appendAllowedTools` / `appendDisallowedTools` (planner/worker) / `appendNotificationSettings` (role 別 hooks) の 4 ヘルパーに分割。`workerDisallowedTools` を package-level const に切り出し、各関数 godoc で背景情報 (D006/D009/sandbox 設定の意図) を保持 |

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
| 対応状況 | ✅ F-039 で完全解決済 (2026-04-26): `internal/contract` パッケージを新設し `ModelSelector` を SSOT 化。`plan.ModelSelector` / `core.ModelSelector` を共に `contract.ModelSelector` のエイリアスに変更し、bridge.go の `coreSelectorAdapter` shim を削除済み |

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
| 対応状況 | ✅ 部分対応 (2026-04-26): `forwardMergeBaseToIntegration` の conflict 処理ブロック (30 行・2 段ネスト) を `recordForwardMergeConflict` に切り出し、merge 成功パスを平坦化。critical invariants (merge を abort しない / PublishConflictSignaled の reset 条件) は新ヘルパー godoc で保持 |

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
| 対応状況 | ✅ 検証済 (2026-04-26): `Execute` (37 行)、`execWithClear` (50 行)、`execInterrupt` (44 行) ほか各関数を精査。すでに early-return + 単純 switch でネスト 2 段以下に整理済みで、深いネストは観測されず。元レビューの「(推定)」が現実と乖離していたケース。実装変更なし |

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
| 対応状況 | ✅ 修正済み (2026-04-26): 4 セクションそれぞれに「コメントアウトしたまま運用 OK / デフォルト適用」と「EXPERIMENTAL: production X が未接続」を明示。実装済 (admission_control, review) と未接続 (rollout, judge) を文言で区別 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `internal/model/doc.go` を新設し package-level godoc に Status / State / Phase の用語境界、各種類の代表例、リンター guidance、歴史的例外 (RolloutState) を明文化。`go doc model` で常時参照可能。Linter rule は別途検討 |

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
| 対応状況 | ✅ 部分対応 (2026-04-26): 各 `instructions/*.md` の冒頭に「SSOT 規約 (F-050)」を blockquote として追加し maestro.md が単一正本である旨と「重複が見つかったら instructions 側を削除して maestro.md 参照に統一」を明記。CI 監査ルールは別途検討 |

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
| 対応状況 | ✅ 対応不要 (誤検知のため、2026-04-27 検証): `grep -rE '^type [A-Z][a-zA-Z]*Cfg\b' internal/` で 0 件確認。コードは `Config` 型 + `cfg` 変数で統一済み。レビュー報告書に誤検知として記録 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `TestLoadState_NoMigrationAtCurrentSchemaVersion` にリネーム、無意味な `defaultMigrator` 差し替えコードを削除し、現状の挙動 (no-op) と将来 schema bump 時の actual migration 検証を doc コメントで明確化 |

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
| 対応状況 | ✅ 部分対応 (2026-04-26): `TestHookScript_BlocksPipeToShell` の dead code (空 if ブロックと未使用ループ) を削除し、B001 セクション存在 + word-boundary anchor の 2 つの assertion に整理。残る `strings.Contains` 系のリファクタは S シリーズ統合に依存するため別途対応 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `panic_recovery_test.go` の 2 関数から `testing.Short()` ガードを除去。godoc に「`require.Eventually` が早期完了するため typical 20ms / max 5s」と根拠を明記。`-short` 実行で 21ms 完了を確認。`integration_test.go` 側の performance テストは別目的なので保留 |

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
| 対応状況 | ✅ 検討済 (2026-04-26): 各箇所を精査した結果、それぞれ deterministic signaling では置換不能な意図 (`uds_test.go:852` ENOENT race の意図的再現 / `integration_test.go:1696` "Intentionally slow subscriber" / `:1920` 不在の確認 (6 回 polling) / `event_bridge_timeout_test.go:103` タイムアウト境界 / `spawn_tracked_test.go:70` 確率的シャッフル) を持つと判断。実装変更なし、現状維持。`require.Eventually` 化は「期待状態に到達」用途のみで「期待しない状態の不在」確認には不向き |

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
| 対応状況 | ✅ 修正済み (2026-04-26): 両ファイルの package コメントから古い列挙を削除し、本レビュー報告書 (F-056) を canonical roadmap として参照する形に統一 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `backoffDuration` を pure 関数として抽出。`TestBackoffDuration_ExactSchedule` で `base * 2^(attempt-1)` を 6 ケース (1ms/50ms × 4 attempt) で厳密検証、wall-clock 検証は base=50ms × attempt 3 = 200ms 期待で閾値 150ms に緩和し flake 耐性向上 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): 7 関数を `TestCheckCommandTasksTerminal` 1 関数 (table-driven 7 ケース) に統合。一部の元テストは `hasFailed` 未検証だったため `ignoreHasFailed` フラグで挙動を保持 |

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
| 対応状況 | ✅ 修正済み (2026-04-27): `executor_test.go:39-103` の `TestApplyDefaults` を入力 cfg / 期待 cfg ペアの table-driven (2 行) + 8 フィールドの accessor closure リストに書き換え。各フィールドは 1 箇所で expectations を定義。`go test ./internal/agent/... -count=1 -run TestApplyDefaults -v` 全シナリオ green を確認 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `logSuppressor` に `nowFn func() time.Time` を追加し、テスト用コンストラクタ `newLogSuppressorWithClock` を提供。`TestLogSuppressor_Allow` を fake clock 化し 150ms `time.Sleep` を排除。本番経路は `nowFn=nil` で `time.Now` フォールバック (挙動不変) |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `queue_handler_test.go` の TODO に「3rd caller 出現または F-040..F-043 split 後 + 2 sprint 経過」を trigger として明記。`worktree_test_helper_test.go` には「`internal/daemon/worktree` に共通 config builder が出来てから」と具体化 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `README.md`「コード規約」セクションに方針を明文化 — godoc 公開 API は英語 / 実装ノートは日本語可 / ファイル単位で混在を避ける / 段階的統一を許容。CONTRIBUTING.md の新規作成は CLAUDE.md ルールに沿って回避し、既存 README に追加 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): UDS fuzz は `seed exceeds maxFrameSize+4 envelope budget — out of fuzz scope`、state fuzz は `seed exceeds 1MiB YAML budget — out of fuzz scope` と理由文字列を付与 |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `TODO(schema)` 行を削除し、見出しを「Migration procedure (when bumping currentSchemaVersion):」に書き換え |

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
| 対応状況 | ✅ 修正済み (2026-04-26): `// Initialize the gate engine` (3 箇所)、`// Start the daemon` / `// Stop the daemon` を削除 |

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
| 対応状況 | ✅ 部分対応 (2026-04-26): `"geminiXXX"` → `"geminiZZZ"`、`"AAAAAXXX"` → `"AAAAAZZZ"` に置換。`cmd_queue_test.go` の `"deprecated"` は実際の deprecation warning 検証なので変更しない |

---

## 4. 総評

### 4.1 全体傾向

2026-04-27 の対応後、Critical / High の主要リスクは修正済み、または設計判断として明文化済み。`go test ./... -count=1` と `go vet ./...` は green で、F-001 の build 確認、F-028 の lock order、F-018/F-019/F-021 の lease/fencing 経路、F-040/F-041/F-042/F-043 の巨大ファイル問題はいずれも解消済み。

コード品質面では `merge_publish.go` / `recover.go` / `result_handler.go` の 1000 行超ファイルを物理分割し、`Clock` と `ModelSelector` は `internal/clock` / `internal/contract` に集約済み。通知・学習・publish/recover の主要責務も分離され、元レビュー時点の「未着手」状態は解消されている。

残る大きな継続課題は F-025 の Worker policy hook Go 化の最終段階。golden corpus、shadow mode、feature flag 切替までは着地したが、Go 未翻訳ルールが残るため bash ロジック削除は安全ゲートで保留している。

### 4.2 残る継続タスク

1. **F-025 Step 8**: Go policy の未翻訳ルールを埋め、shadow divergence ゼロ確認後に bash ロジックを削除する
2. **任意改善**: F-023 の verify-bootstrap 自動挿入、F-049/F-050 の lint / CI 監査ルール化、F-053 の `strings.Contains` 系テスト統合は、今回の高優先度修正とは切り離して扱う

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
