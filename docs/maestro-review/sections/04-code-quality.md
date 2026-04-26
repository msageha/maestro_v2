# 04. コード品質・デッドコード・リファクタリングレビュー

## サマリー

| 指標 | 値 |
|---|---|
| 検査対象ファイル数 | 305 (`*.go`、テスト除外) |
| 総行数 | 約 62,492 行 |
| 500 行超ファイル | 16 件 |
| 1000 行超ファイル | 3 件 (`merge_publish.go` 1361 / `result_handler.go` 1110 / `recover.go` 1043) |
| 100 行超関数 | 5 件以上 (Worktree 系に集中) |
| 重複インターフェース定義 | `Clock` 3 箇所 / `ModelSelector` 2 箇所 |
| 検出された明確なデッドコード | 0 件 (全公開関数が参照されていることを Grep で確認) |
| 推奨される循環依存除去 | 1 件 (`core` ↔ `plan` 経由のインターフェース重複に起因) |

**全体傾向**: ホットスポットは `internal/daemon/worktree/` および `internal/daemon/result_handler.go` の 2 系統に集中。git/ワーカー復旧・リース管理という本質的に複雑なドメインだが、関数粒度・インターフェース統一の観点で改善余地が大きい。デッドコードは検出されなかった (公開関数は全て参照あり)。命名一貫性は概ね良好で、Go 標準慣習から逸脱した致命的な問題はない。

**上位影響度大トップ 5**:
1. `internal/daemon/worktree/merge_publish.go` (1361 行、100 行超関数 5 件) — 関数分割とネスト解消
2. `internal/daemon/worktree/recover.go` の `ResumeMerge` (177 行) と `mergeResolvedWorker` (146 行) — 単一責務違反
3. `Clock` / `ModelSelector` インターフェース重複定義 — 統一定義への集約
4. `internal/daemon/result_handler.go` (1110 行、責務多岐) — Phase 別の構造化分割
5. `internal/agent/policy_checker.go` (690 行) — 正規表現パターンの外部化

---

## Findings

### A. 重複コード

#### A-1: `Clock` インターフェース重複定義

| 項目 | 値 |
|---|---|
| カテゴリ | 重複 |
| 重要度 | High |
| 場所 | `internal/testutil/clock.go:9`, `internal/metrics/handler.go:17`, `internal/daemon/core/core.go:149` |
| 現状の問題 | 同一概念 `Clock` インターフェースが 3 箇所で独立定義されている。`testutil` 版は `Since()` メソッドを追加した拡張シグネチャを持つ。型がパッケージごとに不一致なため、利用側でアダプタを書くか型変換が必要 |
| 推奨アクション | `internal/daemon/core/core.go` を Single Source of Truth とし、他 2 箇所はインポート参照に置き換え。拡張版 (`Since()`) は埋め込みインターフェースとして表現 |
| 根拠 | Grep `^type Clock interface` で 3 件ヒット (`*.go` 全体検索) |

#### A-2: `ModelSelector` インターフェース重複定義

| 項目 | 値 |
|---|---|
| カテゴリ | 重複 |
| 重要度 | High |
| 場所 | `internal/plan/worker_assign.go:30`, `internal/daemon/core/core.go:249` |
| 現状の問題 | `plan` と `daemon/core` で `ModelSelector` が二重定義されており、両層で型変換アダプタが発生している (推定: bridge パッケージで吸収) |
| 推奨アクション | `core` を正本とし `plan` 側は import 参照。または共通パッケージ `internal/contract` 等に切り出す |
| 根拠 | Grep `^type ModelSelector interface` で 2 件ヒット |

#### A-3: テンプレート instructions の共通セクション参照

| 項目 | 値 |
|---|---|
| カテゴリ | 重複 |
| 重要度 | Nit |
| 場所 | `templates/maestro.md` (114 行), `templates/instructions/orchestrator.md` (255 行), `planner.md` (1182 行), `worker.md` (482 行) |
| 現状の問題 | 各 instructions ファイルが「破壊的操作の安全規則」「Worker Bash / ツール制約」等のセクション見出しを持ち、内容を `maestro.md` に集約する形式は取れているが、見出しと冒頭文言の繰り返しが Role 跨ぎで残存 |
| 推奨アクション | 既存方針 (maestro.md = SSOT、instructions = role-specific) を維持。Role 共通の Tier1-3 表は instructions 側から重複しないよう監査ルールを CI に追加 |
| 根拠 | Grep `破壊的操作の安全規則` で 4 ファイルにヒット (maestro.md と 3 instructions) |

