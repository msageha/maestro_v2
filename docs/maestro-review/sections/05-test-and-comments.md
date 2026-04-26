# 05. テスト品質・コメントレビュー

## サマリー

- **検出件数**: テスト品質 9 件 / コメント 7 件 (合計 16 件)。
- **テスト全体傾向**: テスト本数は 247 ファイルと豊富で、`t.Parallel()` の利用、`time.Date` による決定論的時刻、worktree やプラットフォーム差異への `t.Skip` ガードなど **基本的な品質は高い**。一方で (a) hook script の **ソース文字列を `strings.Contains` で確認するだけ** の脆弱なアサーション、(b) **`testing.Short()` を導出根拠不明のまま skip ガード**として用いる例、(c) **`time.Sleep` を並行制御プリミティブ代わり**にする箇所 が散在する。table-driven 化で簡潔にできる繰り返しテスト群もある。`gomock`/`mock.New` 系は未使用 (testify のみ)。
- **コメント全体傾向**: `TODO` は 8 件のみで `FIXME/HACK/XXX` は 0 件。ただし `TODO` は **担当者・日付・追跡 ID なし** で残されており、根拠 issue を付与しないと放置リスクが高い。20 ファイルで **日英コメント混在** があり、ファイル単体では一貫しているもののプロジェクト全体では基準が不明。コメント残骸 (関数削除後のオーファン) は検出されなかった。

---

## Findings — テスト品質

### F-T-01: `TestLoadState_MigratorIntegration_OlderVersion` がほぼ no-op

| 項目 | 値 |
|---|---|
| カテゴリ | 無駄テスト / テスト hack |
| 重要度 | High |
| 場所 | `internal/plan/state_test.go:735-784` |
| 現状の問題 | `currentSchemaVersion` が `const = 1` (`migrator.go:6`) のため `if currentSchemaVersion <= 1` 分岐で常に return し、移行ロジック自体は呼ばれない。コメントに「Since currentSchemaVersion is const=1, NeedsMigration(1) returns false. We can only fully test migration when schema evolves beyond v1.」と自認しているが、テスト名は「Migration_OlderVersion」のまま。`origMigrator`/`testMigrator` の差し替え処理は機能していない (実行はされるが LoadState に影響しない)。 |
| 推奨アクション | 名称を `TestLoadState_NoMigrationNeededAtCurrentVersion` 等に変更するか、`migrator_test.go` 側の単体テストでカバーした上で本テストは削除する。スキーマ v2 が導入された段階で本格的な migration テストを書く。 |

### F-T-02: `policy_checker_test.go` がハードコードされた正規表現パターンを `strings.Contains` で照合

| 項目 | 値 |
|---|---|
| カテゴリ | 不安定要素 / assertion 不足 |
| 重要度 | Medium |
| 場所 | `internal/agent/policy_checker_test.go` (42 件の `strings.Contains(hookScript, ...)`、特に L289-365, L1813-1823 など) |
| 現状の問題 | hook script の **ソースコード断片** (例: ``\b(bash|sh)\s+-[a-zA-Z]*c\b``, ``rm\s+-[a-zA-Z]*[rR][a-zA-Z]*f``) を文字列一致で検証しており、機能テストではなく実装一致テストになっている。リファクタで等価な regex に書き換えると意味なく落ちる。L289-313 `TestHookScript_BlocksPipeToShell` はループ中の `blocked` 値を実行に使わず、結果として `B001` というラベル文字列が含まれているかしか見ていない (L307-312 の `if !strings.Contains(...)` ブロックは中身が空でデッドコード)。 |
| 推奨アクション | `requireJq(t)` 経由で実 hook を起動する S シリーズのテスト (L401〜) に統合し、`strings.Contains` 系は冗長として削除。少なくとも L307-312 の空条件は削除する。 |

### F-T-03: `testing.Short()` による skip 条件の根拠不明

