# Go コード品質レビュー (Part: code_quality)

調査範囲: `internal/**/*.go` および `cmd/maestro/**/*.go` (テスト `*_test.go` 除く)
調査手法: Explore SubAgent 3 並列 (agent+daemon / queue系 daemon サブセット / 残り全体) による Read/Grep 走査
エビデンスレベル: 全項目「参照済み」(コード読解で確認、実行検証は未実施)

---

## 修正対応メモ

2026-04-27 の修正で 4.1 / 4.2 / 4.6 / 4.7 を実装対応済み。追加で 4.3 / 4.4 / 4.5 / 4.8 / 4.12 / 4.16 と、1.2 / 3.6 の局所リファクタを反映した。大型ファイル分割などの構造変更は別作業向けに残す。

---

## 1. 冗長コード

### 1.1 worktree manager のエラーハンドリングパターン重複
- **ファイル**: `internal/daemon/worktree/recover_resume.go:129-193, 227-229, 346-349, 370-372, 391-393`
- **関数/型**: `finalizeResumeMergeIntegrationStatus()`, `tryMergeWorker()`, `resetWorkersToActive()`
- **現状の概要**: 「state 遷移呼び出し → エラー時 Warn ログ + return/continue」というパターンが 6 箇所以上で同一構造で繰り返される
- **問題**: コピー&ペーストによる保守負荷。ログフォーマット文字列が個別に定義されており、揺れが入りやすい
- **推奨対応**: `logAndReturnOnStatusError(state, status, fmtStr, args...)` 等のヘルパー抽出
- **深刻度**: Medium

### 1.2 cancelMark / busy check の条件チェック→ログ→continue 繰り返し
- **ファイル**: `internal/daemon/queue_scan_phase_c.go:95-114`
- **関数**: `applyCancelDispatchAndBusyChecks()`
- **現状の概要**: cancelMarks のループ内で「条件 → ログ → continue」が 3 回連続
- **問題**: スコープが狭く影響は限定的だが、共通サブルーチン抽出余地あり
- **推奨対応**: `findTaskInQueue(tq, taskID)` ヘルパー + cancel 処理サブ関数化
- **深刻度**: Low

### 1.3 execDeliver の orchestrator/planner 重複ロジック
- **ファイル**: `internal/agent/executor_core.go:572-629`
- **関数**: `execDeliver`
- **現状の概要**: orchestrator 分岐と planner/other 分岐で `ensureClaudeRunning → detectBusyWithRetry → sendAndConfirm` の同一構造を別個に記述
- **問題**: DRY 違反。busy check 詳細度が違うだけで全体構造は同一
- **推奨対応**: `deliverWithBusyCheck()` 共通メソッド抽出 + orchestrator 固有ログを分岐
- **深刻度**: Medium

### 1.4 isPromptReady と isAgentReady の prompt スキャン重複
- **ファイル**: `internal/agent/tmux_pane_io.go:130-157`, `internal/agent/process_manager.go:211-227`
- **関数**: `isPromptReady`, `isAgentReady`
- **現状の概要**: `maxPromptSearchLines` 行を逆向きスキャンし trimmed prompt マーカー (❯/>) を探すロジックが 2 箇所に分散
- **問題**: 軽微な重複だが prompt 仕様変更時に修正漏れリスク
- **推奨対応**: `isPromptReady()` を `isAgentReady()` の claude-code 分岐から再利用
- **深刻度**: Low

---

## 2. デッドコード

### 2.1 Manager.AutoCommit / AutoMerge の未呼び出し
- **ファイル**: `internal/daemon/worktree/manager.go:877-880`
- **関数**: `(*Manager) AutoCommit() bool`, `(*Manager) AutoMerge() bool`
- **現状の概要**: いずれも `wm.config.AutoCommit/AutoMerge` を返すだけのアクセサ
- **問題**: Grep で呼び出し元 0 件 (推定: 直接呼び出しのみ調査)。`worktreeManager` インターフェース (`internal/daemon/queue_handler_deps.go:147-148`) には宣言されているが実利用なし
- **推奨対応**: 削除またはインターフェースから外す
- **深刻度**: Low

