# Maestro v2 コードレビュー最終レポート

**レビュー日**: 2026-03-10
**対象**: maestro_v2 リポジトリ全体（Go 1.26 製マルチエージェントオーケストレーションシステム）
**手法**: 4 並列調査 + 2 検証タスクによる段階的レビュー

---

## 1. エグゼクティブサマリー

### 全体評価

Maestro v2 は堅牢な設計原則に基づいて構築されている。3 フェーズスキャン + epoch fencing による並行制御、atomic YAML I/O、SHA-256 ベースの self-write tracking、ファイルロックによるプロセス排他など、分散システムの典型的な課題に対して適切な対策が実装されている。

**重大な脆弱性は検証により大部分が誤検知または緩和済みと判定された。** 初期調査で Critical 4 件・High 9 件が報告されたが、検証の結果、Critical は全件が誤検知/緩和済み、High は 2 件のみが実際の問題として残った。

### 主要リスク

| 重要度 | 確認済み件数 | 概要 |
|--------|-------------|------|
| Critical | 0 | 全件が検証で誤検知/緩和済みと判定 |
| High | 2 | パストラバーサル（ValidateFilePath）、single-writer バイパス（skill CLI） |
| Medium | 10 | Phase B 部分失敗、Phase reopen 無制限、busy detector 不整合 等 |
| Low | 9 | バージョン埋め込み、timestamp サイレント無視、キャッシュ eviction 等 |

---

## 2. Critical（重大問題）

**検証の結果、Critical レベルの問題は 0 件。全件が誤検知または緩和済み。**

### C1. evaluators.go ReDoS リスク — 誤検知
- **初期報告**: Script dangerous pattern の正規表現に catastrophic backtracking リスク
- **検証結果**: Go の `regexp` パッケージは RE2 ベースであり、線形時間保証がある。catastrophic backtracking は原理的に発生しない
- **判定**: **誤検知**

### C2. evaluators.go bash --restricted エスケープ — 誤検知（緩和済み）
- **初期報告**: bash --restricted は subshell/fd manipulation でエスケープ可能
- **検証結果**: `bash --restricted --noprofile --norc` で実行（L414）。環境変数は `PATH=/usr/bin:/bin` と `HOME=/tmp` のみのホワイトリスト（L420-423）。LD_PRELOAD 等は含まれない。スクリプト長制限（4KB）、危険パターン事前検証、tmpDir 実行も実装済み
- **判定**: **誤検知（緩和済み）**

### C3. retry.go State rollback 不完全 — 緩和済み
- **ファイル**: `internal/plan/retry.go:655-666`
- **初期報告**: YAML marshal/unmarshal 経由の手動 rollback でエラーパス漏れの可能性
- **検証結果**: 全エラーパス（8 箇所）で restoreState() が呼ばれており漏れなし。restoreState() の Unmarshal 失敗時は log.Printf のみだが、AddRetryTask 全体が sm.LockCommand で排他制御されており、失敗時は error 返却で呼び出し元が処理。Unmarshal 失敗はメモリ破損レベルの異常時のみ
- **判定**: **緩和済み**
- **推奨**: restoreState() の Unmarshal 失敗時に panic またはより明示的なエラー処理を検討（低優先度）

### C4. task_retry_handler.go DeepCopy 不完全 — 誤検知
- **ファイル**: `internal/daemon/task_retry_handler.go:99-141`
- **初期報告**: Constraints スライスが未クローンで元タスク汚染リスク
- **検証結果**: `withCappedRetryMeta()` （L260-282）が完全に新しいスライスを構築して返す（`out := make([]string, 0, ...)`）。元の Constraints スライスは一切変更されない
- **判定**: **誤検知**

---

## 3. High（重要問題）

### H1. ValidateFilePath にディレクトリトラバーサル防御なし — 確認（問題あり）
- **ファイル**: `internal/validate/path.go:127-138`
- **影響範囲**: `cmd_plan.go:63` → `internal/plan/submit.go:691` の `os.ReadFile()` に到達し、任意ファイル読み取りが可能
- **詳細**: null byte チェックと `filepath.Clean()` のみで `..` トラバーサルを検出しない。同ファイルの `SafePath()` （L55-125）は `filepath.Rel()` + symlink 検証を実装しているが、ValidateFilePath では使われていない
- **推奨対策**: ValidateFilePath の呼び出し元（`cmd_plan.go:63`, `submit.go:676`）を `SafePath()` に置き換えるか、ValidateFilePath 自体にベースディレクトリ制約チェックを追加する
- **優先度**: **高** — 外部入力が到達するパスのため

