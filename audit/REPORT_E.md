# 観点 E: テスト品質監査レポート

- 監査範囲: maestro_v2 リポジトリ全体 (`git ls-files '*_test.go'` で 263 ファイル)
- 監査手段: Read / Grep のみ (テスト実行は禁止)
- 監査者: worker1 (quality-assurance persona)
- 報告日: 2026-05-08
- task_id: task_1778225393_881c0bf2498c29ad

## サマリ

| 観点 | 判定 | 主要な根拠 |
|---|---|---|
| E1: assertion 不在 / 常に true | **問題あり (低〜中)** | 5 件 (path-coverage 専用 / no-panic 専用テスト) |
| E2: 実装側を真に検証していない (mock 過剰) | **問題なし** | mock は `internal/testutil/mocks` と test 内部 `mockPaneIO` に隔離。`Calls` カウント検証も実施 (`result_handler_test.go` 等) |
| E3: 実装側のテスト用ハック | **要確認 (低)** | production 側に test-only seam が 2 箇所 (`testPublishResetHook`, `startupReconcileHook`)。設計上は許容範囲だが production runtime に test 専用フィールドが露出 |
| E4: flaky 兆候 | **問題あり (中)** | 性能アサート 6 件が時間ベース。`time.Sleep` でのポーリング待機 1 件あり |
| E5: fixture / golden file の orphan | **問題なし** | testdata は 1 ファイル (`internal/tmux/testdata/inputrecorder/main.go`) のみで参照済 |
| E6: skip 放置 | **問題なし** | skip 30 件すべて環境ガード (バイナリ / OS / FS / 権限 / 環境変数)。永続放置 TODO は 0 件 |
| E7: integration / unit の境界 | **問題あり (中)** | build tag `integration` を持つテストと、ファイル名に `integration` を含むが build tag を持たないテストが共存。`go test ./...` で重 integration テストが暗黙に走る |

### 件数 (重大度別)

- **critical**: 0
- **major**: 2 (E4-1, E7-1)
- **minor**: 8 (E1-1〜E1-4, E3-1, E3-2, E4-2〜E4-4, E4-6)
- **nit**: 3 (E1-5, E4-5, E7-2)

注: 単一 finding が観点をまたぐ場合があるため、件数合計と finding 数は一致しない。

### 観点別件数

- E1: 5 件 / E2: 0 件 / E3: 2 件 / E4: 6 件 / E5: 0 件 / E6: 0 件 / E7: 2 件

---

## E1: Assertion 不在 / 常に true / 弱いアサート

判定: **問題あり (低〜中)**

### E1-1 (minor)

- file: `internal/agent/executor_test.go:1017-1035`
- 関数: `TestExecute_ModeClear_Success`
- 種別: 結果値破棄 + コメントによる正当化 (`_ = result // result depends on mock timing; path coverage is the goal`)
- 根拠引用 (executor_test.go:1031-1034):
  > ```go
  > // ModeClear goes through waitReady + clearAndConfirm + waitStable
  > // With our mock config, this may fail due to clear confirmation timing,
  > // but the path through Execute() is exercised regardless.
  > // We verify that no panic occurs and the mode is dispatched correctly.
  > _ = result // result depends on mock timing; path coverage is the goal
  > ```
- 原因仮説: ModeClear の executor 配信パスが mock の単純な逐次応答で網羅できない (clearAndConfirm のタイミング検証が困難) ため、テスト名は `_Success` だが結果値検証を放棄している。テストの名前と挙動が乖離している。
- 影響: テスト名 (`*_Success`) が実態 (no-panic + path coverage) と乖離しており、CI が grean でも回帰検出できない可能性。
- 対応: テスト名を `_DoesNotPanic` 等に改名、または mock を deterministic に再設計して `result.Success`/`result.Error` を assertion 化する。

### E1-2 (minor)

