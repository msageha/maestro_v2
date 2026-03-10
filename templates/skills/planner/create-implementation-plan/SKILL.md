---
name: create-implementation-plan
description: AI-to-AI通信に最適化された実装計画作成フレームワーク
version: "1.0.0"
applies_to: ["planner"]
tags: [planner, planning, implementation]
priority: 10
---

# Create Implementation Plan

## 1. Phases 構造

| フェーズ種別 | 説明 | 用途 |
|------------|------|------|
| **concrete** | 詳細確定済み | 即時実行可能なタスク |
| **deferred** | 前フェーズ結果に依存 | 前フェーズ完了後に具体化 |

- 最初のフェーズは常に `concrete`
- `deferred` には「具体化の条件」を明記
- フェーズ内タスクは可能な限り並列化

## 2. Tasks 構造

```yaml
purpose: "<動詞>: <対象> - <意図>"
content: |
  1. ファイルパスと変更内容
  2. 使用する関数・型・API名
  3. 参照すべき既存コード
acceptance_criteria: |
  - Given <前提>, When <操作>, Then <期待結果>
persona_hint: implementer
```

## 3. acceptance_criteria 品質基準（Given-When-Then）

| 要素 | ルール |
|------|-------|
| **Given** | 前提条件・初期状態を明記 |
| **When** | 操作・トリガーを具体的に |
| **Then** | 検証可能な期待結果 |

曖昧表現を排除し、コマンド1つで検証可能にする。依存タスクには blocked_by を設定。

## 4. 並列実行判断

- ファイル変更が独立 → 並列可
- 型定義→使用側 → 直列化
- 異なるパッケージ → 並列可
- 同一ファイル → 直列化

## 5. bloom_level 連携

- **1-2**: 既存パターン再現 → `implementer`
- **3-4**: 応用・分析 → `implementer`/`researcher`
- **5-6**: 評価・創造 → `architect`/`researcher`
