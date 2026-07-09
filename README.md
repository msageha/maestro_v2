# Maestro v2

Claude Code の複数インスタンスを tmux 上で協調させるマルチエージェントオーケストレーションシステム。
大規模タスクを Orchestrator → Planner → Workers の 3 層に分解し、各 Agent が独立した git worktree で並列作業する。

---

## 概要

| 項目         | 内容                                                                                                                                                    |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 言語         | Go 1.26+                                                                                                                                                |
| 通信         | Unix Domain Socket（CLI 経由）                                                                                                                          |
| 隔離         | git worktree（Worker ごと独立）                                                                                                                         |
| 設定         | `.maestro/config.yaml`（YAML）                                                                                                                          |
| 依存（必須） | tmux, jq, Claude Code CLI (`claude`)                                                                                                                    |
| ランタイム   | Orchestrator / Planner は claude-code 必須。Worker は codex / gemini も可（ただし run_on_main タスクは claude-code worker のみに割当・dispatch される） |

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
mise run install      # 依存チェック → ビルド → ~/bin 等へインストール

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
  persona/                 # Worker のペルソナ定義 (implementer/architect/qa/researcher/sweeper)
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

|                      | Orchestrator                           | Planner                                         | Worker                                 |
| -------------------- | -------------------------------------- | ----------------------------------------------- | -------------------------------------- |
| **使えるツール**     | `Bash`(maestro CLI のみ), `Read`(限定) | `Bash`(maestro CLI のみ), `Read`(.maestro/\*\*) | 全ツール（PreToolUse hook で動的制御） |
| **コードの読み書き** | 不可                                   | 不可                                            | 可（タスクスコープ内）                 |
| **git push**         | 不可                                   | 不可                                            | 不可（commit/merge は Daemon が管理）  |

`.maestro/` の制御面（state/queue/results/locks/logs/config.yaml）は Worker から書き換えられない。

> **信頼境界に関する注意**: ロール識別（`MAESTRO_AGENT_ROLE` / tmux pane の `@role`）は **advisory hint であり認証ではない**。UDS ソケットは mode 0600 のため接続できるのは同一 UNIX ユーザーのプロセスのみで、その境界内でのロール分離は best-effort（誠実なエージェントの誤操作防止が目的）である。同一ユーザーの敵対的プロセスに対する防御は提供しない。マルチユーザー環境では OS レベルのユーザー分離を併用すること（詳細: `internal/uds/protocol.go` の SECURITY NOTE）。

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

Worker は変更系の git 操作（commit/push/merge 等）を一切行わない。すべて Daemon が管理する（`git status` / `git diff` / `git show` 等の読み取り専用コマンドは確認用途で許可される）。

---

## 高度な機能

### 稼働している機能（実際の判断・復旧を駆動する）

| 機能                 | 概要                                                                           | 実装                                    |
| -------------------- | ------------------------------------------------------------------------------ | --------------------------------------- |
| 状態修復エンジン     | R0–R10 のルールベースで stall を自動復旧                                       | `internal/daemon/reconcile/`            |
| Lease / Fencing      | dispatch 毎の lease_epoch で stale な heartbeat・結果報告を拒否                | `internal/daemon/lease/`                |
| Worktree 分離        | Worker ごとの worktree 作成・マージ・publish・競合復旧を Daemon が管理         | `internal/daemon/worktree/`             |
| verify.yaml 検証     | `run_on_integration` / `run_on_main` タスク完了時に verify snapshot を実機実行 | `internal/daemon/verify_runner_real.go` |
| スキル・ペルソナ注入 | タスク/コマンド配信時に persona・skill_refs・共有スキルを本文へ注入            | `internal/daemon/dispatch/`, `skill/`   |
| Learnings 注入       | `learnings.enabled` 時、タスク配信時に直近の学習知見 top-K を注入              | `internal/daemon/learnings/`            |

### 観測段階（テレメトリのみ）の機能

