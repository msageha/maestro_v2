---
name: migration-refactor-planning
description: 大規模リファクタ、API変更、移行作業を段階化し、互換性と検証を保ちながら Worker task に分解する Planner 向けガイド
version: "1.0.0"
tags: [planner, migration, refactor, compatibility, phased-rollout]
priority: 20
---

# Migration Refactor Planning

大規模リファクタ、API 変更、データ移行、設定移行を安全に分割するための Planner 向けガイド。単発の巨大 task ではなく、互換性維持・段階移行・検証を明示した task 群にする。

## 1. 分解パターン

| Phase | 目的 | 典型 task |
|---|---|---|
| discovery | 現状把握 | researcher に影響範囲・呼び出し元・互換性制約を確認させる |
| foundation | 新旧併存の土台 | 新 interface / adapter / schema / config key を追加 |
| migration | 利用側移行 | 呼び出し元を小さな単位で新方式へ移す |
| cleanup | 旧方式削除 | 未使用確認後に旧 API / dead code を削除 |
| verification | 回帰確認 | テスト、lint、互換性確認、docs 更新。migration 後の `deferred` phase に `depends_on_phases` で接続。publish 後の main 検証は `run_on_main: true` を `plan add-task` で publish 完了後に投入（publish 自体は Daemon が処理） |

既に影響範囲が明確な場合、discovery phase は省略してよい。不明な関係を ID・名称・配置から推測して task 化しない。

## 2. 互換性ルール

- public API / schema / config / file format は、可能なら一度に置換せず新旧併存期間を作る
- 利用側移行 task は `expected_paths` が重ならない単位に分割する
- cleanup は migration と同一 task に混ぜない
- cleanup task は破壊的操作の安全規則（`maestro.md §破壊的操作の安全規則` および planner.md §タスク設計の原則）に従う。先行する未使用確認 task との `blocked_by` を設定し、削除根拠（grep / 参照解析の結果）を `content` に記載させる
- docs / changelog が必要な場合は最後に `docs-changelog-release-notes` を持つ Worker task を置く

## 3. Worker skill_refs の目安

| task | skill_refs |
|---|---|
| 影響範囲調査 | `source-grounded-response`, `semantic-code-search` |
| 実装移行 | `semantic-code-search`, `constraint-aware-implementation` |
| 振る舞い変更あり | `tdd-red-green-refactor`, `semantic-test-generation` |
| cleanup / review | `evaluation-driven-quality` |
| docs / migration guide | `docs-changelog-release-notes`, `source-grounded-response` |

## 4. 発行前チェック

- [ ] foundation と cleanup が同じ task に混ざっていない
- [ ] 共有契約変更が並列実装 task と衝突しない
- [ ] rollback / abort 時に中途半端な状態を説明できる
- [ ] verification が migration 後に置かれている
- [ ] 旧方式削除の根拠が確認可能になっている
