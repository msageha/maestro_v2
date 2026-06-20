# 8. tmux send-keys 一元化の原則

```
通常メッセージ配信のルール:
  queue/results 由来の「通常メッセージ配信」で tmux send-keys を直接呼び出してよいのは
  デーモン内の agent_executor モジュールのみ。他のモジュール・サブコマンドは必ず agent_executor を経由する。
```

**理由**:

1. ビジー判定ロジック（busy_patterns + idle_stable_sec）を一箇所に集約
2. Orchestrator ペイン保護を一貫して適用
3. `/clear` のクールダウン制御を統一
4. ログ記録を一箇所で行える
5. マルチプロバイダー対応時、プロバイダー別の配信差異をここだけで吸収

**例外: フォーメーション起動処理（startup dialog）**

`maestro up` のフォーメーション起動時に限り、`internal/formation/`（`tmux_formation.go` の `sendStartupDialogKeys` / `autoAcceptTrustDialog` 等）が `tmux.SendKeys` を直接呼び出す。これは agent_executor が扱う「タスク・コマンド・通知の配信」とは別レイヤーの操作であり、claude-code の "Bypass Permissions mode" 確認や workspace trust ダイアログに応答するためのもの。ペイン内容をキャプチャして対象ダイアログを判別し、不明時は fail-closed（何もせず次 tick で再試行）する。

> 原則: **通常メッセージ配信は agent_executor 経由に一元化**。起動時のダイアログ応答のみが formation 側の直接 send-keys の例外であり、ビジー判定・Orchestrator 保護・`/clear` 制御の対象外（起動シーケンス専用）。