### 2.2 (備考) 第 1 / 第 2 Explore は agent / daemon 主要部分のデッドコードを「該当なし」と報告
- 広範囲のシンボル逆引きは時間制約で全数実施できておらず、追加調査の余地あり (リフレクション/インターフェース呼び出し含む網羅は未実施)

---

## 3. リファクタリング候補

### 3.1 800 行超の大型ファイル群
- **ファイル**:
  - `internal/daemon/worktree/manager.go` (約 880 行)
  - `internal/tmux/session.go` (約 864 行)
  - `internal/agent/launcher.go` (約 832 行)
  - `internal/daemon/worktree/merge_publish.go` (約 783 行)
- **現状の概要**: 単一ファイルで複数の責務 (lifecycle, status, IO, merge, publish 等) を内包
- **問題**: 可読性とテスト性低下、変更影響範囲の特定が困難
- **推奨対応**: 機能単位でサブファイル分割 (`manager_lifecycle.go`, `manager_status.go` 等)
- **深刻度**: Medium

### 3.2 Launch 関数の 4 段階責務集約
- **ファイル**: `internal/agent/launcher.go:82-145`
- **関数**: `Launch`
- **現状の概要**: 64 行内に「pane 変数読み込み → system prompt 構築 → CLI args 構築 → runtime 起動」が直列配置
- **問題**: エラーハンドリングが各段階で異なり (return / log+fallback)、ユニットテストが困難
- **推奨対応**: `loadAgentConfig()`, `buildPrompt()`, `buildAndLaunchArgs()` 等への分割
- **深刻度**: Medium

### 3.3 buildLaunchArgs の固定 6 段階チェーン
- **ファイル**: `internal/agent/launcher.go:270-279`
- **関数**: `buildLaunchArgs`
- **現状の概要**: `launchArgsCore → appendAllowedTools → appendDisallowedTools → appendNotificationSettings → appendWorkspaceReadAllowances → appendResolvedMaestroBashAllowances` の固定順チェーン
- **問題**: 新規追加が末尾固定。順序依存が暗黙的でテスト時の組み立て自由度が低い
- **推奨対応**: `LaunchArgsBuilder` 構造体 + method chain 化、または options 構造体導入
- **深刻度**: Medium

### 3.4 stepPlannerSignalsDeferred の 4 レベルネスト
- **ファイル**: `internal/daemon/queue_dispatch.go:22-139`
- **関数**: `stepPlannerSignalsDeferred`
- **現状の概要**: Phase-level / Command-level の判定が if-if-if-else の 4 段ネスト (37-113 行)
- **問題**: Phase/Command 判定ロジックが条件式に隠蔽され、フローが追いにくい
- **推奨対応**: Phase-level と Command-level 処理を独立メソッドに分割
- **深刻度**: Medium

### 3.5 applyCancelDispatchAndBusyChecks の 4 レベルネスト
- **ファイル**: `internal/daemon/queue_scan_phase_c.go:90-121`
- **関数**: `applyCancelDispatchAndBusyChecks`
- **現状の概要**: `if cancelMarks > 0 → for marks → for tasks → if id 一致` の 4 段ネスト
- **問題**: 内部ループは「task ID で task ポインタを引く」だけで抽出可能
- **推奨対応**: `findTaskInQueue(tq, taskID) *Task` ヘルパー抽出
- **深刻度**: Medium

### 3.6 applyMergeResultSignals の複雑分岐
- **ファイル**: `internal/daemon/queue_scan_phase_c.go:231-301`
- **関数**: `applyMergeResultSignals`
- **現状の概要**: `MarkPhaseMerged` 判定が 7 つのコメント段落で説明される複雑条件
- **問題**: 条件意図が自明でなく、コメントなしでは可読性が極めて低い
- **推奨対応**: `isStatusSafeForMarkMerged(status) bool` 関数抽出 + ユニットテスト
- **深刻度**: Medium

