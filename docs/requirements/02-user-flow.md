# 2. ユーザーフロー

```
1. install.sh で maestro をインストール（初回のみ）
   → go build で単一バイナリをビルド・配置
2. maestro setup <project_dir> でプロジェクト初期化
3. maestro up でフォーメーション起動
4. Orchestrator ウィンドウで指示を入力
   → Planner がタスク分解・Worker へ割当
   → Worker がタスク実行・結果報告
   → Planner が結果統合・Orchestrator へ報告
   → macOS 通知 + Orchestrator が結果報告
5. 継続モードの場合は自動ループ（評価に応じて続行 or 停止）
6. 終了時: maestro down
```
