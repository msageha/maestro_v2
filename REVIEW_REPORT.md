# maestro_v2 包括的コードレビュー報告書

**日付**: 2026-04-19
**対象**: maestro_v2 リポジトリ全コードベース
**規模**: Go 約149,000行（ソース51,500行 + テスト97,500行）、494ファイル、35パッケージ

---

## 総合サマリ

| 深刻度 | 件数 | 主要領域 |
|--------|------|---------|
| Critical | 20 | Features層(8), ドキュメント乖離(5), Worktree(2), Reconcile/Plan(2), セキュリティ(1), テスト(2) |
| Major | 68 | Features層(20), CLI/Model(13), Worktree(8), Reconcile/Plan(6), テスト(6), Daemon(5), Agent(5), Docs(5) |
| Minor | 60 | Features層(15), CLI/Model(10), Agent(9), Daemon(8), Worktree(5), テスト(5), Docs(4), Reconcile(4) |
| Info | 18 | Agent(7), Features(5), Daemon(3), テスト(3) |
| **合計** | **166** | |

### 最優先修正推奨 Top 5

1. **envelope.go:58** — U+2028/U+2029（Unicode改行）未処理。ヘッダーインジェクション経路
2. **circuitbreaker.go:64,121** — AppliedResultIDs書き込み不在。冪等性ガード不全
3. **r7_merge_conflict.go:118-130** — state書き込み失敗時の通知欠落。conflict永続化
4. **plan/retry_queue.go:61-96** — ロックなしキュー読み取り。race condition
5. **planner.md** — CLIフラグ2件+サブコマンド4件が未記載。運用混乱

---

## 1. 機能的正当性

**全体評価: 良好。** Orchestrator→Planner→Workerの委譲チェーン、3フェーズスキャン+フェンシングトークン方式のリース管理は堅牢に設計されている。

### Critical

| # | ファイル | 問題 | 改善提案 |
|---|---------|------|---------|
| 1 | circuitbreaker.go:64,121 | AppliedResultIDsは読み取りのみで書き込みが存在しない。冪等性ガードが完全に機能しない | 結果適用時にIDを記録する書き込み処理を追加 |
| 2 | circuitbreaker.go:121 | StatusCompleted/Failed以外のステータス(Cancelled等)がAppliedResultIDs更新をバイパスし重複カウント発生 | 全terminal statusでAppliedResultIDsを更新 |
| 3 | search/tree.go:228-260 | PruneBelow()で親ノードにAvgReward閾値チェックが残存。高スコア親が子全剪定時に誤剪定される | 親ノードの閾値チェックを削除し、子ノードのみで判定 |
| 4 | search/tree.go:117-132 | Backpropagate()でノードID不在時にsilent failし報酬が消失、親ノード伝播も停止 | エラーログ出力+親伝播は継続する設計に変更 |
| 5 | featuregate/evaluator.go:121 | LoadProfiles()がprofilesマップを完全置換。部分configでcomplex/criticalレベルが消失する | マージ方式に変更(既存プロファイルを保持) |
| 6 | judge/judge.go:86-96 | LLM応答のwinner_indexを候補SlotIndex範囲で検証していない。不正インデックスがそのまま返却される | 範囲チェック追加、範囲外はエラー返却 |
| 7 | r7_merge_conflict.go:118-130 | state書き込み失敗時にrepairは返すがnotificationは返さない。workerにconflict解決通知が送られず、conflict状態が永続化する | notification代入をmodifiedブロック外へ移動 |
| 8 | plan/retry_queue.go:61-96 | loadOriginalTasksFromQueueがキューファイルをロックなしで読み取る。retry操作中にキューが変更された場合にrace condition | LockMap引数追加+各workerキューロック取得 |

---

## 2. バグ検出

### Critical

| # | ファイル | 問題 | 改善提案 |
|---|---------|------|---------|
| 1 | rollout/manager.go:113 | CreateGroup()が内部ポインタを返却。ロック外で呼び出し元がGroup構造体を直接変更可能(race condition) | コピーを返却するかread-onlyインターフェースを返す |
| 2 | worktree/recover.go:531-533 | mergeResolvedWorkerでabort失敗後のrecoveryエラーが破棄されworktree不整合放置 | recoveryエラーをwrapして返却 |
| 3 | worktree/operations.go:512-524 | getConflictFilesInDir()がNUL区切り未使用で改行含むファイル名で破損 | `git diff --name-only -z` (NUL区切り)に変更 |

