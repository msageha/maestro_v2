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
| 依存（必須） | tmux, jq, Claude Code CLI (`claude`) |
| ランタイム | Orchestrator / Planner は claude-code 必須。Worker は codex / gemini も可（ただし run_on_main タスクは claude-code worker のみに割当・dispatch される） |

### アーキテクチャ（一言）

```
User → Orchestrator(opus) → Planner(opus) → Daemon → Workers(sonnet/opus × N)
                                                ↑
                            キュー管理・Worktree・スキル注入・品質検証
```

---

## クイックスタート

```bash
# 前提: Go 1.26+, tmux, jq, Claude Code CLI (claude)

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

## ディレクトリ構成（`.maestro/`）

`maestro setup` が生成するプロジェクト直下のランタイム状態。

```
.maestro/
  config.yaml              # エージェント・機能フラグ・worktree 等の設定
  maestro.md               # 全 Agent 共通のシステムルール
  dashboard.md             # リアルタイムダッシュボード
  instructions/            # ロール別の行動規則 (orchestrator/planner/worker)
  persona/                 # Worker のペルソナ定義 (implementer/architect/qa/researcher)
  skills/                  # 再利用可能な知識テンプレート (orchestrator/planner/worker/share)
  queue/                   # タスクキュー（YAML）
  results/                 # 実行結果
  state/                   # Daemon 管理の状態ファイル
  locks/ logs/             # ロック・tmux デバッグログ
  dead_letters/            # リトライ上限超過キュー項目のアーカイブ
  quarantine/              # 破損 YAML ファイルの退避
  worktrees/               # git worktree の作業ディレクトリ
```

---

## Agent の 3 層構造

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

### ロール別の権限

| | Orchestrator | Planner | Worker |
|---|---|---|---|
| **使えるツール** | `Bash`(maestro CLI のみ), `Read`(限定) | `Bash`(maestro CLI のみ), `Read`(.maestro/\*\*) | 全ツール（PreToolUse hook で動的制御） |
| **コードの読み書き** | 不可 | 不可 | 可（タスクスコープ内） |
| **git push** | 不可 | 不可 | 不可（commit/merge は Daemon が管理） |

`.maestro/` の制御面（state/queue/results/locks/logs/config.yaml）は Worker から書き換えられない。

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

Daemon は以下のモジュールを備え、`feature_profiles` でタスク複雑度別に有効化される（デフォルトプロファイル: simple/standard/complex/critical）。

| 機能 | 概要 | 実装 |
|------|------|------|
| 適応的モデル選択 | UCB1 バンディットで実績ベースにモデルを選択 | `internal/daemon/bandit/` |
| 適応的計算深度 | 複雑度スコアでサブタスク分解深度を自動決定 | `internal/daemon/complexity/` |
| クロスエージェントレビュー | 別モデルによる非同期コードレビュー | `internal/daemon/reviewer/` |
| 進化的コード品質 | 変異戦略 (diff/full/cross) と新規性チェック | `internal/daemon/evolution/` |
| 探索的最適化 | MCTS / Thompson Sampling で実装アプローチを探索 | `internal/daemon/search/` |
| 自己改善 | エラー Fingerprint DB で失敗パターンを学習 | `internal/daemon/learnings/` |
| アンサンブル検証 | build/lint/test/typecheck を重み付きで集約 | `internal/daemon/verification/` |
| 状態修復エンジン | R0–R10 のルールベースで stall を自動復旧 | `internal/daemon/reconcile/` |

### コマンド単位の検証設定（verify write）

Worker のタスク完了後に走らせる検証コマンドを `.maestro/verify.yaml`（または `maestro verify write --command-id <id> --config-file <path>`）で宣言する。`build / lint / test / typecheck` および任意の `security / performance` にカテゴリ分けする。

- 各コマンドは `bash -c` 経由で実行される。`&&` / `||` / `;` / `|` / リダイレクト / `$(...)` / env 代入 (`KEY=val cmd`) などの通常の shell 構文をそのまま書いてよい。複数手順を 1 つの category に並べたい場合も `cmd1 && cmd2` で連結できる。
- 唯一の構文制約は「YAML スカラ 1 行に収まること」（改行 / キャリッジリターンのみ reject される）。
- カテゴリは `build`, `lint`, `test`, `typecheck`, `security`, `performance` の 6 つ。これ以外のキー（例: `slow_lint`, `integration_test`）を書くと strict YAML decode で reject され "allowed categories: ..." エラーになる。
- shell の論理否定 `!` は使わない。`! rg ...` は JSON/YAML 経由で `\!` にエスケープされ `bash -c` で `\!: command not found` (exit 127) を踏む。代わりに `if rg <pattern>; then exit 1; fi` や `rg -q <pattern> && exit 1 || exit 0` で書く。
- 検証失敗時は repair タスクをスケジュールし、修復成功なら lineage-aware な `DeriveStatus` で plan 完了扱いになる。

---

## 設定リファレンス（主要項目）

モデル名は短縮エイリアス（`opus` / `sonnet` / `haiku`）と完全 ID（`claude-opus-4-7[1m]` 等）の両方を許容する。`maestro setup` が生成する config はデフォルトで完全 ID を書き出す。

```yaml
agents:
  orchestrator: {model: "claude-opus-4-7[1m]"}
  planner:      {model: "claude-opus-4-7[1m]"}
  workers:
    count: 4
    default_model: "claude-sonnet-4-6[1m]"
    models:                  # worker_id → モデル名の個別オーバーライド
      worker3: "claude-opus-4-7[1m]"
      worker4: "claude-opus-4-7[1m]"
    boost: false              # true で全 Worker が Opus

