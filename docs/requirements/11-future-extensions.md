# 11. 将来の拡張ポイント

## Phase C 観測段階機能（インフラ先行実装・gate off が正常運用）

[REQUIREMENTS.md](REQUIREMENTS.md) §5 の Phase C 機能のうち、**C-1（進化的コード品質改善）と C-4（探索的実装最適化）は、エンジン/木構造インフラが実装済みだが意思決定への配線がなく、観測（observability）段階にとどまる**。これらは feature gate（既定 off）下にあり、**gate off での非発火が正常運用**である。「実装されているのに動かない未完成の必須機能」ではなく、将来の実 dispatch 接続に備えてインフラを先行実装し、現状は telemetry のみを駆動する設計である点に留意する。

以下に C-1 / C-4 の具体的機能（[SHOULD]/[MAY] 中心）を将来拡張要件として集約する。§5 の対応節は概要・実装状況・本節への参照に留める。

### C-1 進化的コード品質改善 ― 将来拡張要件

> **現状**: 進化エンジン（`internal/daemon/evolution/engine.go` の `PlanMutations` / `CheckNovelty` / `SelectSurvivors`）は実装済みだが、`recordEvolutionSignal`（`result_learning.go`）がどの変異戦略を選び「うる」かをログ出力するのみ。Worker への diff/full/cross 変異指示の実適用は未配線で、実リトライは既存 retry path が担う。`SelectSurvivors` には production 呼び出し元が存在しない。

- [C-1-1] 変異戦略の多様化: Worker に対し diff（差分パッチ）、full（全体再生成）、cross（複数候補の交叉）の3種の変異パッチ生成を指示可能とする [SHOULD]。変異種別の選択は Planner が過去の Fitness 結果に基づき決定する。
- [C-1-2] 新規性フィルタ: コード埋め込みの類似度（閾値 0.99）により、既存候補と実質同一の変異を棄却する新規性棄却サンプリングを導入する [SHOULD]。冗長な rollout によるリソース浪費を防止する。
- [C-1-3] 適応度評価の拡張: 既存 Fitness 関数（S1-2）の辞書式4軸に「コード品質スコア」軸を追加する [MAY]。品質スコアは静的解析ツール（lint 警告数、循環的複雑度等）の機械的出力に基づき算出し、LLM 判定に依存しない。

**研究対応**:
- ShinkaEvolve: LLM を変異演算子として活用し、diff/full/cross の3種変異パッチを生成。島モデルによる集団管理
- Darwin Gödel Machine (DGM): アーカイブベース選択により、過去の成功パターンを stepping stone として保持

**統合方法**: 既存 worktree を進化島として再利用し、Fitness 関数（S1-2）を適応度関数として活用する。なお B-1 (Multi-rollout) は廃止済みのため、並列候補生成ではなく逐次的な変異・評価サイクルで運用する。

**Anti-Requirements 整合**:
- §6-1 準拠: Fitness 関数は機械的評価のみ。コード品質スコアも静的解析ツール出力に限定し、LLM による上書きを禁止する
- §6-3 準拠: 変異候補の合成（Frankenstein マージ）は行わない。Winner-takes-all 原則を維持する
- §6-6 準拠: verify.yaml が定義済みの場合のみ進化的サイクルを発動する

### C-4 探索的実装最適化 ― 将来拡張要件

> **現状**: 探索木インフラ（`internal/daemon/search/tree.go` の UCT/Prune/SelectBest、`thompson.go` の widen/deepen サンプリング、`phase_c_search.go` の reward backprop）は実装済みだが、Thompson 決定・UCT 計算はログ/報酬蓄積に使われるのみで実 dispatch を駆動しない（「Actual worker assignment is decided before dispatch」、`phase_c_integration.go`）。**例外として C-4-4（機械的 Tie 解消）は実装・稼働済みであり、将来拡張ではなく現行要件として [REQUIREMENTS.md](REQUIREMENTS.md) §5 C-4 に残置する**。

