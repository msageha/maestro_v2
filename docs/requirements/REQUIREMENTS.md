# Maestro v2 システム要件定義書 (Requirements Definition)

## 1. 基本思想 (Core Philosophy)

本システムは、Git worktreeによる隔離、Daemonによる非同期制御、Planner-DAGによるタスク分割、tmux Workerという「Maestro v2」の既存の強みを最大限に活かす。
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

> **状態モデルの 2 層構造（実装の正規形）**: 上記のライフサイクル状態は `state/commands/{id}.yaml` の `task_states`（タスクの意味的ライフサイクル）に適用される拡張状態であり、`internal/model/status.go` の `validTaskStateTransitions` で管理する。一方 `queue/worker{N}.yaml` の各エントリの `status` は配信制御用の 6 状態（`pending ⇄ in_progress → completed | failed | cancelled`、`pending → dead_letter`）であり、[§4.10](04-yaml-schema.md) のステータス状態遷移に従う。2 層は別レイヤーであり、queue は「配信の進行」、task_states は「タスクの意味的進捗」を表す。`verify_pending` / `repair_pending` / `paused_for_replan` / `paused_for_human` / `aborted` は task_states レイヤーにのみ存在する。

### 2.2. 最小データスキーマ (Minimum Schemas)

Plannerが生成するタスク定義および検証定義は、以下の構造を **MUST** で満たさなければならない。

**Task Schema 例:**

