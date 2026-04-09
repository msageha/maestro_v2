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

———

## 4.5. 実装フェーズ C: 進化的品質最適化と自律改善（Sakana.ai 研究知見）

Phase A-B の基盤が安定稼働した上で、Sakana.ai の研究知見（ShinkaEvolve, Darwin Gödel Machine, AI Scientist, AB-MCTS, Continuous Thought Machines）を統合し、システムの品質最適化能力と自律改善能力を段階的に拡張する。

本フェーズは既存アーキテクチャとの親和性および実装難易度に基づき、以下の3段階で実装する。

- **Phase C-α（既存拡張）**: C-2, C-3 — config/verify.yaml/learnings の拡張で実装可能
- **Phase C-β（新規基盤）**: C-1, C-6 — Multi-rollout 拡張、複雑度推定基盤が必要
- **Phase C-γ（長期研究）**: C-4, C-5 — 探索木管理、自己改善フレームワークが必要

### C-1: Evolutionary Code Quality（進化的コード品質改善）[Phase C-β]

**概要**: Multi-rollout（B-1）を進化的フレームワークに拡張し、変異→評価→選択サイクルによるコード品質の反復改善を実現する。

**研究対応**:
- ShinkaEvolve: LLM を変異演算子として活用し、diff/full/cross の3種変異パッチを生成。島モデルによる集団管理
- Darwin Gödel Machine (DGM): アーカイブベース選択により、過去の成功パターンを stepping stone として保持

**統合方法**: 既存 Multi-rollout 基盤（B-1）を進化的フレームワークとして拡張する。worktree を進化島、Fitness 関数（S1-2）を適応度関数として再利用し、rollout 数を条件付きで増加させる。

**具体的機能**:
- [C-1-1] 変異戦略の多様化: Worker に対し diff（差分パッチ）、full（全体再生成）、cross（複数候補の交叉）の3種の変異パッチ生成を指示可能とする [SHOULD]。変異種別の選択は Planner が過去の Fitness 結果に基づき決定する。
- [C-1-2] 新規性フィルタ: コード埋め込みの類似度（閾値 0.99）により、既存候補と実質同一の変異を棄却する新規性棄却サンプリングを導入する [SHOULD]。冗長な rollout によるリソース浪費を防止する。
- [C-1-3] 適応度評価の拡張: 既存 Fitness 関数（S1-2）の辞書式4軸に「コード品質スコア」軸を追加する [MAY]。品質スコアは静的解析ツール（lint 警告数、循環的複雑度等）の機械的出力に基づき算出し、LLM 判定に依存しない。

**Anti-Requirements 整合**:
- §5-1 準拠: Fitness 関数は機械的評価のみ。コード品質スコアも静的解析ツール出力に限定し、LLM による上書きを禁止する
- §5-3 準拠: 変異候補の合成（Frankenstein マージ）は行わない。Winner-takes-all 原則を維持する
- §5-6 準拠: verify.yaml が定義済みの場合のみ進化的サイクルを発動する

### C-2: Adaptive Model Selection（適応的モデル選択）[Phase C-α]

**概要**: UCB バンディットアルゴリズムによりタスク種別×モデル性能を追跡し、最適モデルを動的に選択する。

**研究対応**:
- ShinkaEvolve: UCB バンディットによる LLM 動的選択。コスト考慮係数による効率的なモデル配分
- AB-MCTS: モデル選択を探索の第3次元として位置付け、複数 LLM の適応的選択を実現

**統合方法**: config.yaml の worker models 設定を動的化する。Trace JSONL（S3-2）に蓄積されたタスク結果データを入力とし、モデル割当を最適化する。§5-7 の「最初からの Bandit アルゴリズム実装禁止」に留意し、**S3 の Trace ログ運用実績が十分に蓄積された後に解禁する** [MUST]。

**具体的機能**:
- [C-2-1] タスク種別別モデル性能トラッキング: タスク種別（bloom_level × persona）ごとに、各モデルの Fitness 結果（Pass率、Repair 回数、実行時間）を Trace JSONL から集計する [SHOULD]。
- [C-2-2] UCB1 スコアに基づくモデル割当: 探索（未知モデルの試行）と活用（高性能モデルの優先）のバランスを UCB1 アルゴリズムで制御する [SHOULD]。
- [C-2-3] コスト考慮係数: モデル選択時に API コスト（トークン単価 × 平均消費量）を考慮し、性能/コスト比を最適化する [MAY]。