- [C-4-1] 探索木管理: 各探索ノードを worktree 上のチェックポイントとして扱い、Planner が展開（新ノード生成）と枝刈り（低スコアノード打切り）を判断する探索木構造を導入する [MAY]。ノード間の親子関係は Planner が管理し、Worker 間の直接通信は発生しない。
- [C-4-2] Thompson Sampling による探索戦略: 「幅（新アプローチの探索）」と「深さ（既存アプローチの改善）」の選択を Thompson Sampling で確率的に決定する [MAY]。探索の多様性と収束のバランスを自動調整する。
- [C-4-3] Alpha-Beta 枝刈り: Fitness 関数（S1-2）のスコアに基づき、一定閾値を下回るブランチを早期に打ち切る [MAY]。リソース消費を抑制しつつ、有望な候補に計算資源を集中させる。

**研究対応**:
- AB-MCTS: 「Wider or Deeper?」を Thompson Sampling で動的選択。Alpha-Beta 枝刈りによる低ポテンシャルブランチの早期削減
- AI Scientist v2: Progressive Agentic Tree Search による複数実験パスの並列探索

**統合方法**: 単一 worktree 上で逐次的に候補を展開する木構造探索として実装する (B-1 Multi-rollout は廃止のため並列展開ではない)。Planner が探索戦略（展開・枝刈り判断）を管理する。

**Anti-Requirements 整合**:
- §6-4 準拠: 探索ノード（Worker）間の直接通信を禁止する。全てのノード間情報伝達は Planner（Hub）経由の Hub-and-Spoke 型とする
- §6-3 準拠: 探索結果の合成（Frankenstein マージ）を禁止する。最終選択は Winner-takes-all で1つの worktree を丸ごと採用する
- §6-6 準拠: verify.yaml 未定義のタスクには探索的実装を適用しない

## マルチプロバイダー対応（Worker は実装済み、role 拡張は予約）

**実装済み**: Worker は claude-code に加え codex / gemini を本番ランタイムとして起動できる（[REQUIREMENTS.md](REQUIREMENTS.md) §5 C-7）。

- `config.yaml` の `agents.workers.models` でランタイムを指定する。書式はスラッシュ区切りではなく、モデル名そのものへの **exact / prefix マッチ**（`internal/model/runtime.go` の `ParseRuntimeFromModel`）。`codex` / `gemini` で各ランタイムの既定モデル、`codex-5` / `gemini-2.5-pro` のように `codex-` / `gemini-` プレフィックス付きで明示モデルを指定する。`codex/o4-mini` や `o4-mini` のようなプレフィックス無しの値は claude-code にルーティングされ、`model not found` で失敗する
- `maestro agent launch` 内のランタイム分岐（`launchAlternativeWorker`）で各ランタイムのコマンド・フラグ・環境を組み立てる
- ビジー判定は `internal/agent/busy_detector.go` がランタイム差を吸収

**未対応（予約）**: Orchestrator / Planner の非 claude-code 化。これらの role は tool-based role enforcement と PreToolUse hook（claude-code 固有）が必須のため claude-code に **fail-closed で固定**しており（`internal/model/config_validate.go` が config ロード時に codex / gemini モデル指定を拒否）、codex / gemini が同等の role enforcement・control-plane 書き込み制限を提供できるようになるまで解禁しない。

## その他の拡張可能性

> YAML → SQLite 等への移行基準は [§4.10](04-yaml-schema.md) で定義済み。

- Worker 数の動的増減（実行中のスケーリング）
- Web UI ダッシュボード（dashboard.md の可視化）
- Worker の得意分野学習（affinity ルーティング: 過去実績に基づくタスク自動割当）。**スキルレジストリ自体は実装済み**（`internal/daemon/skill/` のレジストリ管理 + `cmd/maestro/cmd_skill.go` の `list` / `candidates` / `approve` / `reject` による candidate 承認フロー）。未実装なのはレジストリを使った affinity 割当のみ
- 副作用の冪等性強化（effect_id + CAS-style atomic publish for git operations）
- `maestro` バイナリの自動アップデート機構