```yaml
task:
id: string
purpose: string
blocked_by: [task_id]
expected_paths: [path_prefix] # コンフリクト予測用
definition_of_done: [string]
definition_of_abort: # 撤退条件 (MUST)
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

- [S0-1] Admission Control（リソースと並行実行の管理）: Daemonは、Worker数だけでなく「同時Verify数」「同時Repair数」を厳格に管理する [MUST]。
- [S0-2] 最小ループへの縮退保証（per-task 縮退）: マルチモデルや並列機能がエラーを起こした場合でも、システム全体を停止させず Planner → Worker → Verify → Repair の最小ループで動作継続可能なアーキテクチャとする [MUST]。縮退は **per-task の retry / repair（add-retry-task）/ dead-letter / Circuit Breaker** で実現する。
  - **Worker をブラックリスト化する degraded mode は採用しない**: 連続失敗した Worker の dispatch を抑止する fallback manager は、同一 Worker が `healthy` に復帰できないデッドロック（2026-05-03 e2e で 17 分超の stall・回復ゼロ）を生むため、設計として起動しない（`internal/daemon/daemon_startup.go` で明示的に未起動。`Fallback` config 構造体は YAML 後方互換のため残置するが効果を持たない）。自律オーケストレーションの縮退は worker 単位の沈黙化ではなく per-task の回復機構で行う。

### Priority S1: 客観的評価と厳格なスキーマ（Fitnessの定義）

- [S1-1] command-scoped verify config の生成強制（submit ゲートは fail-closed）と Verification Runner: Planner は `plan submit` の前に対象 command_id に紐づく verify config snapshot を `maestro verify write` で登録する [MUST]。Daemon は submit 時に `validateRequiredVerifySnapshot`（`internal/plan/submit.go`）で snapshot の存在・妥当性（最低 1 コマンドを含むこと）を検証し、未登録・不正な場合は submit を拒否する（fail-closed）[MUST]。この検証は `verify.enabled` が有効なときのみ発火し、`verify.enabled: false`（検証スキップ）は正常運用モードであり別途の許可ガードは設けない。
  - **submit ゲートでの自動補完はしない**: snapshot 未登録を「Daemon が構文チェック等を補完して埋める」ことで submit 要件を回避する方式は不採用。実行不能・空の検証で誤った合格判定が出るのを防ぐため、検証定義は Planner による明示登録を必須とする。
  - **verify runner 自体は project-default フォールバックを保持する**: 実際に検証を実行する `RealVerifyRunner.loadVerifyConfig`（`internal/daemon/verify_runner_real.go`）は、per-command snapshot が正当に存在しない場合（`fs.ErrNotExist`）にプロジェクト言語判定ベースの `model.DefaultVerifyConfigForProject`（source ラベル `project_default`）へフォールバックする。これは Phase-C evolution 等、snapshot を前提としない文脈で検証器が nil／空にならないことを保証するためのもので（`phase_c_boundary_test.go` がこの不変条件をテスト）、submit ゲートの fail-closed 性とは別レイヤーである。
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
- [S3-2] Trace による Event Taxonomy 記録: Task ID / Command ID をトレース軸とし、固定のイベント型を持つイベントを記録・出力する [MUST]。イベントは `internal/events`（Bus）が発行する。
  - **現行の実装済み event type（baseline）**: `task_started` / `task_completed` / `phase_transition` / `queue_written`（`internal/events/bus.go`）。まずこの最小タクソノミーで dispatch〜完了の主要遷移を観測可能にする。
  - **拡張ターゲット（SHOULD）**: ライフサイクルの解像度を上げるため、`task_dispatched` / `task_verify_started` / `task_verify_failed` / `repair_scheduled` / `paused_for_replan` / `task_aborted` 等を順次追加する。固定イベント型の Append-only 記録という原則は維持する。

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
  - **実装の実態（部分達成）**: `UsefulnessTracker`（`internal/daemon/reviewer/usefulness.go`）はモデルごとのレビュー回数・finding 数・採用率（`AdoptionRate` / `AdoptionKnown`）のスキーマを備える。ただし production の記録経路（`review_coordinator.go` の `RecordResult`）が採用 finding ID に常に nil を渡すため、**採用率・「バグを減らしたか」の計測は現状構造的に空**で、蓄積されるのは finding 数とレビュー回数まで。採用判定の入力（後続 repair task との対応付け等）の配線が残課題。
- [A-3] Plannerの自己診断（初期 Meta-Planner）: フェーズ完了時にPlannerへ「Repair多発箇所」「ブロックタスク」を要約させ、次フェーズ
  の入力プロンプトに反省メモとして含める [SHOULD]。
- [A-4] Path-overlap Heuristic（簡易コンフリクト回避）: S3の expected_paths に基づき、「同じファイルを触るタスクは同時にディスパッ
  チしない（または同一Workerに寄せる）」という簡易的なヒューリスティック制御を行う [MUST]。

### Priority B: 群知能の限定解禁（AI Scientistスウォーム）

> **NOTE**: B-1 (Multi-rollout) と B-2 (Judge) は production 配線が完了せず本番採用しない方針となった。実装パッケージ (`internal/daemon/rollout`, `internal/daemon/judge`) およびそれらに紐付く Config (`rollout`, `judge`) と `OperationTypeRollout` は廃止済み。将来的に並列候補生成を再導入する場合は別途要件再定義からやり直す。

- [B-3] Graph-based Schedulingの高度化 (将来拡張): 簡易ヒューリスティック(A-4)の運用実績蓄積後、グラフ彩色やシンボル単位の競合予測
  アルゴリズムの導入を検討する [MAY]。

### 4.5. 実装フェーズ C: Agent—Daemon 責務境界の確立と適応的制御（Phase B完了後）

Phase A–B で確立した群知能基盤の上に、決定論的制御と LLM 推論の責務境界を明確にし、適応的な制御機構を段階的に導入する。

#### 4.5.1. 責務境界原則 (Responsibility Boundary Principles)

Phase C の全機能設計において、以下の責務分割原則を適用する。

1. **決定論的処理の Daemon 集約 [MUST]**: LLM 推論を必要としない決定論的処理（数値計算・統計処理・状態管理・プロセス管理）は Daemon が担う。Agent に機械的計算を委ねてはならない。
2. **Agent 責務の意味的判断限定 [SHOULD]**: Planner の責務は意味的判断（タスク分解・品質評価の解釈・設計方針策定）に限定する。構造的・数値的メトリクスの計算を Planner に委ねてはならない。
3. **Daemon 前処理—Agent 判断パターン [SHOULD]**: 構造的・数値的メトリクスは Daemon が前処理し、Agent には計算済みの結果のみを提供する。Agent は提供された指標に基づき意味的判断を行う。

**根拠**: Daemon 移譲により以下のメリットが得られる。

- **トークン浪費回避**: 決定論的計算に LLM トークンを消費しない
- **レイテンシ改善**: Go プロセスによる μ 秒〜ms オーダーの処理（LLM 経由では秒〜数十秒）
- **一貫性保証**: 機械的判定により結果のブレを排除

#### 4.5.2. Phase C 機能一覧と責務割当 (Feature Responsibility Matrix)

各機能について「判断主体」「実行主体」「Daemon移譲度」「拡張先コンポーネント」を明記する。

| ID  | 機能                             | 判断主体                                       | 実行主体                                                 | LLM 依存度 | Daemon 移譲度 | 拡張先コンポーネント                                 | 移譲メリット                                                                    |
| --- | -------------------------------- | ---------------------------------------------- | -------------------------------------------------------- | ---------- | ------------- | ---------------------------------------------------- | ------------------------------------------------------------------------------- |
| C-1 | 進化的コード品質改善             | Planner（変異方針策定）                        | Worker（変異生成）+ Daemon（Fitness 数値計算）           | 高         | 中            | `quality/engine.go` 拡張                             | Fitness 数値計算の LLM トークン節約                                             |
| C-2 | 適応的モデル選択                 | Daemon（UCB バンディット計算）                 | Daemon（モデル割当）                                     | なし       | 極高          | `worker_assign.go` の `GetModelForBloomLevel()` 置換 | LLM トークン完全不要、μ 秒オーダーレイテンシ、グローバル統計による最適化        |
| C-3 | 自律的検証ループ                 | Planner（検証戦略設計）                        | Worker（検証実行）+ Daemon（リトライ閾値判定・投票集計） | 高         | 低〜中        | （リトライ閾値判定のみ）                             | リトライ判定レイテンシ改善                                                      |
| C-4 | 探索的実装最適化                 | Planner（探索方針設定: 幅 vs 深さ優先度）      | Daemon（MCTS 木管理・UCT 計算・枝刈り）                  | 混合       | 高            | `daemon/worktree/` との統合                          | 探索判断 ms 化（Planner 経由なら数秒〜数十秒）、探索木管理で Planner 消費ゼロ   |
| C-5 | 自己改善メカニズム               | Planner（プロンプト最適化方針）                | Daemon（Fingerprint DB 管理）+ Worker（プロンプト適用）  | 中         | 高（一部）    | `daemon/learnings/` 拡張                             | 失敗パターン即座マッチ                                                          |
| C-6 | 適応的計算深度制御               | Planner（意味的複雑度判断）                    | Daemon（構造的指標前処理）                               | 中         | 中〜高        | 新コンポーネント                                     | 構造指標プリコンピュート                                                        |
| C-7 | マルチランタイム管理             | Planner（タスクスキーマで runtime/model 指定） | Daemon（プロセス起動・管理・ヘルスチェック）             | なし       | 極高          | `formation/tmux_formation.go` + `agent/launcher.go`  | Daemon 既存責務（formation/）の自然な拡張、Worker 禁止操作（D006 tmux）との整合 |
| C-8 | Feature Gate（プロファイル適用） | Planner（複雑度レベルの意味的判断）            | Daemon（プロファイル適用・構造的指標計算）               | 低〜中     | 高            | `quality/engine.go` の `RuleEvaluator` 拡張          | プロファイル適用の一貫性保証、Planner 判断の簡素化                              |

**C-2（適応的モデル選択）**: UCB1 式（`avg_reward + c * sqrt(ln(N) / n_i)`）は純粋な数学計算であり、Daemon が MUST 計算する。グローバルな統計情報（各モデルの成功率・使用回数）へのアクセスも Daemon が一元管理する。

**C-7（マルチランタイム）**: ランタイム起動・tmux セッション管理は Daemon の既存責務（`formation/`）の延長であり、Agent が直接プロセス管理を行ってはならない。Worker の破壊的操作禁止ルール（D006: kill/tmux 操作禁止）とも整合する。Planner はランタイム種別の選択（意味的判断）のみを担う。

**C-8（Feature Gate）**: プロファイル適用（許可機能フィルタリング）は機械的判定であり、Daemon が MUST 実行する。

> **実装の実態（C-8 の判断主体）**: 要件初版は「Planner が複雑度レベルの意味的判断を担う」設計だが、現行実装では複雑度評価も Daemon が機械算出する（`EvaluateLevel`）。Planner は複雑度評価に関与せず、Orchestrator オーバーライドと per-task の実ゲーティングは未接続。詳細と達成状況は [§5 C-8](#c-8-planner-による適応的機能制御feature-gate) の基本要件注記を参照。

#### 4.5.3. Daemon 拡張方針 (Daemon Extension Points)

Phase C 機能の Daemon 側実装は、既存アーキテクチャの拡張ポイントを活用する [SHOULD]。

| 移譲推奨度 | 機能 | 拡張先                                             | 拡張内容                                                                                                                                                                                                 |
| ---------- | ---- | -------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 極高       | C-2  | `worker_assign.go`                                 | 静的 bloom_level マッピングを UCB1 ベースのアーム選択に置換。S3 Trace データを報酬信号として入力。報酬テーブルを JSONL または SQLite で永続化 [SHOULD]                                                   |
| 極高       | C-7  | `formation/tmux_formation.go`, `agent/launcher.go` | ランタイム定義テーブル追加。Planner 指定のランタイム種別に基づく tmux セッション・Worker 起動の拡張 [SHOULD]                                                                                             |
| 高         | C-4  | `daemon/worktree/` + 新パッケージ `search/`        | 探索木管理を worktree 管理と統合。UCT 計算エンジンを `search/` に配置。Planner には方針設定 API のみ公開 [SHOULD]                                                                                        |
| 高         | C-5  | `daemon/learnings/`                                | Fingerprint DB を拡張し、失敗パターンの構造化蓄積・検索を実現 [SHOULD]                                                                                                                                   |
| 高         | C-8  | `daemon/featuregate/evaluator.go`（正本）          | プロファイルベースの複雑度評価・機能ゲート。**実装の正本は `daemon/featuregate/` であり、`quality/engine.go` の `FeatureGateRule` は非登録・非ブロッキングの残骸（常に `(true, nil)` を返す）** [SHOULD] |

#### 4.5.4. Planner 負荷軽減方針 (Planner Load Reduction)

Phase A–B の Planner 7 責務に加え Phase C で追加される判断負荷を、Daemon 前処理により軽減する [SHOULD]。

| Phase C 機能     | 軽減前（Planner 責務）                  | 軽減後（Daemon 前処理 → Planner 意味的判断のみ）                                                                                                                                                         |
| ---------------- | --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| C-2 モデル選択   | Planner がモデル適性を判断              | Daemon が UCB 計算で自動決定。Planner はモデル選択に関与しない                                                                                                                                           |
| C-4 探索戦略     | Planner が探索木全体を管理              | Daemon が木操作・UCT 計算を実行。Planner は方針（幅 vs 深さ優先度）設定のみ                                                                                                                              |
| C-6 計算深度     | Planner が全指標を自力で評価            | Daemon が構造的指標（ファイル数・依存関係・変更行数）を前処理し、Planner は意味的複雑度のみ判断                                                                                                          |
| C-8 プロファイル | Planner が複雑度評価 + プロファイル選択 | Daemon が複雑度を機械算出しプロファイルを決定（現行実装。Planner は関与しない）。要件初版の「Planner が意味的判断で最終選択」は未実装 — [§5 C-8](#c-8-planner-による適応的機能制御feature-gate) 注記参照 |

#### 4.5.5. Anti-Requirements §6 との整合性 (Consistency with Anti-Requirements)

| §6 項目                                     | 整合性確認                                                                                                                                                                        |
| ------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1. LLM による Fitness 判定の上書き禁止      | C-1 の Fitness 数値計算を Daemon 移譲することで、LLM が Fitness を上書きする余地をさらに排除。**整合**                                                                            |
| 2. LLM 同士の多数決の採用禁止               | C-2 の UCB バンディットは統計的手法であり多数決ではない。判断は Daemon の数学計算に基づく。**整合**                                                                               |
| 3. Frankenstein マージ禁止                  | C-4 の探索的実装は独立 Worktree で実行し、勝者を丸ごと採用。合成は行わない。**整合**                                                                                              |
| 4. Worker 間の直接通信禁止                  | C-7 のマルチランタイムも Daemon 経由の Hub-and-Spoke 型を維持。**整合**                                                                                                           |
| 5. 共有可変状態の導入禁止                   | C-5 の学習知見 DB は Daemon が一元管理し、Agent は ReadOnly で参照。**整合**                                                                                                      |
| 6. Verify 不可タスクへの Multi-rollout 禁止 | Multi-rollout 自体が廃止済 (B-1/B-2 削除)。C-4 の探索的実装は verify.yaml 必須を前提とする。**整合**                                                                              |
| 7. 初期段階での Embedding RAG・Bandit 禁止  | C-2 の UCB バンディットは Phase C（Phase S 完了後）で導入するため、§6-7 の「最初から」の制約と矛盾しない。ただし Phase S の Trace ログ蓄積（S3）が前提条件となる [MUST]。**整合** |

———

## 5. 実装フェーズ C: 進化的品質最適化と自律改善（Sakana.ai 研究知見）

> 註: §4 配下の `### 4.5.` は Phase C の責務境界原則をまとめた小節であり、本 §5 とは内容が補完関係にある。本節 §5 は §4.5 で確立した責務境界の上に立つ機能定義（C-1〜C-8）を扱う。歴史的経緯で同じ番号 4.5 が二か所に出現していたのを §5 へ昇格させて解消した。

