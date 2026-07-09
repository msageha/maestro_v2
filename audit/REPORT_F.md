# 観点 F: バグ・並行性・エッジケース監査レポート

- 監査範囲: maestro_v2 リポジトリ全体 (`internal/**`)
- 監査手段: Read / Grep のみ (コード変更・コマンド実行は禁止)
- 監査者: worker2 (researcher persona)
- 報告日: 2026-05-08
- task_id: task_1778225393_ab195d08d804bbab

## サマリ

| 観点                                                   | 判定                                     | 主要な根拠                                                                                                                                                                                                                                                                                                                                          |
| ------------------------------------------------------ | ---------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| F1: race condition                                     | **問題なし (低リスクの観測点 1 件あり)** | mutex 保護は包括的。`task_heartbeat_handler` は scanMu.RLock を取得して TOCTOU を閉じる。`paneactivity.Tracker` の `hintLogState` だけが ForgetAgent で消されないが、agentID 数が config で固定されているため実質的な leak ではない                                                                                                                 |
| F2: context 漏れ                                       | **問題あり (中)**                        | UDS の `Server.processRequest` / handler 群が context を受け取らないため、daemon shutdown 中の長時間 handler は totalDuration まで完走待ち。`SessionRecoverer.RecoverSession` も `time.Sleep` ベースで context cancel を観測しない (バウンド ≤7s)                                                                                                   |
| F3: error wrap / log 漏れ                              | **問題なし**                             | `fmt.Errorf` 使用 448 件のうち 384 件が `%w` 採用 (約 86%)。残りは sentinel error 定義 / boundary check / プロセス境界など `%w` 不要の文脈。daemon ループ側の握り潰しは確認できず                                                                                                                                                                   |
| F4: lease_epoch / fencing                              | **問題あり (低)**                        | `task_heartbeat_handler` は `Epoch < 0` を validation で弾くが、`daemonapi.ResultWriteParams` 側のバリデータ (`ValidateResultWriteRequest`) は `LeaseEpoch` の正値性を検証しない。実害は fencing で必ずミスマッチして拒否されるため低だが、エラーメッセージが `queue=N, request=-1` で操作者に分かりにくい                                          |
| F5: recovery loop 脱出条件                             | **問題あり (低)**                        | `escalateAwaitingFillStall` で `ApplyPhaseTransition` が失敗した場合に FireCount を delete してしまうため、次回の escalate に再度 threshold × 3 ≒ 15min かかる。バウンドはあるがリカバリ遅延の余地。publish_quarantined / blocked_pane / mergeFailureQuarantine は脱出条件確認済み                                                                  |
| F6: retry lineage active 2 本                          | **問題なし**                             | Planner add-retry-task は `lockWorkerQueues(ALL workers)` + `state:<commandID>` を保持し queue X を Cancelled にしてから release。daemon auto-retry の validateFencing は `queue:<reporter>` lock を経由して queue X の status を読むため、Plan が cancel した後の入場では FENCING_REJECT_STATUS で拒否される。invariant は lock 順序で守られている |
| F7: tmux socket 分離 / worktree cleanup / git fallback | **問題なし**                             | `BuildMaestroSocketName` は `sha256(canonicalDir)[:8]` で per-checkout 分離。`gitAddAllWithUnstattableFallback` は `gitAddAllAttemptLimit=5` で bounded、`gitWorktreeAddWithUnstattableFallback` は `--no-checkout` + skip-worktree で bounded                                                                                                      |

### 件数 (重大度別)

- **critical**: 0
- **major**: 1 (F2-1)
- **minor**: 4 (F1-1, F4-1, F5-1, F3-1 の補足観測 = closeWg 未 Wait)
- **nit**: 2 (F1-2, F2-2)

注: 単一 finding が観点をまたぐ場合があるため、件数合計と finding 数は一致しない。

### 観点別件数

- F1: 2 件 / F2: 2 件 / F3: 1 件 / F4: 1 件 / F5: 1 件 / F6: 0 件 / F7: 0 件

---

## F1: Race Condition

判定: **問題なし (観測点 1 件)**

### 設計レベルの確認

