# Maestro v2 システム要件定義書 (Requirements Definition)

## 1. 基本思想 (Core Philosophy)
本システムは、Git worktreeによる隔離、Daemonによる非同期制御、Planner-DAGによるタスク分割、tmux Workerという「Maestro v2」の既存の
強みを最大限に活かす。
「AI Scientist / 群知能」の実現を最終目標とするが、自律型システムが自重で崩壊する（機能過多・無限ループ・観測不能）のを防ぐため、
**「客観的評価に基づく淘汰（Fitness）」と「安全な停止・縮退能力（Circuit Breaker）」の構築を最優先事項とする。**

### 用語と強制度 (RFC 2119準拠)
本書では要件の強制度を以下の用語で定義する。
- **MUST**: 必須要件（システムの中核を成し、妥協不可）
- **SHOULD**: 強く推奨される要件（特段の理由がない限り実装すべき）
- **MAY**: 任意・将来拡張要件（基盤安定後に検討可能）

---

## 2. タスク状態遷移と最小スキーマ (State & Schema)

### 2.1. タスク状態遷移 (Task State Machine)
自律システムの確実な停止と再開を保証するため、全タスクは以下の状態遷移（State Machine）を **MUST** で実装する。

- 基本フロー: `planned -> ready -> dispatched -> running -> verify_pending`
- Verify 成功時: `verify_pending -> completed`
- Verify 失敗時: `verify_pending -> repair_pending`
- Repair 上限超過時: `repair_pending -> paused_for_replan` (Planner差し戻し)
- 人間介入要求時: `* -> paused_for_human`
- 中止条件成立時: `* -> aborted`

### 2.2. 最小データスキーマ (Minimum Schemas)
Plannerが生成するタスク定義および検証定義は、以下の構造を **MUST** で満たさなければならない。

**Task Schema 例:**
```yaml
task:
id: string
title: string
blocked_by: [task_id]
expected_paths: [path_prefix] # コンフリクト予測用
definition_of_done: [string]
definition_of_abort:          # 撤退条件 (MUST)
    max_repair_count: integer
    max_wall_clock_sec: integer
    explicit_failure_conditions: [string]

Verify Schema 例:

verify:
build: [command]
lint: [command]
test: [command]
typecheck: [command]
```
———

## 3. 実装フェーズ S: 制御・評価・観測基盤（最優先事項）

高度なLLM機能を載せる前に、システムが安全に失敗し、確実に停止・再開できる土台を構築する。

### Priority S0: 安全に止まれる最小制御面（システムの命綱）

- [S0-1] Admission Control（リソースと並行実行の管理）: Daemonは、Worker数だけでなく「同時Verify数」「同時Repair数」「同時Rollout
数」を厳格に管理する [MUST]。
- [S0-2] Single-worker Fallback（単一ワーカー縮退動作の保証）: マルチモデルや並列機能がエラーを起こした場合でも、システム全体を停止
させず Planner → 1 Worker → Verify → Repair の最小ループに縮退して動作継続可能なアーキテクチャとする [MUST]。

### Priority S1: 客観的評価と厳格なスキーマ（Fitnessの定義）

- [S1-1] verify.yaml の生成強制と Verification Runner: Plannerは Phase 0 で必ず .maestro/verify.yaml を生成する [MUST]。未定義の場
合は、Daemonが構文チェック等のFallback Verifyを自動補完・実行する [MUST]。
- [S1-2] 機械的な Fitness（適応度）関数の定義: 勝敗や成功判定をLLMに委ねない。評価はシステムが以下の辞書式順序で機械的に決定する
[MUST]。
    1. Pass / Fail (Failは即時不採用)
    2. Repair回数 (少ない方が勝者)
    3. Diffサイズ (変更ファイル数・行数が少なく、expected_pathsを逸脱していない方が勝者)
    4. 実行時間 (短い方が勝者)
    - ※上記すべてが同等の（差分が閾値未満の）場合のみ Tie（引き分け）とみなす。
