# Maestro v2

Claude Code の複数インスタンスを tmux 上で協調動作させるマルチエージェントオーケストレーションシステム。

## 本質: なぜ Maestro が必要か

Claude Code は単体でも強力だが、大規模タスクでは**認知負荷の分散**と**並列実行**が必要になる。Maestro は 1 つのユーザー指示を、責務の異なる 3 層の Agent に分解・委譲し、複数の Claude Code インスタンスが互いに干渉せず並列作業できる環境を提供する。

核心は「**Agent ごとにコンテキスト（PERSONA / SKILLS / INSTRUCTIONS）を組み立て、最小権限で渡す**」こと。これにより各 Agent は自分の役割だけに集中でき、スコープ逸脱や相互干渉を構造的に防ぐ。

---

## アーキテクチャ

```
User
  ↓ 指示
Orchestrator (opus)          … ユーザーの意図をコマンドに構造化
  ↓ maestro queue write planner --type command --skill-refs <skills>
Planner (opus)               … コマンドをタスクに分解、Worker への配分を設計
  ↓ maestro plan submit --tasks-file
Daemon                       … タスクキュー管理・ディスパッチ・Worktree 管理
  ↓ タスク配信 (persona + skills + learnings を注入)
Workers (sonnet/opus × N)    … タスク実行・結果報告
  ↓ maestro result write --status completed
Daemon → Planner → Orchestrator → User
```

各 Agent は tmux の独立ペインで動作する Claude Code インスタンス。Agent 間の通信はすべて Daemon 経由の CLI コマンド（Unix Domain Socket）で行われ、直接通信は禁止されている。

---

## Agent へのコンテキスト注入

Maestro の設計の核心は、**各 Agent にどのコンテキストを、どのタイミングで、どう注入するか**にある。

### コンテキストの 4 要素

| 要素 | 何を定義するか | いつ注入されるか | 誰が決めるか |
|------|---------------|-----------------|-------------|
| **INSTRUCTIONS** | Agent の行動規則・ツール制限・禁止事項 | Agent 起動時（静的） | システム固定 |
| **PERSONA** | 作業視点・思考様式（実装者/設計者/QA/調査者） | タスク配信時（動的） | Planner が `persona_hint` で指定 |
| **SKILLS** | 再利用可能な知識・フレームワーク | タスク配信時（動的） | Planner が `skill_refs` で指定 + 共有スキルは自動注入 |
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

タスク配信時 (Daemon → Worker):
  ┌─────────────────────────────────────┐
  │ ペルソナ: {persona_hint}.md の内容    │  ← Planner が選択
  │ スキル: skill_refs[] の内容           │  ← Planner が選択 (最大3件)
  │ 共有スキル: skills/share/* の全内容    │  ← 自動注入
  │ [有効時] 学習知見: 最新 K 件           │  ← 自動注入
  │ タスク本文 (content, acceptance_criteria, constraints) │
  └─────────────────────────────────────┘
    ↓ envelope として Worker の stdin に配信
```

### Planner はどう選択するか

Planner は Five Questions Analysis（What / Who / Order / Risk / Verify）を内部で行い、以下を決定する:

**PERSONA の選択基準**:
| persona_hint | 割り当てる場面 |
|---|---|
| `implementer` | コード実装・修正・設定変更・ドキュメント作成 |
| `architect` | 設計判断・構造変更の方針策定 |
| `quality-assurance` | テスト作成・レビュー・セキュリティ分析 |
| `researcher` | コードベース調査・技術調査・選択肢比較 |

**SKILLS の選択基準**:
- Planner は `maestro skill list --role worker` で利用可能なスキルを確認
- タスクの性質に合ったスキルを 1〜3 件 `skill_refs` に指定
- 例: 制約の多い実装 → `constraint-aware-implementation`、テスト生成 → `semantic-test-generation`

**Bloom Level → モデル選択**:
| レベル | 認知的複雑さ | モデル |
|--------|-------------|--------|
| L1-L3 | 記憶・理解・応用（定型的な変更、既知パターンの適用） | Sonnet |
| L4-L6 | 分析・評価・創造（設計判断、新規アーキテクチャ） | Opus |

`boost: true` 設定時は全 Worker が Opus になる。

### Orchestrator のスキル選択

Orchestrator もユーザーの指示を Planner に委譲する際、Planner 向けスキルを選択する:

| ユーザーの指示の性質 | 選択するスキル |
|---|---|
| 新機能実装 | `breakdown-plan` + `create-implementation-plan` |
| 大規模・多層変更 | `breakdown-epic-arch` + `breakdown-plan` |
| 要件が曖昧 | `prd` + `breakdown-plan` |
| セキュリティ関連 | `security-threat-model` + `breakdown-plan` |

---

## ロール別の権限設計

各ロールは最小権限の原則で設計されている。

| | Orchestrator | Planner | Worker |
|---|---|---|---|
| **使えるツール** | `Bash`(maestro CLI のみ), `Read`(.maestro/ のみ) | 同左 | 全ツール |
| **コードの読み書き** | 不可 | 不可 | 可能（タスクスコープ内） |
| **自分でタスク実行** | 不可 | 不可 | 自身の担当タスクのみ |
| **他 Agent への直接通信** | 不可 | 不可 | 不可 |
| **git push** | 不可 | 不可 | 不可 |
| **.maestro/ 制御面** | Read のみ | Read のみ | Read 不可（一部例外） |

Worker は全ツールが使えるが、`.maestro/` の制御面（state/queues/results/locks/logs/config.yaml）への読み書きは禁止されている。

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

## クイックスタート

```bash
# 前提: Go 1.26+, tmux, Claude Code CLI (claude)

# インストール
git clone https://github.com/msageha/maestro_v2.git
cd maestro_v2
./install.sh

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
  config.yaml              # エージェント・スキル・worktree 等の設定
  maestro.md               # 全 Agent 共通のシステムルール
  instructions/            # ロール別の行動規則
    orchestrator.md
    planner.md
    worker.md
  persona/                 # Worker のペルソナ定義
    implementer.md / architect.md / quality-assurance.md / researcher.md
  skills/                  # 再利用可能な知識テンプレート
    orchestrator/           # Orchestrator 用スキル
    planner/                # Planner 用スキル (タスク分解フレームワーク等)
    worker/                 # Worker 用スキル (実装・テスト・調査手法等)
    share/                  # 全 Agent 自動注入の共有スキル
  queue/                   # タスクキュー (YAML)
  results/                 # 実行結果
  state/                   # Daemon 管理の状態ファイル
  worktrees/               # git worktree の作業ディレクトリ

templates/                 # maestro setup のテンプレート元
internal/                  # Go ソースコード
  agent/                   # Agent 起動・システムプロンプト組立・envelope 構築
  daemon/                  # Daemon (キュー管理・ディスパッチ・persona/skill 注入)
  formation/               # tmux セッション構築・モデル割当
  plan/                    # Bloom Level → モデル選択・Worker 割当
  ...
```
