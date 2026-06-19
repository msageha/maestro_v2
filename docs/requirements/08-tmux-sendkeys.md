# 8. tmux send-keys 一元化の原則

```
全モジュール共通ルール:
  tmux send-keys を直接呼び出してよいのはデーモン内の agent_executor モジュールのみ。
  他のモジュール・サブコマンドは必ず agent_executor を経由する。
```

**理由**:

1. ビジー判定ロジック（busy_patterns + idle_stable_sec）を一箇所に集約
2. Orchestrator ペイン保護を一貫して適用
3. `/clear` のクールダウン制御を統一
4. ログ記録を一箇所で行える
5. マルチプロバイダー対応時、プロバイダー別の配信差異をここだけで吸収
