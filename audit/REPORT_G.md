# 観点G 監査レポート: リファクタリング候補

**監査対象**: maestro_v2 の `internal/` 配下プロダクションコード (test ファイル除外)。
**監査スタンス**: 修正は行わない (Read のみ)。コード断片の提案を含めない (タスク制約)。
**メトリクス取得方法**:
- 関数長は `^func ... {` から対応する閉じ `}` までを brace tracking で算出 (`ACTUAL_LINES`)。`^func ` 単純距離 heuristic は package level 定数を含むため使わない。
- 引数数は `^func ... ( ... )` の `,` 区切り個数で近似 (型パラメータの `[T comparable]` 等で過大計上の可能性あり)。
- magic number は `[0-9]+\s*\*\s*time\.(Second|Minute|Hour)` および inline if-比較を grep。
- magic string は `filepath.Join(maestroDir, "<dir>", ...)` 形式の頻度を grep。
**日付**: 2026-05-08

---

## 0. サマリ判定 (5 項目別)

| ID | 観点 | 判定 |
|----|------|------|
| G1 | 関数が長すぎる / 責務過多 | **問題あり** (8 件) |
| G2 | パッケージ間の循環依存・抽象漏れ | **問題なし (循環なし) + 要確認 (構造的肥大 1 件)** |
| G3 | 命名が実装と乖離 | **要確認** (3 件) |
| G4 | magic number / magic string の点在 | **問題あり** (5 件) |
| G5 | 早期 return 連鎖で本流が圧迫されている | **問題あり** (3 件) |

合計 finding 数: **20 件** (G1=8, G2=1+1, G3=3, G4=5, G5=3)。

---

## 1. 重大度凡例

| Severity | 意味 |
|----------|------|
| **critical** | バグ温床。条件分岐の取りこぼしや race を生みやすく、可読性低下が致命的 |
| **major**    | 将来の改修コスト増・テスト被覆度低下を招く |
| **minor**    | 局所的な可読性問題 |
| **nit**      | 表現・命名の改善余地のみ |

---

## 2. G1 — 長すぎる関数 / 責務過多

### G1-1 [critical] `collectWorktreePublishAndCleanup` (355 行 / 単一関数)

- **対象**: `internal/daemon/queue_scan_helpers.go:462` (`func (qh *QueueHandler) collectWorktreePublishAndCleanup(commandID string, commandContent string, taskQueues map[string]*taskQueueEntry) ([]worktreePublishItem, []worktreeCleanupItem)`)
- **メトリクス**: ACTUAL_LINES=355、引数 3 個、戻り値 2 種、`switch cmdState.Integration.Status` (462-823 内に存在)、`if hasFailed` 分岐 + integration status 5 ステータス分岐
- **責務**:
  1. 全タスク terminal 判定 (`checkCommandTasksTerminal`)
  2. 状態側 non-terminal gate (`HasNonTerminalTaskState`)
  3. phase terminal 判定
  4. failed 派生 + synthetic_failed_planner_result 書き出し
  5. integration status (`Quarantined` / `Merged` / `PublishFailed` / etc.) で publish vs cleanup 分岐
  6. cleanup 種別の決定 (`success` / `failure` / dirty)
- **重大度**: critical — このサイズの関数を変更する PR は局所安全性を主張できない
- **リファクタ方向**:
  - 5 ステップの top-level decision tree を `step1AllTerminal` / `step2StateGate` / `step3PhaseGate` / `step4FailureBranch` / `step5IntegrationSwitch` に分割し、それぞれが `(continue|return)` の 2 値を返す short-circuit パイプラインへ
  - integration status の switch をディスパッチテーブル (map[Status]func) 化
  - `hasFailed` 派生コードブロックを `derivePlanFailureFromState` ヘルパに切り出し
- **分類**: 設計議論が必要 (state 駆動の switch 構造そのものに踏み込むため)
- **根拠**: 上記行 + `awk` brace tracking で `ACTUAL=355`

### G1-2 [major] `Complete` (246 行)