- 共有 state はすべて `sync.Mutex` / `sync.RWMutex` / `sync.Map` / `atomic.*` で保護されている (`grep -rn "sync\.Mutex\|sync\.RWMutex" --include="*.go" internal/ | grep -v _test.go` 約 60 件)。
- `events.Bus` は `closed atomic.Bool` + `mu sync.RWMutex` の二段ガード + `defer recover()` で `send-on-closed-channel` panic を防御 (`internal/events/bus.go:103-116, 261-269`)。
- `messageDeliverer` の `paneMu sync.Map` + per-pane `*sync.Mutex` で TOCTOU(`ensureClaudeRunning` → `SendTextAndSubmit`) の interleave を遮断 (`internal/agent/message_deliverer.go:34, 101-110, 126-130`)。
- `task_heartbeat_handler` は `scanMu.RLock` を **blocking** で取得し、Phase A/C による queue 上書きとの TOCTOU を閉じている (`internal/daemon/task_heartbeat_handler.go:118-128`、コメント "M-2 fix" 参照)。
- `lease.Manager` は単独 mutex を持たず、callers (queue handler / heartbeat handler) が scanMu を取って serialise する設計が一貫している (`internal/daemon/lease/manager.go:22-29`、Doc コメント "all lease operations use a single Clock instance scoped to the daemon process")。
- `qh.timeCache` は scan ごとに `Reset()` され (`internal/daemon/scan_phase_host.go:8-10`)、scan は `scanRunMu` で serialise されているため scan 間 race は発生しない (`internal/daemon/scan_phase_executor.go:75-76`)。

### F1-1 (minor — 観測のみ)

- file: `internal/events/bus.go:108-116, 386-407`
- 関数: `Bus.Close`
- 種別: 約束されているが達成されない sync invariant
- 根拠引用 (bus.go:387-389):
  > ```go
  > // Wait for subscriber goroutines to finish with timeout.
  > // A single goroutine bridges sync.WaitGroup.Wait (which is not cancellable)
  > // to a channel select. It is tracked by closeWg so that callers can verify
  > // cleanup is complete even after Close returns on timeout.
  > ```
- 検証: `grep -rn "closeWg\|.closeWg.Wait" --include="*.go" internal/` の結果は `bus.go` 内部の 4 件のみ。`closeWg.Wait()` を呼ぶ caller は production / test ともに **0 件**。
- Given-When-Then:
  - Given: `bus.Close()` が 5 秒タイムアウトで返却し、subscriber goroutine が drain 中
  - When: caller が「全 subscriber goroutine が完了したことを待ちたい」と思った場合
  - Then: 公開 API 上にそれを行う手段がない (`bus.closeWg` は unexported)。コメントに反して caller verification は不可能
- 重大度: minor (機能上の害はない。コメントが contract を約束しているだけで、現状利用者は誰も期待していない)
- 再現条件: 機能テストでなく、コードレビューで検出される問題
- 対応 (設計議論レベル): `bus.WaitDone()` のような public method を生やすか、コメントから "callers can verify" の節を削除する。Close 既存仕様 (timeout で error 返却) を保つなら後者で十分

### F1-2 (nit)

- file: `internal/daemon/paneactivity/tracker.go:948-954`
- 関数: `Tracker.ForgetAgent`
- 種別: 部分的なクリア (`hintLogState` が消えない)
- 根拠引用:
  > ```go
  > func (t *Tracker) ForgetAgent(agentID string) {
  >     t.mu.Lock()
  >     defer t.mu.Unlock()
  >     delete(t.snapshots, agentID)
  >     delete(t.uncertainStreak, agentID)
  >     delete(t.blockedSince, agentID)
  > }
  > ```
- Given-When-Then:
  - Given: 大量の worker_id が動的に生成される deployment (現状の maestro は固定 worker 数なので非該当)
  - When: 各 worker pane が wedged → ForgetAgent → respawn を繰り返す
  - Then: `hintLogState` map のエントリが ForgetAgent で消えず、process 寿命にわたって蓄積