- file: `internal/formation/process_manager_test.go:95-104`
- 関数: `TestOsProcessManager_Signal_NegativePID`
- 種別: error 値破棄 (`_ = err`) + コメントで正当化 ("Just ensure no panic")
- 根拠引用 (process_manager_test.go:99-103):
  > ```go
  > // Negative PID should return error (or be handled by OS)
  > err := pm.Signal(-1, syscall.Signal(0))
  > // -1 sends to all processes the caller has permission to signal,
  > // which may or may not error. Just ensure no panic.
  > _ = err
  > ```
- 原因仮説: PID=-1 の OS 動作が POSIX 仕様で「全プロセスへ送信」になりエラー値が環境依存になるため。
- 影響: 「Negative PID で何が起きるか」を検証できておらず、リファクタで挙動が変わっても気づけない。
- 対応: テスト名を `_DoesNotPanic` に改名するか、PID=-1 の `pm.Signal` で error が返ることを `errors.Is(err, syscall.EPERM)` 等で具体化する。

### E1-3 (minor)

- file: `internal/formation/daemon_lifecycle_test.go:739-745`
- 関数: `TestHasOtherMaestroSessions_NoTmuxServer`
- 種別: 戻り値破棄 (`_ = hasOtherMaestroSessions()`)
- 根拠引用 (daemon_lifecycle_test.go:741-744):
  > ```go
  > // When tmux server isn't running, ListSessions returns ErrTmuxServer.
  > // Since this test environment may not have tmux running at all,
  > // we just verify the function doesn't panic.
  > _ = hasOtherMaestroSessions()
  > ```
- 原因仮説: テスト環境で tmux server が起動していない場合と起動している場合で戻り値が変動するため、明示的なアサートを避けた。
- 影響: 「tmux server なし」のケースで `false` が返るという契約を保証できない。
- 対応: tmux 不在を mock し、`hasOtherMaestroSessions() == false` を断言する。または `tmux` の有無を skip 条件にして実環境でのみ検証する。

### E1-4 (minor)

- file: `internal/model/fingerprint_test.go:258-287`
- 関数: `TestConsecutiveFailureTracker_ThreadSafety`
- 種別: -race 依存の暗黙 assertion (`// If we get here without a race condition panic, the test passes`)
- 根拠引用 (fingerprint_test.go:286):
  > ```go
  > // If we get here without a race condition panic, the test passes
  > ```
- 原因仮説: ConcurrentReader/Writer の同期検証を `-race` フラグの instrument 機能に委ねている。
- 影響: 通常の `go test` (race フラグなし) ではこのテストは無条件 PASS。CI で `make test-race` を回さない限り回帰検知できない。`Makefile:75-76` に `test-race` ターゲットは存在するが、デフォルト `make test` には含まれない。
- 対応: テスト名末尾に `_RaceOnly` 等の prefix を付け、ドキュメントに「-race 必須」と明示する。または assertion を追加 (Record の戻り値、IsConsecutiveDuplicate の決定的呼び出し等で具体的検証)。

### E1-5 (nit)

- file: `internal/plan/complete_test.go:737`
- 関数: `TestComplete_H3_StaleTaskResults_DetectedAndRetried` (の前関数 `TestComplete_RecoverFromIntent`)
- 種別: 未使用 import を回避するためのダミー参照 (`_ = strings.TrimSpace`)
- 根拠引用 (complete_test.go:736-737):
  > ```go
  > // Sanity: avoid unused-import lint when strings package is dropped.
  > _ = strings.TrimSpace
  > ```
- 原因仮説: かつてテスト本文で `strings.TrimSpace` を使用していたが、削除後も import を残したまま整理されていない。
- 影響: テスト挙動には影響なし。コード衛生上の臭いのみ。
- 対応: 該当 import を削除 (`strings` がほかで使われていないことを確認した上で)。

---

## E2: Mock 過剰で何も保証していない

判定: **問題なし**