| 項目 | 値 |
|---|---|
| カテゴリ | テスト hack |
| 重要度 | Medium |
| 場所 | `internal/daemon/panic_recovery_test.go:51-53,121-122`, `internal/daemon/integration_test.go:1635-1637` |
| 現状の問題 | パニックリカバリは実時間 5 秒以内で完了する想定 (`panicRecoveryTimeout = 5 * time.Second`) であり「short モードで重い」根拠が乏しい。一方 `QualityGatePerformanceUnderLoad` (8 worker × 40 評価) は妥当な skip 対象。同じ `testing.Short()` ガードでも判断が一貫せず、CI ランナーが `-short` を付けると panic recovery が走らずカバレッジが落ちる。 |
| 推奨アクション | panic recovery 系は `testing.Short()` ガードを外すか、`-short` で skip する明確な閾値 (例: 1s 超) を導入してコメントに記載する。 |

### F-T-04: `time.Sleep` を並行同期に使うパターン

| 項目 | 値 |
|---|---|
| カテゴリ | 不安定要素 |
| 重要度 | Medium |
| 場所 | `internal/uds/uds_test.go:852` (`time.Sleep(50ms)` で ENOENT を再現), `internal/daemon/integration_test.go:1696` (`Intentionally slow subscriber`), `internal/daemon/integration_test.go:1919-1920` (`for i:=0; i<6 { time.Sleep(5*ms) ... }`), `internal/daemon/event_bridge_timeout_test.go:103`, `internal/daemon/spawn_tracked_test.go:70` |
| 現状の問題 | CI 高負荷時に `time.Sleep` の境界が崩れて flake を起こす。debounce_controller_test.go:73 では「replacing non-deterministic time.Sleep with deterministic signaling」とコメントされており、置換方針は既に確立されているが他箇所には適用されていない。 |
| 推奨アクション | チャネル `<-readyCh` / `require.Eventually` / fake clock (`boundary_notify_test.go:754` の方針) に置換する。`Intentionally slow subscriber` も chan blocking で表現可能。 |

### F-T-05: `TestSleepWithBackoff_ExponentialIncrease` のタイミング検証が脆い

| 項目 | 値 |
|---|---|
| カテゴリ | 不安定要素 |
| 重要度 | Low |
| 場所 | `internal/agent/message_deliverer_test.go:467-476` |
| 現状の問題 | `elapsed < 3*time.Millisecond` でしか検証していない。1ms 単位の閾値はスケジューラ揺らぎで偽陽性が出やすく、また期待値 4ms に対して 3ms 下限のみで上限がないため指数増加性は実質テストしていない。 |
| 推奨アクション | base interval を 50-100ms に上げるか、呼び出し回数とバックオフ計算式を直接検証するテストに置き換える。 |

### F-T-06: `checkCommandTasksTerminal` の 7 連続テスト関数

| 項目 | 値 |
|---|---|
| カテゴリ | 無駄テスト (重複) |
| 重要度 | Low |
| 場所 | `internal/daemon/queue_scan_phase_test.go:27-153` |
| 現状の問題 | `TestCheckCommandTasksTerminal_AllCompleted`, `_HasFailed`, `_HasDeadLetter`, `_NotAllTerminal`, `_NoTasks`, `_MixedCommands`, `_AcrossWorkers` が同一構造で 7 関数並ぶ。総行数 130 行弱だが table-driven なら 30 行程度。 |
| 推奨アクション | `tests := []struct{ name string; tasks map[string][]model.Task; wantTerminal, wantFailed bool }` で集約。 |

### F-T-07: `TestApplyDefaults` の冗長な if 連結

| 項目 | 値 |
|---|---|
| カテゴリ | 無駄テスト (冗長) |
| 重要度 | Low |
| 場所 | `internal/agent/executor_test.go:39-103` |
| 現状の問題 | 既定値検証 8 フィールド × 2 ケース (zero / non-zero) を 16 連の `if` で展開。フィールド追加時の保守コストが増える。 |
| 推奨アクション | フィールド名と期待値のテーブル + `reflect.ValueOf(cfg).FieldByName(name)` による table-driven に置換。あるいは構造体の deep equal 比較。 |

