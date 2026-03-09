---
name: error-diagnosis-patterns
description: エラーの分類・根本原因分析・高速診断のパターン集
version: "1.0"
applies_to: ["orchestrator", "planner", "worker"]
tags: [error, diagnosis, debugging, retry, root-cause-analysis]
priority: 10
---

# Error Diagnosis Patterns

## 1. エラー分類体系

エラーを以下の 4 カテゴリに分類し、対応方針を決定する:

| カテゴリ | 例 | retry_safe | 典型的対応 |
|---------|-----|-----------|----------|
| **環境要因** | ポート競合、ディスク不足、依存サービス停止、ネットワークタイムアウト | `true` | 環境修復後にリトライ |
| **ロジック不具合** | nil pointer、境界値エラー、型不一致、無限ループ | `false` | コード修正が必要 |
| **仕様不一致** | API レスポンス形式の変更、期待値と実測値の乖離、未定義動作 | `false` | 仕様確認→コード修正 |
| **リソース制約** | メモリ不足、タイムアウト、ファイルディスクリプタ枯渇 | 状況依存 | 設定調整またはアルゴリズム改善 |

## 2. retry_safe フラグの判断ロジック

`--no-retry-safe` を指定すべきケースを判断するフローチャート:

```
エラー発生
  ├─ 同じ入力で再実行しても同じ結果になるか？
  │   ├─ Yes → no-retry-safe（リトライ無意味）
  │   │   例: コンパイルエラー、型エラー、ロジックバグ
  │   └─ No / 不明 → 次の質問へ
  │
  ├─ 部分的な変更がリポジトリに残っているか？
  │   ├─ Yes + 変更が中途半端 → no-retry-safe + partial-changes
  │   │   例: 3ファイル中2ファイルのみ変更済み
  │   └─ Yes + 変更は自己完結 → partial-changes のみ
  │
  └─ 外部要因（ネットワーク、サービス停止等）か？
      ├─ Yes → retry_safe（デフォルト）
      └─ No → no-retry-safe
```

## 3. 根本原因分析（簡略 5 Whys）

全てのエラーに 5 回の Why を問う必要はない。以下の短縮パターンを使う:

### 短縮パターン: 3 ステップ診断

1. **What**: 何が起きたか（エラーメッセージ・スタックトレースの要約）
2. **Where**: どこで起きたか（ファイル:行番号、関数名）
3. **Why**: なぜ起きたか（直接原因を 1 文で）

**例:**
```
What: "nil pointer dereference" in handler
Where: internal/auth/handler.go:42 Login()
Why: GetUser が nil を返すケースで nil チェックなし
```

ほとんどのエラーは 3 ステップで原因が特定できる。特定できない場合のみ追加の Why を重ねる。

## 4. エラー報告テンプレート

`--summary` でエラーを報告する際の構造:

```
[変更理由] <何を試みたか>
[注意事項] エラー: <カテゴリ>
- What: <エラーメッセージの要約>
- Where: <ファイル:行 or コマンド>
- Why: <直接原因>
- 試行した対応: <何をしたか（省略可）>
[未完了] <完了できなかった項目>
```

**例:**
```
[変更理由] UserService に GetByEmail メソッドを追加しようとした
[注意事項] エラー: ロジック不具合
- What: コンパイルエラー "cannot use *User as User value"
- Where: internal/service/user.go:58
- Why: UserRepository.FindByEmail の戻り値が *User だが interface は User を要求
- 試行した対応: interface 側の修正を検討したがスコープ外と判断
[未完了] GetByEmail メソッドの実装
```

## 5. 既知パターンによる高速診断

以下のエラーパターンは根本原因分析をスキップし、直接対応に進む:

### ビルド・コンパイルエラー

| パターン | 原因 | 対応 |
|---------|------|------|
| `undefined: X` | import 不足 or シンボル未定義 | import 追加 or 定義追加 |
| `cannot use X as Y` | 型不一致 | 型変換 or interface 確認 |
| `imported and not used` | 不要 import | import 削除 |
| `declared and not used` | 未使用変数 | `_` に変更 or 削除 |
| `missing method X` | interface 未実装 | メソッド追加 |

### テストエラー

| パターン | 原因 | 対応 |
|---------|------|------|
| `expected X but got Y` | アサーション失敗 | 期待値 or 実装を確認 |
| `test timed out` | 無限ループ or デッドロック | ロジック確認（retry_safe: false） |
| `panic: runtime error` | nil 参照 or 範囲外アクセス | nil チェック or 境界確認 |
| `connection refused` | テスト用サーバー未起動 | セットアップ確認（retry_safe: true） |

### ランタイムエラー

| パターン | 原因 | 対応 |
|---------|------|------|
| `permission denied` | ファイル権限 | chmod or パス確認 |
| `address already in use` | ポート競合 | ポート変更 or プロセス確認（retry_safe: true） |
| `context deadline exceeded` | タイムアウト | タイムアウト値調整 or 処理最適化 |
| `no such file or directory` | パス誤り | パス確認 or ファイル生成 |

## 6. 診断フロー

```
エラー発生
  │
  ├─ 既知パターンに該当？ → 高速診断（セクション5）で直接対応
  │
  ├─ 該当しない → 3 ステップ診断（セクション3）
  │   │
  │   ├─ 原因特定 → 修正 → 検証
  │   │
  │   └─ 原因不明 → 追加の Why を重ねる（最大2回追加）
  │       │
  │       ├─ 特定 → 修正 → 検証
  │       └─ 不明 → --status failed で報告（セクション4のテンプレート使用）
  │
  └─ 修正後の検証失敗？ → 別の原因を疑い再診断（最大2ラウンド）
```
