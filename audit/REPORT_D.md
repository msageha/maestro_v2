# 観点D 監査レポート: デッドコード・冗長コード

調査日: 2026-05-08
スコープ: `cmd/maestro/`, `internal/`, `templates/embed.go`, `MEMORY.md` 残骸チェック

調査手法: Read / Grep のみ。`go build`, `go test`, `go vet` 等のコマンドは実行していない (constraints による禁止)。
従って「production code でビルドされない」という静的判定は grep ベースの参照件数によるもので、
ビルドツールチェーン上は最終バイナリにリンクされる可能性 (例: `init()` chain や reflection 経由) を 100% 排除しきれない点は不確実 (researcher 規則に基づき注記)。

## 観点別サマリ

| 観点 | 結論 | 件数 |
|------|------|------|
| 1. 未参照シンボル / オーファンファイル | 問題あり | 12 件 (critical 2 / major 4 / minor 5 / nit 1) |
| 2. DRY 違反 | 問題あり | 2 件 (major 1 / minor 1) |
| 3. 固定値化された feature flag/experimental gate | 問題あり | 1 件 (minor 1) |
| 4. MEMORY.md 撤去済み残骸 | 問題あり (ただし観点1と重複) | 5 章中 1 章 |
| 5. 使われていない config キー、env var | 問題なし (撤去ガイダンスは適切) | 0 件 |

合計 critical: 2 / major: 5 / minor: 7 / nit: 1 = **15 件**

## 重大度ラベル定義

| ラベル | 定義 |
|--------|------|
| critical | 死コードのインポート/型継続が、新たな機能追加時の混乱や誤接続を招く規模 (パッケージ規模、または常に nil な dependency injection) |
| major | 重要 API のうち production 参照が 0 で、今後の意図しない再生・分岐の温床 |
| minor | 単独関数・ヘルパが production 参照 0 で、即削除可能 |
| nit | 純コメント/プレースホルダ、即削除可能で副作用なし |

---

## 観点1: 未参照シンボル・パッケージ

「production 参照」は grep で `_test.go` を除外したヒット数で判定。同一ファイル内の自己参照は除外する。

### D1-1 [critical] `internal/daemon/scheduler` パッケージ全体

- file_path:line: `internal/daemon/scheduler/graph.go:1-128` (全体)
- 公開シンボル: `TaskNode` (graph.go:11), `ConflictGraph` (graph.go:20), `NewConflictGraph` (graph.go:27)
- 参照件数: production = 0 件、test = `internal/daemon/scheduler/graph_test.go` (8 件、自己テストのみ)
- 検証コマンド: `grep -rn "scheduler\.\|/daemon/scheduler" --include="*.go" .` → 自パッケージ内 (graph_test.go) しかヒットしない
- 削除可否: **すぐ削除可能**。同パッケージはどこからも import されていない。`graph_test.go` (162 行) は使われない実装に対するテストなので一緒に削除してよい。
- 根拠: `grep -rn "github.com/msageha/maestro_v2/internal/daemon/scheduler"` の出力が 0 件 (production), 1 件 (テスト内の文字列定数として `"internal/daemon/scheduler"` を ExpectedPaths のサンプルパスとして使用しているのみ)

### D1-2 [critical] `internal/daemon/fallback` パッケージ + 結線が完全に休眠

- file_path:line:
  - パッケージ実体: `internal/daemon/fallback/manager.go:1-179` (179 行 + テスト 270 行)
  - 結線フィールド: `internal/daemon/daemon.go:75` (`fallbackMgr *fallback.Manager`), `internal/daemon/queue_handler.go:69` (`fallbackMgr *fallback.Manager`)
  - 結線セッタ: `internal/daemon/handler_registry.go:57-63` (`SetFallbackManager`)
  - 結線アクセサ: `internal/daemon/daemon.go:200-205` (`fallbackAccessor`), `internal/daemon/api_factory.go:27` (`fallbackMgr: d.fallbackMgr`)
  - 結線 callsite: `internal/daemon/result_write_handler.go:336-345` (`recordFallback`), インターフェース定義 `internal/daemon/result_write_handler.go:38-41` (`fallbackRecorder`)
  - 起動時の no-op: `internal/daemon/daemon_startup.go:198-206` ("Fallback manager ... intentionally not started")
