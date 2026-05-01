---
name: docs-changelog-release-notes
description: 実際の差分に基づいて README、CHANGELOG、リリースノート、移行手順を作成・更新する Worker 向けガイド
version: "1.0.0"
tags: [worker, docs, changelog, release-notes, migration]
priority: 20
---

# Docs Changelog Release Notes

ドキュメント・CHANGELOG・リリースノートを実装差分に基づいて作成する。推測で機能や破壊的変更を書かず、確認できた変更だけを記述する。

## 1. 情報源

優先順:

1. acceptance_criteria / task content
2. 実際の diff / files_changed
3. テスト・ビルド結果
4. 既存 README / docs / CHANGELOG の書式

## 2. 出力種別

| 種別 | 含める内容 |
|---|---|
| README | 利用者が次に実行するコマンド、設定、制約 |
| CHANGELOG | Added / Changed / Fixed / Removed / Security |
| Release notes | ユーザー影響、互換性、移行要否 |
| Migration guide | 旧手順、新手順、注意点、検証方法 |

## 3. 書き方

- 実装されていない予定や願望を書かない
- 破壊的変更は明示する
- コマンドは実行確認済みか未確認かを分ける
- 既存の見出し・語調・粒度に合わせる
- 長い内部実装説明より、利用者が行動できる情報を優先する

## 4. 完了前チェック

- [ ] 記載内容が diff と一致している
- [ ] 既存ドキュメントの構造を壊していない
- [ ] 変更理由とユーザー影響が分かる
- [ ] 未確認事項を断定していない