**Anti-Requirements 整合**:
- §5-7 準拠: S3 の Trace ログ運用と古典的タグ検索による効果測定が確立された後に初めて導入する。Trace データ不足時は静的な config 設定にフォールバックする [MUST]
- §5-1 準拠: モデル選択の評価指標は機械的 Fitness 結果のみ。LLM の自己評価を選択根拠としない

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
- §5-1 準拠: 検証結果の集約は機械的な重み付き計算のみ。LLM による判定上書きを禁止する
- §5-2 準拠: 多観点検証は多数決ではなく、各検証コマンドの Pass/Fail を独立に評価する
- §5-6 準拠: verify.yaml 未定義タスクへの拡張検証適用を禁止する

### C-4: Exploratory Search（探索的実装最適化）[Phase C-γ]

**概要**: AB-MCTS の探索木構造を worktree 並列実行に適用し、複数の実装候補を木構造で展開して最良解を選択する。

**研究対応**:
- AB-MCTS: 「Wider or Deeper?」を Thompson Sampling で動的選択。Alpha-Beta 枝刈りによる低ポテンシャルブランチの早期削減
- AI Scientist v2: Progressive Agentic Tree Search による複数実験パスの並列探索

**統合方法**: Multi-rollout（B-1）を木構造探索に拡張する。Planner が探索戦略（展開・枝刈り判断）を管理し、各 worktree が探索ノードとして機能する。

**具体的機能**:
- [C-4-1] 探索木管理: 各 worktree を探索ノードとし、Planner が展開（新ノード生成）と枝刈り（低スコアノード打切り）を判断する探索木構造を導入する [MAY]。ノード間の親子関係は Planner が管理し、Worker 間の直接通信は発生しない。
- [C-4-2] Thompson Sampling による探索戦略: 「幅（新アプローチの探索）」と「深さ（既存アプローチの改善）」の選択を Thompson Sampling で確率的に決定する [MAY]。探索の多様性と収束のバランスを自動調整する。
- [C-4-3] Alpha-Beta 枝刈り: Fitness 関数（S1-2）のスコアに基づき、一定閾値を下回るブランチを早期に打ち切る [MAY]。リソース消費を抑制しつつ、有望な候補に計算資源を集中させる。
- [C-4-4] Judge（B-2）との連携: 探索木の最終候補選択において、機械的 Fitness が Tie の場合に限り Judge 機構を Tie-breaker として活用する [MUST]。B-2 の制約を継承する。

**Anti-Requirements 整合**:
- §5-4 準拠: 探索ノード（Worker）間の直接通信を禁止する。全てのノード間情報伝達は Planner（Hub）経由の Hub-and-Spoke 型とする
- §5-3 準拠: 探索結果の合成（Frankenstein マージ）を禁止する。最終選択は Winner-takes-all で1つの worktree を丸ごと採用する
- §5-6 準拠: verify.yaml 未定義のタスクには探索的実装を適用しない

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
- Reward hacking 防止（DGM の教訓）: 自己改善が評価指標自体を操作するリスクに対し、Fitness 関数の機械的評価原則（§5-1）を安全弁として維持する [MUST]。Fitness 関数の定義・重み・閾値は自己改善の対象外とする [MUST]。
- 改善対象の限定: 自己改善対象は Planner/Worker のプロンプト・設定・ペルソナ定義のみとする。Daemon 制御ロジック、状態遷移定義、Fitness 関数定義は改変禁止とする [MUST]。

**Anti-Requirements 整合**:
- §5-1 準拠: Fitness 関数の評価ロジック自体は自己改善の対象外。LLM が Fitness 判定を上書きすることを禁止する
- §5-5 準拠: 自己改善で得た知見は learnings 機構経由で伝達する。共有可変状態を導入しない
- §5-7 準拠: S3 の Trace ログ運用実績が蓄積された後に段階的に解禁する

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
- §5-1 準拠: 複雑度推定および確信度判定は機械的メトリクスに基づく。LLM の主観的判断に依存しない
- §5-7 準拠: 複雑度推定の入力データは Trace JSONL（S3-2）の蓄積データを使用する。十分なデータ蓄積前はデフォルトの分割粒度にフォールバックする [MUST]

### C-7 マルチコーディングエージェントランタイム

Worker の実行バックエンドとして claude code（デフォルト）に加え、codex および gemini を動的に起動・選択する機能を導入する。

#### 基本要件