### H2. cmd_skill.go が single-writer アーキテクチャをバイパス — 一部確認
- **ファイル**: `cmd/maestro/cmd_skill.go:159,215,250,272`
- **影響範囲**: `state/skill_candidates.yaml` の直接読み書き。daemon の lockMap 排他制御をバイパス
- **詳細**: `runSkillApprove` と `runSkillReject` が state ファイルを直接操作。他の CLI コマンドは UDS 経由で daemon に委譲しているが、skill approve/reject のみ直接ファイル操作。atomic rename で書き込みするため破損リスクは低く、人手操作のため実質的な競合リスクも低い
- **推奨対策**: CLI skill approve/reject を daemon API 経由に変更するか、file-level advisory lock（flock）を追加して排他制御する
- **優先度**: **中高** — 設計上の一貫性の問題。実害リスクは低い

### 検証で誤検知/緩和済みと判定された High 項目

| ID | 初期報告 | 判定 | 理由 |
|----|---------|------|------|
| H5 | persona.go パストラバーサル | 誤検知 | `fs.ValidPath()` は `..` を含むパスに false を返す。`strings.HasPrefix(file, "persona/")` で二重チェック。テストでも検証済み |
| H6 | dispatcher.go missing_ref_policy="error" でタスクブロック | 設計意図通り | error ポリシーはタスクブロックが期待動作 |
| H7 | retry metadata capping 超過 | 誤検知 | `withCappedRetryMeta()` が正確に maxRetryMeta 件に制限（古い meta 最大 4 件 + 新規 1 件 = 5 件） |
| H8 | skill.go O(n²) truncation | 低リスク | スキル数が実用上少数のため性能問題は発生しない |
| H9 | quality_gate.go context.Background() | 緩和済み | テスト専用関数（L393 コメント "for testing"）。メイン実行パスは `qg.ctx` を使用 |

---

## 4. Medium（中程度の問題）

### M1. config.go Effective* パターンの 0 値曖昧性
- **ファイル**: `internal/model/config.go`
- **詳細**: `EffectiveMaxEntries` 等が 0 をデフォルト値（100）に置換。ユーザーが意図的に 0 を設定した場合と未設定の区別がつかない
- **影響**: 設定の意図しない上書き
- **推奨**: ポインタ型（`*int`）を使用して未設定と 0 設定を区別する

### M2. cmd_plan.go 一時ファイルのクリーンアップ不備
- **ファイル**: `cmd/maestro/cmd_plan.go`
- **詳細**: stdin を一時ファイルに書き出す際、defer で os.Remove しているが SIGKILL 時にゴミファイルが残る。定期的な tmp クリーンアップ機構なし
- **影響**: ディスク容量の漸増（長期運用時）
- **推奨**: daemon の起動時に `.maestro/tmp/` の古いファイルをクリーンアップする処理を追加

### M3. Phase B 部分失敗時のロールバック機構なし
- **ファイル**: `internal/daemon/queue_scan_phase_b.go`
- **詳細**: dispatch 成功後に signal 配信が失敗した場合、次スキャンで再試行されるが一時的不整合あり
- **影響**: タスクの一時的な停滞（次スキャンで回復）
- **推奨**: Phase B の操作をべき等にし、失敗時の明示的なリカバリパスを追加

### M4. Phase failed→active 遷移が回数無制限 — 確認（低リスク）
- **ファイル**: `internal/model/status.go:376-378`, `internal/plan/retry.go:499-509`
- **詳細**: `reopenPhase()` に回数チェックなし。retry 自体に MaxRetries 制限があるが、cascade recovery での reopen は別タスクの retry で発生するため理論上は制限を超えうる
- **影響**: 無限ループの理論的可能性
- **推奨**: `Phase.ReopenCount` フィールド追加と `reopenPhase` での上限チェック