- [S1-3] スキーマの厳格なバリデーション: Plannerが出力するYAMLは、後段のWorkerへ流す前にDaemon側で2.2のスキーマに従い厳格に
Validationし、不完全な場合は即座に差し戻す [MUST]。

### Priority S2: 停止可能性と自己治癒の限界設定（無限ループ防止）

- [S2-1] Failure Fingerprint（エラー指紋）による無限修復の防止: Verify/Repair時のエラーログを正規化・ハッシュ化し、同一指紋のエラー
が連続発生した場合は、即座に自動Repairループを停止し paused_for_replan へ遷移させる [MUST]。
- [S2-2] 多角的な Circuit Breaker（遮断器）の実装: Taskスキーマの definition_of_abort で定義された閾値（Repair深度、実時間）を超過した時点でタスクを強制終了させる [MUST]。

### Priority S3: 観測性とタスク定義の解像度向上

- [S3-1] definition_of_abort と expected_paths の必須化: 全タスクに対して撤退条件と想定変更パスの記述を義務付ける [MUST]。
- [S3-2] Trace JSONL による Event Taxonomy 記録: Task ID をトレースIDとし、計画→ディスパッチ→実行→検証→修復の全状態遷移ライフサイク
ルを、固定のイベント型を持つ Append-only な JSONL として記録・出力する [MUST]。

———

## 4. 実装フェーズ A〜B: 安全な知能化と群知能（Phase S完了後）

制御・評価基盤が完全に安定稼働した上で、LLMの自律的な拡張機能を段階的に解禁する。

### Priority A: 限定的マルチモデルと自己反省

- [A-1] 異種モデルの「レビュアー限定」非同期投入: Level 2 以上の重要タスクにおいて、CodexやGemini等の異種モデルをマージ前の「Read-
only レビュアー」としてアサインする [SHOULD]。
    - Reviewの非ブロッキング性: Reviewerの出力は Advisory (助言) とし、単独でマージを阻止してはならない [MUST]。指摘はPlanner経由で
    Repair Task候補へ変換される。
- [A-2] レビュアー有用性のロギング: 提案されたレビュー指摘が採用されバグを減らしたかを記録し、モデルごとの有用性データを蓄積する
[SHOULD]。
- [A-3] Plannerの自己診断（初期 Meta-Planner）: フェーズ完了時にPlannerへ「Repair多発箇所」「ブロックタスク」を要約させ、次フェーズ
の入力プロンプトに反省メモとして含める [SHOULD]。
- [A-4] Path-overlap Heuristic（簡易コンフリクト回避）: S3の expected_paths に基づき、「同じファイルを触るタスクは同時にディスパッ
チしない（または同一Workerに寄せる）」という簡易的なヒューリスティック制御を行う [MUST]。

### Priority B: 群知能の限定解禁（AI Scientistスウォーム）

- [B-1] 厳格な条件付き Multi-rollout: 以下の条件を すべて満たす場合のみ、同一タスクを複数の独立したWorktreeで並列実行(最大2並列から
開始)させる [MAY]。
    1. verify.yaml が定義され実行可能であること。
    2. 単一Worker実行またはRepairで一定回数失敗している、またはBloom Taxonomy高難度であること。
    3. expected_paths が過度に広すぎない（リポジトリ全体に及ばない）こと。
- [B-2] Judge（裁定者）の「Tie-breaker」限定利用: Multi-rolloutの勝者はS1-2の機械的Fitnessで決定する。最優秀LLM（Judge）の介入は、
「機械スコアが Tie (同点) の場合」に単一候補を採択する Tie-breaker としてのみ許可する [MUST]。
- [B-3] Graph-based Schedulingの高度化 (将来拡張): 簡易ヒューリスティック(A-4)の運用実績蓄積後、グラフ彩色やシンボル単位の競合予測
アルゴリズムの導入を検討する [MAY]。

## 4.5. 実装フェーズ C: 責務境界と Daemon 移譲（Phase A〜B の前提整理）

Phase A〜B の知能化機能を安全かつ効率的に実現するため、Agent（LLM）と Daemon（Go プロセス）の責務境界を明確化する。