### 根拠

- mock 定義は test-only パッケージに完全隔離されている:
  - `internal/testutil/clock.go`: `FakeClock` (deterministic clock for time-dependent tests)
  - `internal/testutil/mocks/executor.go`: `MockExecutor` (records `Calls` slice, configurable per-call results)
  - `internal/agent/test_helpers_test.go:13-` の `mockPaneIO`: callback / sequence / default の 3 階層 fallback で structured。
- production code には `Mock`/`Fake`/`Stub` 型は存在しない (確認: `grep -rEn "type.*[Mm]ock\|type.*[Ff]ake\|type.*[Ss]tub" --include="*.go" | grep -v "_test.go"` の結果は testutil パッケージのみ)。
- mock 利用テストでも実プロダクションコードの interaction を検証している例:
  - `internal/daemon/result_handler_test.go:101-104`: `if len(mock.Calls) != 1 { t.Fatalf(...) }` で executor 呼び出し回数を厳密検証。
  - `internal/daemon/result_handler_test.go:851`: `if len(mock.Calls) != 2` (2 回呼ばれることを検証)。

mock 過剰 (mock の戻り値だけを assert する空回りテスト) を示すコードは検出されなかった。

---

## E3: 実装側のテスト用ハック

判定: **要確認 (低)**

production code 内に test-only フィールド / hook が存在する。設計上は dependency injection seam として許容されるが、production binary に test 専用名のフィールドが残っている点は要監視。

### E3-1 (minor)

- file: `internal/daemon/worktree/manager.go:92-94` (定義) + `internal/daemon/worktree/publish.go:321-322` (利用箇所)
- 種別: production 構造体に test-only フィールドを公開 (unexported だが production binary に含まれる)
- 根拠引用 (manager.go:92-94):
  > ```go
  > // testPublishResetHook, if non-nil, replaces git reset --hard HEAD during
  > // PublishToBase. Used only for testing error paths. Must be nil in production.
  > testPublishResetHook func() error
  > ```
- 原因仮説: `git reset --hard` の失敗パスを deterministic に検証するため、test 側から replace できる seam を入れている。
- 影響: production binary に test 専用フィールドが残り、誤って production 経路で値が設定された場合の挙動が不定になる (現状はコメントベースの規約のみ)。binary footprint 上は微小だが品質ゲートとしては脆弱。
- 対応 (設計議論レベル): build tag (`//go:build maestro_testseam`) で実体を分離するか、interface 経由の DI にして production 側では interface を直接呼ぶ形に整理する。

### E3-2 (minor)

- file: `internal/daemon/daemon.go:124-128` (定義) + `internal/daemon/daemon_startup.go:380-388` (利用箇所)
- 種別: production 構造体に test-only `startupReconcileHook` を露出
- 根拠引用 (daemon.go:124-128):
  > ```go
  > // startupReconcileHook, when non-nil, is called instead of
  > // worktreeManager.Reconcile() in startRuntime(). It allows tests to inject
  > // slow reconciliation without a real git repository. Must be nil in
  > // production.
  > startupReconcileHook func()
  > ```
- 原因仮説: 起動時 reconcile の遅延注入で「reconcile が server.Start を block しない」回帰テストを動かすため。
- 影響: E3-1 と同様。「Must be nil in production」の規約はコメント任せ。
- 対応 (設計議論レベル): E3-1 と同じ。

### E3 — 適切に設計された test seam の例 (参考、findings ではない)

- `internal/lock/lock_order_disabled.go` (`//go:build !lockorder`) と `internal/lock/lock_order_enabled.go` (`//go:build lockorder`) のペア — production build には no-op 実装 (`noopOrderChecker`) のみが含まれ、デバッグ実装は build tag で物理的に除外。これが理想形。
- `internal/formation/watch_loop.go:36` の `sleepFn` フィールド — `WithSleepFn` option で test 時に override。option pattern なので test と production の経路が明確に分離されている。
- `internal/agent/launcher.go:1143` の `MAESTRO_AGENT_TMPDIR_OVERRIDE` env var — production の運用フラグ (test 専用ではない)。