Phase A-B の基盤が安定稼働した上で、Sakana.ai の研究知見（ShinkaEvolve, Darwin Gödel Machine, AI Scientist, AB-MCTS, Continuous Thought Machines）を統合し、システムの品質最適化能力と自律改善能力を段階的に拡張する。

本フェーズは既存アーキテクチャとの親和性および実装難易度に基づき、以下の3段階で実装する。

- **Phase C-α（既存拡張）**: C-2, C-3 — config/verify.yaml/learnings の拡張で実装可能
- **Phase C-β（新規基盤）**: C-1, C-6 — 進化的フレームワーク基盤、複雑度推定基盤が必要
- **Phase C-γ（長期研究）**: C-4, C-5 — 探索木管理、自己改善フレームワークが必要

> **C-1 / C-4 の現状（観測段階）**: C-1（進化エンジン）と C-4（探索木）はインフラのみ実装済みで意思決定に未配線（gate off が正常運用）。具体的機能要件は [§11 Phase C 観測段階機能](11-future-extensions.md) へ集約し、本 §5 の対応節は参照ハブに留める。例外として C-4-4（機械的 Tie 解消、[MUST]）のみ実装・稼働済みで §5 C-4 に残置する。

### C-1: Evolutionary Code Quality（進化的コード品質改善）[Phase C-β / 観測段階]