- 重大度: nit (現行設計では config の `agents.workers` の数で bounded、実害は無視可能)
- 再現条件: maestro が将来 dynamic worker provisioning に対応した時点で leak になる可能性
- 対応: `delete(t.hintLogState, agentID)` を加える

---

## F2: Context 漏れ / cancellation 未伝播

判定: **問題あり (中)**

### F2-1 (major)

- file: `internal/uds/server.go:250-330` (handleConn / processRequest)
- 関数: `Server.processRequest` および登録される全 handler (`uds.Handler = func(*Request) *Response`)
- 種別: handler 関数シグネチャに context が無く、daemon shutdown が in-flight handler を中断できない
- 根拠引用 (server.go:319-330):
  > ```go
  > s.mu.RLock()
  > handler, ok := s.handlers[req.Command]
  > s.mu.RUnlock()
  >
  > if !ok {
  >     return ErrorResponse(...)
  > }
  >
  > return handler(req)
  > ```
- 補足: 各 handler 内では context を内製しているケースもある (例: `plan/retry.go:saveStateWithContext`)。だが `Server.ctx` (daemon shutdown) は handler に伝わらないため、長時間 I/O (state save / yaml AtomicWrite / git ops 等) に対して **daemon 側からの cancellation が効かない**
- Given-When-Then:
  - Given: daemon が `Shutdown()` を呼び出し、`d.cancel()` で context をキャンセル中
  - When: handler が`saveStateWithContext` の `<-ctx.Done()` 経路ではなく、純粋な disk I/O (例: `os.ReadFile` を呼ぶ各種 handler) を実行中
  - Then: handler は disk I/O が完了するまで block する。errgroup.Wait は `totalDuration` (default `ShutdownTimeoutSec`) 待機、その後 `runtime.Stack(buf, true)` を吐いて grace period 後 `os.Exit(1)` で強制終了 (`internal/daemon/daemon_shutdown.go:144-162`)
- 重大度: major (機能上の害は最終的に doExit で限定されるが、コードレベルで daemon が長時間止まる可能性がある。一方で同期 I/O 中心の handler は通常 1 秒以内に完了するので production 影響はおそらく低)
- 再現条件: shutdown 中に slow disk / NFS / sandbox-restricted FS に対して `result_write` 等が走る
- 対応 (設計議論レベル): `uds.Handler` を `func(ctx context.Context, *Request) *Response` に変更し、`processRequest` で `context.WithCancel(s.ctx)` を派生させる。重い I/O 系 handler (state save, yaml.AtomicWrite, git ops) で context.Done を観測

### F2-2 (nit)

- file: `internal/formation/watch_loop.go:69, 94-110`
- 関数: `SessionRecoverer.RecoverSession`
- 種別: `time.Sleep` ベース backoff が context を観測しない
- 根拠引用 (watch_loop.go:99-101):
  > ```go
  > backoff := time.Duration(1<<(attempt-2)) * time.Second // 1s, 2s, 4s
  > // ...
  > r.sleepFn(backoff)
  > ```
- Given-When-Then:
  - Given: tmux session 再作成中 (1+2+4 = 7 秒の総バックオフ)
  - When: daemon shutdown 要求が同時に来る
  - Then: `RecoverSession` が最大 7 秒間 block。backoff 中の cancellation 通知は効かない
- 重大度: nit (上限 7 秒で bounded。production 影響軽微)
- 対応: `time.Sleep` を `select { case <-ctx.Done(): return; case <-time.After(backoff): }` に置換

---

## F3: Error Wrap / Log 漏れ

判定: **問題なし**

### 検証

- `internal/daemon/` 配下の `fmt.Errorf` 使用 448 件 (`grep -rn 'fmt\.Errorf' --include="*.go" internal/daemon/ | grep -v _test.go | wc -l`)
- うち `%w` (error wrap) 採用 384 件 (86%)。
- `%w` 不採用 64 件は以下の正当な文脈に分類できる:
  - sentinel error 定義 (`var ErrPhaseMaxTasksExceeded = fmt.Errorf("...")`)
  - boundary check (`fmt.Errorf("task is nil")`, `fmt.Errorf("no state reader")`)
  - 利用者に渡す string-based error (`fmt.Errorf("dispatch blocked: command %s cancel-requested", item.Task.CommandID)` — 元 err なし)
  - validation error (`fmt.Errorf("explorationCoeff must not be negative")`)

