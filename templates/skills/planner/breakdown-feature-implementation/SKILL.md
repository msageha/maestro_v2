---
name: breakdown-feature-implementation
description: 機能要件を実装タスクに分解するフレームワーク
version: "1.0.0"
applies_to: ["planner"]
tags: [planner, feature, implementation]
priority: 10
---

# Breakdown Feature Implementation

## 1. レイヤーごとのタスク分割

| レイヤー | 典型的なタスク | persona_hint |
|---------|-------------|-------------|
| DB/データ層 | スキーマ、モデル | `implementer` |
| Backend/API層 | ハンドラ、ロジック | `implementer` |
| Frontend/UI層 | コンポーネント | `implementer` |
| Infra/設定層 | CI/CD、設定 | `implementer` |
| テスト層 | テスト作成・実行 | `quality-assurance` |
| 調査 | 影響範囲、設計判断 | `researcher`/`architect` |

## 2. タスク YAML マッピング

```yaml
- purpose: "<レイヤー>: <変更の意図>"
  content: |
    具体的な実装指示（ファイルパス、関数名、変更内容）
  acceptance_criteria: |
    Given-When-Then 形式の検証可能な完了条件
  persona_hint: implementer
  blocked_by: [依存先タスクインデックス]
```

## 3. persona_hint 割り当て基準

| 判断基準 | 割り当て |
|---------|---------|
| コード変更が主目的 | `implementer` |
| テスト・品質検証が主目的 | `quality-assurance` |
| 情報収集・分析が主目的 | `researcher` |
| 設計判断が主目的 | `architect` |

## 4. blocked_by 設計（ファイル競合回避）

- 同一ファイル書き込みタスク間は `blocked_by` で直列化必須
- 読み取りのみのタスクは並列実行可能
- レイヤー間依存（DB→Backend→Frontend）は自然な直列チェーン
- 共通型定義に依存する場合は型定義タスクを先行させる
- 1タスクの変更は **5ファイル以下**、テストは実装と分離する