**概要**: 単一 worktree 内で変異→評価→選択サイクルによるコード品質の反復改善を実現する。

> **実装状況・要件の所在**: 進化エンジン（`internal/daemon/evolution/engine.go` の `PlanMutations` / `CheckNovelty` / `SelectSurvivors`）は実装され feature gate（`Evolution.EffectiveEnabled()`、既定 off）下にあるが、現状は**観測段階（observability）にとどまり、実 dispatch を駆動しない**（`recordEvolutionSignal` がログ出力のみ、`SelectSurvivors` は production 呼び出し元なし）。**gate off での非発火が正常運用**であり、本機能は未完成の必須機能ではない。この位置づけと判断に基づき、C-1 の具体的機能（C-1-1〜C-1-3、[SHOULD]/[MAY]）・研究対応・統合方法・Anti-Requirements 整合は将来拡張として [§11 Phase C 観測段階機能](11-future-extensions.md) へ集約した。受け入れ基準 §7 Phase C-β の「変異→評価→選択サイクルが機能」は未達（本節は将来拡張への参照ハブとして ID を保持する）。

### C-2: Adaptive Model Selection（適応的モデル選択）[Phase C-α]

**概要**: UCB バンディットアルゴリズムによりタスク種別×モデル性能を追跡し、最適モデルを動的に選択する。

**研究対応**:

- ShinkaEvolve: UCB バンディットによる LLM 動的選択。コスト考慮係数による効率的なモデル配分
- AB-MCTS: モデル選択を探索の第3次元として位置付け、複数 LLM の適応的選択を実現

**統合方法**: config.yaml の worker models 設定を動的化する。Trace JSONL（S3-2）に蓄積されたタスク結果データを入力とし、モデル割当を最適化する。§6-7 の「最初からの Bandit アルゴリズム実装禁止」に留意し、**S3 の Trace ログ運用実績が十分に蓄積された後に解禁する** [MUST]。

**具体的機能**:

- [C-2-1] タスク種別別モデル性能トラッキング: タスク種別（bloom_level × persona）ごとに、各モデルの Fitness 結果（Pass率、Repair 回数、実行時間）を Trace JSONL から集計する [SHOULD]。
- [C-2-2] UCB1 スコアに基づくモデル割当: 探索（未知モデルの試行）と活用（高性能モデルの優先）のバランスを UCB1 アルゴリズムで制御する [SHOULD]。
- [C-2-3] コスト考慮係数: モデル選択時に API コスト（トークン単価 × 平均消費量）を考慮し、性能/コスト比を最適化する [MAY]。

**Anti-Requirements 整合**:

- §6-7 準拠: S3 の Trace ログ運用と古典的タグ検索による効果測定が確立された後に初めて導入する。Trace データ不足時は静的な config 設定にフォールバックする [MUST]
- §6-1 準拠: モデル選択の評価指標は機械的 Fitness 結果のみ。LLM の自己評価を選択根拠としない

### C-3: Autonomous Verification Loop（自律的検証改善ループ）[Phase C-α]

**概要**: AI Scientist の自動査読＋リフレクション機構を verification フェーズに統合し、検証の多観点化と自律的な改善サイクルを実現する。

**研究対応**:

- AI Scientist: 5 独立レビュー＋5 リフレクションによる品質保証。NeurIPS Area Chair 方式の統合判定
- Darwin Gödel Machine (DGM): 確率的デバッグ（debug_prob）による自動修正試行

**統合方法**: 既存 verify.yaml（S1-1）と Planner 自己診断（A-3）を拡張する。検証観点を追加し、失敗時の自動修正試行を構造化する。

**具体的機能**:

- [C-3-1] 多観点アンサンブル検証: verify.yaml の build/lint/test/typecheck に加え、セキュリティ（依存脆弱性スキャン）およびパフォーマンス（ベンチマーク回帰）の検証観点を追加可能とする [SHOULD]。各観点は独立した検証コマンドとして定義され、結果は機械的に集約される。
- [C-3-2] 確率的リトライと自動修正: Verify 失敗時に、Failure Fingerprint（S2-1）と照合した上で、同一指紋でない新規エラーに対し自動修正を試行する [SHOULD]。試行回数は definition_of_abort の max_repair_count と連携し、上限を超過した場合は paused_for_replan へ遷移する。
- [C-3-3] 検証結果の重み付き集約: 複数検証観点の結果を重み付きで集約し、総合 Fitness スコアに反映する [MAY]。重みは検証観点ごとに config で静的に定義し、LLM による動的調整は行わない。

**Anti-Requirements 整合**:

- §6-1 準拠: 検証結果の集約は機械的な重み付き計算のみ。LLM による判定上書きを禁止する
- §6-2 準拠: 多観点検証は多数決ではなく、各検証コマンドの Pass/Fail を独立に評価する
- §6-6 準拠: verify.yaml 未定義タスクへの拡張検証適用を禁止する

### C-4: Exploratory Search（探索的実装最適化）[Phase C-γ / 観測段階]

**概要**: AB-MCTS の探索木構造を worktree 並列実行に適用し、複数の実装候補を木構造で展開して最良解を選択する。

> **実装状況・要件の所在**: 探索木インフラ（`internal/daemon/search/tree.go` の UCT/Prune/SelectBest、`thompson.go` の widen/deepen サンプリング、`phase_c_search.go` の reward backprop）は実装され gate（既定 off）下にあるが、現状は**観測・報酬集計段階にとどまり、実 dispatch を駆動しない**（Thompson 決定・UCT 計算はログ/報酬蓄積のみ。「Actual worker assignment is decided before dispatch」、`phase_c_integration.go`）。**gate off での非発火が正常運用**である。この位置づけに基づき、探索木管理・探索戦略・枝刈り（C-4-1〜C-4-3、[MAY]）と研究対応・統合方法・Anti-Requirements 整合は将来拡張として [§11 Phase C 観測段階機能](11-future-extensions.md) へ集約した。受け入れ基準 §7 Phase C-γ の「木構造探索が機能」は未達。
>
> **本節に残る現行要件 ― C-4-4（実装・稼働済み）**:
>
> - [C-4-4] 機械的 Tie 解消: 探索木の最終候補選択において Fitness が Tie の場合は、決定論的なフォールバック（最初に到達した候補を採用）で解消する [MUST]。`search/tree.go` の `SelectBest`（strict `>` 比較で到達順先頭が勝つ）として実装済みで、Judge (B-2) は廃止済みのため LLM 介入は行わない。これは観測段階機能ではなく、探索インフラが将来 gate on された際の決定論性を保証する現行の機械的規範である。