worktree:
  enabled: true
  base_branch: "main"
  auto_commit: true
  auto_merge: true
  cleanup_on_success: true   # 成功時はクリーンアップ、失敗時は保全

verify:
  enabled: true              # daemon 所有の検証ランナー
  stall_threshold_sec: 600   # verify_pending stall 時の R9 復旧しきい値

extended_verification:
  enabled: true              # アンサンブル検証 (build/lint/test/typecheck/...)
  max_auto_retries: 2

bandit:        {enabled: false, exploration_coefficient: 1.41, min_samples_before_use: 10}
evolution:     {enabled: false, novelty_threshold: 0.99}
complexity:    {enabled: false}
review:        {enabled: false, models: [], min_bloom_level: 2}

feature_profiles:
  simple:   {adaptive_model_selection: false, adaptive_depth: false, cross_agent_review: false,
             evolutionary_quality: false, exploratory_optimization: false, self_improvement: false}
  standard: {adaptive_model_selection: true,  adaptive_depth: false, cross_agent_review: false,
             evolutionary_quality: false, exploratory_optimization: false, self_improvement: false}
  complex:  {adaptive_model_selection: true,  adaptive_depth: true,  cross_agent_review: true,
             evolutionary_quality: false, exploratory_optimization: false, self_improvement: false}
  critical: {adaptive_model_selection: true,  adaptive_depth: true,  cross_agent_review: true,
             evolutionary_quality: true,  exploratory_optimization: true,  self_improvement: true}
```

全項目は `templates/config.yaml` にコメント付きで列挙されている。

---

## 開発

```bash
make build        # ビルド
make test         # テスト実行
make test-race    # レースコンディション検出付きテスト
make test-cover   # カバレッジレポート生成（coverage.html）
make lint         # golangci-lint 実行
make lint-fix     # 自動修正付き lint
make format       # gofmt + goimports
make install      # check-deps → ビルド → ($MAESTRO_INSTALL_DIR > ~/bin > /usr/local/bin) へ配置
make check-deps   # tmux / go / claude / jq の存在チェック
make clean        # 成果物削除
```

### Docker 開発環境

`docker/Dockerfile` は mise 管理の言語ランタイム + Claude Code / Codex / Gemini CLI + `maestro` バイナリを同梱した Ubuntu 24.04 イメージを定義する。任意のリポジトリをホストと同じパスにマウントして検証用途に使える。

```bash
make docker-build                            # イメージをビルド
make docker-run   DIR=/path/to/repo          # maestro を起動
make docker-shell DIR=/path/to/repo          # bash を起動
```
