---
name: structured-ai-communication
description: AI-to-AI通信品質を高めるための構造化コミュニケーションガイドライン
version: "1.0"
applies_to: ["orchestrator", "planner", "worker"]
tags: [communication, structured-output, clarity, ai-to-ai]
priority: 10
---

# Structured AI Communication

AI Agent間通信（コマンド→タスク→結果報告）の品質を高めるガイドライン。

## 1. コンテキスト最小十分セット

受信側Agentが追加調査なしに着手できるよう、以下を含める。

| 項目 | 良い例 | 悪い例 |
|------|--------|--------|
| **What** 具体的アクション | `UserService.Create に email バリデーション追加` | `バリデーション改善` |
| **Where** 対象パス・関数 | `internal/service/user.go: Create()` | `ユーザー関連コード` |
| **Why** 目的（1文） | `重複登録防止` | （省略） |
| **Boundary** スコープ外 | `既存ユーザーの email 更新は対象外` | （省略） |
| **Dependencies** 前提 | `TASK-1 の GetByEmail を使用` | （省略） |

## 2. Acceptance Criteria: SMART + Given-When-Then

| SMART | 適用例 |
|-------|--------|
| **S**pecific | `GetByEmail(email string) (*User, error)` を追加 |
| **M**easurable | `go test ./internal/service/... が PASS` |
| **A**chievable | 1タスクで完了できる粒度 |
| **R**elevant | purpose との整合性 |
| **T**ime-bounded | タスクのスコープ内で完結 |

検証可能な criteria のテンプレート:
```
Given: {前提条件}  When: {アクション}  Then: {検証可能な期待結果}
```

## 3. 構造化IDパターン

複数要件・タスク間の参照に曖昧さを排除するためIDを付与する。
```
REQ-1: email検索  REQ-2: 重複拒否
TASK-1: GetByEmail実装 (REQ-1)  TASK-2: 重複チェック追加 (REQ-1, REQ-2)
```

## 4. 曖昧性検出パターン

| 曖昧パターン | 例 | 改善 |
|-------------|-----|------|
| 程度の形容詞 | 「適切に処理」 | 具体的条件に置換 |
| 暗黙の主語 | 「更新する」 | 対象を明示 |
| 曖昧な範囲 | 「関連ファイル」 | パスを列挙 |
| 未定義用語 | 略語 | 定義を付記 |
| 条件欠落 | 「エラー時」 | 種別と対応を列挙 |

- Planner: `[NEEDS CLARIFICATION: {質問}]` を付記
- Worker: 最も保守的な解釈で実装し `[注意事項]` に明記

## 5. アンチパターン

| パターン | 問題 | 改善 |
|---------|------|------|
| **丸投げ** | コンテキストなし | §1の最小十分セットを満たす |
| **過剰指定** | Worker判断余地ゼロ | What/Where/Whyを指定、Howは委ねる |
| **暗黙の依存** | 前提を明記しない | Dependencies に明記 |
| **ゴールポスト移動** | 事後のcriteria変更 | 追加要件は新タスク |
| **エコー応答** | 「完了しました」のみ | summaryタグで構造化報告 |
| **推測実装** | 曖昧指示を独自解釈 | §4で検出→保守的解釈+明記 |