### M5. busy_detector.go VerdictUndecided のリトライ整合性
- **ファイル**: `internal/agent/busy_detector.go`
- **詳細**: 即時 2 回リトライするがメインループでは未リトライ。動作不整合
- **影響**: busy 判定の信頼性低下
- **推奨**: リトライポリシーを統一

### M6. circuitbreaker.go LastProgressAt の time.Parse 失敗がサイレント
- **ファイル**: `internal/daemon/circuitbreaker/`
- **詳細**: パース失敗時にログなし。デバッグ困難
- **影響**: 運用時のトラブルシュート困難
- **推奨**: パース失敗時にログ出力を追加

### M7. standby.go キューファイル読取エラーで worker が消失
- **ファイル**: `internal/worker/standby.go`
- **詳細**: エラー発生時に worker がリストから消える。エラー状態の明示なし
- **影響**: worker の一時的な消失
- **推奨**: エラー状態を明示的にハンドリングし、リトライまたは報告する

### M8. executor.go /clear 確認の 500ms ディレイが Claude 動作前提に依存
- **ファイル**: `internal/agent/executor.go`
- **詳細**: タイミング仮定に基づく。Claude のレスポンス速度が変わると動作不安定
- **影響**: 環境依存の不安定動作
- **推奨**: ポーリングベースの確認に変更

### M9. cmd_queue.go target 未検証
- **ファイル**: `cmd/maestro/cmd_queue.go`
- **詳細**: `args[0]` に対する `ValidateID` 呼び出しがない。daemon 側で検証される可能性はあるが、CLI 側でも防御すべき
- **影響**: 不正入力が daemon に到達
- **推奨**: CLI 側で ValidateID を追加

### M10. engine.go singleflight キーにコロン区切り使用
- **ファイル**: `internal/quality/engine.go`
- **詳細**: gateType にコロンが含まれる場合にキー衝突リスク
- **影響**: 異なる gate の結果が混在する可能性
- **推奨**: 区切り文字にコロン以外（例: null byte や専用デリミタ）を使用

---

## 5. Low（軽微な問題）

| ID | ファイル | 詳細 | 推奨 |
|----|---------|------|------|
| L1 | `cmd/maestro/main.go` | version 埋め込みに ldflags 機構なし | ビルド時 ldflags で version を設定する機構を追加 |
| L2 | `cmd/maestro/cmd_*.go` | CLI エラーメッセージの usage 文字列がハードコード | テンプレート化またはフラグ自動生成を検討 |
| L3 | `cmd/maestro/cmd_plan.go` | runPlanRequestCancel が他の plan サブコマンドと異なるルーティング | コメントで理由を説明 |
| L4 | `internal/daemon/events/bus.go` | Close() と Subscribe() 間の TOCTOU ウィンドウ | コンテキストキャンセルで回復するため実害なし |
| L5 | `internal/daemon/queue_write_handler.go` | キューアーカイブ失敗時にサイズチェックが再試行されない | 次スキャンで回復するため実害なし |
| L6 | `internal/daemon/learnings/learnings.go` | malformed timestamp がログなしで無視 | warn ログ出力を追加 |
| L7 | `internal/daemon/skill/skill_candidate.go` | コンテンツ正規化が内部空白を考慮しない | 正規化ロジックを強化 |
| L8 | `internal/quality/cache.go` | lazy cleanup が 100 回の Set 操作ごと（期限切れエントリが長期残存） | TTL ベースの定期クリーンアップを追加 |
| L9 | `internal/model/state.go` | InProgressAt 未設定時の UpdatedAt フォールバック | ドキュメントで仕様を明記 |

---

## 6. 未使用コード・YAGNI 違反

| ファイル | 項目 | 詳細 |
|---------|------|------|
| `internal/quality/evaluators.go` | `CompiledScript` フィールド | 設定されるパスが存在しない。デッドコード |
| `internal/quality/types.go` | `ActionType: retry, rollback, escalate, prompt` | 定義のみで Engine 未実装 |
| `internal/quality/loader.go` | `getOrCompileRegex` キャッシュ | eviction なしで無制限増加。メモリリークの潜在的リスク |