- シンボル: `fallback.Manager`, `fallback.NewManager`, `fallback.Mode`, `fallback.Config`, `fallback.IsWorkerAllowed`, `fallback.LastSuccessAt`, `fallback.RecordSuccess`, `fallback.RecordFailure`, `Manager.Mode()`
- 参照件数 (production):
  - `fallback.Manager` 型: 3 件 (全て `*fallback.Manager` 型のフィールド宣言だが値は常に nil)
  - `SetFallbackManager`: 1 件 (定義のみ、callsite ゼロ)
  - `RecordSuccess` / `RecordFailure`: production callsite は `result_write_handler.go:340,342` のみだが、その前段 `if fm := h.fallbackMgr(); fm != nil` の `fm` は常に nil である (起動時に `SetFallbackManager` が呼ばれないため)。
- 検証コマンド:
  - `grep -rn "fallback\.NewManager" --include="*.go" .` → 0 件 (production)
  - `grep -rn "SetFallbackManager" --include="*.go" .` → 1 件 (定義のみ)
- 削除可否: **すぐ削除可能だが範囲が広い**。`MEMORY.md` の `fallback_degraded_mode_removed.md` で「再導入しない」と明記されている。残置は意図せず再有効化される温床。
- 根拠: `daemon_startup.go:198-206` で「intentionally not started」とコメントしつつ `*fallback.Manager` 型のフィールドと結線インフラを残しており、`MEMORY.md fallback_degraded_mode_removed.md` の禁止に正面から反する状態。
- 注意: `model/config_types.go:140` の `(Removed) Fallback struct` コメント、および `model/config_load.go:158` の deprecation 警告メッセージは適切に残されている (config 互換性のため)。削除対象は **コード側** だけ。

### D1-3 [major] `internal/model/fingerprint.go` の `ConsecutiveFailureTracker` インフラ全体

- file_path:line: `internal/model/fingerprint.go:13-161` (全体)
- 公開シンボル: `FailureFingerprint` (l.16), `ConsecutiveFailureTracker` (l.24), `NormalizeError` (l.48), `ComputeFingerprint` (l.82), `NewConsecutiveFailureTracker` (l.103), `Record` (l.114), `IsConsecutiveDuplicate` (l.125), `LastFingerprint` (l.144), `Reset` (l.156)
- 参照件数: production = 0 件、test = `internal/model/fingerprint_test.go` のみ
- 検証コマンド: `grep -rn "model\.NormalizeError\|model\.ComputeFingerprint\|model\.ConsecutiveFailureTracker\|model\.FailureFingerprint" --include="*.go" .` → 0 件
- 削除可否: **すぐ削除可能**。production 用の同等機能は `internal/daemon/learnings/fingerprint_compute.go` の `NormalizeError` / `ComputeErrorFingerprint` が果たしている (D2-1 の DRY 違反元)。
- 根拠: `result_learning.go:59` は `learnings.ComputeErrorFingerprint` を呼んでおり、model 側は呼ばれない。

### D1-4 [major] `internal/model/featuregate.go` の重複 API

- file_path:line: `internal/model/featuregate.go:13-110`
- 公開シンボル: `ValidateProfileLevel` (l.15), `DefaultFeatureProfiles` (l.26), `IsFeatureEnabled` (l.65)
- 参照件数 (production, `model.` 経由):
  - `model.ValidateProfileLevel`: 0 件
  - `model.DefaultFeatureProfiles`: 0 件
  - `model.IsFeatureEnabled`: 0 件
- production 用の featuregate は別パッケージ `internal/daemon/featuregate/evaluator.go` にあり、`phase_c_manager.go:178` 等で使われている。
- 検証コマンド: `grep -rn "model\.DefaultFeatureProfiles\|model\.IsFeatureEnabled" --include="*.go" .` → 0 件
- 削除可否: **すぐ削除可能**。テスト (`featuregate_test.go`) も同時に削除。
- 根拠: featuregate の責務は `internal/daemon/featuregate` パッケージに移行済みで、model 側は遺物。

### D1-5 [major] `internal/model/runtime.go: ValidateRuntime`

