---
name: tdd-red-green-refactor
description: 振る舞い変更やバグ修正で red-green-refactor を使い、テスト先行で安全に実装する Worker 向けガイド
version: "1.0.0"
tags: [worker, tdd, testing, implementation, refactor]
priority: 20
---

# TDD Red-Green-Refactor

振る舞い変更・バグ修正・回帰防止が重要な task で使う。目的は、先に期待動作を固定してから最小実装し、最後に既存パターンへ整えること。

## 1. 手順

1. acceptance_criteria から検証すべき振る舞いを 1-3 個に絞る。
2. 既存テストの配置・命名・ヘルパーを確認する。
3. まず失敗するテストを書く。失敗理由が期待どおりか確認する。
4. 最小の実装でテストを通す。
5. 既存パターンに合わせて refactor する。
6. 関連テストを再実行し、summary に実行コマンドと結果を書く。

## 2. 適用範囲

| 状況 | 対応 |
|---|---|
| 既存テスト基盤がある | 既存パターンに合わせる |
| テスト基盤が見つからない | 無理に新基盤を作らず、確認範囲と代替検証を報告 |
| acceptance_criteria が曖昧 | 保守的に解釈し、未確認点を `[注意事項]` に明記 |
| 既存設計が大きく崩れる | 実装を止め、必要な設計判断を `[未完了]` に報告 |

## 3. 禁止

- テストを通すためだけに production code の契約を弱めない
- 既存テストを理由なく削除・緩和しない
- 大規模 refactor を task 範囲外に広げない

リトライ・修復ループの上限管理（`definition_of_abort`、エスカレーション段階）は `resilient-execution` に従う。本 skill では再定義しない。