### Daemon ループ側の握り潰し検証

主要 daemon goroutine (`internal/daemon/event_bridge.go`, `internal/daemon/quality_gate.go`, `internal/uds/server.go:acceptLoop`) で:

- panic は `defer recover()` + `slog.Error` + `Shutdown()` 形式 (`event_bridge.go:33-43, 144-149`)
- error は logger 経由で WARN/ERROR に出力後、return / continue
- `subscribeWithRecovery` パターンが事故防止層として一貫適用されている

握り潰し (silent error swallow) と認識できる箇所はなかった。

### F3-1 補足観測 (informational)

- 上記 F1-1 で挙げた `closeWg` の問題は技術的には「contract と実装の不一致」だが、観点 F1 の race condition ではなく契約違反として扱う方が適切。F3 (error / contract 漏れ) として再カウントしない

---

## F4: lease_epoch / Fencing 境界条件

判定: **問題あり (低)**

### F4-1 (minor)

- file: `internal/daemon/daemonapi/result_write.go:14-93`
- 関数: `ResultWriteParams` / `ValidateResultWriteRequest`
- 種別: `LeaseEpoch` の正値性検証が不在 (heartbeat handler は持つ)
- 根拠引用1 (result_write.go:14-27):
  > ```go
  > type ResultWriteParams struct {
  >     ...
  >     LeaseEpoch             int      `json:"lease_epoch"`
  >     ...
  > }
  > ```
- 根拠引用2 (result_write.go:59-93): `ValidateResultWriteRequest` は Reporter / TaskID / CommandID / Status のみ検証、`LeaseEpoch >= 0` チェックは無い
- 比較対象 (`task_heartbeat_handler.go:97-99`):
  > ```go
  > if p.Epoch < 0 {
  >     return uds.ErrorResponse(uds.ErrCodeValidation, "epoch must be non-negative")
  > }
  > ```
- Given-When-Then:
  - Given: Worker 側で `--lease-epoch` 未指定の場合、CLI default は `-1` (sentinel)
  - When: Worker が誤って `-1` を渡したまま `result write` を投げる (CLI 側で本来弾かれるが、API 直叩きの場合は通る)
  - Then: `validateFencing` が `queueTask.LeaseEpoch != params.LeaseEpoch` で fail し `FENCING_REJECT_EPOCH` (queue=N, request=-1) を返す。エラーメッセージが「epoch ミスマッチ」を語るため操作者には sentinel 値だと分かりにくい
- 重大度: minor (実害はなく fencing は守られる。ただ heartbeat 側との対称性が崩れている)
- 再現条件: `-1` を含む不正値の API 直接呼び出し
- 対応: `ValidateResultWriteRequest` に `if params.LeaseEpoch < 0 { return ... "lease_epoch must be non-negative" }` を追加。heartbeat 側と対称化

### Fencing ロジックの境界確認 (問題なしの根拠)

- `validateFencing` (`internal/daemon/result_write_phase_a.go:218-306`) は queue task の status / epoch / lease_owner の三軸を検査し、それぞれに `FencingDetails` 構造体を埋めて応答
- `task_heartbeat_handler.Handle` (`internal/daemon/task_heartbeat_handler.go:165-230`) は status → epoch → max_runtime の順で検査
- `LeaseManager.acquireLease` は `(*ref.leaseEpoch)++` で 1 ずつインクリメント (`internal/daemon/lease/manager.go:138`)。`releaseLease` / `extendLeaseExpiry` で epoch は不変 (mem ルール準拠)
- `IsLeaseExpired` (parse error → 期限切れ扱い、line 285-294) と `IsTaskMaxInProgressTimeout` (parse error → タイムアウトしてない扱い、line 379-395) はパースエラー時の挙動が **逆向き**。意図的な「fail-safe vs fail-open」の使い分けに見えるが、両関数の docstring が `IsLeaseExpired` 側で説明不足 (parse error が leak しないという point は明示されているが、その理由は不明)。バグではなく、可読性の懸念
- 結論: epoch 比較ロジック自体は `leaseInvalidReason` (`internal/daemon/queue_scan_helpers.go:22-30`) を SSOT として共有しており、heartbeat / result_write / queue scan apply で同じ判定が動く