1. **デフォルトランタイム**: 明示的な指定がない場合、Worker は常に claude code で実行される [MUST]。
2. **タスク単位のエージェント指定**: Planner はタスク生成時に `runtime`（claude-code / codex / gemini）および `model` を指定できる [SHOULD]。未指定時はランタイムの `default_model` が適用される [MUST]。
3. **動的起動**: Daemon は tmux Worker セッション内でタスクに応じたエージェントプロセスを動的に起動する [MUST]。ランタイム切替時は既存セッションの安全な終了を確認後に新プロセスを起動する [MUST]。
4. **ランタイム無効化**: `enabled: false` の設定により、特定ランタイムの使用を禁止できる [MUST]。無効化されたランタイムを指定するタスクは Planner へ差し戻す [MUST]。
5. **縮退動作**: 指定ランタイムの起動に失敗した場合、S0-2 の Single-worker Fallback に従い claude code へ縮退する [MUST]。

#### config.yaml 設定例

```yaml
agents:
  workers:
    runtimes:
      claude-code:
        enabled: true
        default: true
        models: [sonnet, opus, haiku]
        default_model: sonnet
      codex:
        enabled: true
        models: [o3, o4-mini, o4-mini-high]
        default_model: o4-mini
      gemini:
        enabled: true
        models: [gemini-2.5-pro, gemini-2.5-flash]
        default_model: gemini-2.5-flash
```

#### ユースケース

- **MCTS 探索的実装最適化（C-4 統合）**: 複数 worktree で異なるエージェントが同一タスクの実装を並列探索し、Fitness 関数で最適解を選定する [MAY]。
- **クロスエージェントレビュー**: 実装エージェントと異なるエージェントがレビュー・検証を担当し、異なる LLM バイアスの相互補完により品質を向上させる [SHOULD]。
    - §5-4 との整合性: クロスエージェントレビューは Worker 間の直接通信ではなく、Planner 経由の DAG 依存関係として実現する [MUST]。
- **適応的モデル選択（C-2）統合**: UCB バンディットの選択空間を「ランタイム × モデル」の2次元に拡張し、タスク特性に応じた最適な組み合わせを学習する [SHOULD]。
- **修正時の別エージェント担当**: Repair タスクにおいて、元の実装エージェントと異なるエージェントを割り当て、同一 LLM のバイアスによる修正失敗を回避する [MAY]。

#### Anti-Requirements 整合性

- **§5-4（Worker 間直接通信禁止）**: 全てのエージェント間連携は Daemon 経由の非同期通信で行い、Worker 間の直接通信は発生しない [MUST]。
- **§5-5（共有可変状態禁止）**: 各エージェントは独立した worktree で動作し、共有可変状態を持たない [MUST]。認識の同期は Planner によるインターフェースの先行定義と ReadOnly 参照で解決する。
- **§5-3（Frankenstein マージ禁止）**: 異なるエージェントの出力を LLM が合成することは禁止し、Winner-takes-all を維持する [MUST NOT]。

#### タスクスキーマ拡張

§2.2 の最小データスキーマに以下のフィールドを追加する。既存フィールドとの後方互換性を維持する [MUST]。

```yaml
task:
  id: string
  title: string
  blocked_by: [task_id]
  expected_paths: [path_prefix]
  definition_of_done: [string]
  definition_of_abort:
    max_repair_count: integer
    max_wall_clock_sec: integer
    explicit_failure_conditions: [string]
  # Phase C 拡張フィールド
  runtime: string            # claude-code | codex | gemini（未指定時: claude-code）[MAY]
  model: string              # ランタイム固有のモデル名（未指定時: ランタイムの default_model）[MAY]
  complexity_level: string   # simple | standard | complex | critical（未指定時: Planner が自動判定）[MAY]
```

- `runtime` および `model` は任意フィールド [MAY] とし、未指定時はデフォルト値が適用される [MUST]。
- `complexity_level` は任意フィールド [MAY] とし、Planner が自動判定するが Orchestrator からの明示オーバーライドも受け付ける [MUST]。
- 全拡張フィールドが未指定の場合、既存の Phase A-B 動作と完全に同一となる [MUST]。

### C-8 Planner による適応的機能制御（Feature Gate）

Phase C の各機能およびマルチエージェント機能を、Planner がタスク複雑度に応じて動的に On/Off 制御する仕組みを導入する。

#### 基本要件

1. **複雑度評価**: Planner はコマンド受付時にタスク複雑度を評価し、`complexity_level` を決定する [MUST]。
2. **複雑度レベル別機能プロファイル**: 4段階の複雑度レベルに対応する機能プロファイルを定義する [MUST]。
    - **Simple**: 基本実行のみ（claude code + デフォルトモデル、Phase C 機能は全て無効）
    - **Standard**: C-3（自律的検証改善ループ）有効、クロスエージェントレビュー任意
    - **Complex**: C-1, C-2, C-4 有効、マルチエージェント並列探索を積極活用
    - **Critical**: 全機能フル稼働（C-5, C-6 含む）