以下のモジュールはインフラ実装済みだが、**計測・記録はするものの自動判断への反映（配線）は未実装**である。enabled にしても挙動は変わらないか、限定的にしか変わらない。詳細は `docs/requirements/REQUIREMENTS.md` §5 と `docs/requirements/11-future-extensions.md`（Phase C 観測段階機能）を参照。

| 機能                       | 概要                                            | 実装                            | 現状                                                                                                                                                                                                                                                                                                     |
| -------------------------- | ----------------------------------------------- | ------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 適応的モデル選択           | UCB1 バンディットで実績ベースにモデルを選択     | `internal/daemon/bandit/`       | **稼働**: arm 統計は `state/bandit_state.json` に永続化され warm-up が再起動をまたぐ。arm はモデルファミリ（sonnet/opus/haiku）単位で、報酬帰属もファミリ正規化される。`feature_profiles` の `adaptive_model_selection` が複雑度レベル単位で選択を gate する（off のレベルは静的マッピング、学習は継続） |
| 適応的計算深度             | 複雑度スコアを機械算出                          | `internal/daemon/complexity/`   | dispatch 時に複雑度を算出・ログするのみ。分解深度や機能 on/off には未接続                                                                                                                                                                                                                                |
| クロスエージェントレビュー | 別モデルによる非同期コードレビュー              | `internal/daemon/reviewer/`     | findings は `.maestro/state/reviews/<task_id>.yaml` に監査ログとして保存されるのみで、Planner / Worker の判断には自動反映されない                                                                                                                                                                        |
| 進化的コード品質           | 変異戦略 (diff/full/cross) と新規性チェック     | `internal/daemon/evolution/`    | 選択されうる変異戦略のログ出力のみ。変異の実適用・生存選択（`SelectSurvivors`）は未配線                                                                                                                                                                                                                  |
| 探索的最適化               | MCTS / Thompson Sampling で実装アプローチを探索 | `internal/daemon/search/`       | 報酬蓄積・ログのみで dispatch を駆動しない（例外: A/B 選抜の機械的 tie 解消 C-4-4 は稼働）                                                                                                                                                                                                               |
| 自己改善                   | エラー Fingerprint DB で失敗パターンを学習      | `internal/daemon/learnings/`    | **稼働**: 失敗時に fingerprint を記録し、その retry が成功すると成功率と修復アプローチ (retry の summary) をパターンに刻む。以後、同種失敗の retry タスク配信時に検証済み修復戦略が DATA セクションとして注入される (`SuggestStrategy` は修復成功実績のあるパターンのみ提案)                             |
| アンサンブル検証           | build/lint/test/typecheck を重み付きで集約      | `internal/daemon/verification/` | verify runner に接続されているが perspective を投入する経路が無く常に空。verify.yaml のカテゴリはすべて critical として実行される                                                                                                                                                                        |

`feature_profiles`（simple/standard/complex/critical）はタスク単位の複雑度プロファイルを算出・ログする。現時点で実ゲーティングに接続されているのは `adaptive_model_selection` のみ（レベル単位でバンディット選択を on/off。Bloom レベルから導出した複雑度レベルで判定し、off のレベルでは静的マッピングにフォールバックする。報酬の学習は gate に関係なく継続）。その他の機能（cross_agent_review 等）の発火は引き続き config のグローバル `enabled` フラグでのみ決まる。

### コマンド単位の検証設定（verify write）

コマンド単位の検証コマンドを `.maestro/verify.yaml`（または `maestro verify write --command-id <id> --config-file <path>`）で宣言する。`build / lint / test / typecheck` および任意の `security / performance` にカテゴリ分けする。

