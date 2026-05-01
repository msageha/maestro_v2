---
name: dependency-worktree-planning
description: worktree mode での expected_paths、blocked_by、phase 分離、共有契約変更を安全に設計する Planner 向けガイド
version: "1.0.0"
tags: [planner, worktree, dependencies, blocked-by, phase-design]
priority: 15
---

# Dependency Worktree Planning

worktree mode で並列 Worker を安全に動かすためのタスク設計ガイド。目的は、同一ファイル競合、共有契約変更の並列化、失敗時の巻き添え破棄を減らすこと。

## 1. expected_paths の設計

各 task は、書き込む可能性のある相対パスを `expected_paths` に必ず含める。

| 状況 | expected_paths |
|---|---|
| 単一ファイル変更 | そのファイル |
| 複数ファイルだが同一領域 | 共通ディレクトリ |
| 影響範囲が未確定 | 先行 researcher task で範囲確定 |
| リポジトリ横断の設定変更 | `["."]` を使い、並列化しない |

## 2. blocked_by の判断

同一フェーズ内では、以下を逐次化する:

- 同じファイルまたは同じ生成物を書き換える task
- 公開 API / schema / migration / 設定キー / proto / generated source など共有契約を変更する task と、その利用側 task
- `go.mod` / lock file / formatter 設定 / codegen 設定を触る task
- rename / move / delete を含む task

`blocked_by` は同一フェーズ内の task `name` だけを参照する。フェーズを跨ぐ依存は phase 境界で表現する。

## 3. Phase 分離

失敗時に守りたい成果物と、失敗リスクが高い後続作業を同じ concrete phase に混ぜない。

| パターン | 推奨 |
|---|---|
| 共通型・interface・schema を先に作る | foundation phase または別 command |
| foundation 成果に依存する複数実装 | deferred / 後続 phase |
| 実験的変更 | required: false または別 command |
| verification | implementation 後の `deferred` phase に `depends_on_phases` で接続。publish 後の main 検証は publish 完了後に `run_on_main: true` の追加タスクを `plan add-task` で投入（publish 自体は Daemon が処理するため Planner は polling しない） |

## 4. 発行前チェック

- [ ] 同一フェーズ内の `expected_paths` が重なっていないか
- [ ] 共有契約変更が並列 task と衝突しないか
- [ ] codegen / formatter / lock file 更新が単独化されているか
- [ ] 守りたい foundation 成果物が高リスク task と同じ失敗単位に入っていないか
- [ ] 未確定の影響範囲を推測で分割していないか
