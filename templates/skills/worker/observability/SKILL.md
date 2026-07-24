---
name: observability
description: 構造化ログ・メトリクス・トレース・アラートを実装コードに組み込む規律 — event 設計、シークレット非混入、相関 ID 伝播、SLO 起点のアラート条件を検証可能な形で実装する Worker 向けガイド
version: "1.0.0"
tags: [worker, observability, logging, metrics, tracing]
priority: 20
---

# Observability

サービス・バッチ・CLI に**観測性 (構造化ログ / メトリクス / トレース / アラート) を実装する**タスクで使う。「動くコード」で終わらせず、「壊れたときに原因へ辿り着けるコード」を完了条件にする。

## 1. 適用条件と棲み分け

適用するのは、観測性の**実装・改修**を含むタスク (新規エンドポイント、ジョブ、ログ整備、metrics 追加、alert 定義など)。

| 本 skill が扱わないもの                | 委ね先                                                           |
| -------------------------------------- | ---------------------------------------------------------------- |
| 発生済みエラーの原因分析・デバッグ手順 | `error-diagnosis-patterns` (本 skill は「診断可能にする」実装側) |
| リトライ・エスカレーションの実行時戦略 | `resilient-execution`                                            |
| 性能ボトルネックの計測・改善           | `performance-optimization` (本 skill は常設の計測基盤を作る側)   |
| JSON/YAML 出力自体のスキーマ検証       | `structured-output-lifecycle`                                    |

ツールは**プロジェクトの既存スタックを最優先**する (既存の logger / metrics client / tracer をまず探す)。OpenTelemetry・Prometheus 等の導入はタスクが明示的に要求した場合のみ。既存基盤も導入許可も無い場合は、**標準ライブラリの構造化ログ (key-value / JSON) まで degrade** し、metrics/trace は「未整備」として `[注意事項]` に明記する。無断で新規依存を追加しない。

## 2. 構造化ログの規律

1. **既存のログ規約を確認する**: logger の取得方法、フィールド命名 (snake_case / camelCase)、level の使い分けを既存コードから抽出し、それに合わせる。
2. **イベントは機械可読に**: メッセージ文字列への変数埋め込みではなく、固定の event 名 + 構造化フィールドで書く (例: `event=payment_failed order_id=... reason=...`)。grep とログ基盤の両方で集計できる形にする。
3. **level の判断基準**: ERROR = 人の対応が必要 / WARN = 自動回復したが異常兆候 / INFO = 状態遷移の記録 / DEBUG = 開発時のみ。「とりあえず ERROR」「全部 INFO」にしない。
4. **相関 ID を伝播する**: request ID / trace ID / job ID を受け取り、境界 (HTTP handler、queue consumer、goroutine/thread 起点) で必ずログフィールドに載せる。境界を越えて消える相関 ID は無いのと同じ。
5. **シークレット・PII を混入させない**: token、password、API key、個人情報はログに出さない。構造体を丸ごと dump するコード (`%+v`、`JSON.stringify(obj)` 等) は、含まれるフィールドを確認してから使う。
6. **エラーは文脈付きで 1 回だけ**: 同じエラーを各層で重複ログしない。原則、処理を打ち切る層で文脈 (何をしようとして、どの入力で) を付けて記録する。

## 3. メトリクスの規律

- **何を測るかを先に決める**: リクエスト駆動なら RED (Rate / Errors / Duration)、リソースなら USE (Utilization / Saturation / Errors) を出発点に、タスクの acceptance_criteria に対応する指標を選ぶ。
- **カーディナリティを制御する**: user ID・URL 全文・エラーメッセージ全文をラベルにしない。ラベルは有限集合 (status class、endpoint 名、queue 名) に限定する。
- **命名は既存規約に従う**: 単位をサフィックスで明示 (`_seconds`, `_bytes`, `_total`)。既存メトリクスと同じ prefix / 命名スタイルを使う。
- counter / gauge / histogram の選択根拠を実装コメントではなく summary に書く (レビュー判断材料になる)。

## 4. トレースの規律 (基盤がある場合)

- span はビジネス的に意味のある単位 (外部呼び出し、DB クエリ、ジョブ処理) で切る。1 行ごとの span は濃度過剰。
- context (trace context) を関数境界・プロセス境界で伝播する。fire-and-forget の非同期処理で親 span を切らない場合は、その判断を記録する。
- span への属性付与もログと同じシークレット規律に従う。

## 5. アラートの規律 (定義を求められた場合)

- アラートは**利用者影響 (SLO 違反・エラー率・レイテンシ)** を起点に定義する。原因側 (CPU 使用率等) は原則ダッシュボード行きで、単独のページ条件にしない。
- 各アラートに「発火時に何をするか」(runbook への参照、確認コマンド) を添える。対応不能なアラートは作らない。
- 閾値は実データ (直近のメトリクス値) を確認して決め、根拠を summary に書く。推測値の場合はその旨を明記する。

## 6. よくある言い訳 (Common Rationalizations)

| 言い訳                                                | なぜ間違いか                                                                                    |
| ----------------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| 「ログは後で整備すればよい。まず機能を動かす」        | 障害は最初のデプロイ直後に起きる。観測性の無い新機能は失敗時に切り分け不能で、結局実装者に戻る  |
| 「printf デバッグ用のログを残しておけば観測性になる」 | 一時ログは event 設計もフィールドも無く集計できない。消すか、規約に沿った構造化ログに昇格させる |
| 「エラーは全部 ERROR で出しておけば安全」             | 自動回復する異常まで ERROR にするとアラートが常時発火し、本物の障害が埋もれる                   |
| 「構造体をそのまま dump した方が情報量が多い」        | フィールドにシークレット・PII が含まれた瞬間にインシデントになる。出すフィールドは明示列挙する  |
| 「メトリクスのラベルに ID を入れた方が調査が楽」      | カーディナリティ爆発で metrics 基盤自体が落ちる。ID での調査はログ・トレースの責務              |

## 7. 危険信号 (Red Flags)

- 既存の logger / metrics client を確認する前に、新しい観測ライブラリを import しようとしている
- `fmt.Println` / `console.log` / `print` を production path に追加している
- エラーを catch した後、ログだけ出して握りつぶしている (観測性はエラーハンドリングの代替ではない)
- ログ出力行に token・password・Authorization ヘッダらしき変数が渡っている
- 相関 ID を受け取る引数・context を境界で捨てている

## 8. 完了チェックリスト (Verification)

- [ ] 追加・変更したログを実際に 1 回発火させ、出力 (フィールド・level・相関 ID) を確認した — 出力例を summary に貼る
- [ ] ログ・span 属性にシークレット / PII が無いことを、出力フィールドの列挙で確認した
- [ ] メトリクスを追加した場合、ラベルのカーディナリティが有限集合であることを確認した
- [ ] 既存の観測規約 (logger、命名、level) に合わせたこと、または規約が無く degrade した旨を summary に明記した
- [ ] アラートを定義した場合、閾値の根拠と発火時アクションを記載した

---

着想元: [addyosmani/agent-skills](https://github.com/addyosmani/agent-skills) の `observability-and-instrumentation` (MIT License)。Maestro worker 文脈 (worktree 隔離・self-verify・構造化 summary) に合わせて再構成。