- **対象**: `internal/plan/complete.go:193` (`func Complete(opts CompleteOptions) (*CompleteResult, error)`)
- **メトリクス**: ACTUAL_LINES=246、引数 1 個 (struct)、エラーチェック 11 件、内部分岐: intent recovery / idempotency / auto-cancel paused_for_replan / can-complete / advancePhasesInline / executeCompleteSteps / deferred_publish 経路
- **責務**:
  1. `LockMap` / `commandID` バリデーション
  2. lock 順序 (queue → state → result)
  3. intent ファイル復旧
  4. idempotency check
  5. auto-cancel paused_for_replan
  6. CanComplete + worktreeNotPublishedError 経路
  7. executeCompleteSteps への委譲
- **重大度**: major — 関数本体が 6 種類の orthogonal concern を直列に実行し、各分岐が独立したエラーハンドリング
- **リファクタ方向**:
  - 各 step を `step()` メソッドへ分割し、`Complete` は orchestrator に縮約
  - intent recovery / idempotency / auto-cancel を pre-execution hook 列にできる
- **分類**: すぐ着手可能 (各ブロックが既に明示的に区切られている)

### G1-3 [major] `(*Tracker).ObserveVerdict` (236 行)

- **対象**: `internal/daemon/paneactivity/tracker.go:707`
- **メトリクス**: ACTUAL_LINES=236、引数 4 個、内部に 4 系統の fast path + tail-hash 比較 + uncertain streak 制御
- **責務**:
  1. terminal-error fast path (Claude API 4xx 等)
  2. context-budget-exhausted fast path (>=97% 利用)
  3. blocked-prompt fast path (`blockedRe` / `defaultBlockedTailRegex`)
  4. unmatched hint diagnostic logging
  5. 通常の hash + busy_pattern + active hint 評価
  6. `uncertainStreak` のキャップ判定
- **重大度**: major — verdict の意味論を支配する関数が 4 fast path + 1 default path で 236 行。新しい verdict を追加する際の影響範囲が読み解けない
- **リファクタ方向**:
  - `[]VerdictRule{ TerminalErrorRule, ContextBudgetRule, BlockedRule, ActiveHintRule, HashRule }` 列を順次評価する形に再構成
  - 各 rule は `(matched bool, verdict Verdict, sideEffect func())` を返す純粋関数化
- **分類**: 設計議論が必要 (verdict precedence 仕様が現コードに implicit に埋まっており、外部化に判断要)

### G1-4 [major] `(*QueueHandler).collectExpiredTaskBusyChecks` (229 行)

- **対象**: `internal/daemon/queue_scan_collect.go:482`
- **メトリクス**: ACTUAL_LINES=229、引数 4 個、`for _, idx := range expired` 内に nested switch + 6 種の verdict 分岐 (TerminalError / Blocked / Active / Idle / Uncertain / no agent)
- **責務**:
  1. malformed entry release
  2. agentID 空 release
  3. max_in_progress_min cap log
  4. pane verdict による分岐 (5 種)
  5. busy-check fallback
- **重大度**: major
- **リファクタ方向**:
  - inner-loop body を `handleExpiredTask(...)` メソッドに切り出し、verdict ごとの handler に dispatch
- **分類**: すぐ着手可能

### G1-5 [major] `(*QueueHandler).collectPendingTaskDispatches` (191 行) + 引数 6 個

- **対象**: `internal/daemon/queue_scan_collect.go:96`
- **メトリクス**: ACTUAL_LINES=191、引数 6 個 (`tq, workerID, globalInFlight, inFlightPaths, allTaskQueues, work`)、内部 gate チェーン: dependency → pre-merge gate → system-commit gate → markTaskReady → path-overlap → admission → lease acquire
- **責務**: dispatch eligibility の 7 段階 gate 評価
- **重大度**: major
- **リファクタ方向**:
  - 引数を `dispatchContext` struct に集約 (tq / workerID / globalInFlight / inFlightPaths / allTaskQueues / work)
  - gate チェーンを `[]gateFn{ depGate, preMergeGate, sysCommitGate, ... }` に外出しして fallthrough 順を宣言的に
- **分類**: すぐ着手可能 (引数集約は機械的なリファクタ)

### G1-6 [major] `(*Manager).EnsureWorkerWorktree` (175 行 / error 21 件)

