---
name: create-specification
description: PRDの要件をWorkerが実装可能な技術仕様に変換するフレームワーク
version: "1.0.0"
applies_to: ["planner"]
tags: [specification, requirements, quality]
priority: 10
---

# 仕様書作成スキル

PRDの要件をWorkerが実装可能な技術仕様に変換する。

## 利用場面

- PRD確定後、実装タスクに分解する直前
- タスクの`content`に技術仕様を含める必要がある場合

## 要件分類体系

| プレフィックス | 分類 | 説明 |
|---|---|---|
| `REQ-xxx` | 機能要件 | 実装すべき振る舞い |
| `SEC-xxx` | セキュリティ | 認証・認可・入力検証 |
| `CON-xxx` | 制約条件 | 技術的制約・互換性 |
| `GUD-xxx` | ガイドライン | 規約・命名規則 |
| `PAT-xxx` | パターン | 採用する設計パターン |

## Given-When-Then

各要件のAcceptance CriteriaはGWT形式で記述する。

```
REQ-001: Given: 前提条件 When: 操作 Then: 期待結果
```

## 仕様書テンプレート

1. **概要**: PRD/User Story参照、技術アプローチ概要
2. **要件一覧**: REQ/SEC/CON（各要件にGWT付与）
3. **設計指針**: GUD/PATを列挙
4. **テスト基準**: GWTがテストケースに直結、カバレッジ目標

## Maestroタスク設計への適用

| 仕様書要素 | タスクフィールド |
|----------|-----------------|
| `REQ-xxx` + GWT | `content` + `acceptance_criteria` |
| `SEC-xxx` / `CON-xxx` | `constraints` |
| `GUD-xxx` / `PAT-xxx` | `content`内の実装指針 |
| テスト基準 | verificationの`content` |

## 注意事項

- 仕様書は「どう作るか」を定義（「何を作るか」はPRD担当）
- Worker が追加調査なしで実装着手できる詳細度を目指す
- 各要件は独立して検証可能な粒度にする