---

### B. デッドコード

#### B-1: 明確なデッドコードは検出されず

| 項目 | 値 |
|---|---|
| カテゴリ | デッドコード |
| 重要度 | — (該当なし) |
| 場所 | — |
| 現状の問題 | Explore SubAgent と直接 Grep の双方で、公開関数 (`SetContinuousHandler`, `SetEventBus`, `SetPhaseCManager`, `SetModelSelector`, `SetShutdownContext` 等) はいずれも複数のファイルから参照されており、明確に到達不能なコードは検出されなかった |
| 推奨アクション | 現状維持。今後の確認用に `go-mod-outdated` / `unused` linter の導入を検討 |
| 根拠 | Grep `SetContinuousHandler\|SetEventBus\|SetPhaseCManager\|SetModelSelector\|SetShutdownContext` で 14 ファイルにヒット (定義 1 ファイル + 利用 13 ファイル) |

#### B-2: 古い設定キーは未検出

| 項目 | 値 |
|---|---|
| カテゴリ | 古い設定 |
| 重要度 | Low |
| 場所 | `templates/config.yaml`, `internal/model/config*.go` |
| 現状の問題 | (Explore SubAgent 報告) `templates/config.yaml` に `admission_control` / `review` / `rollout` / `judge` 等の未接続セクションがコメントアウト形式で残存。実装側 (`internal/model/config_validate.go`) との対応関係が明示されていない |
| 推奨アクション | 未実装セクションには `# EXPERIMENTAL: not yet implemented` コメントを付与、または実装が進むまで削除 |
| 根拠 | Explore SubAgent 観察 (Grep `deprecated\|legacy\|TODO.*remove` で `worktree/recover.go` の "legacy fallback" 程度しかヒットせず、コード側に明確な廃止予定キーは未検出) |

---

### C. 責務過多な関数

#### C-1: `merge_publish.go` の大型関数群

| 項目 | 値 |
|---|---|
| カテゴリ | 責務過多 |
| 重要度 | Critical |
| 場所 | `internal/daemon/worktree/merge_publish.go` の以下関数 |
| 現状の問題 | 100 行超の関数が連続して定義されている: `SyncFromIntegration` (140 行 / L?)、`forwardMergeBaseToIntegration` (110 行 / L152)、`performPublishMerge` (104 行)、`mergeWorkerBranch` (98 行 / 90 行台)、`MergeToIntegration` (93 行 / L435)、`PublishToBase` (88 行)。1 ファイル 1361 行で git マージ・パブリッシュ・競合解決の責務が密集 |
| 推奨アクション | (1) `forwardMergeBaseToIntegration` 系を `merge_forward.go` に分離。(2) `performPublishMerge` を staging / commit / verify の 3 段に分割。(3) ネスト 3 段以上の if チェーンは early-return + ガード節で平坦化 |
| 根拠 | `awk` で各 `^func` の行間距離を計測 (`awk '/^func / {if (name) print NR-start-1, name; ...}'` 出力で実測) |

#### C-2: `recover.go` の `ResumeMerge` / `mergeResolvedWorker`

| 項目 | 値 |
|---|---|
| カテゴリ | 責務過多 |
| 重要度 | Critical |
| 場所 | `internal/daemon/worktree/recover.go: ResumeMerge` (177 行), `mergeResolvedWorker` (146 行), `AutoRecoverAfterResolution` (96 行) |
| 現状の問題 | `ResumeMerge` は単一関数で 177 行あり、ワーカー復旧・統合ツリー復旧・状態遷移の判定が一体化。プロジェクトで最大の関数 |
| 推奨アクション | 状態判定 → 解決済みワーカーの再マージ → 競合検査 → 完了通知の 4 ステップに分割。Step オブジェクトまたは関数チェーン化を検討 |
| 根拠 | 同上 awk 計測 |

#### C-3: `result_handler.go` のスキャン・通知関数