### F-T-08: `daemon_startup_test.go` の polling 待機

| 項目 | 値 |
|---|---|
| カテゴリ | 不安定要素 |
| 重要度 | Low |
| 場所 | `internal/daemon/daemon_startup_test.go:114` (`time.Sleep(10 * time.Millisecond)`), `internal/plan/retry_test.go:1528` (`time.Sleep(150ms)` で window 期限切れを再現) |
| 現状の問題 | sleep window を超えるたびにテストが伸びる。retry_test.go では 150ms の固定 sleep が決定論的だが時間コストが高い。 |
| 推奨アクション | clock モックに置き換える (sliding-window スロットリング側で fake clock を受け付けるよう小改修)。 |

### F-T-09: skip 系の根拠コメントが薄い

| 項目 | 値 |
|---|---|
| カテゴリ | テスト hack |
| 重要度 | Nit |
| 場所 | `internal/uds/uds_fuzz_test.go:35` (`t.Skip()` 引数なし), `internal/plan/state_fuzz_test.go:49` (`t.Skip()` 引数なし) |
| 現状の問題 | skip 理由が明示されておらず、なぜ skip されているかコード読まないと分からない。fuzz の seed 不適合をフィルタしている用途と推測されるが文字列が無い。 |
| 推奨アクション | `t.Skip("invalid seed: <reason>")` と書く。 |

---

## Findings — コメント

### F-C-01: `TODO(coverage)` がトラッキング情報なしで放置

| 項目 | 値 |
|---|---|
| カテゴリ | TODO 放置 |
| 重要度 | Medium |
| 場所 | `internal/plan/state_test.go:1-5`, `internal/daemon/daemon_test.go:1-9` |
| 現状の問題 | 「The following plan package files lack dedicated unit tests」「The following daemon package files lack unit test coverage. Priority order (highest first)」が package コメント先頭に列挙されているが、issue 番号も担当者も日付も無い。state_reader はテスト済みなど一部は古い記述になっている可能性が高い。`state_test.go:1` のリストに「retry_cascade.go, retry_queue.go」とあるが当該ファイルが現存するか別途検証が必要。 |
| 推奨アクション | issue 化して `TODO(coverage #NNN)` を残すか、TASKS.md / GitHub issue に移管しコメントは削除する。 |

### F-C-02: `TODO(DRY)` でリファクタ対象とゴール先まで明記済みだが ID なし

| 項目 | 値 |
|---|---|
| カテゴリ | TODO 放置 |
| 重要度 | Low |
| 場所 | `internal/daemon/queue_handler_test.go:24-27`, `internal/daemon/worktree_test_helper_test.go:14-17` |
| 現状の問題 | `Target: internal/daemon/testhelper_test.go` 等の移送先が書かれているが「Prerequisite: daemon test suite structure stabilization」の到達条件が抽象的で、いつ実施するかの判定基準がない。 |
| 推奨アクション | trigger 条件 (例: テストファイル数が N 以下に減ったら / refactor フェーズ X 完了後) を明文化するか、issue 化。 |

### F-C-03: `TODO(refactor)` がパッケージ doc に常駐

| 項目 | 値 |
|---|---|
| カテゴリ | TODO 放置 |
| 重要度 | Low |
| 場所 | `internal/daemon/daemon.go:3-20` |
| 現状の問題 | リファクタ方針として 18 行の長文計画が package コメントとして残っている (godoc に出る)。意図的なロードマップだが「Recommended decomposition direction」「This refactoring should be done incrementally」と作業未着手の語感で、godoc を見たユーザーに最新仕様か否かが伝わらない。 |
| 推奨アクション | `docs/architecture/refactor-roadmap.md` 等へ移し、godoc は完了済みの設計のみ残す。あるいはタイトルを「Refactor roadmap (status: in progress)」と明確化。 |

### F-C-04: `TODO(schema)` の意図と Status 行の不一致