---

## F5: Recovery Loop 脱出条件

判定: **問題あり (低)**

### F5-1 (minor)

- file: `internal/daemon/queue_scan_phase_a_phase.go:609-669`
- 関数: `QueueHandler.escalateAwaitingFillStall`
- 種別: phase 遷移失敗時に FireCount を delete するため、復旧が遅延
- 根拠引用 (queue_scan_phase_a_phase.go:622-668):
  > ```go
  > if err := stateMgr.ApplyPhaseTransition(cmd.ID, phase.ID, model.PhaseStatusFailed); err != nil {
  >     qh.log(LogLevelWarn,
  >         "awaiting_fill_watchdog_escalate_phase_transition_failed command=%s phase=%s error=%v "+
  >             "(continuing — Orchestrator will still see the synthetic notification)",
  >         cmd.ID, phase.Name, err)
  > }
  > ...
  > // Reset the fire counter so a future re-entry starts fresh.
  > qh.awaitingFillStallFireCount.Delete(cmd.ID + "::" + phase.ID)
  > ```
- Given-When-Then:
  - Given: phase が `awaiting_fill` で 3 回連続 FireCount に達し、escalate に入った
  - When: `ApplyPhaseTransition` が disk I/O 失敗等で error を返す
  - Then: phase は依然 `awaiting_fill` のまま、FireCount は 0 にリセット。次回 escalate には threshold × 3 ≒ 15 分が改めて必要
- 重大度: minor (bounded recovery、永久 stall ではない)
- 再現条件: state file write がエラー (sandbox 制限 / 一時的な ENOSPC / I/O error 等)
- 対応 (設計議論レベル): `if err == nil { qh.awaitingFillStallFireCount.Delete(fireKey) }` のように成功時のみ delete に変更。失敗時は次回 scan で即 escalate するほうが望ましい

### Recovery loop の脱出条件確認 (問題なしの根拠)

#### blocked-pane timeout

- `stepBlockedPaneTimeout` (`internal/daemon/queue_scan_blocked_pane.go:33-127`):
  - threshold (`blockedPaneFailAfter()`) を超えたら `failTaskBlockedPane` で task を Failed に。`recoverWorkerPaneAfterBlocked` で worker pane を respawn。`paneActivity.ForgetAgent` で snapshot をクリア
  - threshold が 0 (disabled) なら skip。VerdictTerminalError なら threshold 待たず即 fail
  - **bounded**: 単一 scan tick で完結し、再訪時には status が Failed なので `if !hasInProgress { continue }` で抜ける

#### awaiting_fill_stall

- `stepAwaitingFillWatchdog` (`internal/daemon/queue_scan_phase_a_phase.go:357-528`):
  - FireCount を `bumpAwaitingFillStallFireCount` で上限ナシでインクリメント、しかし 3 連続で `escalateAwaitingFillStall` が phase=failed に遷移 → 主ループ line 390 で skip
  - VerdictActive (Planner pane が spinner 動作中) には wall-clock backstop `awaitingFillStallActiveBackstop = 25min` が確実に escalate を強制 (line 446-457)
  - **bounded**: F5-1 の小さな遅延を除けば 15min または 25min で必ず escalate

#### publish_quarantined

- `recordPublishFailure` (`internal/daemon/worktree/merge_publish.go:74-109`):
  - `publishFailureQuarantineThreshold = 5` 回到達で `IntegrationStatusQuarantined` に終端化、`NextPublishRetryAt = ""` で backoff も停止
  - exponential backoff (`backoff *= multiplier; if > publishRetryMaxBackoff { break }`) も `publishRetryMaxBackoff = 5min` で cap
  - `R8PublishFailed.Apply` (`internal/daemon/reconcile/r8_publish_failed.go:31-99`) で `StallSignaled = true` ガードにより通知は once 限り
  - `stepFinalizeQuarantinedDeferredComplete` (`internal/daemon/queue_scan_phase_a_worktree.go:295-337`) でデファード complete intent は強制終端
  - **bounded**: 5 回失敗で必ず終端、operator manual unquarantine が必要だが loop しない

