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

### 4.5. 実装フェーズ C: Agent—Daemon 責務境界の確立と適応的制御（Phase B完了後）

Phase A–B で確立した群知能基盤の上に、決定論的制御と LLM 推論の責務境界を明確にし、適応的な制御機構を段階的に導入する。

#### 4.5.1. 責務境界原則 (Responsibility Boundary Principles)

Phase C の全機能設計において、以下の責務分割原則を適用する。

1. **決定論的処理の Daemon 集約**: LLM 推論を必要としない決定論的処理（数値計算・統計処理・状態管理・プロセス管理）は Daemon が担う [MUST]。Agent に機械的計算を委ねてはならない。
2. **Agent 責務の意味的判断限定**: Planner の責務は意味的判断（タスク分解・品質評価の解釈・設計方針策定）に限定する [SHOULD]。構造的・数値的メトリクスの計算を Planner に委ねてはならない。
3. **Daemon 前処理—Agent 判断パターン**: 構造的・数値的メトリクスは Daemon が前処理し、Agent には計算済みの結果のみを提供する [SHOULD]。Agent は提供された指標に基づき意味的判断を行う。

#### 4.5.2. Phase C 機能一覧と責務割当 (Feature Responsibility Matrix)

各機能について「判断主体」（何をすべきか決める者）と「実行主体」（実際に処理を行う者）を明記する。

| ID | 機能 | 判断主体 | 実行主体 | Daemon移譲度 | 根拠 |
|----|------|---------|---------|-------------|------|
| C-1 | 進化的コード品質改善 | Planner（変異方針策定） | Worker（変異生成）+ Daemon（Fitness 数値計算） | 中 | 変異生成は LLM 必須。品質軸の数値計算は S1-2 Fitness 関数の拡張として Daemon が担う |
| C-2 | 適応的モデル選択 | Daemon（UCB バンディット計算） | Daemon（モデル割当） | 極高 | UCB1 計算（avg_reward + c×√(ln(N)/n_i)）は純粋数学。S3 Trace データに基づく自動決定 [MUST] |
| C-3 | 自律的検証ループ | Planner（検証戦略設計） | Worker（検証実行）+ Daemon（リトライ閾値判定・投票集計） | 低〜中 | 検証自体は LLM 必須。debug_prob 閾値判定と Area Chair 投票集計のみ Daemon |
| C-4 | 探索的実装最適化 | Planner（探索方針設定: 幅 vs 深さ優先度） | Daemon（MCTS 木管理・UCT 計算・枝刈り） | 高 | 探索木操作は全て決定論的。worktree 管理と自然統合 [MUST] |
| C-5 | 自己改善メカニズム | Planner（プロンプト最適化方針） | Daemon（Fingerprint DB 管理）+ Worker（プロンプト適用） | 高（一部） | S2-1 Fingerprint の拡張。DB 管理は Daemon、プロンプト最適化は LLM 依存 |
| C-6 | 適応的計算深度制御 | Planner（意味的複雑度判断） | Daemon（構造的指標前処理） | 中〜高 | 構造的指標（ファイル数・依存関係・変更行数）は Daemon が前処理 [SHOULD] |
| C-7 | マルチランタイム管理 | Planner（タスクスキーマで runtime/model 指定） | Daemon（プロセス起動・管理・ヘルスチェック） | 極高 | プロセス管理は Daemon の既存責務の延長 [MUST] |
| C-8 | Feature Gate（プロファイル適用） | Planner（複雑度レベルの意味的判断） | Daemon（プロファイル適用・構造的指標計算） | 高 | quality gate 基盤の RuleEvaluator 拡張 [MUST] |

#### 4.5.3. Daemon 拡張方針 (Daemon Extension Points)

Phase C 機能の Daemon 側実装は、既存アーキテクチャの拡張ポイントを活用する [SHOULD]。新規パッケージの追加は最小限とし、既存のモジュール境界を尊重する。

