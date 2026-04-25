# Maestro v2

Claude Code の複数インスタンスを tmux 上で協調させるマルチエージェントオーケストレーションシステム。  
大規模タスクを Orchestrator → Planner → Workers の 3 層に分解し、各 Agent が独立した git worktree で並列作業する。

---

## 概要

| 項目 | 内容 |
|------|------|
| 言語 | Go 1.26+ |
| 通信 | Unix Domain Socket（CLI 経由） |
| 隔離 | git worktree（Worker ごと独立） |
| 設定 | `.maestro/config.yaml`（YAML） |
| 依存（必須） | tmux, Claude Code CLI (`claude`) |
| 依存（任意） | Codex CLI (`codex`), Gemini CLI (`gemini`)　※マルチランタイム使用時 |

### アーキテクチャ（一言）

```
User → Orchestrator(opus) → Planner(opus) → Daemon → Workers(sonnet/opus × N)
                                                ↑
                            キュー管理・Worktree・スキル注入・品質検証
```

---

## クイックスタート

```bash
# 前提: Go 1.26+, tmux, Claude Code CLI (claude)
# マルチランタイムを使う場合は追加で codex / gemini CLI も必要

# インストール
git clone https://github.com/msageha/maestro_v2.git
cd maestro_v2
make install          # 依存チェック → ビルド → ~/bin 等へインストール

# プロジェクトで初期化
cd /path/to/your-project
maestro setup .

# フォーメーション起動
maestro up

# tmux セッションにアタッチして Orchestrator に指示
tmux attach -t maestro-<project-name>
# ウィンドウ: 0=Orchestrator, 1=Planner, 2=Workers

# 終了
maestro down
```

---

## ディレクトリ構成

```
.maestro/                  # 実行時状態（maestro setup で生成）
  config.yaml              # エージェント・機能フラグ・worktree 等の設定
  maestro.md               # 全 Agent 共通のシステムルール
  dashboard.md             # リアルタイムダッシュボード
  instructions/            # ロール別の行動規則
    orchestrator.md / planner.md / worker.md
  persona/                 # Worker のペルソナ定義
    implementer.md / architect.md / quality-assurance.md / researcher.md
  skills/                  # 再利用可能な知識テンプレート
    orchestrator/ planner/ worker/ share/
  queue/                   # タスクキュー（YAML）
  results/                 # 実行結果
  state/                   # Daemon 管理の状態ファイル
  locks/                   # デーモンプロセスロック
  logs/                    # tmux デバッグログ
  dead_letters/            # リトライ上限超過キュー項目のアーカイブ
  quarantine/              # 破損 YAML ファイルの退避
  worktrees/               # git worktree の作業ディレクトリ

internal/
  agent/                   # Agent 起動・envelope 構築・システムプロンプト組立
  daemon/                  # Daemon + 高度化機能モジュール群
    admission/             # 並行性スロット制御
    bandit/                # 適応的モデル選択（UCB1）
    circuitbreaker/        # サーキットブレーカー
    complexity/            # 複雑度スコアリング
    core/                  # 共有型・インターフェース
    evolution/             # 進化的コード品質
    fallback/              # 縮退モード管理
    featuregate/           # 機能ゲート
    judge/                 # LLM ベース結果選択
    learnings/             # エラー学習 DB（Fingerprint）
    persona/               # ペルソナ注入
    reconcile/             # 状態修復エンジン（R0-R6 の7パターン）
    reviewer/              # 非同期コードレビュー
    rollout/               # ロールアウト実行管理
    scheduler/             # タスクスケジューラ
    search/                # 探索的最適化（MCTS / Thompson Sampling）
    skill/                 # スキル注入
    verification/          # アンサンブル検証
    worktree/              # git worktree 管理
  agent/runtime_launcher.go # マルチランタイム対応
  model/                   # 全データモデル・設定構造体
  formation/               # tmux セッション構築・モデル割当
  plan/                    # Bloom Level → モデル選択・Worker 割当
  quality/                 # 品質ゲートエンジン
  uds/                     # Unix Domain Socket 通信

templates/                 # maestro setup のテンプレート元
cmd/maestro/               # CLI エントリーポイント（サブコマンド群）
```

---

## アーキテクチャ詳細

### Agent の 3 層構造