- file_path:line: `internal/model/runtime.go:13-19`
- 参照件数: production = 0 件、test = `internal/model/runtime_test.go` + `internal/daemon/phase_c_acceptance_test.go` の 2 ファイル (両方テスト)
- 検証コマンド: `grep -rn "ValidateRuntime\b" --include="*.go" .` → 4 件すべて `_test.go`
- 削除可否: **すぐ削除可能**。
- 根拠: production callsite ゼロ。同パッケージ内の `ParseRuntimeFromModel` (l.40) は活用されているが、`ValidateRuntime` は純粋に dead。

### D1-6 [major] `internal/model/fitness.go: DefaultFitnessThresholds`

- file_path:line: `internal/model/fitness.go:29-33`
- 参照件数: production = 0 件、test = `model/fitness_test.go` 2 件 + `daemon/phase_c_acceptance_test.go` 2 件 (4 件全てテスト)
- 検証コマンド: `grep -rn "DefaultFitnessThresholds" --include="*.go" .` → テストのみ
- 削除可否: **要確認**。`FitnessThresholds` 型自体が他で使われている可能性があるので、関数だけ削除して struct は残す形が無難。

### D1-7 [major] `internal/daemon/lease_manager.go` の dead aliases

- file_path:line:
  - `LeaseManagerOption` (l.14)
  - `LeaseInfo` (l.17)
  - `WithLeaseManagerClock` (l.23)
- 参照件数 (lease_manager.go 自身を除外): 0 件 (3 種すべて)
- 検証コマンド: `grep -rn "LeaseManagerOption\b\|LeaseInfo\b\|WithLeaseManagerClock\b" --include="*.go" . | grep -v "lease_manager.go:"` → 0 件
- 削除可否: **すぐ削除可能**。`LeaseManager`, `NewLeaseManager` は使われているが、上記 3 つは未使用。
- 根拠: `LeaseManager` (l.11) と `NewLeaseManager` (l.20) はテスト/production の両方で参照あり。残り 3 alias は完全 orphan。

### D1-8 [minor] `internal/daemon/dispatch_envelope.go` プレースホルダ

- file_path:line: `internal/daemon/dispatch_envelope.go:1-6`
- 内容: `package daemon` 宣言 + 4 行のコメント (移行先を示すスタブ) のみ。Go declaration ゼロ。
- 削除可否: **すぐ削除可能**。コメント内容 (`Content building methods ... have been moved to internal/daemon/dispatch/envelope.go`) は git history で十分追える。
- 根拠: ファイル全体 5 行で実装コードなし。

### D1-9 [minor] `internal/model/verify.go: LoadOrDefaultVerifyConfig` の冗長 API

- file_path:line: `internal/model/verify.go:144-153`
- 関数のドキュメント自体に「New production code should prefer LoadOrDefaultVerifyConfigForProject」と明記
- 参照件数 (production): 0 件 (`verify_runner_real.go:402` は `LoadOrDefaultVerifyConfigForProject` を呼ぶ。`LoadOrDefaultVerifyConfig` は test/`phase_c_boundary_test.go` のみ)
- 検証コマンド: `grep -rn "LoadOrDefaultVerifyConfig\b" --include="*.go" . | grep -v "verify.go:" | grep -v "_test.go"` → 0 件
- 削除可否: **要設計議論**。テストが存在しているため、削除前に `LoadOrDefaultVerifyConfigForProject` で代替できるかテスト書き換えが必要。

### D1-10 [minor] `internal/model/verify.go: DefaultVerifyConfigForProject` の dead parameter

- file_path:line: `internal/model/verify.go:78-85`
- 関数本体: `_ = projectRoot; return DefaultVerifyConfig()` (l.83-84)
- コメント (l.79-81): "projectRoot is accepted for backward compatibility ... but is no longer consulted: language detection has been removed"
- 削除可否: **要設計議論**。削除すると `LoadOrDefaultVerifyConfigForProject` のシグネチャ変更が伝播。本関数は `DefaultVerifyConfig` の thin wrapper でしかないため、callsite を `DefaultVerifyConfig()` に統合する案がある。

### D1-11 [minor] `internal/model/status.go` の dead Is* 述語

- file_path:line:
  - `IsActiveStatus` (l.482-489)
  - `IsPausedStatus` (l.492-495)
  - `IsWorktreeTerminal` (l.507-509)
