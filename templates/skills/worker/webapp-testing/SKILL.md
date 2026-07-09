---
name: webapp-testing
description: Web UI やブラウザ E2E を Playwright 等の既存ツールで検証し、スクリーンショットや失敗原因を報告する Worker 向けガイド
version: "1.0.0"
tags: [worker, webapp, e2e, playwright, browser-testing]
priority: 20
---

# Webapp Testing

Web UI / ブラウザ E2E / アクセシビリティの確認が必要な task で使う。対象 project に既存のブラウザテスト手段がある場合に適用する。

## 1. 事前確認

1. プロジェクト固有の検証手段（`package.json` scripts、Playwright/Cypress 設定など）を確認し、既存の起動口を最優先で利用する。独自に同等処理を実装し直さない。
2. dev server が必要な場合は既存 scripts を使う。
3. テスト対象 URL、viewport、主要フローを acceptance_criteria から特定する。
4. 既存 tooling がない場合は、無理に新規導入せず確認不可として報告する。
5. ランタイム保護対象の設定ファイル（例: `~/.claude/`、`.claude/` 配下、`.codex/`、`.gemini/`）は task 中で編集しない。これらは Maestro の責務外（オペレーターのグローバル設定）であり、編集すると runtime 自身が確認プロンプトを出して自動進行が止まる。

## 2. 検証観点

| 観点           | 確認内容                                           |
| -------------- | -------------------------------------------------- |
| Flow           | 主要操作が完了するか                               |
| Rendering      | blank / layout break / overlap がないか            |
| Responsive     | mobile / desktop の主要 viewport                   |
| Accessibility  | ラベル、キーボード操作、コントラストの明らかな問題 |
| Error handling | 失敗時の表示と回復手段                             |

## 3. 報告

summary には以下を含める:

- 実行したコマンド
- 対象 URL / viewport
- PASS/FAIL と根拠
- 失敗時の再現手順
- スクリーンショットやログを保存した場合はパス

## 4. 禁止

- 視覚確認なしに「UI は問題ない」と断定しない
- 既存テスト設定を理由なく置き換えない
- E2E 検証のために production code を task 範囲外で大きく変更しない