```
User
  ↓ 指示
Orchestrator (opus)          … ユーザーの意図をコマンドに構造化
  ↓ maestro queue write planner --type command --skill-refs <skills>
Planner (opus)               … コマンドをタスクに分解、Worker への配分を設計
  ↓ maestro plan submit --tasks-file
Daemon                       … タスクキュー管理・ディスパッチ・Worktree 管理
  ↓ タスク配信（persona + skills + learnings を注入）
Workers (sonnet/opus × N)    … タスク実行・結果報告
  ↓ maestro result write --status completed
Daemon → Planner → Orchestrator → User
```

各 Agent は tmux の独立ペインで動作する Claude Code インスタンス。  
Agent 間の通信はすべて **Daemon 経由の CLI コマンド（Unix Domain Socket）** で行われ、直接通信は禁止されている。

---

## Agent へのコンテキスト注入

Maestro の設計の核心は、**各 Agent にどのコンテキストを、どのタイミングで、どう注入するか**にある。

### コンテキストの 4 要素

| 要素 | 何を定義するか | いつ注入されるか | 誰が決めるか |
|------|---------------|-----------------|-------------|
| **INSTRUCTIONS** | Agent の行動規則・ツール制限・禁止事項 | Agent 起動時（静的） | システム固定 |
| **PERSONA** | 作業視点・思考様式（実装者/設計者/QA/調査者） | タスク配信時（動的） | Planner が `persona_hint` で指定 |
| **SKILLS** | 再利用可能な知識・フレームワーク | コマンド・タスク配信時（動的） | Orchestrator がコマンド用に、Planner がタスク用に `skill_refs` で指定。Daemon がファイル内容を解決して `content` に注入 |
| **Bloom Level** | タスクの認知的複雑さ → モデル選択 | タスク配信時（動的） | Planner が `bloom_level` で指定 |

### 注入の流れ

```
Agent 起動時 (maestro agent launch):
  ┌─────────────────────────────────────┐
  │ maestro.md        (共通システムルール)  │
  │ instructions/{role}.md (ロール別規則)  │
  │ [Orchestrator のみ] 全スキル一括注入   │
  └─────────────────────────────────────┘
    ↓ --append-system-prompt として Claude CLI に渡す

Orchestrator → Planner (maestro queue write planner):
  ┌─────────────────────────────────────┐
  │ コマンド内容 (content)                │  ← Orchestratorが入力
  │ skill_refs: 参照スキルの名前リスト     │  ← Orchestratorが --skill-refs で指定
  └─────────────────────────────────────┘
    ↓ Daemon がコマンドをキューから取得し、配信前に skill_refs を解決
    ↓ スキルファイルの内容を content 末尾に動的注入（enriched content）
    ↓ Planner には「コマンド内容 + スキル内容」が一体化した content として届く
    ※ Orchestrator は起動時に全スキルを持つため、
       関連スキルを skill_refs で Planner に伝えることができる

タスク配信時 (Daemon → Worker):
  ┌─────────────────────────────────────┐
  │ ペルソナ: {persona_hint}.md の内容    │  ← Planner が選択
  │ スキル: skill_refs[] の内容           │  ← Planner が選択（最大 3 件）
  │ 共有スキル: skills/share/* の全内容    │  ← 自動注入
  │ [有効時] 学習知見: 最新 K 件           │  ← 自動注入
  │ タスク本文 (content, acceptance_criteria, constraints) │
  └─────────────────────────────────────┘
    ↓ envelope として Worker の stdin に配信
```

### PERSONA の選択基準

| persona_hint | 割り当てる場面 |
|---|---|
| `implementer` | コード実装・修正・設定変更・ドキュメント作成 |
| `architect` | 設計判断・構造変更の方針策定 |
| `quality-assurance` | テスト作成・レビュー・セキュリティ分析 |
| `researcher` | コードベース調査・技術調査・選択肢比較 |

### Bloom Level → モデル選択

| レベル | 認知的複雑さ | モデル |
|--------|-------------|--------|
| L1-L3 | 記憶・理解・応用（定型的な変更、既知パターンの適用） | Sonnet |
| L4-L6 | 分析・評価・創造（設計判断、新規アーキテクチャ） | Opus |

`boost: true` 設定時は全 Worker が Opus になる。

---

## ロール別の権限設計

各ロールは最小権限の原則で設計されている。

