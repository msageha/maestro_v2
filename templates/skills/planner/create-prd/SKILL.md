---
name: create-prd
description: 曖昧なコマンドを構造化要件に変換するPRDフレームワーク
version: "1.0.0"
applies_to: ["planner"]
tags: [prd, requirements, planning]
priority: 10
---

# PRD作成スキル

ユーザーの曖昧な要求を構造化要件に変換し、Maestroタスク設計に接続する。

## 利用場面

- 抽象的な要求（「〜を作りたい」等）を受けた場合
- タスク分解前に要件を明確化する必要がある場合

## PRDテンプレート

### 1. 目的・背景

| 項目 | 内容 |
|------|------|
| **目標** | 達成目標と成果指標 |
| **対象** | 利用者・利用シナリオ |
| **価値** | 提供する価値（1-2文） |

### 2. 成功指標

- 定量指標（パフォーマンス、エラー率等）
- 定性指標（開発体験、保守性等）

### 3. User Stories

```
AS A [ロール] I WANT [機能] SO THAT [価値]
```

各ストーリーに付与:
- **Acceptance Criteria**: 検証可能な完了条件
- **優先度**: Must / Should / Could

### 4. Out of Scope

- スコープ外を明示的に列挙
- 将来対応候補は「Future Work」として分離

## Maestroタスク設計への対応

| PRD要素 | タスクフィールド |
|---------|-----------------|
| User Story | `purpose` |
| Acceptance Criteria | `acceptance_criteria` |
| Out of Scope | `constraints` |
| 優先度 | フェーズ分割・順序 |
| 成功指標 | verificationの`content` |

## 作成手順

1. 要求から目的・背景を抽出
2. 対象と提供価値を定義
3. User Storiesを作成しAcceptance Criteriaを付与
4. Out of Scopeを明示
5. タスク設計に変換可能な粒度まで分解

## 注意事項

- PRDは「何を作るか」を定義、「どう作るか」はcreate-specificationに委譲
- 大きすぎる場合はフェーズに分割