- **対象**: `internal/daemon/worktree/manager.go:113`
- **メトリクス**: ACTUAL_LINES=175、`if err != nil` 21 件、3 つの ad-hoc rollback closure (`rollbackIntegration`, save-state rollback, attach-worker rollback)
- **責務**:
  1. ID 検証
  2. state ロード
  3. state 不在 → integration branch + integration worktree + worker worktree の atomic 作成
  4. state 存在 → 既存 worker チェック + worker 追加
  5. saveState 失敗時の cascading rollback
- **重大度**: major
- **リファクタ方向**:
  - state 不在 path と state 存在 path を別関数 (`createNewIntegrationAndWorker` / `attachWorkerToExistingState`) へ分離
  - rollback closure を `worktreeRollback` 構造体に集約 (Add(opName, fn) で stack pop の逆順実行)
- **分類**: 設計議論が必要 (rollback の atomicity 仕様を保つ責務がある)

### G1-7 [minor] `(*QueueHandler).stepAwaitingFillWatchdog` (172 行)

- **対象**: `internal/daemon/queue_scan_phase_a_phase.go:357`
- **メトリクス**: ACTUAL_LINES=172、外側ループ + nested phase ループ + 5 種の continue 条件
- **責務**: command × phase の二重ループでawaiting_fill 経過時間をチェックし、threshold / pane verdict / 連続発火数 ごとに WARN / DEBUG / 状態書き込みを切り替え
- **重大度**: minor
- **リファクタ方向**: phase 単位の判定本体 (`evaluatePhaseAwaitingFill(phase, cmd, now)`) と side-effect (`firePhaseStallSignal`) を分離
- **分類**: すぐ着手可能

### G1-8 [minor] `(*QueueHandler).collectWorktreePhaseMerges` (161 行)

- **対象**: `internal/daemon/queue_scan_helpers.go:246`
- **メトリクス**: ACTUAL_LINES=161、引数 2 個、phase 単位の merge 計画立案
- **重大度**: minor
- **リファクタ方向**: phase 単位の決定 (`shouldMergePhase` / `buildMergeItems`) を切り出し
- **分類**: すぐ着手可能

---

## 3. G2 — 循環依存・抽象漏れ

### G2-A 循環依存 [問題なし]

- **検査**: `agent → daemon`, `plan → daemon` の逆向き import を grep
- **結果**:
  - `internal/agent/*.go` 内で `"github.com/msageha/maestro_v2/internal/daemon` を import する非テストファイル: **0 件**
  - `internal/plan/*.go` 内で `"github.com/msageha/maestro_v2/internal/daemon` を import する非テストファイル: **0 件**
- **判定**: **問題なし**。既存の依存方向 (daemon → plan / agent / model / lock など) は片方向で循環は無い
- **根拠**: `grep -l '"github.com/msageha/maestro_v2/internal/daemon' internal/agent/*.go` と `internal/plan/*.go` がいずれも空

### G2-1 [major] `QueueHandler` 構造体が 40 フィールド・148 メソッド・20 ファイルに散在 (God Object 兆候)

- **対象**: `internal/daemon/queue_handler.go:49` (`type QueueHandler struct`)
- **メトリクス**:
  - 構造体フィールド: **40 個** (config, dl, logger, clock, execProvider, queueStore, leaseManager, dispatcher, dependencyResolver, cancelHandler, resultHandler, reconciler, deadLetterProcessor, metricsHandler, circuitBreaker, admissionCtrl, fallbackMgr, worktreeManager, deferredPlanCompleter, lockMap, scanExecutor, scanRunMu, daemonPID, initMu, shutdownCtx, shuttingDown, sessionLost, undecidedTracker, paneActivity, paneCapture, paneFinder, timeCache, phaseC, consecutiveCascadeBreakScans, phaseMergeDeferStart, awaitingFillStallLastFire, awaitingFillStallFireCount, …)
  - メソッド数: `^func (qh *QueueHandler)` で **148 件** (非テスト)
  - 散在ファイル数: **20 ファイル** (`queue_scan_apply.go` / `queue_handler.go` / `queue_scan_phase_a_*` / `scan_orchestrator.go` / 他)
- **抽象漏れ**: `QueueHandler` が「scan オーケストレータ」「dispatcher proxy」「metrics ハンドラ」「pane activity 観測ファサード」「worktree manager 経由地」を兼任。20 ファイルに 148 メソッドが散らばっており、struct 単体としての凝集度が低い
- **重大度**: major — 影響範囲が読み取れない / テストの worker mock 注入が困難
- **リファクタ方向**:
  - `QueueScanController` (scan tick オーケストレーション) と `QueueDispatchService` (dispatch path) と `LeaseLifecycleService` (lease expiry / extension) に責務分割
  - パッケージは `internal/daemon/queue/{scan,dispatch,lease}` で再編