#### merge failure

- `recordMergeFailure` (`internal/daemon/worktree/merge_publish.go:54-68`):
  - `mergeFailureQuarantineThreshold = 3` で同様に終端化
  - `MergeToIntegration` の早期 return guard (line 166-168) が H10 ループを断つ

#### git add fallback

- `gitAddAllWithUnstattableFallback` (`internal/daemon/worktree/git_operations.go:339-394`):
  - `gitAddAllAttemptLimit = 5` で必ず終了 (line 392-393)
  - 各 attempt で新規 path を 1 件以上 exclude に追加できなければ抜ける (`if len(fresh) == 0 { return err }`, line 375-379)
  - **bounded**

---

## F6: Retry Lineage Active 2 本レース

判定: **問題なし**

MEMORY.md の `retry chain は predecessor あたり active 1 本まで` (`retry_one_active_per_predecessor.md`) と `retry enqueue 後は original task を即 cancelled-superseded` (`retry_supersedes_directly_no_repair_pending.md`) の両 invariant が現コードでも維持されている。

### 設計確認

#### Planner add-retry-task の検証層 (`internal/plan/retry.go:217-271, 456-603`)

- `validateRetryRequest` line 542-561: `state.RetryLineage` を walk し、`predecessor == opts.RetryOf` かつ非 terminal な descendant がいれば即拒否
  > ```go
  > return nil, &planValidationError{Msg: fmt.Sprintf(
  >     "retry-of task %s already has an active retry %s (status=%s); only one outstanding retry per predecessor is allowed",
  >     opts.RetryOf, descendantID, descendantStatus)}
  > ```
- `lockWorkerQueues(opts.LockMap, allConfiguredWorkerIDs(opts.Config.Agents.Workers))` (line 233): **全ワーカー** の queue lock を取得 → daemon validateFencing の queue 読み取りを完全 block
- `sm.LockCommand(opts.CommandID)` (line 235): state lock も取得 → daemon RegisterRetryTaskInState を block
- `writeAndCommitRetryQueue` line 346: 元タスクの queue entry を Cancelled に更新してから state save

#### Daemon auto-retry の入場ガード

- `validateFencing` (`internal/daemon/result_write_phase_a.go:218-306`) line 263-274: `queueTask.Status != model.StatusInProgress` なら `FENCING_REJECT_STATUS`
- `resultWritePhaseA` line 60-78: `acquireFileLock()` (scanMu.RLock) + `queue:<reporter>` lock を取得してから読む
- 両 lock は Planner の `lockWorkerQueues` と排他関係 → Planner が先行して queue X を Cancelled にしていれば daemon は遅延的に validateFencing で reject される

### Race scenario (シミュレーション)

- T0: Worker X が失敗、daemon に result_write が到着 (`status=failed`, `retry_safe=true`)
- T1: 操作者が `plan add-retry-task --retry-of X` を発行
- 順序 A: T1 が先に state lock + queue lock を取得 → 元タスク X を Cancelled にして state save → daemon の T0 が validateFencing で `queueTask.Status == Cancelled` を観測し `FENCING_REJECT_STATUS` を返す。**daemon retry は走らない**
- 順序 B: T0 が先 → daemon の `evaluateRetry` → `RetryTaskAtomically` → state lock 取得して RegisterRetryTaskInState (retry-A 登録)。Phase B で X を Cancelled。Lock 解放後に T1 が validateRetryRequest で `RetryLineage[retry-A] = X` && retry-A 非 terminal を観測 → 拒否。**Planner retry は走らない**

両方向で active 2 本レースは発生しない。

### 補足: daemon 単独経路の active 1 本制約

- daemon は `RetryTaskAtomically` を Phase A の `phaseAResult.retryTask != nil` を満たすケースでのみ呼び出す。`evaluateRetry` の前提として task は in_progress 状態 (validateFencing 通過後) でなければならず、同一タスクの第 2 回 result_write は idempotency でブロックされる (`validateFencing` line 244-258)
- 従って daemon が同一 predecessor に対し 2 度 RegisterRetryTaskInState を呼ぶ経路は構造的に存在しない