- 参照件数 (production):
  - `IsActiveStatus`: 0 件
  - `IsPausedStatus`: 0 件
  - `IsWorktreeTerminal`: 1 件 (同ファイル内 l.599 の自己参照のみ)
- 削除可否: **要確認**。`IsWorktreeTerminal` は同ファイル `ValidateWorktreeTransition` から呼ばれているので残す必要あり。`IsActiveStatus` と `IsPausedStatus` は完全 orphan で削除可能。

### D1-12 [minor] `internal/model/review.go: IsTerminalReviewStatus`

- file_path:line: `internal/model/review.go:51-58`
- 参照件数: production = 0 件、test = own test file `review_test.go` のみ
- 検証コマンド: `grep -rn "IsTerminalReviewStatus" --include="*.go" .` → 4 件すべて自パッケージのテスト
- 削除可否: **すぐ削除可能**。

### D1-13 [nit] `cmd/maestro/main.go: 既存 dead code なし (確認済み)` (skip)

(該当なし。)

---

## 観点2: DRY 違反 (重複処理)

### D2-1 [major] `NormalizeError` 関数の二重実装

- 実装A: `internal/model/fingerprint.go:48` (Japanese-comment, dead in production — D1-3 と同根)
- 実装B: `internal/daemon/learnings/fingerprint_compute.go:25` (English-comment, production 用)
- 共通の責務: エラーメッセージから揮発性要素 (タイムスタンプ・行番号・絶対パス・メモリアドレス・連続空白) を除去して正規化する
- 差分:
  - 実装A: 行番号 (`reLineNumber`), Unix timestamp (`reUnixTimestamp`), 絶対パスのベース名抽出 (`reAbsPath` + `filepath.Base`), メモリアドレス → "ADDR"
  - 実装B: 行番号は除去しない、Unix timestamp は除去しない、絶対パスは `/tmp` 系のみ (`reTmpPath`), メモリアドレス → "<hex>", 数字 → "<n>"
- 検証コマンド: `grep -rn "NormalizeError" --include="*.go" .` → 上記 2 ファイルが該当
- 削除可否: **要設計議論**。一方を採用 (production 用は実装 B) し、他方 (実装 A) を削除する。実装 B の方が単純で `<hex>` / `<n>` プレースホルダを残すため fingerprint としても扱いやすい。
- 根拠: 同じ目的に対して 2 つの異なる正規化規則が並走しており、将来「fingerprint が一致するはずなのに別物として扱われる」回帰の温床。実装 A は D1-3 で削除候補。

### D2-2 [minor] `Clock` / `RealClock` の triple-hop alias chain

- 実装: `internal/clock/clock.go:13-21` (canonical)
- 第一段 alias: `internal/daemon/core/core.go:151,154` (`type Clock = clock.Clock`, `type RealClock = clock.RealClock`)
- 第二段 alias: `internal/daemon/core_aliases.go:44,47` (`type Clock = core.Clock`, `type RealClock = core.RealClock`)
- 削除可否: **要設計議論**。「再エクスポート」のドキュメンテーション目的だが、`daemon` パッケージ内で `RealClock{}` がインライン使用されているため、削除すると 50+ callsite を更新する必要あり。低優先度。

---

## 観点3: 固定値化された feature flag / experimental gate

### D3-1 [minor] config の `Fallback` block — 受け取るが効かない構造

- file_path:line: `internal/model/config_types.go:140` (`(Removed) Fallback struct`)
- 起動コード: `internal/daemon/daemon_startup.go:198-206` で「intentionally not started」と明記し、起動を skip する
- 削除可否: **要設計議論**。`config.yaml` の互換性のために形だけ残すという設計選択は妥当だが、記述側のヒントは `model/config_load.go:158` の deprecation メッセージで操作員に伝えている。**コード上の `*fallback.Manager` 結線** は D1-2 の通り削除できる。

(他の experimental gate は確認したが、`NormalizeExperimentalConfig` / `validateExperimental` は production で活用されており、固定化された旧 gate は見当たらない。)

---

## 観点4: MEMORY.md「撤去済み」項目残骸チェック

5 章別に検証する。

### 4-1: fallback_manager の degraded mode (`fallback_degraded_mode_removed.md`)