- **分類**: 設計議論が必要 (公開 API への影響大)
- **根拠**: 上記 grep 結果 + struct 定義行範囲 49-300 内のフィールド数集計

### Daemon 構造体の参考メトリクス (補助情報)

- `internal/daemon/daemon.go:52` `type Daemon struct` フィールド数: **46 個**
- これは composition root として正当だが、QueueHandler との責務重複が起きていないかは別途設計レビュー対象

---

## 4. G3 — 命名乖離

### G3-1 [minor] `validateRunOnMainContent` は no-op だが「destructive content rejected」と読める命名 / コメント

- **対象**: `internal/daemon/dispatch/validate_run_on_main.go:14-25`
- **観察**: `var ErrDestructiveContentRejected = errors.New("dispatch: task content rejected by run_on_main pre-flight")` と error 定義しつつ、関数本体は `_ = task; return nil` (常に nil)。呼出側 `dispatcher.go:271` のコメント "Defense-in-depth pre-flight check that rejects destructive shell snippets" は実装と矛盾
- **重大度**: minor (機能的影響なし。命名と挙動の不一致のみ)
- **リファクタ方向**:
  - 関数名を `validateRunOnMainContent_NoOp` または `_ = validateRunOnMainContent` の dead-call 削除に
  - 呼出側コメントを実態 ("retained for ABI compatibility") に更新
- **分類**: すぐ着手可能

### G3-2 [minor] `Tracker.ObserveVerdict` は副作用 (snapshot 記録 / streak 更新 / log emit) を含むが「Observe」だけでは副作用が不可視

- **対象**: `internal/daemon/paneactivity/tracker.go:707`
- **観察**: `ObserveVerdict` 内で `t.snapshots[agentID] = ...`、`t.uncertainStreak[agentID] = streak + 1`、`slog.Warn("pane_terminal_error_detected", ...)` 等の副作用が発生する。読み手は名前から「観測値の getter」と推定しがち
- **重大度**: minor
- **リファクタ方向**: `RecordAndEvaluateVerdict` 等、副作用が含意される動詞に改名
- **分類**: すぐ着手可能 (リネームのみ)

### G3-3 [nit] `daemon.Manager` / `lease.Manager` / `worktree.Manager` / `fallback.Manager` のような generic な型名

- **対象**:
  - `internal/daemon/lease/manager.go` (Receiver `lm *Manager`)
  - `internal/daemon/worktree/manager.go` (Receiver `wm *Manager`)
  - `internal/daemon/fallback/manager.go` (Receiver `m *Manager`)
- **観察**: 各パッケージで型名 `Manager` が使われているため、daemon パッケージ側で同時に import すると `lease.Manager`, `worktree.Manager`, `fallback.Manager` と都度 prefix が必要。Receiver 名 `lm`/`wm`/`m` で差別化はされているがパッケージを跨いだ可読性は低下
- **重大度**: nit (Go 慣習上はパッケージ修飾で十分とも言える)
- **リファクタ方向**: `LeaseManager` / `WorktreeManager` / `FallbackManager` のように型名にロール名を含める (パッケージ名と被るが import 名前空間衝突は減る)
- **分類**: 設計議論が必要 (パッケージ命名規則の方針判断)

---

## 5. G4 — magic number / magic string の点在

### G4-1 [major] `.maestro/` 直下のサブディレクトリ名がリテラルで散在 (192 箇所)

- **対象**:
  - `"state"` / `"queue"` / `"results"` / `"locks"` / `"logs"` / `"hooks"` / `"worktrees"` / `"skills"` / `"persona"` / `"instructions"` / `"cache"` / `"bin"` の文字列リテラルを `filepath.Join(maestroDir, ...)` の引数に直接埋め込む箇所