### C-5: Self-Improvement Mechanism（自己改善メカニズム）[Phase C-γ]

**概要**: Darwin Gödel Machine の自己改善パラダイムを応用し、システムが過去の実行結果から学習して戦略を自動改善する。

**研究対応**:

- Darwin Gödel Machine (DGM): エージェントが自身のコードを進化的に自己改善。アーカイブベース選択による stepping stone 保持
- ShinkaEvolve: プロンプト進化、EVOLVE-BLOCK 的な進化対象領域の指定

**統合方法**: 既存 learnings 機構を構造化強化し、Planner 自己診断（A-3）を拡張する。自己改善の対象は Planner/Worker のプロンプト・設定に限定し、Daemon 制御ロジックや状態遷移は改変対象としない。

**具体的機能**:

- [C-5-1] 構造化失敗パターン学習: Failure Fingerprint（S2-1）を活用し、失敗パターンを分類・構造化する [SHOULD]。類似パターンの再発時に過去の修復戦略を自動提案する。
- [C-5-2] Planner プロンプト/ペルソナの進化的最適化: EVOLVE-BLOCK 的な進化対象指定により、Planner のプロンプトおよびペルソナ定義の特定領域を進化的に最適化する [MAY]。最適化の評価は Fitness 関数（S1-2）の結果に基づく。
- [C-5-3] アーカイブベース戦略: 過去の成功パターン（高 Fitness スコアのタスク実行結果）を stepping stone として保持し、類似タスクの初期戦略として参照する [MAY]。

**安全装置**:

- Reward hacking 防止（DGM の教訓）: 自己改善が評価指標自体を操作するリスクに対し、Fitness 関数の機械的評価原則（§6-1）を安全弁として維持する [MUST]。Fitness 関数の定義・重み・閾値は自己改善の対象外とする [MUST]。
- 改善対象の限定: 自己改善対象は Planner/Worker のプロンプト・設定・ペルソナ定義のみとする。Daemon 制御ロジック、状態遷移定義、Fitness 関数定義は改変禁止とする [MUST]。

**Anti-Requirements 整合**:

- §6-1 準拠: Fitness 関数の評価ロジック自体は自己改善の対象外。LLM が Fitness 判定を上書きすることを禁止する
- §6-5 準拠: 自己改善で得た知見は learnings 機構経由で伝達する。共有可変状態を導入しない
- §6-7 準拠: S3 の Trace ログ運用実績が蓄積された後に段階的に解禁する

### C-6: Adaptive Computation Depth（適応的計算深度）[Phase C-β]

**概要**: Continuous Thought Machines (CTM) の「内部刻み（Internal Ticks）」概念を応用し、タスクの複雑度に応じて Worker の推論ステップ数（サブタスク分割粒度）を動的に調整する。

**研究対応**:

- Continuous Thought Machines (CTM): タスク複雑度に応じた内部反復ステップ数の動的調整。確信度ベースの早期終了

**統合方法**: bloom_level とタスク複雑度メトリクスに基づき、Worker への指示粒度（サブタスク分割の深さ）と検証深度を動的に調整する。

**具体的機能**:

- [C-6-1] タスク複雑度推定: bloom_level、content の構造（変更対象ファイル数、依存関係の深さ）、過去の類似タスクの Repair 率から複雑度スコアを機械的に算出する [SHOULD]。
- [C-6-2] 推論ステップ数の動的調整: 複雑度スコアに基づき、単純タスクには浅い分割（単一 Worker 直接実行）、複雑タスクには深い分割（多段サブタスク）を Planner が自動選択する [SHOULD]。
- [C-6-3] 確信度ベース早期終了: Worker が acceptance_criteria を十分な確度で満たした場合、追加の検証ステップ（拡張検証観点等）をスキップ可能とする [MAY]。確信度の判定は Fitness 関数のスコアマージンに基づき、LLM の自己申告に依存しない。

**Anti-Requirements 整合**:

- §6-1 準拠: 複雑度推定および確信度判定は機械的メトリクスに基づく。LLM の主観的判断に依存しない
- §6-7 準拠: 複雑度推定の入力データは Trace JSONL（S3-2）の蓄積データを使用する。十分なデータ蓄積前はデフォルトの分割粒度にフォールバックする [MUST]

### C-7 マルチコーディングエージェントランタイム

ランタイム制限は **role 単位** で適用する。Orchestrator / Planner はプロジェクトルート上で maestro CLI 権限を持って直接動作するため、`claude-code` の `--allowedTools` / `--disallowedTools` と PreToolUse hook による機械的な role enforcement が必須であり、**claude-code に限定する**。Worker は per-worker git worktree で blast radius が封じ込められるため、**codex / gemini を本番ランタイムとして許可する**。

#### 基本要件

1. **デフォルトランタイム**: 明示的な指定がない場合、全 role は claude-code で実行される [MUST]。
2. **Orchestrator / Planner は claude-code 限定**: これらの role に codex / gemini を指定した config は validation で拒否する [MUST]（`internal/model/config_validate.go`: `ParseRuntimeFromModel != RuntimeClaudeCode` で拒否）。
3. **launcher 側二重防御**: validation を迂回しても、launcher は Orchestrator / Planner role の non-claude-code runtime を hard-reject する [MUST]（`internal/agent/launcher.go`: `launchAlternativeRuntime` が role=="orchestrator"/"planner" を拒否）。
4. **Worker の非 claude-code ランタイム許可と封じ込め**: Worker は codex / gemini で起動できる [MAY]。ただし `run_on_main`（実 main checkout 上で実行するタスク）は非 claude worker に割り当てない [MUST]。これは `plan.AssignWorkers` の `RequireClaudeRuntime` と `dispatch.validateRunOnMainPreflight` で機械的に保証する。worktree 外への破壊的操作の防御は operator 側（host sandbox / shell policy）の責務であり、in-tree では行わない。
5. **フォールバック禁止**: Orchestrator / Planner では codex / gemini から claude-code への runtime fallback は行わず、設定時点で拒否する [MUST]。

#### config.yaml 設定例