- 結果: **問題あり**
- 詳細: D1-2 を参照。パッケージ実体・結線・interface・accessor が完全に残置。`MEMORY.md` 文言 (「ワーカーをブラックリスト化する機能は再導入しない」) に対し、**再導入できる足場が常駐** している状態。
- 推奨: D1-2 の対象を削除。

### 4-2: Go policy hook (F-025) (`policy_hook_bash_only.md`)

- 結果: **問題なし**
- 検証: `grep -rn "F-025\|policy_hook_go\|policy_checker_go\|wrapper_go\|/policy_hook" --include="*.go" .` → 0 件
- 詳細: `internal/agent/policy_checker.go` には bash 一本の hook 生成ロジックのみで、Go 移植の残骸なし。

### 4-3: sensitive-file / expected_paths / max_files / message_pattern などの commit gate (`commit_policy_removed.md`)

- 結果: **問題なし** (撤去済みコメントが適切に維持されている)
- 検証:
  - `model/config_types.go:224-225`: `(Removed) CommitPolicyConfig — max_files / require_gitignore / message_pattern were policy gates predating the orchestrator's ...` (適切な removal note)
  - `model/config_load.go:157`: deprecated key 警告 (`worktree.commit_policy`) を operator に出力する deprecation message 配線
  - `model/config_defaults.go:121-122`: removed comment (`(Removed) Fallback defaults`)
  - `daemon/queue_scan_collect.go:138`: `(Removed) Fallback degraded-mode gate` コメント
  - `worktree/manager.go:463,1065`: コメント上で「no sensitive-file filter」「no message-pattern」を明示
- 注意: `expected_paths` 自体は **task-level の必須フィールド** として活用中 (`plan/validate.go:354-363`)。これは「commit gate」とは別概念で、撤去対象ではない。混同注意。
- 詳細: コードからは removed gate ロジックは綺麗に消えており、`(Removed) ...` の history note のみが残る (DRY 違反/dead code は無し)。

### 4-4: verify env guard (MAESTRO_ALLOW_VERIFY_SKIP) (`verify_disabled_is_normal_mode.md`)

- 結果: **問題なし**
- 検証: `grep -rn "MAESTRO_ALLOW_VERIFY_SKIP" --include="*.go" .` → 2 件すべてコメント
  - `daemon/dashboard_data.go:137`: history note (`existed to surface a deprecated emergency env opt-out`)
  - `daemon/dashboard_formatter_test.go:104`: regression test note (`retired MAESTRO_ALLOW_VERIFY_SKIP emergency env gate`)
- 詳細: `os.Getenv` 経由の参照はゼロ。実装上は完全撤去済み。

### 4-5: agent の hook 抑止 (`agent_hook_suppression_is_user_responsibility.md`)

- 結果: **問題なし** (regression test として残置済み、意図的)
- 検証: `internal/agent/launcher_test.go:856-878` に「`Stop:[]` or `Notification:[]` を `--settings` に **入れない**」regression test がある
- 詳細: 撤去後の再混入を防ぐ防壁テストとして妥当。残しておくべきテスト。

---

## 観点5: 使われていない config キー、env var

### 5-1: env vars

- 結果: **問題なし**
- 列挙 (production code, `os.Getenv` / `os.LookupEnv`):
  - `MAESTRO_DIR` (主軸)
  - `MAESTRO_AGENT_ID`
  - `MAESTRO_AGENT_TMPDIR_OVERRIDE`
  - `MAESTRO_LOCKORDER`
  - `MAESTRO_LOG_PANE_TAIL`
  - `MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC`
  - `CODEX_HOME`, `TERM`, `TMUX`, `TMUX_PANE`, `PATH`, `HOME`
  - test 専用: `MAESTRO_INTEGRATION`
- 撤去済み env var (`MAESTRO_ALLOW_VERIFY_SKIP`) は §4-4 の通り清算済み。

### 5-2: config キー

- 結果: **問題なし** (deprecated key は適切に warning を出す)
- 検証ロジックは `model/config_load.go:155-160` (`fallback`, `worktree.commit_policy` などの deprecated key を操作員にメッセージで知らせる) で集中管理されている。
- 注意: 観点 3 (D3-1) で言及した通り、`fallback` は config レイヤでは互換性を保つ設計が選択されており、これ自体は dead code ではない。