---

## E4: Flaky テスト兆候

判定: **問題あり (中)**

時間ベースのアサートは macOS / Linux CI の負荷状況に依存しやすい。`time.Sleep` を含むテストは 10 件あるが、ほとんどが「最低保証」(下限) 系で、上限系の方がリスクが高い。

### E4-1 (major)

- file: `internal/daemon/event_hook_integration_test.go:217-250`
- 関数: `TestEventHookPerformance`
- 種別: 100 iteration の平均時間が `5*time.Millisecond` を超えると fail
- 根拠引用 (event_hook_integration_test.go:235-249):
  > ```go
  > for i := 0; i < iterations; i++ {
  >     bus.Publish(events.EventTaskStarted, map[string]interface{}{
  >         "task_id": "task_perf",
  >     })
  >     time.Sleep(1 * time.Millisecond) // Simulate minimal processing
  > }
  >
  > elapsed := time.Since(start)
  > avgPerIteration := elapsed / time.Duration(iterations)
  >
  > // Average iteration time should be close to 1ms (our sleep time)
  > // If events are blocking, this would be much higher
  > if avgPerIteration > 5*time.Millisecond {
  >     t.Errorf("average iteration time %v too high, events may be blocking", avgPerIteration)
  > }
  > ```
- 原因仮説: 1ms スリープ ×100 で 100ms ターゲット、5ms × 100 = 500ms が上限。負荷の高い CI / `-race` 下では 1ms スリープがそもそも不安定になり閾値を超える可能性が高い。
- 影響: macOS GitHub Actions runner / `-race` 環境で flaky 化する可能性。
- 対応: `events` パッケージに publish 経路の `latency_ns` 計測を入れて、subscriber 配信パスを synchronous にした state でユニットテストする (sleep を使わない deterministic 化)。または閾値を緩和し threshold を `os.Getenv` で調整可能にする。

### E4-2 (minor)

- file: `internal/daemon/reviewer/dispatcher_test.go:151-168`
- 関数: `TestDispatch_Async_NonBlocking`
- 種別: 単一呼び出しが `100*time.Millisecond` を超えると fail
- 根拠引用 (dispatcher_test.go:163-165):
  > ```go
  > if elapsed > 100*time.Millisecond {
  >     t.Errorf("Dispatch took %v; expected non-blocking return", elapsed)
  > }
  > ```
- 原因仮説: `Dispatch` が同期的処理に退化したことを検出する目的。
- 影響: CI 過負荷時に false positive。
- 対応: 「内部 channel への enqueue が完了したら return」を別経路で検証 (channel buffer 充足チェック等)。

### E4-3 (minor)

- file: `internal/daemon/event_hook_integration_test.go:166-178`
- 関数: subtest `EventsAreNonBlocking`
- 種別: 単一 dispatch が `10*time.Millisecond` を超えると fail
- 根拠引用 (event_hook_integration_test.go:174-177):
  > ```go
  > // Dispatch should complete very quickly (< 10ms) even with event publishing
  > if elapsed > 10*time.Millisecond {
  >     t.Errorf("DispatchTask took %v, expected < 10ms (events may be blocking)", elapsed)
  > }
  > ```
- E4-2 と同種。閾値が更に厳しい (10ms) のでより flaky 化しやすい。

### E4-4 (minor)