```yaml
agents:
  orchestrator:
    model: opus # claude-code 限定（非 claude を指定すると validation で拒否）
  planner:
    model: opus # claude-code 限定
  workers:
    count: 4
    default_model: sonnet
    models:
      worker3: opus
      worker4: codex # Worker は codex / gemini を本番指定可（codex の既定モデル）
      # worker5: codex-5      # codex- / gemini- プレフィックスで明示モデル指定（例: codex-5, gemini-2.5-pro）
```

#### ユースケース

- **MCTS 探索的実装最適化（C-4 統合）**: 複数 worktree で claude-code worker が同一タスクの実装を並列探索し、Fitness 関数で最適解を選定する [MAY]。
- **クロスモデルレビュー**: 実装 worker と異なる claude-code model がレビュー・検証を担当し、モデル差分による相互補完により品質を向上させる [SHOULD]。
  - §6-4 との整合性: クロスエージェントレビューは Worker 間の直接通信ではなく、Planner 経由の DAG 依存関係として実現する [MUST]。
- **適応的モデル選択（C-2）統合**: UCB バンディットの選択空間を claude-code model に限定し、タスク特性に応じた最適な model を学習する [SHOULD]。
- **修正時の別 worker/model 担当**: Repair タスクにおいて、元の実装 worker と異なる worker/model を割り当て、同一 model のバイアスによる修正失敗を回避する [MAY]。

#### Anti-Requirements 整合性

- **§6-4（Worker 間直接通信禁止）**: 全てのエージェント間連携は Daemon 経由の非同期通信で行い、Worker 間の直接通信は発生しない [MUST]。
- **§6-5（共有可変状態禁止）**: 各エージェントは独立した worktree で動作し、共有可変状態を持たない [MUST]。認識の同期は Planner によるインターフェースの先行定義と ReadOnly 参照で解決する。
- **§6-3（Frankenstein マージ禁止）**: 異なるエージェントの出力を LLM が合成することは禁止し、Winner-takes-all を維持する [MUST NOT]。

#### タスクスキーマ拡張

§2.2 の最小データスキーマに以下のフィールドを追加する。既存フィールドとの後方互換性を維持する [MUST]。

**Planner の `plan submit` 入力タスクスキーマ**（`internal/plan/input.go` の `TaskInput`。完全な一覧は [§4.7.1](04-yaml-schema.md) 参照）:

```yaml
task:
  name: string # submit 内ローカル名（blocked_by 参照用）
  purpose: string
  content: string
  acceptance_criteria: string
  blocked_by: [name]
  bloom_level: integer
  required: bool
  expected_paths: [path_prefix] # S3-1
  definition_of_done: [string]
  definition_of_abort: # S3-1 / S2-2
    max_repair_count: integer
    max_wall_clock_sec: integer
    explicit_failure_conditions: [string]
  # 実行レーン制御（Planner が指定可能）
  worker_id: string # 特定 Worker へのピン留め（省略時は bloom-level 自動割当）[MAY]
  run_on_main: bool # main checkout 上で実行（読み取り専用検証等。非 claude worker には割り当てない）[MAY]
  run_on_integration: bool # integration worktree 上で実行（publish conflict 解決等）[MAY]
  persona_hint: string # 適用 persona [MAY]
  skill_refs: [string] # 参照 skill [MAY]
```

> **runtime / model は Planner タスク入力ではない**: Worker のランタイム・モデルは `plan submit` のタスク入力では指定しない。**config の `agents.workers.models`** で Worker 単位に設定する（`worker4: codex` で既定モデル、`worker4: codex-5` / `gemini-2.5-pro` でモデル明示。書式は `<runtime>/<model>` のスラッシュ区切りではなく、モデル名そのものへの exact / prefix マッチ。詳細は [§11](11-future-extensions.md) のマルチプロバイダー対応）（[REQUIREMENTS.md](REQUIREMENTS.md) §5 C-7、[§4.1.1](04-yaml-schema.md)）。

> **内部 queue スキーマ（参考）**: デーモンが書き出す `queue/worker{N}.yaml` のタスク型（`internal/model/queue.go`）は、スキーマ互換のため `runtime` / `model_override` / `complexity_level` フィールドを保持する。これらは Planner 入力フィールドではなく、Worker 割当・Feature Gate（C-8）の結果としてデーモンが設定する内部フィールドである。`complexity_level` は Planner が自動判定し、Orchestrator から明示オーバーライドも受け付ける [MUST]。

- 上記の実行レーン制御フィールドが未指定の場合、既存の Phase A-B 動作と完全に同一となる [MUST]。

### C-8 Planner による適応的機能制御（Feature Gate）

Phase C の各機能およびマルチエージェント機能を、Planner がタスク複雑度に応じて動的に On/Off 制御する仕組みを導入する。

#### 基本要件

> **実装状況の注記（要件初版との乖離）**: 本節の要件初版は複雑度評価を Planner の意味的判断として設計していたが、現行実装は §4.5.1 の「決定論的処理の Daemon 集約」原則に沿い、複雑度評価を **Daemon が機械的に算出する**方向へ移行している（`internal/daemon/complexity/scorer.go` + `internal/daemon/phase_c_integration.go` の `EvaluateLevel`）。その結果、下記要件のうち **1（Planner 評価）/ 3（planner.yaml への根拠記録）/ 4（Orchestrator オーバーライド）/ 5（タスク単位の実ゲーティング）は現状未接続**である。詳細は各項目の注記を参照。要件 2・6 は実装済み。

1. **複雑度評価**: タスク複雑度を評価し `complexity_level`（`simple|standard|complex|critical`）を決定する [MUST]。
   - **実装の実態**: 評価は Daemon が機械的に算出する（`EvaluateLevel` → `ComplexityScorer.Estimate` / `FeatureEvaluator.GetLevel`）。Planner の `plan submit` 入力スキーマ（`internal/plan/input.go` の `TaskInput`）に `complexity_level` フィールドは存在せず、Planner は複雑度評価に関与しない。要件初版の「Planner がコマンド受付時に評価」は未実装であり、責務は Daemon 側へ移っている。`complexity_level` フィールド自体は `queue/`（`model.Command` / `model.Task`、`internal/model/queue.go:100-101`）に存在するが、現状 production code で代入する経路はない（telemetry のみ。下記 5 を参照）。