| | Orchestrator | Planner | Worker |
|---|---|---|---|
| **使えるツール** | `Bash`(maestro CLI のみ), `Read`(dashboard.md, results/\*, config.yaml のみ) | `Bash`(maestro CLI のみ), `Read`(.maestro/\*\* 全体) | 全ツール（制限は PreToolUse hook で動的に制御） |
| **コードの読み書き** | 不可 | 不可 | 可能（タスクスコープ内） |
| **自分でタスク実行** | 不可 | 不可 | 自身の担当タスクのみ |
| **他 Agent への直接通信** | 不可 | 不可 | 不可 |
| **git push** | 不可 | 不可 | 不可 |
| **.maestro/ 制御面** | Read のみ（限定的） | Read のみ（全体） | Read 不可（一部例外） |

Worker は全ツールが使えるが、`.maestro/` の制御面（state/queue/results/locks/logs/config.yaml）への読み書きは禁止されている。

---

## Worktree による並列作業の分離

各 Worker に git worktree で独立したファイルシステムを提供する（デフォルト有効）。

```
コマンド受信
  → integration ブランチ + Worker ごとの worktree 作成
  → Worker が各 worktree 内で独立作業
  → フェーズ境界で Worker ブランチ → integration にマージ
  → 全フェーズ完了後 integration → base ブランチに publish
  → worktree クリーンアップ
```

Worker は git 操作（commit/push/merge 等）を一切行わない。すべて Daemon が管理する。

---

## 高度な機能

Daemon は以下の機能モジュールを備えており、タスクの複雑度に応じて自動的に有効化される。

### 進化的コード品質

生物の自然選択を模倣して、コードを「進化」させる仕組み。

**ファイル:** `internal/daemon/evolution/engine.go`, `internal/model/evolution.go`

#### 変異戦略

| 戦略 | 内容 | 割合 |
|------|------|------|
| `diff` | 差分ベースの小さな変更 | 2/4 |
| `full` | 全体書き直し | 1/4 |
| `cross` | 他コードとの交差（組み合わせ） | 1/4 |

`PlanMutations(parentCount int)` が `diff:full:cross = 2:1:1` の比率でスロットを自動配分する。

#### 新規性チェック（NoveltyCheck）

```
生成コード → SHA-256 ハッシュ計算 → 既存ハッシュ群と完全一致チェック
                                      一致 → 除外（重複排除）
                                      不一致 → IsNovel: true
```

#### 生存者選択（SelectSurvivors）

Fitness スコアで降順ソートし、上位 `maxSurvivors` 件を選択する winner-takes-all 方式。

---

### 適応的モデル選択

「どの LLM モデルが最も効果的か」を実際の結果から自動学習するバンディット問題の実装。

**ファイル:** `internal/daemon/bandit/ucb1.go`, `internal/model/config.go`

#### UCB1 スコア計算

```
score = avg_reward + exploration_coeff × √(ln(総試行数) / このアームの試行数)
```

- 試行回数が少ないモデルほどスコアが高い → 探索を促進
- 良い結果が出たモデルは平均報酬が高い → 活用を促進
- **LLM トークン消費ゼロ**（純粋な数値計算）

#### 動作フロー

1. **探索フェーズ:** `PullCount == 0` のアームを優先して選択
2. **活用フェーズ:** UCB1 スコアが最高のアームを選択
3. **報酬更新:** `UpdateReward(armName, reward)` でインクリメンタル平均更新

#### 設定（BanditConfig）

| パラメータ | デフォルト | 説明 |
|-----------|---------|------|
| `ExplorationCoeff` | 1.41 | 探索・活用トレードオフ係数 |
| `MinSamplesBeforeUse` | 10 | 学習開始前の最低サンプル数 |
| `DecayFactor` | 0.95 | 報酬の時間減衰率 |

データが不足している間（`MinSamplesBeforeUse` 未満）は静的フォールバックを使用する。

---

### 自律検証ループ

複数の「観点」からコードを検証し、重み付き集計で総合判定する。

**ファイル:** `internal/daemon/verification/ensemble.go`, `internal/model/verify.go`

#### 検証観点

| 観点 | 重み |
|------|------|
| build | 1.0 |
| lint | 0.8 |
| test | 1.0 |
| typecheck | 0.9 |
| security | 設定次第 |
| performance | 設定次第 |

#### 集約ロジック