### Major

| # | ファイル | 問題 | 改善提案 |
|---|---------|------|---------|
| 4 | dispatch/dispatcher.go:172-188 | DispatchCommand指数バックオフ上限なし。maxRetries大設定で数十秒〜数分のsleep。context非対応でshutdown中もブロック | バックオフ上限(30秒)追加+context.Done()対応 |
| 5 | dispatch/dispatcher.go:231-260 | DispatchTaskも同様の指数バックオフ問題 | 同上 |
| 6 | daemon_shutdown.go:155-163 | shutdownタイムアウト時にos.Exit(1)でdeferのリソース解放が未実行 | 2段階方式(cancel伝播→猶予期間→os.Exit) |
| 7 | daemon_shutdown.go:43-55 | 二重シグナル時のgoroutineがerrgroup外で起動。watcher.Close()の二重呼び出し競合 | sync.Once化またはforceExitフラグで制御 |
| 8 | queue_scan_collect.go:125 | model.Taskのスライスフィールドがshallow copy。現在は読み取り専用で実害なしだが将来脆弱 | deep copyまたはポインタ参照に統一 |
| 9 | search/thompson.go:92-120 | gammaSample()のMarsaglia-Tsang法に無限ループリスク(タイムアウト/最大反復なし) | 最大反復回数+タイムアウト追加 |
| 10 | search/thompson.go:92-96 | alpha<1時の再帰にスタックオーバーフロー保護なし | 反復上限追加 |
| 11 | bandit/ucb1.go:28-33 | explorationCoeffにNaN/負値の入力検証なし。NaN伝播でアルゴリズム破損 | コンストラクタで入力検証 |
| 12 | learnings/fingerprint_db.go:52-146 | Store()とRecordSuccess()並行実行でSuccessRate不整合 | ロック粒度の見直しまたはatomic操作 |
| 13 | worktree/cleanup_gc.go:117-121 | CleanupAllがcmdLocks未取得でresolver競合 | cmdLocks取得を追加 |
| 14 | worktree/merge_publish.go:116-136 | forwardMergeでSHA未検証 | マージ前にSHAの存在確認 |
| 15 | worktree/merge_publish.go:844-869 | 三重失敗でtemp branch残留 | cleanup処理追加 |
| 16 | worktree/recover.go:108 | RetryPublishのquarantine判定がstrings.Contains依存で誤判定リスク | 構造化された状態チェックに変更 |
| 17 | worktree/config_validate.go:258-276 | base_branch/path_prefix未検証 | バリデーション追加 |
| 18 | worktree/state_store.go:38-46 | Stat-ReadFile TOCTOU | atomic read pattern適用 |

---

## 3. コード品質

### DRY違反 (Major)

| # | ファイル | 問題 | 改善提案 |
|---|---------|------|---------|
| 1 | cmd/maestro/全体 | requireMaestroDir()が13ファイル26箇所で重複 | 共通ヘルパーに抽出 |
| 2 | cmd/maestro/6箇所 | udsErrorInfo()エラー処理パターンが重複(cmd_queue.go:177, cmd_result.go:114, cmd_skill.go:162,213, cmd_plan.go:189, cmd_plan_ops.go:51) | 共通エラーハンドラに統合 |
| 3 | cmd/maestro/全体 | CommandBuilder初期化の同一パターン反復 | ビルダーファクトリに統合 |

### デッドコード

| # | ファイル | 問題 |
|---|---------|------|
| 4 | evolution/engine.go:50,55,59 | noveltyThresholdフィールドが設定されるが一切使用されない(CheckNoveltyはexact hashのみ) |
| 5 | model/config_types.go:43-44 | qualityGateThresholds空struct。フィールド未実装 |
| 6 | model/evolution.go:4 | mutationStrategy型が未参照(定数のみ使用) |
| 7 | fallback/manager.go:60 | lastSuccessAtフィールドが書き込みのみで読み取りなし |

### Go idiom違反

| # | ファイル | 問題 | 改善提案 |
|---|---------|------|---------|
| 8 | cmd_task.go:48 | map[string]interface{}が1箇所残存(他はmap[string]any統一) | map[string]anyに統一 |
| 9 | lock_order_enabled.go:163-172 | runtime.Stack()パースでGoroutine ID取得。非公式API依存 | Goバージョン変更リスクを文書化 |
| 10 | cmd/maestro/全体 | エラーメッセージの書式不統一(exit code 2の条件が散在) | 定数化して統一 |