- 実行には `verify.enabled: true` が必要（テンプレートデフォルトは `false`。false の場合、検証ランナーは no-op で snapshot を書いても実行されない）。
- Daemon が verify.yaml を実行するのは **`run_on_integration: true` / `run_on_main: true` のタスク完了時のみ**。通常の worker タスク完了直後には実行されない（worker worktree は gitignored な依存キャッシュを持たず言語ツールが動かないため。Worker はタスク内で self-verification を行う）。pre-publish の機械的検証は、最終 phase に `run_on_integration: true` の検証専用タスクを配置して実現する。
- 各コマンドは `bash -c` 経由で実行される。`&&` / `||` / `;` / `|` / リダイレクト / `$(...)` / env 代入 (`KEY=val cmd`) などの通常の shell 構文をそのまま書いてよい。複数手順を 1 つの category に並べたい場合も `cmd1 && cmd2` で連結できる。
- 唯一の構文制約は「YAML スカラ 1 行に収まること」（改行 / キャリッジリターンのみ reject される）。
- カテゴリは `build`, `lint`, `test`, `typecheck`, `security`, `performance` の 6 つ。これ以外のキー（例: `slow_lint`, `integration_test`）を書くと strict YAML decode で reject され "allowed categories: ..." エラーになる。
- shell の論理否定 `!` は使わない。`! rg ...` は JSON/YAML 経由で `\!` にエスケープされ `bash -c` で `\!: command not found` (exit 127) を踏む。代わりに `if rg <pattern>; then exit 1; fi` や `rg -q <pattern> && exit 1 || exit 0` で書く。
- 検証失敗時は repair タスクをスケジュールし、修復成功なら lineage-aware な `DeriveStatus` で plan 完了扱いになる。

---

## 設定リファレンス（主要項目）

モデル名は短縮エイリアス（`opus` / `sonnet` / `haiku`）と完全 ID（`claude-opus-4-8[1m]` 等）の両方を許容する。`maestro setup` が生成する config はデフォルトで完全 ID を書き出す。

```yaml
agents:
  orchestrator: { model: "claude-opus-4-8[1m]" }
  planner: { model: "claude-opus-4-8[1m]" }
  workers:
    count: 4
    default_model: "claude-sonnet-4-6[1m]"
    models: # worker_id → モデル名の個別オーバーライド
      worker3: "claude-opus-4-8[1m]"
      worker4: "claude-opus-4-8[1m]"
    boost: false # true で全 Worker が Opus

worktree:
  enabled: true
  base_branch: "main"
  auto_commit: true
  auto_merge: true
  cleanup_on_success: true # 成功時はクリーンアップ、失敗時は保全

verify:
  enabled: false # daemon 所有の検証ランナー（テンプレートデフォルトは false。ソフトウェア開発で使う場合のみ true にする）
  stall_threshold_sec: 600 # verify_pending stall 時の R9 復旧しきい値

extended_verification:
  enabled: false # アンサンブル検証。現状 perspective 未投入で実質不活性（「高度な機能」参照）

bandit: {
  enabled: false,
  exploration_coefficient: 1.41,
  min_samples_before_use: 10,
}
evolution: { enabled: false, novelty_threshold: 0.99 }
complexity: { enabled: false }
review: { enabled: false, models: [], min_bloom_level: 2 }

# feature_profiles は現状テレメトリのみ（プロファイルを書き換えても機能 on/off は変わらない。「高度な機能」参照）
feature_profiles:
  simple: {
    adaptive_model_selection: false,
    adaptive_depth: false,
    cross_agent_review: false,
    evolutionary_quality: false,
    exploratory_optimization: false,
    self_improvement: false,
  }
  standard: {
    adaptive_model_selection: true,
    adaptive_depth: false,
    cross_agent_review: false,
    evolutionary_quality: false,
    exploratory_optimization: false,
    self_improvement: false,
  }
  complex: {
    adaptive_model_selection: true,
    adaptive_depth: true,
    cross_agent_review: true,
    evolutionary_quality: false,
    exploratory_optimization: false,
    self_improvement: false,
  }
  critical: {
    adaptive_model_selection: true,
    adaptive_depth: true,
    cross_agent_review: true,
    evolutionary_quality: true,
    exploratory_optimization: true,
    self_improvement: true,
  }
```