```
TotalScore = Σ(weight × passed) / Σ(weight)
全体 Passed = 全観点が passed AND weight ≥ 1.0 の観点が1つも失敗していない
```

`weight >= 1.0` の観点（build, test）が 1 つでも失敗すると、他が通っていても全体失敗になる。

#### Fingerprint 連携リトライ

```go
ShouldRetry(result, fingerprint, seenFingerprints) bool
```

- 失敗時、エラーの Fingerprint が「新しい種類」→ リトライ推奨
- 同じエラーの繰り返し → 無駄なリトライをしない

---

### 探索的最適化（MCTS）

チェス AI が使うモンテカルロ木探索で「どの実装アプローチを試すか」を探索する。

**ファイル:** `internal/daemon/search/tree.go`, `internal/daemon/search/thompson.go`

#### MCTS Tree

**ノード状態:** `Unexpanded → Expanded → Terminal / Pruned`

**UCT スコア計算（ノード選択）:**

```
UCT = avg_reward + exploration_coeff × √(ln(親の訪問回数) / このノードの訪問回数)
未訪問ノード = +∞（必ず先に探索）
```

**Backpropagate:** 報酬をルートまで逆伝播し、経路上の全ノードの `Visits` と `AvgReward` を更新。

**Prune:** 閾値以下の AvgReward を持つノードを削除し、探索空間を圧縮。

#### Thompson Sampling（幅 vs 深さの決定）

```
Beta(α, β) 分布からサンプリング
  > 0.5 → Widen（横に広げる）
  ≤ 0.5 → Deepen（縦に深める）

成功時: α++ (Widen の実績を強化)
失敗時: β++ (Deepen の実績を強化)
```

乱数生成は Marsaglia-Tsang 法による Gamma 分布サンプリングで実装されている。

---

### 自己改善（エラー学習）

過去の失敗パターンを「Fingerprint DB」に蓄積し、同種のエラーが来たら修復戦略を提案する。

**ファイル:** `internal/daemon/learnings/fingerprint_db.go`, `internal/model/fingerprint.go`

#### エラー正規化フロー

```
生エラー: "Error at line 42: null pointer at 0x7fff..."
    ↓ NormalizeError（行番号・アドレス・タイムスタンプ・絶対パスを除去、小文字化）
正規化: "error: null pointer"
    ↓ ComputeFingerprint（SHA-256）
fingerprint: "a3f5c7..."
```

#### FingerprintDB API

| メソッド | 機能 |
|---------|------|
| `Store(fp, category, strategy)` | 指紋・修復戦略を保存。既存は出現回数を増やす |
| `Query(fp)` | 正確な指紋で検索 |
| `FindSimilar(fp, maxResults)` | プレフィックス共有で類似パターンを検索 |
| `RecordSuccess(fp)` | 修復成功時に成功率を更新 |
| `SuggestStrategy(fp)` | 成功率 > 0 の場合のみ修復戦略を提案 |

容量上限（デフォルト 1000 件）超過時は最古エントリを自動 evict する。

#### 連続失敗トラッカー

`ConsecutiveFailureTracker` が直近 N 件の Fingerprint を記録し、同一エラーが閾値を超えて繰り返される場合に検知する。

---

### 適応的計算深度

タスクの複雑さを機械的に計算し、サブタスク分解深度を自動決定する。

**ファイル:** `internal/daemon/complexity/scorer.go`, `internal/model/complexity.go`

#### 複雑度スコア計算式

```
RawScore = 0.3 × normalize(ファイル数, 50)
         + 0.2 × normalize(依存関係の深さ, 10)
         + 0.2 × normalize(Bloom レベル, 6)
         + 0.2 × 過去の修復率
         + 0.1 × normalize(期待パス数, 20)

Confidence = (非ゼロフィールド数) / 5
```

LLM を一切使わず、純粋な数値計算のみで判定する。

#### 複雑度レベル → 分解深度

| レベル | RawScore 閾値 | 分解深度 | ファイル数目安 |
|--------|-------------|---------|------------|
| Simple | ≤ 0.25 | 1 | 1-3 |
| Standard | ≤ 0.50 | 2 | 4-10 |
| Complex | ≤ 0.75 | 3 | 11-30 |
| Critical | > 0.75 | 4 | 31+ |

---

### マルチランタイム対応

複数の AI コーディングエージェントを切り替えて使える仕組み。