### その他 Minor

- model/config_experimental.go:177-267 — NormalizeExperimentalConfig後のバリデーション欠如
- model/state.go:130-134 — PhaseConstraints.MaxTasks/TimeoutMinutesに0/負値バリデーションなし
- UDS server.go:145 — backpressure応答のwrite deadline 1秒がマジックナンバー
- events/bus.go:285-299 — バッファフル時のイベントドロップがクリティカルイベント種別を区別しない
- cmd_queue.go:184-196 — json.Unmarshal失敗時にprintJSONResponseへフォールスルー、エラー隠蔽
- UDS server.go:71-73 — chmod失敗時にソケットファイルが残存(次回Start()失敗の原因)
- UDS client.go — plan以外のCLIコマンドがタイムアウト/signal context未設定
- UDS server.go — Unixソケットパス長(108byte上限)の未検証
- UDS client.go:39-43 — isTransientDialError()がECONNREFUSED/EAGAIN/ENOENTのみ判定。永続エラーも3回リトライ
- UDS protocol — バージョンネゴシエーション/ハンドシェイク未実装
- model/review.go:87-97 — NewReviewResult()がIsAdvisory=trueをハードコード
- yaml/schema.go:49-54 — SchemaVersionの上限チェックが将来バージョンで未知の変更を受容するリスク
- validate/path.go:73-78 — ContentLength()がバイト長チェック。UTF-8マルチバイト文字でユーザー期待と乖離
- yaml/atomic.go:63-64 — 書き込み検証がlen比較のみ。bytes.Equalでより堅牢化可能
- model/queue.go:107-115 — GetDoneConditions()がnilを返却。空スライス返却がより安全

---

## 4. テスト品質

**全体評価: 高品質。** テスト/ソース比1.9:1。t.Parallel使用1705箇所。goleak設定済み7パッケージ。testutil適切に構造化。

### Critical

| # | ファイル | 問題 | 改善提案 |
|---|---------|------|---------|
| 1 | envelope_test.go | U+2028/U+2029テストケース欠落 | テストケース追加 |
| 2 | quality_gate_evaluator.go:148 | mapイテレーション由来のSliceIsSortedが常にfalse(テスト不能な無駄分岐) | SliceIsSortedチェック削除 |

### Major

| # | ファイル | 問題 | 改善提案 |
|---|---------|------|---------|
| 3 | uds_test.go, lock_test.go | goroutine大量生成テストにgoleak未設定 | goleak_test.go追加 |
| 4 | panic_recovery_test.go:104-109 | time.Sleep(5ms)ポーリングループ(最大5秒)。CI負荷時にflaky risk | require.Eventuallyに統一 |
| 5 | tmux/input_separation_e2e_test.go:74-136 | time.Sleep(200ms/100ms)による複数ポーリング。タイミング依存が高い | ポーリング間隔堅牢化またはテストタグ分離 |
| 6 | agent/executor_test.go | message_deliverer.go, executor_core.go, process_manager.goに個別ユニットテストなし | 個別ユニットテスト追加 |
| 7 | agent/test_helpers_test.go:28-29 | mockPaneIOとcovMockPaneIOの2類似モック共存 | 統合 |
| 8 | debounce_controller_test.go:74 | time.Sleep(50ms)が「essential」コメント付き使用 | チャネルベース同期に変更 |

### テスト未設置パッケージ

| パッケージ | リスク | 推奨 |
|-----------|--------|------|
| pathutil | セキュリティ関連(パストラバーサル) | テスト追加推奨 |
| ptr | 小規模ユーティリティ | リスク低 |
| setup | 初期化処理 | 中程度リスク |

### Flaky Risk の高いテスト

- `internal/daemon/panic_recovery_test.go:104-109` — time.Sleepポーリング
- `internal/tmux/input_separation_e2e_test.go:74-136` — タイミング依存
- `internal/daemon/debounce_controller_test.go:74` — time.Sleep同期
- `internal/daemon/integration_test.go:1696` — intentionally slow subscriber
- `internal/daemon/spawn_tracked_test.go:70` — レース条件再現用Sleep

---

## 5. 設計・アーキテクチャ

**全体評価: 優秀。** 最小依存設計（直接依存5パッケージのみ）。DB・HTTPフレームワーク不使用。R1-R5 Reconcileパターンで状態整合性を構造的管理。パッケージ間循環依存なし。