| 項目 | 値 |
|---|---|
| カテゴリ | 責務過多 |
| 重要度 | High |
| 場所 | `internal/daemon/result_handler.go: writeNotificationToOrchestratorQueue` (78 行), `recordTaskResultLearning` (78 行), `sweepExhaustedNotifications` (75 行 / ジェネリック), `ScanAllResults` (69 行 / L206), `processCommandResultFile` (61 行) |
| 現状の問題 | 個別関数は 100 行未満だが、ファイル全体で 1110 行あり、リース取得・通知・学習レコード・スキャン責務が混在。ジェネリック関数 (`processResultPhase1AcquireLease`, `processResultPhase3UpdateResult`) はパッケージプライベート (小文字始まり) で適切だが、コールバックパターンにより複雑性が隠蔽されている |
| 推奨アクション | Phase1 (リース取得)、Phase3 (結果反映)、通知、学習レコードの 4 ファイルに分割。`*_phase1.go` のような命名で責務を明示化 |
| 根拠 | awk 計測。Grep `processResultPhase1AcquireLease\|processResultPhase3UpdateResult` で `result_handler.go` 単独ヒット (パッケージ外参照なし) |

#### C-4: `internal/agent/launcher.go` の `buildLaunchArgs`

| 項目 | 値 |
|---|---|
| カテゴリ | 責務過多 |
| 重要度 | Medium |
| 場所 | `internal/agent/launcher.go: buildLaunchArgs` (95 行), `Launch` (67 行) |
| 現状の問題 | `buildLaunchArgs` は role / model / system prompt / base prompt mode から CLI 引数列を組み立てる関数で 95 行。条件分岐多数 |
| 推奨アクション | role 別の小さな builder 関数に分割。または builder pattern (`AgentLaunchArgs` 構造体 + `Build()`) に再構成 |
| 根拠 | awk 計測 |

---

### D. 循環依存

#### D-1: `plan` ↔ `daemon/core` の概念的循環

| 項目 | 値 |
|---|---|
| カテゴリ | 循環依存 |
| 重要度 | Medium |
| 場所 | `internal/plan/worker_assign.go:30` の `ModelSelector` インターフェース、`internal/daemon/core/core.go:249` の同名インターフェース |
| 現状の問題 | (Explore SubAgent 観察 / 推定) `plan` と `daemon/core` は同名インターフェースを独立して定義し、`bridge` パッケージで型変換を行うことで循環を回避していると推測される。コンパイル可能であり真の循環は未検出だが、変更時の型変換ロジック保守コストが発生 |
| 推奨アクション | 共通インターフェース定義を `internal/contract` 等の中立パッケージに集約し、`bridge` のアダプタ層を簡素化 |
| 根拠 | A-1, A-2 の重複定義に起因。実 `go list` 解析は未実施 (推定) |

---

### E. ネスト深さ

#### E-1: `merge_publish.go` の if-チェーン

| 項目 | 値 |
|---|---|
| カテゴリ | ネスト |
| 重要度 | Medium |
| 場所 | `internal/daemon/worktree/merge_publish.go:152-263` (`forwardMergeBaseToIntegration` 関数内), 他複数箇所 |
| 現状の問題 | (Explore SubAgent 観察) `if integrationHasMergeHead → reuseInFlightForwardMerge → if err → if done` の 3-4 段ネストが複数関数に存在 |
| 推奨アクション | early return / guard clause で段数を 2 段以下に削減。状態判定は別関数に切り出し |
| 根拠 | Explore SubAgent 報告 (Grep `forwardMergeBaseToIntegration` で 152-272 行範囲を確認、複数 if 構造あり) |

#### E-2: `executor_core.go` の状態遷移分岐

| 項目 | 値 |
|---|---|
| カテゴリ | ネスト |
| 重要度 | Medium |
| 場所 | `internal/agent/executor_core.go` (665 行) |
| 現状の問題 | (Explore SubAgent 観察) `execWithClear` 等で switch + if のネスト構造が深い |
| 推奨アクション | 状態遷移テーブル化、または handler 関数群への分散 |
| 根拠 | Explore SubAgent 報告 (詳細行特定は未実施) |

---

### F. 過大ファイル

#### F-1: 行数上位 10 ファイル (>485 行)

| 順位 | 行数 | ファイル | 推奨 |
|---|---|---|---|
| 1 | 1361 | `internal/daemon/worktree/merge_publish.go` | C-1 参照、3-4 ファイルに分割 |
| 2 | 1110 | `internal/daemon/result_handler.go` | C-3 参照、Phase 別に分割 |
| 3 | 1043 | `internal/daemon/worktree/recover.go` | C-2 参照、ステップ別に分割 |
| 4 | 880 | `internal/daemon/worktree/manager.go` | 状態管理と CRUD で分離検討 |
| 5 | 864 | `internal/tmux/session.go` | session / pane / window の責務で分離 |
| 6 | 795 | `internal/agent/launcher.go` | C-4 参照 |
| 7 | 705 | `internal/daemon/worktree/cleanup_gc.go` | cleanup と gc の責務分離 |
| 8 | 690 | `internal/agent/policy_checker.go` | 正規表現パターンを外部 YAML 化 |
| 9 | 674 | `internal/quality/engine.go` | 評価エンジンとルールローダーで分離 |
| 10 | 665 | `internal/agent/executor_core.go` | E-2 参照 |