**ファイル:** `internal/agent/runtime_launcher.go`, `internal/model/runtime.go`

#### 対応ランタイム

| Runtime | デフォルト | 有効化方法 |
|---------|----------|----------|
| `claude-code` | ✅ 常に有効 | — |
| `codex` | ❌ | `model: codex` を指定（worker のみ） |
| `gemini` | ❌ | `model: gemini` または `model: gemini-*` を指定（worker のみ） |

#### Orchestrator / Planner ロールの制約

Orchestrator の「委譲のみ」、Planner の「計画のみ」というロール制約は、claude-code の `--allowedTools` / `--disallowedTools` CLI フラグで技術的に強制している。codex と gemini にはこの強制機構が存在せず、プロンプトに依存した運用になるため、過去には codex Orchestrator が main を直接編集する事故が発生した。これを踏まえ、Orchestrator と Planner では codex / gemini を許可しない（config 検証時に拒否する）。Worker は本来ファイル編集を行う役割なので codex / gemini も使用可能。

#### フォールバック動作

指定ランタイムが利用不可・失敗の場合、自動的に `claude-code` にフォールバックする。

```go
FallbackToDefault() (string, []string)
```

---

### 機能ゲート

タスクの複雑度レベルによって、どの機能を有効にするかを自動制御する。

**ファイル:** `internal/daemon/featuregate/evaluator.go`, `internal/model/featuregate.go`

#### デフォルトプロフィール

| レベル | ファイル数 | 有効な機能 |
|--------|-----------|-----------|
| `simple` | 1-3 | なし |
| `standard` | 4-10 | 適応的モデル選択 |
| `complex` | 11-30 | + クロスエージェントレビュー, 適応的計算深度 |
| `critical` | 31+ | + 探索的最適化, 進化的コード品質, 自己改善（全機能有効） |

#### 使い方

```go
evaluator := featuregate.NewEvaluator()
level := evaluator.GetLevel(fileCount, depCount)     // ファイル数から自動判定
enabled := evaluator.IsEnabled(level, featuregate.FeatureEvolutionaryQuality)
```

YAML の `feature_profiles` セクションでデフォルト設定を上書き可能。

---

## 機能の連携フロー

```
タスク受信
    ↓
機能ゲート ──→ 複雑度レベルを評価し、有効な機能セットを決定
    ↓
複雑度スコアリング ──→ RawScore を計算し、サブタスク分解深度を決定
    ↓
MCTS / Thompson Sampling ──→ 探索戦略（幅/深さ）を確率的に選択
    ↓
UCB1 モデル選択 ──→ 最適な LLM モデルをバンディットで選択
    ↓
ランタイム選択 ──→ 実行ランタイムを選択（失敗時は claude-code へフォールバック）
    ↓
進化的コード品質 ──→ 変異戦略（diff/full/cross）でコードを生成・評価
    ↓
アンサンブル検証 ──→ 4 観点(build/lint/test/typecheck)で検証、重み付き集約で合否判定
    │                   ※ security/performance は将来実装予定（config フラグは定義済み）
    ↓
エラー学習 ──→ 失敗パターンを学習・蓄積し、次回以降の修復に活用
```

各コンポーネントは独立したパッケージとして実装されており、`daemon.Daemon` が全体を統合する。  
すべての挙動は `.maestro/config.yaml` から制御できる設計になっている。

---

## 設定リファレンス（主要項目）

```yaml
agents:
  orchestrator: {model: "opus"}
  planner: {model: "opus"}
  workers: {count: 4, default_model: "sonnet", boost: false}

worktree:
  enabled: true
  base_branch: "main"
  auto_commit: true
  auto_merge: true

evolution:
  enabled: true
  novelty_threshold: 0.9

bandit:
  enabled: true
  exploration_coeff: 1.41
  min_samples_before_use: 10

complexity:
  enabled: true

runtimes:
  claude-code: {enabled: true, default: true}
  codex: {enabled: false}
  gemini: {enabled: false}

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

---

## 開発

```bash
make build        # ビルド
make test         # テスト実行
make test-race    # レースコンディション検出付きテスト
make test-cover   # カバレッジレポート生成（coverage.html）
make lint         # golangci-lint 実行
make lint-fix     # 自動修正付き lint
make install      # ビルド + インストール
make clean        # 成果物削除
```