### 3.7 maybeAutoRecoverAfterResolution の switch 分岐
- **ファイル**: `internal/daemon/result_write_handler.go:181-232`
- **関数**: `maybeAutoRecoverAfterResolution`
- **現状の概要**: 復帰アクションの switch が複数段階の条件判定を内包
- **問題**: ケース内処理が肥大
- **推奨対応**: ケース毎にヘルパーメソッド抽出
- **深刻度**: Low

---

## 4. バグの可能性

### 4.1 Phase C scan: Load エラー時に zero 値で後続処理続行
- **ファイル**: `internal/daemon/queue_scan_phase_c.go:22-50`
- **関数**: `executeScanPhaseCBody`
- **現状の概要**:
  ```go
  commandQueue, commandPath, err := qh.queueStore.LoadCommandQueue()
  if err != nil { qh.log(...); /* err は捨てて続行 */ }
  taskQueues, err := qh.queueStore.LoadAllTaskQueues()
  if err != nil { qh.log(...); /* 同上 */ }
  notificationQueue, notificationPath, err := qh.queueStore.LoadNotificationQueue()
  if err != nil { qh.log(...); /* 同上 */ }
  signalQueue, signalPath, err := qh.queueStore.LoadPlannerSignalQueue()
  if err != nil { qh.log(...); /* 同上 */ }
  ...
  signalIndex := buildSignalIndex(signalQueue.Signals) // Load 失敗時は空の signalQueue で処理継続
  qh.applyCancelDispatchAndBusyChecks(..., commandQueue, commandPath, taskQueues, notificationQueue, notificationPath)
  qh.syncIdleAfterPhaseC(commandQueue, taskQueues, notificationQueue)
  ```
- **問題**:
  - Load エラー時、戻り値の zero 値 (struct / nil map / nil slice) のまま applyCancelDispatch/sync/buildSignalIndex を呼ぶ。Go の nil slice/map range は安全なため `signalQueue.Signals` 自体は panic しないが、キュー内容が「空」として扱われ、dispatch / busy / signal / metrics / dashboard の反映が欠落または誤反映される可能性がある。
  - エラーログのみで `err` を上位に返さない設計のため、呼び出し側が Phase C を失敗扱いにできない。
- **推奨対応**: 各 Load エラーで early return し、その scan cycle の apply / metrics / dashboard 更新を中止する。部分継続する場合でも、ロード成功したキューだけを処理対象にする明示的な success flag を持たせる。
- **深刻度**: High (panic ではなく、失敗キューを空として扱う状態反映リスク)

### 4.2 confirmSubmittedOrRetry のサイレント失敗
- **ファイル**: `internal/agent/message_deliverer.go:147-182`
- **関数**: `confirmSubmittedOrRetry`
- **現状の概要**: `CapturePaneJoined` のキャプチャエラー時に `return nil` (line 159)。プローブ予算 exhausted 時も `return nil` (line 181)
- **問題**: 双方とも caller に「成功」シグナルを返すため、配信失敗が隠蔽される。コメント (F-016) によれば double plan_submit 防止の意図的設計だが、silent failure リスクが残る
- **推奨対応**: 中間結果型 (`SubmitProbeResult{ProbeAttempted/NoProbeNeeded/ProbeExhausted/ProbeError}`) を返し、caller に retry 判断を委ねる
- **深刻度**: Critical (intentional だが配信欠落の根本原因)