3. **判断根拠の透明性**: Planner は複雑度判定の根拠を planner.yaml に記録する [MUST]。
4. **オーバーライド**: Orchestrator は `complexity_level` を明示的に指定して Planner の自動判定を上書きできる [MUST]。
5. **タスク単位の粒度**: 同一コマンド内の異なるタスクに対して、それぞれ異なるプロファイルを適用できる [MUST]。
6. **縮退動作**: 機能プロファイルの適用に失敗した場合、Simple プロファイルへフォールバックする [MUST]。

#### config.yaml 設定例

```yaml
feature_profiles:
  simple:
    cross_agent_review: false
    exploratory_optimization: false
    evolutionary_quality: false
    adaptive_model_selection: false
  standard:
    cross_agent_review: optional
    exploratory_optimization: false
    evolutionary_quality: false
    adaptive_model_selection: true
  complex:
    cross_agent_review: true
    exploratory_optimization: true
    evolutionary_quality: true
    adaptive_model_selection: true
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

| 評価軸 | Simple | Standard | Complex | Critical |
|--------|--------|----------|---------|----------|
| 影響範囲（変更ファイル数予測） | 1-3 ファイル | 4-10 ファイル | 11-30 ファイル | 30+ ファイル |
| 依存関係の複雑さ | 単一モジュール内 | モジュール間参照あり | 複数モジュール横断 | システム全体に波及 |
| アーキテクチャ変更 | なし | 軽微な拡張 | 新規パターン導入 | 基盤設計変更 |
| テスト戦略 | 既存テスト修正のみ | 新規テスト追加 | 統合テスト必要 | E2E テスト必要 |
| 既存コードとの統合難易度 | 追記のみ | 軽微なリファクタリング | API 変更を伴う | 後方互換性考慮必要 |

#### Anti-Requirements 整合性

- **§5-1（LLM による Fitness 判定上書き禁止）**: 複雑度評価は Planner の計画判断であり、Fitness 関数自体の変更は行わない [MUST NOT]。Feature Gate は「どの機能を有効化するか」を制御するのみであり、S1-2 の機械的評価基準は不変 [MUST]。
- **§5-7（最初からの Bandit アルゴリズム禁止）**: 適応的モデル選択（C-2）は Standard 以上で有効化されるが、S3 トレースデータ蓄積が前提条件 [MUST]。Feature Gate はこの前提条件の充足を検証した上で機能を有効化する。

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
- Phase C-α 完了基準:
    - 適応的モデル選択（C-2）が稼働し、Trace JSONL のタスク結果データに基づきモデル割当が動的に最適化されていること。デフォルトモデルへの縮退が正常に動作すること。
    - 自律的検証改善ループ（C-3）が verification フェーズに統合され、2回以上の自律的改善サイクル（検証→修正→再検証）を完了できること。検証カバレッジの拡大方向でのみ改善を行い、基準の緩和が発生しないこと。
- Phase C-β 完了基準:
    - 進化的コード品質改善（C-1）が Multi-rollout（B-1）と統合され、変異→評価→選択の進化的サイクルが機能すること。S1-2 の Fitness 関数による機械的選定が維持されていること。
    - Fitness 関数（S1-2）にコード品質スコア軸が追加され、静的解析ツール出力に基づく機械的評価が稼働していること。
    - 適応的計算深度（C-6）により、bloom_level に応じたサブタスク分割粒度の動的調整が機能すること。definition_of_abort の閾値を超えないこと。
- Phase C-γ 完了基準:
    - 探索木管理（C-4）が worktree と統合され、Planner による展開・枝刈り判断に基づく木構造探索が機能すること。verify.yaml を持つタスクのみを対象とし、worktree 単位の独立評価で Winner-takes-all が機能すること。
    - 自己改善メカニズム（C-5）が learnings 機構経由で機能し、Fitness 関数定義・Daemon 制御ロジック・Circuit Breaker が改変対象から除外されていること。
- Phase C-7 完了基準:
    - 複数ランタイム（claude code / codex / gemini）の動的起動が機能し、指定ランタイム障害時に claude code への縮退が正常に動作すること。全エージェント間通信が Daemon 経由の非同期通信で行われ、Worker 間直接通信が発生しないこと。
- Phase C-8 完了基準:
    - Planner が複雑度レベルに応じた機能プロファイルを正しく適用し、Orchestrator からの明示オーバーライドが機能すること。プロファイル適用失敗時に Simple へのフォールバックが正常に動作すること。