- file: `internal/daemon/integration_test.go:1997-2011`
- 関数: `TestIntegration_EventHooksInvalidPayloadHandling`
- 種別: 「イベントが drop されたこと」を `time.Sleep(5*time.Millisecond)` × 6 回ポーリングで確認
- 根拠引用 (integration_test.go:2002-2011):
  > ```go
  > // Verify event was dropped: poll multiple times to ensure no evaluation occurs
  > for i := 0; i < 6; i++ {
  >     time.Sleep(5 * time.Millisecond)
  >     qg.metrics.mu.RLock()
  >     evalCount := qg.metrics.evaluationCount
  >     qg.metrics.mu.RUnlock()
  >     if evalCount != 0 {
  >         t.Fatalf("expected no evaluations for invalid payload, got %d", evalCount)
  >     }
  > }
  > ```
- 原因仮説: bridge が invalid payload を drop することを検証するため、固定時間 (30ms) で評価が発生しないことを確認。
- 影響: bridge 経由の evaluation がもし 30ms 超で遅延した場合、テストは PASS してしまう (false negative リスク)。「drop された」ことの直接証拠ではなく、間接的な「30ms 内に何も起きていない」ことの観測。
- 対応: bridge に dropped event の counter を追加し、`drop_count == 1` を assertion する。

### E4-5 (nit)

- file: `internal/daemon/integration_test.go:1769-1822`
- 関数: `TestIntegration_LogSystemHighLoadStructuredAndRateLimited`
- 種別: 2000 publish が `1*time.Second` を超えると fail。コメントで flaky 性を自己申告。
- 根拠引用 (integration_test.go:1812-1818):
  > ```go
  > // The threshold is generous (1s for 2000 publishes ≈ 0.5 ms each) because the assertion is
  > // "publishing doesn't synchronously wait on subscribers", not a strict
  > // throughput guarantee — under -race or heavy CI load instrumentation
  > // adds significant overhead, so a tight bound (e.g. 100ms) is flaky
  > // without invalidating the design property under test.
  > if publishElapsed > 1*time.Second {
  >     t.Fatalf("publish path too slow under load: ...")
  > }
  > ```
- 原因仮説: 著者が flaky 性を理解した上で意図的に閾値を緩和済。
- 影響: 1s も既に重 CI で超過するリスクあり。ただし 2 ms × 2000 = 4s subscriber work に対し 1s 閾値は十分マージンがある。
- 対応: 現状で許容。`time.After(2*time.Second)` 程度に拡大する余地もあるが、現状の閾値の根拠説明は明快。

### E4-6 (minor)

- file: `internal/uds/uds_test.go:842-872`
- 関数: `TestClient_SendRetryOnTransientError`
- 種別: `time.Sleep(50 * time.Millisecond)` でサーバー起動を遅延させ「ENOENT で client が retry する」ことを検証
- 根拠引用 (uds_test.go:850-855):
  > ```go
  > // Start server after first connection attempt fails (simulating transient ENOENT)
  > serverReady := make(chan struct{})
  > go func() {
  >     // essential: delays server start to ensure client encounters at least one ENOENT before retry succeeds
  >     time.Sleep(50 * time.Millisecond)
  >     server.Start()
  >     close(serverReady)
  > }()
  > ```
- 原因仮説: 「client が ENOENT を一度経験する」ことを deterministic に作るため、サーバー起動を遅らせている。
- 影響: 速い CI runner だと client が retry 内部の backoff で 50ms 以上待ち、ENOENT を経験しないまま成功する可能性 (false negative — テストが PASS しても retry path が走っていない)。client 側の retry interval 仕様に依存。
- 対応: client retry の試行回数を mock で観測する。または network simulator (`net.Pipe` 経由で 1 回目 ENOENT、2 回目以降 connect 成功) を入れる。

### E4-7 (時間ベースだが design 上は許容)

- `internal/daemon/spawn_tracked_test.go:70`: `time.Sleep(time.Duration(trial%5) * 50 * time.Microsecond)` — 20 trial × 5 種類の interleaving で race 経路を網羅する意図。短時間 sleep は race 検出のために必要。flaky とは判定しない。
- `internal/daemon/queue_handler_test.go:599, 614, 949, 997` — 5s〜10s 上限の channel select。十分マージン。flaky とは判定しない。
- `internal/daemon/daemon_shutdown_test.go:180`: `time.Sleep(5 * time.Millisecond)` で「instant でない」処理をシミュレート。assertion 側ではなく入力側の sleep で flaky 性は低い。