### 設計上の強み

1. **ファイルベース状態管理 + UDS IPC** — シンプルで信頼性が高い
2. **Feature Flag設計** — bandit/evolution/search等の高度機能をサブパッケージとして分離、featuregateで段階的有効化
3. **スキル/ペルソナシステム** — 26スキル+4ペルソナでAgent行動を宣言的に制御
4. **Worktree隔離** — Worker間のファイル競合を物理的に隔離
5. **二層セキュリティ防御** — L1静的拒否 + L2動的hook

### Major懸念

| # | 問題 | 詳細 |
|---|------|------|
| 1 | Daemon責務集中 | 全体の36%(53K行)がdaemon系、20サブパッケージ。機能密度が高い |
| 2 | reconcile/r4_plan_status.go:25-77 | backoffマップがEngine再構築でリセットされる暗黙の前提 |
| 3 | plan/inject.go:146-178 | 全phaseがterminal時にphase[0]にタスク追加される設計欠陥 |
| 4 | reviewer/dispatcher.go:119-130 | reviewTask()がプレースホルダ実装で常に空結果返却(完了/未実装の区別不能) |
| 5 | plan/complete.go:208-241 | H3 conflict pathでintent.TaskResultsがスナップショットのためstaleリスク |
| 6 | fallback/manager.go:96,122 | ~~workerIDパラメータが完全に無視~~ → **再調査の結果、誤指摘**。`workers map[string]*workerState` で per-worker に連続失敗カウンタと `lastSuccessAt` を追跡しており、`IsWorkerAllowed`/`RecordSuccess`/`RecordFailure`/`LastSuccessAt` すべてで `workerID` が正しくキーとして使用されている。 |
| 7 | verification/ensemble.go:65-114 | ~~ドキュメント(Weight>=1.0ルール)と実装(全perspective均等扱い)が不一致~~ → **再調査の結果、誤指摘**。`Aggregate()` 内の `else if w >= 1.0 { passed = false }` が docstring/README/テスト (`TestAggregate_CriticalWeightFailed`, `TestAggregate_WeightBasedPassed`) と完全に整合。 |

---

## 6. セキュリティ・安全性

**全体評価: 良好。** 二層防御（L1静的拒否 + L2動的hook）は適切。Tier1 D001-D008全パターン網羅。

### Critical

| # | ファイル | 問題 | 改善提案 |
|---|---------|------|---------|
| 1 | envelope.go:58 | U+2028/U+2029（Unicode Line/Paragraph Separator）が未処理。unicode.IsControl()はこれらをControlカテゴリと判定しないため素通り。ASCII改行のみ変換でヘッダーインジェクション経路残存 | strings.Mapで\u2028,\u2029をスペースに変換+テスト追加 |

### Major

| # | ファイル | 問題 | 改善提案 |
|---|---------|------|---------|
| 2 | policy_checker.go:216-220 | D002 TOCTOU競合(symlink swap)。文書化済みだがカーネルレベル対策なし | リスク許容の明示的文書化(カーネルレベル対策は困難) |
| 3 | envelope.go:100-146 | フィールド長制限なしでDoS可能性 | 最大長バリデーション追加 |
| 4 | envelope.go:82-85 | SanitizeUserContentの呼出順序依存に型レベル強制なし | 型で呼出順序を強制(builder pattern等) |
| 5 | persona/persona.go:25-36 | ファイル内容に境界マーカー(--- BEGIN LEARNINGS等)がある場合のサニタイズ欠如 | 境界マーカーのエスケープ処理追加 |
| 6 | judge/judge.go:100-123 | LLM応答の検証なし(prompt injection経由で不正結果の可能性) | 応答のバリデーション追加 |

### セキュリティ防御の良好な実装

- **policy_checker.go** — Tier1 D001-D008全パターン網羅、バックティック・ANSI-C引用・プロセス置換を防御
- **launcher.go** — allowedToolsByRoleでrole別制限、fail-closed設計、環境変数フィルタでDYLD_*/LD_PRELOAD除去
- **envelope.go** — NFKC正規化+ゼロ幅文字除去18種+制御文字除去の多層防御（U+2028/U+2029を除く）
- **tmux/session.go:278-302** — セッション名サニタイズ適切
- **tmux/session.go:199-249** — SendTextAndSubmitはload-buffer方式でエスケープ回避
- **worktree** — validate.ID()+ensureWithinProjectRoot()でパストラバーサル防御、exec.Commandでシェル注入不可