---

## F7: tmux Socket 分離 / Worktree Cleanup / Git Fallback

判定: **問題なし**

### tmux socket 分離 (`internal/tmux/session.go:354-408`)

- `BuildMaestroSocketName(projectName, maestroDir)` line 381-395: `maestro-<projectName>-<sha256(canonicalDir)[:8]>` 形式
- `canonical = filepath.EvalSymlinks(filepath.Clean(filepath.Abs(maestroDir)))` で `/tmp` と `/private/tmp` を統一
- `tmuxArgs(args)` (line 399-408) は `socket != "" → -L socket` を全 tmux 呼び出しに前置
- maestro instance 同士は socket が分離されており、片方の tmux server を kill しても他方に影響しない (MEMORY: `tmux_per_instance_socket_isolation`)

### Worktree cleanup unstattable fallback (`internal/daemon/worktree/git_operations.go:351-393, 515-650`)

- `gitAddAllWithUnstattableFallback`: bounded by `gitAddAllAttemptLimit = 5`、`appendToGitInfoExclude` で worktree-local exclude (`.git/info/exclude`、tracked `.gitignore` は触らない)
- `gitWorktreeAddWithUnstattableFallback`: 失敗時に `os.RemoveAll(worktreePath)` → `--no-checkout` で再試行 → `update-index --skip-worktree` + `checkout-index -a -f` で残りをマテリアライズ
- error message regex (`unstattablePathRe`, `worktreeJustWrittenPathRe`) はそれぞれ stable な git error format に合致
- isUnstattableError は `unable to stat ...: Operation not permitted | Permission denied` のいずれにも match

### Worktree cleanup の race / leak

- `Manager.GC` (Phase scan の最後で 60 scan に 1 回呼び出し、`internal/daemon/scan_phase_executor.go:103-107`) が孤児 worktree を回収
- `CleanupAll(ctx)` を Shutdown 中に呼ぶ (`daemon_shutdown.go:169-181`)、残り時間ベースの context timeout 付き

問題は確認できず。

---

## MEMORY.md 既知レース対策の崩れ確認

MEMORY.md 項目を主要なものから確認:

| 項目                                                                     | 状態                                                                            | 根拠                                                                                                                                                                             |
| ------------------------------------------------------------------------ | ------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `retry chain は predecessor あたり active 1 本まで`                      | **維持**                                                                        | F6 セクションで詳述                                                                                                                                                              |
| `retry enqueue 後は original task を即 cancelled-superseded`             | **維持**                                                                        | `result_write_phase_b.go:194-215` `applyTaskStateProgression` で `retryScheduled=true` 時に `state.TaskStates[taskID] = StatusCancelled` を直接 set。`repair_pending` 経由しない |
| `confirmation prompt 検出時は lease を維持`                              | **維持**                                                                        | `paneactivity/tracker.go:651-663` に `VerdictBlocked` 定義、`extend, do not release` のコメントあり                                                                              |
| `blocked prompt は VerdictBlocked、MarkProgress を呼ばない`              | **維持**                                                                        | `tracker.go:731-733` `Keep BlockedSince untouched` コメント                                                                                                                      |
| `tmux pane capture は main + alternate screen 両方`                      | **維持**                                                                        | `queue_handler.go:233-249` の `capturePaneJoinedFromTmux` 説明                                                                                                                   |
| `awaiting_fill_stall は 3 連続 fire + wall-clock 15min で escalate`      | **維持** (注: 本レポート F5-1 で escalate 失敗時の delete タイミングのみ要改善) | `queue_scan_phase_a_phase.go:530-569` 定数定義                                                                                                                                   |
| `failed phase でも全 task terminal なら merge を回す`                    | **維持**                                                                        | `phaseTasksAllTerminal` (`queue_scan_helpers.go:219-240`) が phase status による skip を行わない                                                                                 |
| `tmux server を maestro instance ごとに socket 分離`                     | **維持**                                                                        | F7 セクションで詳述                                                                                                                                                              |
| `git add -A の Operation not permitted は worktree-local exclude で吸収` | **維持**                                                                        | F7 セクションで詳述                                                                                                                                                              |
| `git worktree add も unstattable fallback 適用`                          | **維持**                                                                        | F7 セクションで詳述                                                                                                                                                              |
| `up -f は in_progress command を quarantine/lost_commands/ に退避`       | 未確認 (本監査対象外)                                                           | —                                                                                                                                                                                |
| `Worker context >=97% used で fast-fail`                                 | 未確認 (本監査対象外)                                                           | —                                                                                                                                                                                |

