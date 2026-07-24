# 3. ファイル構成

## リポジトリ構造

> **注**: 実装はパッケージ数・ファイル数とも下記より大幅に多い（`internal/` 直下 23 パッケージ、`internal/daemon/` 配下に 20 超のサブパッケージ）。本節は責務の地図であり、ファイルの完全列挙ではない。詳細は各パッケージの doc.go を参照。

```
maestro/
├── mise.toml                          # ビルド・インストール（check-deps + go build + install タスク。旧 install.sh / Makefile の代替）
├── cmd/
│   └── maestro/
│       ├── main.go                    # エントリポイント（サブコマンドルーティング）
│       └── cmd_*.go                   # 各サブコマンド（setup/up/down/status/hud/queue/result/plan/agent/worker/skill/persona/verify/task/dashboard）
├── internal/
│   ├── daemon/                        # 常駐プロセス本体（最大の複雑度を持つ）
│   │   ├── daemon.go                  # デーモンプロセス（UDS + メインループ + PhaseCManager 配線）
│   │   ├── queue_handler.go           # queue/ 変更検知 → Agent 配信
│   │   ├── result_*.go                # results/ 変更検知 → 通知 + 2 フェーズロック書き込み
│   │   ├── queue_scan_phase_*.go      # 定期スキャン（Phase A/B/C、20 超の step 関数）
│   │   ├── dead_letter.go             # max_attempts 超過エントリの DLQ 処理
│   │   ├── dashboard_gen 相当         # dashboard.md + metrics.yaml 生成
│   │   ├── reconcile/                 # R0, R0b, R1-R10 不整合修復エンジン
│   │   ├── worktree/                  # git worktree 隔離・integration マージ・publish・GC（最大規模のサブパッケージ）
│   │   ├── verification/, verify_*    # verify パイプライン（実行・stall 回復）
│   │   ├── lease/                     # lease + lease_epoch フェンシング管理
│   │   ├── admission/                 # 同時 verify/repair 数の Admission Control（S0-1）
│   │   ├── circuitbreaker/            # Circuit Breaker（S2-2）
│   │   ├── dispatch/                  # 配信前 quality gate / run_on_main 検証
│   │   ├── bandit/                    # C-2 UCB1 適応的モデル選択
│   │   ├── evolution/, search/        # C-1 進化的品質 / C-4 探索（feature gate でゲート）
│   │   ├── featuregate/, complexity/  # C-8 Feature Gate / C-6 複雑度推定
│   │   ├── learnings/                 # C-5 Failure Fingerprint DB / 学習知見
│   │   ├── reviewer/                  # A-1 非同期レビュアー（Advisory）
│   │   ├── persona/, skill/           # persona / skill 解決
│   │   └── paneactivity/              # ペイン活性・blocked-pane 検知
│   ├── agent/                         # launcher（Claude/Codex/Gemini 起動）+ executor（send-keys + ビジー判定 + /clear）+ policy hook
│   ├── plan/                          # plan submit/complete/add-retry-task（DAG 検証 + cascade recovery + Worker 自動割当）
│   ├── tmux/                          # tmux セッション・ペイン管理 + per-instance socket 分離
│   ├── formation/                     # up/down/attach のフォーメーション統括
│   ├── model/                         # 全 YAML 型定義 + config + status enum（状態機械の正規形）+ fitness（S1-2）
│   ├── quality/                       # Fitness / quality engine（RuleEvaluator）
│   ├── worker/                        # Worker 側ヘルパ・契約
│   ├── bridge/, contract/, envelope/  # メッセージエンベロープ・契約境界
│   ├── events/, metrics/, status/     # Trace JSONL（S3-2）/ メトリクス / 状態表示
│   ├── hud/                           # 読み取り専用 TUI 観測 HUD（`maestro hud`。collector + snapshot diff + 純関数レンダラ）
│   ├── uds/                           # Unix ドメインソケットプロトコル（length-prefix + JSON）
│   ├── yaml/                          # アトミック書き込み + schema_version 検証 + quarantine
│   ├── lock/                          # sync.Mutex ベースの排他制御（MutexMap）
│   ├── validate/, pathutil/, ptr/, clock/, testutil/  # 補助
│   └── setup/                         # .maestro/ ディレクトリ初期化
├── templates/
│   ├── config.yaml                    # プロジェクト設定テンプレート
│   ├── maestro.md                     # 全エージェント共通システムプロンプト
│   ├── dashboard.md                   # ダッシュボードテンプレート
│   ├── instructions/                  # orchestrator.md / planner.md / worker.md 指令書
│   ├── persona/                       # architect / implementer / researcher / quality-assurance / sweeper
│   └── skills/                        # share / planner / worker / orchestrator 別の skill 定義群
├── docker/                            # コンテナ実行環境
├── audit/                             # 監査・E2E レポート類
├── go.mod / go.sum                    # Go モジュール定義 + 依存ハッシュ
└── docs/
    └── requirements/                  # 本要件定義書一式（abstract / 01-11 / REQUIREMENTS.md）
```

## タスク証跡ディレクトリ（`audit/evidence/` — 対象プロジェクトスコープ）

Worker の self-verify 観測証跡（`evidence-bound-verification` skill）の規約パス。`.maestro/` 配下ではなく、**対象プロジェクト直下の `audit/evidence/<task_id>.md`** に置く。

