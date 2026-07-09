# 2. ユーザーフロー

```
1. mise run install で maestro をインストール（初回のみ）
   → check-deps（tmux / go / claude / jq）+ go build で単一バイナリをビルド・配置
2. maestro setup <project_dir> でプロジェクト初期化（config.yaml / maestro.md / dashboard.md に加え instructions/・persona/・skills/ も .maestro/ 直下に展開）
3. maestro up でフォーメーション起動（起動後に tmux セッションへ自動 attach。--detach で抑止）
4. Orchestrator ウィンドウで指示を入力
   → Planner がタスク分解・Worker へ割当
   → Worker がタスク実行・結果報告
   → Planner が結果統合・Orchestrator へ報告
   → Orchestrator がユーザーに結果報告（tmux ペイン上）
5. 継続モードの場合は自動ループ（評価に応じて続行 or 停止）
6. 終了時: maestro down
```