2. **複雑度レベル別機能プロファイル**: 4段階の複雑度レベルに対応する機能プロファイルを定義する [MUST]。デフォルトプロファイルは `internal/daemon/featuregate/evaluator.go` の `DefaultProfiles()` が正本であり、下記の config 例と一致する。**（実装済み）**
   - **Simple**: 全機能無効（基本実行のみ）
   - **Standard**: `adaptive_model_selection`（C-2）のみ有効
   - **Complex**: `adaptive_model_selection`（C-2）+ `cross_agent_review`（A-1）+ `adaptive_depth`（C-6）有効
   - **Critical**: 全機能有効（C-1/C-3/C-4/C-5 含む）
3. **判断根拠の透明性**: 複雑度判定の根拠を記録する [SHOULD]。
   - **実装の実態**: 現行実装は複雑度を `phase_c_command_classified` / `phase_c_task_classified` の **構造化ログ**として `daemon.log` に出力する（`phase_c_integration.go:152-199`）。要件初版の「Planner が planner.yaml に記録」は、Planner が複雑度評価に関与しない設計変更に伴い未実装。透明性は Daemon 側のログで担保される。
4. **オーバーライド**: `complexity_level` を明示指定して自動判定を上書きできる [MUST]。
   - **実装の実態**: **未実装**。Orchestrator から `complexity_level` を渡す CLI フラグ・経路は存在せず（`cmd/maestro/` に complexity フラグなし）、`model.Command` / `model.Task` の `ComplexityLevel` フィールドへ production code で代入する箇所もない。将来オーバーライドを解禁する場合は別途実装する。
5. **タスク単位の粒度**: 同一コマンド内の異なるタスクに対して、それぞれ異なるプロファイルを適用できる [MUST]。
   - **実装の実態**: プロファイルの**算出・分類ログ**はタスク単位で行われる（`classifyAndLogTask`）が、算出結果を実際の機能 on/off に接続する**実ゲーティングは未配線**である。`IsFeatureEnabled`（`phase_c_integration.go:52`）には production 呼び出し元がなく、C-1〜C-6 各機能の発火は per-task プロファイルではなく config のグローバルフラグ（`cfg.X.EffectiveEnabled()`）で決まる。現状の per-task プロファイルは telemetry 段階にとどまる。
6. **縮退動作**: 機能プロファイルの適用に失敗した場合、Simple プロファイルへフォールバックする [MUST]。**（実装済み。`evaluator.go` の `Evaluate` が未知レベルを Simple に縮退）**

#### config.yaml 設定例

```yaml
feature_profiles:
  simple:
    cross_agent_review: false
    exploratory_optimization: false
    evolutionary_quality: false
    adaptive_model_selection: false
    self_improvement: false
    adaptive_depth: false
  standard:
    cross_agent_review: false
    exploratory_optimization: false
    evolutionary_quality: false
    adaptive_model_selection: true
    self_improvement: false
    adaptive_depth: false
  complex:
    cross_agent_review: true
    exploratory_optimization: false
    evolutionary_quality: false
    adaptive_model_selection: true
    self_improvement: false
    adaptive_depth: true
  critical:
    cross_agent_review: true
    exploratory_optimization: true
    evolutionary_quality: true
    adaptive_model_selection: true
    self_improvement: true
    adaptive_depth: true
```

#### 複雑度評価基準

Planner は以下の基準を総合的に評価し、複雑度レベルを決定する [SHOULD]。

| 評価軸                         | Simple             | Standard               | Complex            | Critical           |
| ------------------------------ | ------------------ | ---------------------- | ------------------ | ------------------ |
| 影響範囲（変更ファイル数予測） | 1-3 ファイル       | 4-10 ファイル          | 11-30 ファイル     | 30+ ファイル       |
| 依存関係の複雑さ               | 単一モジュール内   | モジュール間参照あり   | 複数モジュール横断 | システム全体に波及 |
| アーキテクチャ変更             | なし               | 軽微な拡張             | 新規パターン導入   | 基盤設計変更       |
| テスト戦略                     | 既存テスト修正のみ | 新規テスト追加         | 統合テスト必要     | E2E テスト必要     |
| 既存コードとの統合難易度       | 追記のみ           | 軽微なリファクタリング | API 変更を伴う     | 後方互換性考慮必要 |

#### Anti-Requirements 整合性

- **§6-1（LLM による Fitness 判定上書き禁止）**: 複雑度評価は Planner の計画判断であり、Fitness 関数自体の変更は行わない [MUST NOT]。Feature Gate は「どの機能を有効化するか」を制御するのみであり、S1-2 の機械的評価基準は不変 [MUST]。
- **§6-7（最初からの Bandit アルゴリズム禁止）**: 適応的モデル選択（C-2）は Standard 以上で有効化されるが、S3 トレースデータ蓄積が前提条件 [MUST]。Feature Gate はこの前提条件の充足を検証した上で機能を有効化する。

———

## 6. やらないこと (Anti-Requirements / Out of Scope)

システムの崩壊（観測不能・決定不能・暴走）を防ぐため、以下の実装は明確に禁止する。

1. LLMによるFitness判定の上書き禁止 [MUST NOT]
   - LLMは build/lint/test/typecheck の結果を覆してはならない。補助的説明やTie-breakのみを行う。
2. LLM同士の多数決の採用禁止 [MUST NOT]
   - 賢くない多数派（Dumb Majority）に引っ張られるのを防ぐため、判断は常にコンパイラ・テストに委ねる。
3. 複数候補の合成（Frankenstein マージ）禁止 [MUST NOT]
   - 複数モデルのコードをLLMに切り貼りさせる統合は論理破綻の温床となるため、常に「1つのWorktreeを丸ごと採用（Winner-takes-all）」
     とする。Judge (B-2) は廃止済のため LLM による Tie-breaking も行わない。
4. Worker（Agent）間の直接通信禁止 [MUST NOT]
   - 隔離性と制御可能性を損なうため、常にDaemonを介した非同期通信（Hub-and-Spoke型）を維持する。
5. 共有可変状態（Typed Contract Blackboard等）の導入禁止 [MUST NOT]
   - 認識の同期は、Plannerによる「インターフェースの先行定義とReadOnly参照」で解決する。
6. Verify不可のタスクに対する並列候補生成（Multi-rollout）禁止 [MUST NOT]
   - 成功基準が曖昧な状態で群知能を回すことは、高コストな乱数発生器に過ぎないため発火させない。なお B-1 (Multi-rollout) 自体は本実装では廃止済み。将来再導入する場合も本制約を継承する。