- **メトリクス**: `grep -rEn '"(state|queue|results|locks|logs|hooks|worktrees|skills|persona|instructions|cache|bin)"' internal/` で **192 ヒット** (非テスト)
- **観察**: 一例として `internal/plan/submit.go:102` で `filepath.Join(opts.MaestroDir, "state", "verify", opts.CommandID+".yaml")`、`internal/plan/submit_queue.go:200` で `filepath.Join(maestroDir, "queue", workerIDToQueueFile(workerID))` 等。L2 hook 側 (`worker_policy_hook.sh` / `launcher.go workerDisallowedTools`) と整合させる必要があるが、Go 側にディレクトリ名の SSOT が無い
- **重大度**: major — 「`.maestro/state` のリネーム」のような変更が grep 漏れを起こし、L1/L2 ガードと食い違うリスク
- **リファクタ方向**:
  - `internal/pathutil` (既存) または新パッケージに `MaestroSubdir = struct { State, Queue, Results, Locks, Logs, Hooks, Worktrees, Skills, Persona, Instructions, Cache, Bin string }` を定義し定数化
  - 各呼出側を constants 経由に
- **分類**: すぐ着手可能 (機械的置換)

### G4-2 [major] `bufferSec` 計算における magic number `30` / `90`

- **対象**: `internal/daemon/queue_scan_collect.go:746-749`
  ```go
  bufferSec := qh.config.Watcher.ScanIntervalSec + 30
  if bufferSec <= 30 {
      bufferSec = 90
  }
  ```
- **観察**: 「`scan_interval + 30s` を buffer に、それでも 30 秒以下なら 90 秒に固定」という意味論が無名定数で実装されている。`30` は 2 度別の意味で使われる (加算量 / 下限) のため特に紛らわしい
- **重大度**: major (ロジックバグの温床)
- **リファクタ方向**:
  - `const (preemptiveRenewalBufferSec = 30; preemptiveRenewalMinBufferSec = 90)` を導入し、関係性をコメントで明示
- **分類**: すぐ着手可能

### G4-3 [minor] `daemon.NumCPU` cap の magic 値 `8` / `32`

- **対象**: `internal/daemon/daemon.go:40-47`
  ```go
  n := runtime.NumCPU() * 2
  if n < 8 { return 8 }
  if n > 32 { return 32 }
  ```
- **観察**: workerpool 規模の下限 / 上限が無名 magic number
- **重大度**: minor
- **リファクタ方向**: `const (minScanWorkers = 8; maxScanWorkers = 32)` 化 + コメントに根拠 (32 を超えると lock 競合が支配的、8 未満だと…) を残す
- **分類**: すぐ着手可能

### G4-4 [minor] `phaseMergeDeferEscalation` / `awaitingFillStallActiveBackstop` 等の duration 定数は揃っているが、設定可能性は混在

- **対象**: `internal/daemon/queue_scan_phase_a_phase.go:187` / `:569`
  ```go
  const phaseMergeDeferEscalation = 10 * time.Minute
  const awaitingFillStallActiveBackstop = 25 * time.Minute
  ```
  と、`config.Maestro.EffectiveAwaitingFillStallNotifyMinutes()` 等の config 駆動値が混在
- **観察**: 一部の閾値が config に外出し、一部が hard-coded のままで、運用者が「どれが調整可能か」を読み取りにくい。少なくとも各 const の横に「config 化しない理由」のコメントが欲しい
- **重大度**: minor
- **リファクタ方向**: `config.Maestro` への移行可否を整理し、ハードコード組はその不変性 (アルゴリズム invariant) を godoc に明記
- **分類**: 設計議論が必要 (config schema 拡張可否)

### G4-5 [minor] `maxLen = 200` (sanitize) / `maxLen = 100` (launcher) / `maxLen = 512` / `256` (paneactivity log trim) のサニタイズ閾値が散在

- **対象**:
  - `internal/daemon/result_write_handler.go:21` `const maxLen = 200`
  - `internal/agent/launcher.go:1332` `const maxLen = 100`
  - `internal/daemon/paneactivity/tracker.go:740` `trimForLog(tailLines(content), 512)`
  - 同 `:768` `trimForLog(tailForBudget, 256)`
- **観察**: ログサニタイズのためのトリム長が場所ごとに異なる定数で実装されており、ログサイズ管理ポリシーが見通しにくい
- **重大度**: minor
- **リファクタ方向**: `internal/daemon/core` または `internal/observability` に `LogTrim*` 定数群を集約
- **分類**: すぐ着手可能