---

## 7. その他

### ドキュメントと実装の乖離

| 深刻度 | 問題 | 詳細 |
|--------|------|------|
| Critical | planner.md CLIフラグ未記載 | maestro plan add-task --target-phase (cmd_plan_tasks.go:79), --idempotency-key (cmd_plan_tasks.go:80) |
| Critical | planner.md サブコマンド未記載 | resolve-conflict (cmd_plan_ops.go:189-239), unquarantine (cmd_plan_ops.go:90-116), resume-merge (cmd_plan_ops.go:120-144), retry-publish (cmd_plan_ops.go:148-172) |
| Major | add-retry-taskフラグ差異未文書化 | --required, --constraints, --persona-hint, --tools-hint, --skill-refs, --worker-idをサポートしない |
| Major | config.yamlテンプレート13項目欠落 | retry系11項目(signal_inline_retries等), circuit_breaker.half_open_delay_sec, watcher.shell_ready_timeout_sec |

**整合が確認された箇所**: ペルソナ定義(4種)とlauncher.goの注入処理は完全整合。スキル定義(22種)とdaemon/skill/の読込・注入は完全整合。

### ログ品質

| 深刻度 | ファイル | 問題 | 改善提案 |
|--------|---------|------|---------|
| Critical | plan/retry.go | state restore failedが9箇所以上で同一メッセージ出力。カスケード障害時にログスパム | rate-limited logger使用 |
| Critical | plan/submit.go:250,261,345,356 | ロールバック失敗をERROR記録するがシステム状態(復旧可否)の情報なし | 復旧可否情報を含めたログ |
| Major | queue_scan_collect.go | qh.log()とslog直接呼び出しが混在 | 構造化ログに統一 |
| Major | queue_scan_apply.go | dispatch_fence_staleがWARNだがフェーズ遷移時の正常動作 | DEBUGに変更 |
| Minor | quality/evaluators.go:520 | スクリプト実行ログにtmpDirパスを含む | 内部パス情報を除去 |

### パフォーマンス懸念

| 深刻度 | ファイル | 問題 | 改善提案 |
|--------|---------|------|---------|
| Critical | queue_scan_collect.go:40,125 | ホットパスでTask/Command構造体(20+フィールド,複数スライス)の値コピー | ポインタ参照で回避 |
| Critical | yaml/atomic.go:66-68 | 毎回の原子書込でフルYAML Unmarshalバリデーション。スキャンサイクルあたり100回以上 | バリデーションの条件付き実行またはキャッシュ |
| Major | queue_scan_collect.go:74等 | time.Parse(RFC3339)をホットパスで繰り返し | パース結果のキャッシュ |
| Major | model/config_load.go:13-24 | LoadConfigが毎回ディスク読取+YAML Unmarshal | キャッシュ機構追加 |
| Major | worktree/git_operations.go:57-92 | gitエラー分類でstrings.Containsを13パターン逐次検査 | 正規表現への統合 |
| Minor | yaml/atomic.go:50,84,102,150,166 | 単一書込操作で複数fsync呼出 | fsync呼出の最適化 |

---

## レビュー実施タスク一覧

| # | タスク名 | 対象 | 結果 |
|---|---------|------|------|
| 1 | map-codebase | 全コードベース構造調査 | 完了 |
| 2 | review-daemon-core | Daemon本体・キュースキャン・ディスパッチ・リース | Major 5, Minor 8, Info 3 |
| 3 | review-reconcile-plan | Reconcile(R1-R5)・Plan・Scheduler | Critical 2, Major 6, Minor 4 |
| 4 | review-worktree | Worktree管理全般 | Critical 2, Major 8, Minor 5 |
| 5 | review-agent-security | Agent・ポリシーフック・セキュリティ | Critical 1, Major 5, Minor 9, Info 7 |
| 6 | review-cli-model-infra | CLI・UDS・Model・YAML I/O・ロック | Major 13, Minor 10 |
| 7 | review-features | 14サブパッケージ(bandit/evolution等) | Critical 8, Major 20, Minor 15, Info 5 |
| 8 | review-tests | テスト品質(219ファイル) | Critical 2, Major 6, Minor 5, Info 3 |
| 9 | review-docs-cross-cutting | ドキュメント乖離・ログ・パフォーマンス | Critical 5, Major 5, Minor 4 |