---

## E5: Fixture / golden file の orphan

判定: **問題なし**

### 根拠

- `git ls-files | grep -E "testdata|fixtures|golden"` 結果: `internal/tmux/testdata/inputrecorder/main.go` のみ。
- 当該ファイルは `internal/tmux/input_separation_e2e_test.go:48` で `srcDir := filepath.Join(filepath.Dir(thisFile), "testdata", "inputrecorder")` として参照されており、`buildInputRecorder` 経由でビルド対象。
- `*.golden`, `*.fixture`, `*.expected` 等の従来の golden file パターンは検索したが 0 件。

orphan な fixture / golden file は存在しない。

---

## E6: Skip 放置

判定: **問題なし**

### Skip 一覧 (全 30 件、すべて環境ガード)

| ID | file | 行 | 条件 |
|---|---|---|---|
| S01 | `internal/plan/state_fuzz_test.go` | 49 | seed が 1MiB YAML budget 超過時 (fuzz seed scope 制御) |
| S02 | `internal/lock/gc_test.go` | 109 | symlink 非対応 FS |
| S03 | `internal/formation/process_manager_test.go` | 59 | `start time not available on this platform` (Linux 以外?) |
| S04 | `internal/agent/launcher_test.go` | 611 | symlink 不可 |
| S05 | `internal/agent/orchestrator_integration_test.go` | 26 | `claude` CLI 不在 |
| S06 | `internal/agent/orchestrator_integration_test.go` | 82 | `MAESTRO_INTEGRATION` 未設定 |
| S07 | `internal/agent/policy_checker_test.go` | 288 | `jq` 不在 |
| S08 | `internal/agent/policy_checker_test.go` | 736 | `/tmp` 不可 |
| S09 | `internal/agent/policy_checker_test.go` | 961 | macOS 以外 (case-insensitive FS test) |
| S10 | `internal/daemon/daemon_startup_test.go` | 100 | UDS 不可 |
| S11 | `internal/daemon/integration_test.go` | 1720 | `testing.Short()` |
| S12 | `internal/daemon/reconcile/reconcile_repair_test.go` | 1147 | running as root |
| S13-17 | `internal/daemon/verify_runner_real_test.go` | 610, 655, 692, 786, 935 | `git not available` |
| S18 | `internal/daemon/worktree/merge_publish_test.go` | 263 | windows symlink semantics |
| S19 | `internal/daemon/worktree/path_guard_test.go` | 37 | windows symlink semantics |
| S20 | `internal/daemon/worktree/cleanup_gc_test.go` | 623 | windows symlink semantics |
| S21 | `internal/daemon/worktree/cleanup_gc_test.go` | 667 | windows symlink semantics |
| S22 | `internal/daemon/worktree/manager_sync_commit_test.go` | 555 | running as root |
| S23 | `internal/model/model_test.go` | 912 | templates/config.yaml 不在 (非標準位置) |
| S24 | `internal/uds/uds_test.go` | 58 | UDS 不可 |
| S25 | `internal/uds/uds_fuzz_test.go` | 35 | seed scope 外 |
| S26 | `internal/uds/client_test.go` | 112 | running as root |
| S27 | `internal/uds/server_test.go` | 122 | tmp dir path too long |
| S28 | `internal/tmux/session_test.go` | 55 | tmux 不在 |
| S29 | `internal/tmux/session_test.go` | 66 | tmux server 不可 |
| S30 | `internal/tmux/session_test.go` | 98 | symlink 不可 |

### 評価