---

## 6. G5 — 早期 return 連鎖が本流を圧迫

### G5-1 [major] `(*Manager).EnsureWorkerWorktree` は 175 行に 21 件の error check で本流が見えない

- **対象**: `internal/daemon/worktree/manager.go:113`
- **メトリクス**: `if err != nil` 21 件 + 3 種の rollback closure
- **観察**: 「正常系を 1 行で読みたい」と思っても rollback ロジックがインラインで挿入され続け、本流の `state ← create / save` がエラーケースに埋もれる
- **重大度**: major
- **リファクタ方向**: G1-6 と同じ。rollback を stack-style helper に集約することで本流が独立して読めるようになる
- **分類**: 設計議論が必要

### G5-2 [major] `(*QueueHandler).collectPendingTaskDispatches` は 7 段の連続 gate (`continue` 連鎖) で読みにくい

- **対象**: `internal/daemon/queue_scan_collect.go:96-286`
- **観察**: dependency check / pre-merge gate / system-commit gate / markTaskReady / path-overlap / admission / lease acquire が `continue` で順に弾かれる構造。各 gate のエラー型と log 粒度が独自で、責任分離が見えない
- **重大度**: major
- **リファクタ方向**: 各 gate を `func (qh) shouldDispatchTask(...) (bool, string, error)` の signature で切り出して return 型を統一し、parent loop は `for _, gate := range gates { if !gate(task) { continue } }` の宣言的列挙に
- **分類**: すぐ着手可能

### G5-3 [minor] `Complete` の冒頭 100 行が「lock 取得 + intent recovery + idempotency + auto-cancel」の 4 連 early-return

- **対象**: `internal/plan/complete.go:193-285`
- **観察**: `if opts.LockMap == nil { return }` / `if !validate.IsValidBaseName ... { return }` / intent 経路 / idempotency 経路 / auto-cancel 経路と、本流に到達するまで 4 種の早期 return + 1 種の早期 success return が連続。`executeCompleteSteps` まで辿り着くのに ~80 行
- **重大度**: minor
- **リファクタ方向**: G1-2 と同じ。pre-execution hook chain 化することで早期 return が hook の戻り値に移譲される
- **分類**: すぐ着手可能

---

## 7. G2 補足 — 抽象漏れの個別兆候 (循環ではないが構造リスク)

参考までに、循環依存はないが G2-1 (QueueHandler 肥大) 以外の抽象漏れ候補を 2 点列挙する。

- **`internal/daemon/dispatch` が `internal/agent` を import** (`internal/daemon/dispatch/dispatcher.go:12`)。daemon→agent の正当な依存だが、dispatch package が agent の `ExecRequest` を直接組み立てる (dispatcher.go:323) 形は agent の内部表現が daemon に染み出す形になっている。`internal/envelope` のような中立パッケージ経由が望ましい (推定: 既に `internal/envelope.BuildWorkerEnvelope` があるが ExecRequest は別経路)
- **`paneactivity.Tracker` が `slog.Warn` で観測情報を直接吐く** (`tracker.go:742, 770, 833, 868`)。daemon の DaemonLogger を渡さず slog グローバルを使うため、ログレベル統一が package boundary をまたぐ。observability の抽象漏れ

これらは現時点で実害は少ないが、QueueHandler 分割を行う場合の前提条件として再設計検討対象。

---

## 8. すぐ着手可能 / 設計議論が必要 の分離

### 8.1 すぐ着手可能 (機械的または局所変更で完結)