### 4.3 ensureWorkingDir の partial failure による state 不整合
- **ファイル**: `internal/agent/process_manager.go:84-143`
- **関数**: `(*processManager) ensureWorkingDir`
- **現状の概要**: `RespawnPane` → `waitForShell` → `ResetClearReady` → Claude relaunch → `waitReadyStrict` → `SetCWD` の段階実行。途中 error 時はその場で return する。
- **問題**: `RespawnPane` 後かつ `SetCWD` 前に失敗すると、実 pane は新しい workingDir 側へ移動している一方で paneState の `cwd` は旧値のまま残る。次回配送で不要な respawn が再実行される可能性がある。ただし `cwd` が旧値のため「一致と誤判定して配送する」経路ではない。
- **推奨対応**: step 完了状態を track し、失敗時に paneState を明示的に unknown/dirty 扱いへ更新する。`SetCWD` 失敗は warning のみで握りつぶさず、再同期可能な状態にする。
- **深刻度**: Medium (state corruption というより再同期不全 / 不要 respawn リスク)

### 4.4 deliverPlannerSignal の retry ループでの cancel/context 取り扱い
- **ファイル**: `internal/daemon/queue_dispatch.go:237-274`
- **関数**: `deliverPlannerSignal`
- **現状の概要**:
  ```go
  for attempt := 0; attempt <= maxRetries; attempt++ {
    if attempt > 0 {
      select { case <-ctx.Done(): return ...; case <-time.After(retryDelay): }
    }
    attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
    err := qh.deliverPlannerSignalOnce(attemptCtx, ...)
    cancel()
    ...
  }
  ```
- **問題**:
  - `cancel()` の即時呼び出し自体は timer 解放として妥当だが、panic 時には実行されない。
  - ループ先頭で `ctx.Err()` 確認がないため、外側 ctx キャンセル直後の attempt 0 でも `deliverPlannerSignalOnce` まで進む。
- **推奨対応**: ループ先頭で `ctx.Err()` を確認する。panic-safe にするなら attempt 本体を小さなクロージャに閉じ込めて `defer cancel()` するが、通常経路では現在の即時 `cancel()` を遅らせない。
- **深刻度**: Medium

### 4.5 dependencyResolver.GetStateManager() の nil 戻り値リスク
- **ファイル**: `internal/daemon/queue_scan_collect.go:362-365` 付近 (`checkInProgressDependencyFailuresDeferred`)
- **現状の概要**: `if HasStateReader() { GetStateManager().UpdateTaskState(...) }` の構造
- **問題**: `HasStateReader()` が true でも `GetStateManager()` が nil を返す実装可能性。インターフェース契約が緩く、nil チェックなしで .UpdateTaskState を呼ぶ
- **推奨対応**: 戻り値 nil チェック or インターフェース契約を `GetStateManager()` 単独で nil 返さないと明文化
- **深刻度**: Medium

### 4.6 リカバリ用 setIntegrationStatus のエラー無視
- **ファイル**: `internal/daemon/worktree/recover_resume.go:129-193`
- **関数**: `finalizeResumeMergeIntegrationStatus`
- **現状の概要**:
  ```go
  if tErr := wm.setIntegrationStatus(state, IntegrationStatusMerged, now); tErr != nil {
    wm.Log(Warn, "...", tErr)
    _ = wm.setIntegrationStatus(state, IntegrationStatusFailed, now) // ← err 無視
  }
  ```
- **問題**: リカバリ自体が失敗した場合、状態が未確定のまま続行。ログにもリカバリ失敗が残らない
- **推奨対応**: リカバリ err は必ず Log + 上位伝播 (Phase A 再試行のため)
- **深刻度**: High

### 4.7 ResumeMerge: 部分状態変更後の saveState 漏れ
- **ファイル**: `internal/daemon/worktree/recover_resume.go:116-143`
- **関数**: `ResumeMerge`
- **現状の概要**: 行 116-140 で複数の状態遷移を実行後、行 141-143 で `saveState` 呼び出し。途中の遷移で error が返ると saveState 前に return
- **問題**: メモリ上の `state` には partial 変更が残ったまま。次回呼び出し時に古い state から復帰される設計上の脆弱性
- **推奨対応**: トランザクション的アプローチ (snapshot+rollback)、または saveState を `defer` で確実化
- **深刻度**: High