すべての skip は「外部依存の不在」「OS / FS 機能の制約」「権限」を理由とした **正当な環境ガード**。`TODO`/`FIXME`/`disabled` キーワードでの放置は 0 件 (確認: `grep -rEn "TODO|FIXME|XXX|HACK" --include="*_test.go"` で test 内に該当ヒットなし)。

`MAESTRO_INTEGRATION=1` を要する S06 は CI 設計上の話 (E7 と関連) なので別途検討。

---

## E7: Integration test と unit test の境界

判定: **問題あり (中)**

build tag `//go:build integration` を使うテスト群と、ファイル名や構造から見て integration test だが build tag を持たないテスト群が混在する。`go test ./...` 実行時に重 integration test が暗黙に走る (ファイル / テスト関数命名が "Integration" でも build tag なし)。

### E7-1 (major)

- 対象ファイル: 以下 5 件は名前 / 内容が integration test だが build tag を持たない:
  - `internal/daemon/integration_test.go` (~2100 行、quality gate / event bridge / dashboard formatter / scenario 23-29 などを含む)
  - `internal/daemon/event_hook_integration_test.go`
  - `internal/daemon/event_bridge_test.go`, `internal/daemon/event_bridge_timeout_test.go`
  - `internal/daemon/phase_a_daemon_integration_test.go`
  - `internal/daemon/phase_integration_test.go`
  - `internal/daemon/e2e_boundary_integration_test.go` 以外の "boundary" 系テスト群 (`boundary_dispatch_test.go` 等)
- 一方、build tag を持つ integration test:

```
//go:build integration
internal/daemon/e2e_boundary_integration_test.go:1
internal/daemon/e2e_integration_test.go:1
internal/daemon/export_e2e_test.go:1
internal/daemon/signal_integration_test.go:1
internal/plan/state_integration_test.go:1
```

- 種別: build tag 規約の不統一
- 原因仮説: 過去にファイル別で build tag を導入していたが、新規 integration test 追加時に build tag をつけ忘れた / Scenario 23-29 のように quality gate を直接呼ぶテストを `integration_test.go` という名前にしただけで build tag を入れなかった。
- 影響:
  1. `go test ./...` のデフォルト走行で、整合性検証用の integration test が走り、unit test 走行時間を膨らませる。
  2. `make test-race` のようなレースモードで重 integration が走り、本来 race 検出の対象でないテストの flaky 性が顕在化しやすい (例: E4-5)。
  3. 新規開発者が「どれが unit でどれが integration か」を判断する基準が曖昧。
- 対応:
  - **要設計議論**: integration の定義を明文化し、(a) 外部依存 (git / claude / tmux) を要するもの、(b) daemon scaffold を立ち上げて end-to-end 経路を検証するもの、を区別。後者を `//go:build integration` に揃えるか、別ディレクトリ (`internal/daemon/integration/`) に物理的に分離する。

### E7-2 (nit)

- file: `internal/agent/orchestrator_integration_test.go`
- 種別: build tag を使わず `MAESTRO_INTEGRATION=1` の env var で skip 制御
- 根拠引用 (orchestrator_integration_test.go:80-83):
  > ```go
  > func TestIntegration_OrchestratorRoleAwareness(t *testing.T) {
  >     if os.Getenv("MAESTRO_INTEGRATION") == "" {
  >         t.Skip("set MAESTRO_INTEGRATION=1 to run integration tests")
  >     }
  > ```
- 原因仮説: 同ファイル内に `TestIntegration_OrchestratorToolsConfig` (env 不要の純粋 unit test、claude CLI 不要) も並んでおり、build tag では分離できなかった。
- 影響: テストファイル単位での build tag 分離ができず、env-based skip が混在。`go test ./...` が常に skip メッセージを出す。
- 対応: `TestIntegration_OrchestratorToolsConfig` を別ファイル (`orchestrator_tools_config_test.go`) に移し、`orchestrator_integration_test.go` 全体に `//go:build integration` を付与する。