| 項目 | 値 |
|---|---|
| カテゴリ | TODO 放置 |
| 重要度 | Nit |
| 場所 | `internal/plan/migrator.go:60-80` |
| 現状の問題 | `// TODO(schema): Register migration steps when currentSchemaVersion is bumped above 1.` の直下に「Status: currentSchemaVersion=1, no migrations registered yet.」と書かれており、TODO というよりも将来手順の記録。F-T-01 のテストと連動して読み手を混乱させる。 |
| 推奨アクション | `TODO` ではなく `// Migration procedure (when bumping currentSchemaVersion):` に書き換える。 |

### F-C-05: テスト内に自明な「動作のラベルだけのコメント」が散在

| 項目 | 値 |
|---|---|
| カテゴリ | 自明コメント |
| 重要度 | Nit |
| 場所 | `internal/daemon/quality_gate_test.go:102` (`// Initialize the gate engine` の直後 `qg.loadGateDefinitions()`), `:113` (`// Start the daemon` → `qg.Start()`), `:117` (`// Stop the daemon` → `qg.Stop()`), `:479,514` 同 |
| 現状の問題 | コードを読めば分かる文言で、メソッド名と完全に重複。 |
| 推奨アクション | 削除。意味的に追加すべきは「Why」(なぜここで Start するか) のみ。 |

### F-C-06: 日本語コメントと英語コメントの混在

| 項目 | 値 |
|---|---|
| カテゴリ | 言語混在 |
| 重要度 | Low |
| 場所 | 全 Go ファイルのうち 20 ファイルが日本語コメント保持。代表例: `internal/model/fingerprint.go` (大半が日本語), `internal/model/queue.go:138-189` (日本語のみ), `cmd/maestro/cmd_result.go:50-112` (英語の関数引数説明と日本語の補足が混在), `internal/daemon/quality_gate.go:202` (日本語), `internal/lock/lock_order_enabled.go:37` (日本語) |
| 現状の問題 | パッケージ単位ではほぼ統一されているが (e.g. model 配下は日本語多), `cmd_result.go` のように同一ファイル内で関数 doc は英語・補足は日本語とミックスする例がある。godoc 出力の言語が不統一になる。 |
| 推奨アクション | プロジェクト全体の方針 (godoc 公開部は英語、日本語は実装ノートに限定 など) を `CONTRIBUTING.md` で明記し段階的に統一。本レビュー内では強制移行は不要。 |

### F-C-07: `XXX` を含むテスト固定値はコメントではなくデータ

| 項目 | 値 |
|---|---|
| カテゴリ | 自明コメント (誤検知防御) |
| 重要度 | Nit |
| 場所 | `internal/model/runtime_test.go:28` (`"geminiXXX"` テストデータ), `internal/daemon/learnings_test.go:664` (`"AAAAAXXX"`), `cmd/maestro/cmd_queue_test.go:14` (`deprecated` というテキストを期待値として検証) |
| 現状の問題 | Grep で `XXX|deprecated` をかけるとヒットするが、これらは **テスト用ペイロード** であり TODO ではない。検出は誤検知だが、将来の自動化 grep の混乱要因。 |
| 推奨アクション | 必要なら `"geminiSUFFIX"` 等の中立な名前に置換。優先度は低。 |

---

## 補足 — 無風確認できた観点

- `gomock`/`mock.New` パターン: **未検出** (mock 系は testutil/mocks 配下の手書きスタブのみ)。過剰モックは現状なし。
- ポート固定の外部 I/O: `httptest.` / TCP `Listen("tcp"` は **未検出**、UDS は temp dir 内ソケットで隔離済み (`uds_test.go:67,127`)。
- `rand.Seed` / 非決定論乱数: テストファイルでは **未検出**。
- コメント残骸 (削除済み関数を指すコメント): 軽くサンプリングした範囲では未検出。
- `FIXME` / `HACK`: **0 件**。
- build tag による回避テスト: 検出範囲では `goleak_test.go` 等の正当用途のみで、テスト通すための build tag 回避は未検出。
