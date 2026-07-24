---
name: worker-skill-selection
description: Planner が Worker task の skill_refs を選定するためのガイド (デフォルト最大 3 件、config.yaml で上書き可)
version: "1.2.0"
tags: [planner, worker-skills, skill-selection, task-design]
priority: 10
---

# Worker Skill Selection

Planner が Worker に注入する `skill_refs` を選定するためのガイド。Worker skill はタスク固有の専門知識であり、共有スキルは自動注入されるため `skill_refs` に含めない。

## 1. 基本ルール

1. タスク分解前に `maestro skill list --role worker` で利用可能な Worker skill を確認する。
2. `skill_refs` の上限は `config.yaml` の `skills.max_refs_per_task` に従う（デフォルト 3 件）。必要性が弱い skill は入れない。
3. 迷う場合は `skill_refs` を省略する。persona / content / acceptance_criteria で足りるなら skill は不要。
4. 同じ役割の skill を重複指定しない。品質評価は既存の `evaluation-driven-quality` を使い、別の review skill を作らない。
5. `verify.enabled: false` 運用（通常運用モード）でコード変更を伴うタスクには、上限の範囲内で `evidence-bound-verification` の付与を優先的に検討する。daemon が検証を実行しないため、Worker self-verify の耐久証跡が品質担保の唯一の監査可能物になる。pure research / `run_on_main` / `run_on_integration` のタスクでは skill 側の軽量 tier（summary inline）が適用されるため、付与は任意。

## 2. 選定マトリクス

| タスク種別                                      | 推奨 skill_refs                                                      |
| ----------------------------------------------- | -------------------------------------------------------------------- |
| 構造・依存・schema・外部仕様の調査              | `source-grounded-response`, `semantic-code-search`                   |
| 既存パターンに沿った実装                        | `semantic-code-search`, `constraint-aware-implementation`            |
| 既存命名・スタイル・実装パターンへの厳格な準拠  | `example-driven-pattern-learning`, `constraint-aware-implementation` |
| 振る舞い変更・バグ修正                          | `tdd-red-green-refactor`, `semantic-test-generation`                 |
| テスト設計・品質検証                            | `semantic-test-generation`, `evaluation-driven-quality`              |
| ビルド・テスト失敗の原因分析                    | `error-diagnosis-patterns`, `resilient-execution`                    |
| テスト失敗の修正・CI 緑化・例外処理の実装       | `code-craft-anti-patterns`, `error-diagnosis-patterns`               |
| 複数モジュール跨ぎのシグネチャ変更・リファクタ  | `code-structure-discipline`, `semantic-code-search`                  |
| codebase health check・技術的負債の棚卸し       | `tech-debt-audit`, `source-grounded-response`                        |
| JSON/YAML/schema 出力                           | `structured-output-lifecycle`                                        |
| 複数 CLI / tool を順序制御する作業              | `tool-loop-orchestrator`, `idempotent-tool-execution`                |
| Web UI / ブラウザ E2E 検証                      | `webapp-testing`                                                     |
| ドキュメント・CHANGELOG・リリースノート         | `docs-changelog-release-notes`, `source-grounded-response`           |
| Web UI の実装・変更 (画面・コンポーネント)      | `frontend-ui-implementation`, `webapp-testing`                       |
| ログ・メトリクス・トレース・アラートの実装      | `observability`, `constraint-aware-implementation`                   |
| 性能改善・性能要件達成・性能劣化調査            | `performance-optimization`, `error-diagnosis-patterns`               |
| 公開 API・モジュール境界・型契約の設計/変更     | `api-design`, `code-structure-discipline`                            |
| CI/CD パイプライン・quality gate の構築/改修    | `ci-cd-pipelines`, `idempotent-tool-execution`                       |
| リリース手順・ロールアウト/rollback 計画        | `shipping-and-release`, `docs-changelog-release-notes`               |
| verify 無効運用でのコード変更（耐久証跡が担保） | `evidence-bound-verification`, `evaluation-driven-quality`           |

## 3. 選ばない判断

以下では原則 `skill_refs` を省略する:

- 1 ファイルの小さな文言修正
- acceptance_criteria が単純で、既存パターン探索や追加検証が不要
- persona だけで十分に行動が決まる
- 共有スキル（context / self-evaluation / communication）で足りる

## 4. 出力時チェック

各 Worker task について確認する:

- `skill_refs` は実在する Worker skill 名だけか
- `skills.max_refs_per_task` の上限を超えていないか（デフォルト 3 件）
- `content` に skill を使う目的が自然に含まれているか
- `acceptance_criteria` が skill の成果を検証できる形になっているか