- **経路**: Worker が自分の worktree 内に書く → daemon が verbatim commit（`git add -A`）→ integration merge → publish、と**タスク成果物と同じ経路で main に耐久化**される。`.maestro/` 配下は Worker から書き込み不可（制御プレーン）かつ gitignore されるため置き場所にならない
- **gate ではない**: daemon は証跡ファイルの存在を検査しない。daemon が実機実行する唯一の Strong Signal は verify.yaml（[§4](04-yaml-schema.md) / [§5.16](05-script-responsibilities.md)）であり、証跡は `verify.enabled: false`（通常運用モード）の self-verify を監査可能にする prose 規律である。result entry の `--summary`（`証跡: audit/evidence/<task_id>.md`）と `--files-changed` から辿る
- **例外**: `run_on_main` タスク（Write/Edit 拒否）と `run_on_integration` タスク（integration worktree は merge 前に `reset --hard` + `clean -fd` で auto-clean され生成物が残らない）は証跡ファイルを作らず、観測を result summary に inline 記載する
- **gitignore との関係**: daemon の auto_commit は gitignore を尊重する。対象プロジェクトが `audit/` を ignore していると証跡は commit されないため、Worker は書き込み前に `git check-ignore` で確認し、ignored なら summary inline に切り替える（本リポジトリはホワイトリスト方式の `.gitignore` で `!/audit/**` により追跡対象）
- **保持/サイズ**: 1 タスク 1 ファイル、目安 100 行 / 8 KiB 以内（コマンド原文と exit code は省略禁止、出力は判定に効く行のみ抜粋）。保持の正本は git 履歴であり、working tree 上の蓄積は操作員が定期 prune してよい（削除しても履歴から辿れる）

## プロジェクト初期化後の .maestro/ 構造

```
.maestro/
├── config.yaml                        # このプロジェクトの設定
├── maestro.md                         # 共通システムプロンプト
├── dashboard.md                       # デーモンが自動生成するダッシュボード
├── daemon.sock                        # CLI ↔ デーモンの Unix ドメインソケット（デーモン稼働中のみ）
├── instructions/
│   ├── orchestrator.md
│   ├── planner.md
│   └── worker.md
├── persona/                           # persona 定義（setup でテンプレートから配置・上書き）
├── skills/                            # skill 定義（setup でテンプレートから配置・上書き）
├── worktrees/                         # git worktree の実体チェックアウト（既定 .maestro/worktrees。config の worktree.path_prefix）
│   └── {command_id}/{worker}/         # worker / integration ごとの作業ツリー。state/worktrees/ の「状態 YAML」とは別物（こちらは実ファイルツリー）
├── cache/                             # デーモン・Agent 用キャッシュ
│   └── tmp/                           # Agent 起動時の TMPDIR（実行時生成。macOS sandbox 互換のため inherited 値を無条件で上書き）
├── queue/
│   ├── planner.yaml                   # Orchestrator → Planner（コマンド）
│   ├── planner_signals.yaml           # デーモン → Planner（構造化シグナル: フェーズ完了 / commit 失敗 / merge conflict。[§4.6.1](04-yaml-schema.md) 参照）
│   ├── orchestrator.yaml              # Planner 完了通知 → Orchestrator（通知）
│   ├── worker1.yaml                   # Planner → Worker1（タスク）
│   ├── worker2.yaml
│   └── ...                            # worker{N}.yaml（Worker 数分）
├── results/
│   ├── planner.yaml                   # Planner → Orchestrator（コマンド実行結果）
│   ├── worker1.yaml                   # Worker1 → Planner（タスク実行結果）
│   ├── worker2.yaml
│   └── ...                            # worker{N}.yaml（Worker 数分）
├── state/
│   ├── commands/
│   │   ├── cmd_1771722000_a3f2b7c19d4e5f60.yaml   # command 単位の完了条件と task 状態の正本
│   │   └── ...
│   ├── worktrees/                     # command 単位の worktree / integration の「状態 YAML」（デーモンが実行時に管理）。git の実体チェックアウトはトップレベルの .maestro/worktrees/ に置かれる
│   │   └── {command_id}.yaml
│   ├── verify/                        # command-scoped verify config snapshot（S1-1）。デーモンが実行時に管理し `verify write` が登録する（setup は事前作成しない）
│   │   └── {command_id}.yaml
│   ├── continuous.yaml                # 継続モード状態（イテレーションカウンタ等。デーモンが管理）
│   ├── metrics.yaml                   # 可観測性メトリクス（デーモンが更新）
│   ├── fingerprint_db.json            # C-5 Failure Fingerprint DB（self_improvement.enabled 時のみ。デーモンが管理）
│   ├── improvements.yaml              # C-5 friction 駆動改善ループの improvement idea 台帳（self_improvement.friction.enabled 時のみ。デーモンが管理、[§4.12](04-yaml-schema.md) 参照）
│   └── hud_history.jsonl              # `maestro hud` の snapshot 差分トレイル（JSONL・HUD プロセスのみが書き、デーモンは読み書きしない。[§5.19](05-script-responsibilities.md) 参照）
├── locks/                             # ロックファイル
│   └── daemon.lock                    # デーモン単一インスタンス保証（syscall.Flock）
│                                      # per-agent / per-command の排他制御はデーモンプロセス内の
│                                      # sync.Mutex で行うため、ファイルロックは daemon.lock のみ
├── dead_letters/                      # max_attempts 超過エントリの保存先
│   └── {filename}.{timestamp}.yaml
├── quarantine/                        # 破損 YAML の隔離先
│   └── {filename}.{timestamp}.corrupt
└── logs/
    ├── daemon.log                     # デーモンプロセスのログ
    ├── daemon_startup.log             # デーモン子プロセスの stdout/stderr 捕捉（構造化ロガー配線前の early-return エラーを観測可能にする）
    ├── tmux_debug.log                 # tmux コマンド実行のデバッグログ
    └── agent_executor.log             # Agent 配信のログ
```