| ID | 概要 | アプローチ |
|----|------|-----------|
| G1-2 | `Complete` (246 行) を pre-hook chain + executeCompleteSteps に再構成 | hook を順次呼ぶ pattern (intent / idempotency / paused-replan auto-cancel / can-complete) |
| G1-4 | `collectExpiredTaskBusyChecks` のループ内 verdict 分岐をメソッド化 | inner-loop body を `handleExpiredTaskWithVerdict` に切り出し |
| G1-5 | `collectPendingTaskDispatches` 引数を struct に集約 + gate チェーン化 | `dispatchContext` struct と `[]gateFn` の宣言的列挙 |
| G1-7 | `stepAwaitingFillWatchdog` の二重ループ内側を関数化 | `evaluatePhaseAwaitingFill` / `firePhaseStallSignal` の分離 |
| G1-8 | `collectWorktreePhaseMerges` を phase 単位の判定 + merge item builder に分割 | `shouldMergePhase` / `buildMergeItems` |
| G3-1 | `validateRunOnMainContent` の dead-call 削除 / コメント更新 | function 削除 or 命名変更 |
| G3-2 | `Tracker.ObserveVerdict` のリネーム | `RecordAndEvaluateVerdict` 等 |
| G4-1 | `.maestro/` サブディレクトリ名の constants 化 | `pathutil` package に集約 |
| G4-2 | `bufferSec` の magic 30/90 を const 化 | `preemptiveRenewalBufferSec` / `preemptiveRenewalMinBufferSec` |
| G4-3 | `daemon.NumCPU` cap の magic 8/32 を const 化 | `minScanWorkers` / `maxScanWorkers` |
| G4-5 | log trim 長の定数を集約 | observability パッケージに集約 |
| G5-2 | `collectPendingTaskDispatches` の gate チェーンを宣言化 | G1-5 と同じ |
| G5-3 | `Complete` の冒頭 4 連 early-return を hook 化 | G1-2 と同じ |

### 8.2 設計議論が必要 (公開 API / アルゴリズム invariant に踏み込む)

| ID | 概要 | 議論ポイント |
|----|------|------------|
| G1-1 | `collectWorktreePublishAndCleanup` (355 行) の 5 ステップ + integration status switch 分割 | state machine の論理的境界をどう切るか |
| G1-3 | `ObserveVerdict` の 4 fast path を rule list 化 | precedence の implicit→explicit 化に伴うテスト影響 |
| G1-6 | `EnsureWorkerWorktree` の rollback stack 化 | atomicity 保証を保てるか |
| G2-1 | `QueueHandler` の責務分割 (40 フィールド / 148 メソッド / 20 ファイル) | 公開 API 範囲、テストの mock 戦略 |
| G3-3 | `lease.Manager` / `worktree.Manager` 等の generic 型名 | パッケージ命名規則の方針 |
| G4-4 | hard-coded duration vs config 駆動の方針整理 | config schema 拡張可否 |
| G5-1 | `EnsureWorkerWorktree` の rollback 集約 | G1-6 と同じ |

---

## 9. グラウンディング上の注意

- 関数行数は brace tracking で算出した `ACTUAL_LINES` を使用している (前段で `^func ` 距離 heuristic が package level 定数で過大計上したため)。
- 引数 6 個以上の function 列挙は signature 内 `,` で count しただけの近似値。`map[string]interface{}` のような型に内包される `,` は含まれないため一部過小計上の可能性あり。
- 「QueueHandler 148 メソッド」は `^func (qh *QueueHandler)` の単純 grep 結果。ジェネリックメソッド・受信値違いで漏れる可能性は無い (Go ではメソッド表現がこの 1 形式に固定)。
- 確認できなかった事項 (= 推定):
  - 「`bufferSec = 90` の `90` がどの運用上の値に由来するか」は git blame を確認していないので推定 (6 scan × 15 sec の 90 sec, 等)。
  - `paneactivity.Tracker` が DaemonLogger ではなく `slog` グローバルを使う設計上の意図は不明 (推定: テスト容易性のため)。

---

## 10. 報告サマリ用カウント

- critical: 1 (G1-1)
- major: 12 (G1-2, G1-3, G1-4, G1-5, G1-6, G2-1, G4-1, G4-2, G5-1, G5-2)
  ※ 重大度ラベルは finding 本文の `[severity]` 表記が SSOT。集計は手動。
- minor: 7 (G1-7, G1-8, G3-1, G3-2, G4-3, G4-4, G4-5, G5-3)
- nit: 1 (G3-3)
- 合計: 20 件 (G1=8, G2=1+補助1, G3=3, G4=5, G5=3)

### 観点別件数

| 観点 | 件数 |
|------|------|
| G1 | 8 (G1-1 ~ G1-8) |
| G2 | 1 (G2-1) + 「循環なし」判定 |
| G3 | 3 (G3-1 ~ G3-3) |
| G4 | 5 (G4-1 ~ G4-5) |
| G5 | 3 (G5-1 ~ G5-3) |
| 合計 | 20 |