| 機能 | 拡張対象コンポーネント | 方針 |
|------|----------------------|------|
| C-2 | `worker_assign.go` の `GetModelForBloomLevel()` | 静的 bloom_level マッピングを UCB ベース動的選択に置換。S3 Trace データを報酬信号として入力 |
| C-4 | `daemon/worktree/` + 新パッケージ `search/` | 探索木管理を worktree 管理と統合。UCT 計算エンジンを `search/` に配置 |
| C-5 | `daemon/learnings/` | Fingerprint DB を拡張し、失敗パターンの構造化蓄積・検索を実現 |
| C-7 | `formation/tmux_formation.go` + `agent/launcher.go` | Role 別ツール制限を維持しつつ、複数ランタイム（claude/codex/gemini 等）の起動を統一管理 |
| C-8 | `quality/engine.go` の `RuleEvaluator` インターフェース | プロファイルベースの条件評価器を追加。既存 4 ゲートタイプ（pre_task, post_task, phase_transition, command_validation）を活用 |

#### 4.5.4. Planner 負荷軽減方針 (Planner Load Reduction)

Phase A–B の Planner 7 責務（verify.yaml 生成、タスク定義生成、definition_of_abort/expected_paths 定義、DAG 設計、Repair Task 変換、自己診断、インターフェース先行定義）に加え Phase C で追加される判断負荷を、Daemon 前処理により軽減する [SHOULD]。

| Phase C 機能 | 軽減前（Planner 責務） | 軽減後（Daemon 前処理 → Planner 意味的判断のみ） |
|---|---|---|
| C-2 モデル選択 | Planner がモデル適性を判断 | Daemon が UCB 計算で自動決定。Planner はモデル選択に関与しない |
| C-4 探索戦略 | Planner が探索木全体を管理 | Daemon が木操作・UCT 計算を実行。Planner は方針（幅 vs 深さ優先度）設定のみ |
| C-6 計算深度 | Planner が全指標を自力で評価 | Daemon が構造的指標（ファイル数・依存関係・変更行数）を前処理し、Planner は意味的複雑度のみ判断 |
| C-8 プロファイル | Planner が複雑度評価 + プロファイル選択 | Daemon が構造的指標を計算しプロファイル候補を提示。Planner は意味的判断で最終選択 |

#### 4.5.5. Anti-Requirements §5 との整合性 (Consistency with Anti-Requirements)

Phase C の各機能は §5 の禁止事項に抵触しないことを以下の通り確認する。

- **§5-7（Bandit アルゴリズム禁止）との整合**: C-2 の UCB バンディット計算は「S3 Trace ログによる効果測定基盤が稼働済み」であることを前提条件とする [MUST]。Phase C は Phase S 完了後に位置するため、§5-7 の「最初からの実装禁止」条件を満たす。
- **§5-1（LLM による Fitness 判定上書き禁止）との整合**: C-4 の探索木管理（MCTS/UCT）は Daemon 内の決定論的制御であり、探索結果の評価は S1-2 の機械的 Fitness で行う [MUST]。LLM が Fitness を上書きする経路は存在しない。
- **§5-4（Worker 間直接通信禁止）との整合**: C-7 のマルチランタイムは Hub-and-Spoke 型を維持する [MUST]。異種ランタイム間の通信も Daemon 経由のみ許可し、Worker 間の直接通信経路は設けない。
- **§5-3（Frankenstein マージ禁止）との整合**: C-4 の探索的実装は Multi-rollout と同様に Winner-takes-all 方式を採用する [MUST]。複数探索結果の部分的合成は行わない。

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
    - §4.5.1 の責務境界原則（決定論的処理の Daemon 集約・Agent 責務の意味的判断限定・Daemon 前処理パターン）が、Phase C 全機能（C-1〜C-8）の設計・実装において遵守されていること。
    - C-2（適応的モデル選択）において、モデル割当が Daemon の UCB バンディット計算により自動決定され、Planner/Worker がモデル選択判断に関与しないこと。
    - C-4（探索的実装最適化）において、探索木操作（MCTS/UCT 計算・枝刈り）が Daemon 内で完結し、探索結果の評価が S1-2 の機械的 Fitness に基づくこと。
    - C-7（マルチランタイム管理）において、異種ランタイムのプロセス起動・管理が Daemon に集約され、Hub-and-Spoke 型通信が維持されていること。
    - C-8（Feature Gate）において、構造的指標の計算が Daemon 前処理として実行され、Planner には計算済み結果のみが提供されていること。
    - Phase C の全機能が §5 Anti-Requirements の禁止事項に抵触していないこと（特に §5-7 の Bandit 前提条件、§5-1 の Fitness 上書き禁止、§5-4 の直接通信禁止）。
