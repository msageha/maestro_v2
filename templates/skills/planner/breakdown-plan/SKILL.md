---
name: breakdown-plan
description: コマンドをタスクに分解するためのフレームワーク
version: "1.0.0"
applies_to: ["planner"]
tags: [planner, task-design, decomposition]
priority: 10
---

# Breakdown Plan — タスク分解フレームワーク

## タスク階層

コマンドを以下の階層で分解する:

| レベル | 粒度 | 目安 |
|--------|------|------|
| Epic | ユーザー価値単位 | 1コマンド = 1 Epic |
| Feature | 機能単位 | Phase に対応 |
| Task | Worker が1回で完了できる作業単位 | 30分以内 |

## INVEST 基準

各タスクは以下を満たすこと:

- **I**ndependent: 他タスクへの依存を最小化（`blocked_by` で明示）
- **N**egotiable: 実装手段は Worker に委ねる
- **V**aluable: 単体で検証可能な成果物を持つ
- **E**stimable: スコープが明確で見積もり可能
- **S**mall: Worker が1ターンで完了できるサイズ
- **T**estable: `acceptance_criteria` で完了判定可能

## Maestro タスク構造へのマッピング

```yaml
name: 簡潔な作業名（動詞+目的語）
purpose: 全体における役割（他タスクとの関係）
content: 具体的な作業手順（ステップ形式推奨）
acceptance_criteria: 完了条件（検証可能な形式）
blocked_by: [依存タスクID]  # なければ省略
bloom_level: apply | analyze | create
persona_hint: implementer | researcher | architect | quality-assurance
```

## Bloom's Taxonomy 判定

| レベル | 用途 | persona_hint |
|--------|------|-------------|
| remember/understand | 情報収集・調査 | researcher |
| apply | 既知パターンの適用実装 | implementer |
| analyze | 構造分析・影響範囲調査 | researcher / architect |
| evaluate | レビュー・品質検証 | quality-assurance |
| create | 新規設計・アーキテクチャ策定 | architect |

## Worker Persona 割り当て基準

| persona_hint | 用途 |
|---|---|
| researcher | コードベース調査、依存関係分析、技術調査 |
| architect | 設計判断、API設計、データモデル策定 |
| implementer | コード実装、設定変更、テスト作成 |
| quality-assurance | テスト実行、レビュー、セキュリティ検証 |

## 並列実行判断基準

以下を全て満たすタスクは同一 Wave にグループ化し並列実行可能:

1. `blocked_by` に相互依存がない
2. 変更対象ファイルが重複しない
3. 同一リソースへの競合書き込みがない