### 4.5.1 責務境界原則

以下の原則に基づき、各機能の責務配置を決定する。

1. **決定論的処理の Daemon 移譲 [MUST]**: LLM 推論を必要としない決定論的処理（数値計算、パターンマッチ、閾値比較等）は Daemon が担う。Agent にトークンを消費させてはならない。
2. **Planner 責務の意味的判断への限定 [SHOULD]**: Planner の責務はタスク分解、品質評価の解釈、実装方針の選択等の意味的判断に限定する。構造的・数値的な前処理結果を Daemon から受け取り、判断に集中する。
3. **構造的メトリクスの Daemon 前処理 [SHOULD]**: ファイル数、依存関係グラフ、変更行数、Diff サイズ等の構造的・数値的メトリクスの計算は Daemon が前処理し、Agent には結果のみ提供する。

**根拠**: Daemon 移譲により以下のメリットが得られる。
- **トークン浪費回避**: 決定論的計算に LLM トークンを消費しない
- **レイテンシ改善**: Go プロセスによる μ 秒〜ms オーダーの処理（LLM 経由では秒〜数十秒）
- **一貫性保証**: 機械的判定により結果のブレを排除

### 4.5.2 C-1〜C-8 責務マトリクス

Phase A〜B で導入予定の各機能について、LLM 依存度と Daemon 移譲推奨度を以下に定義する。

| 機能 | 概要 | 判断主体 | 実行主体 | LLM 依存度 | Daemon 移譲推奨度 | 移譲先コンポーネント (SHOULD) | 移譲メリット |
|------|------|---------|---------|-----------|-----------------|---------------------------|------------|
| C-1 | 進化的コード品質改善 | Planner | Worker + Daemon | 高 | 中 | `quality/engine.go` 拡張 | Fitness 数値計算の LLM トークン節約 |
| C-2 | 適応的モデル選択（UCB バンディット） | Daemon | Daemon | なし | 極高 | `worker_assign.go` の `GetModelForBloomLevel()` 置換 | LLM トークン完全不要、μ 秒オーダーレイテンシ、グローバル統計による最適化 |
| C-3 | 自律的検証ループ | Planner | Worker + Daemon | 高 | 低〜中 | （リトライ閾値判定のみ） | リトライ判定レイテンシ改善 |
| C-4 | 探索的実装最適化 | Daemon（木管理）+ Planner（方針） | Worker + Daemon | 混合 | 高 | `daemon/worktree/` との統合 | 探索判断 ms 化（Planner 経由なら数秒〜数十秒）、探索木管理で Planner 消費ゼロ |
| C-5 | 自己改善（学習知見 DB） | Daemon（DB）+ Planner（最適化） | Daemon + Worker | 中 | 高（一部） | `daemon/learnings/` 拡張 | 失敗パターン即座マッチ |
| C-6 | 適応的計算深度 | Daemon（前処理）+ Planner（判断） | Daemon + Worker | 中 | 中〜高 | 新コンポーネント (SHOULD) | 構造指標プリコンピュート |
| C-7 | マルチランタイム対応 | Planner（選択）+ Daemon（起動） | Daemon | なし | 極高 | `formation/tmux_formation.go` + `agent/launcher.go` 拡張 | Daemon 既存責務（formation/）の自然な拡張、Worker 禁止操作（D006 tmux）との整合 |
| C-8 | Feature Gate（機能ゲート） | Planner（レベル判断）+ Daemon（適用） | Daemon | 低〜中 | 高 | `quality/engine.go` の `RuleEvaluator` 拡張 | プロファイル適用（許可機能フィルタリング）の一貫性保証、Planner 判断の簡素化 |

#### 責務境界の詳細

**C-2（適応的モデル選択）**: UCB1 式（`avg_reward + c * sqrt(ln(N) / n_i)`）は純粋な数学計算であり、LLM トークンを消費する Agent に任せてはならない。Daemon が MUST 計算する。グローバルな統計情報（各モデルの成功率・使用回数）へのアクセスも Daemon が一元管理する。