### 4.8 clearAndConfirm の連続キャプチャエラーで timeout まで待ち続ける
- **ファイル**: `internal/agent/message_deliverer.go:225-300, 365-399`
- **関数**: `clearAndConfirm`, `clearConfirmationPoller.poll`
- **現状の概要**: poll 内のキャプチャエラーは `p.reset()` のみで stableCount を 0 にリセットして続行
- **問題**: 同じキャプチャエラーが連続発生する状況では、無限ループではないが timeout まで成功不能な poll を続ける。`ClearMaxAttempts` 分だけ遅延が積み上がる可能性がある。
- **推奨対応**: 連続 N 回のキャプチャエラーで early abort (max error budget)
- **深刻度**: Medium

### 4.9 daemon_shutdown.go の defer 順序と goroutine 連携
- **ファイル**: `internal/daemon/daemon_shutdown.go:44-60`
- **関数**: `waitSignals`
- **現状の概要**:
  ```go
  shutdownDone := make(chan struct{})
  var closeShutdownDone sync.Once
  defer closeShutdownDone.Do(func(){ close(shutdownDone) })
  go func(){ select { case <-sigCh: ...; case <-shutdownDone: return } }()
  ```
- **問題**: 通常フローでは問題ないが、panic 時に defer 実行と goroutine 終了の順序が不確定
- **推奨対応**: 明示的 close + context cancel に統一して goroutine ライフサイクルを単純化
- **深刻度**: Medium

### 4.10 context.Background() のロガー直接利用
- **ファイル**: `internal/daemon/quality_gate.go:126`, `internal/daemon/core/core.go:100, 108` 他
- **現状の概要**: `dl.slogger.Log(context.Background(), ...)` の形でログ呼び出し
- **問題**: Daemon shutdown ctx が利用可能なケースでも捨てているため、シャットダウン中もログ出力が継続。slog の trace 連携にも影響
- **推奨対応**: 利用可能な ctx (d.ctx) を引き回す
- **深刻度**: Medium

### 4.11 PeriodicScan / debounce で context.Background() 生成
- **ファイル**: `internal/daemon/scan_orchestrator.go:39-43`, `internal/daemon/debounce_controller.go:100+`
- **現状の概要**: shutdownCtx が nil の場合に `context.Background()` を生成
- **問題**: 非テストコードでも nil フォールバック経路が存在し、shutdown 伝播が遮断される可能性
- **推奨対応**: 初期化時に shutdownCtx を必須化、テスト用フォールバックは build tag で隔離
- **深刻度**: Low (現状はテスト互換のための意図的設計)

### 4.12 busyDetector.softRetryUndecided の cancel 原因区別なし
- **ファイル**: `internal/agent/busy_detector.go:243-272`
- **関数**: `softRetryUndecided`
- **現状の概要**: `sleepCtx` cancel で即 `VerdictBusy` 返却 (intentional)
- **問題**: cancel 原因が shutdown か内部 timeout か区別できない。timeout 中の shutdown は本来 Undecided 維持で十分なケースも Busy 昇格される
- **推奨対応**: `context.Cause()` (Go 1.20+) で原因判定。または明示コメントで設計意図を残す
- **深刻度**: Medium

### 4.13 clearConfirmationPoller hash stability 判定の race
- **ファイル**: `internal/agent/message_deliverer.go:365-399`
- **関数**: `clearConfirmationPoller.poll`, `isConfirmed`
- **現状の概要**: `hashChanged` は一度 true になると不可逆。stableCount は同 hash 連続でインクリメント。`hashChanged && stableCount >= 2` で confirmed 判定
- **問題**: pane content が deterministic cycle (hash 変化後に元値へ戻る) の場合 false positive で confirmed 判定。実運用では `clearTextVisible()` 主条件のため発火稀
- **推奨対応**: clearTextVisible() を主条件に明文化、hash check は補助で OK
- **深刻度**: Medium (実運用では Low)