---

## 「すぐ強化すべき」と「設計議論が必要」の分離

### すぐ強化すべき (low-cost、影響範囲が局所的)

| ID | finding | 想定対応コスト |
|---|---|---|
| E1-2 | `TestOsProcessManager_Signal_NegativePID` を `_DoesNotPanic` に改名 / 具体 assertion 追加 | 1h |
| E1-3 | `TestHasOtherMaestroSessions_NoTmuxServer` を skip ガード化 | 1h |
| E1-5 | `_ = strings.TrimSpace` の削除 + 不要 import 削除 | 10min |
| E1-1 | `TestExecute_ModeClear_Success` を `_DoesNotPanic` 改名 | 1h |
| E4-2, E4-3 | 100ms / 10ms 性能アサートを `os.Getenv("MAESTRO_TEST_PERF_RELAX")` で緩和可能に | 2h |
| E4-4 | `bridge_dropped_count` メトリクスを追加して fixed-sleep を排除 | 4h |

### 設計議論が必要 (構造変更を伴う)

| ID | finding | 議論ポイント |
|---|---|---|
| E1-4 | `-race` 必須テストの命名規則 / Makefile 統合 | `go test ./...` で race build を要求するか、テスト名で `_RaceOnly` を分離するか |
| E3-1, E3-2 | production 構造体への test seam 露出 | build tag による分離 vs interface 注入 vs 現状維持 |
| E4-1 | event hook performance test の deterministic 化 | events パッケージに synchronous mode を入れるか、CI 緩和 env で許容するか |
| E4-6 | uds client retry test の deterministic 化 | mock listener / network simulator の導入 |
| E7-1 | integration test の build tag 規約統一 | 物理ディレクトリ分離 vs build tag 統一。CI matrix の見直しも要 |
| E7-2 | `orchestrator_integration_test.go` のファイル分割 | E7-1 と一括で対応 |

---

## 監査メモ (証拠ベースの集計)

```
test files:                 263
test functions (Test*):     3268
benchmark functions:        8
fuzz functions:             2
subtests (t.Run):           462
t.Helper() usage:           210
TempDir/MkdirTemp:          457
testify (require/assert):   682 (= テスト関数の約 21%)
t.Fatal/Error/Errorf:       3339 (= テスト関数の約 102%、平均 1+ assertion/テスト)
time.Sleep in tests:        13 occurrences (10 unique callers, うち 6 が flaky 兆候候補)
time.Now in tests:          760
time.After in tests:        ~30+ (タイムアウト目的、flaky 直結ではない)
t.Skip / t.Skipf:           30 (すべて環境ガード)
t.Setenv:                   21 (good: auto-restoring)
os.Setenv:                  2 (TestMain or defer-restored)
build tag (integration):    5 ファイル
testdata:                   1 ファイル (inputrecorder/main.go) - 参照済
```

これらの数値はテストカバレッジ・assertion 密度ともに健全であることを示す (assertion 数 / テスト数 ≈ 1.02)。問題は **個別の弱いテスト** と **境界規約** に局所化している。

---

## 監査の限界 (不確実性レベル)

- `go test` 実行禁止のため、本レポートの flaky 兆候 (E4) は **コード読解ベースの推定**。実際の flake rate は CI 過去ログを別途精査する必要あり。
- mock 過剰の判定 (E2) は静的検査ベース。「mock を呼ぶだけで production 経路を網羅していないテスト」を完全網羅するには、各テストの assertion とプロダクションの呼び出しを照合する必要があり、本監査では高頻度パターン (`mocks.MockExecutor`, `mockPaneIO`) のみを抜き取り検査した。
- 観点 E3 の test seam は production binary 内に残存するが、コメント規約 (`Must be nil in production`) を逸脱して production 経路で値が設定された事故が過去にあったかは memory / commit history を別途精査する必要あり。
