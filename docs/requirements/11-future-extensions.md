# 11. 将来の拡張ポイント

## マルチプロバイダー対応（設計予約）

現在は Claude 専用。将来的に以下の拡張を想定：

- `config.yaml` の `workers.models` でプロバイダー名を指定（例: `gpt-5.2-codex`, `gemini-3-pro`）
- `maestro agent launch` 内のプロバイダー分岐:
  - Claude: `--append-system-prompt`
  - Codex: `CODEX.md` ファイル生成
  - Gemini: `GEMINI.md` ファイル生成
- デーモンの agent_executor モジュールにプロバイダー別ビジー判定パターンを追加
- **影響範囲を限定**: `internal/agent/launcher.go` と `internal/agent/executor.go` の 2 箇所のみ変更で対応可能な設計

## その他の拡張可能性

> YAML → SQLite 等への移行基準は [§4.10](04-yaml-schema.md) で定義済み。

- Worker 数の動的増減（実行中のスケーリング）
- Web UI ダッシュボード（dashboard.md の可視化）
- スキル管理システム（Worker の得意分野学習）
- 副作用の冪等性強化（effect_id + CAS-style atomic publish for git operations）
- `maestro` バイナリの自動アップデート機構