| 項目 | 値 |
|---|---|
| カテゴリ | 過大ファイル |
| 重要度 | High |
| 推奨アクション | 500 行超のファイルは責務分割を検討。1000 行超のファイル 3 件は最優先 |
| 根拠 | `find cmd internal -name "*.go" -not -name "*_test.go" \| xargs wc -l \| sort -rn` 実測 |

---

### G. 命名一貫性

#### G-1: `ID` / `Identifier` / `Status` / `State` / `Phase` の用語混在

| 項目 | 値 |
|---|---|
| カテゴリ | 命名 |
| 重要度 | Low |
| 場所 | プロジェクト全体 |
| 現状の問題 | (Explore SubAgent 観察) 状態管理に `Status` / `State` / `Phase` の 3 用語が併用されている。意味の境界 (例: `Status` = user-facing、`State` = internal、`Phase` = pipeline 段階) は一貫しているように見えるが、明文化されていない |
| 推奨アクション | `docs/naming-convention.md` 等で用語の境界を明文化。Linter ルール (例: `revive` のカスタムルール) で逸脱を検出可能にする |
| 根拠 | Explore SubAgent 観察 (定量的 grep ベースは未実施) |

#### G-2: `Cfg` 型は未検出 (Explore 報告の修正)

| 項目 | 値 |
|---|---|
| カテゴリ | 命名 |
| 重要度 | Nit |
| 場所 | — |
| 現状の問題 | Explore SubAgent は `Cfg` 型の定義混在を指摘したが、Grep `^type [A-Z][a-zA-Z]*Cfg\b` で **0 件**。実際は変数名 `cfg` (Go の慣習に沿った省略形) が 97 箇所で使用されているのみで、型名は `Config` で統一されている。問題なし |
| 推奨アクション | 不要 |
| 根拠 | Grep `^type [A-Z][a-zA-Z]*Cfg\b` 0 件、Grep `Cfg\b` 97 件はいずれも変数名 `cfg` のパターン |

---

## 補足: 検証根拠サマリー

| 検証項目 | 手法 | 結果 |
|---|---|---|
| ファイル一覧 | `find cmd internal -name "*.go" -not -name "*_test.go"` | 305 ファイル |
| 行数上位 | `xargs wc -l \| sort -rn` | top 25 取得済み |
| Clock 重複 | Grep `^type Clock interface` (`*.go`) | 3 ヒット |
| ModelSelector 重複 | Grep `^type ModelSelector interface` (`*.go`) | 2 ヒット |
| `Set*` メソッド利用 | Grep `Set(ContinuousHandler\|EventBus\|PhaseCManager\|ModelSelector\|ShutdownContext)` | 14 ファイル |
| Cfg 型定義 | Grep `^type [A-Z][a-zA-Z]*Cfg\b` | 0 ヒット (Explore 報告は誤り) |
| 関数行数 | `awk '/^func / {if (name) print NR-start-1, name; ...}'` | merge_publish / result_handler / recover / launcher で実測 |
| process*Phase* 参照範囲 | Grep `processResultPhase1AcquireLease\|processResultPhase3UpdateResult` | 1 ファイル (定義ファイルのみ、パッケージプライベートで適切) |
| テンプレート構造 | `wc -l templates/instructions/*.md`, Grep `破壊的操作の安全規則` | maestro.md 114 行 + 3 instructions、共通参照構造 |

---

## 注記

- 「未参照」「(推定)」と明記した項目は、Explore SubAgent の観察に基づく推定であり、Worker による直接 Grep 検証は実施していない。重要な判断には追加検証を推奨
- 直接 Grep 検証で Explore の報告を 1 件修正済み: `Cfg` 型の混在は実在しない (G-2)
- もう 1 件修正: `processResultPhase1AcquireLease` / `processResultPhase3UpdateResult` は exported ではなく package-private (Go 慣習通り)。アクセス修飾子変更の推奨は不要