**C-7（マルチランタイム）**: ランタイム起動・tmux セッション管理は Daemon の既存責務（`formation/`）の延長であり、Agent が直接プロセス管理を行ってはならない。Worker の破壊的操作禁止ルール（D006: kill/tmux 操作禁止）とも整合する。Planner はランタイム種別の選択（意味的判断）のみを担う。

**C-8（Feature Gate）**: プロファイル適用（許可機能フィルタリング）は機械的判定であり、Daemon が MUST 実行する。Planner はタスクの複雑度レベルの意味的判断のみを担う。これにより、プロファイル適用の一貫性が保証され、Planner の判断負荷が軽減される。

### 4.5.3 Daemon 拡張方針

移譲推奨度「高」以上の機能について、Daemon 側の具体的な拡張ポイントを以下に定義する。

| 移譲推奨度 | 機能 | 拡張先 (SHOULD) | 拡張内容 |
|-----------|------|----------------|---------|
| 極高 | C-2 | `worker_assign.go` | `GetModelForBloomLevel()` を UCB1 ベースのアーム選択に置換。報酬テーブルを JSONL または SQLite で永続化 [SHOULD] |
| 極高 | C-7 | `formation/tmux_formation.go`, `agent/launcher.go` | ランタイム定義テーブル（Node/Python/Rust 等）の追加。Planner 指定のランタイム種別に基づく tmux セッション・Worker 起動の拡張 [SHOULD] |
| 高 | C-4 | `daemon/worktree/` | 探索木（Tree of Thought）の状態管理。分岐・剪定・選択の機械的操作を Daemon が実行し、Planner には方針設定 API のみ公開 [SHOULD] |
| 高 | C-5 | `daemon/learnings/` | 学習知見 DB の拡張。失敗パターンのハッシュインデックス追加による即座マッチ機能 [SHOULD] |
| 高 | C-8 | `quality/engine.go` | `RuleEvaluator` にプロファイルベースの機能ゲート判定を追加。Planner が設定した複雑度レベルに応じた許可機能リストの機械的フィルタリング [SHOULD] |

### 4.5.4 Planner 負荷軽減方針

Daemon への責務移譲により、Planner の負荷を以下の観点で軽減する [SHOULD]。

1. **構造的指標の Daemon 前処理**: ファイル数、依存関係グラフ、変更行数等の構造的メトリクスを Daemon が事前計算し、Planner には集約結果のみ提供する。Planner はこれらの数値を基に意味的判断（タスク分割粒度、優先度等）に集中できる。
2. **C-2 モデル選択の Daemon 完全移譲**: UCB バンディットによるモデル選択は Daemon が完全に自律実行する。Planner はモデル選択に関与せず、タスク設計に専念する。
3. **C-4 探索木操作の Daemon 移譲**: 探索木の分岐・剪定・状態管理は Daemon が担う。Planner は探索方針（深さ優先/幅優先、最大分岐数等）の設定のみを行う。
4. **C-8 プロファイル適用の Daemon 移譲**: Planner はタスクの複雑度レベル（Bloom Taxonomy 等）の判断のみを行い、そのレベルに応じた機能ゲートの適用は Daemon が実行する。

### 4.5.5 Anti-Requirements（§5）整合性

Phase C の各原則・機能が §5 の Anti-Requirements と矛盾しないことを以下に確認する。