7. 最初からの Embedding (ベクトル) RAG や Bandit アルゴリズムの実装禁止 [MUST NOT]
   - Traceログや古典的タグ検索での効果測定（S3）が回るまでは、高度な自己学習アルゴリズムの実装は見送る。

———

## 7. 受け入れ基準 (Acceptance Criteria)

以下の条件を満たした段階で、各フェーズの実装完了とみなす。

- Phase S 完了基準:
  - **`verify.enabled: true` のとき**、command-scoped verify config snapshot を持たないコマンドが `plan submit` を通過しないこと（submit ゲートが fail-closed で拒否。S1-1）。`verify.enabled: false` は検証を意図的にスキップする正常運用モードであり、この fail-closed 制約の対象外。
  - 同一の Failure Fingerprint が発生した場合、Repairループが停止しPlannerへ差し戻されること。
  - タスクの主要ライフサイクル遷移が `internal/events` の固定イベント型として記録・追跡可能であること（実装済み baseline は S3-2 の 4 イベント型。残りは拡張ターゲット）。
- Phase A 完了基準:
  - 異種モデル Reviewer の指摘事項がマージをブロックせず、かつその採用率がデータとして蓄積されていること。
- Phase B 完了基準:
  - **B-1/B-2 は廃止**（production 配線完了せず）。Multi-rollout・Judge は将来再導入時に再要件化する。B-3 (Graph-based Scheduling) は将来拡張として残置。
- Phase C 責務境界完了基準:
  - §4.5.1 の責務境界原則（決定論的処理の Daemon 集約・Agent 責務の意味的判断限定・Daemon 前処理パターン）が、Phase C 全機能（C-1〜C-8）の設計・実装において遵守されていること。
  - C-1〜C-8 の各機能について「判断主体」と「実行主体」が §4.5.2 のマトリクスに従い実装されていること。
  - C-2（適応的モデル選択）において、モデル割当が Daemon の UCB バンディット計算により自動決定され、Planner/Worker がモデル選択判断に関与しないこと。LLM トークンを消費しないこと。
  - C-4（探索的実装最適化）において、探索木操作（MCTS/UCT 計算・枝刈り）が Daemon 内で完結し、探索結果の評価が S1-2 の機械的 Fitness に基づくこと。
  - C-7（マルチランタイム管理）において、異種ランタイムのプロセス起動・管理が Daemon に集約され、Hub-and-Spoke 型通信が維持されていること。LLM トークンを消費しないこと。
  - C-8（Feature Gate）において、構造的指標の計算が Daemon 前処理として実行されること。**注: 現行実装では複雑度評価を Daemon が完結させ、Planner への結果提供・Planner の意味的最終選択は行わない（[§5 C-8](#c-8-planner-による適応的機能制御feature-gate) 基本要件注記のとおり未接続）。**
  - §4.5.5 の Anti-Requirements 整合性が維持されていること（特に §6-7 との整合：Phase S の Trace ログ蓄積が C-2 導入の前提条件として検証済みであること）。
- Phase C-α 完了基準:
  - 適応的モデル選択（C-2）が稼働し、Trace JSONL のタスク結果データに基づきモデル割当が動的に最適化されていること。デフォルトモデルへの縮退が正常に動作すること。
  - 自律的検証改善ループ（C-3）が verification フェーズに統合され、2回以上の自律的改善サイクル（検証→修正→再検証）を完了できること。検証カバレッジの拡大方向でのみ改善を行い、基準の緩和が発生しないこと。
- Phase C-β 完了基準（**現状未達 — C-1 は観測段階。C-1 の機能要件は [§11](11-future-extensions.md) に集約済みで、本基準はその将来達成条件**）:
  - 進化的コード品質改善（C-1）の変異→評価→選択サイクルが単一 worktree 上で機能すること。S1-2 の Fitness 関数による機械的選定が維持されていること。**注: 現行実装では C-1 エンジンは存在するが変異の実適用が未配線（observability 段階）であり、本基準は未達。**
  - Fitness 関数（S1-2）にコード品質スコア軸が追加され、静的解析ツール出力に基づく機械的評価が稼働していること。
  - 適応的計算深度（C-6）により、bloom_level に応じたサブタスク分割粒度の動的調整が機能すること。definition_of_abort の閾値を超えないこと。
- Phase C-γ 完了基準（**現状未達 — C-4 は観測段階。C-4-1〜C-4-3 の機能要件は [§11](11-future-extensions.md) に集約済みで、本基準はその将来達成条件。C-4-4 のみ実装済み**）:
  - 探索木管理（C-4）が worktree と統合され、Planner による展開・枝刈り判断に基づく木構造探索が機能すること。verify.yaml を持つタスクのみを対象とし、worktree 単位の独立評価で Winner-takes-all が機能すること。**注: 現行実装では C-4 の探索木・Thompson サンプリングは報酬集計/観測に使われるのみで実 dispatch を駆動しておらず、本基準は未達（C-4-4 の機械的 Tie 解消のみ実装済み）。**
  - 自己改善メカニズム（C-5）が learnings 機構経由で機能し、Fitness 関数定義・Daemon 制御ロジック・Circuit Breaker が改変対象から除外されていること。
- Phase C-7 完了基準:
  - Orchestrator / Planner の runtime が claude-code のみに fail-closed で制限され、config validation と launcher の両方で non-claude-code 指定が拒否されること。Worker は codex / gemini で起動可能だが、`run_on_main` タスクが非 claude worker に割り当てられないことが `RequireClaudeRuntime` / `validateRunOnMainPreflight` で保証されること。全エージェント間通信が Daemon 経由の非同期通信で行われ、Worker 間直接通信が発生しないこと。
- Phase C-8 完了基準（**部分達成**）:
  - 機能プロファイル（4段階）が定義され、プロファイル適用失敗時に Simple へフォールバックすること。**（達成。`featuregate/evaluator.go`）**
  - **未達**: Planner による複雑度の意味的判断、Orchestrator からの明示オーバーライド、per-task の実ゲーティングは現行実装で未接続（複雑度は Daemon が機械算出し telemetry ログのみ。[§5 C-8](#c-8-planner-による適応的機能制御feature-gate) 基本要件注記参照）。これらを満たすには別途実装が必要。