これらは現在使用されていないが、将来の機能拡張のための予約である可能性がある。不要であれば削除を推奨する。

---

## 7. コード品質総評

### 命名規則
- **一貫性あり**。CLI 層は `runXxx` パターン、内部は PascalCase エクスポート。Go の慣習に準拠

### 構造
- **適切なレイヤー分離**: CLI（cmd/maestro/）→ ビジネスロジック（internal/）→ データモデル（internal/model/）
- **daemon の single-writer パターン**: キューと状態の一貫性を保証（H2 の skill CLI を除く）
- **3 フェーズスキャン**: 排他→非排他→排他の設計で I/O 影響を最小化しつつ整合性を保証

### エラーハンドリング
- **4 層構造**: CLIError + ValidationErrors + PlanValidationError + ActionRequiredError。適切に分離
- **ID バリデーション**: null byte チェック、パターンマッチ、パストラバーサル防御（SafePath + symlink 解決）
- **sanitizeForTerminal**: 制御文字除去が実装済み

### 設計パターン
- **epoch fencing**: 並行スキャンの整合性保証
- **atomic YAML I/O**: temp → fsync → 検証 → backup → rename → dir fsync
- **MutexMap**: 参照カウント付き keyed mutex、atomic CAS で二重 unlock 防止
- **ロック順序**: queue → state → result（lockorder ビルドタグで強制可能）

### 機能連携の注意点
- `WorkerAssign` と `Standby` で `resolveWorkerModel` が重複実装（乖離リスク）
- Skill injection + Persona injection + Learnings injection の順序が `envelope_builder` で固定（順序依存バグの余地）

---

## 8. セキュリティ総評

### 良好な点
- **RE2 正規表現**: Go の regexp パッケージにより ReDoS は原理的に不可能
- **bash --restricted 実行**: 環境変数ホワイトリスト、スクリプト長制限、危険パターン事前検証が多層防御を形成
- **パストラバーサル防御**: `SafePath()` は `filepath.Rel()` + symlink 検証を実装。`persona.go` は `fs.ValidPath()` + prefix チェックで二重防御
- **プロセス排他**: `syscall.Flock` による FileLock で単一デーモンインスタンスを保証
- **UDS 通信**: シェルインジェクションリスクが低い構造体デシリアライズ
- **sensitive file フィルタ**: worktree commit 時に `.env`, `*.key` 等を除外

### 要改善点
- **H1（ValidateFilePath）**: 外部入力が到達するパスでトラバーサル防御が不足。**最優先で修正すべき**
- **H2（skill CLI）**: single-writer バイパスは設計一貫性の問題
- **loader.go TOCTOU**: `filepath.Walk` 時のファイル置換レースは理論的リスク（実害は低い）

---

## 9. 推奨アクションリスト（優先順位付き）

| 優先度 | ID | アクション | 影響 | 工数目安 |
|--------|-----|----------|------|---------|
| **P1** | H1 | `ValidateFilePath` の呼び出し元を `SafePath()` に置き換え、またはベースディレクトリ制約チェックを追加 | セキュリティ | 小 |
| **P2** | H2 | skill approve/reject を daemon API 経由に変更、または flock 追加 | 設計一貫性 | 中 |
| **P3** | M4 | `Phase.ReopenCount` フィールド追加と上限チェック | 安定性 | 小 |
| **P3** | M3 | Phase B 操作のべき等性確保と明示的リカバリパス追加 | 信頼性 | 中 |
| **P4** | M1 | Effective* パターンのポインタ型化で 0 値と未設定を区別 | 正確性 | 中 |
| **P4** | M2 | daemon 起動時の `.maestro/tmp/` クリーンアップ追加 | 運用性 | 小 |
| **P4** | M5-M8 | busy detector/circuitbreaker/standby/executor の個別改善 | 安定性 | 各小 |
| **P5** | YAGNI | 未使用コード（CompiledScript, ActionType 等）の削除検討 | 保守性 | 小 |
| **P5** | L1-L9 | 各軽微改善 | 品質 | 各小 |
| **P5** | C3推奨 | restoreState() の Unmarshal 失敗時エラー処理強化 | 堅牢性 | 小 |