全項目は `templates/config.yaml` にコメント付きで列挙されている。

---

## 開発環境セットアップ

このリポジトリは [mise](https://mise.jdx.dev/) の利用を前提としています。

```sh
mise trust    # 初回のみ: このディレクトリの mise.toml を信頼する
mise install  # tools (go, golangci-lint, prek, actionlint) をインストールし、pre-commit hook をセットアップする
```

`mise install` を実行すると `[hooks] postinstall` により `prek install` が自動実行され、
`.git/hooks/pre-commit` がセットアップされる。

シェル起動時に mise の PATH・環境変数を自動反映させるため、シェルの rc ファイルに以下を追加する。

```sh
# ~/.zshrc (bash の場合は `mise activate bash`)
eval "$(mise activate zsh)"
```

### タスク

タスクは `mise run <task>` で実行する。`mise.toml` の `[tasks]` を変更した場合は
`mise run docs` を実行し、以下の一覧を更新する (pre-commit hook からも自動実行される)。

<!-- dprint-ignore-start -->
<!-- mise-tasks -->
## `all`

- Depends: lint, test, build

- **Usage**: `all`

lint → test → build を実行

## `build`

- **Usage**: `build`

Go バイナリをビルド

## `check-deps`

- **Usage**: `check-deps`

tmux/go/claude/jq の依存チェック

## `clean`

- **Usage**: `clean`

ビルド成果物を削除

## `docker-build`

- **Usage**: `docker-build`

Docker 開発環境イメージをビルド

## `docker-run`

- **Usage**: `docker-run`

指定ディレクトリで maestro を起動 (DIR=/path/to/repo mise run docker-run)

## `docker-shell`

- **Usage**: `docker-shell`

指定ディレクトリで bash を起動 (DIR=/path/to/repo mise run docker-shell)

## `docs`

- **Usage**: `docs`

Sync the task list embedded in README.md with mise.toml

## `format`

- **Usage**: `format`

gofmt + goimports でフォーマット

## `install`

- Depends: check-deps, build

- **Usage**: `install`

ビルドしてインストール（MAESTRO_INSTALL_DIR > ~/bin > /usr/local/bin）

## `lint`

- **Usage**: `lint`

golangci-lint を実行

## `lint-fix`

- **Usage**: `lint-fix`

golangci-lint の自動修正を適用

## `test`

- **Usage**: `test`

全テストを実行

## `test-cover`

- **Usage**: `test-cover`

カバレッジレポートを生成

## `test-race`

- **Usage**: `test-race`

Race detector 付きでテスト

## `test-v`

- **Usage**: `test-v`

全テストを verbose で実行
<!-- /mise-tasks -->
<!-- dprint-ignore-end -->

---

## 開発

タスクはすべて `mise run <task>` で実行する（一覧は `mise tasks` でも確認できる）。

```bash
mise run build        # ビルド
mise run test         # テスト実行
mise run test-race    # レースコンディション検出付きテスト
mise run test-cover   # カバレッジレポート生成（coverage.html）
mise run lint         # golangci-lint 実行
mise run lint-fix     # 自動修正付き lint
mise run format       # gofmt + goimports
mise run install      # check-deps → ビルド → ($MAESTRO_INSTALL_DIR > ~/bin > /usr/local/bin) へ配置
mise run check-deps   # tmux / go / claude / jq の存在チェック
mise run clean        # 成果物削除
```

### Docker 開発環境

`docker/Dockerfile` は mise 管理の言語ランタイム + Claude Code / Codex / Gemini CLI + `maestro` バイナリを同梱した Ubuntu 24.04 イメージを定義する。任意のリポジトリをホストと同じパスにマウントして検証用途に使える。

```bash
mise run docker-build                        # イメージをビルド
DIR=/path/to/repo mise run docker-run        # maestro を起動
DIR=/path/to/repo mise run docker-shell      # bash を起動
```