| §5 項目 | 整合性確認 |
|---------|----------|
| 1. LLM による Fitness 判定の上書き禁止 | C-1 の Fitness 数値計算を Daemon 移譲することで、LLM が Fitness を上書きする余地をさらに排除。**整合** |
| 2. LLM 同士の多数決の採用禁止 | C-2 の UCB バンディットは統計的手法であり多数決ではない。判断は Daemon の数学計算に基づく。**整合** |
| 3. Frankenstein マージ禁止 | C-4 の探索的実装は独立 Worktree で実行し、勝者を丸ごと採用。合成は行わない。**整合** |
| 4. Worker 間の直接通信禁止 | C-7 のマルチランタイムも Daemon 経由の Hub-and-Spoke 型を維持。**整合** |
| 5. 共有可変状態の導入禁止 | C-5 の学習知見 DB は Daemon が一元管理し、Agent は ReadOnly で参照。**整合** |
| 6. Verify 不可タスクへの Multi-rollout 禁止 | C-4 の探索的実装は verify.yaml 必須を前提とする。**整合** |
| 7. 初期段階での Embedding RAG・Bandit 禁止 | C-2 の UCB バンディットは Phase C（Phase S 完了後）で導入するため、§5-7 の「最初から」の制約と矛盾しない。ただし Phase S の Trace ログ蓄積（S3）が前提条件となる [MUST]。**整合** |

———

## 5. やらないこと (Anti-Requirements / Out of Scope)

システムの崩壊（観測不能・決定不能・暴走）を防ぐため、以下の実装は明確に禁止する。

1. LLMによるFitness判定の上書き禁止 [MUST NOT]
    - LLMは build/lint/test/typecheck の結果を覆してはならない。補助的説明やTie-breakのみを行う。
2. LLM同士の多数決の採用禁止 [MUST NOT]
    - 賢くない多数派（Dumb Majority）に引っ張られるのを防ぐため、判断は常にコンパイラ・テストに委ねる。
3. Judgeによる複数候補の合成（Frankenstein マージ）禁止 [MUST NOT]
    - 複数モデルのコードをLLMに切り貼りさせる統合は論理破綻の温床となるため、常に「1つのWorktreeを丸ごと採用（Winner-takes-all）」
    とする。
4. Worker（Agent）間の直接通信禁止 [MUST NOT]
    - 隔離性と制御可能性を損なうため、常にDaemonを介した非同期通信（Hub-and-Spoke型）を維持する。
5. 共有可変状態（Typed Contract Blackboard等）の導入禁止 [MUST NOT]
    - 認識の同期は、Plannerによる「インターフェースの先行定義とReadOnly参照」で解決する。
6. Verify不可のタスクに対するMulti-rollout禁止 [MUST NOT]
    - 成功基準が曖昧な状態で群知能を回すことは、高コストな乱数発生器に過ぎないため発火させない。
7. 最初からの Embedding (ベクトル) RAG や Bandit アルゴリズムの実装禁止 [MUST NOT]
    - Traceログや古典的タグ検索での効果測定（S3）が回るまでは、高度な自己学習アルゴリズムの実装は見送る。

———

## 6. 受け入れ基準 (Acceptance Criteria)

以下の条件を満たした段階で、各フェーズの実装完了とみなす。

- Phase S 完了基準:
    - verify.yaml を持たないタスクが後段のWorkerへ絶対に流れないこと。
    - 同一の Failure Fingerprint が発生した場合、Repairループが停止しPlannerへ差し戻されること。
    - 1つのタスクの全ライフサイクル（生成〜修復完了/中断）が、1本のJSONLトレースログとして完全に追跡可能であること。
- Phase A 完了基準:
    - 異種モデル Reviewer の指摘事項がマージをブロックせず、かつその採用率がデータとして蓄積されていること。
- Phase B 完了基準:
    - 条件を満たしたMulti-rolloutタスクにおいて、LLMの主観によらず、定義されたFitness関数によって勝者が自動的に選定されること。
- Phase C 完了基準:
    - C-1〜C-8 の各機能について「判断主体」と「実行主体」が §4.5.2 のマトリクスに従い実装されていること。
    - Daemon 移譲推奨度「極高」の機能（C-2, C-7）が Daemon 内で完結し、Agent の LLM トークンを消費しないこと。
    - Daemon 移譲推奨度「高」の機能（C-4, C-5, C-8）の機械的処理部分が Daemon に移譲され、Planner は意味的判断のみを担っていること。
    - §4.5.5 の Anti-Requirements 整合性が維持されていること（特に §5-7 との整合：Phase S の Trace ログ蓄積が C-2 導入の前提条件として検証済みであること）。