---

## 章別まとめ — すぐ削除 / 設計議論 / 要確認

### 即削除可能 (副作用範囲が局所、テストも同時削除可)

| ID | 対象 | 種別 |
|----|------|------|
| D1-1 | `internal/daemon/scheduler` パッケージ全体 + テスト | パッケージ |
| D1-3 | `internal/model/fingerprint.go` の `ConsecutiveFailureTracker` infra | 関数群 |
| D1-4 | `internal/model/featuregate.go` の `ValidateProfileLevel` / `DefaultFeatureProfiles` / `IsFeatureEnabled` | 関数群 |
| D1-5 | `internal/model/runtime.go: ValidateRuntime` | 関数 |
| D1-7 | `internal/daemon/lease_manager.go` の `LeaseManagerOption` / `LeaseInfo` / `WithLeaseManagerClock` | alias |
| D1-8 | `internal/daemon/dispatch_envelope.go` (placeholder file) | ファイル |
| D1-12 | `internal/model/review.go: IsTerminalReviewStatus` | 関数 |

### 削除可能だが範囲が広い・他箇所への影響あり

| ID | 対象 | 影響範囲 |
|----|------|---------|
| D1-2 | `internal/daemon/fallback` package + 結線インフラ全体 | `daemon.go`, `queue_handler.go`, `handler_registry.go`, `result_write_handler.go`, `api_factory.go`, `daemon_startup.go` の修正必要 |
| D1-6 | `DefaultFitnessThresholds` 関数 | `FitnessThresholds` 型自体は別箇所で使われている可能性、削除前に調査 |
| D1-11 | `IsActiveStatus`, `IsPausedStatus` (`model/status.go`) | 削除可能だが status ヘルパは future use を意識した API surface のため要設計議論 |

### 要設計議論

| ID | 対象 | 議論点 |
|----|------|-------|
| D1-9 | `model.LoadOrDefaultVerifyConfig` (no ForProject) | `LoadOrDefaultVerifyConfigForProject` への統合とテスト書き換え方針 |
| D1-10 | `DefaultVerifyConfigForProject` の dead parameter | パラメータ削除 or 関数自体の削除 |
| D2-1 | `NormalizeError` 二重実装 | 採用する正規化規則の選定 (regex 違いがあるため、実装 A の正規化を実装 B に持ち込むべきか) |
| D2-2 | Clock の triple-hop alias | callsite 50+ への波及検証 |
| D3-1 | `Fallback` config block の互換性運用 | YAML 後方互換性 (現在は適切な deprecation message があるため、別途議論) |

---

## 推奨リファクタリング順序 (参考)

1. **D1-8** (placeholder file 削除) — リスクなし、最初に対応
2. **D1-1** (`scheduler` package 削除) — 単一パッケージ完結で副作用なし
3. **D1-7** (lease alias 3 種削除) — 一行ずつ削除
4. **D1-12, D1-5** (model の dead 述語 / Validate 関数削除) — テストと一緒に
5. **D1-3 + D2-1** (model の `NormalizeError` infra と二重実装解消) — まとめて対応で DRY 解消
6. **D1-4** (model の featuregate dead API 削除) — `daemon/featuregate` への一本化
7. **D1-2** (fallback package と結線完全撤去) — 規模大、event-by-event review が必要
8. **D1-9, D1-10, D1-11, D2-2, D3-1** — 設計議論を経て対応

---

## 付記: 調査の限界 (researcher 規則準拠)

- **確認済み**: 各 finding に grep の検索コマンドと結果件数を添付
- **推定**: D1-6 の `DefaultFitnessThresholds` の `FitnessThresholds` 型側の使用については別途 grep が必要 (本レポートでは関数のみ確認)
- **不確実**: ビルドツールチェーン (linker, init() chain, reflection) 経由の暗黙的参照は `go build -gcflags="-m"` 等を実行しなければ完全には排除できない。本レポートは grep ベースの構文的参照件数のみを根拠としており、`init()` 経由の副作用や `reflect.TypeOf` 経由の動的参照を捕捉していない可能性がある (ただし maestro_v2 のコードベースでは reflection 利用は限定的なため、影響は小さいと推定)。