### 4.14 isAgentBusy の undecidedTracker nil 経路
- **ファイル**: `internal/daemon/queue_dispatch.go:327-380`
- **関数**: `isAgentBusy`
- **現状の概要**: `undecidedTracker == nil` の場合 count = 0 のまま閾値比較で常に false
- **問題**: 警告ログが出ない経路。undecidedTracker 未初期化時のテスト時に逆に「正常扱い」される
- **推奨対応**: nil-safe ヘルパー or 初期化保証
- **深刻度**: Low

### 4.15 events.bus の Subscribe 即時 cancel 時のイベント欠落
- **ファイル**: `internal/events/bus.go:182, 241`
- **関数**: `Subscribe`, `SubscribeCoalesced`
- **現状の概要**: ctx.Done() 時 `for range sub.ch {}` で drain
- **問題**: 設計上 at-most-once。重要 event は registration 直後 cancel で失われる
- **推奨対応**: Critical event に at-least-once 経路を別途設ける
- **深刻度**: Low

### 4.16 resultWritePhaseA のエラーラップ抜け
- **ファイル**: `internal/daemon/result_write_phase_a.go:83-86`
- **関数**: `resultWritePhaseA`
- **現状の概要**: `&resultWriteError{ErrCodeInternal, err.Error()}` で string 化
- **問題**: errors.Is/As チェーンが切れ、原因解析時に root cause 追跡不可
- **推奨対応**: `fmt.Errorf("...: %w", err)` か独自 error type に Unwrap 実装
- **深刻度**: Low

---

## 統計サマリ

| 観点 | 件数 |
|------|------|
| 1. 冗長コード | 4 |
| 2. デッドコード | 1 (+情報 1) |
| 3. リファクタリング候補 | 7 |
| 4. バグの可能性 | 16 |
| **合計** | **28** |

### 深刻度別

| 深刻度 | 件数 | 主な対象 |
|--------|------|---------|
| **Critical** | 1 | 4.2 confirmSubmittedOrRetry silent failure |
| **High** | 3 | 4.1 Phase C Load error 継続 / 4.6 リカバリ err 無視 / 4.7 ResumeMerge partial state |
| **Medium** | 16 | 冗長コード 2 件、リファクタ候補 6 件、バグ可能性 8 件 |
| **Low** | 8 | 冗長コード 2 件、デッドコード 1 件、リファクタ候補 1 件、バグ可能性 4 件 |

### 最優先対応 (Critical/High 優先、Medium から 1 件補足)

1. `internal/agent/message_deliverer.go:147-182` — confirmSubmittedOrRetry に SubmitProbeResult を導入
2. `internal/daemon/queue_scan_phase_c.go:22-50` — Load エラー時は early return し、空キュー扱いでの後続反映を止める
3. `internal/daemon/worktree/recover_resume.go:129-193` — リカバリ err の無視を解消
4. `internal/daemon/worktree/recover_resume.go:116-143` — ResumeMerge の partial state / saveState 漏れを解消
5. `internal/agent/process_manager.go:84-143` — ensureWorkingDir の再同期不全リスクを低減 (Medium)

---

## エビデンス注記

- 全項目は Read/Grep に基づく**参照済み**レベル。実行検証 (panic 再現等) は未実施
- デッドコード判定 (2.1) は Grep 逆引きで呼び出し元 0 件確認済み。ただしリフレクション/インターフェース経由の呼び出しは網羅できていない (**推定**: 直接呼び出しはなし)
- 大型ファイル行数 (3.1) は wc 由来でなく Explore 報告値の概算 (`±10 行` 程度の誤差を含みうる)
- 第 1〜3 Explore 間で重複検出された項目は上位深刻度を採用 (例: 4.13 と 4.8 は別観点だが類似領域)
- **永続化制約**: タスクが指定する `.maestro/results/review_part_code_quality.md` への書き込みは Worker policy hook により拒否されるため、本ファイルは `tmp/sections/review_part_code_quality.md` に退避保存している