崩れている対策は検出されなかった。F5-1 はあくまで「失敗時のリカバリ遅延」であり、3 連続 fire + wall-clock 15min の invariant は守られている。

---

## 修正優先度の整理

### すぐ修正すべき (即修正 / minor)

| ID   | 内容                                                         | 修正の方向性                                                                 |
| ---- | ------------------------------------------------------------ | ---------------------------------------------------------------------------- |
| F4-1 | `ValidateResultWriteRequest` に `LeaseEpoch >= 0` 検証を追加 | heartbeat 側 (`task_heartbeat_handler.go:97-99`) と対称化。10 行未満のパッチ |
| F1-1 | `Bus.Close` の `closeWg` コメントの contract 訂正            | 「callers can verify」の節を消すか `WaitDone()` 公開メソッドを追加           |
| F1-2 | `Tracker.ForgetAgent` で `hintLogState` も削除               | 1 行追加。現行は実害なしだが defensive clean-up                              |

### 設計議論が必要 (major / 仕様変更)

| ID   | 内容                                                       | 議論ポイント                                                                                                                                                                                                                                                                        |
| ---- | ---------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| F2-1 | UDS handler 群への context 伝搬                            | `uds.Handler` シグネチャを `func(ctx, *Request) *Response` に変更すると全 handler の修正 + 各 handler 内で context.Done を観測するロジック追加が必要。daemon shutdown 時間短縮効果と修正コストのトレードオフ                                                                        |
| F5-1 | `escalateAwaitingFillStall` の FireCount delete タイミング | phase transition 失敗時に FireCount を保つと、次 scan で即 escalate を再試行できる。失敗が瞬間的な I/O glitch ならむしろ有利。ただし persistent error (sandbox / disk full) の場合は scan tick ごとに escalate が走り log spam するため、retry budget や exponential backoff が必要 |

### 範囲外 (本監査では検証完了せず)

| 項目                                               | 理由                                                                         |
| -------------------------------------------------- | ---------------------------------------------------------------------------- |
| daemon shutdown timeout 中の goroutine leak の実測 | `runtime.Stack(buf, true)` でダンプされる goroutine の所属を実機確認する必要 |
| `up -f` archive 機構                               | F7 観点には触れたが詳細未確認                                                |
| Worker context fast-fail (97%)                     | F1 観点に間接的に該当するが本監査では未検証                                  |

---

## 監査の信頼度

### Grounding

- **確認済み**: 根拠引用に file_path:line_number を併記。コード本文を Read で読んでいる範囲は authoritative
- **推定**: F2-1 の production 影響度評価 (「production 影響はおそらく低」) は handler 群を全数読んでいないため推定。ほとんどの handler は disk I/O だが、長時間 I/O を含むものが残っている可能性は否定できない
- **不確実**: F5-1 の "phase transition 失敗" がどの I/O 経路で発生するかは未踏査。`stateMgr.ApplyPhaseTransition` の実装を読まないと再現条件を特定できない

### 範囲制約

- **本監査は Grep + Read のみ**。`go vet`, `go test`, race detector の実行は禁止条件のため、データレース実測は行っていない (race condition の判定は静的読解ベース)
- 監査時間が限られたため、`internal/daemon/` 以外 (formation, agent, plan の一部) は重点ファイルのみの読解。観点 F の主戦場である `daemon/` 配下は概ね網羅したが、すべての goroutine の context 伝搬を確認したわけではない (例: `qualityGateDaemon.eventLoop` 等は構造概要のみ)
